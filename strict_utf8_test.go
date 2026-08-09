package sofab_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/iotest"

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
// an invalid-UTF-8 string that is *materialized* is the INVALID outcome
// (ErrInvalidMsg), with no option supplied — on the pull path (Decoder.String
// validates internally) and at a visitor destination (generated code calls
// sofab.Utf8Valid in the arm that binds the value). A visitor with no
// destination for the id must not be affected: §6.4 forbids validating a field
// that is only skipped.
func TestStrictUTF8DecodeDefaultRejects(t *testing.T) {
	in := strField(1, []byte{0xFF}) // 0xFF cannot begin any UTF-8 sequence.

	d := sofab.NewDecoder(bytes.NewReader(in))
	mustNext(t, d)
	if _, err := d.String(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull String default = %v, want ErrInvalidMsg", err)
	}
	if err := sofab.AcceptBytes(in, &bindStrV{id: 1}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("visitor destination default = %v, want ErrInvalidMsg", err)
	}
	var v capV
	if err := sofab.AcceptBytes(in, &v); err != nil {
		t.Fatalf("visitor without destination = %v, want nil", err)
	}
	if !v.strSet || v.str != "\xFF" {
		t.Fatalf("unvalidated payload = % X, want FF verbatim", v.str)
	}
}

// TestStrictUTF8DecodeRejectsVariants covers the security-relevant classes a
// real validator must reject: overlong (incl. C0 80, "modified UTF-8" NUL), a
// surrogate code point, and a code point above U+10FFFF. Relocating the check
// to the destination must not downgrade it to a byte-range shortcut, so every
// class is asserted at a materialized position on both decode paths and
// directly against the exported primitive.
func TestStrictUTF8DecodeRejectsVariants(t *testing.T) {
	cases := map[string][]byte{
		"overlong-C0-80":    {0xC0, 0x80},       // overlong encoding of U+0000
		"overlong-slash":    {0xC0, 0xAF},       // overlong '/'
		"surrogate-D800":    {0xED, 0xA0, 0x80}, // U+D800
		"surrogate-DFFF":    {0xED, 0xBF, 0xBF}, // U+DFFF
		"above-10FFFF":      {0xF4, 0x90, 0x80, 0x80},
		"bare-continuation": {0x80},
	}
	for name, payload := range cases {
		if sofab.Utf8Valid(payload) {
			t.Fatalf("%s Utf8Valid = true, want false", name)
		}
		in := strField(0, payload)
		if err := sofab.AcceptBytes(in, &bindStrV{id: 0}); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s visitor destination = %v, want ErrInvalidMsg", name, err)
		}
		d := sofab.NewDecoder(bytes.NewReader(in))
		mustNext(t, d)
		if _, err := d.String(); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s pull = %v, want ErrInvalidMsg", name, err)
		}
	}
}

