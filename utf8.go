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
// entirely (§6.4 lets constrained profiles compile the check out). The runtime
// WithStrictUTF8 option governs the two paths the corelib itself owns — the
// encoder and Decoder.String — and is unreachable from a package-level
// primitive, which is why the compile-time form is the one wired up here.
func Utf8Valid(b []byte) bool {
	if !strictUTF8Compiled {
		return true
	}
	return utf8.Valid(b)
}
