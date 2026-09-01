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
//   - Every collector is built by its New* constructor and takes TWO bound
//     values, Bounds and Caps below: what the SCHEMA declares, and the
//     §6.2.1 receiver caps the DEPLOYMENT configured. Neither is optional and
//     neither is a number this package chose. The fields are unexported so the
//     caps cannot be left off by writing a struct literal — the omission is a
//     compile error, not a runtime one.
//   - The two are never both in play on one bound: §6.2.1 forbids a cap on a
//     field the schema already bounds, so a Caps entry is consulted ONLY where
//     its Bounds sibling is absent. There is no third case: where the schema
//     states nothing, the receiver cap governs, and if it too is absent the
//     call is defective — see overIndex.
//   - An element is PLACED at out[id], never appended, after the gap up to it
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

// Bounds is what the SCHEMA declares about an array: the `count:` capacity and
// the `maxlen:` element bound of MESSAGE_SPEC §7.1/§7.2. A NON-POSITIVE field
// means the schema declares none — a declared bound is at least 1, so zero and
// negative say the same thing.
//
// Count is a CAPACITY, not a length (MESSAGE_SPEC §3): it never adds an element
// the wire did not carry. All it does here is bound the element id — an id >=
// Count is a schema-bound violation (INVALID, §7.1) — which is checked BEFORE
// the slice grows, so an announced index near 2^31 costs a comparison and not
// an allocation.
type Bounds struct {
	Count   int // `count:` — the array's element capacity N
	ElemLen int // `maxlen:` — an element's byte length, where elements are strings or blobs
}

// Caps is the receiver-side technical limits of CORELIB_PLAN §6.2.1, under that
// section's own three names, as generated code configured them for this
// deployment.
//
// These are the CALLER'S numbers, used for one comparison and not retained
// (§6.2.1, "Passing a limit in is not the codec holding one"). This package
// holds none, defaults none, and clamps to none. A breach is ErrLimitExceeded —
// a policy category distinct from INVALID (§6.3), because the same bytes decode
// under a looser cap.
//
// Every entry a collector will actually consult MUST be positive. §6.2.1 admits
// neither an unset state nor an unlimited mode, so there is nothing sensible for
// a missing one to mean: see overIndex for what a collector does with it, and
// why it is not the format ceiling.
type Caps struct {
	ArrayCount int // max_dyn_array_count — elements in a schema-unbounded array
	StringLen  int // max_dyn_string_len — bytes in a schema-unbounded string
	BlobLen    int // max_dyn_blob_len — bytes in a schema-unbounded blob
}

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
//     and a breach is ErrLimitExceeded (§6.2.1, §6.3).
//
// There is NO fallback for a missing rcap, and in particular not the format
// ceiling ARRAY_MAX. §6.2.1: "A format ceiling (§6.2) reached because no cap was
// stated is the FORMAT's bound, not a receiver cap, and a port MUST NOT present
// it as one." Reporting ErrLimitExceeded against a ceiling nobody configured
// would promise the caller a limit to raise that was never set, which §6.3 gives
// as the reason that code is wrong for a call defect.
//
// So a missing cap is a CALLER DEFECT and answers ErrArgument — §6.3's
// InvalidArgument, "the only code for a caller mistake". It is not a policy
// rejection: nothing about the message is at fault, and no number exists to
// compare it against. Stating the cap stays generated code's duty (§6.2.1), and
// the constructors below make omitting it a compile error so this guard is
// reached only by a value built past them.
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
		return ErrArgument
	}
	if int(id) >= rcap {
		return ErrLimitExceeded
	}
	return nil
}

