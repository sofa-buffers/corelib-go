package sofab_test

// Allocation regression tests for the fixed-width (fp32/fp64) reader path,
// issue #85. This is a maxspeed corelib: decoding an fp32/fp64 array over a
// reader must not cost one heap allocation per element. The tests below hold
// the streaming visitor path (AcceptStream) and the pull path
// (ReadFloat32Array / ReadFloat64Array, Float32 / Float64) to allocation counts
// that do NOT scale with the element count, and check that the batched reads
// still decode bit-exactly at every chunk boundary.

import (
	"bytes"
	"io"
	"math"
	"testing"
	"testing/iotest"

	sofab "github.com/sofa-buffers/corelib-go"
)

// fp32Payload builds n distinct fp32 values with bits spread over all four
// bytes, so a mis-strided batch read cannot pass by accident.
func fp32Payload(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(uint32(i)*0x9E3779B1 | 1)
	}
	return out
}

func fp64Payload(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(uint64(i)*0x9E3779B97F4A7C15 | 1)
	}
	return out
}

func encodeFp32Array(t testing.TB, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteFloat32Array(1, fp32Payload(n)); err != nil {
		t.Fatalf("WriteFloat32Array: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

func encodeFp64Array(t testing.TB, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteFloat64Array(1, fp64Payload(n)); err != nil {
		t.Fatalf("WriteFloat64Array: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

// encodeFloatScalars writes n alternating fp32/fp64 scalar fields — the second
// half of issue #85, where every field read a freshly allocated 4/8-byte slice.
func encodeFloatScalars(t testing.TB, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	for i := 0; i < n; i++ {
		var err error
		if i%2 == 0 {
			err = e.WriteFloat32(sofab.ID(i%16+1), float32(i)+0.5)
		} else {
			err = e.WriteFloat64(sofab.ID(i%16+1), float64(i)+0.25)
		}
		if err != nil {
			t.Fatalf("write scalar %d: %v", i, err)
		}
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.Bytes()
}

// fpSink consumes float events without doing floating-point arithmetic on them:
// the payloads deliberately contain NaNs and subnormals (bit patterns are what
// the decoder must preserve), and summing those would time the FPU's subnormal
// penalty rather than the decode being measured.
type fpSink struct {
	baseV
	sum uint64
}

func (s *fpSink) Float32(_ sofab.ID, v float32) error {
	s.sum += uint64(math.Float32bits(v))
	return nil
}
func (s *fpSink) Float64(_ sofab.ID, v float64) error { s.sum += math.Float64bits(v); return nil }
func (s *fpSink) Float32Array(_ sofab.ID, v []float32) error {
	for _, x := range v {
		s.sum += uint64(math.Float32bits(x))
	}
	return nil
}
func (s *fpSink) Float64Array(_ sofab.ID, v []float64) error {
	for _, x := range v {
		s.sum += math.Float64bits(x)
	}
	return nil
}

// allocGrowth measures how many allocations decoding `big` costs over decoding
// `small`. Anything proportional to the element count shows up here; the
// fixed cost of the decoder, its buffer and the output slice's growth does not.
func allocGrowth(t *testing.T, small, big []byte, decode func([]byte)) float64 {
	t.Helper()
	a := testing.AllocsPerRun(50, func() { decode(small) })
	b := testing.AllocsPerRun(50, func() { decode(big) })
	t.Logf("allocs: small=%v big=%v", a, b)
	return b - a
}

// TestAcceptStreamFixlenArrayAllocsDoNotScale is the regression test for issue
// #85: reading a fixlen array element-by-element through readRaw allocated one
// slice per element, so a 1000-element array cost ~1000 allocations more than a
// 100-element one. Batched reads out of the reader's own buffer allocate only
// the output slice's growth.
func TestAcceptStreamFixlenArrayAllocsDoNotScale(t *testing.T) {
	for _, tc := range []struct {
		name       string
		small, big []byte
	}{
		{"fp32", encodeFp32Array(t, 100), encodeFp32Array(t, 1100)},
		{"fp64", encodeFp64Array(t, 100), encodeFp64Array(t, 1100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			growth := allocGrowth(t, tc.small, tc.big, func(msg []byte) {
				var sink fpSink
				if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(&sink); err != nil {
					t.Fatalf("AcceptStream: %v", err)
				}
			})
			// 1000 extra elements: the output slice doubles a handful of times
			// and nothing else may be allocated per element.
			if growth > 16 {
				t.Fatalf("1000 extra elements cost %v extra allocations; the element read is allocating per element", growth)
			}
		})
	}
}

// TestAcceptStreamFloatScalarAllocsDoNotScale covers the scalar fp32/fp64
// fields, which took the same per-value allocation (issue #85).
func TestAcceptStreamFloatScalarAllocsDoNotScale(t *testing.T) {
	small, big := encodeFloatScalars(t, 100), encodeFloatScalars(t, 1100)
	growth := allocGrowth(t, small, big, func(msg []byte) {
		var sink fpSink
		if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(&sink); err != nil {
			t.Fatalf("AcceptStream: %v", err)
		}
	})
	if growth > 4 {
		t.Fatalf("1000 extra float fields cost %v extra allocations; the value read is allocating per field", growth)
	}
}

// TestPullFloatAllocsDoNotScale holds the pull surface to the same rule: it read
// every fp32/fp64 element and every float scalar through the same allocating
// helper.
func TestPullFloatAllocsDoNotScale(t *testing.T) {
	t.Run("fp32 array", func(t *testing.T) {
		growth := allocGrowth(t, encodeFp32Array(t, 100), encodeFp32Array(t, 1100), func(msg []byte) {
			d := sofab.NewDecoder(bytes.NewReader(msg))
			if _, err := d.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if _, err := d.ReadFloat32Array(); err != nil {
				t.Fatalf("ReadFloat32Array: %v", err)
			}
		})
		if growth > 16 {
			t.Fatalf("1000 extra elements cost %v extra allocations", growth)
		}
	})
	t.Run("fp64 array", func(t *testing.T) {
		growth := allocGrowth(t, encodeFp64Array(t, 100), encodeFp64Array(t, 1100), func(msg []byte) {
			d := sofab.NewDecoder(bytes.NewReader(msg))
			if _, err := d.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if _, err := d.ReadFloat64Array(); err != nil {
				t.Fatalf("ReadFloat64Array: %v", err)
			}
		})
		if growth > 16 {
			t.Fatalf("1000 extra elements cost %v extra allocations", growth)
		}
	})
	t.Run("scalars", func(t *testing.T) {
		growth := allocGrowth(t, encodeFloatScalars(t, 100), encodeFloatScalars(t, 1100), func(msg []byte) {
			d := sofab.NewDecoder(bytes.NewReader(msg))
			// encodeFloatScalars alternates fp32, fp64, fp32, ...
			for i := 0; ; i++ {
				f, err := d.Next()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if f.Type != sofab.TypeFixlen {
					t.Fatalf("unexpected wire type %v", f.Type)
				}
				if i%2 == 0 {
					_, err = d.Float32()
				} else {
					_, err = d.Float64()
				}
				if err != nil {
					t.Fatalf("read scalar %d: %v", i, err)
				}
			}
		})
		if growth > 4 {
			t.Fatalf("1000 extra float fields cost %v extra allocations", growth)
		}
	})
}

// TestFixlenArrayBatchedReadIsBitExact guards the batching itself: a long array
// is decoded element-for-element identically however the reader splits the
// bytes, including splits that land inside an element and inside the fill of the
// decoder's own buffer.
func TestFixlenArrayBatchedReadIsBitExact(t *testing.T) {
	const n = 1500 // > one 4096-byte bufio fill for both widths
	want32, want64 := fp32Payload(n), fp64Payload(n)
	msg32, msg64 := encodeFp32Array(t, n), encodeFp64Array(t, n)
	readers := map[string]func([]byte) io.Reader{
		"all-at-once": func(b []byte) io.Reader { return bytes.NewReader(b) },
		"one-byte":    func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
		"half-sized":  func(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) },
		"three-byte":  func(b []byte) io.Reader { return &chunkReader{b: b, n: 3} },
		"seven-byte":  func(b []byte) io.Reader { return &chunkReader{b: b, n: 7} },
	}
	for name, mk := range readers {
		t.Run("fp32/"+name, func(t *testing.T) {
			var got []float32
			d := sofab.NewDecoder(mk(msg32))
			if _, err := d.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			got, err := d.ReadFloat32Array()
			if err != nil {
				t.Fatalf("ReadFloat32Array: %v", err)
			}
			if len(got) != len(want32) {
				t.Fatalf("len = %d, want %d", len(got), len(want32))
			}
			for i := range want32 {
				if math.Float32bits(got[i]) != math.Float32bits(want32[i]) {
					t.Fatalf("element %d = %08x, want %08x", i,
						math.Float32bits(got[i]), math.Float32bits(want32[i]))
				}
			}
		})
		t.Run("fp64/"+name, func(t *testing.T) {
			d := sofab.NewDecoder(mk(msg64))
			if _, err := d.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			got, err := d.ReadFloat64Array()
			if err != nil {
				t.Fatalf("ReadFloat64Array: %v", err)
			}
			if len(got) != len(want64) {
				t.Fatalf("len = %d, want %d", len(got), len(want64))
			}
			for i := range want64 {
				if math.Float64bits(got[i]) != math.Float64bits(want64[i]) {
					t.Fatalf("element %d = %016x, want %016x", i,
						math.Float64bits(got[i]), math.Float64bits(want64[i]))
				}
			}
		})
		t.Run("stream/fp32/"+name, func(t *testing.T) {
			var got []float32
			v := collectF32{&got}
			if err := sofab.NewDecoder(mk(msg32)).AcceptStream(v); err != nil {
				t.Fatalf("AcceptStream: %v", err)
			}
			if len(got) != len(want32) {
				t.Fatalf("len = %d, want %d", len(got), len(want32))
			}
			for i := range want32 {
				if math.Float32bits(got[i]) != math.Float32bits(want32[i]) {
					t.Fatalf("element %d = %08x, want %08x", i,
						math.Float32bits(got[i]), math.Float32bits(want32[i]))
				}
			}
		})
	}
}

// TestFixlenArrayTruncatedMidBatch keeps a truncation INCOMPLETE wherever it
// falls inside a batch — the batched read must not turn a short payload into a
// silently shorter array.
func TestFixlenArrayTruncatedMidBatch(t *testing.T) {
	const n = 300
	for _, tc := range []struct {
		name string
		msg  []byte
	}{
		{"fp32", encodeFp32Array(t, n)},
		{"fp64", encodeFp64Array(t, n)},
	} {
		for _, cut := range []int{1, 2, 3, 5, 17, 400, 1001} {
			t.Run(tc.name, func(t *testing.T) {
				short := tc.msg[:len(tc.msg)-cut]
				var sink fpSink
				err := sofab.NewDecoder(bytes.NewReader(short)).AcceptStream(&sink)
				if err != sofab.ErrIncomplete {
					t.Fatalf("cut %d: err = %v, want ErrIncomplete", cut, err)
				}
			})
		}
	}
}

type collectF32 struct {
	out *[]float32
}

func (c collectF32) Unsigned(sofab.ID, uint64) error        { return nil }
func (c collectF32) Signed(sofab.ID, int64) error           { return nil }
func (c collectF32) Float32(sofab.ID, float32) error        { return nil }
func (c collectF32) Float64(sofab.ID, float64) error        { return nil }
func (c collectF32) String(sofab.ID, string) error          { return nil }
func (c collectF32) Bytes(sofab.ID, []byte) error           { return nil }
func (c collectF32) UnsignedArray(sofab.ID, []uint64) error { return nil }
func (c collectF32) SignedArray(sofab.ID, []int64) error    { return nil }
func (c collectF32) Float32Array(_ sofab.ID, v []float32) error {
	*c.out = append(*c.out, v...)
	return nil
}
func (c collectF32) Float64Array(sofab.ID, []float64) error        { return nil }
func (c collectF32) BeginSequence(sofab.ID) (sofab.Visitor, error) { return c, nil }
func (c collectF32) EndSequence() error                            { return nil }

// chunkReader hands out at most n bytes per Read, so element and buffer-fill
// boundaries land at strides the batch loop must not assume anything about.
type chunkReader struct {
	b []byte
	n int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	k := min(min(len(p), r.n), len(r.b))
	copy(p, r.b[:k])
	r.b = r.b[k:]
	return k, nil
}

// --- benchmarks (issue #85) --------------------------------------------------

func BenchmarkAcceptStreamFp32Array(b *testing.B) {
	msg := encodeFp32Array(b, 1000)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sink fpSink
		if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(&sink); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAcceptStreamFp64Array(b *testing.B) {
	msg := encodeFp64Array(b, 1000)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sink fpSink
		if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(&sink); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAcceptBytesFp32Array(b *testing.B) {
	msg := encodeFp32Array(b, 1000)
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sink fpSink
		if err := sofab.AcceptBytes(msg, &sink); err != nil {
			b.Fatal(err)
		}
	}
}
