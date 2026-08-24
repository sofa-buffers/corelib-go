// Command perfbench is the Go side of the SofaBuffers cross-language comparison.
//
// It mirrors corelib-rs/benches/{bench,perf}.rs and bench/{c,cpp}/{bench,perf}.*:
// the same workloads, data, ids and values, printed in the same shared format
// (BENCH_SPEC) so the implementations can be compared directly. Subcommands:
//
//	bench   throughput (MB/s) table over a ~1s process-CPU-time loop
//	perf    per-op cost (CPU time/op ns + MB/s) for the 12-field perf message
//
// Every workload is one op behind a noinline run_* symbol, and both the
// throughput loop and bench/run_callgrind.sh drive that same symbol — the MB/s
// row and the Ir/op row therefore measure identical code rather than two
// hand-kept copies of it. Passing a workload verb (encode_u64_array, …) runs the
// op once warm and once collected, with setup excluded, which is what the
// Callgrind harness toggles collection around ("main.run_<workload>").
//
// The datasets are BENCH_SPEC's: the 1000-element u64 array, the small "typical"
// message, the 12-field perf message, the unbounded 1 MB blob (one-shot and
// streaming encode, chunk-fed decode) and the composite message
// that reaches the paths the flat ones never do — a wrapper array, multi-byte
// UTF-8, depth-3 nesting, an omitted all-default field and a two-byte field
// header.
//
// Read the `blob 1MB` rows against each other, not against the others: five
// bytes of that message are metadata and a million are payload, so its MB/s is
// the machine's memory bandwidth. The signal is the *difference* — one-shot to
// streaming is what the flush machinery costs — and it is best read as Callgrind
// Ir/op (bench/run_callgrind.sh), where instruction counts do not care about
// bandwidth.
package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"

	sofab "github.com/sofa-buffers/corelib-go"
)

const (
	// n is the u64-array workload's element count.
	n = 1000

	// blobLen is the `blob 1MB` payload length. The encoded message is
	// blobLen+5 bytes on every port — a 1-byte header (1<<3)|2 and a 4-byte
	// fixlen word (1000000<<3)|3 — which is the cross-port parity check.
	blobLen     = 1_000_000
	blobEncoded = blobLen + 5

	// blobChunk is the fixed buffer/chunk size BENCH_SPEC pins for the
	// streaming blob rows on every port, so they stay comparable across
	// languages: 4096, comfortably above MinOutputBuffer (20).
	blobChunk = 4096

	// compositeText is one cycle of the composite message's string field:
	// 1-, 2-, 3- and 4-byte UTF-8, the last a surrogate pair in every UTF-16
	// port. Repeated 32 times it is 320 bytes.
	compositeText  = "aä€\U0001D11E"
	compositeElems = 64
)

var (
	src      [n]uint64
	arr16    = [4]uint16{10, 20, 30, 40}
	blobSrc  []byte
	compElem [compositeElems]string

	// Output buffers for the encode workloads and input for the decode ones.
	// All of them are allocated in a workload's setup, which is excluded from
	// the measurement.
	encOut     []byte // caller-supplied one-shot output buffer
	encScratch []byte // caller-supplied streaming buffer, drained by discardSink
	decBuf     []byte // a pre-encoded message, the decode workloads' input

	// used is the encoded size of the message the last op produced (encode) or
	// consumed (decode). It is the table's `bytes` column and is reported to
	// the Callgrind harness on stderr.
	used int

	// sink and discardAcc keep the measured work observable so nothing is
	// optimized away.
	sink       uint64
	discardAcc byte
)

// must turns a benchmark's failure into a loud one: a workload that silently
// stopped encoding halfway (a mis-sized buffer, say) would otherwise print a
// perfectly plausible — and wrong — MB/s.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// ---- datasets ---------------------------------------------------------------

func makeSrc() {
	for i := 0; i < n; i++ {
		src[i] = uint64(i) * 0x9E3779B97F4A7C15
	}
}

