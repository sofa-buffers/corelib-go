package sofab_test

import (
	"errors"
	"math"
	"runtime"
	"testing"
	"unsafe"

	sofab "github.com/sofa-buffers/corelib-go"
)

// The encoder's own bounds (issue #84). Everything here is about bytes that must
// NOT be produced: the format-wide FIXLEN_MAX/ARRAY_MAX ceiling (CORELIB_PLAN
// §6.2) and the balance of sequence framing (§4.9). Both are cases where a
// caller mistake would otherwise be turned into a byte stream that every
// conformant decoder — this package's included — rejects as INVALID, while the
// encoder reported success. §5.1 forbids exactly that: a write reports a status,
// and partial output is never handed back as if it were complete.
//
// The verdict is ErrArgument (§6.3 `InvalidArgument`), not ErrInvalidMsg: the
// input is a caller argument, and there is no message to be malformed yet.

// ceiling mirrors FIXLEN_MAX / ARRAY_MAX (§6.2): 2³¹−1 for both, the same value
// the decoder enforces on a length or count word.
const ceiling = 0x7FFF_FFFF

// countingSink accepts bytes without keeping them. A regression in the ceiling
// guard writes gigabytes, so a bytes.Buffer here would take the test process out
// with an OOM instead of failing; this reports the size and stays flat.
type countingSink struct{ n int64 }

func (s *countingSink) Write(p []byte) (int, error) { s.n += int64(len(p)); return len(p), nil }

// WriteString keeps the string path copy-free for the same reason (the encoder
// prefers an io.StringWriter sink for an oversized payload).
func (s *countingSink) WriteString(str string) (int, error) {
	s.n += int64(len(str))
	return len(str), nil
}

// TestOversizeFieldIsRejected pins the ceiling on every writer that puts a
// caller-supplied length or element count on the wire. Each one must refuse with
// ErrArgument before a single byte is written: the length word of a >2 GiB blob,
// or a count past ARRAY_MAX, decodes as INVALID everywhere (§6.2), so producing
// it is data loss dressed up as success.
//
// The oversize inputs cost address space rather than memory. The backing arrays
// are allocated once, kept alive for the whole test, and never read or written —
// a rejecting encoder touches neither — so their pages are never faulted in.
// That is also why only the one-byte element types appear: an oversize
// []float32/[]float64 would have to reserve 8/16 GiB, which is a real risk of an
// allocation failure on a CI runner. Their guard is the same single comparison
// on the same branch, made once per writer.
func TestOversizeFieldIsRejected(t *testing.T) {
	if math.MaxInt <= ceiling {
		t.Skip("a slice longer than ARRAY_MAX cannot be built where int is 32-bit")
	}
	u := make([]uint8, ceiling+1)
	s := make([]int8, ceiling+1)
	// unsafe.String is the only way to reach WriteString's own guard without
	// materializing a second 2 GiB copy of u; the bytes are never mutated and
	// never read, which is exactly what it requires.
	str := unsafe.String(unsafe.SliceData(u), len(u))

	writers := map[string]func(*sofab.Encoder) error{
		"WriteBytes":         func(e *sofab.Encoder) error { return e.WriteBytes(1, u) },
		"WriteString":        func(e *sofab.Encoder) error { return e.WriteString(1, str) },
		"WriteUnsignedArray": func(e *sofab.Encoder) error { return sofab.WriteUnsignedArray(e, 1, u) },
		"WriteSignedArray":   func(e *sofab.Encoder) error { return sofab.WriteSignedArray(e, 1, s) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			sink := &countingSink{}
			e := sofab.NewEncoder(sink)
			if err := write(e); !errors.Is(err, sofab.ErrArgument) {
				t.Fatalf("oversize %s = %v, want ErrArgument", name, err)
			}
			if !errors.Is(e.Err(), sofab.ErrArgument) {
				t.Fatalf("sticky error = %v, want ErrArgument", e.Err())
			}
			if err := e.Flush(); !errors.Is(err, sofab.ErrArgument) {
				t.Fatalf("Flush = %v, want the sticky ErrArgument", err)
			}
			if sink.n != 0 {
				t.Fatalf("rejected field still wrote %d bytes", sink.n)
			}
		})
	}
	runtime.KeepAlive(u) // str aliases u's bytes and outlives the loop's last use
}