// overLen is overIndex for a COUNT or a LENGTH the wire announces — an element's
// byte length, a matrix row's element count. Same rule, same exclusivity, same
// ErrArgument on a cap that was never stated; only the comparison differs, an
// announced n being a size rather than an index.
func overLen(n, max, rmax int) error {
	if max > 0 {
		if n > max {
			return ErrInvalidMsg
		}
		return nil
	}
	if rmax <= 0 {
		return ErrArgument
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
// b.Count is the array's schema count bound and b.ElemLen the element maxlen
// bound (non-positive: none); a breach of either is INVALID, never a truncation,
// and both are latched at the length word — see FixlenBegin. c.ArrayCount and
// c.StringLen are their §6.2.1 receiver-cap siblings, consulted only where the
// schema states no bound and answering ErrLimitExceeded.
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
	out *[]string
	b   Bounds
	c   Caps
	acc PayloadAcc
}

// NewStringSeq builds a string-array collector writing into out.
//
// b and c are both required arguments rather than settable fields, which is what
// makes a missing receiver cap (§6.2.1) a compile error at the call site instead
// of a decode that runs uncapped: a struct literal's zero value is exactly the
// omission this API must not accept.
func NewStringSeq(out *[]string, b Bounds, c Caps) *StringSeq {
	return &StringSeq{out: out, b: b, c: c}
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
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	return overLen(total, s.b.ElemLen, s.c.StringLen)
}

// String accumulates one element's pieces and, once the last of them lands,
// places the element at the index its id names, growing Out with empty strings
// across any gap. The bounds are applied here as well as at the header: a
// collector driven by hand, without the header call, must still refuse an
// out-of-range id or an over-long element.
func (s *StringSeq) String(id ID, total, offset int, chunk []byte) error {
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(total, s.b.ElemLen, s.c.StringLen); err != nil {
		return err
	}
	b, done := s.acc.Take(total, offset, chunk)
	if !done {
		return nil
	}
	if !s.UTF8Valid(b) {
		return ErrInvalidMsg
	}
	for len(*s.out) <= int(id) {
		*s.out = append(*s.out, "")
	}
	(*s.out)[id] = string(b)
	return nil
}

// BlobSeq is the blob twin of StringSeq: same bounds, same gap-filled
// placement, same piecewise assembly, no UTF-8 check (a blob is bytes).
// Elements are COPIED out of the accumulator, because a payload that arrived in
// one piece is a window into the caller's fed bytes and this collector keeps it
// past the call (§6.7).
type BlobSeq struct {
	VisitorBase
	out *[][]byte
	b   Bounds
	c   Caps
	acc PayloadAcc
}

// NewBlobSeq builds a blob-array collector writing into out. See NewStringSeq
// for why b and c are arguments rather than fields.
func NewBlobSeq(out *[][]byte, b Bounds, c Caps) *BlobSeq {
	return &BlobSeq{out: out, b: b, c: c}
}

// FixlenBegin latches both bounds at the length word, gated on the declared
// subtype; see StringSeq.FixlenBegin for why both properties matter.
func (s *BlobSeq) FixlenBegin(id ID, subtype FixlenSubtype, total int) error {
	if subtype != FixlenBlob {
		return nil
	}
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	return overLen(total, s.b.ElemLen, s.c.BlobLen)
}

// Bytes accumulates one element's pieces and places a copy of the finished
// element at the index its id names, growing Out with nil elements across any
// gap.
func (s *BlobSeq) Bytes(id ID, total, offset int, chunk []byte) error {
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(total, s.b.ElemLen, s.c.BlobLen); err != nil {
		return err
	}
	b, done := s.acc.Take(total, offset, chunk)
	if !done {
		return nil
	}
	for len(*s.out) <= int(id) {
		*s.out = append(*s.out, nil)
	}
	(*s.out)[id] = append([]byte(nil), b...)
	return nil
}

// MessageSeq collects the elements of a struct/union array into Out: each
// element is a nested sequence decoded straight into the element the child id
// names.
//
// T is the element type and PT its pointer, constrained to be a Visitor — which
// is how a corelib that knows no schema reaches a generated type: through a
// type parameter, never by naming it. b.Count is the array's schema count bound
// (non-positive: none) and c.ArrayCount its §6.2.1 receiver-cap sibling,
// consulted only where the schema states none.
//
// The element is decoded IN PLACE at out[id], after the gap up to it is filled
// with zero elements. That is also what makes a reopened id (§7.4) continue the
// element already there instead of starting a second one.
type MessageSeq[T any, PT interface {
	*T
	Visitor
}] struct {
	VisitorBase
	out *[]T
	b   Bounds
	c   Caps
}

// NewMessageSeq builds a struct/union-array collector writing into out. See
// NewStringSeq for why b and c are arguments rather than fields.
//
// Both type parameters are written out at the call site — PT cannot be inferred
// from out alone — exactly as they were on the struct literal this replaces.
func NewMessageSeq[T any, PT interface {
	*T
	Visitor
}](out *[]T, b Bounds, c Caps) *MessageSeq[T, PT] {
	return &MessageSeq[T, PT]{out: out, b: b, c: c}
}

// BeginSequence hands back the element at id as the visitor for its scope.
func (s *MessageSeq[T, PT]) BeginSequence(id ID) (Visitor, error) {
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return nil, err
	}
	var zero T
	for len(*s.out) <= int(id) {
		*s.out = append(*s.out, zero)
	}
	return PT(&(*s.out)[id]), nil
}

