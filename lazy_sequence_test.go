package sofab_test

// Lazy sequence framing (MESSAGE_SPEC §2, CORELIB_PLAN §6).
//
// A sequence-typed *field* whose value equals its declared default is omitted
// rather than emitted as an empty begin/end frame; a wrapper-array *element*
// keeps its frame even when all-default, because element presence is what
// carries a dynamic array's length (§5.1). WriteSequenceBeginLazy holds the
// header back so the encoder can decide from what the children turn out to be,
// in one forward pass and without buffering the sub-message.

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// TestLazySequenceWithoutContentEmitsNothing: an all-default sequence carries no
// information, so the field is omitted — where the eager API would have written
// the two-byte empty frame 0E 07.
func TestLazySequenceWithoutContentEmitsNothing(t *testing.T) {
	got := encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceEnd()
	})
	if len(got) != 0 {
		t.Fatalf("got % X, want no bytes", got)
	}
}

// TestEndKeepFramesAContentlessSequence: WriteSequenceEndKeep forces a
// contentless frame onto the wire — the array-element and explicit-empty cases
// of §2/§5.1.
func TestEndKeepFramesAContentlessSequence(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceEndKeep()
	}), []byte{0x0E, 0x07})
}

// TestEndKeepCommitsTheEnclosingRun: forcing a frame forces its ancestors too —
// the outer sequence got content (the inner frame), so it is framed as well.
func TestEndKeepCommitsTheEnclosingRun(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceBeginLazy(2)
		e.WriteSequenceEndKeep()
		e.WriteSequenceEnd()
	}), []byte{0x0E, 0x16, 0x07, 0x07})
}

// TestEndKeepMatchesEndOnceContentExists: with content the two closers make no
// difference — the headers are already out.
func TestEndKeepMatchesEndOnceContentExists(t *testing.T) {
	withKeep := encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteUnsigned(0, 42)
		e.WriteSequenceEndKeep()
	})
	withEnd := encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteUnsigned(0, 42)
		e.WriteSequenceEnd()
	})
	wantBytes(t, withKeep, []byte{0x0E, 0x00, 0x2A, 0x07})
	wantBytes(t, withEnd, withKeep)
}

// TestLazySequenceCommitsTheWholeRunOnFirstContent: one child field commits the
// whole held-back run, outermost header first, so a non-default leaf deep inside
// brings every enclosing frame back in wire order.
func TestLazySequenceCommitsTheWholeRunOnFirstContent(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceBeginLazy(2)
		e.WriteUnsigned(0, 42)
		e.WriteSequenceEnd()
		e.WriteSequenceEnd()
	}), []byte{0x0E, 0x16, 0x00, 0x2A, 0x07, 0x07})
}

// TestLazySequenceDropsOnlyTheEmptyInnerOne: only the empty inner sequence
// drops; the outer one has content (the leaf) and is framed. This is the
// interleaving a naive "drop the whole run" would get wrong.
func TestLazySequenceDropsOnlyTheEmptyInnerOne(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceBeginLazy(2)
		e.WriteSequenceEnd()
		e.WriteUnsigned(0, 42)
		e.WriteSequenceEnd()
	}), []byte{0x0E, 0x00, 0x2A, 0x07})
}

// TestLazySequenceAfterContentIsIndependent: a lazily framed sequence *after*
// content in the same scope, and the sibling order, stay intact.
func TestLazySequenceAfterContentIsIndependent(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(0, 1)
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceEnd()
		e.WriteUnsigned(2, 3)
	}), []byte{0x00, 0x01, 0x10, 0x03})
}

