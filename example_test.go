package sofab_test

import (
	"bytes"
	"fmt"
	"math"

	sofab "github.com/sofa-buffers/corelib-go"
)

// SensorReading stands in for a struct emitted by the SofaBuffers code
// generator. The generator would produce the struct plus the Serialize/Decode
// methods below — the names CORELIB_PLAN §6.1.1 fixes for the generated layer —
// which delegate entirely to the corelib runtime. Field ids are fixed by the
// schema.
type SensorReading struct {
	ID          uint32
	Temperature int32
	Name        string
	Samples     []uint16
	Calibration Calibration // nested message -> wire sequence
}

type Calibration struct {
	Offset float32
	Gain   float32
}

const (
	fieldID          sofab.ID = 1
	fieldTemperature sofab.ID = 2
	fieldName        sofab.ID = 3
	fieldSamples     sofab.ID = 4
	fieldCalibration sofab.ID = 5

	calOffset sofab.ID = 1
	calGain   sofab.ID = 2
)

// Serialize writes the message the way generated code does: each field is
// written only when it differs from its declared default — here the zero
// value — because a field equal to its default is omitted from the wire
// (MESSAGE_SPEC §2). The reader reconstructs it from the schema.
func (m *SensorReading) Serialize(e *sofab.Encoder) {
	if m.ID != 0 {
		e.WriteUnsigned(fieldID, uint64(m.ID))
	}
	if m.Temperature != 0 {
		e.WriteSigned(fieldTemperature, int64(m.Temperature))
	}
	if m.Name != "" {
		e.WriteString(fieldName, m.Name)
	}
	if len(m.Samples) != 0 {
		sofab.WriteUnsignedArray(e, fieldSamples, m.Samples)
	}
	// A struct FIELD: BeginLazy holds the header back and End drops the frame if
	// the nested serialize writes nothing, so an all-default sub-message is omitted
	// rather than framed empty (MESSAGE_SPEC §2). That is what makes the per-field
	// test above compose: because Calibration.serialize omits each of its own
	// default-valued fields, "not one child was written" is exactly "the
	// sub-message equals its default" — no extra whole-object comparison needed.
	// A wrapper-array ELEMENT would close with WriteSequenceEndKeep instead — its
	// presence is what carries the array's length (§5.1).
	e.WriteSequenceBeginLazy(fieldCalibration)
	m.Calibration.serialize(e)
	e.WriteSequenceEnd()
}

func (c *Calibration) serialize(e *sofab.Encoder) {
	if c.Offset != 0 {
		e.WriteFloat32(calOffset, c.Offset)
	}
	if c.Gain != 0 {
		e.WriteFloat32(calGain, c.Gain)
	}
}

// SensorReading implements sofab.Visitor: the decode half of what the generator
// emits. Each callback binds the ids this schema declares and ignores the rest —
// an unknown id, and a field whose wire type contradicts the declared one
// (MESSAGE_SPEC §7.3), simply land in no arm and are skipped.
//
// Whatever a callback is handed is valid only until it returns (§6.7). Go's
// string conversion has already copied the payload, and Samples is built into
// storage this object owns, so nothing here outlives the call.

func (m *SensorReading) Unsigned(id sofab.ID, v uint64) error {
	if id == fieldID {
		m.ID = uint32(v)
	}
	return nil
}

func (m *SensorReading) Signed(id sofab.ID, v int64) error {
	if id == fieldTemperature {
		m.Temperature = int32(v)
	}
	return nil
}

func (m *SensorReading) String(id sofab.ID, v string) error {
	if id == fieldName {
		m.Name = v
	}
	return nil
}

func (m *SensorReading) UnsignedArray(id sofab.ID, v []uint64) error {
	if id != fieldSamples {
		return nil
	}
	m.Samples = make([]uint16, len(v))
	for i, x := range v {
		if x > math.MaxUint16 {
			return sofab.ErrInvalidMsg // the schema's declared width (MESSAGE_SPEC §7.1)
		}
		m.Samples[i] = uint16(x)
	}
	return nil
}

