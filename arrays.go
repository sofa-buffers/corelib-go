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
//
// It is a CONVERSION, not a check: an element outside T's range is masked here,
// which is what MESSAGE_SPEC §7.1 forbids for a decoded message, so the caller
// must have applied the declared width already. On the visitor surface — the
// only place a []uint64 array comes from — that means implementing
// ElemBoundVisitor, which lets the decoder reject an over-width element while
// the array goes past (see visitor.go); a caller that instead reads through
// ReadUnsignedArray[T] gets the bound from T and never needs this helper. The
// signature stays value-in/value-out deliberately: generated code calls it in
// expression position, and the bound belongs where the array is still being
// decoded, not after the fact (issue #83).
func NarrowUnsigned[T ~uint8 | ~uint16 | ~uint32 | ~uint64](v []uint64) []T {
	out := make([]T, len(v))
	for i, x := range v {
		out[i] = T(x)
	}
	return out
}

// NarrowSigned is NarrowUnsigned for signed element widths, and carries the same
// caveat: it masks rather than checks, so the §7.1 width bound must already have
// been applied — by ElemBoundVisitor on the visitor path, or by reading through
// ReadSignedArray[T], which takes the bound from T.
func NarrowSigned[T ~int8 | ~int16 | ~int32 | ~int64](v []int64) []T {
	out := make([]T, len(v))
	for i, x := range v {
		out[i] = T(x)
	}
	return out
}
