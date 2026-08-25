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
// bound is an argument — so these belong here rather than being emitted into
// every generated package (generator#345).
//
// Three conventions run through the whole file:
//
//   - Cap is the schema count bound N, ElemMax the schema maxlen bound, and a
//     NON-POSITIVE value means the schema declares none — a declared `count:` /
//     `maxlen:` is at least 1 (MESSAGE_SPEC §7.1/§7.2), so zero and negative
//     say the same thing and the zero value of a field nobody set is safe. N is
//     a CAPACITY, not a length (MESSAGE_SPEC §3): it never adds an element the
//     wire did not carry. All it does here is bound the element id — an id >= N
//     is a schema-bound violation (INVALID, §7.1) — which is checked BEFORE the
//     slice grows, so an announced index near 2^31 costs a comparison and not
//     an allocation.
//   - Beside every schema bound sits its POLICY sibling, spelled with an R
//     prefix: RCap beside Cap, RElemMax beside ElemMax, RowCap beside RowCount.
//     That is the receiver-side technical limit of CORELIB_PLAN §6.2.1 —
//     max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len — and it is a
//     PARAMETER, never a number this package chose: generated code knows the
//     schema and the deployment, and "the codec never invents a limit of its
//     own". A breach of it is ErrLimitExceeded, a policy category distinct from
//     INVALID (§6.3), because the same bytes decode under a looser cap.
//
//     The two are never both in play: §6.2.1 forbids a cap on a field the
//     schema already bounds, so the R field is consulted ONLY where its schema
//     sibling is absent. And there is no unlimited mode (§6.2.1 again): a
//     non-positive R field falls back to the FORMAT CEILING, ARRAY_MAX, which
//     is finite. It is not "no bound" — it is the largest bound the wire format
//     itself admits, and a caller who wants a real one passes it.
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

// ceilingBound is the fallback a receiver cap falls back to when the caller
// passed none: ARRAY_MAX, the format ceiling on an array's element count and on
// a fixlen payload's byte length (§6.2). §6.2.1 admits "no unset state and no
// unlimited mode", so the absent case has to resolve to a NUMBER — and the only
// number this package may use is one the format already fixes, never one it
// invented for the occasion.
const ceilingBound = int(arrayMax)

// overIndex is THE implementation of the element-index rule, in one place.
//
// A wrapper array carries no count header: its elements are keyed by id and its
// length is highest present id + 1 (MESSAGE_SPEC §5.1), so the INDEX is what has
// to be bounded, and §6.2.1 says to bound it "before the container it indexes
// into is extended". Which bound governs depends on the schema, and the two are
// mutually exclusive:
//
//   - cap > 0 — the schema declared a `count:`. An index at or past it
//     contradicts the schema both peers agreed on: ErrInvalidMsg (§7.1).
//   - cap <= 0 — the schema declared none, so the receiver cap governs instead
//     and a breach is ErrLimitExceeded (§6.2.1, §6.3). rcap <= 0 falls back to
//     the format ceiling.
//
// §6.2.1 requires this to have ONE implementation however the bound was stated:
// "this is exactly where two routes have been observed to drift apart". Every
// collector below routes its header latch and its payload backstop through
// here, so the two cannot disagree.
func overIndex(id ID, cap, rcap int) error {
	if cap > 0 {
		if int(id) >= cap {
			return ErrInvalidMsg
		}
		return nil
	}
	if rcap <= 0 {
		rcap = ceilingBound
	}
	if int(id) >= rcap {
		return ErrLimitExceeded
	}
	return nil
}

// overLen is overIndex for a COUNT or a LENGTH the wire announces — an element's
// byte length, a matrix row's element count. Same rule, same exclusivity, same
// fallback; only the comparison differs, an announced n being a size rather than
// an index.
func overLen(n, max, rmax int) error {
	if max > 0 {
		if n > max {
			return ErrInvalidMsg
		}
		return nil
	}
	if rmax <= 0 {
		rmax = ceilingBound
	}
	if n > rmax {
		return ErrLimitExceeded
	}
	return nil
}

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

