package sofab_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// Crucible F-0042 / corelib-go#58: the fixlen-array schema `count` bound must be
// applied only AFTER the element subtype has decided the field is this array's
// value at all (CORELIB_PLAN §4.8, MESSAGE_SPEC §7.3).
//
// The vectors below are the finding's isolates verbatim, at the schema shape it
// was found on: sequence `arrays` (id 100) → `nested` (id 10) → id 0, declared
// `array<fp32, count 5>`. On the wire `20` is the fixlen_word for fp32 (subtype
// 0, elem_len 4) and `41` the one for fp64 (subtype 1, elem_len 8) — the
// contradicting subtype.
//
// Note what is being tested where. The corelib's part is the decode ORDER and
// the kind it reports: the fixlen_word is read and format-checked first, then
// ArrayBegin fires once with ArrayFp32/ArrayFp64. The verdicts below are the
// observable consequence, produced together with fp32SlotVisitor, which stands
// in for the generated visitor and applies its bound the way sofabgen must emit
// it — inside the arm matching the declared element type.

const (
	// Field headers, as varint bytes: `arrays` and `nested` open a sequence, and
	// id 0 in the inner scope is the declared fp32 array.
	hdrArrays = "\xa6\x06" // (100<<3)|TypeSequenceStart
	hdrNested = "\x56"     // (10<<3)|TypeSequenceStart
	hdrArr    = "\x05"     // (0<<3)|TypeFixlenArray
	hdrArrU   = "\x03"     // (0<<3)|TypeVarintArrayUnsigned
	seqEnd    = "\x07\x07" // close `nested`, then `arrays`
)

// wire assembles a message in the arrays→nested scope: the two sequence headers,
// then body, then both end markers.
func wire(body ...string) []byte {
	out := hdrArrays + hdrNested
	for _, s := range body {
		out += s
	}
	return []byte(out + seqEnd)
}

// zeros is n payload bytes of 0x00.
func zeros(n int) string { return string(make([]byte, n)) }

// fp32SlotRec records what a decode delivered to the declared field: every
// ArrayBegin seen at id 0 in the innermost scope, and the value the generated
// field would end up holding.
type fp32SlotRec struct {
	headers  []string  // "kind/count" per ArrayBegin at id 0
	value    []float32 // the declared field
	valueSet bool      // false = still at its default
}

// fp32SlotVisitor is a generated-style visitor for the `arrays.nested` scope,
// where id 0 is declared `array<fp32, count 5>`. It is the shape sofabgen must
// emit after this change: the schema count bound lives INSIDE the arm that
// matches the declared element type, so a header of any other kind falls through
// to the §7.3 skip — the corelib still delivers it, and the visitor leaves the
// declared field alone.
type fp32SlotVisitor struct {
	baseV
	rec *fp32SlotRec
}

// Structural satisfaction means a stale ArrayBegin signature would silently opt
// out of the hooks rather than fail to compile. Pin it.
var _ aggHeader = fp32SlotVisitor{}

func (s fp32SlotVisitor) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
	if id != 0 {
		return nil
	}
	s.rec.headers = append(s.rec.headers, fmt.Sprintf("%d/%d", kind, count))
	// The bound is keyed on the kind: only an fp32 header is this field's value,
	// so only it is measured against the declared count of 5.
	if kind == sofab.ArrayFp32 && count > 5 {
		return sofab.ErrInvalidMsg
	}
	return nil
}

func (fp32SlotVisitor) FixlenHeader(sofab.ID, int, int) error { return nil }

func (s fp32SlotVisitor) Float32Array(id sofab.ID, v []float32) error {
	if id == 0 {
		s.rec.value = append([]float32(nil), v...)
		s.rec.valueSet = true
	}
	return nil
}

// The remaining array callbacks are the §7.3 skip: a header of a kind the schema
// did not declare at this id is consumed by the corelib and dropped here, and
// must not disturb the declared field (§7.4).
func (fp32SlotVisitor) Float64Array(sofab.ID, []float64) error { return nil }
func (fp32SlotVisitor) UnsignedArray(sofab.ID, []uint64) error { return nil }
func (fp32SlotVisitor) SignedArray(sofab.ID, []int64) error    { return nil }

// scopeRouter walks the visitor down arrays(100) → nested(10) and hands the
// innermost scope to fp32SlotVisitor, so the bound is scoped exactly as the
// schema declares it.
type scopeRouter struct {
	baseV
	depth int
	rec   *fp32SlotRec
}

