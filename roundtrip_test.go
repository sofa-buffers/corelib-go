package sofab_test

// §7.2 item 3: encode → decode → compare, for representative messages, on every
// visitor entry point.

import (
	"math"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func TestRoundTripScalars(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(1, math.MaxUint64)
		e.WriteSigned(2, math.MinInt64)
		e.WriteBool(3, true)
		e.WriteFloat32(4, math.Pi)
		e.WriteFloat64(5, math.E)
		e.WriteString(6, "SofaBuffers")
		e.WriteBytes(7, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	})
	want := []string{
		evU(1, math.MaxUint64),
		evS(2, math.MinInt64),
		evU(3, 1), // a bool is an unsigned 0/1 on the wire (§4.4)
		evF32(4, math.Pi),
		evF64(5, math.E),
		evStr(6, "SofaBuffers"),
		evBlob(7, []byte{0xDE, 0xAD, 0xBE, 0xEF}),
	}
	assertRoundTrip(t, in, want)
}

func TestRoundTripArrays(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint16{1, 2, 3, 65535})
		sofab.WriteSignedArray(e, 2, []int32{-1, 0, 1, math.MaxInt32})
		e.WriteFloat32Array(3, []float32{1.5, -2.5})
		e.WriteFloat64Array(4, []float64{math.SmallestNonzeroFloat64, math.MaxFloat64})
	})
	want := []string{
		evAU(1, []uint64{1, 2, 3, 65535}),
		evAS(2, []int64{-1, 0, 1, math.MaxInt32}),
		evAF32(3, []float32{1.5, -2.5}),
		evAF64(4, []float64{math.SmallestNonzeroFloat64, math.MaxFloat64}),
	}
	assertRoundTrip(t, in, want)
}

func TestRoundTripNestedSequences(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 1)
		e.WriteSequenceBeginLazy(2)
		e.WriteUnsigned(1, 2)
		e.WriteSequenceBeginLazy(3)
		e.WriteString(1, "deep")
		e.WriteSequenceEnd()
		e.WriteSequenceEnd()
		e.WriteSigned(4, -9)
	})
	want := []string{
		evU(1, 1),
		"seqbegin/2", evU(1, 2),
		"seqbegin/3", evStr(1, "deep"), "seqend",
		"seqend",
		evS(4, -9),
	}
	assertRoundTrip(t, in, want)
}

// assertRoundTrip decodes in on all three entry points and compares the events.
func assertRoundTrip(t *testing.T, in []byte, want []string) {
	t.Helper()
	for _, s := range surfaces {
		log, err := decodeAll(t, s, in)
		if err != nil {
			t.Fatalf("%s = %v, want COMPLETE", s, err)
		}
		if strings.Join(log, "|") != strings.Join(want, "|") {
			t.Fatalf("%s events =\n %v\nwant\n %v", s, log, want)
		}
	}
}
