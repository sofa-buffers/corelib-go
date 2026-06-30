package sofab_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func newDec(b []byte) *sofab.Decoder { return sofab.NewDecoder(bytes.NewReader(b)) }

func TestDecodeUnsigned(t *testing.T) {
	d := newDec([]byte{0x00, 0x80, 0x01})
	f, err := d.Next()
	if err != nil || f.ID != 0 || f.Type != sofab.TypeVarintUnsigned {
		t.Fatalf("next: %+v %v", f, err)
	}
	v, err := d.Unsigned()
	if err != nil || v != 128 {
		t.Fatalf("got %d %v", v, err)
	}
}

func TestDecodeUnsignedMax(t *testing.T) {
	d := newDec([]byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01})
	d.Next()
	v, err := d.Unsigned()
	if err != nil || v != math.MaxUint64 {
		t.Fatalf("got %d %v", v, err)
	}
}

func TestDecodeSigned(t *testing.T) {
	d := newDec([]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01})
	d.Next()
	v, err := d.Signed()
	if err != nil || v != math.MinInt64 {
		t.Fatalf("got %d %v", v, err)
	}
}

func TestDecodeFloat32(t *testing.T) {
	d := newDec([]byte{0x02, 0x20, 0x56, 0x0E, 0x49, 0x40})
	d.Next()
	v, err := d.Float32()
	if err != nil || v != 3.1415 {
		t.Fatalf("got %v %v", v, err)
	}
}

func TestDecodeFloat64(t *testing.T) {
	d := newDec([]byte{0x02, 0x41, 0x00, 0x00, 0x00, 0x60, 0xFB, 0x21, 0x09, 0x40})
	d.Next()
	v, err := d.Float64()
	if err != nil || v != float64(float32(3.14159265)) {
		t.Fatalf("got %v %v", v, err)
	}
}

func TestDecodeString(t *testing.T) {
	d := newDec([]byte{0x02, 0x62, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x20, 0x43, 0x6F, 0x75, 0x63, 0x68, 0x21})
	d.Next()
	s, err := d.String()
	if err != nil || s != "Hello Couch!" {
		t.Fatalf("got %q %v", s, err)
	}
}

func TestDecodeStringEmpty(t *testing.T) {
	d := newDec([]byte{0x02, 0x02})
	d.Next()
	s, err := d.String()
	if err != nil || s != "" {
		t.Fatalf("got %q %v", s, err)
	}
}

func TestDecodeBlob(t *testing.T) {
	d := newDec([]byte{0x02, 0x2B, 0x01, 0x02, 0x03, 0x04, 0x05})
	d.Next()
	b, err := d.Bytes()
	if err != nil || !bytes.Equal(b, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("got % X %v", b, err)
	}
}