func (r scopeRouter) BeginSequence(id sofab.ID) (any, error) {
	switch {
	case r.depth == 0 && id == 100:
		return scopeRouter{depth: 1, rec: r.rec}, nil
	case r.depth == 1 && id == 10:
		return fp32SlotVisitor{rec: r.rec}, nil
	}
	return r, nil
}

// TestFixlenArrayBoundAfterSubtype is the F-0042 vector table. Each case states
// the verdict §4.8 requires, and — for the accepted ones — that the declared
// field kept its default unless an fp32 header actually delivered a value.
func TestFixlenArrayBoundAfterSubtype(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error // nil = accept
		// headers is the expected ArrayBegin trace at id 0, as "kind/count".
		headers []string
		// wantValue is the declared field after the decode; nil means it stayed
		// at its default (never set).
		wantValue []float32
	}{
		{
			// Row 1: count 3 within the bound, but subtype fp64 contradicts the
			// declared fp32 → §7.3 skip of 3*8 payload bytes, field untouched.
			name:    "in-count mistyped fp64 skips",
			in:      wire(hdrArr, "\x03\x41", zeros(24)),
			headers: []string{"3/3"},
		},
		{
			// Row 2, THE PRIMARY VECTOR: count 8 exceeds the declared count 5,
			// but the fp64 subtype decides first — the field was never this
			// array's value, so its count is not this array's count. The schema
			// bound MUST NOT be applied.
			name:    "over-count mistyped fp64 skips without applying the bound",
			in:      wire(hdrArr, "\x08\x41", zeros(64)),
			headers: []string{"3/8"},
		},
		{
			// Row 3, THE CONTROL that proves the bound was reordered and not
			// removed: the fp32 subtype matches, so count 8 > 5 is INVALID.
			name:    "over-count matching fp32 stays invalid",
			in:      wire(hdrArr, "\x08\x20", zeros(32)),
			want:    sofab.ErrInvalidMsg,
			headers: []string{"2/8"},
		},
		{
			// Row 4, THE SECOND PRIMARY VECTOR: EOF between the count word and
			// the fixlen_word. The decoder cannot yet know whether this is a
			// field it must bound, so §5.2's precedence does not reach INVALID.
			// The hook must not have fired.
			name: "truncated between the two words is incomplete",
			in:   []byte(hdrArrays + hdrNested + hdrArr + "\x08"),
			want: sofab.ErrIncomplete,
		},
		{
			// Row 5, CONTROL: the fixlen_word arrived and matches, so the bound
			// applies immediately — count 8 > 5 is malformed regardless of what
			// follows, and INVALID dominates the truncation (§5.2).
			name:    "over-count matching fp32 with no payload stays invalid",
			in:      []byte(hdrArrays + hdrNested + hdrArr + "\x08\x20"),
			want:    sofab.ErrInvalidMsg,
			headers: []string{"2/8"},
		},
		{
			// Row 6, HAPPY PATH: matching subtype, count within the bound.
			name:      "matching fp32 within the bound accepts",
			in:        wire(hdrArr, "\x03\x20", zeros(12)),
			headers:   []string{"2/3"},
			wantValue: []float32{0, 0, 0},
		},
		{
			// A zero-count fixlen array still carries its fixlen_word (§4.8), so
			// the hook must still fire exactly once — with the right kind — and
			// no payload is read. This is the case the call-site move is most
			// likely to drop.
			name:    "zero-count mistyped fp64 still fires the hook once",
			in:      wire(hdrArr, "\x00\x41"),
			headers: []string{"3/0"},
		},
		{
			// The matching zero-count array is delivered as an empty fp32 array:
			// an empty fp32 array stays distinguishable from an empty fp64 one.
			name:      "zero-count matching fp32 delivers an empty array",
			in:        wire(hdrArr, "\x00\x20"),
			headers:   []string{"2/0"},
			wantValue: []float32{},
		},
		{
			// A string subtype in a fixlen array is a FORMAT violation (§4.8
			// admits only fp32 and fp64), judged before the hook fires — it must
			// NOT be re-routed to the §7.3 skip path even though its subtype
			// also contradicts the declared fp32.
			name: "string subtype in a fixlen array stays invalid",
			in:   wire(hdrArr, "\x03\x22", zeros(12)),
			want: sofab.ErrInvalidMsg,
		},
		{
			// Width mismatches are the same format violation: fp32 must carry
			// elem_len 4 and fp64 elem_len 8.
			name: "fp32 with the wrong element width stays invalid",
			in:   wire(hdrArr, "\x03\x40", zeros(12)),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "fp64 with the wrong element width stays invalid",
			in:   wire(hdrArr, "\x03\x21", zeros(24)),
			want: sofab.ErrInvalidMsg,
		},
		{
			// A blob subtype, likewise.
			name: "blob subtype in a fixlen array stays invalid",
			in:   wire(hdrArr, "\x03\x23", zeros(12)),
			want: sofab.ErrInvalidMsg,
		},
		{
			// Cross-check that the rule generalized rather than special-casing
			// the fixlen path: an INTEGER-array header at the fp32 slot is the
			// same §7.3 skip one step earlier on the wire, so the schema count
			// bound must not be applied to its count 8 either.
			name:    "unsigned-array header at the fp32 slot skips without the bound",
			in:      wire(hdrArrU, "\x08", zeros(8)),
			headers: []string{"0/8"},
		},
		{
			// §7.4: an occurrence skipped under §7.3 is not an occurrence. A
			// correctly typed earlier array must survive a mis-typed later one at
			// the same id.
			name:      "a skipped later occurrence does not clobber an earlier value",
			in:        wire(hdrArr, "\x03\x20", zeros(12), hdrArr, "\x02\x41", zeros(16)),
			headers:   []string{"2/3", "3/2"},
			wantValue: []float32{0, 0, 0},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			for _, path := range []string{"AcceptBytes", "Feed"} {
				rec := &fp32SlotRec{}
				v := scopeRouter{rec: rec}
				var err error
				if path == "AcceptBytes" {
					err = acceptBytes(c.in, v)
				} else {
					err = feedIn(c.in, 0, v)
				}
				if c.want == nil {
					if err != nil {
						t.Errorf("%s: got %v, want accept", path, err)
					}
				} else if !errors.Is(err, c.want) {
					t.Errorf("%s: got %v, want %v", path, err, c.want)
				}
				if got := fmt.Sprint(rec.headers); got != fmt.Sprint(c.headers) {
					t.Errorf("%s: ArrayBegin trace = %s, want %s", path, got, fmt.Sprint(c.headers))
				}
				if c.wantValue == nil {
					if rec.valueSet {
						t.Errorf("%s: declared field was set to %v, want its default", path, rec.value)
					}
				} else {
					if !rec.valueSet {
						t.Errorf("%s: declared field stayed at its default, want %v", path, c.wantValue)
					} else if fmt.Sprint(rec.value) != fmt.Sprint(c.wantValue) {
						t.Errorf("%s: declared field = %v, want %v", path, rec.value, c.wantValue)
					}
				}
			}
		})
	}
}

