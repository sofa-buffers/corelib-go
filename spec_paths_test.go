package sofab_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// Spec obligations that only the reader-driven surface, or only an error path,
// can reach: malformed fixlen words seen mid-stream, reader failures that must
// surface verbatim rather than as a wire verdict, payloads larger than the
// decoder's own read chunk, truncation judged against a declared element width,
// and the visitor hooks' error propagation.
//
// Everything decodable is put through ALL the decode surfaces the corelib
// exposes — AcceptBytes (zero-copy cursor), Accept (slurp + cursor), AcceptStream
// whole, and AcceptStream fed one byte at a time — because §5.2's three-valued
// outcome is a property of the message, not of the API that read it.

// ---- shared wire helpers ---------------------------------------------------

// hdr is a field header (id<<3)|type as varint bytes.
func hdr(id sofab.ID, t sofab.WireType) string {
	return string(uvarintBytes((uint64(id) << 3) | uint64(t)))
}

// fixWord is a fixlen word (length<<3)|subtype as varint bytes.
func fixWord(length uint64, subtype uint64) string {
	return string(uvarintBytes((length << 3) | subtype))
}

func uvarintBytes(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// decodeSurfaces runs msg through every decode surface and returns the verdict
// each produced, keyed by surface name.
func decodeSurfaces(msg []byte, mk func() sofab.Visitor) map[string]error {
	out := map[string]error{}
	out["AcceptBytes"] = sofab.AcceptBytes(msg, mk())
	out["Accept"] = sofab.NewDecoder(bytes.NewReader(msg)).Accept(mk())
	out["AcceptStream"] = sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(mk())
	out["AcceptStream/1-byte"] = sofab.NewDecoder(
		&chunkReader{b: append([]byte(nil), msg...), n: 1}).AcceptStream(mk())
	return out
}

func wantSameVerdict(t *testing.T, msg []byte, mk func() sofab.Visitor, want error) {
	t.Helper()
	for surface, got := range decodeSurfaces(msg, mk) {
		if !errors.Is(got, want) {
			t.Errorf("%s = %v, want %v (message % x)", surface, got, want, msg)
		}
	}
}

// ---- malformed fixlen words (§4.6) -----------------------------------------

// A fixlen word states both the payload length and the subtype. For the two
// float subtypes the length is FIXED by the subtype, so any other length is a
// contradiction the decoder must reject outright (§4.6) — not read as a short
// float, and not skipped as an unknown field. Subtypes 4..7 are reserved and are
// likewise INVALID however well-formed the framing around them looks.
func TestMalformedFixlenWordsAreInvalidOnEverySurface(t *testing.T) {
	cases := []struct {
		name string
		word string
		pay  int
	}{
		{"fp32 with length 0", fixWord(0, 0), 0},
		{"fp32 with length 3", fixWord(3, 0), 3},
		{"fp32 with length 5", fixWord(5, 0), 5},
		{"fp32 with length 8", fixWord(8, 0), 8},
		{"fp64 with length 0", fixWord(0, 1), 0},
		{"fp64 with length 4", fixWord(4, 1), 4},
		{"fp64 with length 7", fixWord(7, 1), 7},
		{"fp64 with length 9", fixWord(9, 1), 9},
		{"reserved subtype 4", fixWord(4, 4), 4},
		{"reserved subtype 5", fixWord(4, 5), 4},
		{"reserved subtype 6", fixWord(4, 6), 4},
		{"reserved subtype 7", fixWord(4, 7), 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A trailing unsigned field so a decoder that mistakenly accepted
			// the field would carry on and finish COMPLETE rather than stall.
			msg := []byte(hdr(1, sofab.TypeFixlen) + tc.word +
				string(make([]byte, tc.pay)) + hdr(2, sofab.TypeVarintUnsigned) + "\x01")
			wantSameVerdict(t, msg, func() sofab.Visitor { return baseV{} }, sofab.ErrInvalidMsg)
		})
	}
}

// The reserved-subtype rejection must also fire for an ARRAY element word
// (§4.8): the count and the framing are well-formed, so only the element word
// itself can condemn the field.
func TestReservedFixlenArrayElementSubtypeIsInvalid(t *testing.T) {
	for sub := uint64(4); sub <= 7; sub++ {
		t.Run(fmt.Sprintf("subtype %d", sub), func(t *testing.T) {
			msg := []byte(hdr(1, sofab.TypeFixlenArray) + "\x02" + fixWord(4, sub) + string(make([]byte, 8)))
			wantSameVerdict(t, msg, func() sofab.Visitor { return baseV{} }, sofab.ErrInvalidMsg)
		})
	}
}