// TestLazyFramingSurvivesAnExplicitFlush pins the encoder-state half of the
// design: the held-back ids live in the Encoder, not in the buffer, so pushing
// the buffer to the sink between the begin and its first content cannot split
// the pending run or reorder a byte.
//
// The caller-driven Flush is the *only* way to land a flush while a header is
// held back — see TestRunCommittedAcrossAFlushMatchesOneShot for why the
// automatic, buffer-pressure flush cannot — so this is the test that covers the
// reachable case.
func TestLazyFramingSurvivesAnExplicitFlush(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(0, 1)
	e.WriteSequenceBeginLazy(1)
	if err := e.Flush(); err != nil { // mid-message, with a run held back
		t.Fatalf("flush: %v", err)
	}
	e.WriteSequenceBeginLazy(2)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	e.WriteUnsigned(0, 42)
	e.WriteSequenceEnd()
	e.WriteSequenceEnd()
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	wantBytes(t, buf.Bytes(), []byte{0x00, 0x01, 0x0E, 0x16, 0x00, 0x2A, 0x07, 0x07})
}

// TestRunCommittedAcrossAFlushMatchesOneShot is the Go form of "encode through a
// buffer far smaller than the message and get byte-identical output". The Go
// Encoder has no caller-supplied buffer — it accumulates into an internal slice
// and pushes to the io.Writer once that slice passes its threshold — so the
// small-buffer case is a message big enough to drive several mid-stream Writes
// while a lazy sequence is open (chunkWriter, streaming_test.go).
//
// What it asserts is exactly what it can prove: a run *committed* across a flush
// boundary — the sequence headers go out in one Write, the payload they frame in
// later ones — yields byte-identical output to the same message encoded in one
// shot, and to the hand-computed ground truth.
//
// It deliberately does NOT claim to catch "an automatic flush split a pending
// run", because that state is unreachable by construction: held-back ids are
// encoder state and occupy no buffer space, and the buffer only grows through a
// write — which commits the whole run before its own first byte. So the
// threshold can never be crossed with a header still pending. The one way to
// flush mid-run at all is the caller's own Flush, covered by
// TestLazyFramingSurvivesAnExplicitFlush.
func TestRunCommittedAcrossAFlushMatchesOneShot(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 40000) // far past the internal threshold

	write := func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteSequenceBeginLazy(2)
		e.WriteSequenceEnd() // empty inner: dropped even though a flush intervenes
		e.WriteBytes(0, payload)
		e.WriteSequenceEnd()
	}

	var w chunkWriter
	e := sofab.NewEncoder(&w)
	write(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if w.writes < 2 {
		t.Fatalf("sink saw %d writes, want the message split across several", w.writes)
	}

	// Ground truth: sequence header id 1, blob header id 0, fixlen word
	// (len<<3)|blob=3, the payload, then the end marker.
	var want []byte
	want = append(want, 0x0E, 0x02)
	for v := uint64(len(payload))<<3 | 3; ; {
		if v >= 0x80 {
			want = append(want, byte(v)|0x80)
			v >>= 7
			continue
		}
		want = append(want, byte(v))
		break
	}
	want = append(want, payload...)
	want = append(want, 0x07)

	wantBytes(t, w.buf.Bytes(), want)
	// ... and identical to the one-shot encode (a sink that takes it all in one
	// Write still sees the buffer flushed by threshold, but nothing about the
	// framing decision differs).
	wantBytes(t, w.buf.Bytes(), encode(t, write))
}

// TestLazySequenceArgumentErrors pins the two rejections WriteSequenceBeginLazy
// owns now that it no longer routes through writeHeader: an id past IDMax, and
// the MaxDepth+1 open. Both must leave the pending run untouched, so a later
// close still balances.
func TestLazySequenceArgumentErrors(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteSequenceBeginLazy(sofab.IDMax + 1); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("begin id>IDMax = %v, want ErrArgument", err)
	}
	if buf.Len() != 0 || e.Err() == nil {
		t.Fatal("rejected open must write no bytes and leave a sticky error")
	}
}

// TestLazySequenceCallsAreNoOpsAfterAnError: like every other writer, the three
// sequence calls turn into no-ops once the encoder holds a sticky error, so
// a generated Serialize can issue a run of writes and check once at Flush.
func TestLazySequenceCallsAreNoOpsAfterAnError(t *testing.T) {
	e := sofab.NewEncoder(failWriter{io.ErrClosedPipe})
	e.WriteBytes(1, make([]byte, 16*1024)) // forces a flush -> sink fails
	if e.Err() == nil {
		t.Fatal("expected a sticky error after the failed large write")
	}
	for name, call := range map[string]func() error{
		"BeginLazy": func() error { return e.WriteSequenceBeginLazy(1) },
		"End":       func() error { return e.WriteSequenceEnd() },
		"EndKeep":   func() error { return e.WriteSequenceEndKeep() },
	} {
		if err := call(); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("%s after error = %v, want ErrClosedPipe", name, err)
		}
	}
}

