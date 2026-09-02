package sofab_test

// Tests for the PUSH surface — Decoder.Feed (CORELIB_PLAN §6.0, §5.2).
//
// The contract is chunk invariance: a message fed one byte at a time must
// produce the exact same visitor event stream and the exact same
// COMPLETE/INCOMPLETE/INVALID outcome as the same bytes fed in one call
// (§6.4.4, §7.2 item 4). Feed is also the ONLY decode machine in the package —
// AcceptBytes is a single Feed of the whole buffer and FeedFrom is a read loop
// over it — so proving it here proves it for every entry point.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	sofab "github.com/sofa-buffers/corelib-go"
)

// feedIn drives a FRESH decoder over in, chunk bytes at a time (chunk <= 0 means
// the whole buffer in one call), and folds the three outcomes back onto the
// sentinels the rest of the suite compares against: nil for COMPLETE,
// ErrIncomplete for INCOMPLETE, the reason for INVALID. That is exactly what
// AcceptBytes does, which is the point — one machine, one set of verdicts.
func feedIn(in []byte, chunk int, dest any, opts ...sofab.Option) error {
	d := sofab.NewDecoder(asVisitor(dest), opts...)
	if chunk <= 0 {
		chunk = len(in)
		if chunk == 0 {
			chunk = 1
		}
	}
	// Nothing fed at all is the valid empty message, so COMPLETE is where the
	// fold starts; every iteration overwrites it with what Feed just answered.
	out := sofab.Complete
	for i := 0; i < len(in); i += chunk {
		end := min(i+chunk, len(in))
		var err error
		if out, err = d.Feed(in[i:end]); err != nil {
			return err // INVALID travels as the error Feed returned
		}
	}
	if out == sofab.Incomplete {
		return sofab.ErrIncomplete
	}
	return nil
}

// feedFrom is feedIn through the io.Reader wrapper, with a caller-supplied
// scratch buffer of n bytes.
func feedFrom(r io.Reader, n int, dest any, opts ...sofab.Option) error {
	d := sofab.NewDecoder(asVisitor(dest), opts...)
	out, err := d.FeedFrom(r, make([]byte, n))
	if err != nil {
		return err
	}
	if out == sofab.Incomplete {
		return sofab.ErrIncomplete
	}
	return nil
}

// TestFeedIsChunkInvariant replays every shared vector through the push surface
// at four chunkings — the whole message, one byte, two bytes, half — and
// compares the event log field-for-field to the canonical expectation, the same
// one TestVisitorDecodesAllVectors holds AcceptBytes to. Passing at one byte per
// feed is what proves the machine suspends and resumes at ANY byte boundary
// (§5.2), payloads and multi-byte varints included.
func TestFeedIsChunkInvariant(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Vectors {
		raw, err := hex.DecodeString(v.Serialized.Hex)
		if err != nil {
			t.Fatalf("hex: %v", err)
		}
		want := strings.Join(expectLog(t, v.Fields), "|")
		checks := 0
		for _, chunk := range []int{0, 1, 2, len(raw)/2 + 1} {
			t.Run(fmt.Sprintf("%s/chunk-%d", v.Name, chunk), func(t *testing.T) {
				var got []string
				if err := feedIn(raw, chunk, aggOf(recorder{&got})); err != nil {
					t.Fatalf("Feed: %v", err)
				}
				if strings.Join(got, "|") != want {
					t.Fatalf("event mismatch\n got: %v\nwant: %v", got, want)
				}
				checks++
			})
		}
		t.Run(v.Name+"/reader", func(t *testing.T) {
			var got []string
			r := iotest.OneByteReader(bytes.NewReader(raw))
			if err := feedFrom(r, 8, aggOf(recorder{&got})); err != nil {
				t.Fatalf("FeedFrom: %v", err)
			}
			if strings.Join(got, "|") != want {
				t.Fatalf("event mismatch\n got: %v\nwant: %v", got, want)
			}
			checks++
		})
		vecRan("decode/chunked", checks)
	}
}

// TestFeedMalformedByteAtATime pins the byte-at-a-time feed to the same
// INVALID/INCOMPLETE verdicts the one-shot AcceptBytes reaches, so a wrong
// outcome cannot hide behind a lucky chunk size. §5.2.3 is the rule under test:
// INVALID dominates INCOMPLETE, and a chunk boundary must not turn one into the
// other.
func TestFeedMalformedByteAtATime(t *testing.T) {
	// The cases live in malformedCases (malformed_test.go), shared with the
	// one-shot path, so a case can never hold one entry point only.
	runMalformedCases(t, func(in []byte, v any) error {
		return feedIn(in, 1, v)
	})
}

