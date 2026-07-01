package sofab

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"unicode/utf8"
)

// Decoder is a pull parser for a Sofab byte stream read from an io.Reader.
//
// Usage: call Next to obtain the next field header, then exactly one typed
// reader (Unsigned, Signed, Float32, String, ReadUnsignedArray, ...) or Skip to
// consume its value, before calling Next again. Next auto-skips an unconsumed
// scalar value for convenience. Next returns io.EOF at the clean end of the
// top-level stream.
type Decoder struct {
	src         io.Reader     // original source; Accept slurps directly from it
	r           *bufio.Reader // pull-parser buffer, created lazily on first Next
	cur         Field
	needConsume bool // a value-bearing field header is read but not yet consumed
}

// NewDecoder returns a Decoder reading from r. The internal buffer for the
// pull-parser path is allocated lazily on first use, so the visitor path
// (Accept) — which reads the message into one contiguous buffer itself — does
// not pay for it.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{src: r}
}

// asBufio reuses an existing *bufio.Reader source, otherwise wraps it once.
func asBufio(r io.Reader) *bufio.Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return br
	}
	return bufio.NewReader(r)
}

// Next reads the next field header. After a value-bearing field it returns, the
// caller must consume the value (typed reader or Skip) before the following
// Next; an unconsumed scalar/array/fixlen value is auto-skipped. Sequence
// start/end markers carry no value. Returns io.EOF at the end of the stream.
func (d *Decoder) Next() (Field, error) {
	if d.r == nil {
		d.r = asBufio(d.src)
	}
	if d.needConsume {
		if err := d.skipValue(); err != nil {
			return Field{}, err
		}
	}
	h, err := d.readVarint(true)
	if err != nil {
		return Field{}, err
	}
	t := WireType(h & 0x07)
	id := h >> 3
	if id > uint64(IDMax) {
		return Field{}, ErrInvalidMsg
	}
	d.cur = Field{ID: ID(id), Type: t}
	switch t {
	case TypeVarintUnsigned, TypeVarintSigned, TypeFixlen,
		TypeVarintArrayUnsigned, TypeVarintArraySigned, TypeFixlenArray:
		d.needConsume = true
	case TypeSequenceStart, TypeSequenceEnd:
		d.needConsume = false
	default:
		return Field{}, ErrInvalidMsg
	}
	return d.cur, nil
}

// Field returns the field header most recently returned by Next.
func (d *Decoder) Field() Field { return d.cur }

// readVarint reads a base-128 varint. If firstEOFok is true, an EOF before any
// byte is reported as io.EOF (a clean stream boundary); a mid-varint EOF is
// always ErrInvalidMsg.
func (d *Decoder) readVarint(firstEOFok bool) (uint64, error) {
	var val uint64
	var shift uint
	for i := 0; ; i++ {
		b, err := d.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				if i == 0 && firstEOFok {
					return 0, io.EOF
				}
				return 0, ErrInvalidMsg
			}
			return 0, err
		}
		val |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return val, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, ErrInvalidMsg
		}
	}
}

// readFixlenHeader reads a fixed-length field's length-and-subtype varint,
// splitting it into the byte length (h>>3) and the 3-bit subtype (h&0x07). A
// length past arrayMax is rejected as a malformed message.
func (d *Decoder) readFixlenHeader() (length uint64, sub uint64, err error) {
	h, err := d.readVarint(false)
	if err != nil {
		return 0, 0, err
	}
	length = h >> 3
	sub = h & 0x07
	if length > arrayMax {
		return 0, 0, ErrInvalidMsg
	}
	return length, sub, nil
}

// readRaw reads exactly n bytes into a freshly allocated buffer. A short read
// (the stream ending early) is reported as ErrInvalidMsg.
func (d *Decoder) readRaw(n uint64) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return nil, eofToInvalid(err)
	}
	return buf, nil
}

// Unsigned consumes the current field as an unsigned integer.
func (d *Decoder) Unsigned() (uint64, error) {
	if !d.needConsume || d.cur.Type != TypeVarintUnsigned {
		return 0, ErrUsage
	}
	v, err := d.readVarint(false)
	if err != nil {
		return 0, err
	}
	d.needConsume = false
	return v, nil
}

