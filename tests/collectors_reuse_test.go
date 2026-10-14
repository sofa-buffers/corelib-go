package sofab_test

// Storage reuse: a destination decoded into AGAIN (issue #152).
//
// Generated code truncates a wrapper array's field before refilling it
// (`m.Field = m.Field[:0]`), so the outer slice keeps its backing array. Past
// its new length, that backing array still holds the previous decode's blob
// elements and rows, and the collectors refill each from the one at the SAME
// index instead of allocating. These tests pin the three properties that make
// that safe: the storage is actually reused (no allocation in the steady
// state), it is reused index-for-index only (no two elements of one decode
// share memory), and nothing observable changes — an empty blob is still nil,
// an arrived empty row is still [], a gap is still nil.

import (
	"bytes"
	"errors"
	"math"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func TestReuseSliceKeepsTheBackingArrayWhenItFits(t *testing.T) {
	s := make([]uint32, 3, 8)
	got := sofab.ReuseSlice(s, 8)
	if len(got) != 0 || cap(got) != 8 || &got[:1][0] != &s[0] {
		t.Fatalf("ReuseSlice(cap 8, 8) = len %d cap %d, not s's backing array", len(got), cap(got))
	}

	got = sofab.ReuseSlice(s, 9)
	if len(got) != 0 || cap(got) != 9 || &got[:1][0] == &s[0] {
		t.Fatalf("ReuseSlice(cap 8, 9) = len %d cap %d, want a fresh slice of cap 9", len(got), cap(got))
	}
}

// An array that arrived empty is an empty slice, not nil — what make([]T, 0, 0)
// gave before, and what a serializer renders as [] rather than null.
func TestReuseSliceNeverReturnsNil(t *testing.T) {
	if got := sofab.ReuseSlice[uint8](nil, 0); got == nil {
		t.Fatal("ReuseSlice(nil, 0) = nil, want an empty non-nil slice")
	}
	if got := sofab.ReuseSlice(make([]uint8, 0), 0); got == nil {
		t.Fatal("ReuseSlice(empty, 0) = nil, want an empty non-nil slice")
	}
}

func TestReuseBytes(t *testing.T) {
	dst := make([]byte, 2, 4)
	got := sofab.ReuseBytes(dst, []byte{7, 8, 9})
	if !bytes.Equal(got, []byte{7, 8, 9}) || &got[0] != &dst[0] {
		t.Fatalf("ReuseBytes into room = %v, want [7 8 9] in dst's storage", got)
	}

	got = sofab.ReuseBytes(dst, []byte{1, 2, 3, 4, 5})
	if !bytes.Equal(got, []byte{1, 2, 3, 4, 5}) || &got[0] == &dst[0] || cap(got) > 8 {
		t.Fatalf("ReuseBytes past room = %v (cap %d), want [1 2 3 4 5] in fresh storage", got, cap(got))
	}

	// Grown storage is sized to src, not doubled from cap(dst) as
	// append(dst[:0], src...) would.
	big := make([]byte, 0, 4096)
	if got := sofab.ReuseBytes(big, make([]byte, 4097)); cap(got) >= 2*4096 {
		t.Fatalf("ReuseBytes(cap 4096, 4097 B) has cap %d, want it sized to src (size class), not doubled to 8192", cap(got))
	}

	if got := sofab.ReuseBytes(nil, []byte{1}); !bytes.Equal(got, []byte{1}) {
		t.Fatalf("ReuseBytes(nil, [1]) = %v", got)
	}

	// The nil-versus-empty distinction is observable, so an empty blob stays
	// nil even when there was storage to put it in.
	if got := sofab.ReuseBytes(dst, nil); got != nil {
		t.Fatalf("ReuseBytes(dst, nil) = %#v, want nil", got)
	}
	if got := sofab.ReuseBytes(dst, []byte{}); got != nil {
		t.Fatalf("ReuseBytes(dst, []byte{}) = %#v, want nil", got)
	}
}

// Over real wire bytes, the second decode writes each blob element into the
// storage the first one left at the same index.
func TestBlobSeqReusesElementStorage(t *testing.T) {
	first := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteBytes(0, []byte{1, 2, 3})
		e.WriteBytes(1, []byte{4, 5, 6})
	})
	second := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteBytes(0, []byte{9})
		e.WriteBytes(1, []byte{8, 7})
	})

	var out [][]byte
	mustCollect(t, first, sofab.NewBlobSeq(&out, sofab.Bounds{}, tcaps))
	p0, p1 := &out[0][0], &out[1][0]

	out = out[:0] // the reset generated code emits for the field
	mustCollect(t, second, sofab.NewBlobSeq(&out, sofab.Bounds{}, tcaps))

	if len(out) != 2 || !bytes.Equal(out[0], []byte{9}) || !bytes.Equal(out[1], []byte{8, 7}) {
		t.Fatalf("out = %v, want [[9] [8 7]]", out)
	}
	if &out[0][0] != p0 || &out[1][0] != p1 {
		t.Fatal("the second decode did not reuse the first decode's element storage at the same index")
	}
}

