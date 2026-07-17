package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// capV captures the last string and blob handed to it, so a decode with strict
// UTF-8 OFF can assert the wire bytes were preserved verbatim.
type capV struct {
	str    string
	strSet bool
	blob   []byte
}

func (v *capV) Unsigned(sofab.ID, uint64) error { return nil }
func (v *capV) Signed(sofab.ID, int64) error    { return nil }
func (v *capV) Float32(sofab.ID, float32) error { return nil }
func (v *capV) Float64(sofab.ID, float64) error { return nil }
func (v *capV) String(_ sofab.ID, s string) error {
	v.str, v.strSet = s, true
	return nil
}
func (v *capV) Bytes(_ sofab.ID, b []byte) error              { v.blob = append([]byte(nil), b...); return nil }
func (v *capV) UnsignedArray(sofab.ID, []uint64) error        { return nil }
func (v *capV) SignedArray(sofab.ID, []int64) error           { return nil }
func (v *capV) Float32Array(sofab.ID, []float32) error        { return nil }
func (v *capV) Float64Array(sofab.ID, []float64) error        { return nil }
func (v *capV) BeginSequence(sofab.ID) (sofab.Visitor, error) { return v, nil }
func (v *capV) EndSequence() error                            { return nil }

// strField hand-builds a fixlen string field at id with the given raw payload
// (which need not be valid UTF-8).
func strField(id sofab.ID, payload []byte) []byte {
	out := vhdr(id, sofab.TypeFixlen)
	out = append(out, vbytes((uint64(len(payload))<<3)|subStr)...)
	return append(out, payload...)
}

// --- decode, strict ON (default): invalid UTF-8 is INVALID ------------------

// TestStrictUTF8DecodeDefaultRejects proves SOFAB_STRICT_UTF8 defaults to ON:
// an invalid-UTF-8 string is the INVALID outcome (ErrInvalidMsg) on both the
// pull and the visitor decode paths, with no option supplied.
func TestStrictUTF8DecodeDefaultRejects(t *testing.T) {
	in := strField(1, []byte{0xFF}) // 0xFF cannot begin any UTF-8 sequence.

	d := sofab.NewDecoder(bytes.NewReader(in))
	mustNext(t, d)
	if _, err := d.String(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull String default = %v, want ErrInvalidMsg", err)
	}
	if err := sofab.AcceptBytes(in, &capV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("visitor default = %v, want ErrInvalidMsg", err)
	}
}

// TestStrictUTF8DecodeRejectsVariants covers the security-relevant classes a
// real validator must reject: overlong (incl. C0 80, "modified UTF-8" NUL), a
// surrogate code point, and a code point above U+10FFFF.
func TestStrictUTF8DecodeRejectsVariants(t *testing.T) {
	cases := map[string][]byte{
		"overlong-C0-80":    {0xC0, 0x80},       // overlong encoding of U+0000
		"overlong-slash":    {0xC0, 0xAF},       // overlong '/'
		"surrogate-D800":    {0xED, 0xA0, 0x80}, // U+D800
		"above-10FFFF":      {0xF4, 0x90, 0x80, 0x80},
		"bare-continuation": {0x80},
	}
	for name, payload := range cases {
		in := strField(0, payload)
		if err := sofab.AcceptBytes(in, &capV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s visitor = %v, want ErrInvalidMsg", name, err)
		}
		d := sofab.NewDecoder(bytes.NewReader(in))
		mustNext(t, d)
		if _, err := d.String(); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s pull = %v, want ErrInvalidMsg", name, err)
		}
	}
}

// --- decode, strict OFF: bytes preserved verbatim ---------------------------

// TestStrictUTF8DecodeOffVerbatim proves that with the check OFF the same
// invalid-UTF-8 input decodes successfully and the wire bytes are kept verbatim
// (never lossy, no U+FFFD) on both decode paths.
func TestStrictUTF8DecodeOffVerbatim(t *testing.T) {
	payload := []byte{0x41, 0xFF, 0x42} // 'A', invalid, 'B'
	in := strField(1, payload)

	d := sofab.NewDecoder(bytes.NewReader(in), sofab.WithStrictUTF8(false))
	mustNext(t, d)
	got, err := d.String()
	if err != nil {
		t.Fatalf("pull String off = %v, want nil", err)
	}
	if got != string(payload) {
		t.Fatalf("pull String off = % X, want % X", got, payload)
	}

	var v capV
	if err := sofab.AcceptBytes(in, &v, sofab.WithStrictUTF8(false)); err != nil {
		t.Fatalf("visitor off = %v, want nil", err)
	}
	if !v.strSet || v.str != string(payload) {
		t.Fatalf("visitor off string = % X, want % X", v.str, payload)
	}
}

