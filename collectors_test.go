package sofab_test

// Unit tests for the collector layer (collectors.go), which CORELIB_PLAN §7
// makes mandatory. They are worth more than the shared vectors here: a
// collector is not wire-visible, so two implementations can disagree about a
// gap-filled row, about a reopened element, or about how much an announced
// index may allocate, and still produce byte-identical output. Nothing in the
// vector suite can tell them apart.
//
// Each collector is therefore driven twice where it matters: over real wire
// bytes through AcceptBytes, which is how generated code reaches it, and
// directly, which is the only way to reach the boundaries a schema cannot
// express today (an element id at 2^30, a row past its capacity).

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// tagHolder stands in for a message the generator emits for a schema with one
// string-array field declared `count: 4, maxlen: 8`.
type tagHolder struct {
	sofab.VisitorBase
	Tags []string
}

// BeginSequence is the whole of the generated arm for that field: hand back a
// collector bound to the member, carrying the schema's bounds.
func (m *tagHolder) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	if id == 1 {
		m.Tags = m.Tags[:0]
		return &sofab.StringSeq{Out: &m.Tags, Cap: 4, ElemMax: 8}, nil
	}
	return sofab.VisitorBase{}, nil
}

func ExampleStringSeq() {
	// The wire image of ["alpha", "", "gamma"]: the interior element equals the
	// element default and is omitted (MESSAGE_SPEC §2), so the collector has to
	// restore it as a gap rather than shift "gamma" down to index 1.
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteSequenceBeginLazy(1)
	e.WriteString(0, "alpha")
	e.WriteString(2, "gamma")
	e.WriteSequenceEndKeep()
	if err := e.Flush(); err != nil {
		fmt.Println(err)
		return
	}

	var m tagHolder
	if err := sofab.AcceptBytes(buf.Bytes(), &m); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("%d %q\n", len(m.Tags), m.Tags)

	// Output: 3 ["alpha" "" "gamma"]
}

// --- harness -----------------------------------------------------------------

// seqRoot binds the one top-level sequence field of a test message to a
// collector, standing in for the generated BeginSequence arm that does exactly
// this.
type seqRoot struct {
	sofab.VisitorBase
	child sofab.Visitor
}

func (r *seqRoot) BeginSequence(sofab.ID) (sofab.Visitor, error) { return r.child, nil }

// wrapperSeq encodes one wrapper-sequence array field (id 1) whose elements
// write writes. The frame is kept even when empty: element presence is what
// carries the array's length (§5.1).
func wrapperSeq(t testing.TB, write func(*sofab.Encoder)) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteSequenceBeginLazy(1); err != nil {
		t.Fatalf("WriteSequenceBeginLazy: %v", err)
	}
	write(e)
	if err := e.WriteSequenceEndKeep(); err != nil {
		t.Fatalf("WriteSequenceEndKeep: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

// collect decodes raw into the collector, returning the decode's outcome.
func collect(raw []byte, child sofab.Visitor, opts ...sofab.Option) error {
	return sofab.AcceptBytes(raw, &seqRoot{child: child}, opts...)
}

// mustCollect decodes raw into the collector and fails on anything but a clean
// decode.
func mustCollect(t *testing.T, raw []byte, child sofab.Visitor) {
	t.Helper()
	if err := collect(raw, child); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// openSeq is the header of the test message's array field, for the hand-crafted
// wire bytes below (an element header alone, with no terminating end marker, is
// how a truncated element is expressed).
func openSeq() []byte { return vhdr(1, sofab.TypeSequenceStart) }

// fixlenElem is one fixlen element header: id, subtype and announced length,
// with no payload — the truncation point §5.2's anti-folding rule is about.
func fixlenElem(id sofab.ID, subtype, length uint64) []byte {
	return append(vhdr(id, sofab.TypeFixlen), vbytes((length<<3)|subtype)...)
}

// --- VisitorBase -------------------------------------------------------------

// A destination that binds nothing still decodes: every callback is a no-op and
// an unbound nested scope is decoded into another VisitorBase rather than a nil
// visitor, which would panic the decoder on the scope's first field.
func TestVisitorBaseAcceptsEveryEvent(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(1, 7)
	e.WriteSigned(2, -7)
	e.WriteFloat32(3, 1.5)
	e.WriteFloat64(4, 2.5)
	e.WriteString(5, "hi")
	e.WriteBytes(6, []byte{1, 2})
	sofab.WriteUnsignedArray(e, 7, []uint32{1, 2})
	sofab.WriteSignedArray(e, 8, []int32{-1, 2})
	e.WriteFloat32Array(9, []float32{1, 2})
	e.WriteFloat64Array(10, []float64{1, 2})
	e.WriteSequenceBeginLazy(11)
	e.WriteUnsigned(0, 1)
	e.WriteSequenceEnd()
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if err := sofab.AcceptBytes(buf.Bytes(), sofab.VisitorBase{}); err != nil {
		t.Fatalf("decode into VisitorBase: %v", err)
	}

	v, err := sofab.VisitorBase{}.BeginSequence(1)
	if err != nil || v == nil {
		t.Fatalf("BeginSequence = (%v, %v), want a non-nil visitor and no error", v, err)
	}
}

// --- StringSeq / BlobSeq -----------------------------------------------------

// The placement contract, on the wire: an interior element equal to the element
// default is omitted (§2), so it must decode as a GAP filled with the element
// default and not shift the elements after it down. The last element is always
// written, so the decoded length is exact.
func TestStringSeqFillsGapsAndKeepsLength(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteString(0, "a")
		e.WriteString(3, "d")
	})

	var out []string
	mustCollect(t, raw, &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1})

	want := []string{"a", "", "", "d"}
	if len(out) != len(want) {
		t.Fatalf("length %d, want %d (%q)", len(out), len(want), out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("element %d = %q, want %q", i, out[i], want[i])
		}
	}
}