// What the reuse must not change: an empty element and a gap still decode to
// nil, even where the previous decode left storage at that index.
func TestBlobSeqReuseKeepsNilForEmptyAndGap(t *testing.T) {
	first := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteBytes(0, []byte{1})
		e.WriteBytes(1, []byte{2})
		e.WriteBytes(2, []byte{3})
	})
	// id 0 omitted (a gap), id 1 empty, id 2 the last element.
	second := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteBytes(1, []byte{})
		e.WriteBytes(2, []byte{4})
	})

	var out [][]byte
	mustCollect(t, first, sofab.NewBlobSeq(&out, sofab.Bounds{}, tcaps))
	out = out[:0]
	mustCollect(t, second, sofab.NewBlobSeq(&out, sofab.Bounds{}, tcaps))

	if len(out) != 3 || out[0] != nil || out[1] != nil || !bytes.Equal(out[2], []byte{4}) {
		t.Fatalf("out = %#v, want [nil nil [4]]", out)
	}
}

// The steady state the issue measured: once the destination has the room, a
// decode into it allocates nothing for its blob elements.
func TestBlobSeqSteadyStateAllocatesNothing(t *testing.T) {
	var out [][]byte
	s := sofab.NewBlobSeq(&out, sofab.Bounds{Count: 4}, tcaps)
	payloads := [][]byte{{1, 2}, {3}, {4, 5, 6}} // built once: the fed bytes are the caller's
	fill := func() {
		out = out[:0]
		for id, b := range payloads {
			if err := s.Bytes(sofab.ID(id), len(b), 0, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	fill()
	if a := testing.AllocsPerRun(100, fill); a != 0 {
		t.Fatalf("%v allocations per refill, want 0", a)
	}
}

// Reuse is index-for-index. An element whose own previous storage is too small
// gets fresh storage — never a neighbour's, which would make two elements of one
// decode share memory.
func TestBlobSeqReuseNeverBorrowsANeighbour(t *testing.T) {
	var out [][]byte
	s := sofab.NewBlobSeq(&out, sofab.Bounds{}, tcaps)
	put := func(id int, b []byte) {
		t.Helper()
		if err := s.Bytes(sofab.ID(id), len(b), 0, b); err != nil {
			t.Fatal(err)
		}
	}
	put(0, []byte{1})
	put(1, []byte{1, 2, 3, 4, 5, 6, 7, 8})

	out = out[:0]
	put(0, []byte{9, 9, 9, 9})
	put(1, []byte{5})

	if !bytes.Equal(out[0], []byte{9, 9, 9, 9}) || !bytes.Equal(out[1], []byte{5}) {
		t.Fatalf("out = %v, want [[9 9 9 9] [5]]", out)
	}
}

func TestMatrixSeqReusesRowStorage(t *testing.T) {
	first := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{1, 2, 3})
		sofab.WriteUnsignedArray(e, 1, []uint32{4, 5})
	})
	second := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{7})
		sofab.WriteUnsignedArray(e, 1, []uint32{8, 9})
	})

	var out [][]uint16
	newSeq := func() sofab.Visitor {
		return sofab.NewUnsignedMatrixSeq[uint16](&out, sofab.Bounds{}, sofab.Bounds{}, tcaps, math.MaxUint16)
	}
	mustCollect(t, first, newSeq())
	p0, p1 := &out[0][0], &out[1][0]

	out = out[:0]
	mustCollect(t, second, newSeq())

	if len(out) != 2 || len(out[0]) != 1 || out[0][0] != 7 || len(out[1]) != 2 || out[1][1] != 9 {
		t.Fatalf("out = %v, want [[7] [8 9]]", out)
	}
	if &out[0][0] != p0 || &out[1][0] != p1 {
		t.Fatal("the second decode did not reuse the first decode's row storage at the same index")
	}
}

