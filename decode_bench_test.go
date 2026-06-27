package sofab_test

// Benchmarks for the visitor-driven decode path (Decoder.Accept). These drive
// the same two workloads as cmd/perfbench (a 1000-element u64 array and the
// mixed "typical" message) but under the go test harness, so -count/benchstat
// give stable, comparable numbers across optimization steps.

import (
	"bytes"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func benchSrc() []uint64 {
	src := make([]uint64, 1000)
	for i := range src {
		src[i] = uint64(i) * 0x9E3779B97F4A7C15
	}
	return src
}

func encodeU64Array() []byte {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	sofab.WriteUnsignedArray(e, 1, benchSrc())
	_ = e.Flush()
	return buf.Bytes()
}

func encodeTypicalMsg() []byte {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(1, 0xDEADBEEF)
	e.WriteSigned(2, -12345)
	e.WriteBool(3, true)
	e.WriteFloat32(4, 3.14159)
	e.WriteString(5, "sofab")
	sofab.WriteUnsignedArray(e, 6, []uint16{10, 20, 30, 40})
	e.WriteSequenceBegin(7)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
	_ = e.Flush()
	return buf.Bytes()
}

var benchSink uint64

type benchU64Visitor struct{ baseV }

func (benchU64Visitor) UnsignedArray(_ sofab.ID, v []uint64) error {
	benchSink += v[0] + v[len(v)-1]
	return nil
}

type benchTypicalVisitor struct{ baseV }

func (benchTypicalVisitor) Unsigned(_ sofab.ID, v uint64) error { benchSink += v; return nil }
func (benchTypicalVisitor) Signed(_ sofab.ID, v int64) error    { benchSink += uint64(v); return nil }
func (benchTypicalVisitor) Float32(_ sofab.ID, v float32) error { benchSink += uint64(v); return nil }
func (benchTypicalVisitor) String(_ sofab.ID, s string) error {
	benchSink += uint64(len(s))
	return nil
}
func (benchTypicalVisitor) UnsignedArray(_ sofab.ID, v []uint64) error {
	benchSink += v[0]
	return nil
}

func BenchmarkDecodeU64Array(b *testing.B) {
	msg := encodeU64Array()
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sofab.NewDecoder(bytes.NewReader(msg)).Accept(benchU64Visitor{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeTypical(b *testing.B) {
	msg := encodeTypicalMsg()
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sofab.NewDecoder(bytes.NewReader(msg)).Accept(benchTypicalVisitor{}); err != nil {
			b.Fatal(err)
		}
	}
}

// The *Bytes variants decode straight from an in-memory buffer (AcceptBytes),
// skipping the reader slurp — the zero-copy ceiling.

func BenchmarkDecodeU64ArrayBytes(b *testing.B) {
	msg := encodeU64Array()
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sofab.AcceptBytes(msg, benchU64Visitor{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeTypicalBytes(b *testing.B) {
	msg := encodeTypicalMsg()
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sofab.AcceptBytes(msg, benchTypicalVisitor{}); err != nil {
			b.Fatal(err)
		}
	}
}