func TestDecodeArrayU32(t *testing.T) {
	d := newDec([]byte{0x03, 0x05, 0x01, 0x02, 0x03, 0x80, 0x80, 0x80, 0x80, 0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F})
	f, _ := d.Next()
	if f.Type != sofab.TypeVarintArrayUnsigned {
		t.Fatalf("type %v", f.Type)
	}
	got, err := sofab.ReadUnsignedArray[uint32](d)
	want := []uint32{1, 2, 3, 0x8000_0000, 0xFFFF_FFFF}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestDecodeArrayI32(t *testing.T) {
	d := newDec([]byte{0x04, 0x05, 0x01, 0x03, 0x05, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F, 0xFE, 0xFF, 0xFF, 0xFF, 0x0F})
	d.Next()
	got, err := sofab.ReadSignedArray[int32](d)
	want := []int32{-1, -2, -3, -2147483648, 2147483647}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestDecodeArrayFloat32(t *testing.T) {
	d := newDec([]byte{0x05, 0x05, 0x20, 0x00, 0x00, 0x80, 0x3F, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00,
		0x40, 0x40, 0xFF, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0x7F, 0x7F})
	d.Next()
	got, err := d.ReadFloat32Array()
	want := []float32{1, 2, 3, -math.MaxFloat32, math.MaxFloat32}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestDecodeNestedSequence(t *testing.T) {
	d := newDec([]byte{0x00, 0x2A, 0x0E, 0x00, 0x2A, 0x11, 0x53, 0x07, 0x11, 0x53})
	type ev struct {
		id  sofab.ID
		typ sofab.WireType
		val int64
	}
	var got []ev
	for {
		f, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch f.Type {
		case sofab.TypeVarintUnsigned:
			v, _ := d.Unsigned()
			got = append(got, ev{f.ID, f.Type, int64(v)})
		case sofab.TypeVarintSigned:
			v, _ := d.Signed()
			got = append(got, ev{f.ID, f.Type, v})
		default:
			got = append(got, ev{f.ID, f.Type, 0})
		}
	}
	want := []ev{
		{0, sofab.TypeVarintUnsigned, 42},
		{1, sofab.TypeSequenceStart, 0},
		{0, sofab.TypeVarintUnsigned, 42},
		{2, sofab.TypeVarintSigned, -42},
		{0, sofab.TypeSequenceEnd, 0},
		{2, sofab.TypeVarintSigned, -42},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// --- error cases ------------------------------------------------------------

// TestArrayCountZeroIsEmpty confirms a zero-count array is now valid (§4.7) and
// decodes to an empty, non-error slice — exactly the bytes [header][count=0].
func TestArrayCountZeroIsEmpty(t *testing.T) {
	d := newDec([]byte{0x03, 0x00}) // id 0, unsigned array, count 0
	d.Next()
	if a, err := sofab.ReadUnsignedArray[uint32](d); err != nil || len(a) != 0 {
		t.Fatalf("want [] nil, got %v %v", a, err)
	}
}

func TestVarintOverflowInvalid(t *testing.T) {
	d := newDec([]byte{0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	d.Next()
	if _, err := d.Unsigned(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("want ErrInvalidMsg, got %v", err)
	}
}

func TestDanglingSequenceEndInvalid(t *testing.T) {
	// header 0x07 = id 0, type SequenceEnd. The pull decoder surfaces it as a
	// token; a generated decoder treats an unmatched end as end-of-message. Here
	// we just confirm it decodes to the right token type.
	d := newDec([]byte{0x07})
	f, err := d.Next()
	if err != nil || f.Type != sofab.TypeSequenceEnd {
		t.Fatalf("got %+v %v", f, err)
	}
}

func TestIDAboveMaxInvalid(t *testing.T) {
	// Craft a header whose id field is IDMax+1, type unsigned (tag 0).
	var buf bytes.Buffer
	h := (uint64(sofab.IDMax) + 1) << 3
	for {
		b := byte(h & 0x7F)
		h >>= 7
		if h != 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
		if h == 0 {
			break
		}
	}
	buf.WriteByte(0x00)
	d := sofab.NewDecoder(&buf)
	if _, err := d.Next(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("want ErrInvalidMsg, got %v", err)
	}
}

func TestFloat32WrongLengthInvalid(t *testing.T) {
	d := newDec([]byte{0x02, (2 << 3) | 0x00, 0xAA, 0xBB}) // FIXLEN FP32 but length 2
	d.Next()
	if _, err := d.Float32(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("want ErrInvalidMsg, got %v", err)
	}
}

func TestSkipAndStreaming(t *testing.T) {
	// Encode three fields; skip the middle one while decoding.
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(1, 100)
	e.WriteString(2, "skip me")
	e.WriteSigned(3, -5)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	d := sofab.NewDecoder(&buf)
	f, _ := d.Next()
	if v, _ := d.Unsigned(); f.ID != 1 || v != 100 {
		t.Fatal("field 1")
	}
	d.Next() // field 2: do not read it; Next on the following call auto-skips
	f, _ = d.Next()
	if v, _ := d.Signed(); f.ID != 3 || v != -5 {
		t.Fatal("field 3")
	}
}

func TestSkipNestedSequence(t *testing.T) {
	// field 1, a nested (and doubly-nested) sequence in field 2, then field 3.
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteUnsigned(1, 11)
	e.WriteSequenceBegin(2)
	e.WriteUnsigned(0, 99)
	e.WriteSequenceBegin(5) // nested sequence inside
	e.WriteString(0, "deep")
	e.WriteSequenceEnd()
	e.WriteSigned(0, -1)
	e.WriteSequenceEnd()
	e.WriteSigned(3, 7)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}

	d := sofab.NewDecoder(&buf)
	f, _ := d.Next()
	if v, _ := d.Unsigned(); f.ID != 1 || v != 11 {
		t.Fatal("field 1")
	}
	f, _ = d.Next()
	if f.ID != 2 || f.Type != sofab.TypeSequenceStart {
		t.Fatal("expected sequence start")
	}
	if err := d.Skip(); err != nil { // skip the whole subtree
		t.Fatalf("skip: %v", err)
	}
	f, _ = d.Next()
	if v, _ := d.Signed(); f.ID != 3 || v != 7 {
		t.Fatalf("field 3 after skip: %+v %d", f, v)
	}
	if _, err := d.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}