// NestedSeq collects an array whose elements are themselves wrapper-sequence
// arrays ([][]T): each element opens a sequence collected into the inner slice
// its element id names, by Make.
//
// make builds the inner collector for one row — typically another collector
// from this file, carrying the INNER array's own bounds AND its own receiver
// caps. It is called once per row that arrives, after that row's slot exists,
// and is handed the address of the slot.
type NestedSeq[T any] struct {
	VisitorBase
	out  *[][]T
	b    Bounds
	c    Caps
	make func(*[]T) Visitor
}

// NewNestedSeq builds a collector for an array of wrapper-sequence arrays,
// writing into out; b and c bound the OUTER array and make builds each row's
// inner collector, which carries the inner bounds and caps of its own. See
// NewStringSeq for why b and c are arguments rather than fields.
func NewNestedSeq[T any](out *[][]T, b Bounds, c Caps, make func(*[]T) Visitor) *NestedSeq[T] {
	return &NestedSeq[T]{out: out, b: b, c: c, make: make}
}

// BeginSequence reserves the row at id and returns the collector for it.
func (s *NestedSeq[T]) BeginSequence(id ID) (Visitor, error) {
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return nil, err
	}
	for len(*s.out) <= int(id) {
		*s.out = append(*s.out, nil)
	}
	return s.make(&(*s.out)[id]), nil
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
// b.Count is the OUTER array's schema count bound N (non-positive: none) and
// c.ArrayCount its §6.2.1 receiver-cap sibling; together they bound the row id
// exactly as they bound an element id above, and the bound is applied before the
// grow, so the id-keyed fill cannot be turned into an amplification. Both are
// taken as parameters rather than read off a collector because a matrix
// collector applies them twice — at ArrayBegin and again here — and §6.2.1
// requires the two to be one implementation.
//
// The row is stored as given, not copied — unlike a blob element, a decoded
// native array is freshly built by the decoder and has no other owner.
func PlaceRow[T any](out *[][]T, b Bounds, c Caps, id ID, row []T) error {
	if err := overIndex(id, b.Count, c.ArrayCount); err != nil {
		return err
	}
	for len(*out) <= int(id) {
		*out = append(*out, nil)
	}
	(*out)[id] = row
	return nil
}

// rowGate is the MESSAGE_SPEC §7.3 latch every matrix collector carries, and
// the reason it exists is that a row's verdict and a row's placement are two
// different callbacks.
//
// ArrayBegin is where the decision is made — the wire kind either matches the
// declared element type or it names another field's shape — but ArrayEnd is
// where the row would be bounds-checked and PLACED, and the codec pairs every
// ArrayBegin with an ArrayEnd whatever the destination said about the first
// (istream.go delivers both, including for an empty array). Without a latch,
// ArrayEnd cannot tell a row this collector DECLINED from one it accepted, so a
// skipped field is judged against bounds it was never subject to and stored at
// a slot it never claimed. §7.3 gives it neither: a skipped field is walked, and
// that is all it does.
//
// One bool with one implementation, embedded by all five matrix collectors,
// because the rule is the same rule five times and a half-applied fix here is
// invisible — the declined row and the gap row print identically.
type rowGate struct{ declined bool }