// TestFeedHeaderHookAntiFolding mirrors TestHeaderHookAntiFolding under a
// byte-at-a-time feed: ArrayBegin / FixlenBegin fire at the header word before
// the (possibly absent) payload, so an over-count/over-maxlen field that is also
// truncated is INVALID, not INCOMPLETE (§5.2, issue #53).
func TestFeedHeaderHookAntiFolding(t *testing.T) {
	const arrHdr = 0x7b // array<u8>, id 15
	const strHdr = 0x7a // fixlen string, id 15
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"over-count complete", []byte{arrHdr, 5, 1, 2, 3, 4, 5}, sofab.ErrInvalidMsg},
		{"over-count truncated", []byte{arrHdr, 6, 1, 2}, sofab.ErrInvalidMsg},
		{"at-bound truncated", []byte{arrHdr, 4, 1, 2}, sofab.ErrIncomplete},
		{"over-maxlen string truncated", []byte{strHdr, (6 << 3) | subStr, 'a', 'b'}, sofab.ErrInvalidMsg},
		{"at-maxlen string truncated", []byte{strHdr, (4 << 3) | subStr, 'a', 'b'}, sofab.ErrIncomplete},
		{"over-maxlen blob truncated", []byte{strHdr, (6 << 3) | subBlob, 0x01, 0x02}, sofab.ErrInvalidMsg},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if err := feedIn(c.in, 1, aggOf(boundedVisitor{})); !errors.Is(err, c.want) {
				t.Errorf("Feed: got %v, want %v", err, c.want)
			}
		})
	}
	// A visitor with no header bound is unaffected: the over-count truncated case
	// stays INCOMPLETE (backward-compat, additive hook).
	if err := feedIn([]byte{arrHdr, 6, 1, 2}, 1, aggOf(plainVisitor{})); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("Feed plainVisitor: got %v, want ErrIncomplete", err)
	}
}

// TestFeedPropagatesErrors proves a visitor error from every callback surfaces
// verbatim under a byte-at-a-time feed, the streaming twin of
// TestVisitorPropagatesErrors.
func TestFeedPropagatesErrors(t *testing.T) {
	seq := func(e *sofab.Encoder) { e.WriteSequenceBeginLazy(1); e.WriteSequenceEndKeep() }
	build := map[string]func(*sofab.Encoder){
		"Unsigned":      func(e *sofab.Encoder) { e.WriteUnsigned(1, 5) },
		"Signed":        func(e *sofab.Encoder) { e.WriteSigned(1, -5) },
		"Float32":       func(e *sofab.Encoder) { e.WriteFloat32(1, 1.5) },
		"Float64":       func(e *sofab.Encoder) { e.WriteFloat64(1, 1.5) },
		"String":        func(e *sofab.Encoder) { e.WriteString(1, "x") },
		"Bytes":         func(e *sofab.Encoder) { e.WriteBytes(1, []byte{1}) },
		"UnsignedArray": func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 1, []uint32{1}) },
		"SignedArray":   func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 1, []int32{-1}) },
		"Float32Array":  func(e *sofab.Encoder) { e.WriteFloat32Array(1, []float32{1}) },
		"Float64Array":  func(e *sofab.Encoder) { e.WriteFloat64Array(1, []float64{1}) },
		"BeginSequence": seq,
		"EndSequence":   seq,
	}
	sentinel := errors.New("stop")
	for method, fn := range build {
		t.Run(method, func(t *testing.T) {
			msg := encode(t, fn)
			if err := feedIn(msg, 1, aggOf(failOn{which: method, err: sentinel})); !errors.Is(err, sentinel) {
				t.Fatalf("Feed = %v, want sentinel", err)
			}
		})
	}
}

// TestFeedFromReaderError confirms a non-EOF reader error surfaces verbatim
// through the io.Reader wrapper, and that an empty scratch buffer is refused as
// a caller mistake (§6.3 InvalidArgument) rather than spinning.
func TestFeedFromReaderError(t *testing.T) {
	sentinel := errors.New("io boom")
	d := sofab.NewDecoder(aggOf(recorder{new([]string)}))
	if _, err := d.FeedFrom(errReader{sentinel}, make([]byte, 8)); !errors.Is(err, sentinel) {
		t.Fatalf("FeedFrom = %v, want sentinel", err)
	}
	if _, err := sofab.NewDecoder(baseVisitor()).FeedFrom(bytes.NewReader(nil), nil); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("FeedFrom with no scratch = %v, want ErrArgument", err)
	}
}

