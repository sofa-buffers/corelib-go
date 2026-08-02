package sofab

// Helpers the generated encode/decode paths need for arrays. They carry no
// schema knowledge — the element type is the type parameter — so they belong
// here rather than being emitted into every generated package.
//
// There is deliberately NO trailing-default trim and no fill-to-count helper
// here. A schema `count: N` is a CAPACITY, not a length (MESSAGE_SPEC §3): the
// wire count M is the array's length, an encoder writes every element the slice
// holds, and a decoder materializes exactly the M elements the wire carried.
// []uint32{1, 2, 0, 0} and []uint32{1, 2} are different values with different
// bytes. This package once exported TrimTail/TrimTailFloat32/TrimTailFloat64 and
// PadTo for the older fixed-length reading, under which the encoder elided the
// trailing default run and the decoder refilled [M, N); that reading was
// retracted, because the trim silently shortens an array once N is a capacity.
// The helpers are gone rather than deprecated, since a generated caller that
// still invoked them would emit non-conformant bytes.

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
