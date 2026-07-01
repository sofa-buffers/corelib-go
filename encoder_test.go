package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// encode runs fn against a fresh Encoder and returns the produced bytes.
func encode(t *testing.T, fn func(e *sofab.Encoder)) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	fn(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("encode error: %v", err)
	}
	return buf.Bytes()
}

func wantBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes mismatch:\n got=% X\nwant=% X", got, want)
	}
}

func TestIDMin(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteUnsigned(0, 0) }), []byte{0x00, 0x00})
}

func TestIDMax(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteUnsigned(sofab.IDMax, 0) }),
		[]byte{0xF8, 0xFF, 0xFF, 0xFF, 0x3F, 0x00})
}

func TestIDOverflow(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteUnsigned(sofab.IDMax+1, 0); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("want ErrArgument, got %v", err)
	}
}

func TestWriteUnsignedBoundaries(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00, 0x00}},
		{127, []byte{0x00, 0x7F}},
		{128, []byte{0x00, 0x80, 0x01}},
		{0x3FFF, []byte{0x00, 0xFF, 0x7F}},
		{0x4000, []byte{0x00, 0x80, 0x80, 0x01}},
		{0x8000_0000_0000_0000, []byte{0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}},
		{0xFFFF_FFFF_FFFF_FFFF, []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}},
	}
	for _, c := range cases {
		wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteUnsigned(0, c.v) }), c.want)
	}
}

func TestWriteSignedMinMax(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteSigned(0, -9223372036854775808) }),
		[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01})
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteSigned(0, 9223372036854775807) }),
		[]byte{0x01, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01})
}

func TestWriteBool(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteBool(0, true) }), []byte{0x00, 0x01})
}

func TestWriteFloat32(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteFloat32(0, 3.1415) }),
		[]byte{0x02, 0x20, 0x56, 0x0E, 0x49, 0x40})
}

func TestWriteFloat64(t *testing.T) {
	// C test promotes a float literal to double: write_fp64(3.14159265f)
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteFloat64(0, float64(float32(3.14159265))) }),
		[]byte{0x02, 0x41, 0x00, 0x00, 0x00, 0x60, 0xFB, 0x21, 0x09, 0x40})
}

func TestWriteString(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteString(0, "Hello Couch!") }),
		[]byte{0x02, 0x62, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x20, 0x43, 0x6F, 0x75, 0x63, 0x68, 0x21})
}

func TestWriteStringEmpty(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteString(0, "") }), []byte{0x02, 0x02})
}

func TestWriteBlob(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteBytes(0, []byte{1, 2, 3, 4, 5}) }),
		[]byte{0x02, 0x2B, 0x01, 0x02, 0x03, 0x04, 0x05})
}

func TestWriteBlobEmpty(t *testing.T) {
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteBytes(0, nil) }), []byte{0x02, 0x03})
}

func TestWriteArrayU32(t *testing.T) {
	a := []uint32{1, 2, 3, 0x8000_0000, 0xFFFF_FFFF}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 0, a) }),
		[]byte{0x03, 0x05, 0x01, 0x02, 0x03, 0x80, 0x80, 0x80, 0x80, 0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F})
}

func TestWriteArrayI32(t *testing.T) {
	a := []int32{-1, -2, -3, -2147483648, 2147483647}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 0, a) }),
		[]byte{0x04, 0x05, 0x01, 0x03, 0x05, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F, 0xFE, 0xFF, 0xFF, 0xFF, 0x0F})
}

func TestWriteArrayI8(t *testing.T) {
	a := []int8{-1, -2, -3, -128, 127}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 0, a) }),
		[]byte{0x04, 0x05, 0x01, 0x03, 0x05, 0xFF, 0x01, 0xFE, 0x01})
}

func TestWriteArrayU8(t *testing.T) {
	a := []uint8{1, 2, 3, 0, 255}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 0, a) }),
		[]byte{0x03, 0x05, 0x01, 0x02, 0x03, 0x00, 0xFF, 0x01})
}

func TestWriteArrayI16(t *testing.T) {
	a := []int16{-1, -2, -3, -32768, 32767}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 0, a) }),
		[]byte{0x04, 0x05, 0x01, 0x03, 0x05, 0xFF, 0xFF, 0x03, 0xFE, 0xFF, 0x03})
}

