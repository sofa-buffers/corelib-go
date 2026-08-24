package sofab

// BoundKind names the size a header word states, and therefore which of the
// three receiver-side caps (§6.2.1) that word is measured against: an array's
// element count (WithMaxArrayCount), a string's byte length (WithMaxStringLen),
// or a blob's byte length (WithMaxBlobLen). It is the question SchemaBound is
// asked — "does the schema state a `count:`/`maxlen:` for this field?" — and it
// names the size rather than the option so a destination answers from its
// schema, not from the deployment's configuration.
type BoundKind int

const (
	// BoundArrayCount is a count-prefixed array's element count — the unsigned,
	// signed and fixlen array wire types alike. The schema bound is `count:`.
	BoundArrayCount BoundKind = iota
	// BoundStringLen is a fixlen string's declared byte length; the schema bound
	// is `maxlen:`.
	BoundStringLen
	// BoundBlobLen is a fixlen blob's declared byte length; the schema bound is
	// `maxlen:`.
	BoundBlobLen
)

// SchemaBoundVisitor is an optional extension a Visitor may implement to report
// which of its fields the SCHEMA already bounds, so the receiver-side caps
// (WithMaxArrayCount / WithMaxStringLen / WithMaxBlobLen) stay off them.
//
// CORELIB_PLAN §6.2.1 is explicit that the two are different kinds of statement:
// a `max_dyn_*` cap is deployment configuration protecting the receiver from a
// field the schema leaves UNBOUNDED, and it "MUST NOT be applied to a field the
// schema already bounds. There the schema bound governs and its violation is
// INVALID" (MESSAGE_SPEC §7, §7.1). §6.3 says the same from the other end:
// ErrLimitExceeded is "never raised for a field the schema bounds". Without this
// hook the decoder cannot tell the two apart, so a deployment-wide cap of 1000
// would reject a 5000-element array that its schema declares `count: 10000` —
// a message the same schema decodes everywhere else (issue #80).
//
// Only the schema knows, so only generated code can answer. It implements this
// on exactly the destinations that declare a bound, and returns true for exactly
// the (id, kind) pairs the declaration covers — the same set of fields its
// HeaderVisitor arms enforce. Answering true is therefore a promise to enforce:
// the decoder stops capping that field, and the destination's own ArrayBegin /
// FixlenHeader arm is what keeps an over-bound header from allocating — as
// INVALID, which is the outcome the spec wants there.
//
// SEPARATE from HeaderVisitor and from ElemBoundVisitor, deliberately, and for
// the reason ElemBoundVisitor spells out: each is reached by a type assertion,
// so a visitor implementing only part of an interface implements none of it, and
// adding a method to HeaderVisitor would silently switch its hooks off for every
// destination generated before this existed. As its own interface it is purely
// additive — a visitor that does not implement it decodes exactly as before.
//
// Asked at most once per field, and only where the answer can change an outcome:
// the decoder consults it just after a configured cap is exceeded, never on the
// path where no cap is set or the header fits, so a decode with no limits pays
// nothing at all — not even the type assertion.
type SchemaBoundVisitor interface {
	// SchemaBound reports whether the schema declares a bound on the size field
	// id states in this header — `count:` for BoundArrayCount, `maxlen:` for
	// BoundStringLen/BoundBlobLen. The bound's VALUE stays with the destination;
	// this only says which rule applies, and the destination enforces it (as
	// INVALID) through HeaderVisitor.
	//
	// what is the size the WIRE states, so a field arriving under a wire type the
	// schema does not declare for that id — the MESSAGE_SPEC §7.3 skip — is asked
	// about a bound the schema never stated and must be answered false: it is
	// another field's shape, so no schema bound governs it and the receiver cap
	// still protects the skip. Same rule as HeaderVisitor.ArrayBegin.
	SchemaBound(id ID, what BoundKind) bool
}

// schemaBound is where a decode gets the §6.2.1 answer from: it carries the
// scope's visitor and asks it. Its zero value — no visitor — is the "unbounded,
// the cap applies" answer.
//
// It is a plain two-word value with NO cache pointer, unlike hvCache/ebCache/
// spCache, and that is deliberate: a per-scope cache would have to be addressed,
// and a pointer to a scope-local struct threaded through the header reads makes
// that struct escape to the heap — one allocation per scope on the hot decode
// path, to memoize an answer that is only ever asked on the path where a decode
// is already being rejected. The type assertion is made there instead, once per
// over-cap header.
type schemaBound struct {
	v Visitor
}

// bounds reports whether the schema bounds field id's size of kind what. It is
// asked only once a configured cap has been exceeded (see limits.checkArrayCount
// and checkFixlen), so neither the assertion nor the call is on the path where
// the header fits or no cap is set.
func (s schemaBound) bounds(id ID, what BoundKind) bool {
	sb, ok := s.v.(SchemaBoundVisitor)
	return ok && sb.SchemaBound(id, what)
}
