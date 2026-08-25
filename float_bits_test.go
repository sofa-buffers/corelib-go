package sofab_test

// Float bit-exactness (CORELIB_PLAN §4.6, §6.5).
//
// §4.6 requires every float payload to round-trip bit-for-bit — ±0, ±inf and
// NaN included — because the corelib never inspects or normalizes the value.
// §6.5 singles out the one way a language can break that: IEEE-754 distinguishes
// a *signaling* NaN from a *quiet* one by a single mantissa bit, and widening an
// fp32 to a double SETS that bit. A decoder that carries an fp32 through a double
// destroys a signaling NaN, so decode → re-encode silently changes the wire
// bytes.
//
// Go has a native float32 and this package moves the payload as bits
// (Float32bits / Float32frombits over a 4-byte load and store), so it satisfies
// §6.5 with no special handling — the spec says as much for native-fp32 targets.
// These tests exist to keep it that way: they are the reason a future change
// that routes an fp32 through a float64 fails here instead of on the wire.
//
// §6.5 makes the *test* normative and spells out its axes, all of which are
// covered below: signaling, quiet and negative NaN, at a scalar fp32 position
// AND an fp32-array position, across decode → re-encode and a materialized walk,
// on EVERY decode surface. It has to live here rather than in the shared vectors
// because JSON cannot represent NaN (§7.1), which is exactly why the spec asks
// each implementation for its own.
//
// Everything compares BIT PATTERNS, never ==: NaN != NaN, so an == assertion
// would pass no matter what came back.

import (
	"bytes"
	"errors"
	"math"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// The payloads §4.6/§6.5 care about. The three NaNs are the point of §6.5; the
// zeroes and infinities are §4.6's other values that a naive normalization would
// disturb.
var bitProbes32 = []struct {
	name string
	bits uint32
}{
	{"signaling NaN", 0x7F800001},
	{"quiet NaN", 0x7FC00001},
	{"negative quiet NaN", 0xFFC00000},
	{"negative signaling NaN", 0xFF800001},
	{"+0", 0x00000000},
	{"-0", 0x80000000},
	{"+inf", 0x7F800000},
	{"-inf", 0xFF800000},
}

var bitProbes64 = []struct {
	name string
	bits uint64
}{
	{"signaling NaN", 0x7FF0000000000001},
	{"quiet NaN", 0x7FF8000000000001},
	{"negative quiet NaN", 0xFFF8000000000000},
	{"+0", 0x0000000000000000},
	{"-0", 0x8000000000000000},
	{"+inf", 0x7FF0000000000000},
	{"-inf", 0xFFF0000000000000},
}

// The message every probe is carried in: the same value at a scalar position
// (id 1) and as an array element (id 2), which are the two positions §6.5 names.
func encodeF32Probe(t *testing.T, f float32) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteFloat32(1, f)
	e.WriteFloat32Array(2, []float32{f})
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return buf.Bytes()
}

func encodeF64Probe(t *testing.T, f float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	e.WriteFloat64(1, f)
	e.WriteFloat64Array(2, []float64{f})
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return buf.Bytes()
}

// f32Capture takes the scalar and the array element back out of a visitor walk.
type f32Capture struct {
	baseV
	scalar, elem float32
	sawS, sawA   bool
}

func (c *f32Capture) Float32(_ sofab.ID, v float32) error {
	c.scalar, c.sawS = v, true
	return nil
}

func (c *f32Capture) Float32Array(_ sofab.ID, a []float32) error {
	if len(a) != 1 {
		return errors.New("expected a one-element array")
	}
	c.elem, c.sawA = a[0], true
	return nil
}

type f64Capture struct {
	baseV
	scalar, elem float64
	sawS, sawA   bool
}

func (c *f64Capture) Float64(_ sofab.ID, v float64) error {
	c.scalar, c.sawS = v, true
	return nil
}

func (c *f64Capture) Float64Array(_ sofab.ID, a []float64) error {
	if len(a) != 1 {
		return errors.New("expected a one-element array")
	}
	c.elem, c.sawA = a[0], true
	return nil
}