// A reopened id is the same element seen twice, not a second one: the last
// occurrence wins (§7.4). Appending would grow the array instead.
func TestStringSeqReopenedIDReplaces(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteString(0, "first")
		e.WriteString(1, "b")
		e.WriteString(0, "second")
	})

	var out []string
	mustCollect(t, raw, &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1})

	if len(out) != 2 || out[0] != "second" || out[1] != "b" {
		t.Fatalf("out = %q, want [second b]", out)
	}
}

// The capacity edge, both sides of it. Cap is the schema count N: the highest
// legal index is N-1, and N itself is a schema-bound violation (§7.1) — not a
// grow.
func TestStringSeqCapacityEdges(t *testing.T) {
	t.Run("last legal index", func(t *testing.T) {
		var out []string
		s := &sofab.StringSeq{Out: &out, Cap: 2, ElemMax: -1}
		if err := s.String(1, "x"); err != nil {
			t.Fatalf("id cap-1: %v", err)
		}
		if len(out) != 2 || out[0] != "" || out[1] != "x" {
			t.Fatalf("out = %q, want [ x]", out)
		}
	})

	t.Run("first illegal index", func(t *testing.T) {
		var out []string
		s := &sofab.StringSeq{Out: &out, Cap: 2, ElemMax: -1}
		if err := s.String(2, "x"); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("id cap: %v, want ErrInvalidMsg", err)
		}
		if len(out) != 0 {
			t.Fatalf("out grew to %d elements on a rejected id", len(out))
		}
	})

	t.Run("unbounded accepts any index", func(t *testing.T) {
		var out []string
		s := &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1}
		if err := s.String(4, "x"); err != nil {
			t.Fatalf("unbounded: %v", err)
		}
		if len(out) != 5 {
			t.Fatalf("length %d, want 5", len(out))
		}
	})
}

