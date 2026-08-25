package sofab_test

import (
	"errors"
	"io"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// --- helpers -----------------------------------------------------------------

// vbytes encodes v as a base-128 varint (same algorithm as the encoder), for
// hand-crafting wire bytes in the malformed-input tests below.
func vbytes(v uint64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

// vhdr builds a field header (id<<3 | type) as varint bytes.
func vhdr(id sofab.ID, t sofab.WireType) []byte {
	return vbytes((uint64(id) << 3) | uint64(t))
}

// fixlen subtype tags on the wire (mirrors the unexported encoder constants).
const (
	subFP32 = 0x0
	subFP64 = 0x1
	subStr  = 0x2
	subBlob = 0x3
)

// errReader fails every Read with a non-EOF error.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// failWriter fails every Write. Combined with a payload larger than bufio's
// buffer it forces the Encoder's sticky error to trip mid-stream.
type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

// --- trivial getters ---------------------------------------------------------

func TestEncoderErrGetter(t *testing.T) {
	if e := sofab.NewEncoder(nil); e.Err() != nil {
		t.Fatalf("fresh Err()=%v, want nil", e.Err())
	}
}

// --- encoder sticky-error paths ---------------------------------------------

func TestEncoderStickyError(t *testing.T) {
	e := sofab.NewEncoder(failWriter{io.ErrClosedPipe})
	// A blob larger than bufio's buffer forces a flush -> underlying write fails.
	e.WriteBytes(1, make([]byte, 16*1024))
	if e.Err() == nil {
		t.Fatal("expected sticky error after failed large write")
	}
	// A subsequent write must be a no-op returning the same sticky error
	// (exercises writeHeader's early return).
	if err := e.WriteUnsigned(2, 5); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("WriteUnsigned after error = %v, want ErrClosedPipe", err)
	}
	// Flush also returns the sticky error.
	if err := e.Flush(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Flush after error = %v, want ErrClosedPipe", err)
	}
}

// TestWriteBoolFalse: a false bool is an unsigned 0 on the wire (§4.4).
func TestWriteBoolFalse(t *testing.T) {
	log, err := decodeAll(t, "AcceptBytes", encode(t, func(e *sofab.Encoder) { e.WriteBool(3, false) }))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(log) != 1 || log[0] != evU(3, 0) {
		t.Fatalf("events = %v, want %v", log, evU(3, 0))
	}
}

// --- decoder truncated / malformed value payloads ----------------------------

// TestDecoderTruncatedValues is §7.2 item 6 in table form: a message cut short
// mid-field is INCOMPLETE, a malformed word is INVALID, and INVALID wins where
// the input is both (§5.2.3). Every row runs on all three visitor entry points,
// which is where a guard added to only one of them would show up.
func TestDecoderTruncatedValues(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error // nil = COMPLETE
	}{
		{"signed truncated varint", append(vhdr(0, sofab.TypeVarintSigned), 0x80), sofab.ErrIncomplete},
		{"fixlen truncated header", append(vhdr(0, sofab.TypeFixlen), 0x80), sofab.ErrIncomplete},
		{"float32 truncated payload",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subFP32), 0xAA, 0xBB)...), sofab.ErrIncomplete},
		{"fp32 subtype at length 8",
			append(vhdr(0, sofab.TypeFixlen), vbytes((8<<3)|subFP32)...), sofab.ErrInvalidMsg},
		{"float64 truncated payload",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((8<<3)|subFP64), 0x01)...), sofab.ErrIncomplete},
		{"string truncated payload",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subStr), 'h', 'i')...), sofab.ErrIncomplete},
		// A well-formed one-byte string. It is delivered as a string; a
		// destination declaring blob for the id simply does not bind it (§7.3),
		// and the decode stays COMPLETE either way.
		{"one-byte string", append(vhdr(0, sofab.TypeFixlen), append(vbytes((1<<3)|subStr), 'x')...), nil},
		{"fixlen length above max",
			append(vhdr(0, sofab.TypeFixlen), vbytes((uint64(sofab.IDMax+1)<<3)|subBlob)...), sofab.ErrInvalidMsg},
		{"unsigned-array count truncated",
			append(vhdr(0, sofab.TypeVarintArrayUnsigned), 0x80), sofab.ErrIncomplete},
		{"unsigned-array count above max",
			append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(uint64(sofab.IDMax)+1)...), sofab.ErrInvalidMsg},
		{"unsigned-array element truncated",
			append(vhdr(0, sofab.TypeVarintArrayUnsigned), append(vbytes(2), 0x05, 0x80)...), sofab.ErrIncomplete},
		{"signed-array count truncated",
			append(vhdr(0, sofab.TypeVarintArraySigned), 0x80), sofab.ErrIncomplete},
		{"signed-array element truncated",
			append(vhdr(0, sofab.TypeVarintArraySigned), append(vbytes(2), 0x02, 0x80)...), sofab.ErrIncomplete},
		{"fixlen-array count truncated",
			append(vhdr(0, sofab.TypeFixlenArray), 0x80), sofab.ErrIncomplete},
		{"fixlen-array word truncated",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...), sofab.ErrIncomplete},
		{"fp64-array payload absent",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), vbytes((8<<3)|subFP64)...)...), sofab.ErrIncomplete},
		{"fp32-array payload truncated",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((4<<3)|subFP32), 0x00, 0x00)...)...), sofab.ErrIncomplete},
		{"fp32-array payload one byte short",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((8<<3)|subFP64), 0x00)...)...), sofab.ErrIncomplete},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				_, err := decodeAll(t, s, c.in)
				if c.want == nil {
					if err != nil {
						t.Fatalf("%s = %v, want COMPLETE", s, err)
					}
					continue
				}
				if !errors.Is(err, c.want) {
					t.Fatalf("%s = %v, want %v", s, err, c.want)
				}
			}
		})
	}
}

