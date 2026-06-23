package sofab_test

import (
	"bytes"
	"io"
	"math"
	"reflect"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func TestRoundTripScalars(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(1, math.MaxUint64)
	e.WriteSigned(2, math.MinInt64)
	e.WriteBool(3, true)
	e.WriteFloat32(4, math.Pi)
	e.WriteFloat64(5, math.E)
	e.WriteString(6, "SofaBuffers")
	e.WriteBytes(7, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	d := sofab.NewDecoder(&buf)
	expect := func(id sofab.ID, typ sofab.WireType) {
		f, err := d.Next()
		if err != nil || f.ID != id || f.Type != typ {
			t.Fatalf("next want id=%d type=%d, got %+v %v", id, typ, f, err)
		}
	}
	expect(1, sofab.TypeVarintUnsigned)
	if v, _ := d.Unsigned(); v != math.MaxUint64 {
		t.Fatal("u64")
	}
	expect(2, sofab.TypeVarintSigned)
	if v, _ := d.Signed(); v != math.MinInt64 {
		t.Fatal("i64")
	}
	expect(3, sofab.TypeVarintUnsigned)
	if v, _ := d.Bool(); !v {
		t.Fatal("bool")
	}
	expect(4, sofab.TypeFixlen)
	if v, _ := d.Float32(); v != math.Pi {
		t.Fatal("f32")
	}
	expect(5, sofab.TypeFixlen)
	if v, _ := d.Float64(); v != math.E {
		t.Fatal("f64")
	}
	expect(6, sofab.TypeFixlen)
	if v, _ := d.String(); v != "SofaBuffers" {
		t.Fatal("str")
	}
	expect(7, sofab.TypeFixlen)
	if v, _ := d.Bytes(); !bytes.Equal(v, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
		t.Fatal("blob")
	}
}

func TestRoundTripArrays(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	sofab.WriteUnsignedArray(e, 1, []uint16{10, 20, 30})
	sofab.WriteSignedArray(e, 2, []int64{-5, 5})
	e.WriteFloat64Array(3, []float64{1.5, -2.5})
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	d := sofab.NewDecoder(&buf)
	d.Next()
	if u, _ := sofab.ReadUnsignedArray[uint16](d); !reflect.DeepEqual(u, []uint16{10, 20, 30}) {
		t.Fatalf("u16 array %v", u)
	}
	d.Next()
	if s, _ := sofab.ReadSignedArray[int64](d); !reflect.DeepEqual(s, []int64{-5, 5}) {
		t.Fatalf("i64 array %v", s)
	}
	d.Next()
	if f, _ := d.ReadFloat64Array(); !reflect.DeepEqual(f, []float64{1.5, -2.5}) {
		t.Fatalf("f64 array %v", f)
	}
}

func TestRoundTripNestedSequences(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(0, 1)
	for i := 0; i < 5; i++ {
		e.WriteSequenceBegin(1)
		e.WriteUnsigned(0, 42)
	}
	for i := 0; i < 5; i++ {
		e.WriteSequenceEnd()
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	d := sofab.NewDecoder(&buf)
	var starts, ends, scalars int
	for {
		f, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch f.Type {
		case sofab.TypeSequenceStart:
			starts++
		case sofab.TypeSequenceEnd:
			ends++
		case sofab.TypeVarintUnsigned:
			d.Unsigned()
			scalars++
		}
	}
	if starts != 5 || ends != 5 || scalars != 6 {
		t.Fatalf("starts=%d ends=%d scalars=%d", starts, ends, scalars)
	}
}
