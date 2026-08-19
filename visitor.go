package sofab

import "io"

// Visitor is the push/visitor counterpart to the pull parser (Decoder.Next):
// the decoder drives, calling a typed method per field. Generated code
// implements Visitor on the target struct and binds each field straight into a
// member — so a generated object can be deserialized without the caller ever
// writing a Next/Skip loop. Nested sequences descend into the visitor returned
// by BeginSequence (typically the nested generated object).
//
// Array methods receive the values widened to the 64-bit value domain (or the
// concrete float slice); the generated code narrows to its declared element
// width.
type Visitor interface {
	Unsigned(id ID, v uint64) error
	Signed(id ID, v int64) error
	Float32(id ID, v float32) error
	Float64(id ID, v float64) error
	String(id ID, s string) error
	Bytes(id ID, b []byte) error
	UnsignedArray(id ID, v []uint64) error
	SignedArray(id ID, v []int64) error
	Float32Array(id ID, v []float32) error
	Float64Array(id ID, v []float64) error
	// BeginSequence returns the visitor that receives the nested scope's fields,
	// or nil to SKIP the scope: nothing in it is delivered, nothing under it is
	// offered again however deep, and no EndSequence fires for it.
	//
	// Skipping is what a consumer says about a subtree it has no destination for
	// — an unknown id, a field it does not care about. The bytes are still parsed
	// (a sequence is framed by markers, not by a length, so its end has to be
	// found) but nothing in it is built: no string is allocated, no element slice
	// made. Before nil meant this, a consumer had to hand over a no-op visitor
	// instead, and this interface has no optional methods — so every value in the
	// discarded subtree was decoded and every string built before an empty method
	// threw it away.
	//
	// The receiver caps (WithMaxStringLen and friends) do not fire inside a
	// skipped scope: they bound what this consumer is handed, and it is handed
	// nothing. Format ceilings still do, everywhere.
	BeginSequence(id ID) (Visitor, error)
	// EndSequence is called on that nested visitor once its scope closes, so a
	// generated nested object can finalize itself.
	EndSequence() error
}

// HeaderVisitor is an optional extension a Visitor may also implement to inspect
// an array's element kind and count, or a fixlen field's declared length, at the
// header — before the truncation check and before any element or payload byte.
// It exists so a schema-bound violation (over-count, over-maxlen) is rejected at
// the header as INVALID by returning ErrInvalidMsg, even when the field is then
// truncated: MESSAGE_SPEC §5.2 has INVALID dominate INCOMPLETE ("anti-folding" —
// more bytes cannot make a schema-illegal count/length legal), so the
// whole-slice callback the generated len(v)>N guard runs in is too late once the
// array is truncated.
//
// It is additive and backward-compatible. The cursor type-asserts the visitor
// to HeaderVisitor once per scope, so a visitor that does not implement it
// decodes exactly as before — no method, no call. Generated code implements
// these only when the schema declares a bound, and the hooks then fire once per
// array/fixlen field (never per element), so the max-speed decode path is
// unchanged for visitors without bounds.
type HeaderVisitor interface {
	// ArrayBegin is called once per array field with the element kind the wire
	// declares and the wire element count, before the truncation check and any
	// element. A non-nil return (typically ErrInvalidMsg) aborts the decode at
	// the header.
	//
	// WHERE IT FIRES depends on the wire type, because that is where the element
	// kind becomes known (CORELIB_PLAN §4.8):
	//
	//   - integer arrays (ArrayUnsigned, ArraySigned): right after the count
	//     varint — the wire type alone fixes the kind, there is no second word;
	//   - fixlen arrays (ArrayFp32, ArrayFp64): after the fixlen_word, once the
	//     element subtype is read and found format-legal. The count word is read
	//     first, and the FORMAT ceiling and any receiver limit still fire there,
	//     but the hook is deferred so the kind it carries is never a guess.
	//
	// The deferral is what MESSAGE_SPEC §7.3 requires: a fixlen array whose
	// subtype contradicts the declared element type is skipped, and the schema
	// count bound MUST NOT be applied to it — the field was never this array's
	// value, so its element count is not this array's count. Generated code must
	// therefore apply its count bound only in the arm matching the declared
	// element type. A consequence, and intended: a message that ends between the
	// two words is INCOMPLETE, not INVALID, because no bound can yet be judged.
	//
	// A fixlen_word that is format-illegal (a string or blob subtype, or a
	// width mismatch) is INVALID before the hook fires; that is a format
	// violation (§4.8), not a skippable schema mismatch.
	ArrayBegin(id ID, kind ArrayKind, count int) error
	// FixlenHeader is called with the element subtype and declared byte length
	// right after a fixlen length word is read, before the payload is taken.
	// Same contract for the schema maxlen bound.
	FixlenHeader(id ID, subtype int, length int) error
}

