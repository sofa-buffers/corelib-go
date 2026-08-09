package main

// Tests for the §10 perf/bench harness.
//
// The point of testing a benchmark is not the numbers — those are the machine's
// — but the WORKLOAD: BENCH_SPEC pins the two shared messages (the "typical"
// message and the 12-field perf message) down to their ids, wire types and
// values so the Go row can be compared with the C/C++/Rust/… rows. A harness
// that quietly encodes something else, or whose visitor silently drops half the
// fields, still prints a perfectly plausible MB/s figure. So every workload here
// is decoded back and checked field by field, and the reported table is checked
// for the shared shape rather than for its values.
//
// The measured loops (`bench`, `perf`) are driven through main() once each, so
// the ~1s CPU-time loops are paid once for both the subcommand dispatch and the
// functions behind it.

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// ---- helpers ---------------------------------------------------------------

// encodeToBytes runs fn against a fresh encoder over a fresh sliceWriter and
// returns the bytes it produced, restoring nothing: the globals are the
// harness's own and every test that reads them sets them first.
func encodeToBytes(t *testing.T, fn func(*sofab.Encoder)) []byte {
	t.Helper()
	w := &sliceWriter{buf: make([]byte, 0, 1024)}
	e := sofab.NewEncoder(w)
	fn(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return w.buf
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// field is one decoded field, in the order the pull parser delivered it.
type field struct {
	id   sofab.ID
	kind string
	val  string
}

// pullAll walks buf with the pull parser and records every field, descending
// into sequences with a "seq:" prefix on the ids inside them. It is deliberately
// an independent reader — not the harness's own visitors — so a visitor that
// ignores a field cannot hide it.
func pullAll(t *testing.T, buf []byte, fixKind map[sofab.ID]string) []field {
	t.Helper()
	d := sofab.NewDecoder(bytes.NewReader(buf))
	var out []field
	prefix := ""
	for {
		f, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch f.Type {
		case sofab.TypeSequenceStart:
			out = append(out, field{f.ID, prefix + "seq{", ""})
			prefix = "seq:"
			continue
		case sofab.TypeSequenceEnd:
			prefix = ""
			out = append(out, field{0, "}", ""})
			continue
		case sofab.TypeVarintUnsigned:
			v, err := d.Unsigned()
			if err != nil {
				t.Fatalf("Unsigned: %v", err)
			}
			out = append(out, field{f.ID, prefix + "u", fmt.Sprint(v)})
		case sofab.TypeVarintSigned:
			v, err := d.Signed()
			if err != nil {
				t.Fatalf("Signed: %v", err)
			}
			out = append(out, field{f.ID, prefix + "s", fmt.Sprint(v)})
		case sofab.TypeVarintArrayUnsigned:
			v, err := sofab.ReadUnsignedArray[uint64](d)
			if err != nil {
				t.Fatalf("ReadUnsignedArray: %v", err)
			}
			out = append(out, field{f.ID, prefix + "ua", fmt.Sprint(v)})
		case sofab.TypeVarintArraySigned:
			v, err := sofab.ReadSignedArray[int64](d)
			if err != nil {
				t.Fatalf("ReadSignedArray: %v", err)
			}
			out = append(out, field{f.ID, prefix + "sa", fmt.Sprint(v)})
		case sofab.TypeFixlenArray:
			v, err := d.ReadFloat64Array()
			if err != nil {
				t.Fatalf("ReadFloat64Array: %v", err)
			}
			out = append(out, field{f.ID, prefix + "f64a", fmt.Sprint(v)})
		case sofab.TypeFixlen:
			// A typed read consumes the field, so the reader is chosen from the
			// schema the workload declares rather than by trial: the caller says
			// which of fp32/fp64/string each fixlen id is.
			out = append(out, field{f.ID, prefix + "fix", fixlenValue(t, d, fixKind[f.ID])})
		default:
			t.Fatalf("unexpected wire type %d at id %d", f.Type, f.ID)
		}
	}
	return out
}

// fixlenValue reads the current fixlen field as the declared kind and renders
// it canonically. An unmapped id is a workload that grew a field the test does
// not know about — a failure, not something to guess at.
func fixlenValue(t *testing.T, d *sofab.Decoder, kind string) string {
	t.Helper()
	switch kind {
	case "f32":
		v, err := d.Float32()
		if err != nil {
			t.Fatalf("Float32: %v", err)
		}
		return fmt.Sprintf("f32:%08x", math.Float32bits(v))
	case "f64":
		v, err := d.Float64()
		if err != nil {
			t.Fatalf("Float64: %v", err)
		}
		return fmt.Sprintf("f64:%016x", math.Float64bits(v))
	case "str":
		s, err := d.String()
		if err != nil {
			t.Fatalf("String: %v", err)
		}
		return "str:" + s
	}
	t.Fatalf("fixlen field %v has no declared kind in this workload", d.Field().ID)
	return ""
}

// The fixlen ids each shared workload declares, and as what.
var (
	typicalFix = map[sofab.ID]string{4: "f32", 5: "str"}
	perfFix    = map[sofab.ID]string{6: "f32", 7: "f64", 8: "str"}
)

func wantFields(t *testing.T, got []field, want []field) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("field count = %d, want %d\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ---- workload content ------------------------------------------------------

func TestMakeSrcIsTheSharedSequence(t *testing.T) {
	src = [n]uint64{}
	makeSrc()
	if src[0] != 0 {
		t.Errorf("src[0] = %d, want 0", src[0])
	}
	// The multiplier is the shared constant every language's row uses; a change
	// to it silently makes the Go row incomparable with the others.
	for _, i := range []int{1, 2, 500, n - 1} {
		if want := uint64(i) * 0x9E3779B97F4A7C15; src[i] != want {
			t.Errorf("src[%d] = %d, want %d", i, src[i], want)
		}
	}
}

func TestEncodeTypicalRoundTrips(t *testing.T) {
	buf := encodeToBytes(t, encodeTypical)
	wantFields(t, pullAll(t, buf, typicalFix), []field{
		{1, "u", "3735928559"},
		{2, "s", "-12345"},
		{3, "u", "1"}, // bool
		{4, "fix", fmt.Sprintf("f32:%08x", math.Float32bits(3.14159))},
		{5, "fix", "str:sofab"},
		{6, "ua", "[10 20 30 40]"},
		{7, "seq{", ""},
		{1, "seq:u", "99"},
		{2, "seq:s", "-7"},
		{0, "}", ""},
	})
}

func TestPerfEncodeRoundTrips(t *testing.T) {
	buf := encodeToBytes(t, perfEncode)
	wantFields(t, pullAll(t, buf, perfFix), []field{
		{1, "u", "3735928559"},
		{2, "s", "-12345"},
		{3, "u", "81985529216486895"},
		{4, "s", "-5000000000000"},
		{5, "u", "1"}, // bool
		{6, "fix", fmt.Sprintf("f32:%08x", math.Float32bits(3.14159))},
		{7, "fix", fmt.Sprintf("f64:%016x", math.Float64bits(2.718281828459045))},
		{8, "fix", "str:" + perfString},
		{9, "ua", "[1000000 2000000 3000000 4000000 5000000 6000000 7000000 8000000]"},
		{10, "sa", "[-100000 -200000 -300000 -400000 -500000 -600000 -700000 -800000]"},
		{11, "f64a", fmt.Sprint(perfFp64[:])},
		{12, "seq{", ""},
		{1, "seq:u", "99"},
		{2, "seq:s", "-7"},
		{0, "}", ""},
	})
}

// The u64-array workload must really carry all n elements: a harness that
// encoded a shorter array would report a throughput for a message that is not
// the shared one.
func TestEncodeU64ArrayCarriesAllElements(t *testing.T) {
	setupEncodeU64()
	run_encode_u64_array()
	d := sofab.NewDecoder(bytes.NewReader(sw.buf))
	f, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.ID != 1 || f.Type != sofab.TypeVarintArrayUnsigned {
		t.Fatalf("header = id %d type %d, want id 1 type %d", f.ID, f.Type, sofab.TypeVarintArrayUnsigned)
	}
	got, err := sofab.ReadUnsignedArray[uint64](d)
	if err != nil {
		t.Fatalf("ReadUnsignedArray: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len = %d, want %d", len(got), n)
	}
	for i := range got {
		if got[i] != src[i] {
			t.Fatalf("element %d = %d, want %d", i, got[i], src[i])
		}
	}
	if _, err := d.Next(); err != io.EOF {
		t.Errorf("trailing data after the array: %v", err)
	}
}

// ---- setup + measured entry points -----------------------------------------

func TestSetupDecodeU64PreparesADecodableBuffer(t *testing.T) {
	setupDecodeU64()
	if len(decBuf) == 0 || dec == nil {
		t.Fatalf("setup left decBuf=%d dec=%v", len(decBuf), dec)
	}
	before := sink
	run_decode_u64_array()
	// The visitor folds src[0]+src[n-1]; src[0] is 0, so the delta is exactly
	// the last element — an assertion the decode really reached the array.
	if want := src[0] + src[n-1]; sink-before != want {
		t.Errorf("sink delta = %d, want %d", sink-before, want)
	}
}

func TestSetupDecodeTypicalPreparesADecodableBuffer(t *testing.T) {
	setupDecodeTypical()
	if len(decBuf) == 0 || dec == nil {
		t.Fatalf("setup left decBuf=%d dec=%v", len(decBuf), dec)
	}
	before := sink
	run_decode_typical()
	// 0xDEADBEEF + 1 (bool) + (-12345) + 3 (float32 truncated) + 5 (len "sofab")
	// + 10 (arr16[0]) + 99 + (-7), all in uint64 wrap-around arithmetic.
	neg12345, neg7 := int64(-12345), int64(-7)
	want := uint64(0xDEADBEEF) + 1 + uint64(neg12345) + 3 + 5 + 10 + 99 + uint64(neg7)
	if sink-before != want {
		t.Errorf("sink delta = %d, want %d", sink-before, want)
	}
}

// setupEncodeTypical + run_encode_typical are the Callgrind single-shot pair;
// they must leave exactly one encoded message in the writer.
func TestRunEncodeTypicalProducesOneMessage(t *testing.T) {
	setupEncodeTypical()
	run_encode_typical()
	one := len(sw.buf)
	if one == 0 {
		t.Fatal("no bytes produced")
	}
	wantFields(t, pullAll(t, sw.buf, typicalFix), pullAll(t, encodeToBytes(t, encodeTypical), typicalFix))
}

// The perf visitor must observe every one of the 12 fields, nested scope
// included: it is what stops the decode loop from being optimized into a skip.
func TestPerfVisitorFoldsEveryField(t *testing.T) {
	buf := encodeToBytes(t, perfEncode)
	counts := &countingPerfVisitor{}
	if err := sofab.AcceptBytes(buf, counts); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	// 5 scalar ints + fp32 + fp64 + string + 3 arrays at the top level, then the
	// two ints inside the sequence.
	if counts.n != 13 {
		t.Errorf("visitor saw %d values, want 13", counts.n)
	}
	before := sink
	if err := sofab.AcceptBytes(buf, perfVisitor{}); err != nil {
		t.Fatalf("AcceptBytes(perfVisitor): %v", err)
	}
	if sink == before {
		t.Error("perfVisitor folded nothing into sink")
	}
}

type countingPerfVisitor struct {
	baseVisitor
	n int
}

func (c *countingPerfVisitor) Unsigned(sofab.ID, uint64) error        { c.n++; return nil }
func (c *countingPerfVisitor) Signed(sofab.ID, int64) error           { c.n++; return nil }
func (c *countingPerfVisitor) Float32(sofab.ID, float32) error        { c.n++; return nil }
func (c *countingPerfVisitor) Float64(sofab.ID, float64) error        { c.n++; return nil }
func (c *countingPerfVisitor) String(sofab.ID, string) error          { c.n++; return nil }
func (c *countingPerfVisitor) UnsignedArray(sofab.ID, []uint64) error { c.n++; return nil }
func (c *countingPerfVisitor) SignedArray(sofab.ID, []int64) error    { c.n++; return nil }
func (c *countingPerfVisitor) Float64Array(sofab.ID, []float64) error { c.n++; return nil }
func (c *countingPerfVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) {
	return c, nil
}

// baseVisitor is what every workload visitor embeds, so it decides what happens
// to the field kinds a workload does NOT override. It has to accept all of them:
// a base arm that returned an error would abort the decode partway and the
// measured loop would be timing a truncated walk. Drive one message carrying
// every kind through the bare base and require a clean COMPLETE.
func TestBaseVisitorAcceptsEveryFieldKind(t *testing.T) {
	buf := encodeToBytes(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 7)
		e.WriteSigned(2, -7)
		e.WriteFloat32(3, 1.5)
		e.WriteFloat64(4, 2.5)
		e.WriteString(5, "s")
		e.WriteBytes(6, []byte{0xAA})
		sofab.WriteUnsignedArray(e, 7, []uint64{1, 2})
		sofab.WriteSignedArray(e, 8, []int64{-1, -2})
		e.WriteFloat32Array(9, []float32{1, 2})
		e.WriteFloat64Array(10, []float64{1, 2})
		e.WriteSequenceBeginLazy(11)
		e.WriteUnsigned(1, 1)
		e.WriteSequenceEnd()
	})
	if err := sofab.AcceptBytes(buf, baseVisitor{}); err != nil {
		t.Fatalf("baseVisitor rejected a field kind: %v", err)
	}
}

// ---- the clock and the loop ------------------------------------------------

func TestCPUNowIsMonotonicAndAdvances(t *testing.T) {
	t0 := cpuNow()
	if t0 <= 0 {
		t.Fatalf("cpuNow() = %v, want a positive process CPU time", t0)
	}
	// Burn measurable CPU (not wall-clock sleep: the clock is getrusage).
	var x uint64
	for i := 0; i < 50_000_000; i++ {
		x += uint64(i) * 3
	}
	sink += x
	t1 := cpuNow()
	if t1 < t0 {
		t.Errorf("cpuNow() went backwards: %v then %v", t0, t1)
	}
}

func TestCalibrateBatchSpansBatchSeconds(t *testing.T) {
	calls := 0
	var x uint64
	batch := calibrateBatch(func() {
		calls++
		for i := 0; i < 20_000; i++ {
			x += uint64(i)
		}
	})
	sink += x
	if batch < 1 {
		t.Fatalf("calibrateBatch = %d, want >= 1", batch)
	}
	if calls < batch {
		t.Errorf("fn called %d times, want at least the returned batch %d", calls, batch)
	}
}

func TestTimeLoopReportsPositiveThroughput(t *testing.T) {
	var x uint64
	mbs := timeLoop(func() {
		for i := 0; i < 50_000; i++ {
			x += uint64(i)
		}
	}, 1024)
	sink += x
	if !(mbs > 0) || math.IsInf(mbs, 0) || math.IsNaN(mbs) {
		t.Errorf("timeLoop = %v, want a finite positive MB/s", mbs)
	}
}

func TestPerfReportPrintsTheSharedFields(t *testing.T) {
	out := captureStdout(t, func() {
		perfReport("serialize (stream API)", perfResult{iters: 7, nsOp: 12.25, mbS: 99.5}, 128)
	})
	for _, want := range []string{
		"--- perf: serialize (stream API) ---",
		"iterations    : 7",
		"message size  : 128 bytes",
		"cycles/op     :",
		"CPU time/op   : 12.2 ns",
		"throughput    : 99.5 MB/s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("perfReport output missing %q\n%s", want, out)
		}
	}
}

