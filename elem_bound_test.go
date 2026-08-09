package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// An integer array's DECLARED ELEMENT WIDTH is a validity bound (MESSAGE_SPEC
// §7.1), and §5.2 makes INVALID dominate INCOMPLETE: an element already outside
// that width is on the wire, so truncating the array behind it cannot downgrade
// the verdict. The array callbacks hand over the whole slice, so the visitor's
// own guard only runs for an array that arrives — which is why the bound has to
// travel INTO the decoder, as ElemBoundVisitor (generator#267, Crucible F-0043).

// widthVisitor is a generated-style visitor declaring `array<i8, count 5>` at id
// 1 and `array<u8, count 5>` at id 0 — the two narrowed kinds — and nothing at
// id 2, which stands in for a u64/i64 array whose range is the callback's own.
type widthVisitor struct{ baseV }

// Satisfied structurally, so a stale signature would silently opt the visitor
// out of the bound instead of failing to compile. Pin it.
var _ sofab.ElemBoundVisitor = widthVisitor{}

func (widthVisitor) ArrayElemBound(id sofab.ID, kind sofab.ArrayKind) (int64, int64, bool) {
	switch id {
	case 0:
		if kind == sofab.ArrayUnsigned {
			return 0, 255, true
		}
	case 1:
		if kind == sofab.ArraySigned {
			return -128, 127, true
		}
	}
	return 0, 0, false
}

// uHdr/sHdr are the field headers for an unsigned array at id 0 and a signed
// array at id 1.
const (
	uHdr = (0 << 3) | 3
	sHdr = (1 << 3) | 4
)

// TestElemBoundTruncatedArray is the whole point of the extension: the element
// that breaches the width is fully on the wire and the array behind it is not.
// Every case pairs with a control one step away, so this pins an ORDERING, not a
// blanket reject.
func TestElemBoundTruncatedArray(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{
			// Crucible width_elem_trunc: count 5, one element = zigzag(5208), end.
			// Was ErrIncomplete — the count exceeded the bytes left, so the scan
			// bailed without looking at the 5208.
			name: "signed over-width then truncated",
			in:   []byte{sHdr, 5, 0xb0, 0x51},
			want: sofab.ErrInvalidMsg,
		},
		{
			// ctl_width_elem_inrange_trunc: same cut, element 1. Nothing is
			// decided yet, so the truncation IS the verdict.
			name: "signed in-range then truncated",
			in:   []byte{sHdr, 5, 0x02},
			want: sofab.ErrIncomplete,
		},
		{
			name: "unsigned over-width then truncated",
			in:   []byte{uHdr, 5, 0x80, 0x04}, // 512 > 255
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "unsigned in-range then truncated",
			in:   []byte{uHdr, 5, 0xff, 0x01}, // 255 == bound
			want: sofab.ErrIncomplete,
		},
		{
			// The array COMPLETES, so the visitor's own guard would decide it.
			// The decoder must not reject on its own here — id 0 has no arm in
			// baseV, so a reject could only come from the bound.
			name: "over-width but complete is left to the visitor",
			in:   []byte{uHdr, 1, 0x80, 0x04},
			want: nil,
		},
		{
			// A wire kind that contradicts the declaration is skipped whole
			// (§7.3), so this field's width must not be measured against it: id 1
			// declares SIGNED, an unsigned array arrives, and ArrayElemBound
			// answers false for the kind. 5208 would breach if it were applied.
			name: "contradicting wire kind is not measured",
			in:   []byte{(1 << 3) | 3, 5, 0xb0, 0x51},
			want: sofab.ErrIncomplete,
		},
		{
			// An id the visitor declares no width for keeps today's outcome.
			name: "unbounded id stays truncated",
			in:   []byte{(2 << 3) | 4, 5, 0xb0, 0x51},
			want: sofab.ErrIncomplete,
		},
		{
			// The FORMAT bound still outranks it and still fires first: an
			// overlong varint is INVALID with or without a declared width (#66).
			name: "overlong element is still invalid",
			in:   []byte{sHdr, 5, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80},
			want: sofab.ErrInvalidMsg,
		},
		{
			// The count fits in the bytes remaining, so this does NOT take the
			// scan-the-tail branch — it goes through the element fill, where the
			// truncation is found only after the offending element is decoded.
			// Both branches have to apply the bound; the conformance suites cut
			// exactly here (someuintarray, count 4, one 5-byte element).
			name: "count fits the bytes left but the elements do not",
			in:   []byte{uHdr, 4, 0x80, 0x80, 0x80, 0x80, 0x10}, // 2^32 > 255
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "count fits the bytes left, element in range",
			in:   []byte{uHdr, 4, 0x80, 0x80, 0x80, 0x80, 0x00}, // 0, in range
			want: sofab.ErrIncomplete,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// AcceptBytes: the cursor path, where the bound rides in
			// scanTruncatedArray.
			if err := sofab.AcceptBytes(c.in, widthVisitor{}); !errors.Is(err, c.want) {
				t.Errorf("AcceptBytes: got %v, want %v", err, c.want)
			}
			// Accept: slurp, then the same cursor.
			if err := sofab.NewDecoder(bytes.NewReader(c.in)).Accept(widthVisitor{}); !errors.Is(err, c.want) {
				t.Errorf("Accept: got %v, want %v", err, c.want)
			}
			// AcceptStream: the reader-driven twin, which reaches the same
			// verdict through the elements it managed to decode. The two paths
			// agreeing is the contract AcceptStream is documented on.
			if err := sofab.NewDecoder(bytes.NewReader(c.in)).AcceptStream(widthVisitor{}); !errors.Is(err, c.want) {
				t.Errorf("AcceptStream: got %v, want %v", err, c.want)
			}
		})
	}
}

