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
	lim limits
}

// uvarint reads a base-128 varint at pos. If eofOK, a field boundary with no
// bytes left is reported as io.EOF (clean end of stream); a varint truncated by
// the end of the buffer is ErrIncomplete (INCOMPLETE, §7) — it ran out of bytes
// mid-field. A varint that exceeds 64 bits is ErrInvalidMsg (malformed).
//
// The single-byte case (payload < 0x80 — every field header for id<16, and every
// small scalar/count) is peeled into a lean fast path: one bounds-checked load and
// a return, with none of the multi-byte loop's shift/overflow bookkeeping. That is
// the common read, so it is worth the peel even though the two-value return keeps
// uvarint just over the inline budget.
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
// go:noinline is load-bearing: without it the compiler folds the body back into
// uvarint, restoring the original per-call cost and erasing the fast path's win.
//
// The two branches below differ only in whether an end-of-buffer test is needed
// per byte. Away from the buffer's end — the overwhelmingly common case, and the
// whole of a multi-element array — uvarintFast runs the unrolled constant-shift
// decoder with a single bounds-check hint; only the last few bytes of the
// message fall into uvarintTail. Value and overflow semantics are identical in
// both (varint.go).
//
//go:noinline
func (c *cursor) uvarintSlow(eofOK bool) (uint64, error) {
	buf, p := c.buf, c.pos
	if p >= len(buf) {
		if eofOK {
			return 0, io.EOF
		}
		return 0, ErrIncomplete // expected a varint, but the stream ended
	}
	if len(buf)-p >= maxVarintLen {
		v, np, ok := uvarintFast(buf, p)
		if !ok {
			return 0, ErrInvalidMsg // > 64 bits: overlong, malformed
		}
		c.pos = np
		return v, nil
	}
	v, np, st := uvarintTail(buf, p)
	switch st {
	case varintOK:
		c.pos = np
		return v, nil
	case varintOverflow:
		return 0, ErrInvalidMsg // varint > 64 bits: malformed
	}
	return 0, ErrIncomplete // ran off the end mid-varint: truncated, not malformed
}

// fillUnsigned decodes len(out) varint elements into out.
//
// Decoding goes through decodeUvarintRun, the shared bulk decoder (varint.go).
// Calling c.uvarint per element instead would pay three layers — the inlined
// single-byte probe, the out-of-line uvarintSlow dispatch, and uvarintFast
// itself — plus a reload of c.buf/c.pos from memory each time.
func (c *cursor) fillUnsigned(out []uint64) error {
	buf, p := c.buf, c.pos
	// Unsigned elements are the bulk decoder's own output type, so it fills the
	// destination in place — no staging, no copy.
	got, np, st := decodeUvarintRun(buf, p, out)
	if st != varintOK {
		return tailErr(st)
	}
	p = np
	// decodeUvarintRun stops on reaching the last maxVarintLen bytes of the
	// buffer, where an element could run off the end; those go through the
	// bounds-checked tail path.
	for i := got; i < len(out); i++ {
		v, np, st := uvarintTail(buf, p)
		if st != varintOK {
			return tailErr(st)
		}
		out[i], p = v, np
	}
	c.pos = p
	return nil
}

// fillSigned is fillUnsigned for a zigzag-encoded signed array.
func (c *cursor) fillSigned(out []int64) error {
	buf, p := c.buf, c.pos
	// Signed elements are not the bulk decoder's output type, so a chunk is
	// staged on the stack and zigzag-mapped out. varintChunk is sized so that
	// zeroing the staging array stays small against the per-element call
	// overhead it saves.
	var stage [varintChunk]uint64
	i := 0
	for i < len(out) {
		want := min(len(out)-i, varintChunk)
		got, np, st := decodeUvarintRun(buf, p, stage[:want])
		if st != varintOK {
			return tailErr(st)
		}
		for k, v := range stage[:got] {
			out[i+k] = zigzagDecode(v)
		}
		i, p = i+got, np
		if got < want {
			break // reached the buffer's tail region
		}
	}
	for ; i < len(out); i++ {
		v, np, st := uvarintTail(buf, p)
		if st != varintOK {
			return tailErr(st)
		}
		out[i], p = zigzagDecode(v), np
	}
	c.pos = p
	return nil
}

