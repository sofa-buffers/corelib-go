package sofab

import (
	"encoding/binary"
	"io"
	"math"
)

// cursor parses a Sofab message by advancing an index over a single contiguous
// buffer — the Go analogue of the protobuf decode kernel, which advances a
// pointer over a flat byte range rather than pulling a byte at a time through a
// reader. Varints decode in a tight local loop and fixed/raw payloads are taken
// as subslices, so the visitor decode never re-enters an io.Reader (no per-byte
// interface call, no per-field allocation). Decoder.Accept slurps the whole
// message into buf once and runs the cursor over it.
type cursor struct {
	buf []byte
	pos int
}

// uvarint reads a base-128 varint at pos. If eofOK, a field boundary with no
// bytes left is reported as io.EOF (clean end of stream); a varint truncated by
// the end of the buffer is ErrInvalidMsg.
func (c *cursor) uvarint(eofOK bool) (uint64, error) {
	if c.pos >= len(c.buf) {
		if eofOK {
			return 0, io.EOF
		}
		return 0, ErrInvalidMsg
	}
	var val uint64
	var shift uint
	for i := c.pos; i < len(c.buf); i++ {
		b := c.buf[i]
		val |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			c.pos = i + 1
			return val, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, ErrInvalidMsg
		}
	}
	return 0, ErrInvalidMsg // ran off the end mid-varint
}

// take returns the next n bytes as a subslice of buf (zero-copy) and advances.
func (c *cursor) take(n uint64) ([]byte, error) {
	if n > uint64(len(c.buf)-c.pos) {
		return nil, ErrInvalidMsg
	}
	b := c.buf[c.pos : c.pos+int(n)]
	c.pos += int(n)
	return b, nil
}

func (c *cursor) fixlenHeader() (length, sub uint64, err error) {
	h, err := c.uvarint(false)
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

func (c *cursor) arrayCount() (uint64, error) {
	n, err := c.uvarint(false)
	if err != nil {
		return 0, err
	}
	if n == 0 || n > arrayMax {
		return 0, ErrInvalidMsg
	}
	return n, nil
}

// accept drives v over the buffer. nested reports whether we are inside a
// sequence (so a clean end-of-buffer is an error, and a sequence-end returns).
func (c *cursor) accept(v Visitor, nested bool) error {
	for {
		h, err := c.uvarint(true)
		if err != nil {
			if err == io.EOF {
				if nested {
					return ErrInvalidMsg // missing sequence end
				}
				return nil
			}
			return err
		}
		t := WireType(h & 0x07)
		id := ID(h >> 3)
		if t != TypeSequenceEnd && (h>>3) > uint64(IDMax) {
			return ErrInvalidMsg
		}
		switch t {
		case TypeVarintUnsigned:
			x, err := c.uvarint(false)
			if err != nil {
				return err
			}
			if err := v.Unsigned(id, x); err != nil {
				return err
			}
		case TypeVarintSigned:
			x, err := c.uvarint(false)
			if err != nil {
				return err
			}
			if err := v.Signed(id, zigzagDecode(x)); err != nil {
				return err
			}
		case TypeFixlen:
			if err := c.acceptFixlen(v, id); err != nil {
				return err
			}
		case TypeVarintArrayUnsigned:
			n, err := c.arrayCount()
			if err != nil {
				return err
			}
			out := make([]uint64, n)
			for i := range out {
				if out[i], err = c.uvarint(false); err != nil {
					return err
				}
			}
			if err := v.UnsignedArray(id, out); err != nil {
				return err
			}
		case TypeVarintArraySigned:
			n, err := c.arrayCount()
			if err != nil {
				return err
			}
			out := make([]int64, n)
			for i := range out {
				x, err := c.uvarint(false)
				if err != nil {
					return err
				}
				out[i] = zigzagDecode(x)
			}
			if err := v.SignedArray(id, out); err != nil {
				return err
			}
		case TypeFixlenArray:
			if err := c.acceptFixlenArray(v, id); err != nil {
				return err
			}
		case TypeSequenceStart:
			child, err := v.BeginSequence(id)
			if err != nil {
				return err
			}
			if err := c.accept(child, true); err != nil {
				return err
			}
			if err := child.EndSequence(); err != nil {
				return err
			}
		case TypeSequenceEnd:
			if nested {
				return nil
			}
			return ErrInvalidMsg // dangling end at top level
		default:
			return ErrInvalidMsg
		}
	}
}

func (c *cursor) acceptFixlen(v Visitor, id ID) error {
	n, sub, err := c.fixlenHeader()
	if err != nil {
		return err
	}
	switch sub {
	case fixFp32:
		if n != 4 {
			return ErrInvalidMsg
		}
		b, err := c.take(4)
		if err != nil {
			return err
		}
		return v.Float32(id, math.Float32frombits(binary.LittleEndian.Uint32(b)))
	case fixFp64:
		if n != 8 {
			return ErrInvalidMsg
		}
		b, err := c.take(8)
		if err != nil {
			return err
		}
		return v.Float64(id, math.Float64frombits(binary.LittleEndian.Uint64(b)))
	case fixStr:
		b, err := c.take(n)
		if err != nil {
			return err
		}
		return v.String(id, string(b))
	case fixBlob:
		b, err := c.take(n)
		if err != nil {
			return err
		}
		return v.Bytes(id, b)
	default:
		return ErrInvalidMsg
	}
}

func (c *cursor) acceptFixlenArray(v Visitor, id ID) error {
	n, err := c.arrayCount()
	if err != nil {
		return err
	}
	h, err := c.uvarint(false)
	if err != nil {
		return err
	}
	sub := h & 0x07
	size := h >> 3
	switch {
	case sub == fixFp32 && size == 4:
		payload, err := c.take(n * 4)
		if err != nil {
			return err
		}
		out := make([]float32, n)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(payload[i*4:]))
		}
		return v.Float32Array(id, out)
	case sub == fixFp64 && size == 8:
		payload, err := c.take(n * 8)
		if err != nil {
			return err
		}
		out := make([]float64, n)
		for i := range out {
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(payload[i*8:]))
		}
		return v.Float64Array(id, out)
	default:
		return ErrInvalidMsg
	}
}
