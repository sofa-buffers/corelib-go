package sofab_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// The encoder's failure contract (CORELIB_PLAN §5.1, §6.3): the FIRST error is
// recorded and every later write becomes a no-op, so generated code can issue a
// run of writes and check once at Flush. These tests drive that from the places
// a failure can actually originate — a sink/writer that refuses bytes mid-run,
// and a caller-supplied buffer that runs out — rather than from the argument
// checks the rest of the suite already covers.

// refusingWriter accepts ok Writes and then fails every subsequent one with err.
// Failing PERSISTENTLY matters: a writer that recovered would let the encoder
// carry on and hide the sticky-error contract. (coverage_test.go's failWriter
// refuses from the very first Write and cannot express "fail the second".)
type refusingWriter struct {
	ok      int
	err     error
	written int
	calls   int
}

func (w *refusingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.ok > 0 {
		w.ok--
		w.written += len(p)
		return len(p), nil
	}
	return 0, w.err
}

// plainWriter is an io.Writer WITHOUT WriteString, so the string pass-through
// has to fall back to Write. bytes.Buffer implements io.StringWriter, which is
// why the fallback needs a writer of its own to be exercised at all.
type plainWriter struct {
	buf   []byte
	ok    int
	err   error
	calls int
}

func (w *plainWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.err != nil && w.ok <= 0 {
		return 0, w.err
	}
	w.ok--
	w.buf = append(w.buf, p...)
	return len(p), nil
}

var errSinkRefused = errors.New("downstream refused the bytes")

