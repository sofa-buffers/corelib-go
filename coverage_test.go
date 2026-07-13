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

func mustNext(t *testing.T, d *sofab.Decoder) sofab.Field {
	t.Helper()
	f, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return f
}

// --- trivial getters ---------------------------------------------------------

func TestFieldGetter(t *testing.T) {
	d := newDec(encode(t, func(e *sofab.Encoder) { e.WriteUnsigned(9, 1) }))
	f := mustNext(t, d)
	if got := d.Field(); got != f {
		t.Fatalf("Field()=%+v, want %+v", got, f)
	}
}

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

func TestWriteBoolFalse(t *testing.T) {
	d := newDec(encode(t, func(e *sofab.Encoder) { e.WriteBool(3, false) }))
	mustNext(t, d)
	if v, err := d.Bool(); err != nil || v {
		t.Fatalf("Bool()=%v %v, want false nil", v, err)
	}
}

// TestEmptyArraysRoundTripPull confirms zero-count arrays (now legal, §4.7/§4.8)
// decode back to empty slices on the pull path, and that skipping them resyncs
// onto the following field.
func TestEmptyArraysRoundTripPull(t *testing.T) {
	got := encode(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{})
		sofab.WriteSignedArray(e, 2, []int32{})
		e.WriteFloat32Array(3, nil)
		e.WriteFloat64Array(4, nil)
	})

	d := newDec(got)
	mustNext(t, d)
	if a, err := sofab.ReadUnsignedArray[uint32](d); err != nil || len(a) != 0 {
		t.Fatalf("unsigned empty = %v %v, want [] nil", a, err)
	}
	mustNext(t, d)
	if a, err := sofab.ReadSignedArray[int32](d); err != nil || len(a) != 0 {
		t.Fatalf("signed empty = %v %v, want [] nil", a, err)
	}
	mustNext(t, d)
	if a, err := d.ReadFloat32Array(); err != nil || len(a) != 0 {
		t.Fatalf("fp32 empty = %v %v, want [] nil", a, err)
	}
	mustNext(t, d)
	if a, err := d.ReadFloat64Array(); err != nil || len(a) != 0 {
		t.Fatalf("fp64 empty = %v %v, want [] nil", a, err)
	}

	// Skipping all four (via Next's auto-skip) must resync to a clean EOF.
	d2 := newDec(got)
	for {
		_, err := d2.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("skip-walk over empty arrays: %v", err)
		}
	}

	// A trailing field after the empty arrays must still be reachable after skip.
	got2 := encode(t, func(e *sofab.Encoder) {
		e.WriteFloat32Array(3, nil)
		e.WriteUnsigned(5, 42)
	})
	d3 := newDec(got2)
	mustNext(t, d3) // the empty fp32 array, left unconsumed
	f := mustNext(t, d3)
	if f.ID != 5 || f.Type != sofab.TypeVarintUnsigned {
		t.Fatalf("resync field = %+v, want id 5 unsigned", f)
	}
	if v, err := d3.Unsigned(); err != nil || v != 42 {
		t.Fatalf("resync value = %v %v, want 42 nil", v, err)
	}
}

// --- decoder usage errors (right header, wrong typed reader) ------------------

func TestDecoderWrongTypeUsageErrors(t *testing.T) {
	// An unsigned field: every other typed reader must report ErrUsage without
	// consuming the value.
	d := newDec(encode(t, func(e *sofab.Encoder) { e.WriteUnsigned(1, 42) }))
	mustNext(t, d)

	if _, err := d.Signed(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("Signed = %v", err)
	}
	// And the mirror: Unsigned on a signed field is also a usage error.
	ds := newDec(encode(t, func(e *sofab.Encoder) { e.WriteSigned(1, -42) }))
	mustNext(t, ds)
	if _, err := ds.Unsigned(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("Unsigned on signed = %v", err)
	}
	if _, err := d.Float32(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("Float32 = %v", err)
	}
	if _, err := d.Float64(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("Float64 = %v", err)
	}
	if _, err := d.String(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("String = %v", err)
	}
	if _, err := sofab.ReadUnsignedArray[uint32](d); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("ReadUnsignedArray = %v", err)
	}
	if _, err := sofab.ReadSignedArray[int32](d); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("ReadSignedArray = %v", err)
	}
	if _, err := d.ReadFloat32Array(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("ReadFloat32Array = %v", err)
	}
	if _, err := d.ReadFloat64Array(); !errors.Is(err, sofab.ErrUsage) {
		t.Fatalf("ReadFloat64Array = %v", err)
	}
}

// --- decoder truncated / malformed value payloads ----------------------------

