package sofab

import "math"

// Helpers the generated encode/decode paths need for every fixed-length array.
// They carry no schema knowledge — the count and the element default are passed
// in — so they belong here rather than being emitted into every generated
// package.

// TrimTail returns a[:M'], where M' is one past the last element that differs
// from zero (0 when every element equals it).
//
// A count: N array is fixed-length: it always holds exactly N logical elements,
// and its canonical wire form carries only the first M'. The trailing run of
// defaults is not emitted; the decoder rebuilds it from the schema count
// (MESSAGE_SPEC §3). A dynamic (count-less) array has no N to rebuild from and
// is never trimmed.
func TrimTail[T comparable](a []T, zero T) []T {
	n := len(a)
	for n > 0 && a[n-1] == zero {
		n--
	}
	return a[:n]
}

// TrimTailFloat32 is TrimTail for float32, comparing by BIT PATTERN rather than
// by ==.
//
// -0.0 == 0.0 is true, so an == comparison would trim a trailing -0.0 and change
// the bytes a round-trip produces — a §4.6 bit-exactness violation. A NaN
// compares unequal to everything including itself, so it is never mistaken for
// the default either way; the bit test states that intent directly.
func TrimTailFloat32(a []float32) []float32 {
	n := len(a)
	for n > 0 && math.Float32bits(a[n-1]) == 0 {
		n--
	}
	return a[:n]
}

// TrimTailFloat64 is TrimTail for float64, comparing by bit pattern — see
// TrimTailFloat32.
func TrimTailFloat64(a []float64) []float64 {
	n := len(a)
	for n > 0 && math.Float64bits(a[n-1]) == 0 {
		n--
	}
	return a[:n]
}

// PadTo grows a to exactly n elements with the element default.
//
// The decode counterpart of TrimTail: a fixed-count array materializes exactly
// its schema count regardless of the wire count, so a growable container has to
// put back the trailing default run the encoder elided (MESSAGE_SPEC §3). A
// slice that is already n or longer is returned unchanged.
func PadTo[T any](a []T, n int, zero T) []T {
	for len(a) < n {
		a = append(a, zero)
	}
	return a
}

// NarrowUnsigned copies a 64-bit-widened native array down to its declared
// element width. The decode API delivers every unsigned array as []uint64
// (one path for all widths); the generated field is typed to the schema.
func NarrowUnsigned[T ~uint8 | ~uint16 | ~uint32 | ~uint64](v []uint64) []T {
	out := make([]T, len(v))
	for i, x := range v {
		out[i] = T(x)
	}
	return out
}

// NarrowSigned is NarrowUnsigned for signed element widths.
func NarrowSigned[T ~int8 | ~int16 | ~int32 | ~int64](v []int64) []T {
	out := make([]T, len(v))
	for i, x := range v {
		out[i] = T(x)
	}
	return out
}
