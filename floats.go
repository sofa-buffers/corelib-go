package sofab

import "unsafe"

// Bit-exact equality of float arrays (generator#636).
//
// The generated encoder omits a field iff its value equals its declared default
// (MESSAGE_SPEC §2), and floats round-trip bit-for-bit (CORELIB_PLAN §4.6): the
// array [-0.0, 1.5] is NOT the default [0.0, 1.5]. An IEEE elementwise compare
// (slices.Equal) says it is, because -0.0 == +0.0, and would drop the element
// from the wire. It would also call a NaN unequal to itself and so re-encode a
// default NaN forever.
//
// The comparison is the same for every schema — only the element type and the
// default differ, and both are arguments — so it lives here rather than being
// emitted into each generated package.

// BitsEqual reports whether a and b have the same length and, at every index,
// the same IEEE-754 bit pattern: 32 bits for float32, 64 for float64.
//
// There is no == on a float anywhere in it: +0.0 and -0.0 differ, and a NaN
// equals another NaN only when the patterns are identical, payload included.
// The lengths are compared first, so arrays of different length cost one
// comparison. It neither allocates nor mutates, and accepts a field slice and a
// constant default literal alike. Arrays of up to shortLen elements are compared
// element by element, longer ones by their first element and then a single
// memory compare; all read the same bits, so the result does not depend on the
// strategy:
//
//	sofab.BitsEqual(m.A, []float32{0, 1.5})
func BitsEqual[S ~[]E, E float32 | float64](a, b S) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) > shortLen {
		// A field usually leaves its default at the first element, and that
		// answer must not pay for the block compare's call and setup: rejected
		// here, one compare (generator#708).
		if bitsDiffer(&a[0], &b[0]) {
			return false
		}
		return bytesEqual(unsafe.Pointer(unsafe.SliceData([]E(a))), unsafe.Pointer(unsafe.SliceData([]E(b))), len(a)*int(unsafe.Sizeof(a[0])))
	}
	for i := range a {
		if bitsDiffer(&a[i], &b[i]) {
			return false
		}
	}
	return true
}

// shortLen is the longest array compared element by element: a call into
// memequal costs more than the compare, and short arrays are the common case
// (every serialize and isDefault runs one).
const shortLen = 4

// bytesEqual compares n bytes at a and b. A float slice is len*Sizeof(E)
// contiguous bytes with no padding, and two values have the same bit pattern
// exactly when their bytes are equal, so one memequal answers for the whole
// array. The string conversion of a byte view does not copy (the compiler
// lowers the comparison to memequal) and nothing is retained or written.
func bytesEqual(a, b unsafe.Pointer, n int) bool {
	return unsafe.String((*byte)(a), n) == unsafe.String((*byte)(b), n)
}

// bitsDiffer reports whether *p and *q have different bit patterns: 4 bytes
// for a float32, 8 for a float64. Sizeof(E) is a constant per instantiation,
// so the branch is resolved at compile time.
func bitsDiffer[E float32 | float64](p, q *E) bool {
	if unsafe.Sizeof(*p) == 4 {
		return *(*uint32)(unsafe.Pointer(p)) != *(*uint32)(unsafe.Pointer(q))
	}
	return *(*uint64)(unsafe.Pointer(p)) != *(*uint64)(unsafe.Pointer(q))
}