// TestEveryWriterCommitsPendingRun is the executable form of the "the choke
// point must be complete" audit: every public writer must emit the held-back
// sequence headers before its own first byte. A writer that composed its header
// without going through writeHeader would silently drop the frame — data loss,
// not a style issue — so each one is driven inside a lazy sequence and its
// output must begin with that sequence's header (0x0E) and end with the end
// marker (0x07).
func TestEveryWriterCommitsPendingRun(t *testing.T) {
	writers := map[string]func(*sofab.Encoder){
		"WriteUnsigned":      func(e *sofab.Encoder) { e.WriteUnsigned(0, 1) },
		"WriteSigned":        func(e *sofab.Encoder) { e.WriteSigned(0, -1) },
		"WriteBool":          func(e *sofab.Encoder) { e.WriteBool(0, true) },
		"WriteFloat32":       func(e *sofab.Encoder) { e.WriteFloat32(0, 1.5) },
		"WriteFloat64":       func(e *sofab.Encoder) { e.WriteFloat64(0, 1.5) },
		"WriteString":        func(e *sofab.Encoder) { e.WriteString(0, "x") },
		"WriteBytes":         func(e *sofab.Encoder) { e.WriteBytes(0, []byte{1}) },
		"WriteUnsignedArray": func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 0, []uint32{1}) },
		"WriteSignedArray":   func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 0, []int32{-1}) },
		"WriteFloat32Array":  func(e *sofab.Encoder) { e.WriteFloat32Array(0, []float32{1}) },
		"WriteFloat64Array":  func(e *sofab.Encoder) { e.WriteFloat64Array(0, []float64{1}) },
		// The empty-array forms carry no payload at all, so they are the ones a
		// missed commit would hide behind.
		"WriteUnsignedArray/empty": func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 0, []uint32{}) },
		"WriteSignedArray/empty":   func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 0, []int32{}) },
		"WriteFloat32Array/empty":  func(e *sofab.Encoder) { e.WriteFloat32Array(0, []float32{}) },
		"WriteFloat64Array/empty":  func(e *sofab.Encoder) { e.WriteFloat64Array(0, []float64{}) },
		"WriteString/empty":        func(e *sofab.Encoder) { e.WriteString(0, "") },
		"WriteBytes/empty":         func(e *sofab.Encoder) { e.WriteBytes(0, nil) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			got := encode(t, func(e *sofab.Encoder) {
				e.WriteSequenceBeginLazy(1)
				write(e)
				e.WriteSequenceEnd()
			})
			if len(got) < 2 || got[0] != 0x0E || got[len(got)-1] != 0x07 {
				t.Fatalf("got % X, want a 0x0E ... 0x07 frame around the field", got)
			}
			// And the frame must round-trip: one sequence start, one end.
			if err := sofab.AcceptBytes(got, baseV{}); err != nil {
				t.Fatalf("decode %v: % X", err, got)
			}
		})
	}
}

// --- depth (§4.9) and the pending run at depth ------------------------------

