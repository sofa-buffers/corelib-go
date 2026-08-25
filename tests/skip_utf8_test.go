package sofab_test

import (
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// Regression suite for Crucible finding F-0038 (issue #57): strict UTF-8
// validation ran on a `string` field the decoder was SKIPPING.
//
// CORELIB_PLAN §6.4, normative: "Skipping stays what it is everywhere else in
// the design: a length jump over bytes that are not inspected (§5.2). UTF-8
// validation runs only where a `string` is materialized — read into a
// destination — never on skip, in any mode."
//
// The corelib's visitor path cannot tell a bound field from a skipped one: an id
// the schema never declared, and an id whose wire type contradicts the schema
// (MESSAGE_SPEC §7.3, which routes to the same skip), arrive at Visitor.String
// exactly like a declared field does. So the cursor hands the wire bytes through
// verbatim (legal — Go's string is a byte-container type per §6.4) and the
// consumer validates at the destination with sofab.UTF8Valid.
//
// probeV below mirrors the shape sofabgen emits for the Crucible probe schema:
// the destination switch comes FIRST, and the maxlen bound and UTF8Valid run
// inside the matched arm. An id with no arm falls out untouched.

// --- a probe-shaped visitor over the Crucible schema ids ---------------------
//
// top level : id 0 = u8, id 10 = nested (seq), id 200 = string_array (seq),
//             id 202 = struct_array (seq). id 9 is declared by nothing.
// nested    : id 2 = str, maxlen 32.
// string_arr: element id = index, string, maxlen 64.
// struct_arr: element id = index, a sequence of { id 0 = k, id 1 = v maxlen 16 }.

type probeV struct {
	baseV
	u8       uint64
	u8Set    bool
	nested   probeNestedV
	strArray []string
	structV  []string
}

func (p *probeV) Unsigned(id sofab.ID, v uint64) error {
	switch id {
	case 0:
		p.u8, p.u8Set = v, true
	}
	return nil
}

// String has no top-level string destination in this schema, so every string
// reaching it is skipped: no maxlen check, no transcode, no validation.
func (p *probeV) String(sofab.ID, string) error { return nil }

func (p *probeV) BeginSequence(id sofab.ID) (any, error) {
	switch id {
	case 10:
		return &p.nested, nil
	case 200:
		return &probeStrArrayV{p: p}, nil
	case 202:
		return &probeStructArrayV{p: p}, nil
	}
	return baseV{}, nil // undeclared sequence: descend and ignore
}

type probeNestedV struct {
	baseV
	str    string
	strSet bool
}

func (n *probeNestedV) String(id sofab.ID, v string) error {
	switch id {
	case 2:
		if len(v) > 32 { // schema maxlen (§7.1), checked before the content
			return sofab.ErrInvalidMsg
		}
		if !sofab.UTF8Valid([]byte(v)) {
			return sofab.ErrInvalidMsg
		}
		n.str, n.strSet = v, true
	}
	return nil
}

func (n *probeNestedV) BeginSequence(sofab.ID) (any, error) { return baseV{}, nil }

type probeStrArrayV struct {
	baseV
	p *probeV
}

func (s *probeStrArrayV) String(id sofab.ID, v string) error {
	if len(v) > 64 { // element maxlen
		return sofab.ErrInvalidMsg
	}
	if !sofab.UTF8Valid([]byte(v)) {
		return sofab.ErrInvalidMsg
	}
	for len(s.p.strArray) <= int(id) {
		s.p.strArray = append(s.p.strArray, "")
	}
	s.p.strArray[id] = v
	return nil
}

type probeStructArrayV struct {
	baseV
	p *probeV
}

// String on the wrapper has no destination: every element is a sequence, so a
// fixlen string arriving at an element slot is the §7.3 mistyped field and must
// be skipped, not validated.
func (s *probeStructArrayV) String(sofab.ID, string) error { return nil }

func (s *probeStructArrayV) BeginSequence(sofab.ID) (any, error) {
	return &probeStructElemV{p: s.p}, nil
}

type probeStructElemV struct {
	baseV
	p *probeV
}

func (e *probeStructElemV) String(id sofab.ID, v string) error {
	switch id {
	case 1:
		if len(v) > 16 { // element `v` maxlen
			return sofab.ErrInvalidMsg
		}
		if !sofab.UTF8Valid([]byte(v)) {
			return sofab.ErrInvalidMsg
		}
		e.p.structV = append(e.p.structV, v)
	}
	return nil
}

func (e *probeStructElemV) BeginSequence(sofab.ID) (any, error) { return baseV{}, nil }

// materializedStrings returns every string this probe bound into a destination.
// It is what lets the isolates below assert the OTHER half of §6.4: where the
// validator is compiled out (`-tags sofab_no_strict_utf8`) the payload is not
// rejected, and it must then arrive VERBATIM — waiving validation never licenses
// replacing, dropping or emptying the bytes.
func (p *probeV) materializedStrings() []string {
	var out []string
	if p.nested.strSet {
		out = append(out, p.nested.str)
	}
	out = append(out, p.strArray...)
	return append(out, p.structV...)
}

// --- the F-0038 isolates ----------------------------------------------------

// TestF0038SkippedStringNotValidated runs the exact wire isolates from issue #57
// (and the derived framing/content probes) through a probe-shaped visitor.
func TestF0038SkippedStringNotValidated(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error // nil = accepted
		// materialized, when non-nil, marks a case whose invalid-UTF-8 payload
		// reaches a DECLARED destination, and carries the bytes that
		// destination must receive. Those are the only cases whose verdict
		// depends on the build: `want` holds the default build's INVALID, and
		// where §6.4 let the validator be compiled out the case is accepted and
		// these exact bytes must arrive verbatim (issue #88). Every other case
		// here is a skip, and a skip is never validated in either build.
		materialized []byte
	}{
		{
			// THE PRIMARY VECTOR. 4a = id 9, FIXLEN. 0a = fixlen_word len 1,
			// subtype STRING. 8a = a lone continuation byte (invalid UTF-8).
			// Id 9 is undeclared, so the field is skipped: accepted.
			name: "V1 unknown id, invalid utf8 string",
			in:   []byte{0x4a, 0x0a, 0x8a},
		},
		{
			// THE §7.3 SHAPE. d6 0c = id 202 (struct_array), SEQUENCE_START.
			// 02 = element index 0, wire type FIXLEN — but element 0 is declared
			// a sequence, so the wire type contradicts the schema. 12 = len 2,
			// subtype STRING. ff ff = invalid UTF-8. 07 = SEQUENCE_END.
			// §7.3: skipped exactly as an unknown id is, never INVALID.
			name: "V2 §7.3 mistyped element slot",
			in:   []byte{0xd6, 0x0c, 0x02, 0x12, 0xff, 0xff, 0x07},
		},
		{
			// CONTROL C2: same unknown id, valid payload "A". Rules out the id.
			name: "C2 unknown id, valid utf8",
			in:   []byte{0x4a, 0x0a, 0x41},
		},
		{
			// CONTROL C3: same 0x8a byte, subtype BLOB (0b = (1<<3)|3). Rules
			// out the byte. A blob is never UTF-8 validated on any path.
			name: "C3 unknown id, blob payload",
			in:   []byte{0x4a, 0x0b, 0x8a},
		},
		{
			// CONTROL C1, LOAD-BEARING. 56 = id 10 (nested), SEQUENCE_START.
			// 12 = id 2 (nested.str), FIXLEN. 0a = len 1, subtype STRING.
			// 8a = invalid UTF-8. 07 = SEQUENCE_END. The field IS declared, so
			// the payload is materialized and the check MUST fire. If this
			// flips to accepted, the check was deleted instead of moved.
			name:         "C1 declared nested.str, invalid utf8",
			in:           []byte{0x56, 0x12, 0x0a, 0x8a, 0x07},
			want:         sofab.ErrInvalidMsg,
			materialized: []byte{0x8a},
		},
		{
			// A string_array element (wrapper id 200) is a materialized
			// destination too. c6 0c = id 200 SEQUENCE_START; 02 = element 0
			// FIXLEN; 0a = len 1 STRING; 8a; 07 = SEQUENCE_END.
			name:         "string_array element, invalid utf8",
			in:           []byte{0xc6, 0x0c, 0x02, 0x0a, 0x8a, 0x07},
			want:         sofab.ErrInvalidMsg,
			materialized: []byte{0x8a},
		},
		{
			// A struct_array element's `v` (id 202 -> element seq -> id 1).
			// d6 0c = id 202 SEQUENCE_START; 06 = element 0 SEQUENCE_START;
			// 0a = id 1 FIXLEN; 0a = len 1 STRING; 8a; 07 07 = both ends.
			name:         "struct_array element v, invalid utf8",
			in:           []byte{0xd6, 0x0c, 0x06, 0x0a, 0x0a, 0x8a, 0x07, 0x07},
			want:         sofab.ErrInvalidMsg,
			materialized: []byte{0x8a},
		},
		{
			// FRAMING STAYS ON THE SKIP PATH (the F-0012 half). fixlen_word
			// 0c = (1<<3)|4 -> subtype 0x4, RESERVED, at an unknown id. §4.6:
			// a decoder MUST reject a fixlen field carrying a reserved subtype.
			name: "reserved subtype at a skipped field",
			in:   []byte{0x4a, 0x0c, 0x8a},
			want: sofab.ErrInvalidMsg,
		},
		{
			// INCOMPLETE must not fold into either neighbour (§5.2). The
			// skipped string declares length 2 (fixlen_word 0x12) and only one
			// payload byte is present: INCOMPLETE, never accepted, and the
			// invalid byte 0xff is not inspected on the way there.
			name: "truncated skipped string",
			in:   []byte{0x4a, 0x12, 0xff},
			want: sofab.ErrIncomplete,
		},
		{
			// An overlong C0 80 at a SKIPPED position is not looked at.
			name: "overlong C0 80 at a skipped field",
			in:   []byte{0x4a, 0x12, 0xc0, 0x80},
		},
		{
			// The same overlong C0 80 in the DECLARED nested.str is still
			// rejected: relocating the call must not downgrade the validator to
			// a byte-range shortcut.
			name:         "overlong C0 80 at nested.str",
			in:           []byte{0x56, 0x12, 0x12, 0xc0, 0x80, 0x07},
			want:         sofab.ErrInvalidMsg,
			materialized: []byte{0xc0, 0x80},
		},
		{
			// An undeclared id nested INSIDE a declared sequence is skipped the
			// same way (4a 0a 8a between 56 ... 07).
			name: "unknown id inside a declared sequence",
			in:   []byte{0x56, 0x4a, 0x0a, 0x8a, 0x07},
		},
		{
			// §7.3 at a DECLARED string id: nested.str (id 2, declared string)
			// arrives with fixlen subtype BLOB (0b). The wire subtype
			// contradicts the schema, so the field is skipped exactly as an
			// unknown id is (MESSAGE_SPEC §7.3) — it reaches Visitor.Blob,
			// where the string arm does not live, so nothing is validated and
			// nothing is stored. Proves the check is keyed on actual
			// materialization, not on "the schema declares a string here".
			name: "declared string id arriving as blob is skipped",
			in:   []byte{0x56, 0x12, 0x0b, 0x8a, 0x07},
		},
		{
			// The mirror image: nested.bytes_field (id 3, declared blob, 1a =
			// (3<<3)|2 FIXLEN) arrives as subtype STRING. It reaches
			// Visitor.String at an id with no string arm, so the payload is
			// skipped, never validated.
			name: "declared blob id arriving as string is skipped",
			in:   []byte{0x56, 0x1a, 0x0a, 0x8a, 0x07},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p probeV
			err := acceptBytes(tc.in, &p)

			// A materialized invalid-UTF-8 payload is INVALID only where the
			// validator is compiled in; otherwise it is accepted verbatim.
			if tc.materialized != nil && !utf8CheckCompiled {
				if err != nil {
					t.Fatalf("AcceptBytes(% X) = %v, want nil (validator compiled out)", tc.in, err)
				}
				got := p.materializedStrings()
				if len(got) != 1 || got[0] != string(tc.materialized) {
					t.Fatalf("destination got %q, want [% X] verbatim", got, tc.materialized)
				}
				return
			}
			if tc.want == nil {
				if err != nil {
					t.Fatalf("AcceptBytes(% X) = %v, want nil", tc.in, err)
				}
				if p.nested.strSet || len(p.strArray) != 0 || len(p.structV) != 0 {
					t.Fatalf("skipped payload leaked into a destination: %+v", p)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("AcceptBytes(% X) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

// TestF0038SkipAdvancesExactly proves the skip is still a length jump of exactly
// `length` bytes: the field after the skipped invalid-UTF-8 string decodes
// normally. A fix that early-returns before advancing the cursor desynchronizes
// the stream and this test goes red.
func TestF0038SkipAdvancesExactly(t *testing.T) {
	// 4a 0a 8a = the skipped string; 00 = id 0, unsigned varint; 2a = 42.
	in := []byte{0x4a, 0x0a, 0x8a, 0x00, 0x2a}
	var p probeV
	if err := acceptBytes(in, &p); err != nil {
		t.Fatalf("AcceptBytes = %v, want nil", err)
	}
	if !p.u8Set || p.u8 != 42 {
		t.Fatalf("u8 after the skip = (%d, set=%v), want (42, true)", p.u8, p.u8Set)
	}
}

// TestF0038MaterializedStillBinds proves the relocated check is not a blanket
// "never validate": a VALID payload at the declared destination still lands, and
// an embedded U+0000 stays valid (§6.4 "the validator MUST NOT reject it") while
// its overlong form C0 80 stays rejected (covered above).
func TestF0038MaterializedStillBinds(t *testing.T) {
	// 56 = id 10 seq; 12 = id 2 FIXLEN; 12 = len 2 STRING; 61 00 = "a\x00"; 07.
	in := []byte{0x56, 0x12, 0x12, 0x61, 0x00, 0x07}
	var p probeV
	if err := acceptBytes(in, &p); err != nil {
		t.Fatalf("AcceptBytes = %v, want nil (embedded NUL is valid UTF-8)", err)
	}
	if !p.nested.strSet || p.nested.str != "a\x00" {
		t.Fatalf("nested.str = %q (set=%v), want %q", p.nested.str, p.nested.strSet, "a\x00")
	}
}

// TestF0038ChunkBoundaryDeterminism covers the §6.4 normative rule that a chunk
// boundary MUST NOT change any verdict: the same isolates fed one byte at a time
// through Decoder.Feed produce the same outcomes as the one-shot AcceptBytes.
// The two materialized rows take their expectation from utf8Invalid(), so the
// rule is asserted in whichever build is running — a chunk boundary must not
// change the verdict in the footprint build either (issue #88).
func TestF0038ChunkBoundaryDeterminism(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"V1 skipped invalid utf8", []byte{0x4a, 0x0a, 0x8a}, nil},
		{"V1 then a following field", []byte{0x4a, 0x0a, 0x8a, 0x00, 0x2a}, nil},
		{"C1 declared nested.str", []byte{0x56, 0x12, 0x0a, 0x8a, 0x07}, utf8Invalid()},
		{"V2 §7.3 mistyped slot", []byte{0xd6, 0x0c, 0x02, 0x12, 0xff, 0xff, 0x07}, nil},
		{"truncated skipped string", []byte{0x4a, 0x12, 0xff}, sofab.ErrIncomplete},
		{"§7.3 declared string id as blob", []byte{0x56, 0x12, 0x0b, 0x8a, 0x07}, nil},
		{"§7.3 declared blob id as string", []byte{0x56, 0x1a, 0x0a, 0x8a, 0x07}, nil},
		{"materialized string_array element", []byte{0xc6, 0x0c, 0x02, 0x0a, 0x8a, 0x07}, utf8Invalid()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p probeV
			err := feedIn(tc.in, 1, &p)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("chunked Feed = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("chunked Feed = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestF0038BoundVsSkipped pins the two halves of §6.4.5 on the one surface
// there is: a string a destination BINDS is validated, and the identical bytes
// at an id nothing binds are a length jump that is never inspected — with the
// field after it still arriving.
func TestF0038BoundVsSkipped(t *testing.T) {
	in := []byte{0x4a, 0x0a, 0x8a, 0x00, 0x2a} // id 9: string 8A, then id 0: unsigned 42

	// Bound: the destination validates.
	checkUTF8Decode(t, "bound string", acceptBytes(in, &bindStrV{id: 9}))

	// Not bound: no validation, and the decode reaches the following field.
	log, err := decodeAll(t, "AcceptBytes", in)
	if err != nil {
		t.Fatalf("unbound decode = %v, want nil (skips are never validated)", err)
	}
	if len(log) == 0 || log[len(log)-1] != evU(0, 42) {
		t.Fatalf("never resynced onto id 0: %v", log)
	}
}

// probeHookV is probeNestedV plus the FixlenBegin arm, i.e. the schema maxlen
// enforced at the LENGTH WORD. It exists to pin the ordering the fix must not
// disturb: for an over-maxlen field that is ALSO truncated, INVALID dominates
// INCOMPLETE (§5.2), so the bound is checked before any payload byte is taken —
// relocating the UTF-8 content check must not drag the maxlen check after it.
type probeHookV struct {
	baseV
}

func (probeHookV) String(id sofab.ID, v string) error {
	switch id {
	case 2:
		if len(v) > 4 {
			return sofab.ErrInvalidMsg
		}
		if !sofab.UTF8Valid([]byte(v)) {
			return sofab.ErrInvalidMsg
		}
	}
	return nil
}

// NB: the 3-arg form (F-0042). The test-side aggHeader is an OPTIONAL
// interface, so a
// stale signature does not fail to compile -- it silently stops satisfying the
// interface, FixlenHeader is never called, and the maxlen reject below vanishes.
func (probeHookV) ArrayBegin(sofab.ID, sofab.ArrayKind, int) error { return nil }

func (probeHookV) FixlenHeader(id sofab.ID, subtype, length int) error {
	if id == 2 && subtype == 2 && length > 4 {
		return sofab.ErrInvalidMsg
	}
	return nil
}

func (v probeHookV) BeginSequence(sofab.ID) (any, error) { return v, nil }

func TestF0038MaxlenStillDominatesIncomplete(t *testing.T) {
	// id 2, FIXLEN, fixlen_word (6<<3)|2 = 0x32 -> length 6 (over the bound of
	// 4) with only 2 of the 6 payload bytes present. INVALID, not INCOMPLETE.
	in := []byte{0x12, 0x32, 'a', 'b'}
	if err := acceptBytes(in, probeHookV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("over-maxlen + truncated = %v, want ErrInvalidMsg (§5.2 anti-folding)", err)
	}
	// A skipped field is still framing-checked but its content is not: the same
	// truncation at an undeclared id is plain INCOMPLETE.
	if err := acceptBytes([]byte{0x4a, 0x32, 'a', 'b'}, probeHookV{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("truncated skipped string = %v, want ErrIncomplete", err)
	}
}
