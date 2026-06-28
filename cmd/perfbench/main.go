// Command perfbench is the Go side of the SofaBuffers cross-language comparison.
//
// It mirrors bench/{c,cpp}/{bench,perf}.* and corelib-rs/benches/{bench,perf}.rs:
// same workloads, data, ids and values, printed in the same shared format so the
// implementations can be compared directly. Subcommands:
//
//	bench   throughput (MB/s) table over a ~1s process-CPU-time loop
//	perf    per-op cost (CPU time/op ns + MB/s) for the 12-field perf message
//
// The single-workload subcommands (encode_u64_array, …) run one noinline run_*
// function once (setup excluded) for the Callgrind harness, which toggles
// collection on "main.run_<workload>" to match the C/C++/Rust toggle names.
package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"syscall"

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
func (baseVisitor) EndSequence() error                              { return nil }

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

// cpuNow returns process CPU time in seconds (user + system), not wall-clock —
// the Go analogue of the C tool's clock() / Rust's CLOCK_PROCESS_CPUTIME_ID, so
// throughput is measured on the same basis as every other corelib.
func cpuNow() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	sec := float64(ru.Utime.Sec) + float64(ru.Stime.Sec)
	usec := float64(ru.Utime.Usec) + float64(ru.Stime.Usec)
	return sec + usec/1e6
}

// timeLoop runs fn for ~1s of CPU time (after a warmup) and returns throughput
// in MB/s (MB = 1e6 bytes) for messages of the given byte size.
func timeLoop(fn func(), msgBytes int) float64 {
	fn() // warmup
	t0 := cpuNow()
	iters := 0
	var el float64
	for {
		fn()
		iters++
		el = cpuNow() - t0
		if el >= 1.0 {
			break
		}
	}
	return float64(msgBytes) * float64(iters) / el / 1e6
}

// runBench reports throughput (MB/s) for each workload in the shared
// cross-language table format. Encode constructs a fresh encoder per message
// (idiomatic Go); decode reuses a pre-encoded buffer and constructs a fresh
// decoder per message.
func runBench() {
	makeSrc()

	// pre-encode the two messages once for the decode loops.
	setupEncodeU64()
	run_encode_u64_array()
	decU64 := append([]byte(nil), sw.buf...)
	setupEncodeTypical()
	run_encode_typical()
	decTyp := append([]byte(nil), sw.buf...)

	encU64 := timeLoop(func() {
		sw = &sliceWriter{buf: make([]byte, 0, 16*1024)}
		enc = sofab.NewEncoder(sw)
		sofab.WriteUnsignedArray(enc, 1, src[:])
		enc.Flush()
	}, len(decU64))

	encTyp := timeLoop(func() {
		sw = &sliceWriter{buf: make([]byte, 0, 256)}
		enc = sofab.NewEncoder(sw)
		encodeTypical(enc)
		enc.Flush()
	}, len(decTyp))

	decU64MBs := timeLoop(func() {
		dec = sofab.NewDecoder(bytes.NewReader(decU64))
		run_decode_u64_array()
	}, len(decU64))

	decTypMBs := timeLoop(func() {
		dec = sofab.NewDecoder(bytes.NewReader(decTyp))
		run_decode_typical()
	}, len(decTyp))

	fmt.Println("=== SofaBuffers Go throughput (CPU time, MB/s) ===")
	fmt.Printf("%-26s %12s\n", "Workload", "MB/s")
	fmt.Printf("%-26s %12s\n", "--------", "----")
	fmt.Printf("%-26s %12.2f\n", "encode: u64 array (1000)", encU64)
	fmt.Printf("%-26s %12.2f\n", "encode: typical message", encTyp)
	fmt.Printf("%-26s %12.2f\n", "decode: u64 array (1000)", decU64MBs)
	fmt.Printf("%-26s %12.2f\n", "decode: typical message", decTypMBs)
	fmt.Println("\nMB = 1e6 bytes. ~1s CPU-time loop per workload.")
}

// ---- per-op (perf) -----------------------------------------------------------
//
// Mirrors corelib-rs/benches/perf.rs and bench/{c,cpp}/perf.*: the identical
// 12-field message (same ids, types and values), measured over a ~1s CPU-time
// loop and printed in the shared per-op format. Go has no portable hardware
// cycle counter, so cycles/op is reported unavailable (like Java/C#/TS).

const perfString = "perf-benchmark-message"

var (
	perfSamples = [8]uint32{1000000, 2000000, 3000000, 4000000, 5000000, 6000000, 7000000, 8000000}
	perfDeltas  = [8]int32{-100000, -200000, -300000, -400000, -500000, -600000, -700000, -800000}
	perfFp64    = [4]float64{3.14159265, 6.28318530, 9.42477795, 12.56637060}
)