// makeBlob builds the `blob 1MB` payload: exactly 1,000,000 bytes, so MB/s reads
// directly against the MB = 1e6 convention. Same constant as the u64 array, so
// there is one magic number in this file rather than two.
func makeBlob() {
	if blobSrc != nil {
		return
	}
	blobSrc = make([]byte, blobLen)
	for i := range blobSrc {
		blobSrc[i] = byte(uint64(i) * 0x9E3779B97F4A7C15)
	}
}

// makeCompositeElements builds the wrapper array's 64 element strings,
// "item-0" … "item-63". They are dataset content, like src and blobSrc, and are
// built once here rather than formatted inside the measured op: the row is meant
// to measure the encoder, not this language's integer formatting.
func makeCompositeElements() {
	for i := range compElem {
		compElem[i] = "item-" + strconv.Itoa(i)
	}
}

func encodeTypical(e *sofab.Encoder) {
	e.WriteUnsigned(1, 0xDEADBEEF)
	e.WriteSigned(2, -12345)
	e.WriteBool(3, true)
	e.WriteFloat32(4, 3.14159)
	e.WriteString(5, "sofab")
	sofab.WriteUnsignedArray(e, 6, arr16[:])
	e.WriteSequenceBeginLazy(7)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
}

// encodeComposite writes BENCH_SPEC's composite message — every field for a
// reason the flat datasets do not cover:
//
//   - id 1 — the suite's only wrapper array (MESSAGE_SPEC §5.1): one field
//     header per element, element id = array index, so ids 0–15 take a one-byte
//     header and 16–63 take two.
//   - id 2 — 320 UTF-8 bytes covering 1-, 2-, 3- and 4-byte sequences, so the
//     §6.4 validator runs on a payload that is not ASCII.
//   - id 3 — nesting at depth 3, so the lazy hold-back run grows past the single
//     level `typical` and `perf` reach.
//   - id 4 — equal to its declared default, so the encoder must NOT write it:
//     opened lazily, closed with nothing inside, discarded.
//   - id 130 — the suite's only two-byte field header, (130<<3)|0.
func encodeComposite(e *sofab.Encoder) {
	e.WriteSequenceBeginLazy(1)
	for i := range compElem {
		e.WriteString(sofab.ID(i), compElem[i])
	}
	e.WriteSequenceEnd()

	e.WriteString(2, compositeStr)

	e.WriteSequenceBeginLazy(3)
	e.WriteSequenceBeginLazy(1)
	e.WriteSequenceBeginLazy(1)
	e.WriteUnsigned(1, 7)
	e.WriteSequenceEnd()
	e.WriteSequenceEnd()
	e.WriteSigned(2, -1)
	e.WriteSequenceEnd()

	// The all-default field: opened and closed empty, so the lazy hold-back
	// discards it and nothing reaches the wire.
	e.WriteSequenceBeginLazy(4)
	e.WriteSequenceEnd()

	e.WriteUnsigned(130, 0xDEADBEEF)
}

// compositeStr is the composite message's field 2: 32 repetitions of the
// four-width UTF-8 cycle, 320 bytes, built once at program start.
var compositeStr = strings.Repeat(compositeText, 32)

// ---- visitors ---------------------------------------------------------------

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

// foldVisitor folds every value it is handed into the global sink so no part of
// a decode is optimized away, and returns itself for a nested sequence so nested
// fields are folded too. It is the destination for the perf, composite and blob
// decodes — the workloads whose point is that *everything* is materialized.
type foldVisitor struct{ baseVisitor }

func (foldVisitor) Unsigned(id sofab.ID, v uint64) error { sink += v ^ uint64(id); return nil }
func (foldVisitor) Signed(id sofab.ID, v int64) error    { sink += uint64(v) ^ uint64(id); return nil }
func (foldVisitor) Float32(_ sofab.ID, v float32) error {
	sink += uint64(math.Float32bits(v))
	return nil
}
func (foldVisitor) Float64(_ sofab.ID, v float64) error { sink += math.Float64bits(v); return nil }
func (foldVisitor) String(_ sofab.ID, s string) error   { sink += uint64(len(s)); return nil }
func (foldVisitor) Bytes(_ sofab.ID, b []byte) error    { sink += uint64(len(b)); return nil }
func (foldVisitor) UnsignedArray(_ sofab.ID, a []uint64) error {
	sink += uint64(len(a))
	if len(a) > 0 {
		sink += a[0] + a[len(a)-1]
	}
	return nil
}
func (foldVisitor) SignedArray(_ sofab.ID, a []int64) error         { sink += uint64(len(a)); return nil }
func (foldVisitor) Float64Array(_ sofab.ID, a []float64) error      { sink += uint64(len(a)); return nil }
func (v foldVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) { return v, nil }

