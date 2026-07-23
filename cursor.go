package sofab

import (
	"encoding/binary"
	"io"
	"math"
	"unicode/utf8"
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
	lim limits
}

// uvarint reads a base-128 varint at pos. If eofOK, a field boundary with no
// bytes left is reported as io.EOF (clean end of stream); a varint truncated by
// the end of the buffer is ErrIncomplete (INCOMPLETE, §7) — it ran out of bytes
// mid-field. A varint that exceeds 64 bits is ErrInvalidMsg (malformed).
//
// The single-byte case (payload < 0x80 — every field header for id<16, and every
// small scalar/count) is peeled into a lean fast path: one bounds-checked load and
// a return, with none of the multi-byte loop's shift/overflow bookkeeping. uvarint
// is the hottest decode function (~32 % of Ir), so shaving the common read is worth
// it even though the two-value return keeps uvarint just over the inline budget.
// Measured: decode 38603 -> 37044 Ir/op (-4.0 %), encode unchanged.
func (c *cursor) uvarint(eofOK bool) (uint64, error) {
	if p := c.pos; p < len(c.buf) {
		if b := c.buf[p]; b < 0x80 {
			c.pos = p + 1
			return uint64(b), nil
		}
	}
	return c.uvarintSlow(eofOK)
}

// uvarintSlow is the complete varint reader and the out-of-line half of uvarint:
// the empty-cursor EOF/INCOMPLETE cases and every multi-byte value.
//
// go:noinline is load-bearing: without it the compiler folds the loop back into
// uvarint, restoring the original per-call cost and erasing the fast path's win.
//
//go:noinline
func (c *cursor) uvarintSlow(eofOK bool) (uint64, error) {
	if c.pos >= len(c.buf) {
		if eofOK {
			return 0, io.EOF
		}
		return 0, ErrIncomplete // expected a varint, but the stream ended
	}
	var val uint64
	var shift uint
	for i := c.pos; i < len(c.buf); i++ {
		b := c.buf[i]
		// Reject an overlong (>64-bit) varint *before* OR-ing the byte in, so
		// its high payload bits are never silently shifted out (§4.1/§6.3): on
		// the 10th byte (shift == 63) only the single low payload bit fits
		// below bit 63, so any higher bit is a >64-bit overflow.
		if shift+7 > 64 && (b&0x7F)>>(64-shift) != 0 {
			return 0, ErrInvalidMsg // payload spills past bit 63: overlong, malformed
		}
		val |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			c.pos = i + 1
			return val, nil
		}
		shift += 7
		// A continuation bit on the 10th byte demands an 11th: the varint is
		// overlong regardless of whether that byte is present.
		if shift >= 64 {
			return 0, ErrInvalidMsg // varint > 64 bits: malformed
		}
	}
	return 0, ErrIncomplete // ran off the end mid-varint: truncated, not malformed
}

// take returns the next n bytes as a subslice of buf (zero-copy) and advances. A
// payload shorter than n means the stream ended mid-field: ErrIncomplete (§7).
func (c *cursor) take(n uint64) ([]byte, error) {
	if n > uint64(len(c.buf)-c.pos) {
		return nil, ErrIncomplete
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
	if err := c.lim.checkFixlen(sub, length); err != nil {
		return 0, 0, err
	}
	return length, sub, nil
}

// arrayCount reads an array's leading element count. Zero is valid — an empty
// array (§4.7/§4.8); only a count past arrayMax is rejected as ErrInvalidMsg.
func (c *cursor) arrayCount() (uint64, error) {
	n, err := c.uvarint(false)
	if err != nil {
		return 0, err
	}
	if n > arrayMax {
		return 0, ErrInvalidMsg
	}
	if err := c.lim.checkArrayCount(n); err != nil {
		return 0, err
	}
	return n, nil
}

// accept drives v over the buffer. depth is the number of sequences currently
// open (0 at the top level); when depth > 0 we are nested, so a clean
// end-of-buffer means the message stopped inside an open sequence (INCOMPLETE),
// and a sequence-end returns. Recursion is bounded by MaxDepth so a hostile,
// deeply nested message is rejected rather than overflowing the Go stack (§4.9).
func (c *cursor) accept(v Visitor, depth int) error {
	nested := depth > 0
	// One type-assertion per scope, not per field: nil unless this visitor opts
	// into the header hooks (HeaderVisitor). A visitor without them pays only a
	// predictable nil branch per array/fixlen field, so the max-speed path is
	// unchanged.
	hv, _ := v.(HeaderVisitor)
	for {
		h, err := c.uvarint(true)
		if err != nil {
			if err == io.EOF {
				if nested {
					return ErrIncomplete // ended inside an open sequence (§7)
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
			if err := c.acceptFixlen(v, hv, id); err != nil {
				return err
			}
		case TypeVarintArrayUnsigned:
			n, err := c.arrayCount()
			if err != nil {
				return err
			}
			// Header hook at the count word, before the truncation check below, so
			// a schema over-count is INVALID even when the array is then truncated
			// (§5.2). No-op unless the visitor declares a bound.
			if hv != nil {
				if err := hv.ArrayBegin(id, int(n)); err != nil {
					return err
				}
			}
			// Each varint element is at least one byte, so a count exceeding the
			// bytes left cannot be satisfied: fail fast as INCOMPLETE (§7) instead
			// of allocating a huge slice from the untrusted count (issue #40).
			if n > uint64(len(c.buf)-c.pos) {
				return ErrIncomplete
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
			if hv != nil {
				if err := hv.ArrayBegin(id, int(n)); err != nil {
					return err
				}
			}
			// See TypeVarintArrayUnsigned: reject a count larger than the bytes
			// remaining before allocating from it (issue #40).
			if n > uint64(len(c.buf)-c.pos) {
				return ErrIncomplete
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
			if err := c.acceptFixlenArray(v, hv, id); err != nil {
				return err
			}
		case TypeSequenceStart:
			if depth >= MaxDepth {
				return ErrInvalidMsg // nesting past MaxDepth (§4.9)
			}
			child, err := v.BeginSequence(id)
			if err != nil {
				return err
			}
			if err := c.accept(child, depth+1); err != nil {
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

func (c *cursor) acceptFixlen(v Visitor, hv HeaderVisitor, id ID) error {
	n, sub, err := c.fixlenHeader()
	if err != nil {
		return err
	}
	// Header hook at the length word, before take() below can report the payload
	// truncated: an over-maxlen string/blob stays INVALID (§5.2).
	if hv != nil {
		if err := hv.FixlenHeader(id, int(sub), int(n)); err != nil {
			return err
		}
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
		// c.take yields the complete payload (or ErrIncomplete) before this
		// check, so validity is judged on the whole string, not a chunk slice
		// (§6.4). Gated by SOFAB_STRICT_UTF8 (default ON); when off the bytes pass
		// through verbatim.
		if c.lim.strictUTF8 && !utf8.Valid(b) {
			return ErrInvalidMsg // invalid UTF-8 (§5.2 INVALID)
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

func (c *cursor) acceptFixlenArray(v Visitor, hv HeaderVisitor, id ID) error {
	n, err := c.arrayCount()
	if err != nil {
		return err
	}
	if hv != nil {
		if err := hv.ArrayBegin(id, int(n)); err != nil {
			return err
		}
	}
	// A fixlen array always carries its fixlen_word, even when empty (§4.8), so
	// the element subtype is always on the wire: an empty fp32 array dispatches
	// to Float32Array and an empty fp64 array to Float64Array.
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
