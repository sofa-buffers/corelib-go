package sofab

// Chunk reassembly: the carry buffer a caller needs when a payload arrives in
// pieces (CORELIB_PLAN §5.6, §6).
//
// It sits beside the collector layer for the same reason: reassembly is the
// same code for every schema — a length, an offset and a chunk — so it belongs
// in the corelib rather than inlined into every destination that has to survive
// a chunk boundary. The sibling ports emit exactly this by hand today
// (generator#345).

// accChunk bounds the buffer a fresh accumulation opens with, so an announced
// length is never an allocation on its own.
const accChunk = 64 << 10

// PayloadAcc reassembles one payload delivered in chunks, and hands it over
// whole the moment the last byte of it arrives.
//
// It exists because a chunk boundary may fall ANYWHERE, mid-field included
// (CORELIB_PLAN §6), while every property that has to be judged on a payload —
// UTF-8 validity above all (§6.4: "a chunk boundary MUST NOT affect the
// outcome") — is a property of the whole. A destination fed piecewise therefore
// needs somewhere to carry the prefix, and that carry is the only thing that
// must not be a schema decision: how long the payload is comes from the wire,
// how much of it may be accepted from a bound the caller applies BEFORE the
// first chunk is offered here.
//
// It is what a destination binds a string or blob field through. Visitor.String
// and Visitor.Bytes deliver a payload IN PIECES (§6.6.3) — the total, this
// piece's offset, and a window into the caller's own fed bytes — because a
// callback carrying the whole value would oblige the CODEC to build it from the
// wire's size, which §6.6 forbids. The building belongs on this side of the
// callback, where the storage is the caller's: that is the static helper layer
// of §6.6.1, and this is it.
//
// The zero value is ready to use. It is NOT safe for concurrent use, and one
// accumulator carries one payload at a time.
type PayloadAcc struct {
	// buf holds the prefix accumulated so far, and is handed to the caller
	// (never reused afterwards) once the payload completes.
	buf []byte
	// total is the payload length the current accumulation was opened for, so a
	// later chunk that announces a different length is caught rather than
	// spliced into a payload it does not belong to.
	total int
}

// Take contributes one chunk of a payload announced as total bytes, arriving at
// byte offset within it, and reports the payload once it is complete: the whole
// payload and true on the chunk that finishes it, nil and false while bytes are
// still outstanding.
//
// Chunks must be contiguous — offset 0 opens a payload and every later chunk
// must start where the previous one ended. A chunk that overlaps or skips
// (including one announcing a different total) is refused: the accumulation is
// discarded and the call reports incomplete, so a caller cannot be handed a
// payload silently spliced together in the wrong order. The next offset-0 chunk
// starts cleanly.
//
// Two ownership rules, both of which a caller keeping the result must respect:
//
//   - When the whole payload arrives in the FIRST chunk, that chunk is returned
//     as-is — no copy, no allocation. It therefore aliases the caller's own
//     buffer, exactly as the slices AcceptBytes hands a visitor do.
//   - Otherwise the accumulator's buffer is returned and the accumulator drops
//     it, so the bytes are the caller's and the next payload starts from a fresh
//     buffer rather than overwriting them.
//
// A hostile total costs no memory up front: the accumulation opens at
// accChunk at most and grows as bytes actually arrive, so a claimed length near
// 2^31 that is never delivered allocates in proportion to what did arrive. That
// hardening belongs HERE and not in the codec, which is the §6.6.1 division:
// this is the static helper layer, it allocates on the caller's behalf, and the
// codec it sits behind sizes nothing at all from the wire.
func (a *PayloadAcc) Take(total, offset int, chunk []byte) ([]byte, bool) {
	if total < 0 || offset < 0 {
		a.Reset()
		return nil, false
	}
	if offset == 0 {
		// A new payload: whatever a previous one left behind is stale.
		a.buf, a.total = a.buf[:0], total
		if len(chunk) >= total {
			// Complete in one piece — the common case, and the one worth not
			// copying for. Trimmed to total so a caller that over-delivers (a
			// chunk carrying the bytes of the next field too) still gets exactly
			// this payload. Our own buffer stays empty and reusable — what is
			// returned is the caller's chunk, not it.
			a.total = 0
			return chunk[:total], true
		}
		if cap(a.buf) == 0 {
			a.buf = make([]byte, 0, min(total, accChunk))
		}
	} else if total != a.total || offset != len(a.buf) {
		a.Reset()
		return nil, false
	}
	a.buf = append(a.buf, chunk...)
	if len(a.buf) < total {
		return nil, false
	}
	// Hand the buffer over rather than keep it: the caller owns the payload from
	// here, and the next one must not write over it.
	p := a.buf[:total]
	a.buf, a.total = nil, 0
	return p, true
}

// Reset discards a partially accumulated payload. The buffer is kept for the
// next one — it is this accumulator's own, never a payload already handed out —
// so a caller that resets between messages does not pay for a fresh allocation
// each time.
func (a *PayloadAcc) Reset() {
	a.buf, a.total = a.buf[:0], 0
}