// mine records the ArrayBegin verdict for the ArrayEnd that will follow, and
// reports it: true when the row is this array's value, false when §7.3 declined
// it and the collector must return without measuring or opening anything.
func (g *rowGate) mine(ok bool) bool { g.declined = !ok; return ok }

// ended reports whether the row now closing was the declined one, clearing the
// latch for the next row either way. A hand-driven ArrayEnd with no ArrayBegin
// before it reads false, which is what keeps the standalone PlaceRow path — the
// second half of §6.2.1's "one implementation however the bound was stated" —
// reachable.
func (g *rowGate) ended() bool { d := g.declined; g.declined = false; return d }

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
// ever placed — and a row DECLINED at ArrayBegin under §7.3 does not reach
// PlaceRow either, which is what rowGate below is for.
//
// Each carries FOUR bounds, two per axis, because a matrix has two:
//
//   - b.Count / c.ArrayCount bound the ROW ID — the outer array's `count:` and
//     its receiver cap — exactly as on the collectors above.
//   - row.Count / c.ArrayCount bound the row's OWN element count, which the row
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
// hi is the largest value T's DECLARED width allows. The narrowing conversion
// only masks, so an element above hi has to be rejected as it goes past or it
// would be stored as a different value than the wire carried (§7.1) — and
// rejecting it AT THE ELEMENT is what keeps it INVALID rather than INCOMPLETE
// when the row is then truncated (§5.2). hi == 0 means the declared width spans
// the whole range this callback can deliver — u64, or a bitfield — and switches
// the check off rather than running one that can never fire.
type UnsignedMatrixSeq[T Unsigned] struct {
	VisitorBase
	rowGate
	out *[][]T
	b   Bounds
	row Bounds
	c   Caps
	hi  uint64
	cur []T
}

// NewUnsignedMatrixSeq builds an unsigned-matrix collector writing into out: b
// bounds the outer array (the row id), row bounds a row's own element count, c
// carries the §6.2.1 receiver caps for both axes, and hi is the largest value
// the element's declared width allows (0: no narrowing). See NewStringSeq for
// why the bounds are arguments rather than fields.
func NewUnsignedMatrixSeq[T Unsigned](out *[][]T, b, row Bounds, c Caps, hi uint64) *UnsignedMatrixSeq[T] {
	return &UnsignedMatrixSeq[T]{out: out, b: b, row: row, c: c, hi: hi}
}

// ArrayBegin declines a row whose wire kind is not this array's (§7.3), and
// otherwise bounds the row id and the row's announced element count and opens a
// fresh row. The decline is latched for ArrayEnd: neither bound is a declined
// row's to answer to.
func (s *UnsignedMatrixSeq[T]) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if !s.mine(kind == ArrayUnsigned) {
		return nil
	}
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(count, s.row.Count, s.c.ArrayCount); err != nil {
		return err
	}
	s.cur = nil
	return nil
}

// ArrayUnsigned checks one element against the declared width and appends it.
func (s *UnsignedMatrixSeq[T]) ArrayUnsigned(_ ID, _ int, v uint64) error {
	if s.hi != 0 && v > s.hi {
		return ErrInvalidMsg
	}
	s.cur = append(s.cur, T(v))
	return nil
}

// ArrayEnd places the finished row at its element id, unless ArrayBegin
// declined the row under §7.3 — in which case nothing is measured and nothing
// is placed.
func (s *UnsignedMatrixSeq[T]) ArrayEnd(id ID) error {
	if s.ended() {
		return nil
	}
	row := s.cur
	s.cur = nil
	return PlaceRow(s.out, s.b, s.c, id, arrivedRow(row))
}