// ---- the streaming sink and the chunk-feeding reader ------------------------

// discardSink is the flush sink for the streaming blob rows. BENCH_SPEC is
// explicit that it consumes and DISCARDS: accumulating would add to the
// streaming row a copy the one-shot row never pays, and I/O is not deterministic
// under Callgrind. Folding one byte per call is the minimum that keeps the call
// from being optimized away.
//
// It returns without installing a buffer, which is the §5.1 way of saying "I
// copied": the encoder resumes writing into the same buffer at offset 0.
func discardSink(_ *sofab.Encoder, b []byte) error {
	if len(b) > 0 {
		discardAcc ^= b[0]
	}
	return nil
}

// chunkReader hands out at most chunk bytes per Read, so a decode driven off it
// is fed in chunks the way a socket delivers them — BENCH_SPEC's "fed in
// 4096-byte chunks" for the blob decode.
type chunkReader struct {
	buf   []byte
	chunk int
	pos   int
}

func (r *chunkReader) reset() { r.pos = 0 }

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	k := len(r.buf) - r.pos
	if k > r.chunk {
		k = r.chunk
	}
	if k > len(p) {
		k = len(p)
	}
	copy(p[:k], r.buf[r.pos:r.pos+k])
	r.pos += k
	return k, nil
}

var chunks chunkReader

// ---- setup (excluded from measurement) -------------------------------------

func setupEncodeU64() {
	makeSrc()
	encOut = make([]byte, n*11+16)
}

func setupEncodeTypical() {
	encOut = make([]byte, 256)
}

// setupEncodeBlobOneShot sizes the caller buffer by hand at 1,000,005 bytes.
// BENCH_SPEC says so outright: this schema is unbounded, so a generated MAX_SIZE
// would be the configured ceiling rather than a size the message cannot exceed.
func setupEncodeBlobOneShot() {
	makeBlob()
	encOut = make([]byte, blobEncoded)
}

func setupEncodeBlobStreaming() {
	makeBlob()
	encScratch = make([]byte, blobChunk)
}

func setupEncodeComposite() {
	makeCompositeElements()
	encOut = make([]byte, 4096)
}

// encodeOnce runs fn against a fresh encoder over a caller-supplied buffer and
// returns the message. Used by the decode setups, never inside a measured op.
func encodeOnce(buf []byte, fn func(*sofab.Encoder)) []byte {
	e, err := sofab.NewEncoderBuffer(buf, 0)
	must(err)
	fn(e)
	must(e.Flush())
	return append([]byte(nil), e.Bytes()...)
}

func setupDecodeU64() {
	makeSrc()
	decBuf = encodeOnce(make([]byte, n*11+16), func(e *sofab.Encoder) {
		must(sofab.WriteUnsignedArray(e, 1, src[:]))
	})
	used = len(decBuf)
}

func setupDecodeTypical() {
	decBuf = encodeOnce(make([]byte, 256), encodeTypical)
	used = len(decBuf)
}

func setupDecodeBlob() {
	makeBlob()
	decBuf = encodeOnce(make([]byte, blobEncoded), func(e *sofab.Encoder) {
		must(e.WriteBytes(1, blobSrc))
	})
	if len(decBuf) != blobEncoded {
		panic(fmt.Sprintf("blob 1MB encodes to %d bytes, want %d (cross-port parity check)",
			len(decBuf), blobEncoded))
	}
	chunks = chunkReader{buf: decBuf, chunk: blobChunk}
	used = len(decBuf)
}

func setupDecodeComposite() {
	makeCompositeElements()
	decBuf = encodeOnce(make([]byte, 4096), encodeComposite)
	used = len(decBuf)
}

