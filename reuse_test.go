package sofab_test

// The steady-state encode: one Encoder, reused across messages.
//
// This is what CORELIB_PLAN §6.6.4 measures — "after the codec's one-time
// construction" — and it is the row to read when judging the cost of a change to
// the encoder's per-message path. The BenchmarkEncode* benchmarks beside it
// construct a fresh Encoder per iteration, so they measure construction as well;
// after §6.0.1's pending run moved into the constructor (MaxDepth ids, sized
// once) that is a real and visible cost, and separating the two is what keeps it
// from being mistaken for a slower encode.

import (
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func BenchmarkEncodeTypicalReused(b *testing.B) {
	buf := make([]byte, 4096)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.SetBuffer(buf, 0)
		benchEncodeTypical(e)
		if err := e.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}