// Signed consumes the current field as a signed integer.
func (d *Decoder) Signed() (int64, error) {
	if !d.needConsume || d.cur.Type != TypeVarintSigned {
		return 0, ErrUsage
	}
	v, err := d.readVarint(false)
	if err != nil {
		return 0, err
	}
	d.needConsume = false
	return zigzagDecode(v), nil
}

// Bool consumes the current field as a boolean (unsigned 0/1).
func (d *Decoder) Bool() (bool, error) {
	v, err := d.Unsigned()
	return v != 0, err
}

// Float32 consumes the current field as a 32-bit float.
func (d *Decoder) Float32() (float32, error) {
	if !d.needConsume || d.cur.Type != TypeFixlen {
		return 0, ErrUsage
	}
	n, sub, err := d.readFixlenHeader()
	if err != nil {
		return 0, err
	}
	if sub != fixFp32 || n != 4 {
		return 0, ErrInvalidMsg
	}
	buf, err := d.readRaw(4)
	if err != nil {
		return 0, err
	}
	d.needConsume = false
	return math.Float32frombits(binary.LittleEndian.Uint32(buf)), nil
}

// Float64 consumes the current field as a 64-bit float.
func (d *Decoder) Float64() (float64, error) {
	if !d.needConsume || d.cur.Type != TypeFixlen {
		return 0, ErrUsage
	}
	n, sub, err := d.readFixlenHeader()
	if err != nil {
		return 0, err
	}
	if sub != fixFp64 || n != 8 {
		return 0, ErrInvalidMsg
	}
	buf, err := d.readRaw(8)
	if err != nil {
		return 0, err
	}
	d.needConsume = false
	return math.Float64frombits(binary.LittleEndian.Uint64(buf)), nil
}

// String consumes the current field as a string. A payload that is not valid
// UTF-8 is rejected as ErrInvalidMsg (§6.3).
func (d *Decoder) String() (string, error) {
	b, err := d.fixlenBytes(fixStr)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", ErrInvalidMsg
	}
	return string(b), nil
}

// Bytes consumes the current field as a binary blob.
func (d *Decoder) Bytes() ([]byte, error) {
	return d.fixlenBytes(fixBlob)
}

// fixlenBytes consumes the current fixlen field, requiring its subtype to equal
// want (fixStr or fixBlob), and returns the raw payload. It backs String and
// Bytes. A wrong field type is ErrUsage; a mismatched subtype is ErrInvalidMsg.
func (d *Decoder) fixlenBytes(want uint64) ([]byte, error) {
	if !d.needConsume || d.cur.Type != TypeFixlen {
		return nil, ErrUsage
	}
	n, sub, err := d.readFixlenHeader()
	if err != nil {
		return nil, err
	}
	if sub != want {
		return nil, ErrInvalidMsg
	}
	buf, err := d.readRaw(n)
	if err != nil {
		return nil, err
	}
	d.needConsume = false
	return buf, nil
}

// Skip consumes the current field's value. For a sequence-start it skips the
// entire nested sequence up to the matching end.
func (d *Decoder) Skip() error {
	switch d.cur.Type {
	case TypeSequenceStart:
		depth := 1
		for depth > 0 {
			f, err := d.Next()
			if err == io.EOF {
				return ErrInvalidMsg
			}
			if err != nil {
				return err
			}
			switch f.Type {
			case TypeSequenceStart:
				depth++
				if depth > MaxDepth {
					return ErrInvalidMsg
				}
			case TypeSequenceEnd:
				depth--
			default:
				if err := d.skipValue(); err != nil {
					return err
				}
			}
		}
		return nil
	case TypeSequenceEnd:
		return nil
	default:
		return d.skipValue()
	}
}

// skipValue consumes and discards the current scalar, array, or fixlen value
// (everything except sequence markers, which carry no value) so the next Next
// starts at a field boundary. Sequence skipping is handled by Skip.
func (d *Decoder) skipValue() error {
	d.needConsume = false
	switch d.cur.Type {
	case TypeVarintUnsigned, TypeVarintSigned:
		_, err := d.readVarint(false)
		return err
	case TypeFixlen:
		n, _, err := d.readFixlenHeader()
		if err != nil {
			return err
		}
		_, err = d.r.Discard(int(n))
		return eofToInvalid(err)
	case TypeVarintArrayUnsigned, TypeVarintArraySigned:
		n, err := d.readVarint(false)
		if err != nil {
			return err
		}
		for i := uint64(0); i < n; i++ {
			if _, err := d.readVarint(false); err != nil {
				return err
			}
		}
		return nil
	case TypeFixlenArray:
		n, err := d.readVarint(false)
		if err != nil {
			return err
		}
		// A fixlen array always carries its fixlen_word, even when empty (§4.8);
		// only the payload is elided for a zero count.
		h, err := d.readVarint(false)
		if err != nil {
			return err
		}
		size := h >> 3
		_, err = d.r.Discard(int(n * size))
		return eofToInvalid(err)
	}
	return nil
}

