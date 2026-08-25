package sofab_test

import (
	"bytes"
	"errors"
	"fmt"
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
func (v *capV) Bytes(_ sofab.ID, b []byte) error       { v.blob = append([]byte(nil), b...); return nil }
func (v *capV) UnsignedArray(sofab.ID, []uint64) error { return nil }
func (v *capV) SignedArray(sofab.ID, []int64) error    { return nil }
func (v *capV) Float32Array(sofab.ID, []float32) error { return nil }
func (v *capV) Float64Array(sofab.ID, []float64) error { return nil }
func (v *capV) BeginSequence(sofab.ID) (any, error)    { return v, nil }
func (v *capV) EndSequence() error                     { return nil }

// strField hand-builds a fixlen string field at id with the given raw payload
// (which need not be valid UTF-8).
func strField(id sofab.ID, payload []byte) []byte {
	out := vhdr(id, sofab.TypeFixlen)
	out = append(out, vbytes((uint64(len(payload))<<3)|subStr)...)
	return append(out, payload...)
}

// --- the build-configuration-aware assertions (issue #88) -------------------
//
// §6.4 has TWO gates, and the suite has to state the contract for both. The
// runtime one (WithStrictUTF8) is exercised by passing options, below. The
// compile-time one is a build tag, so it cannot be varied inside a run: the
// tests instead branch on utf8CheckCompiled, declared per build in
// utf8_build_on_test.go / utf8_build_off_test.go. Before this, every UTF-8
// assertion hard-coded the ON verdict, so `go test -tags sofab_no_strict_utf8`
// — a build the README documents — was red and could never be run.
//
// utf8Invalid is what an invalid-UTF-8 payload that IS materialized into a
// destination is worth in THIS build: the INVALID outcome where the validator
// is compiled in, and nothing at all where §6.4 let it be compiled out. A
// payload that is only SKIPPED is nil in both builds and needs no helper — that
// rule (§6.4, never validate a skip) is build-independent.
func utf8Invalid() error {
	if utf8CheckCompiled {
		return sofab.ErrInvalidMsg
	}
	return nil
}

// checkUTF8Decode asserts got is the decode verdict utf8Invalid describes.
func checkUTF8Decode(t *testing.T, what string, got error) {
	t.Helper()
	if !utf8CheckCompiled {
		if got != nil {
			t.Fatalf("%s = %v, want nil (the validator is compiled out)", what, got)
		}
		return
	}
	if !errors.Is(got, sofab.ErrInvalidMsg) {
		t.Fatalf("%s = %v, want ErrInvalidMsg", what, got)
	}
}

// checkUTF8Encode is the encode-side twin: §6.4 makes the reject symmetric, so
// where decode says INVALID, WriteString says ErrArgument — and where the check
// is compiled out, the bytes go out verbatim.
func checkUTF8Encode(t *testing.T, what string, got error) {
	t.Helper()
	if !utf8CheckCompiled {
		if got != nil {
			t.Fatalf("%s = %v, want nil (the validator is compiled out)", what, got)
		}
		return
	}
	if !errors.Is(got, sofab.ErrArgument) {
		t.Fatalf("%s = %v, want ErrArgument", what, got)
	}
}

// --- decode, strict ON (default): invalid UTF-8 is INVALID ------------------

// TestStrictUTF8DecodeDefaultRejects proves SOFAB_STRICT_UTF8 defaults to ON:
// an invalid-UTF-8 string that is *materialized* is the INVALID outcome
// (ErrInvalidMsg), with no option supplied. The check lives at the destination
// (§6.4.3: generated code calls sofab.UTF8Valid in the arm that binds the
// value), because the codec cannot tell a field the visitor binds from one it
// skips — and §6.4.5 forbids validating a skip. A visitor with no destination
// for the id must therefore be unaffected.
func TestStrictUTF8DecodeDefaultRejects(t *testing.T) {
	in := strField(1, []byte{0xFF}) // 0xFF cannot begin any UTF-8 sequence.

	checkUTF8Decode(t, "visitor destination default",
		acceptBytes(in, &bindStrV{id: 1}))
	checkUTF8Decode(t, "Feed destination default",
		feedIn(in, 1, &bindStrV{id: 1}))
	var v capV
	if err := acceptBytes(in, &v); err != nil {
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
		if got, want := sofab.UTF8Valid(payload), !utf8CheckCompiled; got != want {
			t.Fatalf("%s UTF8Valid = %v, want %v", name, got, want)
		}
		in := strField(0, payload)
		checkUTF8Decode(t, name+" visitor destination",
			acceptBytes(in, &bindStrV{id: 0}))
		checkUTF8Decode(t, name+" Feed destination",
			feedIn(in, 1, &bindStrV{id: 0}))
	}
}

// TestUTF8ValidPrimitive pins the exported §6.4 primitive: it accepts
// well-formed UTF-8 (including an embedded U+0000 and the empty slice) and is a
// real validator on the reject side. This is the check generated code calls at
// every materialized destination, so its correctness is the whole fix.
func TestUTF8ValidPrimitive(t *testing.T) {
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
		if !sofab.UTF8Valid(b) {
			t.Fatalf("UTF8Valid(%s) = false, want true", name)
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
	// The reject side is exactly what the build tag folds away, so it is
	// asserted against the build: false where the validator is compiled in,
	// true where §6.4 let it be compiled out (never a third answer).
	for name, b := range invalid {
		if got, want := sofab.UTF8Valid(b), !utf8CheckCompiled; got != want {
			t.Fatalf("UTF8Valid(%s) = %v, want %v", name, got, want)
		}
	}
}

// --- decode, strict OFF: bytes preserved verbatim ---------------------------

// TestStrictUTF8DecodeOffVerbatim proves that with the check OFF the same
// invalid-UTF-8 input decodes successfully and the wire bytes are kept verbatim
// (never lossy, no U+FFFD).
func TestStrictUTF8DecodeOffVerbatim(t *testing.T) {
	payload := []byte{0x41, 0xFF, 0x42} // 'A', invalid, 'B'
	in := strField(1, payload)

	var v capV
	if err := acceptBytes(in, &v, sofab.WithStrictUTF8(false)); err != nil {
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
	if err := acceptBytes(buf.Bytes(), &v, sofab.WithStrictUTF8(false)); err != nil {
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
	checkUTF8Encode(t, "WriteString default", e.WriteString(1, bad))
	if !utf8CheckCompiled {
		// The check is compiled out: the bytes go out verbatim (§6.4 OFF is
		// "raw or reject, never silent lossy"), so there is no sticky error.
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush = %v, want nil", err)
		}
		if want := strField(1, []byte(bad)); !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("wrote % X, want % X verbatim", buf.Bytes(), want)
		}
		return
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
	checkUTF8Encode(t, "WriteString surrogate", e.WriteString(0, sur))
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
	if err := acceptBytes(buf.Bytes(), &v); err != nil { // strict ON
		t.Fatalf("decode NUL = %v, want nil", err)
	}
	if v.str != s {
		t.Fatalf("NUL roundtrip = %q, want %q", v.str, s)
	}
}

// --- skipped fields are never validated -------------------------------------

// TestStrictUTF8SkipNotValidated proves §6.4.5: a string the destination does
// not bind is a length jump that is never UTF-8-validated, even under strict ON.
// A message carrying an invalid-UTF-8 string at an id the visitor has no
// destination for decodes cleanly, and the field after it still arrives.
func TestStrictUTF8SkipNotValidated(t *testing.T) {
	// [id 1: string 0xFF][id 2: unsigned 5]
	in := strField(1, []byte{0xFF})
	in = append(in, vhdr(2, sofab.TypeVarintUnsigned)...)
	in = append(in, vbytes(5)...)

	for _, surface := range surfaces {
		// bindStrV validates only the id it declares; here that is id 3, so the
		// invalid payload at id 1 is never bound and never checked.
		v := &bindStrV{id: 3}
		var err error
		switch surface {
		case "AcceptBytes":
			err = acceptBytes(in, v)
		case "Feed":
			err = feedIn(in, 0, v)
		case "Feed/1-byte":
			err = feedIn(in, 1, v)
		}
		if err != nil {
			t.Fatalf("%s = %v, want nil (skips are never validated)", surface, err)
		}
		log, derr := decodeAll(t, surface, in)
		if derr != nil {
			t.Fatalf("%s resync = %v", surface, derr)
		}
		if len(log) == 0 || log[len(log)-1] != evU(2, 5) {
			t.Fatalf("%s never resynced onto id 2: %v", surface, log)
		}
	}
}

// --- the runtime option reaches the visitor destination (§6.4, issue #82) ----

// genStrV models the shape sofabgen emits for a `string` destination, in the
// form that carries this decode's policy: the embedded sofab.StringCheck gives
// it both the StringPolicyVisitor setter the decoder calls at scope entry and
// the promoted UTF8Valid the destination arm runs. The destination lookup still
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
		if !v.UTF8Valid([]byte(s)) {
			return sofab.ErrInvalidMsg
		}
		v.got, v.set = s, true
	}
	return nil
}

func (v *genStrV) BeginSequence(sofab.ID) (any, error) {
	if v.nested != nil {
		return v.nested, nil
	}
	return v, nil
}

// acceptPaths runs one message through every visitor entry point, so a policy
// that reaches only one of them cannot pass. Each entry takes the decode options
// the same way the caller would.
var acceptPaths = map[string]func(in []byte, v any, opts ...sofab.Option) error{
	"AcceptBytes": func(in []byte, v any, opts ...sofab.Option) error {
		return acceptBytes(in, v, opts...)
	},
	"Feed": func(in []byte, v any, opts ...sofab.Option) error {
		return feedIn(in, 0, v, opts...)
	},
	"Feed/1-byte": func(in []byte, v any, opts ...sofab.Option) error {
		return feedIn(in, 1, v, opts...)
	},
}

// TestStrictUTF8OffReachesVisitorDestination is the regression test for issue
// #82: WithStrictUTF8(false) must reach the check the destination runs on the
// visitor path. §6.4 puts both gates inside the primitive — it "folds to true
// when compiled OFF and reads the runtime option otherwise" — so flipping the
// option must never require regenerating or rebuilding anything. Before the
// fix the destination could only reach the package-level UTF8Valid, whose sole
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
	payload := []byte{0x41, 0xFF, 0x42}
	in := strField(1, payload)
	for name, accept := range acceptPaths {
		for _, opts := range [][]sofab.Option{nil, {sofab.WithStrictUTF8(true)}} {
			v := &genStrV{id: 1}
			err := accept(in, v, opts...)
			checkUTF8Decode(t, fmt.Sprintf("%s on (%d opts)", name, len(opts)), err)
			if utf8CheckCompiled {
				if v.set {
					t.Fatalf("%s on bound %q, want the field rejected", name, v.got)
				}
			} else if !v.set || v.got != string(payload) {
				// The compile-time gate wins over the runtime one, so the
				// destination binds the wire bytes verbatim (§6.4).
				t.Fatalf("%s (%d opts) bound % X (set=%v), want % X verbatim",
					name, len(opts), v.got, v.set, payload)
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
		checkUTF8Decode(t, name+" nested on", accept(in, outer))
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
	if got, want := c.UTF8Valid([]byte{0xFF}), !utf8CheckCompiled; got != want {
		t.Fatalf("zero-value StringCheck.UTF8Valid(FF) = %v, want %v", got, want)
	}
	if !c.UTF8Valid([]byte("ok")) {
		t.Fatal("zero-value StringCheck rejected valid UTF-8")
	}

	v := &genStrV{id: 1}
	checkUTF8Decode(t, "undelivered destination", v.String(1, "\xFF"))

	// And the delivered OFF policy is not sticky across decodes: a fresh
	// destination is strict again (in a build that has a check to be strict
	// with — where it is compiled out, both decodes accept).
	if err := acceptBytes(strField(1, []byte{0xFF}), v, sofab.WithStrictUTF8(false)); err != nil {
		t.Fatalf("off decode = %v, want nil", err)
	}
	checkUTF8Decode(t, "fresh destination after an off decode",
		acceptBytes(strField(1, []byte{0xFF}), &genStrV{id: 1}))
}

// TestUTF8ValidPrimitiveStaysAlwaysStrict pins the compatibility contract the
// fix keeps: the package-level primitive ignores the RUNTIME option, so code
// generated before the scoped check existed still compiles and still rejects —
// WithStrictUTF8(false) does not reach it, because a package-level function has
// no decode scope to read the option from. The COMPILE-TIME gate is the one
// thing that does reach it: §6.4 puts it first, so a `sofab_no_strict_utf8`
// build folds the primitive to true and this test asserts that instead.
func TestUTF8ValidPrimitiveStaysAlwaysStrict(t *testing.T) {
	in := strField(1, []byte{0xFF})
	checkUTF8Decode(t, "package-level primitive under off",
		acceptBytes(in, &bindStrV{id: 1}, sofab.WithStrictUTF8(false)))
	if got, want := sofab.UTF8Valid([]byte{0xFF}), !utf8CheckCompiled; got != want {
		t.Fatalf("UTF8Valid(FF) = %v, want %v", got, want)
	}
}