func (VisitorBase) Unsigned(ID, uint64) error                { return nil }
func (VisitorBase) Signed(ID, int64) error                   { return nil }
func (VisitorBase) Float32(ID, float32) error                { return nil }
func (VisitorBase) Float64(ID, float64) error                { return nil }
func (VisitorBase) FixlenBegin(ID, FixlenSubtype, int) error { return nil }
func (VisitorBase) String(ID, int, int, []byte) error        { return nil }
func (VisitorBase) Bytes(ID, int, int, []byte) error         { return nil }
func (VisitorBase) ArrayBegin(ID, ArrayKind, int) error      { return nil }
func (VisitorBase) ArrayUnsigned(ID, int, uint64) error      { return nil }
func (VisitorBase) ArraySigned(ID, int, int64) error         { return nil }
func (VisitorBase) ArrayFloat32(ID, int, float32) error      { return nil }
func (VisitorBase) ArrayFloat64(ID, int, float64) error      { return nil }
func (VisitorBase) ArrayEnd(ID) error                        { return nil }
func (VisitorBase) BeginSequence(ID) (Visitor, error)        { return VisitorBase{}, nil }
func (VisitorBase) EndSequence() error                       { return nil }

// StringSeq collects the elements of a string array into Out.
//
// Cap is the array's schema count bound and ElemMax the element maxlen bound
// (non-positive: none); a breach of either is INVALID, never a truncation, and
// both are latched at the length word — see FixlenBegin. RCap and RElemMax are
// their §6.2.1 receiver-cap siblings — max_dyn_array_count and
// max_dyn_string_len as generated code configured them — consulted only where
// the schema states no bound, answering ErrLimitExceeded; non-positive falls
// back to the format ceiling.
//
// It embeds StringCheck, so the decode's SOFAB_STRICT_UTF8 policy (§6.4) is
// delivered to it before the scope's first element and WithStrictUTF8(false)
// reaches the element check. The zero value of that policy is STRICT, so a
// collector built by hand and never handed a policy validates.
//
// The element's payload arrives IN PIECES (§6.6.3) and is assembled HERE, in a
// PayloadAcc of its own. That is the whole point of the split: the codec never
// sizes storage from the wire, and this collector — the static helper layer of
// §6.6.1, reached only from inside a callback the codec made — does, on the
// caller's behalf.
type StringSeq struct {
	VisitorBase
	StringCheck
	Out      *[]string
	Cap      int
	ElemMax  int
	RCap     int
	RElemMax int
	acc      PayloadAcc
}

// FixlenBegin applies both schema bounds at the element's LENGTH WORD, before a
// byte of payload is taken. §5.2 makes INVALID dominate INCOMPLETE, so a message
// truncated right after the word carrying the violating number must still be
// INVALID; judging it in String, which never completes for such a message, would
// report INCOMPLETE instead.
//
// Both bounds sit inside the declared-subtype test: FixlenBegin fires for ANY
// fixlen subtype at this id, and an element whose subtype contradicts the
// declaration was never this array's value (§7.3), so neither its id nor its
// length may be measured against this array's bounds.
func (s *StringSeq) FixlenBegin(id ID, subtype FixlenSubtype, total int) error {
	if subtype != FixlenStr {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	return overLen(total, s.ElemMax, s.RElemMax)
}

// String accumulates one element's pieces and, once the last of them lands,
// places the element at the index its id names, growing Out with empty strings
// across any gap. The bounds are applied here as well as at the header: a
// collector driven by hand, without the header call, must still refuse an
// out-of-range id or an over-long element.
func (s *StringSeq) String(id ID, total, offset int, chunk []byte) error {
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(total, s.ElemMax, s.RElemMax); err != nil {
		return err
	}
	b, done := s.acc.Take(total, offset, chunk)
	if !done {
		return nil
	}
	if !s.UTF8Valid(b) {
		return ErrInvalidMsg
	}
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, "")
	}
	(*s.Out)[id] = string(b)
	return nil
}

// BlobSeq is the blob twin of StringSeq: same bounds, same gap-filled
// placement, same piecewise assembly, no UTF-8 check (a blob is bytes).
// Elements are COPIED out of the accumulator, because a payload that arrived in
// one piece is a window into the caller's fed bytes and this collector keeps it
// past the call (§6.7).
type BlobSeq struct {
	VisitorBase
	Out      *[][]byte
	Cap      int
	ElemMax  int
	RCap     int
	RElemMax int
	acc      PayloadAcc
}