// ---- reader failures surface verbatim (§5.2) -------------------------------

// prefixErrReader delivers b once and then fails every subsequent Read with err,
// persistently: a socket that dies mid-message. The error is NOT a wire verdict
// and must reach the caller unchanged, so it can be told apart from INCOMPLETE
// (which means "feed me more") and from INVALID.
type prefixErrReader struct {
	b   []byte
	err error
}

func (r *prefixErrReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, r.err
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestReaderFailureMidFieldSurfacesVerbatim(t *testing.T) {
	boom := errors.New("transport died")
	cases := []struct {
		name   string
		prefix string
	}{
		// Inside the field header's varint: one continuation byte, then nothing.
		{"mid header varint", "\x80"},
		// Header read, value varint started and never finished.
		{"mid value varint", hdr(1, sofab.TypeVarintUnsigned) + "\x80"},
		// A fixed-width fp32 payload that never arrives (the fixedWindow path).
		{"before an fp32 payload", hdr(1, sofab.TypeFixlen) + fixWord(4, 0)},
		// A fixed-width fp64 payload that never arrives.
		{"before an fp64 payload", hdr(1, sofab.TypeFixlen) + fixWord(8, 1)},
		// A string payload that never arrives (the readRaw path).
		{"before a string payload", hdr(1, sofab.TypeFixlen) + fixWord(16, 2)},
		// An integer array whose elements run out mid-batch.
		{"mid unsigned array", hdr(1, sofab.TypeVarintArrayUnsigned) + "\x08\x01\x02\x03"},
		{"mid signed array", hdr(1, sofab.TypeVarintArraySigned) + "\x08\x01\x02\x03"},
		// A float array whose elements run out mid-payload.
		{"mid fp32 array", hdr(1, sofab.TypeFixlenArray) + "\x04" + fixWord(4, 0) + "\x00\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &prefixErrReader{b: []byte(tc.prefix), err: boom}
			if err := sofab.NewDecoder(r).AcceptStream(baseV{}); !errors.Is(err, boom) {
				t.Errorf("AcceptStream = %v, want the reader's own error", err)
			}
			// The same prefix ending in a clean EOF instead is INCOMPLETE, not a
			// reader error: the two outcomes must not be confused (§5.2).
			err := sofab.NewDecoder(bytes.NewReader([]byte(tc.prefix))).AcceptStream(baseV{})
			if !errors.Is(err, sofab.ErrIncomplete) {
				t.Errorf("clean EOF on the same prefix = %v, want ErrIncomplete", err)
			}
		})
	}
}

// ---- payloads larger than the decoder's read chunk -------------------------

// readRaw sizes its buffer once for an ordinary field but grows incrementally
// past its chunk, so a payload larger than that chunk exercises a different loop
// than every other test in the suite. It must still deliver the bytes exactly —
// and, truncated, must still be INCOMPLETE rather than an out-of-memory claim on
// the stated length.
func TestBlobLargerThanTheReadChunk(t *testing.T) {
	const size = 1 << 16 * 3 // three chunks plus change is enough to loop
	payload := make([]byte, size+7)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteBytes(1, payload); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	msg := buf.Bytes()

	var got []byte
	sink := &blobSink{onBytes: func(b []byte) { got = append([]byte(nil), b...) }}
	if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(sink); err != nil {
		t.Fatalf("AcceptStream = %v, want nil", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round-trip differs (got %d bytes, want %d)", len(got), len(payload))
	}

	// Chopped one byte short of the stated length: INCOMPLETE, never INVALID.
	err := sofab.NewDecoder(bytes.NewReader(msg[:len(msg)-1])).AcceptStream(&blobSink{})
	if !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("truncated oversized blob = %v, want ErrIncomplete", err)
	}
}

type blobSink struct {
	baseV
	onBytes func([]byte)
}

func (s *blobSink) Bytes(_ sofab.ID, b []byte) error {
	if s.onBytes != nil {
		s.onBytes(b)
	}
	return nil
}

func (s *blobSink) BeginSequence(sofab.ID) (sofab.Visitor, error) { return s, nil }

// ---- pull-surface array truncation (§7) ------------------------------------