// ---- measured workloads ----------------------------------------------------
//
// Each do_* function performs exactly ONE operation, and the //go:noinline
// run_* wrapper beside it (see "the Callgrind toggle symbols" below) is the
// symbol bench/run_callgrind.sh toggles collection on ("main.run_<workload>"),
// to get one op's instructions retired — a deterministic, machine-independent
// per-op cost. The throughput loop drives the same wrappers, so the MB/s row
// and the Ir/op row can never measure different code.
//
// The split into do_* and run_* is what lets the Callgrind path warm the op up
// OUTSIDE the collected region: the warmup calls do_*, the collected call goes
// through run_*, and Callgrind counts only what runs inside the toggled symbol.
// Without it a single collected op also pays the first-touch of every heap size
// class it allocates from — measured at ~1.2k Ir for an encode and ~4.7k for a
// decode, which is 60% of the small rows. That is startup cost, exactly what the
// two-rep subtraction cancels for the JIT ports (BENCH_SPEC "Instruction cost"),
// so the native-toggle ports have to shed it too or their rows mean something
// else.

func do_encode_u64_array() {
	e, err := sofab.NewEncoderBuffer(encOut, 0)
	must(err)
	must(sofab.WriteUnsignedArray(e, 1, src[:]))
	must(e.Flush())
	used = len(e.Bytes())
}

func do_encode_typical() {
	e, err := sofab.NewEncoderBuffer(encOut, 0)
	must(err)
	encodeTypical(e)
	must(e.Flush())
	used = len(e.Bytes())
}

// do_encode_blob_oneshot is the floor for the blob rows: the whole message goes
// into one caller buffer of exactly 1,000,005 bytes, with no sink and therefore
// no flush logic at all (§5.1).
func do_encode_blob_oneshot() {
	e, err := sofab.NewEncoderBuffer(encOut, 0)
	must(err)
	must(e.WriteBytes(1, blobSrc))
	must(e.Flush())
	used = len(e.Bytes())
}

// do_encode_blob_streaming is the same megabyte through a 4096-byte
// caller-supplied buffer and ~245 flushes: every byte is copied through the
// buffer, because §5.1.6 admits no other route. Its distance from the one-shot
// row is the cost of the divisible-run path.
func do_encode_blob_streaming() {
	e, err := sofab.NewEncoderSink(encScratch, 0, discardSink)
	must(err)
	must(e.WriteBytes(1, blobSrc))
	must(e.Flush())
	used = blobEncoded
}

func do_encode_composite() {
	e, err := sofab.NewEncoderBuffer(encOut, 0)
	must(err)
	encodeComposite(e)
	must(e.Flush())
	used = len(e.Bytes())
}

func do_decode_u64_array() {
	must(sofab.NewDecoder(bytes.NewReader(decBuf)).Accept(u64ArrayVisitor{}))
}

func do_decode_typical() {
	must(sofab.NewDecoder(bytes.NewReader(decBuf)).Accept(typicalVisitor{}))
}

// do_decode_blob is the chunk-fed decode: the message arrives in 4096-byte
// pieces and is dispatched with AcceptStream, which never buffers the whole
// message — peak memory is one field, not one megabyte plus the payload.
func do_decode_blob() {
	chunks.reset()
	must(sofab.NewDecoder(&chunks).AcceptStream(foldVisitor{}))
}

func do_decode_composite() {
	must(sofab.NewDecoder(bytes.NewReader(decBuf)).Accept(foldVisitor{}))
}

// declineVisitor declines every sub-sequence: BeginSequence returning nil skips
// the scope whole — nothing in it is delivered and nothing in it is built
// (§6.0's "skip an unwanted sub-sequence whole").
type declineVisitor struct{ baseVisitor }

func (declineVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) { return nil, nil }

// do_decode_composite_skip is `decode: composite skip-all`: walk the message,
// materialize as little as the surface allows — the path a router or filter runs
// in production.
//
// It runs on the visitor surface like every other decode row, because that is
// the only decode surface there is (§5.3.1). What it measures is the skip §6.0
// makes normative: a declined sub-sequence is walked, not built. Top-level
// scalars are still delivered, so read the gap to `decode: composite` as "what
// declining the sub-trees is worth", not as a whole-message skip.
func do_decode_composite_skip() {
	must(sofab.AcceptBytes(decBuf, declineVisitor{}))
	sink++
}

