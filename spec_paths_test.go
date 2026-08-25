package sofab_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// Spec obligations that only the reader-driven surface, or only an error path,
// can reach: malformed fixlen words seen mid-stream, reader failures that must
// surface verbatim rather than as a wire verdict, payloads larger than the
// decoder's own read chunk, truncation judged against a declared element width,
// and the visitor hooks' error propagation.
//
// Everything decodable is put through every entry point and every chunking the
// corelib offers — AcceptBytes, one Feed of the whole message, the same bytes
// fed one at a time, and FeedFrom over a reader — because §5.2's three-valued
// outcome is a property of the message, not of the API that read it, and not of
// where the chunk boundaries fell.

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
func decodeSurfaces(msg []byte, mk func() any) map[string]error {
	out := map[string]error{}
	out["AcceptBytes"] = acceptBytes(msg, mk())
	out["Feed"] = feedIn(msg, 0, mk())
	out["Feed/1-byte"] = feedIn(msg, 1, mk())
	out["FeedFrom/1-byte"] = feedFrom(
		&chunkReader{b: append([]byte(nil), msg...), n: 1}, 1, mk())
	return out
}

func wantSameVerdict(t *testing.T, msg []byte, mk func() any, want error) {
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
			wantSameVerdict(t, msg, func() any { return baseV{} }, sofab.ErrInvalidMsg)
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
			wantSameVerdict(t, msg, func() any { return baseV{} }, sofab.ErrInvalidMsg)
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
			if err := feedFrom(r, 1, baseV{}); !errors.Is(err, boom) {
				t.Errorf("Feed = %v, want the reader's own error", err)
			}
			// The same prefix ending in a clean EOF instead is INCOMPLETE, not a
			// reader error: the two outcomes must not be confused (§5.2).
			err := feedIn([]byte(tc.prefix), 1, baseV{})
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
	if err := feedIn(msg, 1, sink); err != nil {
		t.Fatalf("Feed = %v, want nil", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round-trip differs (got %d bytes, want %d)", len(got), len(payload))
	}

	// Chopped one byte short of the stated length: INCOMPLETE, never INVALID.
	err := feedIn(msg[:len(msg)-1], 1, &blobSink{})
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

func (s *blobSink) BeginSequence(sofab.ID) (any, error) { return s, nil }

// ---- array truncation at every offset (§7) ---------------------------------

// A count that outruns the bytes must be INCOMPLETE at EVERY truncation point,
// including the ones inside the batch decoder's fast path, on every entry point.
func TestArrayTruncationAtEveryOffset(t *testing.T) {
	for _, c := range []struct {
		name  string
		write func(*sofab.Encoder) error
	}{
		{"unsigned", func(e *sofab.Encoder) error {
			return sofab.WriteUnsignedArray(e, 1, []uint64{1, 2, 300, 40000, 5, 6, 7, 8})
		}},
		{"signed", func(e *sofab.Encoder) error {
			return sofab.WriteSignedArray(e, 1, []int64{-1, 2, -300, 40000, -5, 6, -7, 8})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := sofab.NewEncoder(&buf)
			if err := c.write(e); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := e.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			full := buf.Bytes()
			for cut := 1; cut < len(full); cut++ {
				for _, surface := range surfaces {
					if _, err := decodeAll(t, surface, full[:cut]); !errors.Is(err, sofab.ErrIncomplete) {
						t.Errorf("%s cut=%d = %v, want ErrIncomplete", surface, cut, err)
					}
				}
			}
		})
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

func (b *boundedV) BeginSequence(sofab.ID) (any, error) { return b, nil }

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
			bounded := func() any {
				return &boundedV{id: 1, kind: tc.kind, lo: tc.lo, hi: tc.hi, on: true}
			}
			unbounded := func() any { return &boundedV{} }
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
			mk := func() any {
				return &boundedV{id: 1, kind: sofab.ArrayUnsigned, lo: 0, hi: 255, on: true}
			}
			wantSameVerdict(t, msg, mk, sofab.ErrIncomplete)
			wantSameVerdict(t, msg, func() any { return &boundedV{} }, sofab.ErrIncomplete)
		})
	}
}

// The reader-driven array path must surface a transport failure verbatim — a
// reader that dies mid-array is neither INCOMPLETE (there is nothing to wait
// for) nor INVALID (nothing said the bytes were wrong). It must agree whether
// the visitor materializes the array or declines everything and merely walks it.
func TestArrayReadersSurfaceReaderFailures(t *testing.T) {
	boom := errors.New("transport died")

	for _, wt := range []sofab.WireType{sofab.TypeVarintArrayUnsigned, sofab.TypeVarintArraySigned} {
		wire := []byte(hdr(1, wt) + "\x20\x01\x02\x03")

		r := &prefixErrReader{b: wire, err: boom}
		if err := feedFrom(r, 1, recorder{new([]string)}); !errors.Is(err, boom) {
			t.Errorf("wire type %d, materializing = %v, want the reader's error", wt, err)
		}

		// A visitor that takes nothing walks the same elements, and must agree.
		r = &prefixErrReader{b: wire, err: boom}
		if err := feedFrom(r, 1, &countingSkipV{}); !errors.Is(err, boom) {
			t.Errorf("wire type %d, skipping = %v, want the reader's error", wt, err)
		}
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

func (h *hookErrV) BeginSequence(sofab.ID) (any, error) { return h, nil }

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
			mk := func() any { return &hookErrV{err: boom, onFix: tc.fix, target: 1} }
			for surface, got := range decodeSurfaces(msg, mk) {
				if !errors.Is(got, boom) {
					t.Errorf("%s = %v, want the hook's own error", surface, got)
				}
			}
			// The hook must actually have fired — a surface that never asked
			// would pass the check above by returning nil.
			v := &hookErrV{err: boom, onFix: tc.fix, target: 1}
			_ = acceptBytes(msg, v)
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

	// A length-framed transport: each Decoder is handed exactly its own message
	// off the shared buffered reader.
	br := bufio.NewReader(bytes.NewReader(buf.Bytes()))

	var first []string
	if err := feedFrom(io.LimitReader(br, int64(firstLen)), 1, recorder{&first}); err != nil {
		t.Fatalf("first message: %v", err)
	}
	if got, want := strings.Join(first, "|"), evU(1, 42)+"|"+evS(2, -7); got != want {
		t.Fatalf("first message events = %s, want %s", got, want)
	}

	// A second Decoder over the SAME buffered reader picks up the next message:
	// the first must not have drained past its own bytes.
	var second []string
	if err := feedFrom(br, 1, recorder{&second}); err != nil {
		t.Fatalf("second message: %v (the first decoder over-read the shared reader)", err)
	}
	if got, want := strings.Join(second, "|"), evStr(3, "second"); got != want {
		t.Fatalf("second message events = %s, want %s", got, want)
	}
}

// FeedFrom over an already-buffered reader must behave identically to FeedFrom
// over a raw one: it holds no reader state of its own.
func TestFeedOverAnExistingBufioReader(t *testing.T) {
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
	if err := feedIn(msg, 1, recorder{&raw}); err != nil {
		t.Fatalf("Feed(raw) = %v", err)
	}
	br := bufio.NewReaderSize(bytes.NewReader(msg), 16)
	if err := feedFrom(br, 1, recorder{&buffered}); err != nil {
		t.Fatalf("Feed(bufio) = %v", err)
	}
	if fmt.Sprint(raw) != fmt.Sprint(buffered) {
		t.Errorf("events differ:\n raw      %v\n buffered %v", raw, buffered)
	}
	if len(raw) == 0 {
		t.Fatal("no events recorded")
	}
}
