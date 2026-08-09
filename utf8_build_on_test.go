//go:build !sofab_no_strict_utf8

package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// utf8CheckCompiled mirrors, for the test suite, the compile-time half of the
// §6.4 SOFAB_STRICT_UTF8 gate the library keeps in utf8_strict.go /
// utf8_nostrict.go. Every UTF-8 assertion in the suite is written against it, so
// one test body states the contract for both builds instead of the suite
// silently asserting only the shipped one (issue #88).
//
// This file is the DEFAULT (shipped) build: the validator is compiled in, so
// invalid UTF-8 is rejected wherever a string is materialized. §6.4 makes this
// the configuration the shared vectors and the differential fuzzer run in, and
// the one CI must conformance-test — see ci.yml, where it is the whole matrix
// and the tag build is one extra leg.
//
// The twin file utf8_build_off_test.go carries the false constant and the tests
// that can only exist in the footprint build.
const utf8CheckCompiled = true

// TestUtf8ValidatorIsCompiledIn pins what the default build owes: the validator
// is really linked in, not folded away. It is the direct counterpart of
// TestUtf8ValidatorIsCompiledOut in the twin file, so whichever build runs, one
// of the two proves which half of the §6.4 gate this binary was built with.
func TestUtf8ValidatorIsCompiledIn(t *testing.T) {
	if sofab.Utf8Valid([]byte{0xFF}) {
		t.Fatal("Utf8Valid(FF) = true in the default build, want false")
	}
	var c sofab.StringCheck // zero value is strict (§6.4 default ON)
	if c.Utf8Valid([]byte{0xC0, 0x80}) {
		t.Fatal("StringCheck.Utf8Valid(C0 80) = true in the default build, want false")
	}

	in := strField(1, []byte{0xFF})
	d := sofab.NewDecoder(bytes.NewReader(in))
	mustNext(t, d)
	if _, err := d.String(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull String = %v, want ErrInvalidMsg", err)
	}
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteString(1, "\xFF"); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("WriteString = %v, want ErrArgument", err)
	}
}