func TestDecoderTruncatedValues(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		read func(d *sofab.Decoder) error
		want error
	}{
		{
			"signed truncated varint",
			append(vhdr(0, sofab.TypeVarintSigned), 0x80), // continuation bit, then EOF
			func(d *sofab.Decoder) error { _, err := d.Signed(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float32 truncated header",
			append(vhdr(0, sofab.TypeFixlen), 0x80), // truncated length varint
			func(d *sofab.Decoder) error { _, err := d.Float32(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float32 truncated payload",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subFP32), 0xAA, 0xBB)...), // 2 of 4 bytes
			func(d *sofab.Decoder) error { _, err := d.Float32(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float64 wrong subtype",
			append(vhdr(0, sofab.TypeFixlen), vbytes((8<<3)|subFP32)...), // len 8 but sub fp32
			func(d *sofab.Decoder) error { _, err := d.Float64(); return err },
			sofab.ErrInvalidMsg,
		},
		{
			"float64 truncated header",
			append(vhdr(0, sofab.TypeFixlen), 0x80),
			func(d *sofab.Decoder) error { _, err := d.Float64(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float64 truncated payload",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((8<<3)|subFP64), 0x01)...),
			func(d *sofab.Decoder) error { _, err := d.Float64(); return err },
			sofab.ErrIncomplete,
		},
		{
			"string truncated header",
			append(vhdr(0, sofab.TypeFixlen), 0x80),
			func(d *sofab.Decoder) error { _, err := d.String(); return err },
			sofab.ErrIncomplete,
		},
		{
			"string truncated payload",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subStr), 'h', 'i')...), // 2 of 4
			func(d *sofab.Decoder) error { _, err := d.String(); return err },
			sofab.ErrIncomplete,
		},
		{
			"bytes wrong subtype (string, not blob)",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((1<<3)|subStr), 'x')...),
			func(d *sofab.Decoder) error { _, err := d.Bytes(); return err },
			sofab.ErrInvalidMsg,
		},
		{
			"fixlen length above max",
			append(vhdr(0, sofab.TypeFixlen), vbytes((uint64(sofab.IDMax+1)<<3)|subBlob)...),
			func(d *sofab.Decoder) error { _, err := d.Bytes(); return err },
			sofab.ErrInvalidMsg,
		},
		{
			"unsigned-array count truncated",
			append(vhdr(0, sofab.TypeVarintArrayUnsigned), 0x80),
			func(d *sofab.Decoder) error { _, err := sofab.ReadUnsignedArray[uint32](d); return err },
			sofab.ErrIncomplete,
		},
		{
			"unsigned-array count above max",
			append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(uint64(sofab.IDMax)+1)...),
			func(d *sofab.Decoder) error { _, err := sofab.ReadUnsignedArray[uint32](d); return err },
			sofab.ErrInvalidMsg,
		},
		{
			"unsigned-array element truncated",
			append(vhdr(0, sofab.TypeVarintArrayUnsigned), append(vbytes(2), 0x05, 0x80)...), // 2 elems, 2nd truncated
			func(d *sofab.Decoder) error { _, err := sofab.ReadUnsignedArray[uint32](d); return err },
			sofab.ErrIncomplete,
		},
		{
			"signed-array count truncated",
			append(vhdr(0, sofab.TypeVarintArraySigned), 0x80),
			func(d *sofab.Decoder) error { _, err := sofab.ReadSignedArray[int32](d); return err },
			sofab.ErrIncomplete,
		},
		{
			"signed-array element truncated",
			append(vhdr(0, sofab.TypeVarintArraySigned), append(vbytes(2), 0x02, 0x80)...),
			func(d *sofab.Decoder) error { _, err := sofab.ReadSignedArray[int32](d); return err },
			sofab.ErrIncomplete,
		},
		{
			"float32-array count truncated",
			append(vhdr(0, sofab.TypeFixlenArray), 0x80),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float32-array header truncated",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float32-array wrong element header",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), vbytes((8<<3)|subFP64)...)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err },
			sofab.ErrInvalidMsg,
		},
		{
			"float32-array payload truncated",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((4<<3)|subFP32), 0x00, 0x00)...)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float64-array count truncated",
			append(vhdr(0, sofab.TypeFixlenArray), 0x80),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat64Array(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float64-array header truncated",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat64Array(); return err },
			sofab.ErrIncomplete,
		},
		{
			"float64-array wrong element header",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), vbytes((4<<3)|subFP32)...)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat64Array(); return err },
			sofab.ErrInvalidMsg,
		},
		{
			"float64-array payload truncated",
			append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((8<<3)|subFP64), 0x00)...)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat64Array(); return err },
			sofab.ErrIncomplete,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDec(c.in)
			mustNext(t, d)
			if err := c.read(d); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// --- Skip over every value kind (success), exercising skipValue --------------

