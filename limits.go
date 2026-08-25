package sofab

// Option configures a decode- or encode-time POLICY. Options are passed to
// NewDecoder, AcceptBytes and NewEncoder.
//
// There is exactly one: WithStrictUTF8, the SOFAB_STRICT_UTF8 string-validity
// policy of §6.4.
//
// There is deliberately NO receiver-cap option here. CORELIB_PLAN §6.2.1 puts
// the max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len numbers with
// the layer that knows the schema and the target: "The numbers and the
// allocation are not the codec's." What the codec contributes is the report and
// the category — it surfaces the count at the count/length header and the
// element index inside a sequence array, "and THE VISITOR DECIDES. The codec
// never invents a limit of its own and never clamps to one."
//
// So a receiver cap is applied by the destination: a generated ArrayBegin /
// FixlenBegin arm, or one of the collectors in collectors.go, which take the
// number as a parameter from generated code. ErrLimitExceeded travels back out
// through the ordinary visitor-error path and stays a category of its own
// (§6.3). This package holds no cap, invents no default number, and has no
// unlimited mode to fall out of — §6.2.1 admits neither an unset state nor an
// unlimited one, and the cap fields that used to live here had a zero value
// meaning exactly that.
type Option func(*limits)

// limits holds the string-validity policy a decode or an encode runs under.
// strictUTF8 is the SOFAB_STRICT_UTF8 option (§6.4) and is set by newLimits,
// whose default is ON; do not construct a limits value directly (always go
// through newLimits) or strictUTF8 would default to its zero value (OFF)
// instead of the intended ON.
type limits struct {
	strictUTF8 bool // SOFAB_STRICT_UTF8 (§6.4); default ON via newLimits
}

// WithStrictUTF8 sets the SOFAB_STRICT_UTF8 string-validity policy (§6.4). It
// applies to both the decoder (NewDecoder, AcceptBytes) and the encoder
// (NewEncoder). It is not a receiver limit but a validation policy, and it
// defaults to ON — pass WithStrictUTF8(false) to opt out.
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

// newLimits folds the options into a limits value. SOFAB_STRICT_UTF8 (§6.4)
// defaults to ON, so strictUTF8 starts true and only WithStrictUTF8(false)
// turns it off.
//
// The no-option case — every AcceptBytes, NewDecoder and NewEncoder that
// configures nothing, which is the common one — is the whole body here, and the
// option loop is out of line in applyOptions. That is not layout taste: an
// Option is an opaque func(*limits), so `opt(&l)` makes l ESCAPE, and escape
// analysis is per function, not per branch. With the loop in this body every
// caller heap-allocated a limits value (one allocation per encode/decode entry
// point) whether or not it passed an option. Split, the default path returns a
// value that stays in the caller's frame and allocates nothing; only a call that
// really has options pays. The struct shrinking to a single bool does not
// retire the split: the escape comes from the opaque function value, not from
// the size of what it writes to.
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