// FixlenBegin latches both bounds at the length word, gated on the declared
// subtype; see StringSeq.FixlenBegin for why both properties matter.
func (s *BlobSeq) FixlenBegin(id ID, subtype FixlenSubtype, total int) error {
	if subtype != FixlenBlob {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	return overLen(total, s.ElemMax, s.RElemMax)
}

// Bytes accumulates one element's pieces and places a copy of the finished
// element at the index its id names, growing Out with nil elements across any
// gap.
func (s *BlobSeq) Bytes(id ID, total, offset int, chunk []byte) error {
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(total, s.ElemMax, s.RElemMax); err != nil {
		return err
	}
	b, done := s.acc.Take(total, offset, chunk)
	if !done {
		return nil
	}
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, nil)
	}
	(*s.Out)[id] = append([]byte(nil), b...)
	return nil
}

// MessageSeq collects the elements of a struct/union array into Out: each
// element is a nested sequence decoded straight into the element the child id
// names.
//
// T is the element type and PT its pointer, constrained to be a Visitor — which
// is how a corelib that knows no schema reaches a generated type: through a
// type parameter, never by naming it. Cap is the array's schema count bound
// (non-positive: none) and RCap its §6.2.1 receiver-cap sibling, consulted only
// where the schema states none.
//
// The element is decoded IN PLACE at Out[id], after the gap up to it is filled
// with zero elements. That is also what makes a reopened id (§7.4) continue the
// element already there instead of starting a second one.
type MessageSeq[T any, PT interface {
	*T
	Visitor
}] struct {
	VisitorBase
	Out  *[]T
	Cap  int
	RCap int
}

// BeginSequence hands back the element at id as the visitor for its scope.
func (s *MessageSeq[T, PT]) BeginSequence(id ID) (Visitor, error) {
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return nil, err
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
	RCap int
	Make func(*[]T) Visitor
}

// BeginSequence reserves the row at id and returns the collector for it.
func (s *NestedSeq[T]) BeginSequence(id ID) (Visitor, error) {
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return nil, err
	}
	for len(*s.Out) <= int(id) {
		*s.Out = append(*s.Out, nil)
	}
	return s.Make(&(*s.Out)[id]), nil
}

// arrivedRow is the row a matrix collector hands to PlaceRow: never nil, even
// when it is empty.
//
// An empty row that ARRIVED and a row the message never carried are two
// different things, and the destination is where the difference is visible. A
// row is assembled by appending, so an empty one is still the nil slice the
// collector opened with -- indistinguishable from the gap PlaceRow leaves for a
// row that never came, and rendered as `null` rather than `[]` by anything that
// serializes the result. The whole-row callback this replaced could not produce
// that: it built the row before placing it, so an empty row was an empty slice.
func arrivedRow[T any](row []T) []T {
	if row == nil {
		return []T{}
	}
	return row
}

// PlaceRow stores a FINISHED row of a matrix (an array whose elements are
// native arrays) at the index its element id names, growing out with empty rows
// so an id gap decodes as an empty row instead of shifting every later row down
// by one. Gaps are ordinary here: an interior row equal to the element default
// (the empty row) is omitted by a conformant encoder (§2), and only the LAST
// row is guaranteed present — which is what makes the decoded length, highest
// present id + 1, exact.
//
// capacity is the OUTER array's schema count bound N (non-positive: none) and
// rcapacity its §6.2.1 receiver-cap sibling; together they bound the row id
// exactly as Cap/RCap bound an element id above, and the bound is applied before
// the grow, so the id-keyed fill cannot be turned into an amplification. Both
// are taken as parameters rather than read off a collector because a matrix
// collector applies them twice — at ArrayBegin and again here — and §6.2.1
// requires the two to be one implementation.
//
// The row is stored as given, not copied — unlike a blob element, a decoded
// native array is freshly built by the decoder and has no other owner.
func PlaceRow[T any](out *[][]T, capacity, rcapacity int, id ID, row []T) error {
	if err := overIndex(id, capacity, rcapacity); err != nil {
		return err
	}
	for len(*out) <= int(id) {
		*out = append(*out, nil)
	}
	(*out)[id] = row
	return nil
}