// TestStrictUTF8DecodeOffRoundtrips confirms an invalid-UTF-8 string round-trips
// byte-exactly when both sides run with the check OFF.
func TestStrictUTF8DecodeOffRoundtrips(t *testing.T) {
	raw := string([]byte{0xC3, 0x28, 0xA0, 0xFF}) // assorted invalid bytes

	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf, sofab.WithStrictUTF8(false))
	if err := e.WriteString(7, raw); err != nil {
		t.Fatalf("encode off = %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush = %v", err)
	}

	var v capV
	if err := sofab.AcceptBytes(buf.Bytes(), &v, sofab.WithStrictUTF8(false)); err != nil {
		t.Fatalf("decode off = %v", err)
	}
	if v.str != raw {
		t.Fatalf("roundtrip off = % X, want % X", v.str, raw)
	}
}

// --- encode, strict ON/OFF --------------------------------------------------

// TestStrictUTF8EncodeDefaultRejects proves the symmetric encode-side reject:
// WriteString of a non-UTF-8 Go string is ErrArgument by default, and no bytes
// are written.
func TestStrictUTF8EncodeDefaultRejects(t *testing.T) {
	bad := string([]byte{0x41, 0xFF}) // 'A' + invalid byte

	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteString(1, bad); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("WriteString default = %v, want ErrArgument", err)
	}
	if err := e.Flush(); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("Flush after reject = %v, want sticky ErrArgument", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote % X, want no bytes on reject", buf.Bytes())
	}
}

// TestStrictUTF8EncodeRejectsSurrogate covers an unpaired surrogate expressed as
// its (invalid) UTF-8 byte sequence — WriteString must refuse it under strict ON.
func TestStrictUTF8EncodeRejectsSurrogate(t *testing.T) {
	sur := string([]byte{0xED, 0xA0, 0x80}) // U+D800
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteString(0, sur); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("WriteString surrogate = %v, want ErrArgument", err)
	}
}

// TestStrictUTF8EncodeOffVerbatim proves WriteString writes arbitrary bytes
// verbatim with the check OFF, and that they decode back verbatim (OFF/OFF).
func TestStrictUTF8EncodeOffVerbatim(t *testing.T) {
	bad := string([]byte{0x41, 0xFF, 0x42})
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf, sofab.WithStrictUTF8(false))
	if err := e.WriteString(1, bad); err != nil {
		t.Fatalf("WriteString off = %v, want nil", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush = %v", err)
	}
	want := strField(1, []byte(bad))
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("encoded off = % X, want % X", buf.Bytes(), want)
	}
}

// --- embedded NUL is valid UTF-8 --------------------------------------------

// TestStrictUTF8EmbeddedNUL proves an embedded U+0000 is valid UTF-8: it must
// encode and round-trip under strict ON (and its overlong form C0 80 is caught
// by TestStrictUTF8DecodeRejectsVariants).
func TestStrictUTF8EmbeddedNUL(t *testing.T) {
	s := "a\x00b" // one embedded NUL, valid UTF-8

	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf) // strict ON
	if err := e.WriteString(3, s); err != nil {
		t.Fatalf("WriteString NUL = %v, want nil", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush = %v", err)
	}

	var v capV
	if err := sofab.AcceptBytes(buf.Bytes(), &v); err != nil { // strict ON
		t.Fatalf("decode NUL = %v, want nil", err)
	}
	if v.str != s {
		t.Fatalf("NUL roundtrip = %q, want %q", v.str, s)
	}
}

// --- skipped fields are never validated -------------------------------------

// TestStrictUTF8SkipNotValidated proves the §6.4 rule that a skipped string is a
// length jump that is never UTF-8-validated, even under strict ON: a message
// with an invalid-UTF-8 string that the pull caller Skips decodes cleanly, and
// resync onto the following field works.
func TestStrictUTF8SkipNotValidated(t *testing.T) {
	// [id 1: string 0xFF][id 2: unsigned 5]
	in := strField(1, []byte{0xFF})
	in = append(in, vhdr(2, sofab.TypeVarintUnsigned)...)
	in = append(in, vbytes(5)...)

	d := sofab.NewDecoder(bytes.NewReader(in)) // strict ON (default)
	f := mustNext(t, d)
	if f.ID != 1 {
		t.Fatalf("first field id = %d, want 1", f.ID)
	}
	if err := d.Skip(); err != nil {
		t.Fatalf("Skip invalid-utf8 string = %v, want nil (skips are never validated)", err)
	}
	f = mustNext(t, d)
	if f.ID != 2 {
		t.Fatalf("resync field id = %d, want 2", f.ID)
	}
	v, err := d.Unsigned()
	if err != nil || v != 5 {
		t.Fatalf("after skip Unsigned = (%d,%v), want (5,nil)", v, err)
	}
}