func TestSkipEveryValueKind(t *testing.T) {
	// A message with one of every value-bearing field followed by a sentinel.
	msg := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 7)
		e.WriteSigned(2, -7)
		e.WriteString(3, "skip me")
		sofab.WriteUnsignedArray(e, 4, []uint32{1, 2, 3})
		sofab.WriteSignedArray(e, 5, []int32{-1, -2})
		e.WriteFloat32Array(6, []float32{1.5, 2.5})
		e.WriteFloat64Array(7, []float64{3.5})
		e.WriteUnsigned(99, 123) // sentinel
	})
	d := newDec(msg)
	for i := 0; i < 7; i++ {
		mustNext(t, d)
		if err := d.Skip(); err != nil {
			t.Fatalf("Skip #%d: %v", i, err)
		}
	}
	f := mustNext(t, d)
	if f.ID != 99 {
		t.Fatalf("sentinel id=%d, want 99", f.ID)
	}
	if v, err := d.Unsigned(); err != nil || v != 123 {
		t.Fatalf("sentinel value=%d %v", v, err)
	}
}

// --- Skip / auto-skip error propagation --------------------------------------

func TestSkipValueErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"unsigned", append(vhdr(0, sofab.TypeVarintUnsigned), 0x80)},
		{"fixlen header", append(vhdr(0, sofab.TypeFixlen), 0x80)},
		{"fixlen payload", append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subStr), 'h', 'i')...)},
		{"varint array count", append(vhdr(0, sofab.TypeVarintArrayUnsigned), 0x80)},
		{"varint array element", append(vhdr(0, sofab.TypeVarintArrayUnsigned), append(vbytes(2), 0x01, 0x80)...)},
		{"fixlen array count", append(vhdr(0, sofab.TypeFixlenArray), 0x80)},
		{"fixlen array header", append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...)},
		{"fixlen array payload", append(vhdr(0, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((4<<3)|subFP32), 0x00)...)...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDec(c.in)
			mustNext(t, d)
			if err := d.Skip(); !errors.Is(err, sofab.ErrIncomplete) {
				t.Fatalf("Skip got %v, want ErrIncomplete", err)
			}
		})
	}
}

func TestSkipSequenceEndIsNoop(t *testing.T) {
	d := newDec(vhdr(0, sofab.TypeSequenceEnd))
	f := mustNext(t, d)
	if f.Type != sofab.TypeSequenceEnd {
		t.Fatalf("type=%v", f.Type)
	}
	if err := d.Skip(); err != nil {
		t.Fatalf("Skip(SequenceEnd)=%v, want nil", err)
	}
}

func TestSkipUnterminatedSequence(t *testing.T) {
	// A sequence start with no matching end: Skip must hit EOF and report it as
	// an incomplete (truncated) message.
	d := newDec(vhdr(0, sofab.TypeSequenceStart))
	mustNext(t, d)
	if err := d.Skip(); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("Skip = %v, want ErrIncomplete", err)
	}
}

func TestSkipSequenceWithBadToken(t *testing.T) {
	// Sequence start followed by a header whose id exceeds IDMax: the inner
	// Next fails and Skip propagates the error.
	in := append(vhdr(0, sofab.TypeSequenceStart), vbytes((uint64(sofab.IDMax)+1)<<3)...)
	in = append(in, 0x00)
	d := newDec(in)
	mustNext(t, d)
	if err := d.Skip(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("Skip = %v, want ErrInvalidMsg", err)
	}
}

func TestSkipSequenceWithTruncatedValue(t *testing.T) {
	// Sequence start, then an unsigned field with a truncated value: Skip walks
	// into the sequence and fails consuming that value.
	in := append(vhdr(0, sofab.TypeSequenceStart), vhdr(1, sofab.TypeVarintUnsigned)...)
	in = append(in, 0x80) // truncated varint
	d := newDec(in)
	mustNext(t, d)
	if err := d.Skip(); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("Skip = %v, want ErrIncomplete", err)
	}
}

// --- Next auto-skip error + non-EOF reader error -----------------------------

func TestNextAutoSkipPropagatesError(t *testing.T) {
	// Header for an unsigned value, then a truncated varint. The first Next
	// reads the header; the second Next auto-skips the unconsumed (broken) value
	// and must surface the error.
	d := newDec(append(vhdr(1, sofab.TypeVarintUnsigned), 0x80))
	mustNext(t, d)
	if _, err := d.Next(); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("Next = %v, want ErrIncomplete", err)
	}
}

func TestNextNonEOFReaderError(t *testing.T) {
	sentinel := errors.New("boom")
	d := sofab.NewDecoder(errReader{sentinel})
	if _, err := d.Next(); !errors.Is(err, sentinel) {
		t.Fatalf("Next = %v, want sentinel reader error", err)
	}
}
