package sofab

// The collector layer: the visitors a generated decode path binds a
// WRAPPER-SEQUENCE array to, plus the no-op Visitor base they are built on.
//
// A wrapper-sequence array (MESSAGE_SPEC §5.1) is an array whose elements are
// not native scalars — strings, blobs, structs/unions, or arrays — and it
// reaches the visitor as a nested sequence whose child ids ARE the array
// indices. Turning that event stream back into a slice is the same code for
// every schema: place at the id, fill the gap the omitted interior elements
// left, refuse an id past the declared capacity. Only the bounds differ, and a
// bound is an argument — so these belong here, exactly as arrays.go argues for
// NarrowUnsigned/NarrowSigned, rather than being emitted into every generated
// package (generator#345).
//
// Two conventions run through the whole file:
//
//   - Cap is the schema count bound N, ElemMax the schema maxlen bound, and a
//     NEGATIVE value means the schema declares none. N is a CAPACITY, not a
//     length (MESSAGE_SPEC §3): it never adds an element the wire did not
//     carry. All it does here is bound the element id — an id >= N is a
//     schema-bound violation (INVALID, §7.1) — which is checked BEFORE the
//     slice grows, so an announced index near 2^31 costs a comparison and not
//     an allocation. Declaring a count is therefore also what bounds the fill:
//     for an array the schema leaves unbounded, the id the wire carries is the
//     only limit on how far the slice is grown, exactly as it is for the
//     equivalent code the generator emits today.
//   - An element is PLACED at Out[id], never appended, after the gap up to it
//     is filled with element defaults. An interior element equal to the element
//     default is omitted on the wire (§2), so appending would shorten the array
//     by the size of every such gap, and would decode a REOPENED id as a second
//     element instead of overwriting the first (§7.4). The array's LAST element
//     is always written, which is what makes the decoded length — highest
//     present id + 1 — exact.
//
// The collectors are decode destinations, so they are also where a materialized
// string is checked for UTF-8 validity (§6.4): a payload the decoder skips
// never reaches a collector at all, which is the point — validation follows the
// destination, not the wire.

// VisitorBase supplies no-op defaults for every Visitor method, so a type that
// embeds it implements Visitor while overriding only the callbacks its fields
// actually use. Generated objects and the collectors below are both built this
// way.
//
// BeginSequence returns another VisitorBase rather than nil: a nested scope the
// destination does not bind is still decoded and validated, its events simply
// go nowhere. Returning nil would panic the decoder on the first field of that
// scope.
type VisitorBase struct{}

func (VisitorBase) Unsigned(ID, uint64) error         { return nil }
func (VisitorBase) Signed(ID, int64) error            { return nil }
func (VisitorBase) Float32(ID, float32) error         { return nil }
func (VisitorBase) Float64(ID, float64) error         { return nil }
func (VisitorBase) String(ID, string) error           { return nil }
func (VisitorBase) Bytes(ID, []byte) error            { return nil }
func (VisitorBase) UnsignedArray(ID, []uint64) error  { return nil }
func (VisitorBase) SignedArray(ID, []int64) error     { return nil }
func (VisitorBase) Float32Array(ID, []float32) error  { return nil }
func (VisitorBase) Float64Array(ID, []float64) error  { return nil }
func (VisitorBase) BeginSequence(ID) (Visitor, error) { return VisitorBase{}, nil }
func (VisitorBase) EndSequence() error                { return nil }

// StringSeq collects the elements of a string array into Out.
//
// Cap is the array's schema count bound (negative: none) and ElemMax the
// element maxlen bound (negative: none); a breach of either is INVALID, never a
// truncation, and both are latched at the length word — see FixlenHeader.
//
// It embeds StringCheck, so the decode's SOFAB_STRICT_UTF8 policy (§6.4) is
// delivered to it before the scope's first element and WithStrictUTF8(false)
// reaches the element check. The zero value of that policy is STRICT, so a
// collector built by hand and never handed a policy validates.
type StringSeq struct {
	VisitorBase
	StringCheck
	Out     *[]string
	Cap     int
	ElemMax int
}

// ArrayBegin is present only so that one type assertion succeeds. HeaderVisitor
// declares ArrayBegin and FixlenHeader together and the decoder reaches both
// through a single v.(HeaderVisitor) — implementing one alone leaves the
// assertion failing and silently disables the other hook. A string array's
// elements are fixlen fields, so nothing is judged here.
func (s *StringSeq) ArrayBegin(ID, ArrayKind, int) error { return nil }

