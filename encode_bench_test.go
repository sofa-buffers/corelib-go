package sofab_test

// Benchmarks for the encode path. They mirror decode_bench_test.go and drive the
// same shapes cmd/perfbench does — a 1000-element u64 array and the mixed
// "typical" message — so an encode change has a number next to it under the same
// harness the decode side is measured with (-count/benchstat).
//
// Both encoder forms are covered, because they allocate differently: the
// io.Writer form owns (and grows) a window of its own, while the caller-supplied
// buffer form (CORELIB_PLAN §5.1) writes straight into the caller's storage and
// must allocate nothing per message.

import (
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// discard is a sink that keeps nothing: the measured work is the encode, not the
// destination.
type discard struct{ n int }

func (d *discard) Write(p []byte) (int, error) { d.n += len(p); return len(p), nil }

func benchEncodeTypical(e *sofab.Encoder) {
	e.WriteUnsigned(1, 0xDEADBEEF)
	e.WriteSigned(2, -12345)
	e.WriteBool(3, true)
	e.WriteFloat32(4, 3.14159)
	e.WriteString(5, "sofab")
	sofab.WriteUnsignedArray(e, 6, []uint16{10, 20, 30, 40})
	e.WriteSequenceBeginLazy(7)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
}

func BenchmarkEncodeTypical(b *testing.B) {
	w := &discard{}
	benchEncodeTypical(sofab.NewEncoder(w))
	b.SetBytes(int64(w.n))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := sofab.NewEncoder(w)
		benchEncodeTypical(e)
		if err := e.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeU64Array(b *testing.B) {
	src := benchSrc()
	w := &discard{}
	sofab.WriteUnsignedArray(sofab.NewEncoder(w), 1, src)
	b.SetBytes(int64(w.n))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := sofab.NewEncoder(w)
		sofab.WriteUnsignedArray(e, 1, src)
		if err := e.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

// The *Buffer variants encode into a caller-supplied buffer (§5.1) — the form
// with no window of its own and nowhere to flush to, so the steady state must be
// allocation-free apart from the Encoder itself.

func BenchmarkEncodeTypicalBuffer(b *testing.B) {
	buf := make([]byte, 512)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		b.Fatal(err)
	}
	benchEncodeTypical(e)
	b.SetBytes(int64(len(e.Bytes())))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := sofab.NewEncoderBuffer(buf, 0)
		if err != nil {
			b.Fatal(err)
		}
		benchEncodeTypical(e)
		if err := e.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeU64ArrayBuffer(b *testing.B) {
	src := benchSrc()
	buf := make([]byte, 16*1024)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		b.Fatal(err)
	}
	sofab.WriteUnsignedArray(e, 1, src)
	b.SetBytes(int64(len(e.Bytes())))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := sofab.NewEncoderBuffer(buf, 0)
		if err != nil {
			b.Fatal(err)
		}
		sofab.WriteUnsignedArray(e, 1, src)
		if err := e.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}