// The adversarial case the capacity check exists for: an element id near 2^31
// is refused BEFORE the fill, so it costs a comparison rather than the
// allocation an id-keyed fill would otherwise amplify it into.
func TestStringSeqOverIndexAllocatesNothing(t *testing.T) {
	var out []string
	s := &sofab.StringSeq{Out: &out, Cap: 4, ElemMax: -1}

	allocs := testing.AllocsPerRun(100, func() {
		if err := s.String(1<<30, "x"); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("id 2^30: %v, want ErrInvalidMsg", err)
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations rejecting an over-range id, want 0", allocs)
	}
	if len(out) != 0 {
		t.Fatalf("out grew to %d elements", len(out))
	}
}

// Both bounds are judged at the element's LENGTH WORD, so a message truncated
// right after the word carrying the violating number is INVALID and not
// INCOMPLETE (§5.2, anti-folding: no further byte can make an illegal length or
// index legal). The same bytes under no bound are INCOMPLETE, which is what
// makes this a property of the collector and not of the truncation.
func TestStringSeqBoundsDecideAtTheHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  *sofab.StringSeq
		in   []byte
		want error
	}{
		{
			name: "over maxlen, truncated",
			seq:  &sofab.StringSeq{Cap: -1, ElemMax: 4},
			in:   append(openSeq(), fixlenElem(0, subStr, 6)...),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "at maxlen, truncated",
			seq:  &sofab.StringSeq{Cap: -1, ElemMax: 4},
			in:   append(openSeq(), fixlenElem(0, subStr, 4)...),
			want: sofab.ErrIncomplete,
		},
		{
			name: "over capacity, truncated",
			seq:  &sofab.StringSeq{Cap: 2, ElemMax: -1},
			in:   append(openSeq(), fixlenElem(2, subStr, 4)...),
			want: sofab.ErrInvalidMsg, // the index is judged at the header too
		},
		{
			name: "no bound, truncated",
			seq:  &sofab.StringSeq{Cap: -1, ElemMax: -1},
			in:   append(openSeq(), fixlenElem(0, subStr, 6)...),
			want: sofab.ErrIncomplete,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out []string
			tc.seq.Out = &out
			if err := collect(tc.in, tc.seq); !errors.Is(err, tc.want) {
				t.Fatalf("decode = %v, want %v", err, tc.want)
			}
		})
	}
}

// An element whose fixlen subtype contradicts the declared element type was
// never this array's value (§7.3): it is skipped, and neither its id nor its
// length may be measured against this array's bounds. A blob element inside a
// string array must therefore not be rejected for exceeding the string maxlen.
func TestStringSeqIgnoresAForeignSubtype(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteBytes(0, bytes.Repeat([]byte{'x'}, 16))
	})

	var out []string
	if err := collect(raw, &sofab.StringSeq{Out: &out, Cap: 1, ElemMax: 4}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %q, want the skipped element to leave it empty", out)
	}
}

// The blob twin of the placement test, plus the ownership rule that separates
// the two: a blob callback may hand over a slice of the decode buffer, so the
// collector must copy. Scribbling over the input afterwards proves it did.
func TestBlobSeqCopiesAndFillsGaps(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteBytes(0, []byte{1, 2})
		e.WriteBytes(2, []byte{3, 4})
	})

	var out [][]byte
	mustCollect(t, raw, &sofab.BlobSeq{Out: &out, Cap: -1, ElemMax: -1})

	if len(out) != 3 {
		t.Fatalf("length %d, want 3", len(out))
	}
	if !bytes.Equal(out[0], []byte{1, 2}) || out[1] != nil || !bytes.Equal(out[2], []byte{3, 4}) {
		t.Fatalf("out = %v, want [[1 2] [] [3 4]]", out)
	}

	for i := range raw {
		raw[i] = 0xEE
	}
	if !bytes.Equal(out[0], []byte{1, 2}) || !bytes.Equal(out[2], []byte{3, 4}) {
		t.Fatalf("out = %v after overwriting the input: the element aliases the decode buffer", out)
	}
}

func TestBlobSeqBounds(t *testing.T) {
	t.Run("over maxlen", func(t *testing.T) {
		var out [][]byte
		s := &sofab.BlobSeq{Out: &out, Cap: -1, ElemMax: 2}
		if err := s.Bytes(0, []byte{1, 2, 3}); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("over maxlen: %v, want ErrInvalidMsg", err)
		}
		if len(out) != 0 {
			t.Fatalf("out grew to %d elements on a rejected element", len(out))
		}
	})

	t.Run("over maxlen at the header, truncated", func(t *testing.T) {
		var out [][]byte
		in := append(openSeq(), fixlenElem(0, subBlob, 6)...)
		if err := collect(in, &sofab.BlobSeq{Out: &out, Cap: -1, ElemMax: 4}); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("decode = %v, want ErrInvalidMsg", err)
		}
	})

	t.Run("over capacity", func(t *testing.T) {
		var out [][]byte
		s := &sofab.BlobSeq{Out: &out, Cap: 2, ElemMax: -1}
		if err := s.Bytes(2, []byte{1}); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("id cap: %v, want ErrInvalidMsg", err)
		}
	})

	t.Run("foreign subtype is not measured", func(t *testing.T) {
		var out [][]byte
		raw := wrapperSeq(t, func(e *sofab.Encoder) { e.WriteString(0, "abcdefgh") })
		if err := collect(raw, &sofab.BlobSeq{Out: &out, Cap: 1, ElemMax: 4}); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("out = %v, want the skipped element to leave it empty", out)
		}
	})
}

