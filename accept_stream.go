package sofab

import (
	"encoding/binary"
	"io"
	"math"
)

// AcceptStream decodes the top-level stream into v exactly as Accept does — the
// same visitor events, the same HeaderVisitor hooks, and the same
// INVALID/INCOMPLETE outcomes — but WITHOUT slurping the whole message into one
// contiguous buffer first. It drives the pull parser directly: every field's
// value is read and dispatched as the reader delivers it, so peak memory is
// bounded by the largest single field rather than by the whole wire image.
//
// This is what makes generated Go decode memory-bounded over a byte stream, as
// CORELIB_PLAN §5.6 requires ("Generated code MUST support processing a message
// in small chunks, not only whole-buffer-at-once"). Accept keeps its slurp — the
// fastest path when the message is already, or cheaply, in memory — and this is
// the reader-driven counterpart for large messages and constrained devices; the
// two produce identical events for identical input.
//
// Byte/blob and string fields handed to v are freshly read buffers, not aliases
// into a shared slurp, so unlike AcceptBytes the visitor need not copy them.
//
// It returns nil at a clean end of stream, a malformed-message error on bad
// input, or a non-EOF reader error verbatim.
func (d *Decoder) AcceptStream(v Visitor) error {
	if d.r == nil {
		d.r = asBufio(d.src)
	}
	return d.acceptStream(v, 0)
}

