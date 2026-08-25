//go:build !sofab_no_strict_utf8

package sofab_test

// The UTF-8 half of the collector contract (§6.4). It lives in its own file
// because a `sofab_no_strict_utf8` build compiles the validator out entirely, so
// "invalid UTF-8 is rejected" is a property of the default build only — the
// other build's contract is that these bytes survive verbatim, which
// utf8_build_off_test.go already pins for the decode paths.

import (
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// badUTF8Elem is a string array holding one element with a lone 0xFF, which no
// well-formed UTF-8 sequence starts with. Hand-built because the encoder
// refuses to write it.
func badUTF8Elem() []byte {
	in := append(openSeq(), fixlenElem(0, subStr, 1)...)
	in = append(in, 0xFF)
	return append(in, vhdr(0, sofab.TypeSequenceEnd)...)
}

// A collector is a decode DESTINATION, and §6.4 puts the check exactly there:
// the element is being materialized, so this is where its UTF-8 is validated. A
// payload the decoder skips never reaches a collector at all.
func TestStringSeqRejectsInvalidUTF8(t *testing.T) {
	var out []string
	err := collect(badUTF8Elem(), &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1})
	if !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("decode = %v, want ErrInvalidMsg", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %q, want the rejected element not stored", out)
	}
}

// The runtime half of the gate reaches the collector because StringSeq embeds
// StringCheck: WithStrictUTF8(false) is the caller's waiver, and the wire bytes
// are then kept verbatim rather than replaced or dropped.
func TestStringSeqHonorsWithStrictUTF8False(t *testing.T) {
	var out []string
	if err := collect(badUTF8Elem(), &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1}, sofab.WithStrictUTF8(false)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0] != "\xFF" {
		t.Fatalf("out = %q, want the bytes through verbatim", out)
	}
}

// A collector built by hand and never handed a policy validates: the zero value
// of StringCheck is STRICT, so OFF is only ever an explicit waiver.
func TestStringSeqWithoutAPolicyValidates(t *testing.T) {
	var out []string
	s := &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1}
	if err := putString(s, 0, "\xFF"); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("String = %v, want ErrInvalidMsg", err)
	}
}

// A blob is bytes: the same payload is legal in a blob array, and the collector
// must not validate it (§6.4 applies to string destinations only).
func TestBlobSeqDoesNotValidateUTF8(t *testing.T) {
	in := append(openSeq(), fixlenElem(0, subBlob, 1)...)
	in = append(in, 0xFF)
	in = append(in, vhdr(0, sofab.TypeSequenceEnd)...)

	var out [][]byte
	if err := collect(in, &sofab.BlobSeq{Out: &out, Cap: -1, ElemMax: -1}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || len(out[0]) != 1 || out[0][0] != 0xFF {
		t.Fatalf("out = %v, want one element holding 0xFF", out)
	}
}