// arrayCount reads an array's leading element count. Zero is valid — an empty
// array (§4.7/§4.8); only a count past arrayMax is rejected as ErrInvalidMsg.
func (d *Decoder) arrayCount() (uint64, error) {
	n, err := d.readVarint(false)
	if err != nil {
		return 0, err
	}
	if n > arrayMax {
		return 0, ErrInvalidMsg
	}
	return n, nil
}

// ReadUnsignedArray consumes the current field as an array of unsigned integers.
func ReadUnsignedArray[T Unsigned](d *Decoder) ([]T, error) {
	if !d.needConsume || d.cur.Type != TypeVarintArrayUnsigned {
		return nil, ErrUsage
	}
	n, err := d.arrayCount()
	if err != nil {
		return nil, err
	}
	out := make([]T, n)
	for i := range out {
		v, err := d.readVarint(false)
		if err != nil {
			return nil, err
		}
		out[i] = T(v)
	}
	d.needConsume = false
	return out, nil
}

// ReadSignedArray consumes the current field as an array of signed integers.
func ReadSignedArray[T Signed](d *Decoder) ([]T, error) {
	if !d.needConsume || d.cur.Type != TypeVarintArraySigned {
		return nil, ErrUsage
	}
	n, err := d.arrayCount()
	if err != nil {
		return nil, err
	}
	out := make([]T, n)
	for i := range out {
		v, err := d.readVarint(false)
		if err != nil {
			return nil, err
		}
		out[i] = T(zigzagDecode(v))
	}
	d.needConsume = false
	return out, nil
}

// ReadFloat32Array consumes the current field as an array of 32-bit floats.
func (d *Decoder) ReadFloat32Array() ([]float32, error) {
	if !d.needConsume || d.cur.Type != TypeFixlenArray {
		return nil, ErrUsage
	}
	n, err := d.arrayCount()
	if err != nil {
		return nil, err
	}
	// The fixlen_word is always present, even for an empty array (§4.8), so read
	// and validate it before the (possibly zero) payload.
	h, err := d.readVarint(false)
	if err != nil {
		return nil, err
	}
	if (h&0x07) != fixFp32 || (h>>3) != 4 {
		return nil, ErrInvalidMsg
	}
	if n == 0 {
		d.needConsume = false
		return []float32{}, nil
	}
	out := make([]float32, n)
	for i := range out {
		buf, err := d.readRaw(4)
		if err != nil {
			return nil, err
		}
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf))
	}
	d.needConsume = false
	return out, nil
}

// ReadFloat64Array consumes the current field as an array of 64-bit floats.
func (d *Decoder) ReadFloat64Array() ([]float64, error) {
	if !d.needConsume || d.cur.Type != TypeFixlenArray {
		return nil, ErrUsage
	}
	n, err := d.arrayCount()
	if err != nil {
		return nil, err
	}
	// The fixlen_word is always present, even for an empty array (§4.8), so read
	// and validate it before the (possibly zero) payload.
	h, err := d.readVarint(false)
	if err != nil {
		return nil, err
	}
	if (h&0x07) != fixFp64 || (h>>3) != 8 {
		return nil, ErrInvalidMsg
	}
	if n == 0 {
		d.needConsume = false
		return []float64{}, nil
	}
	out := make([]float64, n)
	for i := range out {
		buf, err := d.readRaw(8)
		if err != nil {
			return nil, err
		}
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(buf))
	}
	d.needConsume = false
	return out, nil
}

// eofToInvalid maps an end-of-stream error hit mid-value to ErrInvalidMsg (the
// message was truncated), passing any other error through unchanged.
func eofToInvalid(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return ErrInvalidMsg
	}
	return err
}
