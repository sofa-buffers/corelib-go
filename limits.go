package sofab

// Option configures optional decode-time limits. Options are passed to
// NewDecoder or to AcceptBytes.
//
// With no options the decoder enforces no limits and behaves bit-for-bit as it
// did before limits existed. Limits are strictly opt-in: this package invents no
// default cap. (The amplification hardening in issue #40's Part A is separate and
// unconditional — it applies with or without any Option, because it never
// changes which messages decode, only how eagerly memory is allocated.)
//
// A limit is a receiver-side policy, not a wire-format rule. Exceeding one is
// reported as ErrLimitExceeded, deliberately distinct from ErrInvalidMsg, so a
// message rejected purely for exceeding a locally configured cap is never
// conflated with a malformed message. Limits are enforced at header time —
// before any payload is buffered or any element slice is allocated — so an
// oversize claim is rejected even if the payload bytes never arrive. A limit is
// never clamped or truncated: the field is rejected.
//
// Limits apply per field occurrence: array element count for count-prefixed
// arrays, and byte length for strings and blobs.
//
// They apply to SCHEMA-UNBOUNDED fields only. A cap is capacity the deployment
// is willing to commit to a size the sender chose freely; where the schema
// already states a `count:`/`maxlen:`, that bound governs and its violation is
// INVALID, so §6.2.1 forbids the cap there and §6.3 forbids ErrLimitExceeded on
// such a field. The decoder learns which fields those are from the destination:
// a visitor that implements SchemaBoundVisitor (generated code does, wherever
// its schema declares a bound) is exempt on the fields it names.
type Option func(*limits)

// limits holds the optional per-field decode caps plus the string-validity
// policy. The three cap fields' zero value (0) means unlimited, which is their
// default. strictUTF8 is the SOFAB_STRICT_UTF8 option (§6.4) and is set by
// newLimits, whose default is ON; do not construct a limits value directly
// (always go through newLimits) or strictUTF8 would default to its zero value
// (OFF) instead of the intended ON.
type limits struct {
	maxArrayCount uint64 // 0 = unlimited
	maxStringLen  uint64 // 0 = unlimited
	maxBlobLen    uint64 // 0 = unlimited
	strictUTF8    bool   // SOFAB_STRICT_UTF8 (§6.4); default ON via newLimits
}

// WithMaxArrayCount caps the element count of every count-prefixed array — the
// unsigned, signed, and fixlen (fp32/fp64) array types. A message whose array
// claims more than n elements is rejected with ErrLimitExceeded at the count
// header, before any element slice is allocated. A non-positive n leaves the
// limit unset (unlimited).
func WithMaxArrayCount(n int) Option {
	return func(l *limits) { l.maxArrayCount = clampLimit(n) }
}

// WithMaxStringLen caps the byte length of every string field. A string whose
// length header exceeds n bytes is rejected with ErrLimitExceeded before the
// payload is read. A non-positive n leaves the limit unset (unlimited).
func WithMaxStringLen(n int) Option {
	return func(l *limits) { l.maxStringLen = clampLimit(n) }
}

// WithMaxBlobLen caps the byte length of every blob field. A blob whose length
// header exceeds n bytes is rejected with ErrLimitExceeded before the payload is
// read. A non-positive n leaves the limit unset (unlimited).
func WithMaxBlobLen(n int) Option {
	return func(l *limits) { l.maxBlobLen = clampLimit(n) }
}

// WithStrictUTF8 sets the SOFAB_STRICT_UTF8 string-validity policy (§6.4). It
// applies to both the decoder (NewDecoder, AcceptBytes) and the encoder
// (NewEncoder). Unlike the cap options it is not a receiver limit but a
// validation policy, and it defaults to ON — pass WithStrictUTF8(false) to opt
// out.
//
//   - ON (default): an invalid-UTF-8 string that is *read* on decode is the
//     INVALID outcome (ErrInvalidMsg, §5.2); a non-UTF-8 string passed to
//     WriteString on encode is refused with ErrArgument (§6.3). Skipped fields
//     are never validated.
//   - OFF: validation is waived and Go's byte-container string keeps the wire
//     bytes verbatim on decode / writes them verbatim on encode — never lossy,
//     never a silent replacement (§6.4).
//
// Scope on decode: the decoder itself never validates, because it cannot tell a
// field the visitor binds from one it skips and §6.4.5 forbids validating a
// skip. The check belongs to the destination, which is also the only place the
// payload is assembled at all (§6.6.3). The option reaches it: the decoder hands
// the resolved policy to a visitor that implements StringPolicyVisitor —
// typically by embedding a StringCheck — before that scope's first string, and
// the destination arm calls StringCheck.UTF8Valid (utf8.go). A destination that
// instead calls the package-level UTF8Valid is always strict, since a
// package-level function has no decode scope to read.
//
// The knob never changes how valid data is encoded, so two peers with different
// settings interoperate on all valid data. It is a validation policy, never a
// wire-format switch.
func WithStrictUTF8(enabled bool) Option {
	return func(l *limits) { l.strictUTF8 = enabled }
}