// What the reuse must not change for a matrix: a row that ARRIVED empty is [],
// a row that never came is nil.
func TestMatrixSeqReuseKeepsEmptyAndGapApart(t *testing.T) {
	first := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{1})
		sofab.WriteUnsignedArray(e, 1, []uint32{2})
		sofab.WriteUnsignedArray(e, 2, []uint32{3})
	})
	second := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{})
		sofab.WriteUnsignedArray(e, 2, []uint32{4})
	})

	var out [][]uint8
	newSeq := func() sofab.Visitor {
		return sofab.NewUnsignedMatrixSeq[uint8](&out, sofab.Bounds{}, sofab.Bounds{}, tcaps, math.MaxUint8)
	}
	mustCollect(t, first, newSeq())
	out = out[:0]
	mustCollect(t, second, newSeq())

	if len(out) != 3 || out[0] != nil || out[1] == nil || len(out[1]) != 0 || len(out[2]) != 1 || out[2][0] != 4 {
		t.Fatalf("out = %#v, want [nil [] [4]]", out)
	}
}

// Every matrix flavour starts its row from the recycled storage, so the steady
// state allocates nothing once the rows fit.
func TestMatrixSeqSteadyStateAllocatesNothing(t *testing.T) {
	type rowSink interface {
		ArrayBegin(sofab.ID, sofab.ArrayKind, int) error
		ArrayEnd(sofab.ID) error
	}
	cases := []struct {
		name string
		kind sofab.ArrayKind
		seq  func() (rowSink, func())
		elem func(s rowSink, i int) error
	}{
		{"unsigned", sofab.ArrayUnsigned, func() (rowSink, func()) {
			var out [][]uint32
			return sofab.NewUnsignedMatrixSeq[uint32](&out, sofab.Bounds{}, sofab.Bounds{}, tcaps, math.MaxUint32), func() { out = out[:0] }
		}, func(s rowSink, i int) error {
			return s.(*sofab.UnsignedMatrixSeq[uint32]).ArrayUnsigned(0, i, uint64(i))
		}},
		{"signed", sofab.ArraySigned, func() (rowSink, func()) {
			var out [][]int32
			return sofab.NewSignedMatrixSeq[int32](&out, sofab.Bounds{}, sofab.Bounds{}, tcaps, math.MinInt32, math.MaxInt32), func() { out = out[:0] }
		}, func(s rowSink, i int) error {
			return s.(*sofab.SignedMatrixSeq[int32]).ArraySigned(0, i, int64(-i))
		}},
		{"fp32", sofab.ArrayFp32, func() (rowSink, func()) {
			var out [][]float32
			return sofab.NewFloat32MatrixSeq(&out, sofab.Bounds{}, sofab.Bounds{}, tcaps), func() { out = out[:0] }
		}, func(s rowSink, i int) error {
			return s.(*sofab.Float32MatrixSeq).ArrayFloat32(0, i, float32(i))
		}},
		{"fp64", sofab.ArrayFp64, func() (rowSink, func()) {
			var out [][]float64
			return sofab.NewFloat64MatrixSeq(&out, sofab.Bounds{}, sofab.Bounds{}, tcaps), func() { out = out[:0] }
		}, func(s rowSink, i int) error {
			return s.(*sofab.Float64MatrixSeq).ArrayFloat64(0, i, float64(i))
		}},
		{"bool", sofab.ArrayUnsigned, func() (rowSink, func()) {
			var out [][]bool
			return sofab.NewBoolMatrixSeq(&out, sofab.Bounds{}, sofab.Bounds{}, tcaps), func() { out = out[:0] }
		}, func(s rowSink, i int) error {
			return s.(*sofab.BoolMatrixSeq).ArrayUnsigned(0, i, uint64(i&1))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, reset := tc.seq()
			fill := func() {
				reset() // the truncation generated code emits for the field
				for id := 0; id < 3; id++ {
					if err := s.ArrayBegin(sofab.ID(id), tc.kind, 4); err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 4; i++ {
						if err := tc.elem(s, i); err != nil {
							t.Fatal(err)
						}
					}
					if err := s.ArrayEnd(sofab.ID(id)); err != nil {
						t.Fatal(err)
					}
				}
			}
			fill()
			if a := testing.AllocsPerRun(100, fill); a != 0 {
				t.Fatalf("%v allocations per refill, want 0", a)
			}
		})
	}
}