// --- MessageSeq --------------------------------------------------------------

// elemMsg stands in for a generated struct element: a Visitor that binds its own
// fields, reached by MessageSeq through the type parameter and never by name.
type elemMsg struct {
	sofab.VisitorBase
	N     uint64
	S     string
	ended int
}

func (m *elemMsg) Unsigned(_ sofab.ID, v uint64) error { m.N = v; return nil }
func (m *elemMsg) String(_ sofab.ID, v string) error   { m.S = v; return nil }
func (m *elemMsg) EndSequence() error                  { m.ended++; return nil }

func TestMessageSeqPlacesElementsByID(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(0)
		e.WriteUnsigned(1, 7)
		e.WriteSequenceEndKeep()
		e.WriteSequenceBeginLazy(2)
		e.WriteUnsigned(1, 9)
		e.WriteSequenceEndKeep()
	})

	var out []elemMsg
	mustCollect(t, raw, &sofab.MessageSeq[elemMsg, *elemMsg]{Out: &out, Cap: -1})

	if len(out) != 3 {
		t.Fatalf("length %d, want 3", len(out))
	}
	if out[0].N != 7 || out[2].N != 9 {
		t.Fatalf("out = %+v, want elements 0 and 2 bound", out)
	}
	if out[1].N != 0 || out[1].ended != 0 {
		t.Fatalf("gap element = %+v, want an untouched zero element", out[1])
	}
}

// A reopened element id continues the element already there (§7.4) — decoding
// in place is what makes that fall out; appending would make it a second one.
func TestMessageSeqReopenedIDContinuesTheSameElement(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(0)
		e.WriteUnsigned(1, 7)
		e.WriteSequenceEndKeep()
		e.WriteSequenceBeginLazy(0)
		e.WriteString(2, "late")
		e.WriteSequenceEndKeep()
	})

	var out []elemMsg
	mustCollect(t, raw, &sofab.MessageSeq[elemMsg, *elemMsg]{Out: &out, Cap: -1})

	if len(out) != 1 {
		t.Fatalf("length %d, want 1", len(out))
	}
	if out[0].N != 7 || out[0].S != "late" {
		t.Fatalf("out[0] = %+v, want both occurrences merged into one element", out[0])
	}
}

func TestMessageSeqCapacityEdges(t *testing.T) {
	var out []elemMsg
	s := &sofab.MessageSeq[elemMsg, *elemMsg]{Out: &out, Cap: 2}

	if _, err := s.BeginSequence(1); err != nil {
		t.Fatalf("id cap-1: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("length %d, want 2", len(out))
	}

	v, err := s.BeginSequence(2)
	if !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("id cap: %v, want ErrInvalidMsg", err)
	}
	if v != nil {
		t.Errorf("rejected id returned visitor %v, want nil", v)
	}
	if len(out) != 2 {
		t.Fatalf("out grew to %d elements on a rejected id", len(out))
	}
}

// --- NestedSeq ---------------------------------------------------------------

// An array of string arrays: the outer collector reserves the row and hands the
// inner one the address of that row's slot, so the inner array's own bounds
// travel with the inner collector.
func TestNestedSeqCollectsRowsByID(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(0)
		e.WriteString(0, "a")
		e.WriteSequenceEndKeep()
		e.WriteSequenceBeginLazy(2)
		e.WriteString(0, "b")
		e.WriteString(1, "c")
		e.WriteSequenceEndKeep()
	})

	var out [][]string
	mustCollect(t, raw, &sofab.NestedSeq[string]{
		Out: &out, Cap: -1,
		Make: func(p *[]string) sofab.Visitor {
			return &sofab.StringSeq{Out: p, Cap: -1, ElemMax: -1}
		},
	})

	if len(out) != 3 {
		t.Fatalf("length %d, want 3", len(out))
	}
	if len(out[0]) != 1 || out[0][0] != "a" || out[1] != nil || len(out[2]) != 2 || out[2][1] != "c" {
		t.Fatalf("out = %q, want [[a] [] [b c]]", out)
	}
}