// ---- the Callgrind toggle symbols -------------------------------------------
//
// One //go:noinline wrapper per workload, and nothing else in it: the name is
// the contract with bench/run_callgrind.sh (`--toggle-collect=main.run_<verb>`)
// and the pragma is what keeps the symbol from dissolving into its caller.

//go:noinline
func run_encode_u64_array() { do_encode_u64_array() }

//go:noinline
func run_encode_typical() { do_encode_typical() }

//go:noinline
func run_encode_blob_oneshot() { do_encode_blob_oneshot() }

//go:noinline
func run_encode_blob_streaming() { do_encode_blob_streaming() }

//go:noinline
//go:noinline
func run_encode_composite() { do_encode_composite() }

//go:noinline
func run_decode_u64_array() { do_decode_u64_array() }

//go:noinline
func run_decode_typical() { do_decode_typical() }

//go:noinline
func run_decode_blob() { do_decode_blob() }

//go:noinline
func run_decode_composite() { do_decode_composite() }

//go:noinline
func run_decode_composite_skip() { do_decode_composite_skip() }

// ---- the workload table -----------------------------------------------------

// workload is one row of the shared table: the CLI verb (which is also the
// Callgrind toggle suffix, "main.run_<verb>"), the BENCH_SPEC row label, the
// setup that must stay outside the measurement, the single op through a path
// the Callgrind toggle does NOT see (warm), and the same op through the toggled
// symbol (run).
type workload struct {
	verb  string
	label string
	setup func()
	warm  func()
	run   func()
}

// workloads is the whole suite, in BENCH_SPEC's printed order. bench/*.sh reads
// the verb list from `perfbench workloads`, so a row added here reaches the
// Callgrind table without editing the script.
var workloads = []workload{
	{"encode_u64_array", "encode: u64 array (1000)", setupEncodeU64, do_encode_u64_array, run_encode_u64_array},
	{"encode_typical", "encode: typical message", setupEncodeTypical, do_encode_typical, run_encode_typical},
	{"encode_blob_oneshot", "encode: blob 1MB one-shot", setupEncodeBlobOneShot, do_encode_blob_oneshot, run_encode_blob_oneshot},
	{"encode_blob_streaming", "encode: blob 1MB streaming", setupEncodeBlobStreaming, do_encode_blob_streaming, run_encode_blob_streaming},
	{"encode_composite", "encode: composite", setupEncodeComposite, do_encode_composite, run_encode_composite},
	{"decode_u64_array", "decode: u64 array (1000)", setupDecodeU64, do_decode_u64_array, run_decode_u64_array},
	{"decode_typical", "decode: typical message", setupDecodeTypical, do_decode_typical, run_decode_typical},
	{"decode_blob", "decode: blob 1MB", setupDecodeBlob, do_decode_blob, run_decode_blob},
	{"decode_composite", "decode: composite", setupDecodeComposite, do_decode_composite, run_decode_composite},
	{"decode_composite_skip", "decode: composite skip-all", setupDecodeComposite, do_decode_composite_skip, run_decode_composite_skip},
}

func findWorkload(verb string) *workload {
	for i := range workloads {
		if workloads[i].verb == verb {
			return &workloads[i]
		}
	}
	return nil
}

// ---- timing -----------------------------------------------------------------

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

// batchSeconds is how long one batch of operations should span before the
// clock is read again. cpuNow() is a getrusage syscall costing on the order of
// a microsecond, so reading it once per operation would time the clock rather
// than the codec; ~10 ms of work per read pushes that below 0.01%.
const batchSeconds = 0.01

// calibrateBatch grows a batch until it spans batchSeconds, so the single clock
// read that ends it is a rounding error against the work it timed. Calibration
// doubles as extra warmup.
func calibrateBatch(fn func()) int {
	for batch := 1; ; batch *= 2 {
		t0 := cpuNow()
		for k := 0; k < batch; k++ {
			fn()
		}
		if cpuNow()-t0 >= batchSeconds {
			return batch
		}
	}
}

