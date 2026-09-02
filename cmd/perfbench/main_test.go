package main

// Tests for the §10 perf/bench harness.
//
// The point of testing a benchmark is not the numbers — those are the machine's
// — but the WORKLOAD and the OUTPUT GRAMMAR: BENCH_SPEC pins every shared
// dataset down to its ids, wire types and values, and pins the table the central
// harness parses down to its row labels, so the Go rows can be compared with the
// C/C++/Rust/… rows. A harness that quietly encodes something else, or whose
// visitor silently drops half the fields, still prints a perfectly plausible
// MB/s figure; one whose row label drifted prints a number nothing will read.
// So every workload here is decoded back and checked field by field, the two
// cross-port size parity checks (170 for `perf`, 1,000,005 for `blob 1MB`) are
// asserted, and each printed row is matched against the harness's own regex.
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
	"regexp"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// ---- helpers ---------------------------------------------------------------

// encodeToBytes runs fn against a fresh encoder over a caller-supplied buffer
// and returns the bytes it produced.
func encodeToBytes(t *testing.T, fn func(*sofab.Encoder)) []byte {
	t.Helper()
	e, err := sofab.NewEncoderBuffer(make([]byte, 1<<16), 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	fn(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return append([]byte(nil), e.Bytes()...)
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

// field is one decoded field, in the order the decoder delivered it.
type field struct {
	id   sofab.ID
	kind string
	val  string
}

// walkV is an independent recording visitor — deliberately NOT one of the
// harness's own, so a visitor that ignores a field cannot hide it. Sequences are
// flattened with a "seq:" prefix on the ids inside them, which is what the flat
// workloads reach; the composite message has its own walker below.
//
// The fixlen kinds come from the workload's declared schema (fixKind) rather
// than from the wire, so a field that changed type is a mismatch rather than a
// silently different rendering.
type walkV struct {
	t       *testing.T
	out     *[]field
	fixKind map[sofab.ID]string
	prefix  string

	// The decoder delivers an aggregate in pieces (CORELIB_PLAN §6.6.3), so the
	// value a row of the expectation names is assembled HERE, on the
	// destination's own storage. That is what a generated field's arm does.
	pay  []byte
	sub  sofab.FixlenSubtype
	kind sofab.ArrayKind
	us   []uint64
	ss   []int64
	f32  []float32
	f64  []float64
}

func (v *walkV) add(id sofab.ID, kind, val string) error {
	*v.out = append(*v.out, field{id, v.prefix + kind, val})
	return nil
}

func (v *walkV) Unsigned(id sofab.ID, x uint64) error { return v.add(id, "u", fmt.Sprint(x)) }
func (v *walkV) Signed(id sofab.ID, x int64) error    { return v.add(id, "s", fmt.Sprint(x)) }

func (v *walkV) Float32(id sofab.ID, x float32) error {
	return v.add(id, "fix", fmt.Sprintf("f32:%08x", math.Float32bits(x)))
}

func (v *walkV) Float64(id sofab.ID, x float64) error {
	return v.add(id, "fix", fmt.Sprintf("f64:%016x", math.Float64bits(x)))
}

func (v *walkV) FixlenBegin(_ sofab.ID, sub sofab.FixlenSubtype, _ int) error {
	v.sub, v.pay = sub, v.pay[:0]
	return nil
}

func (v *walkV) String(id sofab.ID, total, offset int, chunk []byte) error {
	v.pay = append(v.pay, chunk...)
	if offset+len(chunk) < total {
		return nil
	}
	return v.add(id, "fix", "str:"+string(v.pay))
}

func (v *walkV) Bytes(id sofab.ID, total, offset int, chunk []byte) error {
	v.pay = append(v.pay, chunk...)
	if offset+len(chunk) < total {
		return nil
	}
	return v.add(id, "fix", fmt.Sprintf("blob:%x", v.pay))
}

func (v *walkV) ArrayBegin(_ sofab.ID, kind sofab.ArrayKind, _ int) error {
	v.kind = kind
	v.us, v.ss, v.f32, v.f64 = v.us[:0], v.ss[:0], v.f32[:0], v.f64[:0]
	return nil
}

func (v *walkV) ArrayUnsigned(_ sofab.ID, _ int, x uint64) error {
	v.us = append(v.us, x)
	return nil
}

func (v *walkV) ArraySigned(_ sofab.ID, _ int, x int64) error {
	v.ss = append(v.ss, x)
	return nil
}

func (v *walkV) ArrayFloat32(_ sofab.ID, _ int, x float32) error {
	v.f32 = append(v.f32, x)
	return nil
}

func (v *walkV) ArrayFloat64(_ sofab.ID, _ int, x float64) error {
	v.f64 = append(v.f64, x)
	return nil
}

func (v *walkV) ArrayEnd(id sofab.ID) error {
	switch v.kind {
	case sofab.ArrayUnsigned:
		return v.add(id, "ua", fmt.Sprint(v.us))
	case sofab.ArraySigned:
		return v.add(id, "sa", fmt.Sprint(v.ss))
	case sofab.ArrayFp32:
		return v.add(id, "f32a", fmt.Sprint(v.f32))
	default:
		return v.add(id, "f64a", fmt.Sprint(v.f64))
	}
}

func (v *walkV) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	*v.out = append(*v.out, field{id, v.prefix + "seq{", ""})
	return &walkV{t: v.t, out: v.out, fixKind: v.fixKind, prefix: "seq:"}, nil
}

func (v *walkV) EndSequence() error {
	*v.out = append(*v.out, field{0, "}", ""})
	return nil
}

// walkAll decodes buf through walkV and returns the fields it delivered.
func walkAll(t *testing.T, buf []byte, fixKind map[sofab.ID]string) []field {
	t.Helper()
	var out []field
	if err := sofab.AcceptBytes(buf, &walkV{t: t, out: &out, fixKind: fixKind}); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	return out
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

// The blob payload is derived from the same constant as the u64 array — one
// magic number in the suite, not two — and must be exactly 1,000,000 bytes so
// MB/s reads directly against the MB = 1e6 convention.
func TestMakeBlobIsTheSharedPayload(t *testing.T) {
	blobSrc = nil
	makeBlob()
	if len(blobSrc) != blobLen {
		t.Fatalf("len(blobSrc) = %d, want %d", len(blobSrc), blobLen)
	}
	for _, i := range []int{0, 1, 2, 4095, 500_000, blobLen - 1} {
		if want := byte(uint64(i) * 0x9E3779B97F4A7C15); blobSrc[i] != want {
			t.Errorf("blobSrc[%d] = %d, want %d", i, blobSrc[i], want)
		}
	}
}

func TestEncodeTypicalRoundTrips(t *testing.T) {
	buf := encodeToBytes(t, encodeTypical)
	wantFields(t, walkAll(t, buf, typicalFix), []field{
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
	wantFields(t, walkAll(t, buf, perfFix), []field{
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

// BENCH_SPEC: "The encoded size of the perf message (170 bytes on every
// implementation) is a quick parity check: if your perf prints a different
// message size, your encoding diverges."
func TestPerfMessageSizeIsThe170ByteParityCheck(t *testing.T) {
	if got := len(encodeToBytes(t, perfEncode)); got != 170 {
		t.Errorf("perf message encodes to %d bytes, want the cross-port 170", got)
	}
}

// The u64-array workload must really carry all n elements: a harness that
// encoded a shorter array would report a throughput for a message that is not
// the shared one.
func TestEncodeU64ArrayCarriesAllElements(t *testing.T) {
	setupEncodeU64()
	run_encode_u64_array()
	c := &u64ArrayCapture{}
	if err := sofab.AcceptBytes(encOut[:used], c); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	if c.fields != 1 {
		t.Fatalf("the message carries %d fields, want exactly the array", c.fields)
	}
	if c.id != 1 {
		t.Fatalf("array id = %d, want 1", c.id)
	}
	if len(c.got) != n {
		t.Fatalf("len = %d, want %d", len(c.got), n)
	}
	for i := range c.got {
		if c.got[i] != src[i] {
			t.Fatalf("element %d = %d, want %d", i, c.got[i], src[i])
		}
	}
}

// ---- the composite message -------------------------------------------------

// walkComposite renders a message as one line per event, with the nesting depth
// in front, so the composite's structure — a wrapper array, depth-3 nesting, an
// omitted field — can be asserted whole. Every fixlen in this message is a
// string, so no per-id kind table is needed.
func walkComposite(t *testing.T, buf []byte) []string {
	t.Helper()
	var out []string
	if err := sofab.AcceptBytes(buf, &compositeWalk{t: t, out: &out}); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	return out
}

type compositeWalk struct {
	t     *testing.T
	out   *[]string
	depth int
	pay   []byte
}

func (w *compositeWalk) add(format string, args ...any) error {
	*w.out = append(*w.out, fmt.Sprintf("%d "+format, append([]any{w.depth}, args...)...))
	return nil
}

func (w *compositeWalk) String(id sofab.ID, total, offset int, chunk []byte) error {
	w.pay = append(w.pay, chunk...)
	if offset+len(chunk) < total {
		return nil
	}
	return w.add("str %d %q", id, string(w.pay))
}

func (w *compositeWalk) FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, _ int) error {
	w.pay = w.pay[:0]
	if sub != sofab.FixlenStr {
		return w.unexpected("non-string fixlen", id)
	}
	return nil
}

func (w *compositeWalk) Unsigned(id sofab.ID, v uint64) error { return w.add("u %d %d", id, v) }
func (w *compositeWalk) Signed(id sofab.ID, v int64) error    { return w.add("s %d %d", id, v) }

func (w *compositeWalk) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	w.add("seq{ %d", id)
	w.depth++
	return w, nil
}

func (w *compositeWalk) EndSequence() error {
	w.depth--
	return w.add("}")
}

func (w *compositeWalk) unexpected(kind string, id sofab.ID) error {
	w.t.Fatalf("unexpected %s field at id %d in the composite message", kind, id)
	return nil
}

func (w *compositeWalk) Float32(id sofab.ID, _ float32) error { return w.unexpected("fp32", id) }
func (w *compositeWalk) Float64(id sofab.ID, _ float64) error { return w.unexpected("fp64", id) }

func (w *compositeWalk) Bytes(id sofab.ID, _, _ int, _ []byte) error {
	return w.unexpected("blob", id)
}

func (w *compositeWalk) ArrayBegin(id sofab.ID, _ sofab.ArrayKind, _ int) error {
	return w.unexpected("array", id)
}
func (w *compositeWalk) ArrayUnsigned(id sofab.ID, _ int, _ uint64) error {
	return w.unexpected("unsigned array", id)
}
func (w *compositeWalk) ArraySigned(id sofab.ID, _ int, _ int64) error {
	return w.unexpected("signed array", id)
}
func (w *compositeWalk) ArrayFloat32(id sofab.ID, _ int, _ float32) error {
	return w.unexpected("fp32 array", id)
}
func (w *compositeWalk) ArrayFloat64(id sofab.ID, _ int, _ float64) error {
	return w.unexpected("fp64 array", id)
}
func (w *compositeWalk) ArrayEnd(sofab.ID) error { return nil }

// u64ArrayCapture takes the one unsigned array the u64 workload writes.
type u64ArrayCapture struct {
	baseVisitor
	id     sofab.ID
	got    []uint64
	fields int
}

func (c *u64ArrayCapture) ArrayBegin(id sofab.ID, _ sofab.ArrayKind, count int) error {
	c.id, c.fields = id, c.fields+1
	c.got = make([]uint64, 0, count)
	return nil
}

func (c *u64ArrayCapture) ArrayUnsigned(_ sofab.ID, _ int, v uint64) error {
	c.got = append(c.got, v)
	return nil
}

func (c *u64ArrayCapture) Unsigned(sofab.ID, uint64) error { c.fields++; return nil }
func (c *u64ArrayCapture) Signed(sofab.ID, int64) error    { c.fields++; return nil }

func (c *u64ArrayCapture) FixlenBegin(sofab.ID, sofab.FixlenSubtype, int) error {
	c.fields++
	return nil
}

// The composite message is the only workload exercising the wrapper-array form,
// multi-byte UTF-8, depth-3 nesting, an omitted all-default field and a two-byte
// field header — so each of those is asserted, not just the total size.
func TestEncodeCompositeHasEveryPathItIsThereFor(t *testing.T) {
	makeCompositeElements()
	got := walkComposite(t, encodeToBytes(t, encodeComposite))

	var want []string
	// id 1: the wrapper array — one header per element, element id = index.
	want = append(want, "0 seq{ 1")
	for i := 0; i < compositeElems; i++ {
		want = append(want, fmt.Sprintf("1 str %d %q", i, fmt.Sprintf("item-%d", i)))
	}
	want = append(want, "0 }")
	// id 2: 320 UTF-8 bytes over 1-, 2-, 3- and 4-byte sequences.
	want = append(want, fmt.Sprintf("0 str 2 %q", strings.Repeat(compositeText, 32)))
	// id 3: { 1: { 1: { 1: unsigned 7 } }, 2: signed -1 } — depth 3.
	want = append(want,
		"0 seq{ 3",
		"1 seq{ 1",
		"2 seq{ 1",
		"3 u 1 7",
		"2 }",
		"1 }",
		"1 s 2 -1",
		"0 }",
	)
	// id 4 is equal to its default and must NOT appear at all.
	// id 130: the suite's only two-byte field header.
	want = append(want, "0 u 130 3735928559")

	if len(got) != len(want) {
		t.Fatalf("composite has %d events, want %d\ngot:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, line := range got {
		if strings.HasPrefix(line, "0 seq{ 4") {
			t.Error("field 4 equals its default and must be omitted (§2), but it is on the wire")
		}
	}
}

// The composite string field must really be the four-width UTF-8 cycle: 320
// bytes carrying a 1-, a 2-, a 3- and a 4-byte sequence, the last a surrogate
// pair in every UTF-16 port. A port that silently used ASCII would not be
// running the §6.4 validator on anything interesting.
func TestCompositeStringCoversAllFourUTF8Widths(t *testing.T) {
	if len(compositeStr) != 320 {
		t.Errorf("composite string is %d bytes, want 320", len(compositeStr))
	}
	widths := map[int]bool{}
	for _, r := range compositeText {
		widths[len(string(r))] = true
	}
	for _, w := range []int{1, 2, 3, 4} {
		if !widths[w] {
			t.Errorf("composite string has no %d-byte UTF-8 sequence", w)
		}
	}
}

// The composite's encoded size is its cross-port parity check, as 170 is the
// perf message's: every port must land on the same number for the same message.
func TestCompositeEncodedSizeIsTheParityCheck(t *testing.T) {
	makeCompositeElements()
	if got := len(encodeToBytes(t, encodeComposite)); got != 956 {
		t.Errorf("composite encodes to %d bytes, want the cross-port 956", got)
	}
}

// ---- the blob workloads ----------------------------------------------------

// 1,000,005 = a 1-byte header, a 4-byte fixlen word and a megabyte of payload.
// BENCH_SPEC makes it a parity check, and the one-shot row's caller buffer is
// sized by hand to exactly that.
func TestBlobEncodedSizeIsTheParityCheck(t *testing.T) {
	setupEncodeBlobOneShot()
	run_encode_blob_oneshot()
	if used != blobEncoded {
		t.Fatalf("blob 1MB encodes to %d bytes, want %d", used, blobEncoded)
	}
	if len(encOut) != blobEncoded {
		t.Errorf("one-shot caller buffer is %d bytes, want exactly %d", len(encOut), blobEncoded)
	}
	c := &blobCapture{}
	if err := sofab.AcceptBytes(encOut[:used], c); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	if c.id != 1 {
		t.Fatalf("blob id = %d, want 1", c.id)
	}
	if !bytes.Equal(c.got, blobSrc) {
		t.Error("the decoded blob payload is not the one that was encoded")
	}
}

// blobCapture takes the one blob field the workload writes, copying it out of
// the callback as §6.7 requires of any caller that keeps a value.
type blobCapture struct {
	baseVisitor
	id  sofab.ID
	got []byte
}

func (c *blobCapture) FixlenBegin(id sofab.ID, _ sofab.FixlenSubtype, total int) error {
	c.id, c.got = id, make([]byte, 0, total)
	return nil
}

func (c *blobCapture) Bytes(_ sofab.ID, _, _ int, chunk []byte) error {
	c.got = append(c.got, chunk...)
	return nil
}

// capturingSink is the test's stand-in for discardSink: it keeps what it is
// handed so the streaming rows can be checked to produce the same wire bytes as
// the one-shot row, and records the largest single hand-over — which is what
// proves no byte outside the installed buffer ever reaches it (§5.1.6).
type capturingSink struct {
	got     []byte
	calls   int
	largest int
}

func (c *capturingSink) fn(_ *sofab.Encoder, b []byte) error {
	c.got = append(c.got, b...)
	c.calls++
	if len(b) > c.largest {
		c.largest = len(b)
	}
	return nil
}

// The two encode: blob rows must both put the SAME message on the wire — the
// rows differ in how the bytes get there, not in what they are.
func TestBlobStreamingRowsProduceTheOneShotBytes(t *testing.T) {
	makeBlob()
	// The one-shot reference: a caller buffer of exactly 1,000,005 bytes, sized
	// by hand as BENCH_SPEC requires, and no sink.
	one, err := sofab.NewEncoderBuffer(make([]byte, blobEncoded), 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	if err := one.WriteBytes(1, blobSrc); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if err := one.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	oneShot := one.Bytes()
	if len(oneShot) != blobEncoded {
		t.Fatalf("one-shot encode is %d bytes, want %d", len(oneShot), blobEncoded)
	}

	// Every byte is copied through the 4096-byte buffer, so no hand-over can
	// exceed it: §5.1.6 forbids handing the sink anything outside the installed
	// buffer, and this assertion is what pins that on the measured row.
	cap := &capturingSink{}
	e, err := sofab.NewEncoderSink(make([]byte, blobChunk), 0, cap.fn)
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	if err := e.WriteBytes(1, blobSrc); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Equal(cap.got, oneShot) {
		t.Errorf("streaming wire bytes differ from the one-shot row (%d vs %d bytes)",
			len(cap.got), len(oneShot))
	}
	if cap.largest > blobChunk {
		t.Errorf("a hand-over of %d bytes exceeds the %d-byte buffer (§5.1.6)", cap.largest, blobChunk)
	}
	if cap.calls < 200 {
		t.Errorf("only %d flushes for a megabyte through a %d-byte buffer", cap.calls, blobChunk)
	}
}

// discardSink is what the measured rows use, and BENCH_SPEC forbids it to
// accumulate: it must consume and discard, doing only the minimum that keeps the
// call from being optimized away.
func TestDiscardSinkFoldsAndKeepsNothing(t *testing.T) {
	discardAcc = 0
	if err := discardSink(nil, []byte{0xA5, 0xFF}); err != nil {
		t.Fatalf("discardSink: %v", err)
	}
	if discardAcc != 0xA5 {
		t.Errorf("discardAcc = %#x, want the first byte folded in", discardAcc)
	}
	if err := discardSink(nil, nil); err != nil {
		t.Errorf("discardSink on an empty hand-over: %v", err)
	}
}

// The blob decode is "fed in 4096-byte chunks" — which is now literally what it
// is: the message is handed to Feed 4096 bytes at a time, and the decoder
// suspends and resumes wherever a field straddles a boundary.
func TestChunkedFeedResumesAtAnyBoundary(t *testing.T) {
	buf := encodeToBytes(t, encodeTypical)
	for _, chunk := range []int{1, 3, 7, blobChunk, len(buf)} {
		d := sofab.NewDecoder(foldVisitor{})
		before := sink
		var out sofab.Outcome
		for off := 0; off < len(buf); off += chunk {
			end := off + chunk
			if end > len(buf) {
				end = len(buf)
			}
			var err error
			if out, err = d.Feed(buf[off:end]); err != nil {
				t.Fatalf("chunk %d: Feed: %v", chunk, err)
			}
		}
		if out != sofab.Complete {
			t.Fatalf("chunk %d: last Feed = %v, want COMPLETE", chunk, out)
		}
		if sink == before {
			t.Errorf("chunk %d: the decode folded nothing", chunk)
		}
	}
}

// do_decode_blob is the harness's own chunked driver; this holds it to the same
// contract on the real workload.
func TestDecodeBlobIsFedInChunks(t *testing.T) {
	setupDecodeBlob()
	before := sink
	run_decode_blob()
	if sink == before {
		t.Error("the chunked blob decode folded nothing")
	}
}

// foldVisitor is the destination for the perf, composite and blob rows, so every
// arm of it has to fold — one that silently dropped a field kind would report a
// throughput for a decode that did less work than the row claims. The fp32 array
// arm is the one no shared workload carries, so it is driven here.
func TestFoldVisitorFoldsEveryFieldKind(t *testing.T) {
	buf := encodeToBytes(t, func(e *sofab.Encoder) {
		e.WriteFloat32Array(1, []float32{1.5, -2.5})
		e.WriteFloat64Array(2, []float64{3.5})
		sofab.WriteSignedArray(e, 3, []int64{-1, -2})
	})
	before := sink
	if err := sofab.AcceptBytes(buf, foldVisitor{}); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	if sink == before {
		t.Error("foldVisitor folded nothing for the float/signed array kinds")
	}
}

// ---- setup + measured entry points -----------------------------------------

func TestSetupDecodeU64PreparesADecodableBuffer(t *testing.T) {
	setupDecodeU64()
	if len(decBuf) == 0 || used != len(decBuf) {
		t.Fatalf("setup left decBuf=%d used=%d", len(decBuf), used)
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
	if len(decBuf) == 0 || used != len(decBuf) {
		t.Fatalf("setup left decBuf=%d used=%d", len(decBuf), used)
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

// The blob decode row folds one length per payload, so a decode that never
// reached the blob (or that stopped at a chunk boundary) shows up as a delta
// that is not the payload length.
func TestRunDecodeBlobReadsTheWholePayload(t *testing.T) {
	setupDecodeBlob()
	before := sink
	run_decode_blob()
	if got := sink - before; got != blobLen {
		t.Errorf("blob decode folded %d, want the %d-byte payload", got, blobLen)
	}
}

// decode: composite and decode: composite skip-all must walk the same bytes to
// the same end — one materializing every value, the other materializing none.
func TestCompositeDecodeRowsWalkTheWholeMessage(t *testing.T) {
	setupDecodeComposite()
	if used != len(decBuf) || used != 956 {
		t.Fatalf("setup left used=%d decBuf=%d, want 956", used, len(decBuf))
	}
	before := sink
	run_decode_composite()
	if sink == before {
		t.Error("decode: composite folded nothing")
	}
	// skip-all must consume the message cleanly; a Skip that stopped early
	// would surface as an error inside run_decode_composite_skip (it panics).
	before = sink
	run_decode_composite_skip()
	if sink != before+1 {
		t.Error("decode: composite skip-all did not complete its walk")
	}
}

// setupEncodeTypical + run_encode_typical are the Callgrind single-shot pair;
// they must leave exactly one encoded message in the buffer.
func TestRunEncodeTypicalProducesOneMessage(t *testing.T) {
	setupEncodeTypical()
	run_encode_typical()
	if used == 0 {
		t.Fatal("no bytes produced")
	}
	wantFields(t, walkAll(t, encOut[:used], typicalFix), walkAll(t, encodeToBytes(t, encodeTypical), typicalFix))
}

// foldVisitor is the destination for the perf, composite and blob decodes, whose
// point is that every value is materialized: it must observe all 12 fields of
// the perf message, nested scope included.
func TestFoldVisitorFoldsEveryField(t *testing.T) {
	buf := encodeToBytes(t, perfEncode)
	counts := &countingVisitor{}
	if err := sofab.AcceptBytes(buf, counts); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	// 5 scalar ints + fp32 + fp64 + string + 3 arrays at the top level, then the
	// two ints inside the sequence.
	if counts.n != 13 {
		t.Errorf("visitor saw %d values, want 13", counts.n)
	}
	before := sink
	if err := sofab.AcceptBytes(buf, foldVisitor{}); err != nil {
		t.Fatalf("AcceptBytes(foldVisitor): %v", err)
	}
	if sink == before {
		t.Error("foldVisitor folded nothing into sink")
	}
}

type countingVisitor struct {
	baseVisitor
	n int
}

func (c *countingVisitor) Unsigned(sofab.ID, uint64) error { c.n++; return nil }
func (c *countingVisitor) Signed(sofab.ID, int64) error    { c.n++; return nil }
func (c *countingVisitor) Float32(sofab.ID, float32) error { c.n++; return nil }
func (c *countingVisitor) Float64(sofab.ID, float64) error { c.n++; return nil }

// One count per FIELD, not per piece or per element: FixlenBegin and ArrayBegin
// fire exactly once each, which is what makes them the right place to count.
func (c *countingVisitor) FixlenBegin(_ sofab.ID, sub sofab.FixlenSubtype, _ int) error {
	if sub == sofab.FixlenStr || sub == sofab.FixlenBlob {
		c.n++
	}
	return nil
}

func (c *countingVisitor) ArrayBegin(sofab.ID, sofab.ArrayKind, int) error { c.n++; return nil }
func (c *countingVisitor) BeginSequence(sofab.ID) (sofab.Visitor, error) {
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

// ---- the workload table ----------------------------------------------------

// benchRowLabel is BENCH_SPEC's own row grammar, minus the value: the harness
// will not parse a row whose label is not one of these, so the table is a
// contract rather than an editorial choice.
var benchRowLabel = regexp.MustCompile(
	`^(encode|decode): (u64 array \(1000\)|typical message|blob 1MB one-shot|` +
		`blob 1MB streaming|blob 1MB|composite skip-all|composite)$`)

// benchRow is the full grammar the central harness matches each printed row
// with, value included.
var benchRow = regexp.MustCompile(
	`^(encode|decode):\s+(u64 array \(1000\)|typical message|blob 1MB one-shot|` +
		`blob 1MB streaming|blob 1MB|composite skip-all|composite)\s+([\d.]+)$`)

func TestWorkloadTableCoversTheSpecifiedDatasets(t *testing.T) {
	want := []string{
		"encode: u64 array (1000)",
		"encode: typical message",
		"encode: blob 1MB one-shot",
		"encode: blob 1MB streaming",
		"encode: composite",
		"decode: u64 array (1000)",
		"decode: typical message",
		"decode: blob 1MB",
		"decode: composite",
		"decode: composite skip-all",
	}
	if len(workloads) != len(want) {
		t.Fatalf("%d workloads, want %d", len(workloads), len(want))
	}
	seen := map[string]bool{}
	for i, w := range workloads {
		if w.label != want[i] {
			t.Errorf("workload %d label = %q, want %q", i, w.label, want[i])
		}
		if !benchRowLabel.MatchString(w.label) {
			t.Errorf("workload %q has a label the central harness will not parse", w.label)
		}
		if w.setup == nil || w.warm == nil || w.run == nil {
			t.Errorf("workload %q is missing a setup, warm or run function", w.verb)
		}
		if seen[w.verb] {
			t.Errorf("duplicate workload verb %q", w.verb)
		}
		seen[w.verb] = true
		if findWorkload(w.verb) == nil {
			t.Errorf("findWorkload(%q) found nothing", w.verb)
		}
	}
	if findWorkload("nonsense") != nil {
		t.Error("findWorkload matched a verb that does not exist")
	}
}

// The warm and the toggled path must be the SAME op: the Callgrind number is
// one call of run_<verb> after one call of warm, and a wrapper wired to the
// wrong body — or a body that only works the first time — would report an Ir/op
// for something other than the row it is printed against.
func TestWarmAndToggledPathsAreTheSameOp(t *testing.T) {
	for _, w := range workloads {
		w.setup()

		before := sink
		w.warm()
		warmUsed, warmDelta := used, sink-before

		before = sink
		w.run()
		runUsed, runDelta := used, sink-before

		if warmUsed == 0 {
			t.Errorf("%s: the warm op reported no message size", w.verb)
		}
		if runUsed != warmUsed {
			t.Errorf("%s: warm op produced %d bytes, toggled op %d", w.verb, warmUsed, runUsed)
		}
		// Both calls walk the same message, so they fold the same amount into
		// the sink — including zero, for the encode rows.
		if runDelta != warmDelta {
			t.Errorf("%s: warm op folded %d, toggled op %d", w.verb, warmDelta, runDelta)
		}
	}
}

// bench/run_callgrind.sh and bench/profile.sh take their workload list from this
// verb, so a row added to the table reaches the Callgrind table without editing
// a shell script. The list must therefore stay machine-readable.
func TestWorkloadsVerbListsEveryRow(t *testing.T) {
	out := captureStdout(t, func() { runMain(t, "workloads") })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(workloads) {
		t.Fatalf("`workloads` printed %d lines, want %d\n%s", len(lines), len(workloads), out)
	}
	for i, line := range lines {
		verb, label, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("line %q is not <verb>TAB<label>", line)
		}
		if verb != workloads[i].verb || label != workloads[i].label {
			t.Errorf("line %d = %q/%q, want %q/%q", i, verb, label, workloads[i].verb, workloads[i].label)
		}
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

func TestPerfMeasureCountsTheOpsItRan(t *testing.T) {
	calls := 0
	var x uint64
	r := perfMeasure(func() {
		calls++
		for i := 0; i < 20_000; i++ {
			x += uint64(i)
		}
	}, 256)
	sink += x
	if r.iters == 0 || r.nsOp <= 0 || r.mbS <= 0 {
		t.Errorf("perfMeasure = %+v, want positive iterations, ns/op and MB/s", r)
	}
	if uint64(calls) < r.iters {
		t.Errorf("reported %d iterations but called the op %d times", r.iters, calls)
	}
}

// ---- the two subcommands that measure --------------------------------------

// Driven through main() so the dispatch and the ~1s loops behind it are paid
// once, not twice.
func TestMainBenchPrintsTheSharedTable(t *testing.T) {
	out := captureStdout(t, func() { runMain(t, "bench") })
	if !strings.Contains(out, "=== SofaBuffers Go throughput (CPU time, MB/s) ===") {
		t.Errorf("bench output missing the harness's header line\n%s", out)
	}
	if !strings.Contains(out, "MB = 1e6 bytes") {
		t.Errorf("bench output missing the MB convention line\n%s", out)
	}

	// Every workload must appear as a row the harness's regex parses, carrying a
	// real number rather than a 0.00 placeholder from a workload that never ran.
	rows := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "encode:") && !strings.HasPrefix(line, "decode:") {
			continue
		}
		m := benchRow.FindStringSubmatch(strings.TrimRight(line, " "))
		if m == nil {
			t.Errorf("row %q does not match the central harness's row grammar", line)
			continue
		}
		label := m[1] + ": " + m[2]
		rows[label] = true
		var mbs float64
		if _, err := fmt.Sscanf(m[3], "%g", &mbs); err != nil || !(mbs > 0) {
			t.Errorf("row %q reports no positive throughput", line)
		}
	}
	for _, w := range workloads {
		if !rows[w.label] {
			t.Errorf("bench printed no row for %q", w.label)
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
		"message size  : 170 bytes", // the cross-port parity check
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

// Every workload must be reachable as a single-shot verb — that is what the
// Callgrind harness runs — and must leave a byte count behind for the table's
// `bytes` column.
func TestMainSingleWorkloadVerbs(t *testing.T) {
	for _, w := range workloads {
		used = 0
		before := sink
		runMain(t, w.verb)
		if used == 0 {
			t.Errorf("%s: reported no message size", w.verb)
		}
		if strings.HasPrefix(w.verb, "decode_") && sink == before {
			t.Errorf("%s: decoded nothing into sink", w.verb)
		}
		if strings.Contains(w.verb, "blob") && used != blobEncoded {
			t.Errorf("%s: used=%d, want the %d-byte parity size", w.verb, used, blobEncoded)
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
		{[]string{"workloads"}, true},      // the verb list the shell tools read
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