// TestUnbalancedSequenceEndIsRejected: a close with nothing open is a caller
// mistake, not a wire form. A bare 0x07 is an unbalanced sequence end — INVALID
// for every decoder (§4.9, §6.3), including this package's own pull and visitor
// surfaces — so the encoder must refuse it with ErrArgument and write nothing,
// symmetric with the MaxDepth guard WriteSequenceBeginLazy already applies to the
// opening direction.
func TestUnbalancedSequenceEndIsRejected(t *testing.T) {
	closers := map[string]func(*sofab.Encoder) error{
		"End":     func(e *sofab.Encoder) error { return e.WriteSequenceEnd() },
		"EndKeep": func(e *sofab.Encoder) error { return e.WriteSequenceEndKeep() },
	}
	// Each prologue leaves the encoder with no sequence open, by a different
	// route, and reports the bytes it legitimately produced.
	prologues := map[string]func(*sofab.Encoder) int{
		"nothing written": func(e *sofab.Encoder) int { return 0 },
		"a plain field":   func(e *sofab.Encoder) int { e.WriteUnsigned(0, 1); return 2 },
		"a framed sequence closed": func(e *sofab.Encoder) int {
			e.WriteSequenceBeginLazy(1)
			e.WriteUnsigned(0, 1)
			e.WriteSequenceEnd()
			return 4 // 0E 00 01 07
		},
		"an empty sequence dropped": func(e *sofab.Encoder) int {
			e.WriteSequenceBeginLazy(1)
			e.WriteSequenceEnd() // contentless: header and end marker both vanish
			return 0
		},
		"a kept empty sequence": func(e *sofab.Encoder) int {
			e.WriteSequenceBeginLazy(1)
			e.WriteSequenceEndKeep()
			return 2 // 0E 07
		},
	}
	for cname, closer := range closers {
		for pname, prologue := range prologues {
			t.Run(cname+"/"+pname, func(t *testing.T) {
				sink := &countingSink{}
				e := sofab.NewEncoder(sink)
				want := int64(prologue(e))
				if err := e.Flush(); err != nil {
					t.Fatalf("prologue: %v", err)
				}
				if sink.n != want {
					t.Fatalf("prologue wrote %d bytes, want %d", sink.n, want)
				}
				if err := closer(e); !errors.Is(err, sofab.ErrArgument) {
					t.Fatalf("unbalanced %s = %v, want ErrArgument", cname, err)
				}
				if !errors.Is(e.Err(), sofab.ErrArgument) {
					t.Fatalf("sticky error = %v, want ErrArgument", e.Err())
				}
				if err := e.Flush(); !errors.Is(err, sofab.ErrArgument) {
					t.Fatalf("Flush = %v, want the sticky ErrArgument", err)
				}
				if sink.n != want {
					t.Fatalf("rejected close wrote %d extra bytes", sink.n-want)
				}
			})
		}
	}
}

// TestBalancedSequenceFramingStillEncodes is the other half of the guard: the
// rejection must be exact, so every legal shape still produces its bytes and
// still round-trips. A depth counter that leaked (a closer that failed to give
// its slot back, an opener that took two) would show up here as a spurious
// ErrArgument on a perfectly balanced message.
func TestBalancedSequenceFramingStillEncodes(t *testing.T) {
	got := encode(t, func(e *sofab.Encoder) {
		for i := 0; i < 3; i++ { // sequential open/close cycles at the top level
			e.WriteSequenceBeginLazy(1)
			e.WriteUnsigned(0, uint64(i))
			e.WriteSequenceEnd()
		}
		e.WriteSequenceBeginLazy(2) // and a nest closed from the inside out
		e.WriteSequenceBeginLazy(3)
		e.WriteUnsigned(4, 7)
		e.WriteSequenceEnd()
		e.WriteSequenceEndKeep()
	})
	want := []byte{
		0x0E, 0x00, 0x00, 0x07,
		0x0E, 0x00, 0x01, 0x07,
		0x0E, 0x00, 0x02, 0x07,
		0x16, 0x1E, 0x20, 0x07, 0x07, 0x07,
	}
	wantBytes(t, got, want)
	if err := acceptBytes(got, baseV{}); err != nil {
		t.Fatalf("balanced framing does not decode: %v", err)
	}
}