// timeLoop runs fn for ~1s of CPU time (after a warmup) and returns throughput
// in MB/s (MB = 1e6 bytes) for messages of the given byte size. The clock is
// read once per batch, never per operation, so the reported time is the work
// and not the measurement.
func timeLoop(fn func(), msgBytes int) float64 {
	fn() // warmup
	batch := calibrateBatch(fn)
	t0 := cpuNow()
	iters := 0
	var el float64
	for el < 1.0 {
		for k := 0; k < batch; k++ {
			fn()
		}
		iters += batch
		el = cpuNow() - t0
	}
	return float64(msgBytes) * float64(iters) / el / 1e6
}

// runBench reports throughput (MB/s) for every workload in the shared
// cross-language table format. Each row runs its own setup first — buffers
// allocated, decode input encoded — and is then measured through the same
// single-op function the Callgrind harness toggles on.
func runBench() {
	mbs := make([]float64, len(workloads))
	for i := range workloads {
		w := &workloads[i]
		w.setup()
		w.run() // one op to establish the message size
		mbs[i] = timeLoop(w.run, used)
	}

	fmt.Println("=== SofaBuffers Go throughput (CPU time, MB/s) ===")
	fmt.Printf("%-26s %12s\n", "Workload", "MB/s")
	fmt.Printf("%-26s %12s\n", "--------", "----")
	for i := range workloads {
		fmt.Printf("%-26s %12.2f\n", workloads[i].label, mbs[i])
	}
	fmt.Println("\nMB = 1e6 bytes. ~1s CPU-time loop per workload.")
	fmt.Println("blob 1MB is bandwidth-bound: read its rows against each other, not against the rest.")
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
	e.WriteSequenceBeginLazy(12)
	e.WriteUnsigned(1, 99)
	e.WriteSigned(2, -7)
	e.WriteSequenceEnd()
}

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

// perfMeasure runs op for ~1s of CPU time after a 1000-call warmup and reports
// per-op cost and throughput for a message of msgBytes bytes.
func perfMeasure(op func(), msgBytes int) perfResult {
	for i := 0; i < 1000; i++ { // warmup
		op()
	}
	batch := calibrateBatch(op)

	var it uint64
	t0 := cpuNow()
	var el float64
	for el < 1.0 {
		for k := 0; k < batch; k++ {
			op()
		}
		it += uint64(batch)
		el = cpuNow() - t0
	}
	return perfResult{it, el / float64(it) * 1e9, float64(msgBytes) * float64(it) / el / 1e6}
}

func runPerf() {
	fmt.Println("=== SofaBuffers Go per-op cost (cycles/op + throughput MB/s) ===")

	// The fresh encoder per iteration is part of the measured op, as it is in
	// generated code: Encode() builds one, writes the message and flushes.
	out := make([]byte, 512)
	buf := encodeOnce(out, perfEncode)

	encR := perfMeasure(func() {
		e, err := sofab.NewEncoderBuffer(out, 0)
		must(err)
		perfEncode(e)
		must(e.Flush())
		sink += uint64(len(e.Bytes()))
	}, len(buf))
	perfReport("serialize (stream API)", encR, len(buf))

	decR := perfMeasure(func() {
		must(sofab.NewDecoder(bytes.NewReader(buf)).Accept(foldVisitor{}))
	}, len(buf))
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
	case "workloads":
		// The verb list bench/run_callgrind.sh and bench/profile.sh drive, so
		// the shell scripts never carry a second copy of the suite.
		for _, w := range workloads {
			fmt.Printf("%s\t%s\n", w.verb, w.label)
		}
		return
	}
	w := findWorkload(os.Args[1])
	if w == nil {
		fmt.Fprintf(os.Stderr, "unknown workload: %s\n", os.Args[1])
		os.Exit(1)
	}
	w.setup()
	w.warm() // one op outside the toggled symbol: see the do_*/run_* split above
	w.run()
	fmt.Fprintf(os.Stderr, "sink=%d used=%d acc=%d\n", sink, used, discardAcc)
}
