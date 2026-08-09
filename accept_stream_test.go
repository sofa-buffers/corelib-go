package sofab_test

// Tests for the reader-driven visitor decode (Decoder.AcceptStream), issue #71.
// The contract is equivalence: AcceptStream must produce the exact same visitor
// event stream and the exact same INVALID/INCOMPLETE outcomes as the slurp-then-
// cursor Accept, for every vector, at every byte boundary — while never holding
// the whole message in memory at once.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	sofab "github.com/sofa-buffers/corelib-go"
)

// TestAcceptStreamMatchesAccept replays every shared vector through the streaming
// path — all-at-once, one-byte, and half-sized readers — and compares the event
// log field-for-field to the canonical expectation, the same one TestVisitor-
// DecodesAllVectors holds Accept to. Passing under the one-byte reader proves the
// dispatch resumes at any boundary without slurping.
func TestAcceptStreamMatchesAccept(t *testing.T) {
	vf := loadVectors(t)
	readers := map[string]func([]byte) io.Reader{
		"all-at-once": func(b []byte) io.Reader { return bytes.NewReader(b) },
		"one-byte":    func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
		"half-sized":  func(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) },
	}
	for _, v := range vf.Vectors {
		raw, err := hex.DecodeString(v.Serialized.Hex)
		if err != nil {
			t.Fatalf("hex: %v", err)
		}
		want := strings.Join(expectLog(t, v.Fields), "|")
		for rname, mk := range readers {
			t.Run(v.Name+"/"+rname, func(t *testing.T) {
				var got []string
				if err := sofab.NewDecoder(mk(raw)).AcceptStream(recorder{&got}); err != nil {
					t.Fatalf("AcceptStream: %v", err)
				}
				if strings.Join(got, "|") != want {
					t.Fatalf("event mismatch\n got: %v\nwant: %v", got, want)
				}
			})
		}
	}
}

// TestAcceptStreamMalformed pins the streaming path to the same INVALID/INCOMPLETE
// verdicts as the cursor path (TestVisitorMalformed), and does so at a byte-at-a-
// time boundary so a wrong outcome cannot hide behind a lucky chunk size.
func TestAcceptStreamMalformed(t *testing.T) {
	// The cases live in malformedCases (malformed_test.go), shared with the
	// cursor and pull surfaces, so a case can never hold one surface only.
	runMalformedCases(t, func(in []byte, v sofab.Visitor) error {
		return sofab.NewDecoder(iotest.OneByteReader(bytes.NewReader(in))).AcceptStream(v)
	})
}

// TestAcceptStreamHeaderHookAntiFolding mirrors TestHeaderHookAntiFolding on the
// streaming path: the HeaderVisitor bound fires at the header word before the
// (possibly absent) payload, so an over-count/over-maxlen field that is also
// truncated is INVALID, not INCOMPLETE (§5.2, issue #53).
func TestAcceptStreamHeaderHookAntiFolding(t *testing.T) {
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
			r := iotest.OneByteReader(bytes.NewReader(c.in))
			if err := sofab.NewDecoder(r).AcceptStream(boundedVisitor{}); !errors.Is(err, c.want) {
				t.Errorf("AcceptStream: got %v, want %v", err, c.want)
			}
		})
	}
	// A visitor with no header bound is unaffected: the over-count truncated case
	// stays INCOMPLETE (backward-compat, additive hook).
	r := iotest.OneByteReader(bytes.NewReader([]byte{arrHdr, 6, 1, 2}))
	if err := sofab.NewDecoder(r).AcceptStream(plainVisitor{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("AcceptStream plainVisitor: got %v, want ErrIncomplete", err)
	}
}

// TestAcceptStreamPropagatesErrors proves a visitor error from every callback
// surfaces verbatim, the streaming twin of TestVisitorPropagatesErrors.
func TestAcceptStreamPropagatesErrors(t *testing.T) {
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
			if err := newDec(msg).AcceptStream(failOn{which: method, err: sentinel}); !errors.Is(err, sentinel) {
				t.Fatalf("AcceptStream = %v, want sentinel", err)
			}
		})
	}
}

// TestAcceptStreamReaderError confirms a non-EOF reader error surfaces verbatim.
func TestAcceptStreamReaderError(t *testing.T) {
	sentinel := errors.New("io boom")
	if err := sofab.NewDecoder(errReader{sentinel}).AcceptStream(recorder{new([]string)}); !errors.Is(err, sentinel) {
		t.Fatalf("AcceptStream = %v, want sentinel", err)
	}
}

// TestAcceptStreamUTF8AtDestination mirrors the visitor half of TestInvalidUTF8-
// Rejected: the streaming path hands string bytes through verbatim (a skip is
// never validated), and the destination is where invalid UTF-8 becomes INVALID.
func TestAcceptStreamUTF8AtDestination(t *testing.T) {
	in := append(vhdr(1, sofab.TypeFixlen), append(vbytes((1<<3)|subStr), 0xFF)...)
	// No destination for the id: the payload is ignored, so accepted.
	if err := newDec(in).AcceptStream(baseV{}); err != nil {
		t.Fatalf("no destination = %v, want nil", err)
	}
	// A destination that validates: INVALID (nil where §6.4 let the validator be
	// compiled out — see utf8_build_on_test.go).
	checkUTF8Decode(t, "destination invalid utf8",
		newDec(in).AcceptStream(&bindStrV{id: 1}))
}

// TestAcceptStreamSignedArrayHeaderHook exercises the header hook on a signed
// array (the unsigned/fixlen arms are covered by the anti-folding table): an
// over-count signed array id 15 is INVALID at the count word, before any element.
func TestAcceptStreamSignedArrayHeaderHook(t *testing.T) {
	// array<i*> id 15, count 5 (>4), then EOF.
	in := append(vhdr(15, sofab.TypeVarintArraySigned), append(vbytes(5), 0x02, 0x04)...)
	r := iotest.OneByteReader(bytes.NewReader(in))
	if err := sofab.NewDecoder(r).AcceptStream(boundedVisitor{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("AcceptStream = %v, want ErrInvalidMsg (over-count signed array)", err)
	}
}

// TestAcceptStreamDeepNesting mirrors TestDeepNestingRejected/TestMaxDepthRound-
// Trip for the streaming path: a message nested past MaxDepth is rejected rather
// than overflowing the Go stack (§4.9), and one nested exactly MaxDepth deep
// still decodes.
func TestAcceptStreamDeepNesting(t *testing.T) {
	// 0x06 = sequence start, id 0; a long run nests far past MaxDepth.
	deep := bytes.Repeat([]byte{0x06}, 100000)
	if err := sofab.NewDecoder(bytes.NewReader(deep)).AcceptStream(baseV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("AcceptStream deep = %v, want ErrInvalidMsg", err)
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
	if err := sofab.NewDecoder(bytes.NewReader(got)).AcceptStream(baseV{}); err != nil {
		t.Fatalf("AcceptStream MaxDepth-deep = %v, want nil", err)
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

// TestAcceptStreamIsIncremental is the core of issue #71: the first field must be
// dispatched before the whole message has been read. A message far larger than
// one chunk, whose first field is a small leading unsigned, must fire that
// field's callback while most of the stream is still unread — which the slurping
// Accept, by construction, cannot do.
func TestAcceptStreamIsIncremental(t *testing.T) {
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
	if err := sofab.NewDecoder(r).AcceptStream(spy); err != nil {
		t.Fatalf("AcceptStream = %v", err)
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