// clampLimit maps a caller-supplied limit to its internal form: a non-positive
// value means "no limit" (stored as 0).
func clampLimit(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n)
}

// newLimits folds the options into a limits value. SOFAB_STRICT_UTF8 (§6.4)
// defaults to ON, so strictUTF8 starts true and only WithStrictUTF8(false)
// turns it off; the cap fields start at their zero value (unlimited).
//
// The no-option case — every AcceptBytes, NewDecoder and NewEncoder that
// configures nothing, which is the common one — is the whole body here, and the
// option loop is out of line in applyOptions. That is not layout taste: an
// Option is an opaque func(*limits), so `opt(&l)` makes l ESCAPE, and escape
// analysis is per function, not per branch. With the loop in this body every
// caller heap-allocated a limits value (one allocation and 32 bytes per
// encode/decode entry point) whether or not it passed an option. Split, the
// default path returns a value that stays in the caller's frame and allocates
// nothing; only a call that really has options pays.
func newLimits(opts []Option) limits {
	if len(opts) == 0 {
		return limits{strictUTF8: true}
	}
	return applyOptions(opts)
}

// applyOptions is the out-of-line half of newLimits: the branch in which the
// options escape. See newLimits for why it is not inline.
//
//go:noinline
func applyOptions(opts []Option) limits {
	l := limits{strictUTF8: true}
	for _, opt := range opts {
		opt(&l)
	}
	return l
}

// strictUTF8On reports whether this encode/decode validates string payloads. It
// is the §6.4 gate in full, in the order the section states it: the COMPILE-TIME
// half first (a `sofab_no_strict_utf8` build folds strictUTF8Compiled to a false
// constant, and with it every caller's utf8.Valid call becomes dead code —
// "compiled OFF means the validation code is not compiled in"), then the RUNTIME
// half (WithStrictUTF8). Both halves belong at every site that validates: gating
// only the generated-code primitive (utf8.go) on the build tag would leave a
// footprint build in which the corelib's own paths still validate, so the
// encoder and the destination would reach different verdicts on the same bytes
// in the same build (issue #88).
//
// It costs nothing in the default build: strictUTF8Compiled is a true constant
// there, the call inlines, and `true && l.strictUTF8` folds to the field load
// the callers did before.
func (l limits) strictUTF8On() bool { return strictUTF8Compiled && l.strictUTF8 }

// stringCheck is the SOFAB_STRICT_UTF8 policy (§6.4) in the form a destination
// can hold. The decoder hands it to a scope's StringPolicyVisitor so that
// WithStrictUTF8 reaches the check generated code runs where it materializes a
// string — the runtime half of the gate the package-level primitive cannot see
// (utf8.go, issue #82).
func (l limits) stringCheck() StringCheck {
	return StringCheck{waived: !l.strictUTF8}
}

// checkArrayCount enforces maxArrayCount. The caller has already range-checked n
// against arrayMax.
//
// sb is consulted only once the cap is exceeded, and only to ask whether the
// SCHEMA bounds this field's count: there the schema governs and the cap must
// not fire (§6.2.1, §6.3 — see SchemaBoundVisitor). A decode with no cap set, or
// a count within it, never reaches the question.
func (l limits) checkArrayCount(n uint64, id ID, sb schemaBound) error {
	if l.maxArrayCount == 0 || n <= l.maxArrayCount {
		return nil
	}
	if sb.bounds(id, BoundArrayCount) {
		return nil
	}
	return ErrLimitExceeded
}

// checkFixlen enforces the string/blob byte-length limits at the fixlen header,
// before the payload is buffered. The fp32/fp64 subtypes carry no configurable
// limit (their length is fixed at 4/8 bytes). A field whose schema declares a
// maxlen is exempt for the reason checkArrayCount spells out.
func (l limits) checkFixlen(sub, length uint64, id ID, sb schemaBound) error {
	switch sub {
	case fixStr:
		if l.maxStringLen != 0 && length > l.maxStringLen && !sb.bounds(id, BoundStringLen) {
			return ErrLimitExceeded
		}
	case fixBlob:
		if l.maxBlobLen != 0 && length > l.maxBlobLen && !sb.bounds(id, BoundBlobLen) {
			return ErrLimitExceeded
		}
	}
	return nil
}