// TestUtf8ValidPrimitive pins the exported §6.4 primitive: it accepts
// well-formed UTF-8 (including an embedded U+0000 and the empty slice) and is a
// real validator on the reject side. This is the check generated code calls at
// every materialized destination, so its correctness is the whole fix.
func TestUtf8ValidPrimitive(t *testing.T) {
	valid := map[string][]byte{
		"empty":         {},
		"ascii":         []byte("hello"),
		"embedded-NUL":  []byte("a\x00b"),
		"lone-NUL":      {0x00},
		"two-byte":      {0xC2, 0xA2},       // U+00A2
		"three-byte":    {0xE2, 0x82, 0xAC}, // U+20AC
		"four-byte":     {0xF0, 0x90, 0x8D, 0x88},
		"max-codepoint": {0xF4, 0x8F, 0xBF, 0xBF}, // U+10FFFF
	}
	for name, b := range valid {
		if !sofab.Utf8Valid(b) {
			t.Fatalf("Utf8Valid(%s) = false, want true", name)
		}
	}
	invalid := map[string][]byte{
		"overlong-C0-80":   {0xC0, 0x80},
		"overlong-C1":      {0xC1, 0xBF},
		"overlong-3byte":   {0xE0, 0x80, 0xAF},
		"overlong-4byte":   {0xF0, 0x80, 0x80, 0x80},
		"surrogate-D800":   {0xED, 0xA0, 0x80},
		"surrogate-DFFF":   {0xED, 0xBF, 0xBF},
		"above-10FFFF":     {0xF4, 0x90, 0x80, 0x80},
		"lone-FF":          {0xFF},
		"truncated-2byte":  {0xC2},
		"continuation-8A":  {0x8A},
		"trailing-partial": {'a', 0xE2, 0x82},
	}
	for name, b := range invalid {
		if sofab.Utf8Valid(b) {
			t.Fatalf("Utf8Valid(%s) = true, want false", name)
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

// --- the runtime option reaches the visitor destination (§6.4, issue #82) ----

// genStrV models the shape sofabgen emits for a `string` destination, in the
// form that carries this decode's policy: the embedded sofab.StringCheck gives
// it both the StringPolicyVisitor setter the decoder calls at scope entry and
// the promoted Utf8Valid the destination arm runs. The destination lookup still
// comes first, so an id this visitor does not bind is never inspected.
type genStrV struct {
	baseV
	sofab.StringCheck
	id     sofab.ID
	got    string
	set    bool
	nested *genStrV // the visitor a nested sequence is decoded into, if any
}

func (v *genStrV) String(id sofab.ID, s string) error {
	switch id {
	case v.id:
		if !v.Utf8Valid([]byte(s)) {
			return sofab.ErrInvalidMsg
		}
		v.got, v.set = s, true
	}
	return nil
}

func (v *genStrV) BeginSequence(sofab.ID) (sofab.Visitor, error) {
	if v.nested != nil {
		return v.nested, nil
	}
	return v, nil
}

// acceptPaths runs one message through every visitor entry point, so a policy
// that reaches only one of them cannot pass. Each entry takes the decode options
// the same way the caller would.
var acceptPaths = map[string]func(in []byte, v sofab.Visitor, opts ...sofab.Option) error{
	"AcceptBytes": func(in []byte, v sofab.Visitor, opts ...sofab.Option) error {
		return sofab.AcceptBytes(in, v, opts...)
	},
	"Accept": func(in []byte, v sofab.Visitor, opts ...sofab.Option) error {
		return sofab.NewDecoder(bytes.NewReader(in), opts...).Accept(v)
	},
	"AcceptStream": func(in []byte, v sofab.Visitor, opts ...sofab.Option) error {
		return sofab.NewDecoder(iotest.OneByteReader(bytes.NewReader(in)), opts...).AcceptStream(v)
	},
}

// TestStrictUTF8OffReachesVisitorDestination is the regression test for issue
// #82: WithStrictUTF8(false) must reach the check the destination runs on the
// visitor path. §6.4 puts both gates inside the primitive — it "folds to true
// when compiled OFF and reads the runtime option otherwise" — so flipping the
// option must never require regenerating or rebuilding anything. Before the
// fix the destination could only reach the package-level Utf8Valid, whose sole
// gate is the build tag, and the OFF state was unreachable on the entire
// generated decode surface.
//
// OFF is pinned, not merely permissive: Go's string is a byte-container type, so
// the wire bytes must arrive verbatim — never replaced, dropped or emptied.
func TestStrictUTF8OffReachesVisitorDestination(t *testing.T) {
	payload := []byte{0x41, 0xFF, 0x42} // 'A', invalid, 'B'
	in := strField(1, payload)
	for name, accept := range acceptPaths {
		v := &genStrV{id: 1}
		if err := accept(in, v, sofab.WithStrictUTF8(false)); err != nil {
			t.Fatalf("%s off = %v, want nil", name, err)
		}
		if !v.set || v.got != string(payload) {
			t.Fatalf("%s off bound % X (set=%v), want % X verbatim", name, v.got, v.set, payload)
		}
	}
}

// TestStrictUTF8OnReachesVisitorDestination is the other half: with no option
// (the §6.4 default) and with WithStrictUTF8(true), the same destination rejects
// the same payload as INVALID on every entry point. The scoped check must not
// have turned the default lax — a policy value that is never delivered, or one
// delivered with the wrong polarity, would silently accept malformed input.
func TestStrictUTF8OnReachesVisitorDestination(t *testing.T) {
	in := strField(1, []byte{0x41, 0xFF, 0x42})
	for name, accept := range acceptPaths {
		for _, opts := range [][]sofab.Option{nil, {sofab.WithStrictUTF8(true)}} {
			v := &genStrV{id: 1}
			if err := accept(in, v, opts...); !errors.Is(err, sofab.ErrInvalidMsg) {
				t.Fatalf("%s on (%d opts) = %v, want ErrInvalidMsg", name, len(opts), err)
			}
			if v.set {
				t.Fatalf("%s on bound %q, want the field rejected", name, v.got)
			}
		}
	}
}

// TestStrictUTF8PolicyReachesNestedScope proves the policy follows the visitor
// into a nested sequence, which is its own scope with its own visitor (a
// generated nested object, or a wrapper-array element collector). A delivery
// made only at the top level would leave every nested string destination stuck
// on the compiled-in default.
func TestStrictUTF8PolicyReachesNestedScope(t *testing.T) {
	payload := []byte{0xFF, 0xFE}
	// [seq 3 start][id 1: string FF FE][seq end]
	in := vhdr(3, sofab.TypeSequenceStart)
	in = append(in, strField(1, payload)...)
	in = append(in, vhdr(0, sofab.TypeSequenceEnd)...)

	for name, accept := range acceptPaths {
		inner := &genStrV{id: 1}
		outer := &genStrV{id: 1, nested: inner}
		if err := accept(in, outer, sofab.WithStrictUTF8(false)); err != nil {
			t.Fatalf("%s nested off = %v, want nil", name, err)
		}
		if !inner.set || inner.got != string(payload) {
			t.Fatalf("%s nested off bound % X (set=%v), want % X", name, inner.got, inner.set, payload)
		}
		if outer.set {
			t.Fatalf("%s delivered the nested string to the outer scope", name)
		}

		inner = &genStrV{id: 1}
		outer = &genStrV{id: 1, nested: inner}
		if err := accept(in, outer); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s nested on = %v, want ErrInvalidMsg", name, err)
		}
	}
}

// TestStrictUTF8ScopedCheckNeverValidatesASkip pins the rule the scoped check
// must not disturb: validation follows the destination, so a string at an id the
// visitor does not bind decodes cleanly under strict ON. Delivering the policy
// to the scope is not the same as validating in the decoder.
func TestStrictUTF8ScopedCheckNeverValidatesASkip(t *testing.T) {
	in := strField(9, []byte{0xFF}) // id 9; the visitor binds id 1
	for name, accept := range acceptPaths {
		v := &genStrV{id: 1}
		if err := accept(in, v); err != nil {
			t.Fatalf("%s undeclared id = %v, want nil (skips are never validated)", name, err)
		}
		if v.set {
			t.Fatalf("%s bound an id it does not declare", name)
		}
	}
}

// TestStringCheckZeroValueIsStrict pins the safety property of the value's
// layout: OFF is stored as an explicit waiver, so a StringCheck that was never
// handed a policy — a destination built by hand, or one used outside a decode —
// validates. Getting this backwards would make every un-delivered destination
// silently accept malformed input.
func TestStringCheckZeroValueIsStrict(t *testing.T) {
	var c sofab.StringCheck
	if c.Utf8Valid([]byte{0xFF}) {
		t.Fatal("zero-value StringCheck accepted invalid UTF-8, want strict")
	}
	if !c.Utf8Valid([]byte("ok")) {
		t.Fatal("zero-value StringCheck rejected valid UTF-8")
	}

	v := &genStrV{id: 1}
	if err := v.String(1, "\xFF"); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("undelivered destination = %v, want ErrInvalidMsg", err)
	}

	// And the delivered OFF policy is not sticky across decodes: a fresh
	// destination is strict again.
	if err := sofab.AcceptBytes(strField(1, []byte{0xFF}), v, sofab.WithStrictUTF8(false)); err != nil {
		t.Fatalf("off decode = %v, want nil", err)
	}
	if err := sofab.AcceptBytes(strField(1, []byte{0xFF}), &genStrV{id: 1}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("fresh destination after an off decode = %v, want ErrInvalidMsg", err)
	}
}

// TestUtf8ValidPrimitiveStaysAlwaysStrict pins the compatibility contract the
// fix keeps: the package-level primitive is the always-strict form, so code
// generated before the scoped check existed still compiles and still rejects —
// WithStrictUTF8(false) does not reach it, because a package-level function has
// no decode scope to read the option from.
func TestUtf8ValidPrimitiveStaysAlwaysStrict(t *testing.T) {
	in := strField(1, []byte{0xFF})
	if err := sofab.AcceptBytes(in, &bindStrV{id: 1}, sofab.WithStrictUTF8(false)); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("package-level primitive under off = %v, want ErrInvalidMsg", err)
	}
	if sofab.Utf8Valid([]byte{0xFF}) {
		t.Fatal("Utf8Valid accepted invalid UTF-8")
	}
}