// A bulk array run reserves room per batch, so the writer failure lands inside
// the run rather than at Flush. Every element kind has its own unrolled run, and
// each must stop at the first failure and keep the error.
func TestArrayRunStopsAtTheFirstWriterFailure(t *testing.T) {
	big := make([]uint64, 2000)
	bigS := make([]int64, 2000)
	bigF32 := make([]float32, 4000)
	bigF64 := make([]float64, 4000)
	for i := range big {
		big[i] = uint64(i) * 0x9E3779B97F4A7C15 // wide values: 10 bytes each
		bigS[i] = -int64(i) * 0x1F3779B97F4A7C
	}

	for _, tc := range []struct {
		name  string
		write func(*sofab.Encoder) error
	}{
		{"unsigned array", func(e *sofab.Encoder) error { return sofab.WriteUnsignedArray(e, 1, big) }},
		{"signed array", func(e *sofab.Encoder) error { return sofab.WriteSignedArray(e, 1, bigS) }},
		{"fp32 array", func(e *sofab.Encoder) error { return e.WriteFloat32Array(1, bigF32) }},
		{"fp64 array", func(e *sofab.Encoder) error { return e.WriteFloat64Array(1, bigF64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &refusingWriter{err: errSinkRefused}
			e := sofab.NewEncoder(w)
			if err := tc.write(e); !errors.Is(err, errSinkRefused) {
				t.Fatalf("write = %v, want the writer's error", err)
			}
			if w.written != 0 {
				t.Errorf("writer accepted %d bytes, want 0", w.written)
			}
			// Sticky: a later write neither succeeds nor replaces the error, and
			// Flush reports the original one.
			if err := e.WriteUnsigned(2, 1); !errors.Is(err, errSinkRefused) {
				t.Errorf("later write = %v, want the first error", err)
			}
			if err := e.Flush(); !errors.Is(err, errSinkRefused) {
				t.Errorf("Flush = %v, want the first error", err)
			}
			if err := e.Err(); !errors.Is(err, errSinkRefused) {
				t.Errorf("Err = %v, want the first error", err)
			}
		})
	}
}

// A blob or string that fits the window's cap but not the room left in it forces
// a mid-stream drain; when that drain fails the payload must not be written
// piecemeal and the error must stick.
func TestPayloadThatNeedsADrainReportsTheWriterFailure(t *testing.T) {
	filler := bytes.Repeat([]byte{'x'}, 4000)
	for _, tc := range []struct {
		name  string
		write func(*sofab.Encoder) error
	}{
		{"blob", func(e *sofab.Encoder) error { return e.WriteBytes(2, bytes.Repeat([]byte{'y'}, 600)) }},
		{"string", func(e *sofab.Encoder) error { return e.WriteString(2, strings.Repeat("y", 600)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &refusingWriter{err: errSinkRefused}
			e := sofab.NewEncoder(w)
			// Fills the window without draining it.
			if err := e.WriteBytes(1, filler); err != nil {
				t.Fatalf("filler write = %v, want nil", err)
			}
			if err := tc.write(e); !errors.Is(err, errSinkRefused) {
				t.Fatalf("write = %v, want the writer's error", err)
			}
			if w.written != 0 {
				t.Errorf("writer accepted %d bytes, want 0", w.written)
			}
		})
	}
}

// A lazily-held sequence header is committed when its first child is written, so
// the commit itself can be the write that runs out of room. The failure must
// surface there, not be swallowed into a message missing its frame.
func TestLazySequenceCommitReportsTheWriterFailure(t *testing.T) {
	w := &refusingWriter{err: errSinkRefused}
	e := sofab.NewEncoder(w)
	// Leaves only a few bytes of the window free, so committing the pending
	// sequence header has to drain first.
	if err := e.WriteBytes(1, bytes.Repeat([]byte{'x'}, 4088)); err != nil {
		t.Fatalf("filler write = %v, want nil", err)
	}
	if err := e.WriteSequenceBeginLazy(2); err != nil {
		t.Fatalf("WriteSequenceBeginLazy = %v, want nil (nothing is written yet)", err)
	}
	if err := e.WriteUnsigned(1, 7); !errors.Is(err, errSinkRefused) {
		t.Fatalf("first child write = %v, want the writer's error", err)
	}
	if w.written != 0 {
		t.Errorf("writer accepted %d bytes, want 0", w.written)
	}
}

// Pass-through (§5.1) hands an oversized string straight to the writer instead
// of copying it through the window. The three outcomes that path can have are
// all contract: the payload arrives intact, a failing drain stops it before the
// payload is offered, and a failing payload write is recorded.
func TestPassThroughStringGoesStraightToTheWriter(t *testing.T) {
	big := strings.Repeat("abcdefgh", 1024) // 8 KiB, past the window cap

	t.Run("delivered through Write when the writer has no WriteString", func(t *testing.T) {
		w := &plainWriter{}
		e := sofab.NewEncoder(w, sofab.WithPassThrough(true))
		if err := e.WriteString(1, big); err != nil {
			t.Fatalf("WriteString = %v, want nil", err)
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush = %v, want nil", err)
		}
		d := sofab.NewDecoder(bytes.NewReader(w.buf))
		if _, err := d.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
		got, err := d.String()
		if err != nil {
			t.Fatalf("String: %v", err)
		}
		if got != big {
			t.Errorf("payload round-trip differs: %d bytes, want %d", len(got), len(big))
		}
	})

	t.Run("a failing drain stops before the payload", func(t *testing.T) {
		w := &plainWriter{err: errSinkRefused}
		e := sofab.NewEncoder(w, sofab.WithPassThrough(true))
		if err := e.WriteString(1, big); !errors.Is(err, errSinkRefused) {
			t.Fatalf("WriteString = %v, want the writer's error", err)
		}
		if len(w.buf) != 0 {
			t.Errorf("writer received %d bytes despite refusing the drain", len(w.buf))
		}
	})

	t.Run("a failing payload write is recorded", func(t *testing.T) {
		w := &plainWriter{err: errSinkRefused, ok: 1} // the drain succeeds, the payload does not
		e := sofab.NewEncoder(w, sofab.WithPassThrough(true))
		if err := e.WriteString(1, big); !errors.Is(err, errSinkRefused) {
			t.Fatalf("WriteString = %v, want the writer's error", err)
		}
		if w.calls < 2 {
			t.Errorf("writer saw %d calls, want the drain and then the payload", w.calls)
		}
	})
}

// The blob twin of the above: an oversized blob is offered to the writer whole,
// and a refusal there is the encoder's error.
func TestPassThroughBlobWriteErrorIsSticky(t *testing.T) {
	big := bytes.Repeat([]byte{0xAB}, 8192)
	w := &plainWriter{err: errSinkRefused, ok: 1}
	e := sofab.NewEncoder(w, sofab.WithPassThrough(true))
	if err := e.WriteBytes(1, big); !errors.Is(err, errSinkRefused) {
		t.Fatalf("WriteBytes = %v, want the writer's error", err)
	}
	if err := e.Flush(); !errors.Is(err, errSinkRefused) {
		t.Errorf("Flush = %v, want the first error", err)
	}
}

// A string payload is a DIVISIBLE run (§5.1): with a caller-supplied buffer and
// a sink, a string far larger than the buffer must stream through it in pieces
// and reassemble byte-for-byte on the far side. Pass-through is OFF here, which
// is what forces the split rather than a handover.
func TestStringStreamsThroughASmallSinkBuffer(t *testing.T) {
	payload := strings.Repeat("sofabuffers-", 40) // ~480 bytes through 32
	var out []byte
	buf := make([]byte, 32)
	e, err := sofab.NewEncoderSink(buf, 0, func(_ *sofab.Encoder, b []byte) error {
		out = append(out, b...)
		return nil
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	if err := e.WriteString(1, payload); err != nil {
		t.Fatalf("WriteString = %v, want nil", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush = %v, want nil", err)
	}
	d := sofab.NewDecoder(bytes.NewReader(out))
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	got, err := d.String()
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if got != payload {
		t.Errorf("streamed payload = %q…, want %q…", got[:min(16, len(got))], payload[:16])
	}
}

// A sink that refuses a flush stops the encode there: the refusal is the
// encoder's error, and nothing further is offered.
func TestSinkRefusalStopsTheEncode(t *testing.T) {
	calls := 0
	buf := make([]byte, 32)
	e, err := sofab.NewEncoderSink(buf, 0, func(*sofab.Encoder, []byte) error {
		calls++
		return errSinkRefused
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	if err := e.WriteString(1, strings.Repeat("z", 500)); !errors.Is(err, errSinkRefused) {
		t.Fatalf("WriteString = %v, want the sink's error", err)
	}
	if calls != 1 {
		t.Errorf("sink called %d times, want exactly 1", calls)
	}
	if err := e.WriteUnsigned(2, 1); !errors.Is(err, errSinkRefused) {
		t.Errorf("later write = %v, want the first error", err)
	}
	if calls != 1 {
		t.Errorf("sink called %d times after the refusal, want it left alone", calls)
	}
}

// A sink-less caller buffer that fills up is ErrBufferFull (§5.1), and the
// encoder must then refuse everything: no further bytes, and no new buffer
// either — SetBuffer is how a caller continues, and continuing past a recorded
// failure would silently produce a message with a hole in it.
func TestFullCallerBufferRefusesEverythingAfterwards(t *testing.T) {
	small := make([]byte, 8)
	e, err := sofab.NewEncoderBuffer(small, 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	if err := e.WriteBytes(1, bytes.Repeat([]byte{'x'}, 100)); err != nil {
		// The overflow may be reported by the write itself or at Flush; both are
		// the same sticky error.
		if !errors.Is(err, sofab.ErrBufferFull) {
			t.Fatalf("WriteBytes = %v, want ErrBufferFull", err)
		}
	}
	if err := e.Flush(); !errors.Is(err, sofab.ErrBufferFull) {
		t.Fatalf("Flush = %v, want ErrBufferFull", err)
	}
	written := len(e.Bytes())

	// A replacement buffer must NOT resurrect the encode.
	if err := e.SetBuffer(make([]byte, 4096), 0); !errors.Is(err, sofab.ErrBufferFull) {
		t.Errorf("SetBuffer after the failure = %v, want the sticky ErrBufferFull", err)
	}
	// And neither must a further write of any shape.
	for _, w := range []func() error{
		func() error { return e.WriteUnsigned(2, 1) },
		func() error { return e.WriteBytes(3, bytes.Repeat([]byte{'y'}, 200)) },
		func() error { return e.WriteString(4, strings.Repeat("y", 200)) },
		func() error { return sofab.WriteUnsignedArray(e, 5, []uint64{1, 2, 3}) },
	} {
		if err := w(); !errors.Is(err, sofab.ErrBufferFull) {
			t.Errorf("write after the failure = %v, want the sticky ErrBufferFull", err)
		}
	}
	if got := len(e.Bytes()); got != written {
		t.Errorf("the buffer grew from %d to %d bytes after the failure", written, got)
	}
}
