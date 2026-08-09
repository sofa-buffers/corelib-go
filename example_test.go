package sofab_test

import (
	"bytes"
	"fmt"
	"io"

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

func (m *SensorReading) Decode(d *sofab.Decoder) error {
	for {
		f, err := d.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case f.ID == fieldID && f.Type == sofab.TypeVarintUnsigned:
			v, _ := d.Unsigned()
			m.ID = uint32(v)
		case f.ID == fieldTemperature && f.Type == sofab.TypeVarintSigned:
			v, _ := d.Signed()
			m.Temperature = int32(v)
		case f.ID == fieldName && f.Type == sofab.TypeFixlen:
			m.Name, _ = d.String()
		case f.ID == fieldSamples && f.Type == sofab.TypeVarintArrayUnsigned:
			m.Samples, _ = sofab.ReadUnsignedArray[uint16](d)
		case f.ID == fieldCalibration && f.Type == sofab.TypeSequenceStart:
			if err := m.Calibration.decode(d); err != nil {
				return err
			}
		default:
			if err := d.Skip(); err != nil {
				return err
			}
		}
	}
}

func (c *Calibration) decode(d *sofab.Decoder) error {
	for {
		f, err := d.Next()
		if err != nil {
			return err
		}
		switch {
		case f.Type == sofab.TypeSequenceEnd:
			return nil
		case f.ID == calOffset:
			c.Offset, _ = d.Float32()
		case f.ID == calGain:
			c.Gain, _ = d.Float32()
		default:
			if err := d.Skip(); err != nil {
				return err
			}
		}
	}
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
	if err := out.Decode(sofab.NewDecoder(&buf)); err != nil {
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
	if err := back.Decode(sofab.NewDecoder(&empty)); err != nil {
		panic(err)
	}
	fmt.Printf("all-default: %d bytes, decodes back to id=%d gain=%.1f\n",
		empty.Len(), back.ID, back.Calibration.Gain)

	// Output: id=7 temp=-12 name=sensor-A samples=[100 200 300] offset=0.5 gain=2.0
	// all-default: 0 bytes, decodes back to id=0 gain=0.0
}