// The inner collector carries the INNER array's bounds, so a breach inside a row
// fails the decode even though the outer array is within its own capacity.
func TestNestedSeqInnerBoundApplies(t *testing.T) {
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(0)
		e.WriteString(0, "a")
		e.WriteString(1, "b")
		e.WriteSequenceEndKeep()
	})

	var out [][]string
	err := collect(raw, &sofab.NestedSeq[string]{
		Out: &out, Cap: 4,
		Make: func(p *[]string) sofab.Visitor {
			return &sofab.StringSeq{Out: p, Cap: 1, ElemMax: -1}
		},
	})
	if !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("decode = %v, want ErrInvalidMsg", err)
	}
}

func TestNestedSeqCapacityEdges(t *testing.T) {
	var out [][]string
	made := 0
	s := &sofab.NestedSeq[string]{
		Out: &out, Cap: 2,
		Make: func(p *[]string) sofab.Visitor {
			made++
			return &sofab.StringSeq{Out: p, Cap: -1, ElemMax: -1}
		},
	}

	if _, err := s.BeginSequence(1); err != nil {
		t.Fatalf("id cap-1: %v", err)
	}
	if _, err := s.BeginSequence(2); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("id cap: %v, want ErrInvalidMsg", err)
	}
	if made != 1 {
		t.Errorf("Make called %d times, want 1: a rejected row must not build a collector", made)
	}
	if len(out) != 2 {
		t.Fatalf("length %d, want 2", len(out))
	}
}

// --- PlaceRow ----------------------------------------------------------------

func TestPlaceRowGrowsGapsAndReplaces(t *testing.T) {
	var out [][]uint32

	if err := sofab.PlaceRow(&out, -1, 0, []uint32{1}); err != nil {
		t.Fatalf("first row: %v", err)
	}
	if err := sofab.PlaceRow(&out, -1, 2, []uint32{3}); err != nil {
		t.Fatalf("row after a gap: %v", err)
	}
	if len(out) != 3 || out[1] != nil {
		t.Fatalf("out = %v, want an empty row in the gap", out)
	}

	if err := sofab.PlaceRow(&out, -1, 0, []uint32{9}); err != nil {
		t.Fatalf("reopened row: %v", err)
	}
	if len(out) != 3 || out[0][0] != 9 {
		t.Fatalf("out = %v, want the reopened row replaced in place", out)
	}
}

func TestPlaceRowCapacityEdges(t *testing.T) {
	var out [][]uint32

	if err := sofab.PlaceRow(&out, 2, 1, []uint32{1}); err != nil {
		t.Fatalf("id cap-1: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("length %d, want 2", len(out))
	}

	if err := sofab.PlaceRow(&out, 2, 2, []uint32{1}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("id cap: %v, want ErrInvalidMsg", err)
	}
	if len(out) != 2 {
		t.Fatalf("out grew to %d rows on a rejected id", len(out))
	}

	allocs := testing.AllocsPerRun(100, func() {
		if err := sofab.PlaceRow(&out, 2, 1<<30, nil); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("id 2^30: %v, want ErrInvalidMsg", err)
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations rejecting an over-range row id, want 0", allocs)
	}
}

// --- matrix collectors -------------------------------------------------------

// matrixBytes encodes a matrix field: one native array per row, keyed by row id.
func matrixBytes(t testing.TB, write func(*sofab.Encoder)) []byte {
	return wrapperSeq(t, write)
}

func TestUnsignedMatrixSeqNarrowsAndPlaces(t *testing.T) {
	raw := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{1, 2})
		sofab.WriteUnsignedArray(e, 2, []uint32{3})
	})

	var out [][]uint16
	mustCollect(t, raw, &sofab.UnsignedMatrixSeq[uint16]{Out: &out, Cap: -1, Hi: math.MaxUint16})

	if len(out) != 3 || len(out[0]) != 2 || out[0][1] != 2 || out[1] != nil || out[2][0] != 3 {
		t.Fatalf("out = %v, want [[1 2] [] [3]]", out)
	}
}