// The matrix collectors: an array whose elements are themselves NATIVE arrays.
//
// Each row arrives as ArrayBegin, one element callback per element, ArrayEnd —
// the piecewise shape §6.6.3 requires of the codec — and is assembled here, in
// the helper layer that is allowed to allocate (§6.6.1). The row grows as
// elements ACTUALLY ARRIVE rather than being sized from the announced count, so
// a hostile count costs a comparison and not an allocation; §6.6.1 names exactly
// this shape ("an id-keyed wrapper-array collector, growing the container as
// elements arrive") as the helper case.
//
// The finished row is handed to PlaceRow at ArrayEnd and the collector drops it,
// so the next row starts from a fresh slice and no placed row is written over.
// An array the message truncates never reaches ArrayEnd, so no partial row is
// ever placed.
//
// Each carries FOUR bounds, two per axis, because a matrix has two:
//
//   - Cap / RCap bound the ROW ID — the outer array's `count:` and its receiver
//     cap — exactly as on the collectors above.
//   - RowCount / RowCap bound the row's OWN element count, which the row
//     announces as a real count header because a row IS a native array. That
//     header is the enforcement point §6.2.1 names ("at the count/length header —
//     before the allocation it is meant to prevent"), and ArrayBegin is where the
//     destination is told it. Nothing else bounds it: the codec applies no
//     receiver cap of its own (§6.2.1), and the row is grown by append, so an
//     announced count no bound refuses would be a hostile sender's number.

// UnsignedMatrixSeq collects the rows of an unsigned integer matrix into Out.
// Elements arrive widened to the 64-bit value domain (one callback for every
// declared width), so each is checked against Hi and then narrowed to T.
//
// Hi is the largest value T's DECLARED width allows. The narrowing conversion
// only masks, so an element above Hi has to be rejected as it goes past or it
// would be stored as a different value than the wire carried (§7.1) — and
// rejecting it AT THE ELEMENT is what keeps it INVALID rather than INCOMPLETE
// when the row is then truncated (§5.2). Hi == 0 means the declared width spans
// the whole range this callback can deliver — u64, or a bitfield — and switches
// the check off rather than running one that can never fire.
type UnsignedMatrixSeq[T Unsigned] struct {
	VisitorBase
	Out      *[][]T
	Cap      int
	RCap     int
	RowCount int
	RowCap   int
	Hi       uint64
	row      []T
}

// ArrayBegin bounds the row id and the row's announced element count, then
// opens a fresh row.
func (s *UnsignedMatrixSeq[T]) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if kind != ArrayUnsigned {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(count, s.RowCount, s.RowCap); err != nil {
		return err
	}
	s.row = nil
	return nil
}

// ArrayUnsigned checks one element against the declared width and appends it.
func (s *UnsignedMatrixSeq[T]) ArrayUnsigned(_ ID, _ int, v uint64) error {
	if s.Hi != 0 && v > s.Hi {
		return ErrInvalidMsg
	}
	s.row = append(s.row, T(v))
	return nil
}

// ArrayEnd places the finished row at its element id.
func (s *UnsignedMatrixSeq[T]) ArrayEnd(id ID) error {
	row := s.row
	s.row = nil
	return PlaceRow(s.Out, s.Cap, s.RCap, id, arrivedRow(row))
}

// SignedMatrixSeq is UnsignedMatrixSeq for signed element widths: elements
// arrive as int64 and are checked against [Lo, Hi] before being narrowed to T.
//
// Lo == 0 switches the check off, for the same reason Hi == 0 does above: every
// signed width that narrows anything has a negative Lo, so a zero Lo means i64
// (or an enum), whose range is the callback parameter's own.
type SignedMatrixSeq[T Signed] struct {
	VisitorBase
	Out      *[][]T
	Cap      int
	RCap     int
	RowCount int
	RowCap   int
	Lo       int64
	Hi       int64
	row      []T
}