// The generic array readers must report a count that outruns the bytes as
// INCOMPLETE at EVERY truncation point, including the ones inside the batch
// decoder's fast path, and must not narrow silently.
func TestPullArrayTruncationAtEveryOffset(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := sofab.WriteUnsignedArray(e, 1, []uint64{1, 2, 300, 40000, 5, 6, 7, 8}); err != nil {
		t.Fatalf("WriteUnsignedArray: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	full := buf.Bytes()

	for cut := 1; cut < len(full); cut++ {
		d := sofab.NewDecoder(bytes.NewReader(full[:cut]))
		if _, err := d.Next(); err != nil {
			// The header itself was cut: INCOMPLETE is the only acceptable verdict.
			if !errors.Is(err, sofab.ErrIncomplete) {
				t.Errorf("cut=%d Next = %v, want ErrIncomplete", cut, err)
			}
			continue
		}
		if _, err := sofab.ReadUnsignedArray[uint64](d); !errors.Is(err, sofab.ErrIncomplete) {
			t.Errorf("cut=%d ReadUnsignedArray = %v, want ErrIncomplete", cut, err)
		}
	}

	buf.Reset()
	e = sofab.NewEncoder(&buf)
	if err := sofab.WriteSignedArray(e, 1, []int64{-1, 2, -300, 40000, -5, 6, -7, 8}); err != nil {
		t.Fatalf("WriteSignedArray: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	full = buf.Bytes()
	for cut := 1; cut < len(full); cut++ {
		d := sofab.NewDecoder(bytes.NewReader(full[:cut]))
		if _, err := d.Next(); err != nil {
			if !errors.Is(err, sofab.ErrIncomplete) {
				t.Errorf("cut=%d Next = %v, want ErrIncomplete", cut, err)
			}
			continue
		}
		if _, err := sofab.ReadSignedArray[int64](d); !errors.Is(err, sofab.ErrIncomplete) {
			t.Errorf("cut=%d ReadSignedArray = %v, want ErrIncomplete", cut, err)
		}
	}
}

// ---- declared element width vs truncation (§5.2, §7.1) ---------------------

// boundedV declares a width for one array field and records the elements it was
// handed. It is the generated-visitor shape: the bound comes from the schema,
// and the corelib applies it while the elements go past.
type boundedV struct {
	baseV
	id     sofab.ID
	kind   sofab.ArrayKind
	lo, hi int64
	on     bool
}

func (b *boundedV) ArrayElemBound(id sofab.ID, kind sofab.ArrayKind) (int64, int64, bool) {
	if !b.on || id != b.id || kind != b.kind {
		return 0, 0, false
	}
	return b.lo, b.hi, true
}

func (b *boundedV) BeginSequence(sofab.ID) (sofab.Visitor, error) { return b, nil }

// An element already fully on the wire and outside its declared width makes the
// message INVALID even though what follows is merely truncated: §5.2 has INVALID
// dominate INCOMPLETE. Without the declaration the very same bytes are only
// INCOMPLETE — which is what proves the verdict comes from the bound and not
// from the framing.
func TestOverWidthElementInATruncatedArrayIsInvalid(t *testing.T) {
	// count=5 but only two element bytes: the array can never complete.
	// 0x80 0x02 is 256, one past a declared u8's maximum.
	unsigned := []byte(hdr(1, sofab.TypeVarintArrayUnsigned) + "\x05\x80\x02")
	// count=2 with two element bytes, so the fill (not the pre-scan) runs: the
	// first element decodes to +1, the second is a truncated varint.
	signed := []byte(hdr(1, sofab.TypeVarintArraySigned) + "\x02\x02\x80")

	for _, tc := range []struct {
		name string
		msg  []byte
		kind sofab.ArrayKind
		lo   int64
		hi   int64
	}{
		{"unsigned, declared u8", unsigned, sofab.ArrayUnsigned, 0, 255},
		{"signed, declared range [-1,0]", signed, sofab.ArraySigned, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounded := func() sofab.Visitor {
				return &boundedV{id: 1, kind: tc.kind, lo: tc.lo, hi: tc.hi, on: true}
			}
			unbounded := func() sofab.Visitor { return &boundedV{} }
			for surface, got := range decodeSurfaces(tc.msg, bounded) {
				if !errors.Is(got, sofab.ErrInvalidMsg) {
					t.Errorf("%s with the declared width = %v, want ErrInvalidMsg", surface, got)
				}
			}
			for surface, got := range decodeSurfaces(tc.msg, unbounded) {
				if !errors.Is(got, sofab.ErrIncomplete) {
					t.Errorf("%s without a declared width = %v, want ErrIncomplete", surface, got)
				}
			}
		})
	}
}

// An element INSIDE the declared width leaves the truncation verdict alone: the
// bound must not turn every short array into INVALID. Nor may the walk that
// applies it mistake its own end of buffer for malformation — including when the
// bytes stop half-way through an element's varint, which is where the walk has
// to decide between "malformed" and "send me the rest".
func TestInWidthElementInATruncatedArrayStaysIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"stops at an element boundary", "\x05\xff\x01"},
		{"stops mid-element", "\x05\x01\x80"},
		{"stops mid-element after several", "\x09\x01\x02\x03\xff\xff\x80"},
		{"no element bytes at all", "\x05"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := []byte(hdr(1, sofab.TypeVarintArrayUnsigned) + tc.body)
			mk := func() sofab.Visitor {
				return &boundedV{id: 1, kind: sofab.ArrayUnsigned, lo: 0, hi: 255, on: true}
			}
			wantSameVerdict(t, msg, mk, sofab.ErrIncomplete)
			wantSameVerdict(t, msg, func() sofab.Visitor { return &boundedV{} }, sofab.ErrIncomplete)
		})
	}
}