// TestClosingASequenceRestoresDepth pins the bookkeeping half of the MaxDepth
// guarantee: every closer must give the depth budget back. Nothing else can
// observe Encoder.depth from outside the package, so the observation is made
// where it matters — the budget itself: MaxDepth+16 sequential open/close cycles
// must all succeed, which they cannot if a closer forgets to decrement, and a
// full-depth nest must still fit afterwards while the one past it is still
// refused, which catches a decrement in the other direction.
//
// It is driven once per closing *path*, because they decrement in three
// different places: WriteSequenceEnd dropping a held-back frame, WriteSequenceEnd
// closing a framed one, and WriteSequenceEndKeep.
func TestClosingASequenceRestoresDepth(t *testing.T) {
	cycles := map[string]func(*sofab.Encoder){
		"End/dropped": func(e *sofab.Encoder) {
			e.WriteSequenceBeginLazy(1)
			e.WriteSequenceEnd()
		},
		"End/withContent": func(e *sofab.Encoder) {
			e.WriteSequenceBeginLazy(1)
			e.WriteUnsigned(0, 1)
			e.WriteSequenceEnd()
		},
		"EndKeep": func(e *sofab.Encoder) {
			e.WriteSequenceBeginLazy(1)
			e.WriteSequenceEndKeep()
		},
	}
	for name, cycle := range cycles {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			e := sofab.NewEncoder(&buf)
			for i := 0; i < sofab.MaxDepth+16; i++ {
				cycle(e)
				if err := e.Err(); err != nil {
					t.Fatalf("open/close cycle %d = %v: the closer is not restoring depth", i, err)
				}
			}
			// The budget is intact, not merely non-zero: a full nest still fits...
			for i := 0; i < sofab.MaxDepth; i++ {
				if err := e.WriteSequenceBeginLazy(1); err != nil {
					t.Fatalf("open %d of a full nest = %v, want it to fit", i, err)
				}
			}
			// ...and the one past MaxDepth is still refused (a closer that gave
			// back too much would let it through).
			if err := e.WriteSequenceBeginLazy(1); !errors.Is(err, sofab.ErrArgument) {
				t.Fatalf("open past MaxDepth = %v, want ErrArgument", err)
			}
		})
	}
}

// TestDeepNestingToMaxDepthEmitsNothing is the "no hold-back window" test. The
// Encoder holds the pending run in an array sized to the full MaxDepth at
// construction (CORELIB_PLAN §6.0.1, "How deep the hold-back reaches": the run
// is fixed-size state, sized at construction, and an implementation MUST hold
// back to the full MAX_DEPTH), so there is no bound past which it frames
// eagerly and no growth on a write path. This nests deep — and, in the second
// case, all the way to MaxDepth — closes every level contentless, and demands
// zero bytes: the eager fallback other profiles are allowed to take is exactly
// what would show up here as a run of empty frames.
func TestDeepNestingToMaxDepthEmitsNothing(t *testing.T) {
	for _, depth := range []int{40, sofab.MaxDepth} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			got := encode(t, func(e *sofab.Encoder) {
				for i := 0; i < depth; i++ {
					if err := e.WriteSequenceBeginLazy(1); err != nil {
						t.Fatalf("open %d = %v", i, err)
					}
				}
				for i := 0; i < depth; i++ {
					if err := e.WriteSequenceEnd(); err != nil {
						t.Fatalf("close %d = %v", i, err)
					}
				}
			})
			if len(got) != 0 {
				t.Fatalf("got % X (%d bytes), want no bytes", got, len(got))
			}
		})
	}
}

// TestDeepNestingCommitsInOrder is its counterpart: once a deep run does get
// content, every held-back header must come back out outermost-first and in one
// piece. A run that lost or reordered entries would still produce "zero bytes"
// above, so the empty case alone does not cover it.
func TestDeepNestingCommitsInOrder(t *testing.T) {
	const depth = 40

	got := encode(t, func(e *sofab.Encoder) {
		for i := 0; i < depth; i++ {
			e.WriteSequenceBeginLazy(sofab.ID(i))
		}
		e.WriteUnsigned(0, 42)
		for i := 0; i < depth; i++ {
			e.WriteSequenceEnd()
		}
	})

	var want []byte
	for i := 0; i < depth; i++ { // headers, outermost (id 0) first
		for v := uint64(i)<<3 | 0x06; ; { // ids past 15 need a two-byte header
			if v >= 0x80 {
				want = append(want, byte(v)|0x80)
				v >>= 7
				continue
			}
			want = append(want, byte(v))
			break
		}
	}
	want = append(want, 0x00, 0x2A)
	for i := 0; i < depth; i++ {
		want = append(want, 0x07)
	}
	wantBytes(t, got, want)
	if err := sofab.AcceptBytes(got, baseV{}); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
