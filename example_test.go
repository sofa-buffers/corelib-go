package sofab_test

import (
	"bytes"
	"fmt"
	"io"

	sofab "github.com/sofa-buffers/corelib-go"
)

// SensorReading stands in for a struct emitted by the SofaBuffers code
// generator. The generator would produce the struct plus the Marshal/Unmarshal
// methods below; both delegate entirely to the corelib runtime. Field ids are
// fixed by the schema.
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

func (m *SensorReading) Marshal(e *sofab.Encoder) {
	e.WriteUnsigned(fieldID, uint64(m.ID))
	e.WriteSigned(fieldTemperature, int64(m.Temperature))
	e.WriteString(fieldName, m.Name)
	sofab.WriteUnsignedArray(e, fieldSamples, m.Samples)
	e.WriteSequenceBegin(fieldCalibration)
	m.Calibration.marshal(e)
	e.WriteSequenceEnd()
}

func (c *Calibration) marshal(e *sofab.Encoder) {
	e.WriteFloat32(calOffset, c.Offset)
	e.WriteFloat32(calGain, c.Gain)
}

func (m *SensorReading) Unmarshal(d *sofab.Decoder) error {
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
			if err := m.Calibration.unmarshal(d); err != nil {
				return err
			}
		default:
			if err := d.Skip(); err != nil {
				return err
			}
		}
	}
}

func (c *Calibration) unmarshal(d *sofab.Decoder) error {
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
	in.Marshal(enc)
	if err := enc.Flush(); err != nil {
		panic(err)
	}

	var out SensorReading
	if err := out.Unmarshal(sofab.NewDecoder(&buf)); err != nil {
		panic(err)
	}

	fmt.Printf("id=%d temp=%d name=%s samples=%v offset=%.1f gain=%.1f\n",
		out.ID, out.Temperature, out.Name, out.Samples, out.Calibration.Offset, out.Calibration.Gain)
	// Output: id=7 temp=-12 name=sensor-A samples=[100 200 300] offset=0.5 gain=2.0
}
