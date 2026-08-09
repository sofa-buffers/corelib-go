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
	depth       int  // sequences open on this stream (0 at the top level), §4.9
	lim         limits
}

// NewDecoder returns a Decoder reading from r. The internal buffer for the
// pull-parser path is allocated lazily on first use, so the visitor path
// (Accept) — which reads the message into one contiguous buffer itself — does
// not pay for it.
//
// Optional decode limits (WithMaxArrayCount, WithMaxStringLen, WithMaxBlobLen)
// apply to both the pull parser and Decoder.Accept; with none, no limits are
// enforced.
func NewDecoder(r io.Reader, opts ...Option) *Decoder {
	return &Decoder{src: r, lim: newLimits(opts)}
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
//
// Next owns the stream's nesting state (§4.9): it counts the sequences the
// stream has opened, rejects the one that would nest past MaxDepth, and rejects
// a sequence-end marker that closes nothing — both ErrInvalidMsg (§6.3). The
// count is a property of the stream, not of any one call, so every pull consumer
// inherits the same ceiling the visitor kernels apply (cursor.accept,
// Decoder.acceptStream) and the three surfaces agree on which messages are
// well-formed (issue #78).
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
	case TypeSequenceStart:
		// MaxDepth nested sequences are legal; the one past it is malformed
		// regardless of what follows (§4.9/§6.2), so it is refused here rather
		// than handed to the caller as an ordinary header for its own scope
		// stack to absorb.
		if d.depth >= MaxDepth {
			return Field{}, ErrInvalidMsg
		}
		d.depth++
		d.needConsume = false
	case TypeSequenceEnd:
		// An end marker with no open sequence is InvalidMessage (§6.3): the
		// stream is unbalanced, not merely surprising.
		if d.depth == 0 {
			return Field{}, ErrInvalidMsg
		}
		d.depth--
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
// ErrIncomplete (INCOMPLETE, §7 — ran out of bytes mid-field, not malformed). A
// varint that exceeds 64 bits is ErrInvalidMsg (malformed).
//
// It decodes out of the reader's own buffer via Peek, then Discard, rather than
// pulling one byte at a time: bufio.ReadByte is a method call and a bounds test
// per byte, so a ten-byte varint cost ten of them before any decoding happened.
// Peek hands back a window into that same buffer with no copy, the shared
// unrolled decoder (varint.go) reads it, and Discard advances by exactly the
// bytes the varint used.
//
// Peek fills until it has maxVarintLen bytes or the source reports an error, so
// a short window means the stream really is ending — never merely a chunk
// boundary. That is what keeps a varint split across reads INCOMPLETE rather
// than misread, and nothing is consumed unless a whole varint was decoded.
func (d *Decoder) readVarint(firstEOFok bool) (uint64, error) {
	w, err := d.r.Peek(maxVarintLen)
	if len(w) >= maxVarintLen {
		v, np, ok := uvarintFast(w, 0)
		if !ok {
			return 0, ErrInvalidMsg // > 64 bits: overlong, malformed
		}
		d.r.Discard(np)
		return v, nil
	}
	if len(w) == 0 {
		if err != nil && err != io.EOF {
			return 0, err // a real reader failure, not a stream boundary
		}
		if firstEOFok {
			return 0, io.EOF // clean end of stream at a field boundary
		}
		return 0, ErrIncomplete // expected a varint, but the stream ended
	}
	v, np, st := uvarintTail(w, 0)
	switch st {
	case varintOK:
		d.r.Discard(np)
		return v, nil
	case varintOverflow:
		return 0, ErrInvalidMsg // varint > 64 bits: malformed
	}
	if err != nil && err != io.EOF {
		return 0, err
	}
	return 0, ErrIncomplete // ended mid-varint: truncated
}

// readVarintBatch decodes as many of dst's elements as it can straight out of
// the reader's buffer, and returns how many. Zero means the buffer is down to
// its last few bytes (or the stream is ending) and the caller should fall back
// to readVarint for the next element, which is where truncation and end-of-
// stream are judged.
//
// It exists because Peek and Discard are the pull parser's per-varint cost once
// the byte-at-a-time reads are gone. One Peek/Discard pair per batch amortizes
// that away, and the elements decode through the same shared kernel the visitor
// path uses (varint.go).
//
// Nothing is consumed beyond the elements actually decoded, so a batch that runs
// into the end of the buffer leaves the remaining bytes for the next call and
// the resume-at-any-boundary property is unchanged.
func (d *Decoder) readVarintBatch(dst []uint64) (int, error) {
	avail := d.r.Buffered()
	if avail < maxVarintLen {
		// Force a fill so a whole varint is present if the stream can supply
		// one; a short result here just means the caller takes the slow path.
		if _, err := d.r.Peek(maxVarintLen); err != nil {
			if err != io.EOF {
				return 0, err
			}
		}
		if avail = d.r.Buffered(); avail < maxVarintLen {
			return 0, nil
		}
	}
	w, err := d.r.Peek(avail)
	if err != nil {
		return 0, err
	}
	got, np, st := decodeUvarintRun(w, 0, dst)
	if st != varintOK {
		return 0, tailErr(st)
	}
	if np > 0 {
		d.r.Discard(np)
	}
	return got, nil
}

// readFixlenHeader reads a fixed-length field's length-and-subtype varint,
// splitting it into the byte length (h>>3) and the 3-bit subtype (h&0x07). A
// length past arrayMax, a reserved subtype, or a width that contradicts an
// fp32/fp64 subtype is rejected as a malformed message.
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
	// §4.6 defines subtypes 0x0-0x3 only — 0x4-0x7 are reserved — and pins the
	// float widths: fp32 carries 4 payload bytes, fp64 carries 8; strings and
	// blobs take any length. §6.3 names a reserved subtype and a wrong-width
	// fp32/fp64 as InvalidMessage. This is framing, not schema, so it holds
	// whether or not the field is materialised: the check lives here so that
	// every pull consumer — Float32, Float64, String, Bytes, Skip and Next's
	// auto-skip — inherits it from one place, and a skipped field is a length
	// jump over a validated word rather than one over an attacker-chosen stride
	// the parser resynchronises on (issue #76). The visitor paths already ruled
	// the same way in their subtype switch (cursor.acceptFixlen,
	// acceptStreamFixlen), so all three surfaces now agree on every word.
	if err := checkFixlenSubtype(sub, length); err != nil {
		return 0, 0, err
	}
	if err := d.lim.checkFixlen(sub, length); err != nil {
		return 0, 0, err
	}
	return length, sub, nil
}

