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
// The encode half asserts zero on every encoder form. The decode half is
// currently NOT zero, and the numbers are pinned rather than asserted away: the
// Visitor interface delivers materialized aggregates (whole strings, whole
// element slices), which §6.6.3 says obliges the codec to build them from the
// wire's size. Removing that is a breaking change to the Visitor interface that
// has to land together with the generator's golang backend (corelib-go#127), so
// until it does, this test is the artefact §6.6.4 asks for: the count is
// visible, it is bounded, and it CANNOT GROW WITHOUT THIS TEST FAILING.

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

// TestDecodeAllocationsArePinned is the decode half of §6.6.4. It is a PINNED
// COUNT, not a conformance pass: the numbers below are the aggregates the
// Visitor interface obliges the codec to materialize (§6.6.3), and corelib-go#127
// is the change that removes them. Two properties are asserted meanwhile:
//
//   - the count does not grow — a new allocation on a decode path fails here;
//   - the count does not depend on the MESSAGE, only on its field shape. §6.6.4
//     names that as the fallback claim for a runtime that cannot reach zero, and
//     it is the property the prohibition exists for: a sender must not be able to
//     drive the receiver's allocation COUNT (the sizes still move, which is
//     exactly why #127 has to land).
func TestDecodeAllocationsArePinned(t *testing.T) {
	build := func(blobLen, strLen int) []byte {
		buf := make([]byte, blobLen+strLen+256)
		e, err := sofab.NewEncoderBuffer(buf, 0)
		if err != nil {
			t.Fatalf("NewEncoderBuffer: %v", err)
		}
		e.WriteUnsigned(1, 7)
		e.WriteString(2, string(bytes.Repeat([]byte{'x'}, strLen)))
		e.WriteBytes(3, bytes.Repeat([]byte{0x5A}, blobLen))
		sofab.WriteUnsignedArray(e, 4, []uint64{1, 2, 3, 4})
		if err := e.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		return append([]byte(nil), e.Bytes()...)
	}

	small, big := build(8, 4), build(8000, 4000)

	for _, tc := range []struct {
		name string
		want float64
		run  func(msg []byte)
	}{
		// AcceptBytes: the string (Go's string(b) copies) and the u64 slice.
		// The blob is passed through the callback as a slice of the caller's own
		// bytes, which §6.7 permits and does not allocate.
		{"AcceptBytes", 2, func(msg []byte) {
			if err := sofab.AcceptBytes(msg, baseV{}); err != nil {
				t.Fatalf("AcceptBytes: %v", err)
			}
		}},
		// Decoder.Accept adds the slurp buffer, which is sized from the stream,
		// plus the per-message Decoder the API forces (the reader is bound at
		// construction, so there is no reusable codec to hoist out of the loop —
		// itself part of what corelib-go#127 has to answer).
		{"Accept", 4, func(msg []byte) {
			if err := sofab.NewDecoder(bytes.NewReader(msg)).Accept(baseV{}); err != nil {
				t.Fatalf("Accept: %v", err)
			}
		}},
		// AcceptStream never slurps; it materializes each field as it arrives,
		// and adds a bufio.Reader over the source. Same per-message construction
		// caveat as Accept.
		{"AcceptStream", 7, func(msg []byte) {
			if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(baseV{}); err != nil {
				t.Fatalf("AcceptStream: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(small)
			a := testing.AllocsPerRun(200, func() { tc.run(small) })
			b := testing.AllocsPerRun(200, func() { tc.run(big) })
			if a != tc.want {
				t.Errorf("%s allocates %.0f per message, pinned at %.0f "+
					"(corelib-go#127 is the change that takes it to 0)", tc.name, a, tc.want)
			}
			if a != b {
				t.Errorf("%s allocation COUNT moved with the message: %.0f for a small one, "+
					"%.0f for a 12 KB one; the sender must not drive it", tc.name, a, b)
			}
		})
	}
}