// SignedMatrixSeq is UnsignedMatrixSeq for signed element widths: elements
// arrive as int64 and are checked against [Lo, Hi] before being narrowed to T.
//
// lo == 0 switches the check off, for the same reason hi == 0 does above: every
// signed width that narrows anything has a negative lo, so a zero lo means i64
// (or an enum), whose range is the callback parameter's own.
type SignedMatrixSeq[T Signed] struct {
	VisitorBase
	rowGate
	out *[][]T
	b   Bounds
	row Bounds
	c   Caps
	lo  int64
	hi  int64
	cur []T
}

// NewSignedMatrixSeq builds a signed-matrix collector writing into out; lo and
// hi are the declared width's range (lo == 0: no narrowing). See
// NewUnsignedMatrixSeq for the bound arguments.
func NewSignedMatrixSeq[T Signed](out *[][]T, b, row Bounds, c Caps, lo, hi int64) *SignedMatrixSeq[T] {
	return &SignedMatrixSeq[T]{out: out, b: b, row: row, c: c, lo: lo, hi: hi}
}

// ArrayBegin declines a row whose wire kind is not this array's (§7.3), and
// otherwise bounds the row id and the row's announced element count and opens a
// fresh row. The decline is latched for ArrayEnd: neither bound is a declined
// row's to answer to.
func (s *SignedMatrixSeq[T]) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if !s.mine(kind == ArraySigned) {
		return nil
	}
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(count, s.row.Count, s.c.ArrayCount); err != nil {
		return err
	}
	s.cur = nil
	return nil
}

// ArraySigned checks one element against the declared width and appends it.
func (s *SignedMatrixSeq[T]) ArraySigned(_ ID, _ int, v int64) error {
	if s.lo != 0 && (v < s.lo || v > s.hi) {
		return ErrInvalidMsg
	}
	s.cur = append(s.cur, T(v))
	return nil
}

// ArrayEnd places the finished row at its element id, unless ArrayBegin
// declined the row under §7.3 — in which case nothing is measured and nothing
// is placed.
func (s *SignedMatrixSeq[T]) ArrayEnd(id ID) error {
	if s.ended() {
		return nil
	}
	row := s.cur
	s.cur = nil
	return PlaceRow(s.out, s.b, s.c, id, arrivedRow(row))
}

// Float32MatrixSeq collects the rows of an fp32 matrix. No width check: the
// callback already delivers the declared element type.
type Float32MatrixSeq struct {
	VisitorBase
	rowGate
	out *[][]float32
	b   Bounds
	row Bounds
	c   Caps
	cur []float32
}

// NewFloat32MatrixSeq builds an fp32-matrix collector writing into out. See
// NewUnsignedMatrixSeq for the bound arguments.
func NewFloat32MatrixSeq(out *[][]float32, b, row Bounds, c Caps) *Float32MatrixSeq {
	return &Float32MatrixSeq{out: out, b: b, row: row, c: c}
}

// ArrayBegin declines a row whose wire kind is not this array's (§7.3), and
// otherwise bounds the row id and the row's announced element count and opens a
// fresh row. The decline is latched for ArrayEnd: neither bound is a declined
// row's to answer to.
func (s *Float32MatrixSeq) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if !s.mine(kind == ArrayFp32) {
		return nil
	}
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(count, s.row.Count, s.c.ArrayCount); err != nil {
		return err
	}
	s.cur = nil
	return nil
}

// ArrayFloat32 appends one element to the row being built.
func (s *Float32MatrixSeq) ArrayFloat32(_ ID, _ int, v float32) error {
	s.cur = append(s.cur, v)
	return nil
}