// checkFixlenSubtype validates a fixlen word's subtype and its declared width
// against §4.6. It is pure framing: it never consults the schema or the
// receiver-side limits (those come after it, as ErrLimitExceeded).
func checkFixlenSubtype(sub, length uint64) error {
	switch sub {
	case fixFp32:
		if length != 4 {
			return ErrInvalidMsg
		}
	case fixFp64:
		if length != 8 {
			return ErrInvalidMsg
		}
	case fixStr, fixBlob:
		// Any length, including zero.
	default:
		return ErrInvalidMsg // reserved subtype 0x4-0x7
	}
	return nil
}

// readRaw reads exactly n bytes. A short read (the stream ending early) is
// reported as ErrIncomplete — the payload was truncated mid-field (§7), not
// malformed.
//
// It never pre-allocates the full n bytes from the (untrusted) claimed length:
// up to readRawChunk it sizes the buffer once — the common path for ordinary
// fields — but a larger claim is grown as bytes actually arrive, so a hostile
// length costs memory only in proportion to the bytes really delivered before
// the stream ends (amplification hardening, issue #40).
func (d *Decoder) readRaw(n uint64) ([]byte, error) {
	if n <= readRawChunk {
		buf := make([]byte, n)
		if _, err := io.ReadFull(d.r, buf); err != nil {
			return nil, eofToIncomplete(err)
		}
		return buf, nil
	}
	buf := make([]byte, 0, readRawChunk)
	tmp := make([]byte, readRawChunk)
	for uint64(len(buf)) < n {
		want := min(n-uint64(len(buf)), uint64(readRawChunk))
		m, err := io.ReadFull(d.r, tmp[:want])
		buf = append(buf, tmp[:m]...)
		if err != nil {
			return nil, eofToIncomplete(err)
		}
	}
	return buf, nil
}

// readRawChunk bounds the largest slice readRaw allocates up front; past it the
// buffer grows incrementally as bytes arrive.
const readRawChunk = 1 << 16