// The declared element width is checked BEFORE the narrowing conversion, which
// only masks: an element above the width would otherwise be stored as a
// different value than the wire carried (§7.1).
func TestUnsignedMatrixSeqRejectsAnOverWideElement(t *testing.T) {
	raw := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{1, 256})
	})

	var out [][]uint8
	err := collect(raw, &sofab.UnsignedMatrixSeq[uint8]{Out: &out, Cap: -1, Hi: math.MaxUint8})
	if !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("decode = %v, want ErrInvalidMsg", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %v, want nothing placed", out)
	}
}

// Hi == 0 is "the declared width spans everything this callback can deliver", so
// the scan is off and even the largest u64 goes through.
func TestUnsignedMatrixSeqZeroBoundSkipsTheScan(t *testing.T) {
	var out [][]uint64
	s := &sofab.UnsignedMatrixSeq[uint64]{Out: &out, Cap: -1}
	if err := s.UnsignedArray(0, []uint64{math.MaxUint64}); err != nil {
		t.Fatalf("u64 row: %v", err)
	}
	if len(out) != 1 || out[0][0] != math.MaxUint64 {
		t.Fatalf("out = %v, want the value through unchanged", out)
	}
}

func TestSignedMatrixSeqNarrowsAndBounds(t *testing.T) {
	raw := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteSignedArray(e, 0, []int32{-128, 127})
	})

	var out [][]int8
	mustCollect(t, raw, &sofab.SignedMatrixSeq[int8]{Out: &out, Cap: -1, Lo: math.MinInt8, Hi: math.MaxInt8})
	if len(out) != 1 || out[0][0] != -128 || out[0][1] != 127 {
		t.Fatalf("out = %v, want [[-128 127]]", out)
	}

	for _, x := range []int32{-129, 128} {
		raw := matrixBytes(t, func(e *sofab.Encoder) {
			sofab.WriteSignedArray(e, 0, []int32{x})
		})
		var out [][]int8
		err := collect(raw, &sofab.SignedMatrixSeq[int8]{Out: &out, Cap: -1, Lo: math.MinInt8, Hi: math.MaxInt8})
		if !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("element %d: %v, want ErrInvalidMsg", x, err)
		}
	}
}

func TestSignedMatrixSeqZeroBoundSkipsTheScan(t *testing.T) {
	var out [][]int64
	s := &sofab.SignedMatrixSeq[int64]{Out: &out, Cap: -1}
	if err := s.SignedArray(0, []int64{math.MinInt64}); err != nil {
		t.Fatalf("i64 row: %v", err)
	}
	if len(out) != 1 || out[0][0] != math.MinInt64 {
		t.Fatalf("out = %v, want the value through unchanged", out)
	}
}

func TestFloatMatrixSeqPlacesRows(t *testing.T) {
	t.Run("fp32", func(t *testing.T) {
		raw := matrixBytes(t, func(e *sofab.Encoder) {
			e.WriteFloat32Array(0, []float32{1.5})
			e.WriteFloat32Array(2, []float32{2.5, 3.5})
		})
		var out [][]float32
		mustCollect(t, raw, &sofab.Float32MatrixSeq{Out: &out, Cap: -1})
		if len(out) != 3 || out[0][0] != 1.5 || out[1] != nil || out[2][1] != 3.5 {
			t.Fatalf("out = %v, want [[1.5] [] [2.5 3.5]]", out)
		}
	})

	t.Run("fp64", func(t *testing.T) {
		raw := matrixBytes(t, func(e *sofab.Encoder) {
			e.WriteFloat64Array(1, []float64{2.5})
		})
		var out [][]float64
		mustCollect(t, raw, &sofab.Float64MatrixSeq{Out: &out, Cap: -1})
		if len(out) != 2 || out[0] != nil || out[1][0] != 2.5 {
			t.Fatalf("out = %v, want [[] [2.5]]", out)
		}
	})

	t.Run("capacity", func(t *testing.T) {
		var out [][]float32
		s := &sofab.Float32MatrixSeq{Out: &out, Cap: 1}
		if err := s.Float32Array(1, []float32{1}); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("row id cap: %v, want ErrInvalidMsg", err)
		}
	})
}