// A reopened row id in ONE decode replaces the row (§7.4) — now over the row's
// own storage, so a shorter second occurrence must still shorten it.
func TestMatrixSeqReopenedRowReplacesInPlace(t *testing.T) {
	raw := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{1, 2, 3})
		sofab.WriteUnsignedArray(e, 1, []uint32{4})
		sofab.WriteUnsignedArray(e, 0, []uint32{9})
	})

	var out [][]uint32
	mustCollect(t, raw, sofab.NewUnsignedMatrixSeq[uint32](&out, sofab.Bounds{}, sofab.Bounds{}, tcaps, 0))

	if len(out) != 2 || len(out[0]) != 1 || out[0][0] != 9 || len(out[1]) != 1 || out[1][0] != 4 {
		t.Fatalf("out = %v, want [[9] [4]]", out)
	}
}

// A reopened row is NOT built over the row it replaces: when it then fails
// mid-row, the earlier row must stand as it was, not half overwritten with a
// mix no message carried.
func TestMatrixSeqFailedReopenLeavesTheEarlierRowIntact(t *testing.T) {
	raw := matrixBytes(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{1, 2, 3})
		sofab.WriteUnsignedArray(e, 0, []uint32{9, 300}) // 300 is over u8
	})

	var out [][]uint8
	err := collect(raw, sofab.NewUnsignedMatrixSeq[uint8](&out, sofab.Bounds{}, sofab.Bounds{}, tcaps, math.MaxUint8))
	if !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("err = %v, want ErrInvalidMsg", err)
	}
	if len(out) != 1 || len(out[0]) != 3 || out[0][0] != 1 || out[0][2] != 3 {
		t.Fatalf("out = %v, want [[1 2 3]] -- the failed reopen wrote into the placed row", out)
	}
}

// A blob assembled from several chunks is the accumulator's buffer, handed
// over; where the stale slot is too small it is kept, not copied again, and
// either way the element is right and owns its bytes.
func TestBlobSeqChunkedElement(t *testing.T) {
	var out [][]byte
	s := sofab.NewBlobSeq(&out, sofab.Bounds{}, tcaps)
	put := func(id int, b []byte) {
		t.Helper()
		for off := 0; off < len(b); off += 2 {
			chunk := append([]byte(nil), b[off:min(off+2, len(b))]...)
			if err := s.Bytes(sofab.ID(id), len(b), off, chunk); err != nil {
				t.Fatal(err)
			}
			clear(chunk) // the caller's fed bytes are borrowed only for the call
		}
	}
	put(0, []byte{1, 2, 3})
	if !bytes.Equal(out[0], []byte{1, 2, 3}) {
		t.Fatalf("out[0] = %v, want [1 2 3]", out[0])
	}
	p := &out[0][0]

	out = out[:0]
	put(0, []byte{4, 5, 6}) // fits the stale slot: copied into it
	if !bytes.Equal(out[0], []byte{4, 5, 6}) || &out[0][0] != p {
		t.Fatalf("out[0] = %v, want [4 5 6] in the stale slot's storage", out[0])
	}

	out = out[:0]
	put(0, []byte{1, 2, 3, 4, 5, 6, 7}) // does not fit: the assembled buffer is kept
	if !bytes.Equal(out[0], []byte{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("out[0] = %v, want [1 .. 7]", out[0])
	}
}

func TestNestedSeqReusesRowStorage(t *testing.T) {
	first := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(0)
		e.WriteString(0, "a")
		e.WriteString(1, "b")
		e.WriteSequenceEndKeep()
	})
	second := wrapperSeq(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(0)
		e.WriteString(0, "c")
		e.WriteSequenceEndKeep()
	})

	var out [][]string
	newSeq := func() sofab.Visitor {
		return sofab.NewNestedSeq(&out, sofab.Bounds{}, tcaps, func(p *[]string) sofab.Visitor {
			return sofab.NewStringSeq(p, sofab.Bounds{}, tcaps)
		})
	}
	mustCollect(t, first, newSeq())
	p := &out[0][0]

	out = out[:0]
	mustCollect(t, second, newSeq())

	if len(out) != 1 || len(out[0]) != 1 || out[0][0] != "c" {
		t.Fatalf("out = %q, want [[c]]", out)
	}
	if &out[0][0] != p {
		t.Fatal("the second decode did not reuse the first decode's row storage")
	}
}
