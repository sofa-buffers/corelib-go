package sofab_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// An integer array's DECLARED ELEMENT WIDTH is a validity bound (MESSAGE_SPEC
// §7.1): a value the schema's element type cannot hold is INVALID, and a decoder
// must never narrow it silently. On the visitor surface the bound travels in
// through ElemBoundVisitor (elem_bound_test.go) because the array callback hands
// over the whole slice at once. The PULL surface needs no hook at all — the
// caller instantiates ReadUnsignedArray[T]/ReadSignedArray[T] with the declared
// element type, so the width is right there in T — yet both readers used to do a
// bare `T(v)` conversion, which is the mask §7.1 forbids (issue #83).

// arrayWire assembles one integer array field: header, count, then the raw
// element varints exactly as given (already zigzag-mapped for a signed array).
func arrayWire(id uint64, signed bool, elems []uint64) []byte {
	typ := uint64(3)
	if signed {
		typ = 4
	}
	out := binary.AppendUvarint(nil, id<<3|typ)
	out = binary.AppendUvarint(out, uint64(len(elems)))
	for _, e := range elems {
		out = binary.AppendUvarint(out, e)
	}
	return out
}

// zz is the zigzag mapping an encoder applies to a signed array element.
func zz(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// atField positions a decoder on the message's first field, ready for a typed
// read.
func atField(t *testing.T, raw []byte) *sofab.Decoder {
	t.Helper()
	d := sofab.NewDecoder(bytes.NewReader(raw))
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	return d
}

// TestPullUnsignedArrayElementWidth pins the reject for every narrowed unsigned
// width, each against the control one step below it: the bound is the declared
// type's own range, so the largest value that fits must still decode.
func TestPullUnsignedArrayElementWidth(t *testing.T) {
	cases := []struct {
		name  string
		elems []uint64
		read  func(*sofab.Decoder) (uint64, error)
		want  error // nil = must decode
	}{
		{"u8 over", []uint64{300, 5}, readU[uint8], sofab.ErrInvalidMsg},
		{"u8 at max", []uint64{255, 5}, readU[uint8], nil},
		{"u16 over", []uint64{math.MaxUint16 + 1, 5}, readU[uint16], sofab.ErrInvalidMsg},
		{"u16 at max", []uint64{math.MaxUint16, 5}, readU[uint16], nil},
		{"u32 over", []uint64{math.MaxUint32 + 1, 5}, readU[uint32], sofab.ErrInvalidMsg},
		{"u32 at max", []uint64{math.MaxUint32, 5}, readU[uint32], nil},
		// u64 spans the value domain: no wire value can breach it, so the guard
		// must not invent one.
		{"u64 top of domain", []uint64{math.MaxUint64, 5}, readU[uint64], nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := atField(t, arrayWire(0, false, tc.elems))
			got, err := tc.read(d)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("read: %v, want success", err)
				}
				if got != tc.elems[0] {
					t.Fatalf("element 0 = %d, want %d", got, tc.elems[0])
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("read: %v (first element %d), want %v", err, got, tc.want)
			}
		})
	}
}

