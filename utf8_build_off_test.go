//go:build sofab_no_strict_utf8

package sofab_test

import (
	"bytes"
	"testing"
	"unicode/utf8"

	sofab "github.com/sofa-buffers/corelib-go"
)

// utf8CheckCompiled is false in the footprint build (`-tags
// sofab_no_strict_utf8`), the build §6.4 allows when it says
// "Constrained/footprint profiles MAY default to OFF or compile the check out
// entirely" and pins with "compiled OFF means the validation code is not
// compiled in".
//
// See utf8_build_on_test.go for how the suite uses this constant. The tests
// below are the ones that can ONLY exist here: they assert the fold itself, and
// the §6.4 OFF contract that survives it — the wire bytes are kept verbatim,
// never replaced, dropped or emptied.
const utf8CheckCompiled = false

// TestUTF8ValidatorIsCompiledOut is the counterpart of
// TestUTF8ValidatorIsCompiledIn: it proves the build tag actually reached the
// library. Without it a mis-tagged build would just run the ON suite's
// relaxed-by-branching assertions and look green while validating everything.
func TestUTF8ValidatorIsCompiledOut(t *testing.T) {
	for _, b := range [][]byte{{0xFF}, {0xC0, 0x80}, {0xED, 0xA0, 0x80}, {0x80}} {
		if !sofab.UTF8Valid(b) {
			t.Fatalf("UTF8Valid(% X) = false, want true (validator compiled out)", b)
		}
		var c sofab.StringCheck // the zero value is strict — the build tag still wins
		if !c.UTF8Valid(b) {
			t.Fatalf("zero StringCheck.UTF8Valid(% X) = false, want true", b)
		}
	}
}

// TestNoStrictBuildIgnoresTheRuntimeOption pins the ORDER of the two §6.4 gates:
// the compile-time half comes first, so WithStrictUTF8(true) — including the
// implicit default ON — cannot resurrect a check that is not in the binary. A
// build in which the option still validated would be a build whose validator is
// linked in, i.e. not this build at all.
func TestNoStrictBuildIgnoresTheRuntimeOption(t *testing.T) {
	payload := []byte{0x41, 0xFF, 0x42} // 'A', invalid, 'B'
	in := strField(1, payload)

	for _, opts := range [][]sofab.Option{nil, {sofab.WithStrictUTF8(true)}, {sofab.WithStrictUTF8(false)}} {
		for name, accept := range acceptPaths {
			v := &genStrV{id: 1}
			if err := accept(in, v, opts...); err != nil {
				t.Fatalf("%s (%d opts) = %v, want nil", name, len(opts), err)
			}
			if !v.set || v.got != string(payload) {
				t.Fatalf("%s (%d opts) bound % X (set=%v), want % X verbatim",
					name, len(opts), v.got, v.set, payload)
			}
		}

		var buf bytes.Buffer
		e := sofab.NewEncoder(&buf, opts...)
		if err := e.WriteString(1, string(payload)); err != nil {
			t.Fatalf("WriteString (%d opts) = %v, want nil", len(opts), err)
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush (%d opts) = %v", len(opts), err)
		}
		if !bytes.Equal(buf.Bytes(), in) {
			t.Fatalf("encoded (%d opts) = % X, want % X verbatim", len(opts), buf.Bytes(), in)
		}
	}
}

// TestNoStrictBuildNeverSubstitutes is the normative half of §6.4 that OFF does
// NOT waive: "Silent replacement is forbidden in every mode". Waiving the
// validation must never turn into replacing, dropping or emptying the payload,
// so every invalid class round-trips byte-for-byte and no U+FFFD appears.
func TestNoStrictBuildNeverSubstitutes(t *testing.T) {
	cases := map[string][]byte{
		"overlong-C0-80":    {0xC0, 0x80},
		"surrogate-D800":    {0xED, 0xA0, 0x80},
		"above-10FFFF":      {0xF4, 0x90, 0x80, 0x80},
		"bare-continuation": {0x80},
		"lone-FF":           {0xFF},
		"truncated-2byte":   {0xC2},
	}
	for name, payload := range cases {
		var buf bytes.Buffer
		e := sofab.NewEncoder(&buf)
		if err := e.WriteString(1, string(payload)); err != nil {
			t.Fatalf("%s WriteString = %v, want nil", name, err)
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("%s Flush = %v", name, err)
		}
		if want := strField(1, payload); !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("%s encoded % X, want % X", name, buf.Bytes(), want)
		}

		var v capV
		if err := sofab.AcceptBytes(buf.Bytes(), &v); err != nil {
			t.Fatalf("%s decode = %v, want nil", name, err)
		}
		if !v.strSet || v.str != string(payload) {
			t.Fatalf("%s round-tripped % X, want % X", name, v.str, payload)
		}
		// Checked on the BYTES, not with ContainsRune: decoding a Go string
		// that holds invalid bytes yields utf8.RuneError for each of them, so a
		// rune-level probe reports U+FFFD for input that was never substituted.
		// Substitution means the replacement character's ENCODING (EF BF BD)
		// appearing in the payload.
		if bytes.Contains([]byte(v.str), []byte(string(utf8.RuneError))) {
			t.Fatalf("%s produced U+FFFD: % X (§6.4 forbids substitution in every mode)", name, v.str)
		}
	}
}