// Bools travel as an unsigned array (§4.6): every nonzero element is true, and
// no unsigned value is out of range for a bool.
func TestBoolMatrixSeqMapsRows(t *testing.T) {
	raw := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint64{0, 1, 2, math.MaxUint64})
	})

	var out [][]bool
	mustCollect(t, raw, &sofab.BoolMatrixSeq{Out: &out, Cap: -1})

	want := []bool{false, true, true, true}
	if len(out) != 1 || len(out[0]) != len(want) {
		t.Fatalf("out = %v, want one row of %d", out, len(want))
	}
	for i, w := range want {
		if out[0][i] != w {
			t.Errorf("element %d = %v, want %v", i, out[0][i], w)
		}
	}
}

func TestBoolMatrixSeqCapacity(t *testing.T) {
	var out [][]bool
	s := &sofab.BoolMatrixSeq{Out: &out, Cap: 2}
	if err := s.UnsignedArray(2, []uint64{1}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("row id cap: %v, want ErrInvalidMsg", err)
	}
	if len(out) != 0 {
		t.Fatalf("out grew to %d rows on a rejected id", len(out))
	}
}

// TestSequenceGrowthIsGeometric is the one assertion in CORELIB_PLAN §7.2 item 8
// that needs the language's own allocation-counting facility: growth geometry.
//
// "Extending to at least `id + 1` rather than exactly `id + 1`, so a sparse
// array does not cost O(n²) copies — is the one property here needing the
// language's own allocation-counting facility. Test it where the language offers
// one." Go does, so it is tested rather than stated.
//
// The measurement is the allocation COUNT for placing n elements at ids 0..n-1.
// Exact-length growth would reallocate once per element (n allocations); Go's
// append doubles, so the count is O(log n) — and the test asserts the shape by
// comparing two sizes an order of magnitude apart: ten times the elements must
// not cost ten times the allocations.
//
// This is the ONLY place in this library where a container grows from a wire
// value, and it is deliberately in the static helper layer (§6.6.1) rather than
// in the codec: a wrapper array carries no count header, so its length is
// "highest present id + 1" (MESSAGE_SPEC §5.1) and is not known until the array
// ends. Everything with a count or length on the wire ahead of its payload
// checks that word and allocates exactly it, once.
func TestSequenceGrowthIsGeometric(t *testing.T) {
	place := func(n int) float64 {
		return testing.AllocsPerRun(20, func() {
			out := make([]string, 0)
			s := &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1}
			for i := 0; i < n; i++ {
				if err := s.String(sofab.ID(i), "x"); err != nil {
					t.Fatalf("place %d: %v", i, err)
				}
			}
			if len(out) != n {
				t.Fatalf("length %d, want %d", len(out), n)
			}
		})
	}

	small, big := place(64), place(640)
	if big >= small*10 {
		t.Errorf("640 elements cost %.0f allocations against %.0f for 64: growth is "+
			"linear in the element count, i.e. one reallocation per element", big, small)
	}
	// A sanity floor: geometric growth over 640 elements is a handful of
	// reallocations, not hundreds.
	if big > 32 {
		t.Errorf("640 elements cost %.0f allocations; geometric growth is O(log n)", big)
	}

	// The gap-filling half of the same property: placing ONE element at a high id
	// extends the container to id+1 in geometric steps, not one step per slot.
	sparse := testing.AllocsPerRun(20, func() {
		out := make([]string, 0)
		s := &sofab.StringSeq{Out: &out, Cap: -1, ElemMax: -1}
		if err := s.String(1000, "x"); err != nil {
			t.Fatalf("sparse place: %v", err)
		}
		if len(out) != 1001 {
			t.Fatalf("length %d, want 1001", len(out))
		}
	})
	if sparse > 32 {
		t.Errorf("one element at id 1000 cost %.0f allocations; the gap fill is not geometric", sparse)
	}
}