func TestWriteArrayU16(t *testing.T) {
	a := []uint16{1, 2, 3, 0, 65535}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 0, a) }),
		[]byte{0x03, 0x05, 0x01, 0x02, 0x03, 0x00, 0xFF, 0xFF, 0x03})
}

func TestWriteArrayI64(t *testing.T) {
	a := []int64{-1, -2, -3, -9223372036854775808, 9223372036854775807}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 0, a) }),
		[]byte{0x04, 0x05, 0x01, 0x03, 0x05, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			0x01, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01})
}

func TestWriteArrayU64(t *testing.T) {
	a := []uint64{1, 2, 3, 0, 0xFFFF_FFFF_FFFF_FFFF}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 0, a) }),
		[]byte{0x03, 0x05, 0x01, 0x02, 0x03, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			0xFF, 0x01})
}

func TestWriteArrayFloat32(t *testing.T) {
	const fmax = 3.40282346638528859811704183484516925440e+38 // FLT_MAX
	a := []float32{1, 2, 3, -fmax, fmax}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteFloat32Array(0, a) }),
		[]byte{0x05, 0x05, 0x20, 0x00, 0x00, 0x80, 0x3F, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x40,
			0x40, 0xFF, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0x7F, 0x7F})
}

func TestWriteArrayFloat64(t *testing.T) {
	const dmax = 1.79769313486231570814527423731704356798070e+308 // DBL_MAX
	a := []float64{1, 2, 3, -dmax, dmax}
	wantBytes(t, encode(t, func(e *sofab.Encoder) { e.WriteFloat64Array(0, a) }),
		[]byte{0x05, 0x05, 0x41, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xF0, 0x3F, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08, 0x40, 0xFF,
			0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xEF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xEF,
			0x7F})
}

func TestWriteNestedSequence(t *testing.T) {
	got := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(0, 42)
		e.WriteSequenceBegin(1)
		e.WriteUnsigned(0, 42)
		e.WriteSigned(2, -42)
		e.WriteSequenceEnd()
		e.WriteSigned(2, -42)
	})
	wantBytes(t, got, []byte{0x00, 0x2A, 0x0E, 0x00, 0x2A, 0x11, 0x53, 0x07, 0x11, 0x53})
}

func TestWriteNestedSequenceWithArray(t *testing.T) {
	got := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(0, 42)
		e.WriteSequenceBegin(3)
		e.WriteUnsigned(0, 42)
		sofab.WriteSignedArray(e, 3, []int32{-42, -43, -44})
		e.WriteSequenceEnd()
		e.WriteSigned(2, -42)
	})
	wantBytes(t, got, []byte{0x00, 0x2A, 0x1E, 0x00, 0x2A, 0x1C, 0x03, 0x53, 0x55, 0x57, 0x07, 0x11, 0x53})
}

// TestEmptyArraysEncode verifies the §4.7/§4.8 zero-count wire forms: an empty
// integer array is exactly [header][count=0] (no fixlen_word — element width is
// API-only), while an empty fixlen array still carries its fixlen_word (but no
// payload), so empty fp32 and fp64 arrays stay distinguishable on the wire.
func TestEmptyArraysEncode(t *testing.T) {
	// unsigned array, id 0: header (0<<3)|0b011 = 0x03, then count 0x00.
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 0, []uint32{})
	}), []byte{0x03, 0x00})
	// signed array, id 0: header (0<<3)|0b100 = 0x04, then count 0x00.
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		sofab.WriteSignedArray(e, 0, []int32{})
	}), []byte{0x04, 0x00})
	// fp32 array, id 0: header (0<<3)|0b101 = 0x05, count 0x00, fixlen_word
	// (4<<3)|fp32 = 0x20.
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteFloat32Array(0, nil)
	}), []byte{0x05, 0x00, 0x20})
	// fp64 array, id 0: header 0x05, count 0x00, fixlen_word (8<<3)|fp64 = 0x41 —
	// the fixlen_word keeps empty fp32 and fp64 arrays distinct on the wire.
	wantBytes(t, encode(t, func(e *sofab.Encoder) {
		e.WriteFloat64Array(0, nil)
	}), []byte{0x05, 0x00, 0x41})
}