// TestElemBoundNested pins that the extension is resolved per SCOPE, like the
// header hooks: the bound belongs to the visitor that owns the field, not to the
// top-level one. Crucible's vector carries the array inside a sequence.
func TestElemBoundNested(t *testing.T) {
	// sequence start at id 100 → (100<<3)|6, then the signed array, then EOF.
	in := []byte{0xa6, 0x06, sHdr, 5, 0xb0, 0x51}
	if err := sofab.AcceptBytes(in, nestedWidthVisitor{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Errorf("AcceptBytes: got %v, want ErrInvalidMsg", err)
	}
}

// nestedWidthVisitor declares nothing itself and hands the nested scope to the
// visitor that does — the shape generated code produces for a struct field.
type nestedWidthVisitor struct{ baseV }

func (nestedWidthVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) {
	return widthVisitor{}, nil
}

// TestElemBoundBackwardCompat pins the additive contract, as
// TestHeaderHookBackwardCompat does for HeaderVisitor: a visitor that does not
// implement ElemBoundVisitor decodes exactly as before. The vector that is
// INVALID above stays INCOMPLETE here.
func TestElemBoundBackwardCompat(t *testing.T) {
	in := []byte{sHdr, 5, 0xb0, 0x51}

	if err := sofab.AcceptBytes(in, plainVisitor{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("AcceptBytes: got %v, want ErrIncomplete", err)
	}
	if err := sofab.NewDecoder(bytes.NewReader(in)).AcceptStream(plainVisitor{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("AcceptStream: got %v, want ErrIncomplete", err)
	}
}

// TestElemBoundIsAskedOnlyWhereItCanMatter pins the cost claim in
// ElemBoundVisitor's doc: the bound is resolved at most once per array FIELD —
// never per element, however long the array — and only where the array fails to
// complete, which is the only place it can change an outcome. An array that
// arrives whole reaches the visitor's own guard, and that guard sees every
// element and reaches the same verdict, so nothing is asked for it.
//
// Both visitor surfaces are held to the same count on the same bytes: the
// reader-driven one always resolved it this late, and the cursor now agrees.
func TestElemBoundIsAskedOnlyWhereItCanMatter(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want int
	}{
		{"complete array", []byte{sHdr, 4, 0x02, 0x04, 0x06, 0x08}, 0},
		// count 5 with one element on the wire: the array never completes.
		{"truncated array", []byte{sHdr, 5, 0x02}, 1},
	} {
		v := &countingBoundVisitor{}
		if err := sofab.AcceptBytes(tc.in, v); (err != nil) != (tc.want != 0) {
			t.Fatalf("AcceptBytes %s: %v", tc.name, err)
		}
		if v.asked != tc.want {
			t.Errorf("AcceptBytes %s: bound asked %d time(s), want %d", tc.name, v.asked, tc.want)
		}

		s := &countingBoundVisitor{}
		if err := sofab.NewDecoder(bytes.NewReader(tc.in)).AcceptStream(s); (err != nil) != (tc.want != 0) {
			t.Fatalf("AcceptStream %s: %v", tc.name, err)
		}
		if s.asked != v.asked {
			t.Errorf("AcceptStream %s: bound asked %d time(s), cursor asked %d", tc.name, s.asked, v.asked)
		}
	}
}

// TestElemBoundNotAskedWithoutTheInterface pins the other half of the cost
// claim: a scope holding no array asks nothing, and a visitor that does not
// implement the extension is never consulted (the assertion is cached, and a
// nil answer costs one compare).
func TestElemBoundNotAskedWithoutTheInterface(t *testing.T) {
	// A scalar-only message: no array field, so no assertion is ever made.
	if err := sofab.AcceptBytes([]byte{(3 << 3) | 0, 0x2a}, plainVisitor{}); err != nil {
		t.Fatalf("scalar message: %v", err)
	}
	// And an array reaching a visitor without the extension decodes as before.
	if err := sofab.AcceptBytes([]byte{sHdr, 2, 0x02, 0x04}, plainVisitor{}); err != nil {
		t.Fatalf("array into a plain visitor: %v", err)
	}
}

type countingBoundVisitor struct {
	baseV
	asked int
}

func (v *countingBoundVisitor) ArrayElemBound(sofab.ID, sofab.ArrayKind) (int64, int64, bool) {
	v.asked++
	return -128, 127, true
}