// The pull array readers must surface a transport failure verbatim too — a
// reader that dies mid-array is neither INCOMPLETE (there is nothing to wait
// for) nor INVALID (nothing said the bytes were wrong).
func TestPullArrayReadersSurfaceReaderFailures(t *testing.T) {
	boom := errors.New("transport died")

	r := &prefixErrReader{b: []byte(hdr(1, sofab.TypeVarintArrayUnsigned) + "\x20\x01\x02\x03"), err: boom}
	d := sofab.NewDecoder(r)
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, err := sofab.ReadUnsignedArray[uint64](d); !errors.Is(err, boom) {
		t.Errorf("ReadUnsignedArray = %v, want the reader's error", err)
	}

	r = &prefixErrReader{b: []byte(hdr(1, sofab.TypeVarintArraySigned) + "\x20\x01\x02\x03"), err: boom}
	d = sofab.NewDecoder(r)
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, err := sofab.ReadSignedArray[int64](d); !errors.Is(err, boom) {
		t.Errorf("ReadSignedArray = %v, want the reader's error", err)
	}

	// Skip walks the same elements, and must agree.
	r = &prefixErrReader{b: []byte(hdr(1, sofab.TypeVarintArrayUnsigned) + "\x20\x01\x02\x03"), err: boom}
	d = sofab.NewDecoder(r)
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := d.Skip(); !errors.Is(err, boom) {
		t.Errorf("Skip = %v, want the reader's error", err)
	}
}

// ---- visitor hook errors propagate (§6.1.1) --------------------------------

// hookErrV fails ArrayBegin (or FixlenHeader) for one id. A generated visitor
// applies its schema `count`/`maxlen` bound there, so the error it returns is
// how an over-bound header becomes INVALID — it must reach the caller from every
// array kind and on both decode surfaces, not just the ones already covered.
type hookErrV struct {
	baseV
	err    error
	onFix  bool
	seen   int
	target sofab.ID
}

func (h *hookErrV) ArrayBegin(id sofab.ID, _ sofab.ArrayKind, _ int) error {
	if h.onFix || id != h.target {
		return nil
	}
	h.seen++
	return h.err
}

func (h *hookErrV) FixlenHeader(id sofab.ID, _ int, _ int) error {
	if !h.onFix || id != h.target {
		return nil
	}
	h.seen++
	return h.err
}

func (h *hookErrV) BeginSequence(sofab.ID) (sofab.Visitor, error) { return h, nil }

