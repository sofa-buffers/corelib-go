package sofab

import "unicode/utf8"

// Utf8Valid reports whether b is well-formed UTF-8. It is the CORELIB_PLAN §6.4
// `utf8_valid` primitive: the check the corelib exposes for the places where
// *generated* code — not the corelib — materializes a string.
//
// Why it exists. Go's string is a byte-container type (§6.4), so the visitor
// decode path hands the wire bytes through verbatim and never validates them:
// the cursor cannot tell a field the consumer binds from one it skips, and
// §6.4 is normative that a skipped string is a length jump over bytes that are
// never inspected. Validation therefore has to happen one layer up, at the
// point the payload is bound into a declared destination — which only generated
// code can identify. Generated code calls Utf8Valid inside each destination arm
// and rejects with ErrInvalidMsg (the INVALID outcome, §5.2) when it returns
// false. The pull parser needs no such help: Decoder.String is by construction a
// materializing read and validates internally, while Decoder.Skip stays a pure
// Discard.
//
// It is a real validator, not a byte-range shortcut (§6.4 "Validator
// correctness", normative): overlong encodings (including C0 80, Java's
// "Modified UTF-8" NUL), surrogate code points U+D800–U+DFFF, and code points
// above U+10FFFF are all rejected. An embedded U+0000 is valid UTF-8 and is
// accepted. The empty slice is valid.
//
// The SOFAB_STRICT_UTF8 gate lives inside the primitive, so callers invoke it
// unconditionally and generated code never has to be regenerated for a
// different build configuration. In the shipped (default) build the check is ON
// and Utf8Valid validates; a footprint build made with the `sofab_no_strict_utf8`
// build tag folds it to a constant true and the validator is compiled out
// entirely (§6.4 lets constrained profiles compile the check out).
//
// Utf8Valid carries the COMPILE-TIME half of the gate only: a package-level
// function has no decoder to read the runtime option from, so it is the
// always-strict form. §6.4 wants both halves ("it folds to true when compiled
// OFF and reads the runtime option otherwise"), and the runtime half needs a
// value scoped to the decode that is running — that is StringCheck, which the
// decoder hands to a visitor implementing StringPolicyVisitor. Destination code
// that wants WithStrictUTF8 to reach it calls StringCheck.Utf8Valid; this
// function stays for callers that have no decode scope at hand.
func Utf8Valid(b []byte) bool {
	if !strictUTF8Compiled {
		return true
	}
	return utf8.Valid(b)
}

// StringCheck is the SOFAB_STRICT_UTF8 policy (§6.4) of one decode, as a value
// the destination can hold. It is what makes the runtime option reachable from
// the visitor path: the decoder resolves the policy from its own options
// (WithStrictUTF8, defaulting to ON) and hands this value to the scope's visitor
// before the first string of that scope is delivered, so the check generated
// code runs at the destination is the one the caller configured — no rebuild,
// no regeneration, and no change to where validation happens (still only where a
// string is materialized, never on a skip).
//
// The zero value is STRICT, matching the §6.4 default: OFF is stored as an
// explicit waiver, so a destination that is never handed a policy — one built by
// hand, or reached through a path that does not deliver — validates rather than
// silently accepting malformed input.
//
// It is a plain comparable struct with no pointer inside, so holding one costs a
// bool and calling Utf8Valid on it inlines to the same test as the package-level
// primitive.
type StringCheck struct {
	// waived is the OFF state (WithStrictUTF8(false)). Stored inverted so the
	// zero value is the ON default; see above.
	waived bool
}

// Utf8Valid reports whether b is well-formed UTF-8 under this decode's policy.
// It is the same validator as the package-level Utf8Valid — real, not a
// byte-range shortcut — with both §6.4 gates in front of it: the compile-time
// one (a `sofab_no_strict_utf8` build folds the whole check to true) and then
// the runtime one (WithStrictUTF8(false) waives it, and the wire bytes are kept
// verbatim, never replaced or dropped).
func (c StringCheck) Utf8Valid(b []byte) bool {
	if !strictUTF8Compiled || c.waived {
		return true
	}
	return utf8.Valid(b)
}

// SetStringCheck stores the policy the decoder resolved for this scope. It
// implements StringPolicyVisitor for any visitor that EMBEDS a StringCheck,
// which is the intended shape: embedding gives the visitor both the setter and a
// promoted Utf8Valid, so a destination arm reads
//
//	if !m.Utf8Valid([]byte(v)) {
//		return sofab.ErrInvalidMsg
//	}
//
// exactly as it read the package-level primitive before.
func (c *StringCheck) SetStringCheck(policy StringCheck) { *c = policy }

// StringPolicyVisitor is an optional extension a Visitor may implement to be
// handed the decode's SOFAB_STRICT_UTF8 policy (§6.4).
//
// It exists because Go's string is a byte-container type, so the corelib's
// visitor path deliberately does not validate — the cursor cannot tell a field
// the visitor binds from one it skips, and §6.4 forbids validating a skip — and
// the check therefore belongs to the destination, which only generated code can
// identify. Without this hook the destination could reach only the package-level
// Utf8Valid, whose sole gate is the build tag: WithStrictUTF8(false) was inert
// for the entire generated decode surface, and the OFF state §6.4 makes
// normative was reachable only by rebuilding with a different build tag — the
// regeneration/rebuild the design exists to avoid (issue #82).
//
// Delivery mirrors the other visitor extensions (HeaderVisitor,
// ElemBoundVisitor): the type assertion is made at most once per scope, and only
// where it can matter — at the first string field of that scope, so a visitor
// that never sees a string never pays for the assertion. A nested sequence
// visitor is its own scope and is handed the same policy when its first string
// arrives. A visitor that does not implement the interface decodes exactly as
// before: no method, no call.
//
// Implementations must simply store the value (embedding StringCheck does this);
// SetStringCheck is called before the scope's first Visitor.String, never
// concurrently, and never with a different policy inside one decode.
type StringPolicyVisitor interface {
	SetStringCheck(StringCheck)
}

// spCache delivers the scope's StringCheck to the visitor at most once, and asks
// the interface question at most once — the same laziness, and for the same
// reason, as hvCache (cursor.go): a failed v.(StringPolicyVisitor) walks the
// type's whole method list, so a scope with no string field must not pay it.
type spCache struct {
	done bool
}

// deliver hands l's policy to v if v accepts one. Called immediately before the
// first Visitor.String of the scope.
func (c *spCache) deliver(v Visitor, l limits) {
	if c.done {
		return
	}
	c.done = true
	if sp, ok := v.(StringPolicyVisitor); ok {
		sp.SetStringCheck(l.stringCheck())
	}
}
