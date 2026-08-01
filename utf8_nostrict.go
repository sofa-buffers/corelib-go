//go:build sofab_no_strict_utf8

package sofab

// strictUTF8Compiled is false in a footprint build (-tags sofab_no_strict_utf8):
// Utf8Valid folds to a constant true and the validator is compiled out (§6.4
// "Constrained/footprint profiles MAY ... compile the check out entirely"). Such
// a build is a documented non-strict build; the check-ON configuration above
// remains the one CI conformance-tests.
const strictUTF8Compiled = false
