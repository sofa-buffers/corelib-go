package sofab

import "math"

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
// constant default literal alike:
//
//	sofab.BitsEqual(m.A, []float32{0, 1.5})
func BitsEqual[S ~[]E, E float32 | float64](a, b S) bool {
	if len(a) != len(b) {
		return false
	}
	// []E drops a named slice type, so the switch sees []float32 or []float64
	// whatever S is.
	switch x := any([]E(a)).(type) {
	case []float32:
		y := any([]E(b)).([]float32)
		y = y[:len(x)]
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
	case []float64:
		y := any([]E(b)).([]float64)
		y = y[:len(x)]
		for i := range x {
			if math.Float64bits(x[i]) != math.Float64bits(y[i]) {
				return false
			}
		}
	}
	return true
}