// acceptStream is the reader-driven twin of cursor.accept: same loop, same
// dispatch, same depth/nesting rules (§4.9), but every read goes through the
// Decoder's pull primitives so no more than one field is ever in memory at once.
// depth is the number of open sequences (0 at the top level); nested, a clean
// end-of-stream means the message stopped inside an open sequence (INCOMPLETE).
func (d *Decoder) acceptStream(v Visitor, depth int) error {
	nested := depth > 0
	// Header hooks resolved lazily, as on the cursor path (see hvCache): at most
	// one type assertion per scope, and none in a scope holding no array/fixlen.
	// The element-bound extension likewise, asked only where an integer array
	// fails to complete (see ebCache). The SOFAB_STRICT_UTF8 policy is handed to
	// the visitor just as lazily, at this scope's first string (see spCache).
	var hooks hvCache
	var bounds ebCache
	var policy spCache
	for {
		h, err := d.readVarint(true)
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
		// The id ceiling binds every header, the sequence-end marker included: its
		// id is discarded (§4.9) but discarded is not unvalidated, so an id above
		// ID_MAX is INVALID here as anywhere else (§5.2, §6.2). One unconditional
		// check, before the wire-type switch.
		if (h >> 3) > uint64(IDMax) {
			return ErrInvalidMsg
		}
		switch t {
		case TypeVarintUnsigned:
			x, err := d.readVarint(false)
			if err != nil {
				return err
			}
			if err := v.Unsigned(id, x); err != nil {
				return err
			}
		case TypeVarintSigned:
			x, err := d.readVarint(false)
			if err != nil {
				return err
			}
			if err := v.Signed(id, zigzagDecode(x)); err != nil {
				return err
			}
		case TypeFixlen:
			if err := d.acceptStreamFixlen(v, hooks.of(v), &policy, id); err != nil {
				return err
			}
		case TypeVarintArrayUnsigned:
			if err := d.acceptStreamUnsignedArray(v, hooks.of(v), &bounds, id); err != nil {
				return err
			}
		case TypeVarintArraySigned:
			if err := d.acceptStreamSignedArray(v, hooks.of(v), &bounds, id); err != nil {
				return err
			}
		case TypeFixlenArray:
			if err := d.acceptStreamFixlenArray(v, hooks.of(v), id); err != nil {
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
			if err := d.acceptStream(child, depth+1); err != nil {
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

func (d *Decoder) acceptStreamFixlen(v Visitor, hv HeaderVisitor, sp *spCache, id ID) error {
	n, sub, err := d.readFixlenHeader()
	if err != nil {
		return err
	}
	// Header hook at the length word, before readRaw below can report the payload
	// truncated: an over-maxlen string/blob stays INVALID (§5.2). Mirrors
	// cursor.acceptFixlen exactly.
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
		b, err := d.readRaw(4)
		if err != nil {
			return err
		}
		return v.Float32(id, math.Float32frombits(binary.LittleEndian.Uint32(b)))
	case fixFp64:
		if n != 8 {
			return ErrInvalidMsg
		}
		b, err := d.readRaw(8)
		if err != nil {
			return err
		}
		return v.Float64(id, math.Float64frombits(binary.LittleEndian.Uint64(b)))
	case fixStr:
		b, err := d.readRaw(n)
		if err != nil {
			return err
		}
		// No UTF-8 validation here, exactly as on the cursor path (§6.4 "Skipped
		// fields are never validated"): the decoder cannot know whether v has a
		// destination for this id, so validation belongs at the destination —
		// generated code calls Utf8Valid inside the arm that binds the value. The
		// POLICY it validates under is this decode's, handed to the scope here
		// exactly as on the cursor path (issue #82).
		sp.deliver(v, d.lim)
		return v.String(id, string(b))
	case fixBlob:
		b, err := d.readRaw(n)
		if err != nil {
			return err
		}
		return v.Bytes(id, b)
	default:
		return ErrInvalidMsg
	}
}

func (d *Decoder) acceptStreamUnsignedArray(v Visitor, hv HeaderVisitor, eb *ebCache, id ID) error {
	n, err := d.arrayCount()
	if err != nil {
		return err
	}
	// Hook at the count word, before any element is read, so a schema over-count
	// is INVALID even when the array is then truncated (§5.2). An integer array
	// carries no second word, so the wire type alone fixes the element kind and
	// the hook fires here — unlike the fixlen array (§4.8).
	if hv != nil {
		if err := hv.ArrayBegin(id, ArrayUnsigned, int(n)); err != nil {
			return err
		}
	}
	out, err := d.readUnsignedElements(n)
	if err != nil {
		// The array never completes, so v.UnsignedArray below never fires and the
		// visitor's own width guard never runs. The elements that DID decode are
		// on the wire all the same, and §5.2 makes one outside its declared width
		// INVALID over the truncation — the reader-driven twin of the bound
		// cursor.scanTruncatedArray applies (generator#267). Nothing is asked on
		// the path where the array arrives whole.
		b := elemBoundOf(eb.of(v), id, ArrayUnsigned)
		for _, x := range out {
			if b.breached(x) {
				return ErrInvalidMsg
			}
		}
		return err
	}
	return v.UnsignedArray(id, out)
}

func (d *Decoder) acceptStreamSignedArray(v Visitor, hv HeaderVisitor, eb *ebCache, id ID) error {
	n, err := d.arrayCount()
	if err != nil {
		return err
	}
	if hv != nil {
		if err := hv.ArrayBegin(id, ArraySigned, int(n)); err != nil {
			return err
		}
	}
	out, err := d.readSignedElements(n)
	if err != nil {
		// See acceptStreamUnsignedArray. These elements are already zigzag-mapped,
		// so they take the bound's decoded form.
		b := elemBoundOf(eb.of(v), id, ArraySigned)
		for _, x := range out {
			if b.breachedSigned(x) {
				return ErrInvalidMsg
			}
		}
		return err
	}
	return v.SignedArray(id, out)
}

func (d *Decoder) acceptStreamFixlenArray(v Visitor, hv HeaderVisitor, id ID) error {
	n, err := d.arrayCount()
	if err != nil {
		return err
	}
	// A fixlen array always carries its fixlen_word, even when empty (§4.8). The
	// word is read BEFORE the hook fires (§4.8 decode order): the count word above
	// already enforced arrayMax and any receiver limit, but nothing schema-bound
	// may be judged on the count alone — the subtype decides whether this header
	// is the declared field's value at all. A message ending between the two words
	// is therefore INCOMPLETE (the hook never fires). Mirrors
	// cursor.acceptFixlenArray.
	h, err := d.readVarint(false)
	if err != nil {
		return err
	}
	sub := h & 0x07
	size := h >> 3
	// Validate the word as a FORMAT matter first: §4.8 admits only fp32/4 and
	// fp64/8. A string/blob subtype or a width mismatch is malformed regardless of
	// the schema — INVALID here, never routed to a §7.3 skip.
	var kind ArrayKind
	switch {
	case sub == fixFp32 && size == 4:
		kind = ArrayFp32
	case sub == fixFp64 && size == 8:
		kind = ArrayFp64
	default:
		return ErrInvalidMsg
	}
	if hv != nil {
		if err := hv.ArrayBegin(id, kind, int(n)); err != nil {
			return err
		}
	}
	if kind == ArrayFp32 {
		out := make([]float32, 0, initialArrayCap(n))
		for i := uint64(0); i < n; i++ {
			b, err := d.readRaw(4)
			if err != nil {
				return err
			}
			out = append(out, math.Float32frombits(binary.LittleEndian.Uint32(b)))
		}
		return v.Float32Array(id, out)
	}
	out := make([]float64, 0, initialArrayCap(n))
	for i := uint64(0); i < n; i++ {
		b, err := d.readRaw(8)
		if err != nil {
			return err
		}
		out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(b)))
	}
	return v.Float64Array(id, out)
}

// readUnsignedElements reads n unsigned varint elements, widened to the 64-bit
// value domain the visitor receives. It never pre-allocates from the untrusted
// count: the slice grows via append as elements actually decode, so a hostile
// count costs memory only in proportion to the bytes delivered before the stream
// ends (issue #40). Elements go through the same batch/tail split as
// ReadUnsignedArray, and a truncated or overlong element surfaces its outcome
// (ErrIncomplete / ErrInvalidMsg) exactly where readVarint decides it — the
// reader-side analogue of cursor.scanTruncatedArray (issue #66).
//
// On failure it returns the elements decoded so far ALONGSIDE the error, so the
// caller can apply the schema's declared-width bound to them: they are on the
// wire, and §5.2 has an out-of-width element outrank the truncation that
// stopped the read (generator#267).
func (d *Decoder) readUnsignedElements(n uint64) ([]uint64, error) {
	out := make([]uint64, 0, initialArrayCap(n))
	var stage [varintChunk]uint64
	for uint64(len(out)) < n {
		want := min(n-uint64(len(out)), uint64(varintChunk))
		got, err := d.readVarintBatch(stage[:want])
		if err != nil {
			return out, err
		}
		if got == 0 {
			// Near the end of the buffer or the stream: one element at a time,
			// where truncation and end-of-stream are decided.
			x, err := d.readVarint(false)
			if err != nil {
				return out, err
			}
			out = append(out, x)
			continue
		}
		out = append(out, stage[:got]...)
	}
	return out, nil
}

// readSignedElements is readUnsignedElements for a zigzag-encoded signed array.
func (d *Decoder) readSignedElements(n uint64) ([]int64, error) {
	out := make([]int64, 0, initialArrayCap(n))
	var stage [varintChunk]uint64
	for uint64(len(out)) < n {
		want := min(n-uint64(len(out)), uint64(varintChunk))
		got, err := d.readVarintBatch(stage[:want])
		if err != nil {
			return out, err
		}
		if got == 0 {
			x, err := d.readVarint(false)
			if err != nil {
				return out, err
			}
			out = append(out, zigzagDecode(x))
			continue
		}
		for _, x := range stage[:got] {
			out = append(out, zigzagDecode(x))
		}
	}
	return out, nil
}
