package sofab

// Varint primitives, shared by the encoder and the cursor decoder.
//
// Both directions are fully unrolled over the ten possible byte positions. That
// shape is deliberate and is what makes them cheap:
//
//   - every shift is a COMPILE-TIME CONSTANT. Go defines x<<s as 0 for s>=64,
//     which x86 SHL/SHR do not (they mask the count to 63), so a variable shift
//     costs an extra compare-and-mask sequence on every use. The rolled loops
//     these replaced ran two variable shifts per byte; the unrolled form runs
//     none.
//   - the loop-carried `shift` and the per-byte overflow bookkeeping disappear.
//     The >64-bit rejection is a property of the tenth byte alone, so it is
//     tested once, in the tenth block, instead of being re-derived per byte.
//   - one bounds-check hint (`_ = b[9]`) covers all ten indices, so the prove
//     pass drops the per-byte checks.
//
//
// THE UNROLLED BLOCK APPEARS MORE THAN ONCE, and the copies must stay in
// lockstep. In this file: putUvarint (single value, encode), uvarintFast
// (single value, decode) and decodeUvarintRun (bulk decode). In encoder.go:
// putUvarintRun and putZigzagRun (bulk encode). Each copy exists because the
// call it replaces is a real cost on its hot path, not for taste.
// varint_internal_test.go pins every one of them to the naive reference encoder
// and to each other, so a change made to one and not the rest fails the suite
// rather than corrupting the wire.

// maxVarintLen is the largest number of bytes a 64-bit varint occupies:
// ceil(64/7) = 10.
const maxVarintLen = 10

// putUvarint writes v as a base-128 varint into b at p and returns the position
// just past it. b MUST have maxVarintLen bytes of room at p; callers reserve
// that headroom before calling (see Encoder.putVarint).
//
// It takes (buffer, offset) rather than a pre-sliced tail so that a caller
// writing many elements keeps one slice header live in registers for the whole
// loop. Passing b[n:] instead costs a slice bounds check and a three-register
// header rebuild per element.
//
// The encoding is always minimal — the fewest bytes that represent v — which is
// the canonical form CORELIB_PLAN §4.1 requires of an encoder.
func putUvarint(b []byte, p int, v uint64) int {
	// Re-slice to a fixed-width window so the ten stores can use CONSTANT
	// indices. Indexing b[p+k] directly defeats bounds-check elimination — the
	// prove pass will not relate p+k to p+9 — and cost a check on every one of
	// the ten stores. Here len(s) folds to maxVarintLen and all ten checks go.
	s := b[p : p+maxVarintLen]
	if v < 0x80 {
		s[0] = byte(v)
		return p + 1
	}
	s[0] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[1] = byte(v)
		return p + 2
	}
	s[1] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[2] = byte(v)
		return p + 3
	}
	s[2] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[3] = byte(v)
		return p + 4
	}
	s[3] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[4] = byte(v)
		return p + 5
	}
	s[4] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[5] = byte(v)
		return p + 6
	}
	s[5] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[6] = byte(v)
		return p + 7
	}
	s[6] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[7] = byte(v)
		return p + 8
	}
	s[7] = byte(v) | 0x80
	v >>= 7
	if v < 0x80 {
		s[8] = byte(v)
		return p + 9
	}
	s[8] = byte(v) | 0x80
	v >>= 7
	s[9] = byte(v)
	return p + 10
}

// uvarintFast decodes a base-128 varint from b[p:] when at least maxVarintLen
// bytes are available, so no per-byte end-of-buffer test is needed. It returns
// the value and the position just past the varint.
//
// The tenth byte carries the value's top bit and nothing else: at shift 63 only
// one payload bit fits below bit 64, and a continuation flag there would demand
// an eleventh byte. Both conditions collapse into the single test `x > 1`, which
// is exactly the >64-bit rejection of CORELIB_PLAN §4.1 (ok=false ⇒ malformed,
// never merely truncated — the caller maps it to ErrInvalidMsg).
//
// A non-minimal encoding within the 64-bit bound is accepted and normalized to
// the value it denotes, as §4.1 requires ("minimality on encode, tolerance on
// decode").
func uvarintFast(b []byte, p int) (v uint64, np int, ok bool) {
	// A fixed-width window, as in putUvarint: len(s) folds to maxVarintLen, so
	// this single slice check covers all ten loads with constant indices.
	s := b[p : p+maxVarintLen]

	x := uint64(s[0])
	v = x & 0x7F
	if x < 0x80 {
		return v, p + 1, true
	}
	x = uint64(s[1])
	v |= (x & 0x7F) << 7
	if x < 0x80 {
		return v, p + 2, true
	}
	x = uint64(s[2])
	v |= (x & 0x7F) << 14
	if x < 0x80 {
		return v, p + 3, true
	}
	x = uint64(s[3])
	v |= (x & 0x7F) << 21
	if x < 0x80 {
		return v, p + 4, true
	}
	x = uint64(s[4])
	v |= (x & 0x7F) << 28
	if x < 0x80 {
		return v, p + 5, true
	}
	x = uint64(s[5])
	v |= (x & 0x7F) << 35
	if x < 0x80 {
		return v, p + 6, true
	}
	x = uint64(s[6])
	v |= (x & 0x7F) << 42
	if x < 0x80 {
		return v, p + 7, true
	}
	x = uint64(s[7])
	v |= (x & 0x7F) << 49
	if x < 0x80 {
		return v, p + 8, true
	}
	x = uint64(s[8])
	v |= (x & 0x7F) << 56
	if x < 0x80 {
		return v, p + 9, true
	}
	// Tenth byte: payload must be a single bit and must terminate the varint.
	// Anything else spills past bit 63 or demands an eleventh byte — both are
	// the >64-bit malformed case.
	x = uint64(s[9])
	if x > 1 {
		return 0, p, false
	}
	return v | (x << 63), p + 10, true
}