// initialArrayCap chooses a starting capacity for a decoded array that never
// pre-allocates from the untrusted wire count: the slice grows via append as
// elements actually decode, so a hostile count costs memory only in proportion
// to the bytes delivered before the stream ends (amplification hardening,
// issue #40).
func initialArrayCap(n uint64) int {
	const cap0 = 64
	if n < cap0 {
		return int(n)
	}
	return cap0
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

// String consumes the current field as a string. When strict UTF-8 is enabled
// (SOFAB_STRICT_UTF8, the default; §6.4), a payload that is not valid UTF-8 is
// rejected as ErrInvalidMsg (the INVALID outcome, §5.2). With it disabled
// (WithStrictUTF8(false)) the wire bytes are kept verbatim. Validation runs only
// here, where the string is materialized — a skipped field (Skip) is a length
// jump that is never validated (§6.4). The full payload is assembled (via
// fixlenBytes → readRaw) before this check, so a payload split across a chunk
// boundary stays ErrIncomplete rather than being misread as invalid (§6.4
// cross-chunk).
func (d *Decoder) String() (string, error) {
	b, err := d.fixlenBytes(fixStr)
	if err != nil {
		return "", err
	}
	if d.lim.strictUTF8 && !utf8.Valid(b) {
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
		// A plain walk to the matching end. The counter is only how far this
		// call has descended below its starting scope — the MaxDepth ceiling is
		// the decoder's, applied by Next against the stream's absolute depth
		// (§4.9). Checking a relative counter here was the bug: nesting already
		// established by earlier Next calls was invisible to it, so a skip that
		// started 200 scopes deep could walk 200 more and still return nil
		// (issue #78).
		open := 1
		for open > 0 {
			f, err := d.Next()
			if err == io.EOF {
				return ErrIncomplete // sequence never closed: truncated (§7)
			}
			if err != nil {
				return err
			}
			switch f.Type {
			case TypeSequenceStart:
				open++
			case TypeSequenceEnd:
				open--
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
		return eofToIncomplete(err)
	case TypeVarintArrayUnsigned, TypeVarintArraySigned:
		// As on the fixlen-array arm below and in the typed readers, the count goes
		// through arrayCount: the ceilings are properties of the wire format, so a
		// count past arrayMax is INVALID (§6.2), and INVALID dominates INCOMPLETE
		// (§5.2). Read as a bare varint, an over-ceiling count on a truncated array
		// was masked by the element walk simply running out of bytes, and the caller
		// was told to wait for more of a message that can never become valid (issue
		// #77). Any receiver limit applies here too (§6.2.1, ErrLimitExceeded),
		// matching the visitor paths on the same bytes.
		n, err := d.arrayCount()
		if err != nil {
			return err
		}
		var stage [varintChunk]uint64
		for n > 0 {
			want := min(n, uint64(varintChunk))
			got, err := d.readVarintBatch(stage[:want])
			if err != nil {
				return err
			}
			if got == 0 {
				if _, err := d.readVarint(false); err != nil {
					return err
				}
				n--
				continue
			}
			n -= uint64(got)
		}
		return nil
	case TypeFixlenArray:
		// The count goes through arrayCount, exactly as the typed readers and the
		// visitor paths do: skipping a field checks its framing, it does not trust
		// it. That enforces the format ceiling arrayMax (§6.2, INVALID) and any
		// receiver limit (§6.2.1, ErrLimitExceeded) here, and it is what keeps the
		// payload length below computable — with n ≤ arrayMax and size ≤ 8 the
		// product is at most 2^34, so it can neither wrap mod 2^64 nor go negative
		// when converted to int (issue #75).
		n, err := d.arrayCount()
		if err != nil {
			return err
		}
		// A fixlen array always carries its fixlen_word, even when empty (§4.8);
		// only the payload is elided for a zero count.
		h, err := d.readVarint(false)
		if err != nil {
			return err
		}
		sub := h & 0x07
		size := h >> 3
		// §4.8 admits only fp32/4 and fp64/8 as fixlen-array elements. Anything
		// else — a string/blob subtype, a width that contradicts its subtype — is
		// malformed regardless of the schema, so a skip rejects it just as
		// cursor.acceptFixlenArray and acceptStreamFixlenArray do; the size would
		// otherwise be an attacker-chosen stride the parser resynchronises on.
		if !((sub == fixFp32 && size == 4) || (sub == fixFp64 && size == 8)) {
			return ErrInvalidMsg
		}
		_, err = d.r.Discard(int(n) * int(size))
		return eofToIncomplete(err)
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
	if err := d.lim.checkArrayCount(n); err != nil {
		return 0, err
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
	out := make([]T, 0, initialArrayCap(n))
	var stage [varintChunk]uint64
	for uint64(len(out)) < n {
		want := min(n-uint64(len(out)), uint64(varintChunk))
		got, err := d.readVarintBatch(stage[:want])
		if err != nil {
			return nil, err
		}
		if got == 0 {
			// Near the end of the buffer or the stream: one element at a time,
			// where truncation and end-of-stream are decided.
			v, err := d.readVarint(false)
			if err != nil {
				return nil, err
			}
			out = append(out, T(v))
			continue
		}
		for _, v := range stage[:got] {
			out = append(out, T(v))
		}
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
	out := make([]T, 0, initialArrayCap(n))
	var stage [varintChunk]uint64
	for uint64(len(out)) < n {
		want := min(n-uint64(len(out)), uint64(varintChunk))
		got, err := d.readVarintBatch(stage[:want])
		if err != nil {
			return nil, err
		}
		if got == 0 {
			v, err := d.readVarint(false)
			if err != nil {
				return nil, err
			}
			out = append(out, T(zigzagDecode(v)))
			continue
		}
		for _, v := range stage[:got] {
			out = append(out, T(zigzagDecode(v)))
		}
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
	out := make([]float32, 0, initialArrayCap(n))
	for i := uint64(0); i < n; i++ {
		buf, err := d.readRaw(4)
		if err != nil {
			return nil, err
		}
		out = append(out, math.Float32frombits(binary.LittleEndian.Uint32(buf)))
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
	out := make([]float64, 0, initialArrayCap(n))
	for i := uint64(0); i < n; i++ {
		buf, err := d.readRaw(8)
		if err != nil {
			return nil, err
		}
		out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(buf)))
	}
	d.needConsume = false
	return out, nil
}

// eofToIncomplete maps an end-of-stream error hit mid-value to ErrIncomplete
// (the payload was truncated mid-field, §7 — INCOMPLETE, not malformed), passing
// any other error through unchanged.
func eofToIncomplete(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return ErrIncomplete
	}
	return err
}