// ---- the two subcommands that measure --------------------------------------

// Driven through main() so the dispatch and the ~1s loops behind it are paid
// once, not twice.
func TestMainBenchPrintsTheSharedTable(t *testing.T) {
	out := captureStdout(t, func() { runMain(t, "bench") })
	for _, want := range []string{
		"=== SofaBuffers Go throughput (CPU time, MB/s) ===",
		"Workload",
		"encode: u64 array (1000)",
		"encode: typical message",
		"decode: u64 array (1000)",
		"decode: typical message",
		"MB = 1e6 bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bench output missing %q\n%s", want, out)
		}
	}
	// Every row must carry a real number, not a 0.00 placeholder from a
	// workload that never ran.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "encode:") && !strings.HasPrefix(line, "decode:") {
			continue
		}
		var mbs float64
		if _, err := fmt.Sscanf(strings.TrimSpace(line[26:]), "%g", &mbs); err != nil {
			t.Errorf("row %q has no parseable MB/s: %v", line, err)
			continue
		}
		if !(mbs > 0) {
			t.Errorf("row %q reports non-positive throughput %v", line, mbs)
		}
	}
}

func TestMainPerfPrintsBothHalves(t *testing.T) {
	out := captureStdout(t, func() { runMain(t, "perf") })
	for _, want := range []string{
		"=== SofaBuffers Go per-op cost",
		"--- perf: serialize (stream API) ---",
		"--- perf: deserialize (stream API) ---",
		"iterations    :",
		"CPU time/op   :",
		"throughput    :",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("perf output missing %q\n%s", want, out)
		}
	}
}

