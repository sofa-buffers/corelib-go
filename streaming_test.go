package sofab_test

// Chunked-streaming conformance (§7.2.4): the encoder must drive its sink
// repeatedly when the message exceeds the internal buffer, and the decoder must
// suspend/resume at any byte boundary when fed in tiny or odd-sized chunks.

import (
	"bytes"
	"encoding/hex"
	"io"
	"testing"
	"testing/iotest"

	sofab "github.com/sofa-buffers/corelib-go"
)

// chunkWriter records each Write call separately so we can prove the encoder
// drained its buffer more than once (streamed) rather than emitting one blob.
type chunkWriter struct {
	writes int
	buf    bytes.Buffer
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

func TestChunkedEncodeMatchesOneShot(t *testing.T) {
	// A 1000-element u64 array encodes to well over the encoder's internal
	// buffer, so the sink is necessarily driven multiple times mid-stream.
	src := make([]uint64, 1000)
	for i := range src {
		src[i] = uint64(i) * 0x9E3779B97F4A7C15
	}

	var oneShot bytes.Buffer
	e := sofab.NewEncoder(&oneShot)
	sofab.WriteUnsignedArray(e, 1, src)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	cw := &chunkWriter{}
	e2 := sofab.NewEncoder(cw)
	sofab.WriteUnsignedArray(e2, 1, src)
	if err := e2.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if oneShot.Len() <= 4096 {
		t.Fatalf("message too small (%d B) to exercise mid-stream flushing", oneShot.Len())
	}
	if cw.writes < 2 {
		t.Fatalf("sink driven %d time(s); expected multiple flushes while streaming", cw.writes)
	}
	if !bytes.Equal(cw.buf.Bytes(), oneShot.Bytes()) {
		t.Fatalf("streamed output (%d B) differs from one-shot output (%d B)",
			cw.buf.Len(), oneShot.Len())
	}
}

func TestChunkedDecodeAtEveryBoundary(t *testing.T) {
	// The composite vector exercises every value type plus nested sequences;
	// decoding it correctly from a byte-at-a-time (and half-sized) reader proves
	// the state machine resumes at any boundary.
	vf := loadVectors(t)
	var comp vector
	for _, v := range vf.Vectors {
		if v.Group == "composite" {
			comp = v
		}
	}
	if comp.Name == "" {
		t.Fatal("no composite vector found")
	}
	raw, err := hex.DecodeString(comp.Serialized.Hex)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}

	readers := map[string]func() io.Reader{
		"all-at-once": func() io.Reader { return bytes.NewReader(raw) },
		"one-byte":    func() io.Reader { return iotest.OneByteReader(bytes.NewReader(raw)) },
		"half-sized":  func() io.Reader { return iotest.HalfReader(bytes.NewReader(raw)) },
	}
	for name, mk := range readers {
		t.Run(name, func(t *testing.T) {
			d := sofab.NewDecoder(mk())
			for _, f := range comp.Fields {
				decodeField(t, d, f)
			}
		})
	}
}