// FixlenHeader applies both schema bounds at the element's LENGTH WORD, before
// a byte of payload is taken. §5.2 makes INVALID dominate INCOMPLETE, so a
// message truncated right after the word carrying the violating number must
// still be INVALID; judging it in String, which never runs for such a message,
// would report INCOMPLETE instead.
//
// Both bounds sit inside the declared-subtype test: FixlenHeader fires for ANY
// fixlen subtype at this id, and an element whose subtype contradicts the
// declaration was never this array's value (§7.3), so neither its id nor its
// length may be measured against this array's bounds.
func (s *StringSeq) FixlenHeader(id ID, subtype, length int) error {
	if subtype != int(fixStr) {
		return nil
	}
	if s.Cap >= 0 && int(id) >= s.Cap {
		return ErrInvalidMsg
	}
	if s.ElemMax >= 0 && length > s.ElemMax {
		return ErrInvalidMsg
	}
	return nil
}

// String places one element at the index its id names, growing Out with empty
// strings across any gap. The bounds are applied here as well as at the header:
// the header hook fires only on the paths that read a length word, and a
// collector driven without it must still refuse an out-of-range id or an
// over-long element.
func (s *StringSeq) String(id ID, v string) error {
	if s.Cap >= 0 && int(id) >= s.Cap {
		return ErrInvalidMsg
	}
	if s.ElemMax >= 0 && len(v) > s.ElemMax {
		return ErrInvalidMsg
	}
	if !s.UTF8Valid([]byte(v)) {
		return ErrInvalidMsg
	}
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, "")
	}
	(*s.Out)[id] = v
	return nil
}

// BlobSeq is the blob twin of StringSeq: same bounds, same gap-filled
// placement, no UTF-8 check (a blob is bytes). Elements are COPIED, because the
// slice a blob callback delivers may alias the decode buffer (AcceptBytes) and
// the collector keeps it past the call.
type BlobSeq struct {
	VisitorBase
	Out     *[][]byte
	Cap     int
	ElemMax int
}

// ArrayBegin exists for the single type assertion, as StringSeq.ArrayBegin
// explains.
func (s *BlobSeq) ArrayBegin(ID, ArrayKind, int) error { return nil }

// FixlenHeader latches both bounds at the length word, gated on the declared
// subtype; see StringSeq.FixlenHeader for why both properties matter.
func (s *BlobSeq) FixlenHeader(id ID, subtype, length int) error {
	if subtype != int(fixBlob) {
		return nil
	}
	if s.Cap >= 0 && int(id) >= s.Cap {
		return ErrInvalidMsg
	}
	if s.ElemMax >= 0 && length > s.ElemMax {
		return ErrInvalidMsg
	}
	return nil
}

// Bytes places a copy of one element at the index its id names, growing Out
// with nil elements across any gap.
func (s *BlobSeq) Bytes(id ID, v []byte) error {
	if s.Cap >= 0 && int(id) >= s.Cap {
		return ErrInvalidMsg
	}
	if s.ElemMax >= 0 && len(v) > s.ElemMax {
		return ErrInvalidMsg
	}
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, nil)
	}
	(*s.Out)[id] = append([]byte(nil), v...)
	return nil
}

// MessageSeq collects the elements of a struct/union array into Out: each
// element is a nested sequence decoded straight into the element the child id
// names.
//
// T is the element type and PT its pointer, constrained to be a Visitor — which
// is how a corelib that knows no schema reaches a generated type: through a
// type parameter, never by naming it. Cap is the array's schema count bound
// (negative: none).
//
// The element is decoded IN PLACE at Out[id], after the gap up to it is filled
// with zero elements. That is also what makes a reopened id (§7.4) continue the
// element already there instead of starting a second one.
type MessageSeq[T any, PT interface {
	*T
	Visitor
}] struct {
	VisitorBase
	Out *[]T
	Cap int
}

// BeginSequence hands back the element at id as the visitor for its scope.
func (s *MessageSeq[T, PT]) BeginSequence(id ID) (Visitor, error) {
	if s.Cap >= 0 && int(id) >= s.Cap {
		return nil, ErrInvalidMsg
	}
	var zero T
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, zero)
	}
	return PT(&(*s.Out)[id]), nil
}

// NestedSeq collects an array whose elements are themselves wrapper-sequence
// arrays ([][]T): each element opens a sequence collected into the inner slice
// its element id names, by Make.
//
// Make builds the inner collector for one row — typically another collector
// from this file, carrying the INNER array's own bounds. It is called once per
// row that arrives, after that row's slot exists, and is handed the address of
// the slot.
type NestedSeq[T any] struct {
	VisitorBase
	Out  *[][]T
	Cap  int
	Make func(*[]T) Visitor
}

// BeginSequence reserves the row at id and returns the collector for it.
func (s *NestedSeq[T]) BeginSequence(id ID) (Visitor, error) {
	if s.Cap >= 0 && int(id) >= s.Cap {
		return nil, ErrInvalidMsg
	}
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, nil)
	}
	return s.Make(&(*s.Out)[id]), nil
}