// TestFixlenArrayRow6RoundTrip pins the one vector in the set whose re-encode
// equals its input byte for byte: count 3, fp32, full payload. It guards against
// a "fix" that changes what the accepted array decodes to.
func TestFixlenArrayRow6RoundTrip(t *testing.T) {
	in := wire(hdrArr, "\x03\x20", zeros(12))

	rec := &fp32SlotRec{}
	if err := acceptBytes(in, scopeRouter{rec: rec}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rec.valueSet || len(rec.value) != 3 {
		t.Fatalf("decoded value = %v (set %v), want 3 elements", rec.value, rec.valueSet)
	}

	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteSequenceBeginLazy(100); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteSequenceBeginLazy(10); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteFloat32Array(0, rec.value); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteSequenceEnd(); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteSequenceEnd(); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), in) {
		t.Errorf("round-trip = % x, want % x", buf.Bytes(), in)
	}
}

// TestFixlenArrayHeaderCeilingsStayOnTheCountWord pins what did NOT move with
// the hook: the FORMAT ceiling is judged on the count word, before the
// fixlen_word is read, so it fires on a message that carries no fixlen_word at
// all.
//
// Its receiver-cap half is gone, and that is the point. A receiver cap is the
// destination's (§6.2.1), the destination is told about a fixlen array at
// ArrayBegin, and §4.8.1 defers ArrayBegin until the fixlen_word says what the
// elements are — so a cap cannot be judged one word earlier without the codec
// holding it, which is what this change removes. The behavioural consequence is
// recorded here rather than left to be rediscovered: an over-cap fixlen-array
// count in a message truncated between the two words is now INCOMPLETE where it
// was a policy rejection. That is the same shape the schema `count:` bound has
// always had, so the two categories now agree instead of splitting.
func TestFixlenArrayHeaderCeilingsStayOnTheCountWord(t *testing.T) {
	// Count past arrayMax (INT32_MAX), nothing after it: a FORMAT violation, so
	// INVALID rather than INCOMPLETE, and nothing is allocated from the count.
	over := append([]byte(hdrArrays+hdrNested+hdrArr), vbytes(overArrayMax)...)
	rec := &fp32SlotRec{}
	if err := acceptBytes(over, scopeRouter{rec: rec}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Errorf("over-arrayMax count: got %v, want ErrInvalidMsg", err)
	}
	if len(rec.headers) != 0 {
		t.Errorf("over-arrayMax count fired ArrayBegin %v, want no call", rec.headers)
	}

	// A count within the format ceiling but with the fixlen_word missing: no
	// verdict is available yet, so it is INCOMPLETE, and no policy category may
	// appear — the codec holds no cap to reach for.
	lim := []byte(hdrArrays + hdrNested + hdrArr + "\x08")
	rec = &fp32SlotRec{}
	err := acceptBytes(lim, scopeRouter{rec: rec})
	if !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("count word alone: got %v, want ErrIncomplete", err)
	}
	if errors.Is(err, sofab.ErrLimitExceeded) {
		t.Errorf("count word alone: got %v, but the codec holds no cap (§6.2.1)", err)
	}
	if len(rec.headers) != 0 {
		t.Errorf("count word alone fired ArrayBegin %v, want no call", rec.headers)
	}
}