// ArrayEnd places the finished row at its element id, unless ArrayBegin
// declined the row under §7.3 — in which case nothing is measured and nothing
// is placed.
func (s *Float32MatrixSeq) ArrayEnd(id ID) error {
	if s.ended() {
		return nil
	}
	row := s.cur
	s.cur = nil
	return PlaceRow(s.out, s.b, s.c, id, arrivedRow(row))
}

// Float64MatrixSeq is Float32MatrixSeq for fp64 rows.
type Float64MatrixSeq struct {
	VisitorBase
	rowGate
	out *[][]float64
	b   Bounds
	row Bounds
	c   Caps
	cur []float64
}

// NewFloat64MatrixSeq builds an fp64-matrix collector writing into out. See
// NewUnsignedMatrixSeq for the bound arguments.
func NewFloat64MatrixSeq(out *[][]float64, b, row Bounds, c Caps) *Float64MatrixSeq {
	return &Float64MatrixSeq{out: out, b: b, row: row, c: c}
}

// ArrayBegin declines a row whose wire kind is not this array's (§7.3), and
// otherwise bounds the row id and the row's announced element count and opens a
// fresh row. The decline is latched for ArrayEnd: neither bound is a declined
// row's to answer to.
func (s *Float64MatrixSeq) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if !s.mine(kind == ArrayFp64) {
		return nil
	}
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(count, s.row.Count, s.c.ArrayCount); err != nil {
		return err
	}
	s.cur = nil
	return nil
}

// ArrayFloat64 appends one element to the row being built.
func (s *Float64MatrixSeq) ArrayFloat64(_ ID, _ int, v float64) error {
	s.cur = append(s.cur, v)
	return nil
}

// ArrayEnd places the finished row at its element id, unless ArrayBegin
// declined the row under §7.3 — in which case nothing is measured and nothing
// is placed.
func (s *Float64MatrixSeq) ArrayEnd(id ID) error {
	if s.ended() {
		return nil
	}
	row := s.cur
	s.cur = nil
	return PlaceRow(s.out, s.b, s.c, id, arrivedRow(row))
}

// BoolMatrixSeq collects the rows of a bool matrix. Bools travel as an unsigned
// array (§4.6), so elements arrive through ArrayUnsigned and every nonzero one
// is true; no width check applies, because no unsigned value is out of range for
// a bool.
type BoolMatrixSeq struct {
	VisitorBase
	rowGate
	out *[][]bool
	b   Bounds
	row Bounds
	c   Caps
	cur []bool
}

// NewBoolMatrixSeq builds a bool-matrix collector writing into out. See
// NewUnsignedMatrixSeq for the bound arguments.
func NewBoolMatrixSeq(out *[][]bool, b, row Bounds, c Caps) *BoolMatrixSeq {
	return &BoolMatrixSeq{out: out, b: b, row: row, c: c}
}

// ArrayBegin declines a row whose wire kind is not this array's (§7.3), and
// otherwise bounds the row id and the row's announced element count and opens a
// fresh row. The decline is latched for ArrayEnd: neither bound is a declined
// row's to answer to.
func (s *BoolMatrixSeq) ArrayBegin(id ID, kind ArrayKind, count int) error {
	if !s.mine(kind == ArrayUnsigned) {
		return nil
	}
	if err := overIndex(id, s.b.Count, s.c.ArrayCount); err != nil {
		return err
	}
	if err := overLen(count, s.row.Count, s.c.ArrayCount); err != nil {
		return err
	}
	s.cur = nil
	return nil
}

// ArrayUnsigned appends one element to the row being built.
func (s *BoolMatrixSeq) ArrayUnsigned(_ ID, _ int, v uint64) error {
	s.cur = append(s.cur, v != 0)
	return nil
}

// ArrayEnd places the finished row at its element id, unless ArrayBegin
// declined the row under §7.3 — in which case nothing is measured and nothing
// is placed.
func (s *BoolMatrixSeq) ArrayEnd(id ID) error {
	if s.ended() {
		return nil
	}
	row := s.cur
	s.cur = nil
	return PlaceRow(s.out, s.b, s.c, id, arrivedRow(row))
}
