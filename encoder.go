package sofab

import (
	"encoding/binary"
	"io"
	"math"
)

// Encoder writes Sofab fields to an io.Writer. It accumulates the message in an
// internal byte slice and writes it to the destination in one shot on Flush —
// advancing over a contiguous buffer instead of pushing each byte through a
// bufio.Writer interface call. Errors are sticky: once a write fails, subsequent
// writes are no-ops and the same error is returned, so generated Marshal code
// can issue a run of writes and check only the final Flush.
type Encoder struct {
	w     io.Writer
	buf   []byte
	err   error
	depth int
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w, buf: make([]byte, 0, 512)}
}

// Err returns the first error encountered, if any.
func (e *Encoder) Err() error { return e.err }

// Flush writes any buffered bytes to the underlying writer in one Write.
func (e *Encoder) Flush() error {
	if e.err != nil {
		return e.err
	}
	if len(e.buf) > 0 {
		_, e.err = e.w.Write(e.buf)
		e.buf = e.buf[:0]
	}
	return e.err
}

// setErr records err as the sticky error, keeping the first one set so the
// original failure is what Err and Flush report.
func (e *Encoder) setErr(err error) {
	if e.err == nil {
		e.err = err
	}
}

// flushThreshold bounds how large the internal buffer grows before it is
// written out mid-stream. It mirrors bufio's capacity-triggered flush: small
// messages accumulate and go out in a single Write on Flush, while a large
// message (or a big blob) drives the destination multiple times and surfaces a
// write error immediately rather than only at Flush.
const flushThreshold = 4096

// maybeFlush writes the buffer out and resets it once it grows past the
// threshold, keeping memory bounded and preserving mid-stream write semantics.
func (e *Encoder) maybeFlush() {
	if e.err == nil && len(e.buf) >= flushThreshold {
		_, e.err = e.w.Write(e.buf)
		e.buf = e.buf[:0]
	}
}

// putVarint writes v as a base-128 varint, least-significant 7-bit group first.
// It is a no-op once the encoder holds a sticky error.
func (e *Encoder) putVarint(v uint64) {
	if e.err != nil {
		return
	}
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		e.buf = append(e.buf, b)
		if v == 0 {
			break
		}
	}
	e.maybeFlush()
}

// putRaw writes data verbatim (no length prefix). It is a no-op once the
// encoder holds a sticky error.
func (e *Encoder) putRaw(data []byte) {
	if e.err != nil {
		return
	}
	e.buf = append(e.buf, data...)
	e.maybeFlush()
}

// writeHeader writes a field header, the varint (id<<3 | type). It sets
// ErrArgument when id exceeds IDMax and is a no-op once an error is held.
func (e *Encoder) writeHeader(id ID, t WireType) {
	if e.err != nil {
		return
	}
	if id > IDMax {
		e.err = ErrArgument
		return
	}
	e.putVarint((uint64(id) << 3) | uint64(t))
}

// WriteUnsigned writes an unsigned-integer field.
func (e *Encoder) WriteUnsigned(id ID, v uint64) error {
	e.writeHeader(id, TypeVarintUnsigned)
	e.putVarint(v)
	return e.err
}

// WriteSigned writes a signed-integer field (zigzag + varint).
func (e *Encoder) WriteSigned(id ID, v int64) error {
	e.writeHeader(id, TypeVarintSigned)
	e.putVarint(zigzagEncode(v))
	return e.err
}

// WriteBool writes a boolean as an unsigned 0/1.
func (e *Encoder) WriteBool(id ID, b bool) error {
	if b {
		return e.WriteUnsigned(id, 1)
	}
	return e.WriteUnsigned(id, 0)
}

// writeFixlen writes a fixed-length field: the header, then a length-and-subtype
// varint (len(data)<<3 | sub), then the raw bytes. sub selects float/string/blob.
func (e *Encoder) writeFixlen(id ID, data []byte, sub uint64) {
	e.writeHeader(id, TypeFixlen)
	e.putVarint((uint64(len(data)) << 3) | sub)
	e.putRaw(data)
}