// ---- the Callgrind single-shot verbs ---------------------------------------

// runMain invokes main() with the given argv tail. Only verbs that do NOT exit
// may be passed; the exiting ones are covered by TestCLIExitCodes below.
func runMain(t *testing.T, args ...string) {
	t.Helper()
	saved := os.Args
	os.Args = append([]string{"perfbench"}, args...)
	defer func() { os.Args = saved }()
	main()
}

func TestMainSingleWorkloadVerbs(t *testing.T) {
	// main() reports "sink=… used=…" on stderr for the Callgrind harness; the
	// assertion here is that each verb runs its workload and leaves bytes
	// behind, which `used` is derived from.
	for _, verb := range []string{
		"encode_u64_array", "encode_typical", "decode_u64_array", "decode_typical",
	} {
		sw = nil
		before := sink
		runMain(t, verb)
		if sw == nil || len(sw.buf) == 0 {
			t.Errorf("%s: produced no bytes", verb)
		}
		if strings.HasPrefix(verb, "decode_") && sink == before {
			t.Errorf("%s: decoded nothing into sink", verb)
		}
	}
}

// The two os.Exit(1) arms cannot be exercised in-process, so the CLI contract
// the Callgrind harness depends on — an unknown or missing verb is a failure,
// a known one is a success — is checked on the real binary.
func TestCLIExitCodes(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	bin := filepath.Join(t.TempDir(), "perfbench")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		args   []string
		wantOK bool
	}{
		{nil, false},                       // no verb
		{[]string{"nonsense"}, false},      // unknown verb
		{[]string{"encode_typical"}, true}, // a known single-shot verb
		{[]string{"decode_typical"}, true}, //
	} {
		err := exec.Command(bin, tc.args...).Run()
		if tc.wantOK && err != nil {
			t.Errorf("%v: exited %v, want success", tc.args, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("%v: exited 0, want a non-zero status", tc.args)
		}
	}
}

// ---- the fixed-capacity writer ---------------------------------------------

func TestSliceWriterAppendsEverything(t *testing.T) {
	w := &sliceWriter{buf: make([]byte, 0, 4)}
	n, err := w.Write([]byte("abc"))
	if n != 3 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, err = w.Write([]byte("defgh")); n != 5 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if string(w.buf) != "abcdefgh" {
		t.Errorf("buf = %q, want %q", w.buf, "abcdefgh")
	}
}