// --- skipping what the visitor does not take ---------------------------------

// TestSkipEveryValueKind: a visitor that binds only the sentinel id must walk
// past one of EVERY value-bearing field kind and resync onto it (§7.2 item 7).
func TestSkipEveryValueKind(t *testing.T) {
	msg := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 7)
		e.WriteSigned(2, -7)
		e.WriteString(3, "skip me")
		e.WriteBytes(4, []byte{1, 2, 3})
		e.WriteFloat32(5, 1.5)
		e.WriteFloat64(6, 2.5)
		sofab.WriteUnsignedArray(e, 7, []uint32{1, 2, 3})
		sofab.WriteSignedArray(e, 8, []int32{-1, -2})
		e.WriteFloat32Array(9, []float32{1.5, 2.5})
		e.WriteFloat64Array(10, []float64{3.5})
		e.WriteSequenceBeginLazy(11)
		e.WriteUnsigned(1, 1)
		e.WriteSequenceEnd()
		e.WriteUnsigned(99, 123) // sentinel
	})
	for _, s := range surfaces {
		t.Run(s, func(t *testing.T) {
			var log []string
			v := &declineSeqV{log: &log}
			var err error
			switch s {
			case "AcceptBytes":
				err = acceptBytes(msg, v)
			case "Feed":
				err = feedIn(msg, 0, v)
			case "Feed/1-byte":
				err = feedIn(msg, 1, v)
			}
			if err != nil {
				t.Fatalf("%s = %v, want COMPLETE", s, err)
			}
			if len(log) == 0 || log[len(log)-1] != evU(99, 123) {
				t.Fatalf("%s never resynced onto the sentinel: %v", s, log)
			}
		})
	}
}

// TestSkipSequenceErrors: a DECLINED sub-sequence is walked, not trusted. A
// malformed or truncated construct inside one still decides the outcome.
func TestSkipSequenceErrors(t *testing.T) {
	badID := append(vhdr(0, sofab.TypeSequenceStart), vbytes((uint64(sofab.IDMax)+1)<<3)...)
	badID = append(badID, 0x00)

	truncVal := append(vhdr(0, sofab.TypeSequenceStart), vhdr(1, sofab.TypeVarintUnsigned)...)
	truncVal = append(truncVal, 0x80)

	for _, c := range []struct {
		name string
		in   []byte
		want error
	}{
		{"unterminated sequence", vhdr(0, sofab.TypeSequenceStart), sofab.ErrIncomplete},
		{"id above ID_MAX inside", badID, sofab.ErrInvalidMsg},
		{"truncated value inside", truncVal, sofab.ErrIncomplete},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				var err error
				v := &declineSeqV{log: new([]string)}
				switch s {
				case "AcceptBytes":
					err = acceptBytes(c.in, v)
				case "Feed":
					err = feedIn(c.in, 0, v)
				case "Feed/1-byte":
					err = feedIn(c.in, 1, v)
				}
				if !errors.Is(err, c.want) {
					t.Fatalf("%s = %v, want %v", s, err, c.want)
				}
			}
		})
	}
}

// TestFeedFromNonEOFReaderError: a real reader failure surfaces verbatim, not as
// a decode outcome.
func TestFeedFromNonEOFReaderError(t *testing.T) {
	sentinel := errors.New("boom")
	d := sofab.NewDecoder(baseVisitor())
	if _, err := d.FeedFrom(errReader{sentinel}, make([]byte, 16)); !errors.Is(err, sentinel) {
		t.Fatalf("FeedFrom = %v, want the reader error", err)
	}
}
