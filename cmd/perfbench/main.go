// Command perfbench is the Go side of the SofaBuffers CPU-cost comparison.
//
// It mirrors test/perf/bench.c, bench.cpp and corelib-rs/benches/perf.rs: same
// workloads, data, ids and values, measured the same way (Callgrind
// --toggle-collect on the noinline run_* functions, setup excluded). Function
// names use underscores so the harness can toggle on "main.run_<workload>",
// matching the C/C++/Rust toggle names.
package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	sofab "github.com/sofa-buffers/corelib-go"
)

const n = 1000

var (
	src    [n]uint64
	arr16  = [4]uint16{10, 20, 30, 40}
	sw     *sliceWriter
	enc    *sofab.Encoder
	dec    *sofab.Decoder
	decBuf []byte
	sink   uint64
)

// sliceWriter is a fixed-capacity io.Writer (no reallocation in the measured
// region), the Go analogue of the static output buffer used by the C/C++/Rust
// benchmarks.
type sliceWriter struct{ buf []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func makeSrc() {
	for i := 0; i < n; i++ {
		src[i] = uint64(i) * 0x9E3779B97F4A7C15
	}
}

func encodeTypical(e *sofab.Encoder) {
	e.WriteUnsigned(1, 0xDEADBEEF)
	e.WriteSigned(2, -12345)
	e.WriteBool(3, true)
	e.WriteFloat32(4, 3.14159)
	e.WriteString(5, "sofab")
	sofab.WriteUnsignedArray(e, 6, arr16[:])
	e.WriteSequenceBegin(7)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
}

// ---- setup (excluded from measurement) -------------------------------------

func setupEncodeU64() {
	makeSrc()
	sw = &sliceWriter{buf: make([]byte, 0, 16*1024)}
	enc = sofab.NewEncoder(sw)
}

func setupEncodeTypical() {
	sw = &sliceWriter{buf: make([]byte, 0, 256)}
	enc = sofab.NewEncoder(sw)
}

func setupDecodeU64() {
	setupEncodeU64()
	run_encode_u64_array()
	decBuf = append([]byte(nil), sw.buf...)
	dec = sofab.NewDecoder(bytes.NewReader(decBuf))
}

func setupDecodeTypical() {
	setupEncodeTypical()
	run_encode_typical()
	decBuf = append([]byte(nil), sw.buf...)
	dec = sofab.NewDecoder(bytes.NewReader(decBuf))
}

// ---- measured workloads ----------------------------------------------------

//go:noinline
func run_encode_u64_array() {
	sofab.WriteUnsignedArray(enc, 1, src[:])
	enc.Flush()
}

//go:noinline
func run_encode_typical() {
	encodeTypical(enc)
	enc.Flush()
}

// baseVisitor is a no-op visitor; workload visitors embed it and override only
// the field kinds they care about (the generated code would do the same).
type baseVisitor struct{}

func (baseVisitor) Unsigned(sofab.ID, uint64) error                 { return nil }
func (baseVisitor) Signed(sofab.ID, int64) error                    { return nil }
func (baseVisitor) Float32(sofab.ID, float32) error                 { return nil }
func (baseVisitor) Float64(sofab.ID, float64) error                 { return nil }
func (baseVisitor) String(sofab.ID, string) error                   { return nil }
func (baseVisitor) Bytes(sofab.ID, []byte) error                    { return nil }
func (baseVisitor) UnsignedArray(sofab.ID, []uint64) error          { return nil }
func (baseVisitor) SignedArray(sofab.ID, []int64) error             { return nil }
func (baseVisitor) Float32Array(sofab.ID, []float32) error          { return nil }
func (baseVisitor) Float64Array(sofab.ID, []float64) error          { return nil }
func (b baseVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) { return b, nil }

type u64ArrayVisitor struct{ baseVisitor }

func (u64ArrayVisitor) UnsignedArray(_ sofab.ID, v []uint64) error {
	sink += v[0] + v[len(v)-1]
	return nil
}

type typicalVisitor struct{ baseVisitor }

func (typicalVisitor) Unsigned(id sofab.ID, v uint64) error {
	switch id {
	case 1:
		sink += v
	case 3: // bool encoded as unsigned 0/1
		if v != 0 {
			sink++
		}
	}
	return nil
}
func (typicalVisitor) Signed(id sofab.ID, v int64) error {
	if id == 2 {
		sink += uint64(v)
	}
	return nil
}
func (typicalVisitor) Float32(id sofab.ID, v float32) error {
	if id == 4 {
		sink += uint64(v)
	}
	return nil
}
func (typicalVisitor) String(id sofab.ID, s string) error {
	if id == 5 {
		sink += uint64(len(s))
	}
	return nil
}
func (typicalVisitor) UnsignedArray(id sofab.ID, v []uint64) error {
	if id == 6 {
		sink += v[0]
	}
	return nil
}
func (typicalVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) { return seqVisitor{}, nil }

type seqVisitor struct{ baseVisitor }

func (seqVisitor) Unsigned(id sofab.ID, v uint64) error {
	if id == 1 {
		sink += v
	}
	return nil
}
func (seqVisitor) Signed(id sofab.ID, v int64) error {
	if id == 2 {
		sink += uint64(v)
	}
	return nil
}

//go:noinline
func run_decode_u64_array() {
	_ = dec.Accept(u64ArrayVisitor{})
}

//go:noinline
func run_decode_typical() {
	_ = dec.Accept(typicalVisitor{})
}

// timeLoop runs fn for ~1s and returns throughput in MB/s for messages of the
// given byte size.
func timeLoop(fn func(), msgBytes int) float64 {
	fn() // warmup
	start := time.Now()
	iters := 0
	var el time.Duration
	for {
		fn()
		iters++
		el = time.Since(start)
		if el >= time.Second {
			break
		}
	}
	return float64(msgBytes) * float64(iters) / el.Seconds() / 1e6
}

// runTimed reports real wall-clock throughput (MB/s) for each workload. Encode
// constructs a fresh encoder per message (idiomatic Go); decode reuses a
// pre-encoded buffer and constructs a fresh decoder per message.
func runTimed() {
	makeSrc()

	// pre-encode the two messages once for the decode loops.
	setupEncodeU64()
	run_encode_u64_array()
	decU64 := append([]byte(nil), sw.buf...)
	setupEncodeTypical()
	run_encode_typical()
	decTyp := append([]byte(nil), sw.buf...)

	fmt.Printf("encode_u64_array %.2f\n", timeLoop(func() {
		sw = &sliceWriter{buf: make([]byte, 0, 16*1024)}
		enc = sofab.NewEncoder(sw)
		sofab.WriteUnsignedArray(enc, 1, src[:])
		enc.Flush()
	}, len(decU64)))

	fmt.Printf("encode_typical %.2f\n", timeLoop(func() {
		sw = &sliceWriter{buf: make([]byte, 0, 256)}
		enc = sofab.NewEncoder(sw)
		encodeTypical(enc)
		enc.Flush()
	}, len(decTyp)))

	fmt.Printf("decode_u64_array %.2f\n", timeLoop(func() {
		dec = sofab.NewDecoder(bytes.NewReader(decU64))
		run_decode_u64_array()
	}, len(decU64)))

	fmt.Printf("decode_typical %.2f\n", timeLoop(func() {
		dec = sofab.NewDecoder(bytes.NewReader(decTyp))
		run_decode_typical()
	}, len(decTyp)))
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	if os.Args[1] == "time" {
		runTimed()
		return
	}
	switch os.Args[1] {
	case "encode_u64_array":
		setupEncodeU64()
		run_encode_u64_array()
	case "encode_typical":
		setupEncodeTypical()
		run_encode_typical()
	case "decode_u64_array":
		setupDecodeU64()
		run_decode_u64_array()
	case "decode_typical":
		setupDecodeTypical()
		run_decode_typical()
	default:
		os.Exit(1)
	}
	used := 0
	if sw != nil {
		used = len(sw.buf)
	}
	fmt.Fprintf(os.Stderr, "sink=%d used=%d\n", sink, used)
}