// TestFeedUTF8AtDestination mirrors the visitor half of TestInvalidUTF8Rejected
// under a byte-at-a-time feed: the decoder hands string bytes through verbatim
// (a skip is never validated), and the destination is where invalid UTF-8
// becomes INVALID.
func TestFeedUTF8AtDestination(t *testing.T) {
	in := append(vhdr(1, sofab.TypeFixlen), append(vbytes((1<<3)|subStr), 0xFF)...)
	// No destination for the id: the payload is ignored, so accepted.
	if err := feedIn(in, 1, baseVisitor()); err != nil {
		t.Fatalf("no destination = %v, want nil", err)
	}
	// A destination that validates: INVALID (nil where §6.4 let the validator be
	// compiled out — see utf8_build_on_test.go).
	checkUTF8Decode(t, "destination invalid utf8",
		feedIn(in, 1, aggOf(&bindStrV{id: 1})))
}

// TestFeedSignedArrayHeaderHook exercises ArrayBegin on a signed array (the
// unsigned/fixlen arms are covered by the anti-folding table): an over-count
// signed array id 15 is INVALID at the count word, before any element.
func TestFeedSignedArrayHeaderHook(t *testing.T) {
	// array<i*> id 15, count 5 (>4), then EOF.
	in := append(vhdr(15, sofab.TypeVarintArraySigned), append(vbytes(5), 0x02, 0x04)...)
	if err := feedIn(in, 1, aggOf(boundedVisitor{})); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("Feed = %v, want ErrInvalidMsg (over-count signed array)", err)
	}
}

// TestFeedDeepNesting mirrors TestDeepNestingRejected/TestMaxDepthRoundTrip for
// the push path: a message nested past MaxDepth is rejected at the marker that
// breaches it (§4.9), and one nested exactly MaxDepth deep still decodes. The
// parse stack is sized to MaxDepth at construction and never grows, so the
// rejection is a comparison, not a memory event.
func TestFeedDeepNesting(t *testing.T) {
	// 0x06 = sequence start, id 0; a long run nests far past MaxDepth.
	deep := bytes.Repeat([]byte{0x06}, 100000)
	if err := feedIn(deep, 1, baseVisitor()); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("Feed deep = %v, want ErrInvalidMsg", err)
	}

	got := encode(t, func(e *sofab.Encoder) {
		for i := 0; i < sofab.MaxDepth; i++ {
			e.WriteSequenceBeginLazy(1)
		}
		e.WriteUnsigned(2, 7)
		for i := 0; i < sofab.MaxDepth; i++ {
			e.WriteSequenceEnd()
		}
	})
	if err := feedIn(got, 1, baseVisitor()); err != nil {
		t.Fatalf("Feed MaxDepth-deep = %v, want nil", err)
	}
}

// countingReader hands the wire out in fixed-size chunks and counts the Read
// calls, so a test can observe how much of the stream had been pulled when the
// first field was dispatched.
type countingReader struct {
	data  []byte
	pos   int
	reads int
	chunk int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	r.reads++
	n := copy(p, r.data[r.pos:min(r.pos+r.chunk, len(r.data))])
	r.pos += n
	return n, nil
}

// firstDispatchSpy records how many reads had happened when the first field was
// dispatched — the incrementality probe from issue #71.
type firstDispatchSpy struct {
	baseV
	r       *countingReader
	firstAt int
}

func (s *firstDispatchSpy) Unsigned(sofab.ID, uint64) error {
	if s.firstAt == 0 {
		s.firstAt = s.r.reads
	}
	return nil
}

// TestFeedIsIncremental is the core of issue #71, restated for the push surface:
// the first field must be dispatched before the whole message has been read.
// A message far larger than one chunk, whose first field is a small leading
// unsigned, must fire that field's callback while most of the stream is still
// unread — which is the property a decoder that buffered the message could not
// have.
func TestFeedIsIncremental(t *testing.T) {
	// Leading small unsigned (id 0), then a large u64 array that dwarfs it.
	big := make([]uint64, 4000)
	for i := range big {
		big[i] = uint64(i) * 0x9E3779B97F4A7C15
	}
	wire := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(0, 7)
		sofab.WriteUnsignedArray(e, 1, big)
	})
	if len(wire) < 8*1024 {
		t.Fatalf("wire too small (%d B) to prove incrementality", len(wire))
	}

	const chunk = 64
	r := &countingReader{data: wire, chunk: chunk}
	spy := &firstDispatchSpy{r: r}
	if err := feedFrom(r, chunk, aggOf(spy)); err != nil {
		t.Fatalf("FeedFrom = %v", err)
	}
	if spy.firstAt == 0 {
		t.Fatal("first field never dispatched")
	}
	totalReads := (len(wire) + chunk - 1) / chunk
	// The first field sits in the first few bytes, so it must dispatch within the
	// first read or two — far before the stream is drained.
	if spy.firstAt > 2 {
		t.Fatalf("first field dispatched after read #%d of %d; not incremental", spy.firstAt, totalReads)
	}
	if spy.firstAt >= totalReads {
		t.Fatalf("first field dispatched only after the whole stream was read (#%d of %d)", spy.firstAt, totalReads)
	}
}