// The decode surfaces this package exposes. §6.5 requires the guarantee on every
// one of them — a guard added to a single surface is the defect class the clause
// exists to prevent, so each is driven separately rather than trusting that they
// share a path.
var f32Surfaces = []struct {
	name   string
	decode func(t *testing.T, raw []byte) (scalar, elem float32)
}{
	{"AcceptBytes", func(t *testing.T, raw []byte) (float32, float32) {
		t.Helper()
		c := &f32Capture{}
		if err := acceptBytes(raw, c); err != nil {
			t.Fatalf("AcceptBytes: %v", err)
		}
		if !c.sawS || !c.sawA {
			t.Fatal("AcceptBytes: scalar or array not delivered")
		}
		return c.scalar, c.elem
	}},
	{"Feed", func(t *testing.T, raw []byte) (float32, float32) {
		t.Helper()
		c := &f32Capture{}
		if err := feedIn(raw, 0, c); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if !c.sawS || !c.sawA {
			t.Fatal("Accept: scalar or array not delivered")
		}
		return c.scalar, c.elem
	}},
	{"Feed/1-byte", func(t *testing.T, raw []byte) (float32, float32) {
		t.Helper()
		c := &f32Capture{}
		if err := feedIn(raw, 1, c); err != nil {
			t.Fatalf("Feed: %v", err)
		}
		if !c.sawS || !c.sawA {
			t.Fatal("Feed: scalar or array not delivered")
		}
		return c.scalar, c.elem
	}},
}

// TestFloat32BitsSurviveEverySurface is the §6.5 test: every probe, both
// positions, every decode surface, plus the decode → re-encode round trip that
// is where a widened fp32 actually shows up as changed bytes.
func TestFloat32BitsSurviveEverySurface(t *testing.T) {
	for _, p := range bitProbes32 {
		t.Run(p.name, func(t *testing.T) {
			in := math.Float32frombits(p.bits)
			raw := encodeF32Probe(t, in)

			for _, s := range f32Surfaces {
				t.Run(s.name, func(t *testing.T) {
					scalar, elem := s.decode(t, raw)
					if got := math.Float32bits(scalar); got != p.bits {
						t.Errorf("scalar fp32: got %#08x, want %#08x", got, p.bits)
					}
					if got := math.Float32bits(elem); got != p.bits {
						t.Errorf("fp32 array element: got %#08x, want %#08x", got, p.bits)
					}

					// Decode → re-encode must reproduce the exact wire bytes. A
					// value that lost its signaling bit still *looks* like a NaN
					// above; only this catches it.
					if again := encodeF32Probe(t, scalar); !bytes.Equal(again, raw) {
						t.Errorf("re-encode from scalar changed the bytes:\n got % x\nwant % x", again, raw)
					}
					if again := encodeF32Probe(t, elem); !bytes.Equal(again, raw) {
						t.Errorf("re-encode from element changed the bytes:\n got % x\nwant % x", again, raw)
					}
				})
			}
		})
	}
}

// The fp64 counterpart. §6.5 says a native double carries its own signaling NaN
// and so never needs the raw-bytes path, which is true here — the assertion is
// cheap and keeps the claim honest rather than assumed.
func TestFloat64BitsSurviveRoundTrip(t *testing.T) {
	for _, p := range bitProbes64 {
		t.Run(p.name, func(t *testing.T) {
			in := math.Float64frombits(p.bits)
			raw := encodeF64Probe(t, in)

			c := &f64Capture{}
			if err := acceptBytes(raw, c); err != nil {
				t.Fatalf("AcceptBytes: %v", err)
			}
			if !c.sawS || !c.sawA {
				t.Fatal("scalar or array not delivered")
			}
			if got := math.Float64bits(c.scalar); got != p.bits {
				t.Errorf("scalar fp64: got %#016x, want %#016x", got, p.bits)
			}
			if got := math.Float64bits(c.elem); got != p.bits {
				t.Errorf("fp64 array element: got %#016x, want %#016x", got, p.bits)
			}
			if again := encodeF64Probe(t, c.scalar); !bytes.Equal(again, raw) {
				t.Errorf("re-encode changed the bytes:\n got % x\nwant % x", again, raw)
			}
		})
	}
}

// The payload really is the raw IEEE-754 little-endian pattern (§4.6), not
// something the encoder derived from the value. Checking the bytes directly
// means the round-trip tests above cannot pass by encoding and decoding the same
// mistake twice.
func TestFloat32PayloadIsRawLittleEndian(t *testing.T) {
	const sNaN = 0x7F800001
	raw := encodeF32Probe(t, math.Float32frombits(sNaN))

	// Scalar field: header (id 1, fixlen) + fixlen_word (len 4, subtype fp32) +
	// the 4 payload bytes, little-endian.
	want := []byte{sNaN & 0xFF, (sNaN >> 8) & 0xFF, (sNaN >> 16) & 0xFF, (sNaN >> 24) & 0xFF}
	if !bytes.Contains(raw, want) {
		t.Fatalf("wire bytes % x do not contain the raw fp32 payload % x", raw, want)
	}
}