// ArrayBegin bounds the row id and the row's announced element count, then
// opens a fresh row.
func (s *SignedMatrixSeq[T]) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if kind != ArraySigned {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(count, s.RowCount, s.RowCap); err != nil {
		return err
	}
	s.row = nil
	return nil
}

// ArraySigned checks one element against the declared width and appends it.
func (s *SignedMatrixSeq[T]) ArraySigned(_ ID, _ int, v int64) error {
	if s.Lo != 0 && (v < s.Lo || v > s.Hi) {
		return ErrInvalidMsg
	}
	s.row = append(s.row, T(v))
	return nil
}

// ArrayEnd places the finished row at its element id.
func (s *SignedMatrixSeq[T]) ArrayEnd(id ID) error {
	row := s.row
	s.row = nil
	return PlaceRow(s.Out, s.Cap, s.RCap, id, arrivedRow(row))
}

// Float32MatrixSeq collects the rows of an fp32 matrix. No width check: the
// callback already delivers the declared element type.
type Float32MatrixSeq struct {
	VisitorBase
	Out      *[][]float32
	Cap      int
	RCap     int
	RowCount int
	RowCap   int
	row      []float32
}

// ArrayBegin bounds the row id and the row's announced element count, then
// opens a fresh row.
func (s *Float32MatrixSeq) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if kind != ArrayFp32 {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(count, s.RowCount, s.RowCap); err != nil {
		return err
	}
	s.row = nil
	return nil
}

// ArrayFloat32 appends one element to the row being built.
func (s *Float32MatrixSeq) ArrayFloat32(_ ID, _ int, v float32) error {
	s.row = append(s.row, v)
	return nil
}

// ArrayEnd places the finished row at its element id.
func (s *Float32MatrixSeq) ArrayEnd(id ID) error {
	row := s.row
	s.row = nil
	return PlaceRow(s.Out, s.Cap, s.RCap, id, arrivedRow(row))
}

// Float64MatrixSeq is Float32MatrixSeq for fp64 rows.
type Float64MatrixSeq struct {
	VisitorBase
	Out      *[][]float64
	Cap      int
	RCap     int
	RowCount int
	RowCap   int
	row      []float64
}

// ArrayBegin bounds the row id and the row's announced element count, then
// opens a fresh row.
func (s *Float64MatrixSeq) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if kind != ArrayFp64 {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(count, s.RowCount, s.RowCap); err != nil {
		return err
	}
	s.row = nil
	return nil
}

// ArrayFloat64 appends one element to the row being built.
func (s *Float64MatrixSeq) ArrayFloat64(_ ID, _ int, v float64) error {
	s.row = append(s.row, v)
	return nil
}

// ArrayEnd places the finished row at its element id.
func (s *Float64MatrixSeq) ArrayEnd(id ID) error {
	row := s.row
	s.row = nil
	return PlaceRow(s.Out, s.Cap, s.RCap, id, arrivedRow(row))
}

// BoolMatrixSeq collects the rows of a bool matrix. Bools travel as an unsigned
// array (§4.6), so elements arrive through ArrayUnsigned and every nonzero one
// is true; no width check applies, because no unsigned value is out of range for
// a bool.
type BoolMatrixSeq struct {
	VisitorBase
	Out      *[][]bool
	Cap      int
	RCap     int
	RowCount int
	RowCap   int
	row      []bool
}

// ArrayBegin bounds the row id and the row's announced element count, then
// opens a fresh row.
func (s *BoolMatrixSeq) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if kind != ArrayUnsigned {
		return nil
	}
	if err := overIndex(id, s.Cap, s.RCap); err != nil {
		return err
	}
	if err := overLen(count, s.RowCount, s.RowCap); err != nil {
		return err
	}
	s.row = nil
	return nil
}

// ArrayUnsigned appends one element to the row being built.
func (s *BoolMatrixSeq) ArrayUnsigned(_ ID, _ int, v uint64) error {
	s.row = append(s.row, v != 0)
	return nil
}

// ArrayEnd places the finished row at its element id.
func (s *BoolMatrixSeq) ArrayEnd(id ID) error {
	row := s.row
	s.row = nil
	return PlaceRow(s.Out, s.Cap, s.RCap, id, arrivedRow(row))
}