// PlaceRow stores a FINISHED row of a matrix (an array whose elements are
// native arrays) at the index its element id names, growing out with empty rows
// so an id gap decodes as an empty row instead of shifting every later row down
// by one. Gaps are ordinary here: an interior row equal to the element default
// (the empty row) is omitted by a conformant encoder (§2), and only the LAST
// row is guaranteed present — which is what makes the decoded length, highest
// present id + 1, exact.
//
// capacity is the OUTER array's schema count bound N (negative: none) and
// bounds the row id exactly as Cap bounds an element id above: a row id >= N is
// INVALID and is refused before the grow, so the id-keyed fill cannot be turned
// into an amplification.
//
// The row is stored as given, not copied — unlike a blob element, a decoded
// native array is freshly built by the decoder and has no other owner.
func PlaceRow[T any](out *[][]T, capacity int, id ID, row []T) error {
	if capacity >= 0 && int(id) >= capacity {
		return ErrInvalidMsg
	}
	for len(*out) <= int(id) {
		*out = append(*out, nil)
	}
	(*out)[id] = row
	return nil
}

// UnsignedMatrixSeq collects the rows of an unsigned integer matrix into Out.
// Rows arrive widened to []uint64 (one callback for every element width), so
// each is scanned against Hi and then narrowed to T.
//
// Hi is the largest value T's DECLARED width allows. The narrowing conversion
// only masks, so an element above Hi has to be rejected here or it would be
// stored as a different value than the wire carried (§7.1). Hi == 0 means the
// declared width spans the whole range this callback can deliver — u64, or a
// bitfield — and switches the scan off rather than running one that can never
// fire.
type UnsignedMatrixSeq[T Unsigned] struct {
	VisitorBase
	Out *[][]T
	Cap int
	Hi  uint64
}

// UnsignedArray narrows one row and places it at its element id.
func (s *UnsignedMatrixSeq[T]) UnsignedArray(id ID, v []uint64) error {
	if s.Hi != 0 {
		for _, x := range v {
			if x > s.Hi {
				return ErrInvalidMsg
			}
		}
	}
	return PlaceRow(s.Out, s.Cap, id, NarrowUnsigned[T](v))
}

// SignedMatrixSeq is UnsignedMatrixSeq for signed element widths: rows arrive
// as []int64 and are scanned against [Lo, Hi] before being narrowed to T.
//
// Lo == 0 switches the scan off, for the same reason Hi == 0 does above: every
// signed width that narrows anything has a negative Lo, so a zero Lo means i64
// (or an enum), whose range is the callback parameter's own.
type SignedMatrixSeq[T Signed] struct {
	VisitorBase
	Out *[][]T
	Cap int
	Lo  int64
	Hi  int64
}

// SignedArray narrows one row and places it at its element id.
func (s *SignedMatrixSeq[T]) SignedArray(id ID, v []int64) error {
	if s.Lo != 0 {
		for _, x := range v {
			if x < s.Lo || x > s.Hi {
				return ErrInvalidMsg
			}
		}
	}
	return PlaceRow(s.Out, s.Cap, id, NarrowSigned[T](v))
}

// Float32MatrixSeq collects the rows of an fp32 matrix. No width scan: the
// callback already delivers the declared element type.
type Float32MatrixSeq struct {
	VisitorBase
	Out *[][]float32
	Cap int
}

// Float32Array places one row at its element id.
func (s *Float32MatrixSeq) Float32Array(id ID, v []float32) error {
	return PlaceRow(s.Out, s.Cap, id, v)
}

// Float64MatrixSeq is Float32MatrixSeq for fp64 rows.
type Float64MatrixSeq struct {
	VisitorBase
	Out *[][]float64
	Cap int
}

// Float64Array places one row at its element id.
func (s *Float64MatrixSeq) Float64Array(id ID, v []float64) error {
	return PlaceRow(s.Out, s.Cap, id, v)
}

// BoolMatrixSeq collects the rows of a bool matrix. Bools travel as an unsigned
// array (§4.6), so a row arrives as []uint64 and every nonzero element is true;
// no width scan applies, because no unsigned value is out of range for a bool.
type BoolMatrixSeq struct {
	VisitorBase
	Out *[][]bool
	Cap int
}

// UnsignedArray maps one row to bools and places it at its element id.
func (s *BoolMatrixSeq) UnsignedArray(id ID, v []uint64) error {
	row := make([]bool, len(v))
	for i, x := range v {
		row[i] = x != 0
	}
	return PlaceRow(s.Out, s.Cap, id, row)
}