// WriteFloat32 writes a 32-bit float field.
func (e *Encoder) WriteFloat32(id ID, f float32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(f))
	e.writeFixlen(id, b[:], fixFp32)
	return e.err
}

// WriteFloat64 writes a 64-bit float field.
func (e *Encoder) WriteFloat64(id ID, f float64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(f))
	e.writeFixlen(id, b[:], fixFp64)
	return e.err
}

// WriteString writes a string field (raw UTF-8 bytes, no NUL on the wire). The
// bytes are appended straight from the string, with no []byte(s) copy.
func (e *Encoder) WriteString(id ID, s string) error {
	e.writeHeader(id, TypeFixlen)
	e.putVarint((uint64(len(s)) << 3) | fixStr)
	if e.err == nil {
		e.buf = append(e.buf, s...)
		e.maybeFlush()
	}
	return e.err
}

// WriteBytes writes a binary blob field.
func (e *Encoder) WriteBytes(id ID, data []byte) error {
	e.writeFixlen(id, data, fixBlob)
	return e.err
}

// WriteSequenceBegin opens a nested sequence with the given field id. Opening
// would-be sequence number MaxDepth+1 is rejected with ErrArgument and writes no
// bytes, so the wire never nests deeper than MaxDepth (§4.9).
func (e *Encoder) WriteSequenceBegin(id ID) error {
	if e.err != nil {
		return e.err
	}
	if e.depth >= MaxDepth {
		e.setErr(ErrArgument)
		return e.err
	}
	e.writeHeader(id, TypeSequenceStart)
	if e.err == nil {
		e.depth++
	}
	return e.err
}

// WriteSequenceEnd closes the most recently opened nested sequence.
func (e *Encoder) WriteSequenceEnd() error {
	e.writeHeader(0, TypeSequenceEnd)
	if e.err == nil && e.depth > 0 {
		e.depth--
	}
	return e.err
}

// WriteUnsignedArray writes an array of unsigned integers. An empty array is
// valid and emits exactly [header][count=0] (§4.7).
func WriteUnsignedArray[T Unsigned](e *Encoder, id ID, a []T) error {
	e.writeHeader(id, TypeVarintArrayUnsigned)
	e.putVarint(uint64(len(a)))
	for _, x := range a {
		e.putVarint(uint64(x))
	}
	return e.err
}

// WriteSignedArray writes an array of signed integers. An empty array is valid
// and emits exactly [header][count=0] (§4.7).
func WriteSignedArray[T Signed](e *Encoder, id ID, a []T) error {
	e.writeHeader(id, TypeVarintArraySigned)
	e.putVarint(uint64(len(a)))
	for _, x := range a {
		e.putVarint(zigzagEncode(int64(x)))
	}
	return e.err
}

// WriteFloat32Array writes an array of 32-bit floats. An empty array is valid
// and emits exactly [header][count=0][fixlen_word] — the fixlen_word is always
// present (even when empty) so the element subtype is never ambiguous (§4.8).
func (e *Encoder) WriteFloat32Array(id ID, a []float32) error {
	e.writeHeader(id, TypeFixlenArray)
	e.putVarint(uint64(len(a)))
	e.putVarint((4 << 3) | fixFp32)
	var b [4]byte
	for _, f := range a {
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(f))
		e.putRaw(b[:])
	}
	return e.err
}

// WriteFloat64Array writes an array of 64-bit floats. An empty array is valid
// and emits exactly [header][count=0][fixlen_word] — the fixlen_word is always
// present (even when empty) so the element subtype is never ambiguous (§4.8).
func (e *Encoder) WriteFloat64Array(id ID, a []float64) error {
	e.writeHeader(id, TypeFixlenArray)
	e.putVarint(uint64(len(a)))
	e.putVarint((8 << 3) | fixFp64)
	var b [8]byte
	for _, f := range a {
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(f))
		e.putRaw(b[:])
	}
	return e.err
}
