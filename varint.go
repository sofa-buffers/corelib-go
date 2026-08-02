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
// Measured on bench/run_callgrind.sh: decode of the 1000-element u64 array went
// from ~255 Ir per element to ~45, encode from ~115 to ~30.
//
// THE UNROLLED BLOCK APPEARS MORE THAN ONCE, and the copies must stay in
// lockstep. In this file: putUvarint (single value, encode), uvarintFast
// (single value, decode) and decodeUvarintRun (bulk decode). In encoder.go:
// putUvarintRun and putZigzagRun (bulk encode). Each copy exists because the
// call it replaces was measured to cost 15-25 % of its workload, not for taste.
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
// loop. Passing b[n:] instead cost a slice bounds check and a three-register
// header rebuild per element — 36 of the 70 Ir an element took.
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

// varintTailStatus reports how a varint decode near the end of a buffer ended.
type varintTailStatus uint8

const (
	varintOK        varintTailStatus = iota // a complete varint was decoded
	varintTruncated                         // ran off the end mid-varint (INCOMPLETE)
	varintOverflow                          // exceeds 64 bits (INVALID)
)

// uvarintTail decodes a varint from b[p:] when fewer than maxVarintLen bytes
// remain, so every byte needs an end-of-buffer test. It is the cold counterpart
// of uvarintFast and keeps the identical value/overflow semantics; only the
// truncation outcome is extra.
func uvarintTail(b []byte, p int) (v uint64, np int, st varintTailStatus) {
	var shift uint
	for i := p; i < len(b); i++ {
		x := uint64(b[i])
		if shift == 63 {
			// Same single-bit rule as uvarintFast's tenth byte.
			if x > 1 {
				return 0, p, varintOverflow
			}
			return v | (x << 63), i + 1, varintOK
		}
		v |= (x & 0x7F) << shift
		if x < 0x80 {
			return v, i + 1, varintOK
		}
		shift += 7
	}
	return 0, p, varintTruncated
}

// varintChunk is how many elements one decodeUvarintRun call fills when the
// caller has to stage them (fillSigned). Large enough that the call amortizes
// to nothing per element, small enough that zeroing the staging array does not
// itself become the cost on a short array.
const varintChunk = 32

// decodeUvarintRun decodes up to len(dst) varints from b starting at p. It
// returns how many it decoded, the position just past them, and the status.
//
// It decodes only while at least maxVarintLen bytes remain, so no element needs
// a per-byte end-of-buffer test; it stops early (got < len(dst), status
// varintOK) when the buffer's tail is reached, leaving those last elements to
// the caller's bounds-checked uvarintTail path.
//
// This is the bulk decoder BOTH array element types go through — the unsigned
// destination copies its output, the signed one zigzag-maps it — so the unrolled
// loop exists once rather than once per element type. Calling uvarintFast per
// element instead cost about 15 % of the array decode in call overhead and a
// re-derived window; staging a chunk pays that once per varintChunk elements and
// adds only a stack store and load each.
func decodeUvarintRun(b []byte, p int, dst []uint64) (got, np int, st varintTailStatus) {
	// The window advances as a slice rather than being re-cut from b at each
	// element. Carrying `len(s) >= maxVarintLen` in the LOOP CONDITION is what
	// makes this cheap: the prove pass then knows every one of s[0]..s[9] is in
	// range, so the ten loads need no bounds check and no per-element slice is
	// built. Re-slicing b[p:p+maxVarintLen] per element instead cost 10 Ir an
	// element — 12 % of the whole decode — for the bounds check and the
	// pointer/length/capacity triple it had to materialize.
	s := b[p:]
	i := 0
	for ; i < len(dst) && len(s) >= maxVarintLen; i++ {
		var v, x uint64
		x = uint64(s[0])
		v = x & 0x7F
		if x < 0x80 {
			dst[i] = v
			s = s[1:]
			continue
		}
		x = uint64(s[1])
		v |= (x & 0x7F) << 7
		if x < 0x80 {
			dst[i] = v
			s = s[2:]
			continue
		}
		x = uint64(s[2])
		v |= (x & 0x7F) << 14
		if x < 0x80 {
			dst[i] = v
			s = s[3:]
			continue
		}
		x = uint64(s[3])
		v |= (x & 0x7F) << 21
		if x < 0x80 {
			dst[i] = v
			s = s[4:]
			continue
		}
		x = uint64(s[4])
		v |= (x & 0x7F) << 28
		if x < 0x80 {
			dst[i] = v
			s = s[5:]
			continue
		}
		x = uint64(s[5])
		v |= (x & 0x7F) << 35
		if x < 0x80 {
			dst[i] = v
			s = s[6:]
			continue
		}
		x = uint64(s[6])
		v |= (x & 0x7F) << 42
		if x < 0x80 {
			dst[i] = v
			s = s[7:]
			continue
		}
		x = uint64(s[7])
		v |= (x & 0x7F) << 49
		if x < 0x80 {
			dst[i] = v
			s = s[8:]
			continue
		}
		x = uint64(s[8])
		v |= (x & 0x7F) << 56
		if x < 0x80 {
			dst[i] = v
			s = s[9:]
			continue
		}
		// Tenth byte: the same single-bit rule as uvarintFast.
		x = uint64(s[9])
		if x > 1 {
			return i, len(b) - len(s), varintOverflow
		}
		dst[i] = v | (x << 63)
		s = s[10:]
	}
	return i, len(b) - len(s), varintOK
}