func perfEncode(e *sofab.Encoder) {
	e.WriteUnsigned(1, 0xDEADBEEF)
	e.WriteSigned(2, -12345)
	e.WriteUnsigned(3, 0x0123456789ABCDEF)
	e.WriteSigned(4, -5000000000000)
	e.WriteBool(5, true)
	e.WriteFloat32(6, 3.14159)
	e.WriteFloat64(7, 2.718281828459045)
	e.WriteString(8, perfString)
	sofab.WriteUnsignedArray(e, 9, perfSamples[:])
	sofab.WriteSignedArray(e, 10, perfDeltas[:])
	e.WriteFloat64Array(11, perfFp64[:])
	e.WriteSequenceBegin(12)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
}

// perfVisitor folds every value into the global sink so nothing is elided; it
// returns itself for nested sequences so nested fields are folded too.
type perfVisitor struct{ baseVisitor }

func (perfVisitor) Unsigned(id sofab.ID, v uint64) error { sink += v ^ uint64(id); return nil }
func (perfVisitor) Signed(id sofab.ID, v int64) error    { sink += uint64(v) ^ uint64(id); return nil }
func (perfVisitor) Float32(_ sofab.ID, v float32) error {
	sink += uint64(math.Float32bits(v))
	return nil
}
func (perfVisitor) Float64(_ sofab.ID, v float64) error { sink += math.Float64bits(v); return nil }
func (perfVisitor) String(_ sofab.ID, s string) error   { sink += uint64(len(s)); return nil }
func (perfVisitor) UnsignedArray(_ sofab.ID, a []uint64) error {
	sink += uint64(len(a))
	if len(a) > 0 {
		sink += a[0] + a[len(a)-1]
	}
	return nil
}
func (perfVisitor) SignedArray(_ sofab.ID, a []int64) error         { sink += uint64(len(a)); return nil }
func (perfVisitor) Float64Array(_ sofab.ID, a []float64) error      { sink += uint64(len(a)); return nil }
func (v perfVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) { return v, nil }

type perfResult struct {
	iters     uint64
	nsOp, mbS float64
}

func perfReport(what string, r perfResult, msgBytes int) {
	fmt.Printf("\n--- perf: %s ---\n", what)
	fmt.Printf("  iterations    : %d\n", r.iters)
	fmt.Printf("  message size  : %d bytes\n", msgBytes)
	fmt.Printf("  cycles/op     : (hardware cycle counter unavailable on this platform)\n")
	fmt.Printf("  CPU time/op   : %.1f ns  (process CPU time, not wall-clock)\n", r.nsOp)
	fmt.Printf("  throughput    : %.1f MB/s  (speedtest, MB = 1e6 bytes)\n", r.mbS)
}

func perfMeasureEncode() (perfResult, int) {
	sw = &sliceWriter{buf: make([]byte, 0, 512)}
	enc = sofab.NewEncoder(sw)
	perfEncode(enc)
	enc.Flush()
	msg := len(sw.buf)

	for i := 0; i < 1000; i++ { // warmup
		sw.buf = sw.buf[:0]
		enc = sofab.NewEncoder(sw)
		perfEncode(enc)
		enc.Flush()
	}

	var it uint64
	t0 := cpuNow()
	var el float64
	for {
		sw.buf = sw.buf[:0]
		enc = sofab.NewEncoder(sw)
		perfEncode(enc)
		enc.Flush()
		it++
		el = cpuNow() - t0
		if el >= 1.0 {
			break
		}
	}
	return perfResult{it, el / float64(it) * 1e9, float64(msg) * float64(it) / el / 1e6}, msg
}

func perfMeasureDecode(buf []byte) perfResult {
	for i := 0; i < 1000; i++ { // warmup
		_ = sofab.NewDecoder(bytes.NewReader(buf)).Accept(perfVisitor{})
	}
	var it uint64
	t0 := cpuNow()
	var el float64
	for {
		_ = sofab.NewDecoder(bytes.NewReader(buf)).Accept(perfVisitor{})
		it++
		el = cpuNow() - t0
		if el >= 1.0 {
			break
		}
	}
	return perfResult{it, el / float64(it) * 1e9, float64(len(buf)) * float64(it) / el / 1e6}
}

func runPerf() {
	fmt.Println("=== SofaBuffers Go per-op cost (cycles/op + throughput MB/s) ===")

	encR, msg := perfMeasureEncode()
	perfReport("serialize (stream API)", encR, msg)

	// pre-encode once as decode input.
	sw = &sliceWriter{buf: make([]byte, 0, 512)}
	pe := sofab.NewEncoder(sw)
	perfEncode(pe)
	pe.Flush()
	buf := append([]byte(nil), sw.buf...)

	decR := perfMeasureDecode(buf)
	perfReport("deserialize (stream API)", decR, len(buf))

	fmt.Println("\ncycles/op tracks code cost; MB/s is this machine's throughput.")
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	switch os.Args[1] {
	case "bench", "time": // "time" kept as an alias for older callers
		runBench()
		return
	case "perf":
		runPerf()
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
