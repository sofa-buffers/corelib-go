//go:build !sofab_no_strict_utf8

package sofab

// strictUTF8Compiled is the compile-time half of SOFAB_STRICT_UTF8 (§6.4). This
// is the shipped default — the check is compiled in, and conformance testing
// plus the differential fuzzer run in exactly this configuration.
const strictUTF8Compiled = true
