package sofab_test

import (
	"bytes"
	"math"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// encodeOneArray is the bytes one array field produces on its own.
func encodeOneArray(t *testing.T, write func(*sofab.Encoder)) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	write(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return buf.Bytes()
}

// arrayRecorder captures the one array a message carries.
type arrayRecorder struct {
	baseV
	u    []uint64
	i    []int64
	f32  []float32
	f64  []float64
	seen bool
}

func (r *arrayRecorder) UnsignedArray(_ sofab.ID, v []uint64) error {
	r.u, r.seen = v, true
	return nil
}
func (r *arrayRecorder) SignedArray(_ sofab.ID, v []int64) error {
	r.i, r.seen = v, true
	return nil
}
func (r *arrayRecorder) Float32Array(_ sofab.ID, v []float32) error {
	r.f32, r.seen = v, true
	return nil
}
func (r *arrayRecorder) Float64Array(_ sofab.ID, v []float64) error {
	r.f64, r.seen = v, true
	return nil
}

func decodeOneArray(t *testing.T, raw []byte) *arrayRecorder {
	t.Helper()
	r := &arrayRecorder{}
	if err := sofab.AcceptBytes(raw, r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.seen {
		t.Fatal("no array delivered")
	}
	return r
}

// A trailing element equal to the element default is part of the array's value:
// the wire count M IS the length, so eliding it would shorten the array
// (MESSAGE_SPEC §3, "count is a capacity, not a length"). This is the rule the
// retired TrimTail/PadTo helpers broke, so it is pinned at the library level
// where it actually matters — nothing here can be satisfied by a helper nobody
// calls.
func TestTrailingDefaultsAreNotElided(t *testing.T) {
	full := encodeOneArray(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{1, 2, 0, 0})
	})
	short := encodeOneArray(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{1, 2})
	})
	if bytes.Equal(full, short) {
		t.Fatalf("[1 2 0 0] and [1 2] encoded identically (% x): the trailing "+
			"default run was trimmed", full)
	}
	if got := decodeOneArray(t, full).u; len(got) != 4 {
		t.Fatalf("round trip changed the length: 4 -> %d (%v)", len(got), got)
	}
	if got := decodeOneArray(t, short).u; len(got) != 2 {
		t.Fatalf("round trip changed the length: 2 -> %d (%v)", len(got), got)
	}

	// An all-default array is a value of that length, not an empty one.
	allZero := encodeOneArray(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{0, 0, 0, 0})
	})
	if got := decodeOneArray(t, allZero).u; len(got) != 4 {
		t.Fatalf("all-zero array decoded with length %d, want 4 (%v)", len(got), got)
	}
}

// The same rule for the signed and fixlen array wire types.
func TestTrailingDefaultsAreNotElidedOtherKinds(t *testing.T) {
	signed := encodeOneArray(t, func(e *sofab.Encoder) {
		sofab.WriteSignedArray(e, 1, []int32{-5, 0, 0})
	})
	if got := decodeOneArray(t, signed).i; len(got) != 3 {
		t.Fatalf("signed: length %d, want 3 (%v)", len(got), got)
	}

	f32 := encodeOneArray(t, func(e *sofab.Encoder) {
		e.WriteFloat32Array(1, []float32{1, 0, 0})
	})
	if got := decodeOneArray(t, f32).f32; len(got) != 3 {
		t.Fatalf("fp32: length %d, want 3 (%v)", len(got), got)
	}

	f64 := encodeOneArray(t, func(e *sofab.Encoder) {
		e.WriteFloat64Array(1, []float64{1, 0, 0})
	})
	if got := decodeOneArray(t, f64).f64; len(got) != 3 {
		t.Fatalf("fp64: length %d, want 3 (%v)", len(got), got)
	}
}

// A trailing -0.0 must survive bit-for-bit. It compares == to +0.0, so any
// default-valued trim would drop it and the re-encoded bytes would differ from
// the ones received — a §4.6 bit-exactness violation. NaN is never equal to
// anything, including itself, so it is checked by bit pattern too.
func TestTrailingSignedZeroAndNaNSurvive(t *testing.T) {
	negZero32 := float32(math.Copysign(0, -1))
	raw := encodeOneArray(t, func(e *sofab.Encoder) {
		e.WriteFloat32Array(1, []float32{1, negZero32})
	})
	got := decodeOneArray(t, raw).f32
	if len(got) != 2 {
		t.Fatalf("fp32: length %d, want 2 (%v)", len(got), got)
	}
	if math.Float32bits(got[1]) != math.Float32bits(negZero32) {
		t.Fatalf("trailing -0.0 changed: got bits %#x, want %#x",
			math.Float32bits(got[1]), math.Float32bits(negZero32))
	}

	negZero64, nan := math.Copysign(0, -1), math.NaN()
	raw = encodeOneArray(t, func(e *sofab.Encoder) {
		e.WriteFloat64Array(1, []float64{nan, negZero64})
	})
	got64 := decodeOneArray(t, raw).f64
	if len(got64) != 2 {
		t.Fatalf("fp64: length %d, want 2 (%v)", len(got64), got64)
	}
	if math.Float64bits(got64[0]) != math.Float64bits(nan) {
		t.Fatalf("NaN changed: got bits %#x, want %#x",
			math.Float64bits(got64[0]), math.Float64bits(nan))
	}
	if math.Float64bits(got64[1]) != math.Float64bits(negZero64) {
		t.Fatalf("trailing -0.0 changed: got bits %#x, want %#x",
			math.Float64bits(got64[1]), math.Float64bits(negZero64))
	}
}

// An empty array is M = 0 and stays empty: a declared count never adds elements
// a decoder did not receive (no fill-to-N, MESSAGE_SPEC §3).
func TestEmptyArrayStaysEmpty(t *testing.T) {
	raw := encodeOneArray(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{})
	})
	if got := decodeOneArray(t, raw).u; len(got) != 0 {
		t.Fatalf("empty array decoded with %d elements (%v)", len(got), got)
	}
}

func TestNarrow(t *testing.T) {
	if got := sofab.NarrowUnsigned[uint8]([]uint64{1, 255}); got[1] != 255 {
		t.Errorf("NarrowUnsigned = %v", got)
	}
	if got := sofab.NarrowSigned[int16]([]int64{-1, 32767}); got[0] != -1 || got[1] != 32767 {
		t.Errorf("NarrowSigned = %v", got)
	}
}