// ElemBoundVisitor is a second optional extension a Visitor may implement, to
// declare the value range an INTEGER ARRAY's elements may take under the schema.
//
// It exists for the same reason HeaderVisitor does, one level down. The array
// callbacks hand over the whole slice, so the generated `for _, x := range v`
// width guard can only run once every element has arrived — and an array that
// is truncated never arrives. MESSAGE_SPEC §5.2 has INVALID dominate INCOMPLETE,
// so an element that is already outside its declared width and fully on the wire
// must keep the message INVALID however little follows it. Only the decoder can
// apply that bound while the elements go past, and only the schema knows what
// the bound is; this is how the two meet (generator#267, Crucible F-0043).
//
// SEPARATE from HeaderVisitor, deliberately. Both are reached by a type
// assertion, so a visitor that implements only part of an interface implements
// none of it — adding a third method to HeaderVisitor would silently switch
// ArrayBegin and FixlenHeader off for every visitor generated before it existed.
// As its own interface it is purely additive: a visitor that does not implement
// it decodes exactly as before.
//
// Asked AT MOST ONCE per array FIELD, never per element, and only where the
// array fails to complete. That is the whole of its cost: the bound can change
// an outcome only where the whole-slice callback does not fire, since an array
// that arrives whole reaches the visitor's own guard, which sees every element
// and reaches the same verdict. So a long array costs no call at all when it
// decodes, one when it is truncated — and the element loops stay a pure decode.
// A scope holding no array never even makes the type assertion, and both visitor
// surfaces ask the same number of times on the same bytes.
type ElemBoundVisitor interface {
	// ArrayElemBound reports the inclusive range an element of the integer array
	// field id may take, and whether the schema narrows it at all (false for u64
	// and i64, which span the value domain, and for an id this scope does not
	// declare). Both bounds are int64: the widest NARROWED unsigned kind is u32,
	// so every bound that exists fits, and an unsigned element is compared
	// against uint64(max).
	//
	// kind is the kind the WIRE declares. A field whose wire kind contradicts the
	// declared element type is skipped whole (§7.3) — the value was never this
	// field's — so an implementation must return false for a kind it does not
	// declare rather than measure another field's elements against this bound.
	// Same rule as HeaderVisitor.ArrayBegin.
	ArrayElemBound(id ID, kind ArrayKind) (min, max int64, ok bool)
}

// ebCache resolves a visitor's optional ElemBoundVisitor exactly as hvCache
// resolves HeaderVisitor, and for the same reason: the assertion is an itab
// lookup that walks the whole method list when it fails, so it is asked at most
// once per scope and only where the answer can matter.
type ebCache struct {
	eb    ElemBoundVisitor
	known bool
}

func (c *ebCache) of(v Visitor) ElemBoundVisitor {
	if !c.known {
		c.eb, _ = v.(ElemBoundVisitor)
		c.known = true
	}
	return c.eb
}

// elemBound is one array field's answer, resolved once and then applied per
// element — the interface call stays out of the element loop.
type elemBound struct {
	lo, hi int64
	signed bool
	ok     bool
}

// elemBoundOf asks v for the bound on field id, given the kind the wire
// declares. A visitor without the extension, or a field it does not narrow,
// yields a bound that never breaches.
func elemBoundOf(eb ElemBoundVisitor, id ID, kind ArrayKind) elemBound {
	if eb == nil {
		return elemBound{}
	}
	lo, hi, ok := eb.ArrayElemBound(id, kind)
	return elemBound{lo: lo, hi: hi, signed: kind == ArraySigned, ok: ok}
}

// breached reports whether the raw wire value x is outside the declared width.
// x is the varint as read: zigzag is undone here for a signed array, so a caller
// walking undecoded elements passes the same value whatever the kind.
func (b elemBound) breached(x uint64) bool {
	if !b.ok {
		return false
	}
	if b.signed {
		return b.breachedSigned(zigzagDecode(x))
	}
	return x > uint64(b.hi)
}

// breachedSigned is breached for an element a caller has already zigzag-decoded.
func (b elemBound) breachedSigned(s int64) bool {
	return b.ok && (s < b.lo || s > b.hi)
}

// Accept decodes the entire top-level stream into v. It slurps the remaining
// input into one contiguous buffer and advances a cursor over it (see cursor),
// so dispatch never re-enters the io.Reader per byte. It returns nil at a clean
// end of stream, or a malformed-message error on bad input. A non-EOF reader
// error surfaces verbatim.
func (d *Decoder) Accept(v Visitor) error {
	buf, err := d.slurp()
	if err != nil {
		return err
	}
	c := cursor{buf: buf, lim: d.lim}
	return c.accept(v, 0)
}

// AcceptBytes decodes a complete message already held in one contiguous buffer,
// dispatching each field to v. It is the zero-copy form of Accept: the cursor
// advances directly over buf with no input slurp, so it is the fastest entry
// point when the message is already in memory (e.g. a generated Decode<Name>).
// buf is not retained, but byte/blob fields handed to v alias it, so the visitor
// must copy any it keeps past the call.
//
// Optional decode limits (WithMaxArrayCount, WithMaxStringLen, WithMaxBlobLen)
// may be supplied; with none, no limits are enforced.
func AcceptBytes(buf []byte, v Visitor, opts ...Option) error {
	c := cursor{buf: buf, lim: newLimits(opts)}
	return c.accept(v, 0)
}

// slurp reads everything still pending into a single buffer. When the source
// reports its remaining length (bytes.Reader, bytes.Buffer, strings.Reader) the
// buffer is sized and filled in one shot; otherwise it falls back to io.ReadAll.
// Anything already buffered by a prior Next is honored.
func (d *Decoder) slurp() ([]byte, error) {
	var r io.Reader = d.src
	if d.r != nil {
		r = d.r
	}
	if l, ok := r.(interface{ Len() int }); ok {
		buf := make([]byte, l.Len())
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(r)
}