func TestHeaderHookErrorPropagatesFromEveryArrayKind(t *testing.T) {
	boom := errors.New("schema bound exceeded")
	cases := []struct {
		name string
		body string
		fix  bool
	}{
		{"unsigned array", hdr(1, sofab.TypeVarintArrayUnsigned) + "\x02\x01\x02", false},
		{"signed array", hdr(1, sofab.TypeVarintArraySigned) + "\x02\x02\x04", false},
		{"fp32 array", hdr(1, sofab.TypeFixlenArray) + "\x01" + fixWord(4, 0) + "\x00\x00\x00\x00", false},
		{"fp64 array", hdr(1, sofab.TypeFixlenArray) + "\x01" + fixWord(8, 1) + string(make([]byte, 8)), false},
		{"string", hdr(1, sofab.TypeFixlen) + fixWord(2, 2) + "hi", true},
		{"blob", hdr(1, sofab.TypeFixlen) + fixWord(2, 3) + "\x00\x01", true},
		{"fp32 scalar", hdr(1, sofab.TypeFixlen) + fixWord(4, 0) + "\x00\x00\x00\x00", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := []byte(tc.body)
			mk := func() sofab.Visitor { return &hookErrV{err: boom, onFix: tc.fix, target: 1} }
			for surface, got := range decodeSurfaces(msg, mk) {
				if !errors.Is(got, boom) {
					t.Errorf("%s = %v, want the hook's own error", surface, got)
				}
			}
			// The hook must actually have fired — a surface that never asked
			// would pass the check above by returning nil.
			v := &hookErrV{err: boom, onFix: tc.fix, target: 1}
			_ = sofab.AcceptBytes(msg, v)
			if v.seen != 1 {
				t.Errorf("hook fired %d times, want exactly 1", v.seen)
			}
		})
	}
}

// ---- an existing *bufio.Reader is reused, not re-wrapped -------------------

// Wrapping a *bufio.Reader in a second one would let the inner reader be drained
// past the end of the message the Decoder was asked for, so a caller framing
// several messages on one buffered connection would lose the bytes of the next
// one. asBufio therefore reuses the reader it is given; this is the observable
// consequence.
func TestSharedBufioReaderResumesAtTheNextMessage(t *testing.T) {
	var buf bytes.Buffer
	a := sofab.NewEncoder(&buf)
	a.WriteUnsigned(1, 42)
	a.WriteSigned(2, -7)
	if err := a.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	firstLen := buf.Len()
	b := sofab.NewEncoder(&buf)
	b.WriteString(3, "second")
	if err := b.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if buf.Len() == firstLen {
		t.Fatal("the second message wrote nothing")
	}

	br := bufio.NewReader(bytes.NewReader(buf.Bytes()))

	d1 := sofab.NewDecoder(br)
	if _, err := d1.Next(); err != nil {
		t.Fatalf("d1.Next: %v", err)
	}
	if v, err := d1.Unsigned(); err != nil || v != 42 {
		t.Fatalf("d1.Unsigned = %d, %v", v, err)
	}
	if _, err := d1.Next(); err != nil {
		t.Fatalf("d1.Next: %v", err)
	}
	if v, err := d1.Signed(); err != nil || v != -7 {
		t.Fatalf("d1.Signed = %d, %v", v, err)
	}

	// A second Decoder over the SAME buffered reader picks up the next message.
	d2 := sofab.NewDecoder(br)
	f, err := d2.Next()
	if err != nil {
		t.Fatalf("d2.Next: %v (the first decoder over-read the shared reader)", err)
	}
	if f.ID != 3 {
		t.Fatalf("d2 read id %d, want 3", f.ID)
	}
	if s, err := d2.String(); err != nil || s != "second" {
		t.Fatalf("d2.String = %q, %v", s, err)
	}
	if _, err := d2.Next(); err != io.EOF {
		t.Errorf("d2.Next after the last field = %v, want io.EOF", err)
	}
}

// AcceptStream over an already-buffered reader must behave identically to
// AcceptStream over a raw one.
func TestAcceptStreamOverAnExistingBufioReader(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(1, 42)
	e.WriteString(2, "hello")
	sofab.WriteSignedArray(e, 3, []int64{-1, 2, -3})
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	msg := buf.Bytes()

	var raw, buffered []string
	if err := sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(recorder{&raw}); err != nil {
		t.Fatalf("AcceptStream(raw) = %v", err)
	}
	br := bufio.NewReaderSize(bytes.NewReader(msg), 16)
	if err := sofab.NewDecoder(br).AcceptStream(recorder{&buffered}); err != nil {
		t.Fatalf("AcceptStream(bufio) = %v", err)
	}
	if fmt.Sprint(raw) != fmt.Sprint(buffered) {
		t.Errorf("events differ:\n raw      %v\n buffered %v", raw, buffered)
	}
	if len(raw) == 0 {
		t.Fatal("no events recorded")
	}
}