// scanTruncatedArray decides the outcome for an integer array whose element
// count exceeds the bytes remaining. Because every element is at least one byte,
// the count can never be satisfied, so the array is truncated (INCOMPLETE, §7) —
// UNLESS an element already in hand is malformed, since INVALID dominates
// INCOMPLETE (§5.2). The elements are walked as varints (no allocation from the
// untrusted count — see issue #40): an overlong one (>64 bits) is INVALID, and
// running off the end mid-element or exhausting the buffer is the expected
// truncation. This is what keeps `count=11` over ten all-continuation bytes
// INVALID rather than INCOMPLETE (issue #66).
func (c *cursor) scanTruncatedArray() error {
	buf, p := c.buf, c.pos
	for p < len(buf) {
		_, np, st := uvarintTail(buf, p)
		switch st {
		case varintOverflow:
			return ErrInvalidMsg
		case varintTruncated:
			return ErrIncomplete // ran off the end mid-element (§7)
		}
		p = np
	}
	return ErrIncomplete
}

// tailErr maps a non-OK tail status to its outcome: overflow is malformed
// (INVALID), running off the end is truncation (INCOMPLETE) — §5.2.
func tailErr(st varintTailStatus) error {
	if st == varintOverflow {
		return ErrInvalidMsg
	}
	return ErrIncomplete
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

// hvCache resolves a visitor's optional HeaderVisitor hooks on first need
// rather than on scope entry.
//
// v.(HeaderVisitor) is a runtime itab lookup, and for a concrete visitor type
// that does NOT implement the interface the first lookup for that (type,
// interface) pair walks the type's whole method list — resolveNameOff /
// resolveTypeOff / itabInit. Asking eagerly in every scope makes a nested
// sequence of plain scalars pay for an answer it never uses. The hooks are only
// ever consulted at an array or fixlen field, so that is where the question is
// asked.
//
// Still at most one assertion per scope: the answer is cached, including a nil
// one (known is what distinguishes "no hooks" from "not asked yet").
type hvCache struct {
	hv    HeaderVisitor
	known bool
}

func (c *hvCache) of(v Visitor) HeaderVisitor {
	if !c.known {
		c.hv, _ = v.(HeaderVisitor)
		c.known = true
	}
	return c.hv
}

// accept drives v over the buffer. depth is the number of sequences currently
// open (0 at the top level); when depth > 0 we are nested, so a clean
// end-of-buffer means the message stopped inside an open sequence (INCOMPLETE),
// and a sequence-end returns. Recursion is bounded by MaxDepth so a hostile,
// deeply nested message is rejected rather than overflowing the Go stack (§4.9).
func (c *cursor) accept(v Visitor, depth int) error {
	nested := depth > 0
	// The header hooks are resolved lazily — see hvCache. Still at most one
	// assertion per scope, and none at all in a scope that holds no array or
	// fixlen field.
	var hooks hvCache
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
			if err := c.acceptFixlen(v, hooks.of(v), id); err != nil {
				return err
			}
		case TypeVarintArrayUnsigned:
			n, err := c.arrayCount()
			if err != nil {
				return err
			}
			// Header hook at the count word, before the truncation check below, so
			// a schema over-count is INVALID even when the array is then truncated
			// (§5.2). No-op unless the visitor declares a bound. An integer array
			// carries no second word, so the wire type alone fixes the element kind
			// and the hook fires here — unlike the fixlen array (§4.8).
			if hv := hooks.of(v); hv != nil {
				if err := hv.ArrayBegin(id, ArrayUnsigned, int(n)); err != nil {
					return err
				}
			}
			// Each varint element is at least one byte, so a count exceeding the
			// bytes left can never be satisfied — the array is truncated. Rather
			// than allocate a huge slice from the untrusted count (issue #40),
			// scan the elements in hand: INVALID dominates INCOMPLETE (§5.2), so an
			// element varint already provably overlong is INVALID, not truncation
			// (issue #66).
			if n > uint64(len(c.buf)-c.pos) {
				return c.scanTruncatedArray()
			}
			out := make([]uint64, n)
			if err := c.fillUnsigned(out); err != nil {
				return err
			}
			if err := v.UnsignedArray(id, out); err != nil {
				return err
			}
		case TypeVarintArraySigned:
			n, err := c.arrayCount()
			if err != nil {
				return err
			}
			if hv := hooks.of(v); hv != nil {
				if err := hv.ArrayBegin(id, ArraySigned, int(n)); err != nil {
					return err
				}
			}
			// See TypeVarintArrayUnsigned: a count larger than the bytes remaining
			// is truncated, but scan the elements in hand first so a provably
			// overlong varint stays INVALID (§5.2, issues #40 and #66).
			if n > uint64(len(c.buf)-c.pos) {
				return c.scanTruncatedArray()
			}
			out := make([]int64, n)
			if err := c.fillSigned(out); err != nil {
				return err
			}
			if err := v.SignedArray(id, out); err != nil {
				return err
			}
		case TypeFixlenArray:
			if err := c.acceptFixlenArray(v, hooks.of(v), id); err != nil {
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
		// No UTF-8 validation here (§6.4 "Skipped fields are never validated").
		// The cursor cannot know whether the visitor has a destination for this
		// id: an id the schema does not declare — and a field whose wire type
		// contradicts the schema (MESSAGE_SPEC §7.3) — reaches this callback
		// exactly like a declared one, and validating here would turn a field
		// that is merely skipped into INVALID. Go's string is a byte-container
		// type (§6.4), so the wire bytes pass through verbatim and validation
		// belongs at the destination: generated code calls the Utf8Valid
		// primitive (utf8.go) inside the arm that binds the value. The framing —
		// the fixlen word, the reserved-subtype rejection, arrayMax, and the
		// exact length advance in c.take — is fully checked either way, which is
		// what keeps a skip a length jump and not a peek.
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
	// A fixlen array always carries its fixlen_word, even when empty (§4.8), so
	// the element subtype is always on the wire: an empty fp32 array dispatches
	// to Float32Array and an empty fp64 array to Float64Array.
	//
	// The word is read BEFORE the header hook fires (§4.8 decode order). The
	// count word above already enforced the format ceiling arrayMax and any
	// receiver limit — neither moves — but nothing schema-bound may be judged on
	// the count alone: the subtype decides whether this header is the declared
	// field's value at all, and a header skipped under §7.3 must not have the
	// schema count bound applied to it. Two consequences, both intended: a
	// message ending between the two words is INCOMPLETE (the hook never fires),
	// and an over-count with a contradicting subtype is a skip, not INVALID.
	h, err := c.uvarint(false)
	if err != nil {
		return err
	}
	sub := h & 0x07
	size := h >> 3
	// Validate the word as a FORMAT matter first: §4.8 admits only fp32/4 and
	// fp64/8 as fixlen-array elements. A string or blob subtype, or a width
	// mismatch, is malformed regardless of the schema — it is INVALID here and is
	// never routed to the §7.3 skip path, even though its subtype also
	// contradicts whatever was declared.
	var kind ArrayKind
	switch {
	case sub == fixFp32 && size == 4:
		kind = ArrayFp32
	case sub == fixFp64 && size == 8:
		kind = ArrayFp64
	default:
		return ErrInvalidMsg
	}
	// Only now is the array offered to the header hook, with a kind that names
	// the actual element subtype. Still exactly one call per array field, never
	// per element; count == 0 is offered like any other.
	if hv != nil {
		if err := hv.ArrayBegin(id, kind, int(n)); err != nil {
			return err
		}
	}
	if kind == ArrayFp32 {
		payload, err := c.take(n * 4)
		if err != nil {
			return err
		}
		out := make([]float32, n)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(payload[i*4:]))
		}
		return v.Float32Array(id, out)
	}
	payload, err := c.take(n * 8)
	if err != nil {
		return err
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(payload[i*8:]))
	}
	return v.Float64Array(id, out)
}