// BeginSequence hands the nested scope to the nested object. Returning nil
// instead would decline the whole sub-tree.
func (m *SensorReading) BeginSequence(id sofab.ID) (any, error) {
	if id == fieldCalibration {
		return &m.Calibration, nil
	}
	return nil, nil
}

func (*SensorReading) Float32(sofab.ID, float32) error        { return nil }
func (*SensorReading) Float64(sofab.ID, float64) error        { return nil }
func (*SensorReading) Bytes(sofab.ID, []byte) error           { return nil }
func (*SensorReading) SignedArray(sofab.ID, []int64) error    { return nil }
func (*SensorReading) Float32Array(sofab.ID, []float32) error { return nil }
func (*SensorReading) Float64Array(sofab.ID, []float64) error { return nil }
func (*SensorReading) EndSequence() error                     { return nil }

func (c *Calibration) Float32(id sofab.ID, v float32) error {
	switch id {
	case calOffset:
		c.Offset = v
	case calGain:
		c.Gain = v
	}
	return nil
}

func (*Calibration) Unsigned(sofab.ID, uint64) error        { return nil }
func (*Calibration) Signed(sofab.ID, int64) error           { return nil }
func (*Calibration) Float64(sofab.ID, float64) error        { return nil }
func (*Calibration) String(sofab.ID, string) error          { return nil }
func (*Calibration) Bytes(sofab.ID, []byte) error           { return nil }
func (*Calibration) UnsignedArray(sofab.ID, []uint64) error { return nil }
func (*Calibration) SignedArray(sofab.ID, []int64) error    { return nil }
func (*Calibration) Float32Array(sofab.ID, []float32) error { return nil }
func (*Calibration) Float64Array(sofab.ID, []float64) error { return nil }
func (*Calibration) EndSequence() error                     { return nil }
func (c *Calibration) BeginSequence(sofab.ID) (any, error) {
	return nil, nil
}

// Example shows how generator-emitted objects use the corelib to serialize and
// deserialize through the streaming Encoder/Decoder.
func Example() {
	in := &SensorReading{
		ID:          7,
		Temperature: -12,
		Name:        "sensor-A",
		Samples:     []uint16{100, 200, 300},
		Calibration: Calibration{Offset: 0.5, Gain: 2.0},
	}

	var buf bytes.Buffer
	enc := sofab.NewEncoder(&buf)
	in.Serialize(enc)
	if err := enc.Flush(); err != nil {
		panic(err)
	}

	var out SensorReading
	if err := acceptBytes(buf.Bytes(), &out); err != nil {
		panic(err)
	}

	fmt.Printf("id=%d temp=%d name=%s samples=%v offset=%.1f gain=%.1f\n",
		out.ID, out.Temperature, out.Name, out.Samples, out.Calibration.Offset, out.Calibration.Gain)

	// The same Serialize on an all-default value writes nothing at all: every
	// field is omitted, so the nested Calibration receives no content and its
	// held-back header is dropped along with its end marker (MESSAGE_SPEC §2).
	// An all-default message is the empty byte string, and decoding it back
	// yields the defaults again.
	var empty bytes.Buffer
	def := sofab.NewEncoder(&empty)
	(&SensorReading{}).Serialize(def)
	if err := def.Flush(); err != nil {
		panic(err)
	}
	var back SensorReading
	if err := acceptBytes(empty.Bytes(), &back); err != nil {
		panic(err)
	}
	fmt.Printf("all-default: %d bytes, decodes back to id=%d gain=%.1f\n",
		empty.Len(), back.ID, back.Calibration.Gain)

	// Output: id=7 temp=-12 name=sensor-A samples=[100 200 300] offset=0.5 gain=2.0
	// all-default: 0 bytes, decodes back to id=0 gain=0.0
}
