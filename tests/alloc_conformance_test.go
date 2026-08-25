package sofab_test

// The CORELIB_PLAN §6.6.4 measurement, as a test rather than a benchmark.
//
// §6.6.4 requires conformance to be checked BOTH ways: by reading the source for
// an allocator call on a codec path, and by MEASURING "an allocation count, or
// the heap high-water mark, over a complete encode and a complete decode,
// measured after the codec's one-time construction", which must be zero.
//
// Go boxes nothing the codec computes, so the zero applies here without the
// boxed-runtime allowance §6.6.4 grants Kotlin/JS and CPython, and this file
// carries no itemised §6.6.2 handles: the codec allocates none.
//
// Both halves assert ZERO. The encode half has for some time; the decode half
// does since the Visitor stopped delivering materialized aggregates (§6.6.3) —
// see TestDecodeAllocatesNothingAfterConstruction below.

import (
	"bytes"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// allocMessage is one complete message covering every field shape the encoder
// has a separate path for: a scalar pair, a float, a string, a blob, an integer
// array, a float array, and a lazily-framed nested sequence.
func allocMessage(e *sofab.Encoder, blob []byte, str string) {
	e.WriteUnsigned(1, 0xDEADBEEF)
	e.WriteSigned(2, -12345)
	e.WriteBool(3, true)
	e.WriteFloat32(4, 3.14159)
	e.WriteFloat64(5, 2.718281828)
	e.WriteString(6, str)
	e.WriteBytes(7, blob)
	sofab.WriteUnsignedArray(e, 8, []uint64{1, 2, 3, 4, 5, 6, 7, 8})
	e.WriteFloat64Array(9, []float64{1.5, 2.5, 3.5})
	e.WriteSequenceBeginLazy(10)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
	e.Flush()
}

// TestNoAllocationsAfterConstruction is the encode half of §6.6.4: after the
// codec's one-time construction, a complete encode allocates NOTHING, on every
// encoder form and whether or not the payload outgrows the buffer it travels
// through.
func TestNoAllocationsAfterConstruction(t *testing.T) {
	blob := bytes.Repeat([]byte{0x5A}, 1000)
	str := "sofabuffers, a payload long enough to cross a small buffer twice over"

	t.Run("caller buffer, no sink", func(t *testing.T) {
		buf := make([]byte, 4096)
		e, err := sofab.NewEncoderBuffer(buf, 0)
		if err != nil {
			t.Fatalf("NewEncoderBuffer: %v", err)
		}
		mustNotAllocate(t, func() {
			e.SetBuffer(buf, 0)
			allocMessage(e, blob, str)
		})
	})

	// The exact-fit tail path (stage): a sink-less buffer too small for the
	// reservation the encoder makes but big enough for the bytes it produces.
	// This used to allocate a stagedTail on the write path; §6.6 requires that
	// state to be sized in the constructor instead.
	t.Run("caller buffer entering the staged tail", func(t *testing.T) {
		small := make([]byte, 12) // legal: no sink, so no minimum applies (§5.1.4)
		e, err := sofab.NewEncoderBuffer(small, 0)
		if err != nil {
			t.Fatalf("NewEncoderBuffer: %v", err)
		}
		mustNotAllocate(t, func() {
			e.SetBuffer(small, 0)
			e.WriteUnsigned(0, 1)
			e.WriteUnsigned(1, 2)
			e.WriteUnsigned(2, 3)
			e.Flush()
		})
	})

	t.Run("caller buffer with a sink at MinOutputBuffer", func(t *testing.T) {
		buf := make([]byte, sofab.MinOutputBuffer)
		var n int
		e, err := sofab.NewEncoderSink(buf, 0, func(_ *sofab.Encoder, b []byte) error {
			n += len(b)
			return nil
		})
		if err != nil {
			t.Fatalf("NewEncoderSink: %v", err)
		}
		mustNotAllocate(t, func() {
			e.SetBuffer(buf, 0)
			allocMessage(e, blob, str)
		})
		if n == 0 {
			t.Fatal("the sink was never driven")
		}
	})

	// The io.Writer form: its scratch window is sized once in NewEncoder and
	// never grown, so the FIRST message that outgrows it allocates nothing
	// either — which is what the growable window used to fail.
	t.Run("io.Writer form, first oversized message", func(t *testing.T) {
		w := &countingWriter{}
		oversized := bytes.Repeat([]byte{7}, 6000) // past the scratch window
		e := sofab.NewEncoder(w)
		mustNotAllocate(t, func() {
			e.WriteBytes(1, oversized)
			e.WriteString(2, str)
			e.Flush()
		})
		if w.n == 0 {
			t.Fatal("the writer was never driven")
		}
	})
}

// countingWriter is an io.Writer that keeps nothing.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

func mustNotAllocate(t *testing.T, fn func()) {
	t.Helper()
	fn() // warm: first-call machinery is construction, not steady state
	if a := testing.AllocsPerRun(200, fn); a != 0 {
		t.Errorf("%.0f allocations per message after construction; §6.6 requires zero", a)
	}
}

// TestDecodeAllocatesNothingAfterConstruction is the decode half of §6.6.4, and
// it is now a CONFORMANCE PASS rather than a pinned count: after the decoder's
// one-time construction, a complete decode allocates NOTHING — for a small
// message and for a 12 KB one, at any chunking, with any field shape.
//
// What made that reachable is the callback surface (§6.6.3). The Visitor used to
// deliver materialized aggregates — a whole string, a whole element slice — and
// §6.6.3 says plainly that such a callback "obliges the codec to build that
// value, and the only size available to build it from is the wire's". Delivering
// a payload in pieces and an array element by element removes the obligation:
// the codec sizes nothing from the wire, and the destination's storage is the
// destination's own.
//
// The destination here therefore keeps nothing — it is the null consumer, which
// is what isolates the CODEC's allocations from its caller's. A destination that
// does keep values allocates for them; that is the generated layer, and §6.6.1
// is explicit that the generated layer allocates and the codec does not.
func TestDecodeAllocatesNothingAfterConstruction(t *testing.T) {
	build := func(blobLen, strLen int) []byte {
		buf := make([]byte, blobLen+strLen+256)
		e, err := sofab.NewEncoderBuffer(buf, 0)
		if err != nil {
			t.Fatalf("NewEncoderBuffer: %v", err)
		}
		e.WriteUnsigned(1, 7)
		e.WriteSigned(2, -7)
		e.WriteFloat32(3, 1.5)
		e.WriteFloat64(4, 2.5)
		e.WriteString(5, string(bytes.Repeat([]byte{'x'}, strLen)))
		e.WriteBytes(6, bytes.Repeat([]byte{0x5A}, blobLen))
		sofab.WriteUnsignedArray(e, 7, []uint64{1, 2, 3, 4})
		sofab.WriteSignedArray(e, 8, []int64{-1, -2, -3})
		e.WriteFloat64Array(9, []float64{1.5, 2.5})
		e.WriteSequenceBeginLazy(10)
		e.WriteUnsigned(1, 99)
		e.WriteSequenceEnd()
		if err := e.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		return append([]byte(nil), e.Bytes()...)
	}

	small, big := build(8, 4), build(8000, 4000)

	// ONE decoder, constructed outside the measured region and Reset per
	// message: that is the shape §6.6 describes, and Reset is what makes it
	// possible without a second construction.
	d := sofab.NewDecoder(sofab.VisitorBase{})

	feed := func(msg []byte, chunk int) func() {
		return func() {
			d.Reset(sofab.VisitorBase{})
			for i := 0; i < len(msg); i += chunk {
				end := i + chunk
				if end > len(msg) {
					end = len(msg)
				}
				if _, err := d.Feed(msg[i:end]); err != nil {
					t.Fatalf("Feed: %v", err)
				}
			}
			if d.Status() != sofab.Complete {
				t.Fatalf("Status = %v, want COMPLETE", d.Status())
			}
		}
	}

	for _, tc := range []struct {
		name string
		run  func()
	}{
		{"whole message, small", feed(small, len(small))},
		{"whole message, 12 KB", feed(big, len(big))},
		{"64-byte chunks, 12 KB", feed(big, 64)},
		{"one byte at a time, small", feed(small, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustNotAllocate(t, tc.run)
		})
	}
}

// TestAcceptBytesAllocatesOnlyItsDecoder pins the one-shot path's single
// allocation and says what it is: AcceptBytes CONSTRUCTS a decoder, which §6.6
// permits ("constructing the encoder or decoder ... MAY allocate"), and then
// allocates nothing at all for the message. The count therefore does not move
// with the message, which is the property the prohibition exists for — a sender
// cannot drive a receiver's memory.
func TestAcceptBytesAllocatesOnlyItsDecoder(t *testing.T) {
	build := func(n int) []byte {
		buf := make([]byte, n+256)
		e, err := sofab.NewEncoderBuffer(buf, 0)
		if err != nil {
			t.Fatalf("NewEncoderBuffer: %v", err)
		}
		e.WriteString(1, string(bytes.Repeat([]byte{'x'}, n)))
		sofab.WriteUnsignedArray(e, 2, []uint64{1, 2, 3, 4})
		if err := e.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		return append([]byte(nil), e.Bytes()...)
	}
	small, big := build(8), build(12000)

	run := func(msg []byte) func() {
		return func() {
			if err := sofab.AcceptBytes(msg, sofab.VisitorBase{}); err != nil {
				t.Fatalf("AcceptBytes: %v", err)
			}
		}
	}
	a := testing.AllocsPerRun(200, run(small))
	b := testing.AllocsPerRun(200, run(big))
	if a > 1 {
		t.Errorf("AcceptBytes allocates %.0f per message; only the decoder's own "+
			"construction may allocate (§6.6)", a)
	}
	if a != b {
		t.Errorf("AcceptBytes allocation count moved with the message: %.0f for a "+
			"small one, %.0f for a 12 KB one; the sender must not drive it", a, b)
	}
}