// TestPullSignedArrayElementWidth is the same table for the signed widths, and
// checks both ends: zigzag maps a negative element to a small wire value, so an
// underflow below the declared minimum is just as invisible to a magnitude-only
// guard as an overflow above the maximum.
func TestPullSignedArrayElementWidth(t *testing.T) {
	cases := []struct {
		name  string
		elems []int64
		read  func(*sofab.Decoder) (int64, error)
		want  error
	}{
		{"i8 over", []int64{128, 5}, readS[int8], sofab.ErrInvalidMsg},
		{"i8 under", []int64{-129, 5}, readS[int8], sofab.ErrInvalidMsg},
		{"i8 at max", []int64{127, 5}, readS[int8], nil},
		{"i8 at min", []int64{-128, 5}, readS[int8], nil},
		{"i16 over", []int64{math.MaxInt16 + 1, 5}, readS[int16], sofab.ErrInvalidMsg},
		{"i16 under", []int64{math.MinInt16 - 1, 5}, readS[int16], sofab.ErrInvalidMsg},
		{"i16 at min", []int64{math.MinInt16, 5}, readS[int16], nil},
		{"i32 over", []int64{math.MaxInt32 + 1, 5}, readS[int32], sofab.ErrInvalidMsg},
		{"i32 under", []int64{math.MinInt32 - 1, 5}, readS[int32], sofab.ErrInvalidMsg},
		{"i32 at max", []int64{math.MaxInt32, 5}, readS[int32], nil},
		{"i64 bottom of domain", []int64{math.MinInt64, 5}, readS[int64], nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := make([]uint64, len(tc.elems))
			for i, v := range tc.elems {
				raw[i] = zz(v)
			}
			d := atField(t, arrayWire(0, true, raw))
			got, err := tc.read(d)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("read: %v, want success", err)
				}
				if got != tc.elems[0] {
					t.Fatalf("element 0 = %d, want %d", got, tc.elems[0])
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("read: %v (first element %d), want %v", err, got, tc.want)
			}
		})
	}
}

// TestPullArrayElementWidthBatchPath walks the OTHER element loop. Both readers
// split into a buffered batch (readVarintBatch, varintChunk elements at a time)
// and a one-at-a-time tail near the end of the stream; the short arrays above
// only ever take the tail. Here the breaching element sits deep inside a long
// array, with enough bytes behind it that the batch keeps winning — a guard
// placed on the tail alone would let it through.
func TestPullArrayElementWidthBatchPath(t *testing.T) {
	const n = 200
	const bad = 100 // well past the first varintChunk of 32

	u := make([]uint64, n)
	for i := range u {
		u[i] = 1
	}
	u[bad] = 256
	d := atField(t, arrayWire(0, false, u))
	if v, err := sofab.ReadUnsignedArray[uint8](d); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("unsigned: %v (%d elements), want ErrInvalidMsg", err, len(v))
	}

	s := make([]uint64, n)
	for i := range s {
		s[i] = zz(1)
	}
	s[bad] = zz(-129)
	d = atField(t, arrayWire(0, true, s))
	if v, err := sofab.ReadSignedArray[int8](d); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("signed: %v (%d elements), want ErrInvalidMsg", err, len(v))
	}
}

// TestPullArrayElementWidthOutranksTruncation is the §5.2 ordering on the pull
// surface: the over-width element is fully on the wire, so the array being cut
// off behind it cannot downgrade INVALID to INCOMPLETE. The control one line
// down decides nothing before the cut, and there the truncation IS the verdict.
func TestPullArrayElementWidthOutranksTruncation(t *testing.T) {
	full := arrayWire(0, false, []uint64{300, 5, 5, 5, 5})
	cut := full[:len(full)-3] // count says 5, only the 300 arrives

	d := atField(t, cut)
	if _, err := sofab.ReadUnsignedArray[uint8](d); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("over-width then truncated: %v, want ErrInvalidMsg", err)
	}

	ctl := arrayWire(0, false, []uint64{200, 5, 5, 5, 5})
	d = atField(t, ctl[:len(ctl)-3])
	if _, err := sofab.ReadUnsignedArray[uint8](d); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("in-range then truncated: %v, want ErrIncomplete", err)
	}
}

// readU/readS adapt the generic readers to the tables above, which need one
// non-generic function type per kind.
func readU[T sofab.Unsigned](d *sofab.Decoder) (uint64, error) {
	v, err := sofab.ReadUnsignedArray[T](d)
	if len(v) == 0 {
		return 0, err
	}
	return uint64(v[0]), err
}

func readS[T sofab.Signed](d *sofab.Decoder) (int64, error) {
	v, err := sofab.ReadSignedArray[T](d)
	if len(v) == 0 {
		return 0, err
	}
	return int64(v[0]), err
}
