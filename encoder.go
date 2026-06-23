package sofab

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
)

// Encoder writes Sofab fields to an io.Writer. It buffers internally, so call
// Flush when done. Errors are sticky: once a write fails, subsequent writes are
// no-ops and the same error is returned, so generated Marshal code can issue a
// run of writes and check only the final Flush.
type Encoder struct {
	w   *bufio.Writer
	err error
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: bufio.NewWriter(w)}
}

// Err returns the first error encountered, if any.
func (e *Encoder) Err() error { return e.err }

// Flush writes any buffered bytes to the underlying writer.
func (e *Encoder) Flush() error {
	if e.err != nil {
		return e.err
	}
	e.err = e.w.Flush()
	return e.err
}

func (e *Encoder) setErr(err error) {
	if e.err == nil {
		e.err = err
	}
}

func (e *Encoder) putVarint(v uint64) {
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		if e.err == nil {
			e.err = e.w.WriteByte(b)
		}
		if v == 0 {
			return
		}
	}
}

func (e *Encoder) putRaw(data []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(data)
	}
}

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

// WriteString writes a string field (raw UTF-8 bytes, no NUL on the wire).
func (e *Encoder) WriteString(id ID, s string) error {
	e.writeFixlen(id, []byte(s), fixStr)
	return e.err
}

// WriteBytes writes a binary blob field.
func (e *Encoder) WriteBytes(id ID, data []byte) error {
	e.writeFixlen(id, data, fixBlob)
	return e.err
}

// WriteSequenceBegin opens a nested sequence with the given field id.
func (e *Encoder) WriteSequenceBegin(id ID) error {
	e.writeHeader(id, TypeSequenceStart)
	return e.err
}

// WriteSequenceEnd closes the most recently opened nested sequence.
func (e *Encoder) WriteSequenceEnd() error {
	e.writeHeader(0, TypeSequenceEnd)
	return e.err
}

// WriteUnsignedArray writes an array of unsigned integers.
func WriteUnsignedArray[T Unsigned](e *Encoder, id ID, a []T) error {
	if len(a) == 0 {
		e.setErr(ErrArgument)
		return e.err
	}
	e.writeHeader(id, TypeVarintArrayUnsigned)
	e.putVarint(uint64(len(a)))
	for _, x := range a {
		e.putVarint(uint64(x))
	}
	return e.err
}

// WriteSignedArray writes an array of signed integers.
func WriteSignedArray[T Signed](e *Encoder, id ID, a []T) error {
	if len(a) == 0 {
		e.setErr(ErrArgument)
		return e.err
	}
	e.writeHeader(id, TypeVarintArraySigned)
	e.putVarint(uint64(len(a)))
	for _, x := range a {
		e.putVarint(zigzagEncode(int64(x)))
	}
	return e.err
}

// WriteFloat32Array writes an array of 32-bit floats.
func (e *Encoder) WriteFloat32Array(id ID, a []float32) error {
	if len(a) == 0 {
		e.setErr(ErrArgument)
		return e.err
	}
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

// WriteFloat64Array writes an array of 64-bit floats.
func (e *Encoder) WriteFloat64Array(id ID, a []float64) error {
	if len(a) == 0 {
		e.setErr(ErrArgument)
		return e.err
	}
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