// TestFixlenArrayKindPlainVisitor pins the additive contract for the fixlen
// path: a destination that declares no bound sees no header arm at all, so
// the vectors that a bounded visitor rejects at the header decode on their bytes
// alone.
func TestFixlenArrayKindPlainVisitor(t *testing.T) {
	// Row 3's bytes: no declared bound, so nothing rejects the count.
	if err := acceptBytes(wire(hdrArr, "\x08\x20", zeros(32)), plainVisitor{}); err != nil {
		t.Errorf("over-count complete: got %v, want accept", err)
	}
	// Row 5's bytes: the payload is missing, which is plain truncation here.
	in := []byte(hdrArrays + hdrNested + hdrArr + "\x08\x20")
	if err := acceptBytes(in, plainVisitor{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("over-count no payload: got %v, want ErrIncomplete", err)
	}
	// A format-illegal fixlen_word is INVALID with or without the hook.
	if err := acceptBytes(wire(hdrArr, "\x03\x22", zeros(12)), plainVisitor{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Errorf("string subtype: got %v, want ErrInvalidMsg", err)
	}
}

// TestArrayBeginKinds pins the kind each wire type reports, including that the
// integer arrays are untouched by this change: they still fire at the count word
// and still carry the kind their wire type fixes.
func TestArrayBeginKinds(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"unsigned", wire(hdrArrU, "\x02", "\x01\x02"), "0/2"},
		{"signed", wire("\x04", "\x02", "\x02\x04"), "1/2"},
		{"fp32", wire(hdrArr, "\x02\x20", zeros(8)), "2/2"},
		{"fp64", wire(hdrArr, "\x02\x41", zeros(16)), "3/2"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := &fp32SlotRec{}
			if err := acceptBytes(c.in, scopeRouter{rec: rec}); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := fmt.Sprint(rec.headers); got != fmt.Sprint([]string{c.want}) {
				t.Errorf("ArrayBegin trace = %s, want [%s]", got, c.want)
			}
		})
	}
}

// TestArrayKindOrdinals pins the wire-family-normative ordinals. They are shared
// by every push-API corelib (they match corelib-ts src/constants.ts ArrayKind),
// so generated code may switch on them across languages.
func TestArrayKindOrdinals(t *testing.T) {
	for _, c := range []struct {
		kind sofab.ArrayKind
		want uint8
	}{
		{sofab.ArrayUnsigned, 0},
		{sofab.ArraySigned, 1},
		{sofab.ArrayFp32, 2},
		{sofab.ArrayFp64, 3},
	} {
		if uint8(c.kind) != c.want {
			t.Errorf("ordinal of %v = %d, want %d", c.kind, uint8(c.kind), c.want)
		}
	}
}
