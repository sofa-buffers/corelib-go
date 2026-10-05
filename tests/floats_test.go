package sofab_test

// Unit tests for BitsEqual (floats.go, generator#636).
//
// The helper's one job is to disagree with an IEEE elementwise compare exactly
// where bit patterns and IEEE equality part ways: -0.0 against +0.0, and NaN
// against NaN. Every case below is therefore also checked against refBits, a
// plain reference loop written here, so a change to the helper cannot move the
// expectation with it.

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

type f32s []float32
type f64s []float64

func refBits32(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float32bits(a[i]) != math.Float32bits(b[i]) {
			return false
		}
	}
	return true
}

func refBits64(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return false
		}
	}
	return true
}

func negZero32() float32 { return math.Float32frombits(0x80000000) }
func negZero64() float64 { return math.Float64frombits(1 << 63) }

type bitsCase32 struct {
	name string
	a, b []float32
	want bool
}

type bitsCase64 struct {
	name string
	a, b []float64
	want bool
}

func TestBitsEqualFloat32(t *testing.T) {
	nz := negZero32()
	nanA := math.Float32frombits(0x7fc00000)
	nanB := math.Float32frombits(0x7fc00001) // same class, different payload
	snan := math.Float32frombits(0x7fa00000)
	negNaN := math.Float32frombits(0xffc00000)
	inf, ninf := float32(math.Inf(1)), float32(math.Inf(-1))
	sub := math.Float32frombits(1)
	cases := []bitsCase32{
		{"both empty", []float32{}, []float32{}, true},
		{"nil and empty", nil, []float32{}, true},
		{"one equal", []float32{1.5}, []float32{1.5}, true},
		{"one differs", []float32{1.5}, []float32{2.5}, false},
		{"equal", []float32{0, 1.5, 3}, []float32{0, 1.5, 3}, true},
		{"-0 vs +0 first", []float32{nz, 1.5, 3}, []float32{0, 1.5, 3}, false},
		{"-0 vs +0 middle", []float32{0, nz, 3}, []float32{0, 0, 3}, false},
		{"-0 vs +0 last", []float32{0, 1.5, nz}, []float32{0, 1.5, 0}, false},
		{"+0 vs -0", []float32{0}, []float32{nz}, false},
		{"-0 equals -0", []float32{nz}, []float32{nz}, true},
		{"same NaN bits", []float32{nanA}, []float32{nanA}, true},
		{"NaN payload differs", []float32{nanA}, []float32{nanB}, false},
		{"quiet vs signaling NaN", []float32{nanA}, []float32{snan}, false},
		{"signaling NaN equals itself", []float32{snan}, []float32{snan}, true},
		{"NaN sign differs", []float32{nanA}, []float32{negNaN}, false},
		{"NaN vs number", []float32{nanA}, []float32{1}, false},
		{"+inf equal", []float32{inf}, []float32{inf}, true},
		{"+inf vs -inf", []float32{inf}, []float32{ninf}, false},
		{"subnormal equal", []float32{sub}, []float32{sub}, true},
		{"subnormal vs zero", []float32{sub}, []float32{0}, false},
		{"subnormal neighbours", []float32{sub}, []float32{math.Float32frombits(2)}, false},
		{"a longer", []float32{1, 2, 3}, []float32{1, 2}, false},
		{"b longer", []float32{1, 2}, []float32{1, 2, 3}, false},
		{"empty vs one", []float32{}, []float32{0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sofab.BitsEqual(c.a, c.b); got != c.want {
				t.Fatalf("BitsEqual = %v, want %v", got, c.want)
			}
			if got := sofab.BitsEqual(c.b, c.a); got != c.want {
				t.Fatalf("swapped: BitsEqual = %v, want %v", got, c.want)
			}
			if ref := refBits32(c.a, c.b); ref != c.want {
				t.Fatalf("test table disagrees with the reference loop: %v", ref)
			}
		})
	}
}

func TestBitsEqualFloat64(t *testing.T) {
	nz := negZero64()
	nanA := math.Float64frombits(0x7ff8000000000000)
	nanB := math.Float64frombits(0x7ff8000000000001)
	snan := math.Float64frombits(0x7ff4000000000000)
	negNaN := math.Float64frombits(0xfff8000000000000)
	inf, ninf := math.Inf(1), math.Inf(-1)
	sub := math.Float64frombits(1)
	cases := []bitsCase64{
		{"both empty", []float64{}, []float64{}, true},
		{"nil and empty", nil, []float64{}, true},
		{"one equal", []float64{1.5}, []float64{1.5}, true},
		{"one differs", []float64{1.5}, []float64{2.5}, false},
		{"equal", []float64{0, 1.5, 3}, []float64{0, 1.5, 3}, true},
		{"-0 vs +0 first", []float64{nz, 1.5, 3}, []float64{0, 1.5, 3}, false},
		{"-0 vs +0 middle", []float64{0, nz, 3}, []float64{0, 0, 3}, false},
		{"-0 vs +0 last", []float64{0, 1.5, nz}, []float64{0, 1.5, 0}, false},
		{"+0 vs -0", []float64{0}, []float64{nz}, false},
		{"-0 equals -0", []float64{nz}, []float64{nz}, true},
		{"same NaN bits", []float64{nanA}, []float64{nanA}, true},
		{"NaN payload differs", []float64{nanA}, []float64{nanB}, false},
		{"quiet vs signaling NaN", []float64{nanA}, []float64{snan}, false},
		{"signaling NaN equals itself", []float64{snan}, []float64{snan}, true},
		{"NaN sign differs", []float64{nanA}, []float64{negNaN}, false},
		{"NaN vs number", []float64{nanA}, []float64{1}, false},
		{"+inf equal", []float64{inf}, []float64{inf}, true},
		{"+inf vs -inf", []float64{inf}, []float64{ninf}, false},
		{"subnormal equal", []float64{sub}, []float64{sub}, true},
		{"subnormal vs zero", []float64{sub}, []float64{0}, false},
		{"subnormal neighbours", []float64{sub}, []float64{math.Float64frombits(2)}, false},
		{"low word only differs", []float64{math.Float64frombits(0x3ff0000000000000)}, []float64{math.Float64frombits(0x3ff0000000000001)}, false},
		{"a longer", []float64{1, 2, 3}, []float64{1, 2}, false},
		{"b longer", []float64{1, 2}, []float64{1, 2, 3}, false},
		{"empty vs one", []float64{}, []float64{0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sofab.BitsEqual(c.a, c.b); got != c.want {
				t.Fatalf("BitsEqual = %v, want %v", got, c.want)
			}
			if got := sofab.BitsEqual(c.b, c.a); got != c.want {
				t.Fatalf("swapped: BitsEqual = %v, want %v", got, c.want)
			}
			if ref := refBits64(c.a, c.b); ref != c.want {
				t.Fatalf("test table disagrees with the reference loop: %v", ref)
			}
		})
	}
}

// TestBitsEqualDisagreesWithIEEE pins the reason the helper exists: the
// elementwise compare the generated code used before it.
func TestBitsEqualDisagreesWithIEEE(t *testing.T) {
	def32 := []float32{0, 1.5}
	got32 := []float32{negZero32(), 1.5}
	if !slices.Equal(got32, def32) {
		t.Fatal("precondition: IEEE equality treats -0.0 as +0.0")
	}
	if sofab.BitsEqual(got32, def32) {
		t.Fatal("fp32: [-0.0, 1.5] must not equal the default [0.0, 1.5]")
	}
	def64 := []float64{0, 1.5}
	got64 := []float64{negZero64(), 1.5}
	if !slices.Equal(got64, def64) {
		t.Fatal("precondition: IEEE equality treats -0.0 as +0.0")
	}
	if sofab.BitsEqual(got64, def64) {
		t.Fatal("fp64: [-0.0, 1.5] must not equal the default [0.0, 1.5]")
	}
	nan32 := []float32{float32(math.NaN())}
	if slices.Equal(nan32, nan32) {
		t.Fatal("precondition: IEEE equality treats NaN as unequal to itself")
	}
	if !sofab.BitsEqual(nan32, nan32) {
		t.Fatal("a NaN default must equal itself bit for bit")
	}
}

func TestBitsEqualSameArray(t *testing.T) {
	a := []float32{negZero32(), float32(math.NaN()), 1}
	if !sofab.BitsEqual(a, a) {
		t.Fatal("an array must equal itself")
	}
	b := []float64{negZero64(), math.NaN(), 1}
	if !sofab.BitsEqual(b, b) {
		t.Fatal("an array must equal itself")
	}
}

// TestBitsEqualNamedSlices covers the shape generated code can use: a named
// slice type, and a mix of a named slice with a literal.
func TestBitsEqualNamedSlices(t *testing.T) {
	if !sofab.BitsEqual(f32s{0, 1.5}, f32s{0, 1.5}) {
		t.Fatal("named float32 slices must compare equal")
	}
	if sofab.BitsEqual(f32s{negZero32(), 1.5}, []float32{0, 1.5}) {
		t.Fatal("named float32 slice with -0.0 must differ from the literal default")
	}
	if !sofab.BitsEqual(f64s{0, 1.5}, f64s{0, 1.5}) {
		t.Fatal("named float64 slices must compare equal")
	}
	if sofab.BitsEqual(f64s{negZero64(), 1.5}, []float64{0, 1.5}) {
		t.Fatal("named float64 slice with -0.0 must differ from the literal default")
	}
}

// TestBitsEqualLong differs in exactly one element, at the start, middle and
// end of arrays well past any block size.
func TestBitsEqualLong(t *testing.T) {
	for _, n := range []int{64, 65, 100, 257, 4096} {
		for _, pos := range []int{0, n / 2, n - 1} {
			t.Run(fmt.Sprintf("n=%d/pos=%d", n, pos), func(t *testing.T) {
				a32 := make([]float32, n)
				b32 := make([]float32, n)
				a64 := make([]float64, n)
				b64 := make([]float64, n)
				for i := range a32 {
					a32[i] = float32(i) * 0.25
					a64[i] = float64(i) * 0.25
				}
				copy(b32, a32)
				copy(b64, a64)
				if !sofab.BitsEqual(a32, b32) || !sofab.BitsEqual(a64, b64) {
					t.Fatal("identical arrays must be equal")
				}
				// one-ulp difference
				b32[pos] = math.Float32frombits(math.Float32bits(b32[pos]) ^ 1)
				b64[pos] = math.Float64frombits(math.Float64bits(b64[pos]) ^ 1)
				if sofab.BitsEqual(a32, b32) || sofab.BitsEqual(a64, b64) {
					t.Fatal("a one-bit difference must be found")
				}
				// signed-zero difference
				a32[pos], b32[pos] = 0, negZero32()
				a64[pos], b64[pos] = 0, negZero64()
				if sofab.BitsEqual(a32, b32) || sofab.BitsEqual(a64, b64) {
					t.Fatal("-0.0 against +0.0 must be found")
				}
				if sofab.BitsEqual(a32, b32[:n-1]) || sofab.BitsEqual(a64[:n-1], b64) {
					t.Fatal("a length mismatch must be unequal")
				}
			})
		}
	}
}

// TestBitsEqualRandomCrossCheck compares the helper with the reference loop on
// deterministic pseudo-random arrays drawn from a pool rich in the awkward
// patterns, so equal and unequal outcomes both occur often.
func TestBitsEqualRandomCrossCheck(t *testing.T) {
	rng := rand.New(rand.NewSource(636))
	pool32 := []uint32{0, 0x80000000, 1, 0x7f800000, 0xff800000, 0x7fc00000, 0x7fc00001, 0x7fa00000, 0x3fc00000}
	pool64 := []uint64{0, 1 << 63, 1, 0x7ff0000000000000, 0xfff0000000000000, 0x7ff8000000000000, 0x7ff8000000000001, 0x3ff8000000000000}
	var equal32, equal64 int
	for it := 0; it < 4000; it++ {
		n := rng.Intn(130)
		a32, b32 := make([]float32, n), make([]float32, n)
		a64, b64 := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			a32[i] = math.Float32frombits(pool32[rng.Intn(len(pool32))])
			a64[i] = math.Float64frombits(pool64[rng.Intn(len(pool64))])
			b32[i], b64[i] = a32[i], a64[i]
			if rng.Intn(200) == 0 {
				b32[i] = math.Float32frombits(rng.Uint32())
			}
			if rng.Intn(200) == 0 {
				b64[i] = math.Float64frombits(rng.Uint64())
			}
		}
		if rng.Intn(10) == 0 {
			b32 = b32[:rng.Intn(n+1)]
			b64 = b64[:rng.Intn(n+1)]
		}
		w32, w64 := refBits32(a32, b32), refBits64(a64, b64)
		if w32 {
			equal32++
		}
		if w64 {
			equal64++
		}
		if got := sofab.BitsEqual(a32, b32); got != w32 {
			t.Fatalf("fp32 iter %d: got %v, want %v", it, got, w32)
		}
		if got := sofab.BitsEqual(a64, b64); got != w64 {
			t.Fatalf("fp64 iter %d: got %v, want %v", it, got, w64)
		}
	}
	if equal32 < 200 || equal64 < 200 || equal32 > 3800 || equal64 > 3800 {
		t.Fatalf("cross-check is lopsided: %d / %d equal of 4000", equal32, equal64)
	}
}

func TestBitsEqualDoesNotAllocate(t *testing.T) {
	a := []float32{0, 1.5, 3}
	b := []float32{0, 1.5, 3}
	c := []float64{0, 1.5, 3}
	d := []float64{0, 1.5, 3}
	if n := testing.AllocsPerRun(100, func() {
		_ = sofab.BitsEqual(a, b)
		_ = sofab.BitsEqual(c, d)
	}); n != 0 {
		t.Fatalf("BitsEqual allocated %v times per run", n)
	}
}

// TestBitsEqualEveryLengthAndPosition sweeps the lengths around the point where
// the helper changes strategy (element-wise for short arrays, one block compare
// beyond), flipping each single bit pattern of interest at every position,
// including the first and last byte of the element, so that neither strategy
// can miss a difference the other would find.
func TestBitsEqualEveryLengthAndPosition(t *testing.T) {
	flips32 := []uint32{1, 1 << 7, 1 << 8, 1 << 23, 0x80000000, 0x00400000}
	flips64 := []uint64{1, 1 << 7, 1 << 8, 1 << 52, 1 << 63, 1 << 51}
	for n := 0; n <= 40; n++ {
		a32, a64 := make([]float32, n), make([]float64, n)
		for i := range a32 {
			a32[i] = float32(i) + 0.5
			a64[i] = float64(i) + 0.5
		}
		b32, b64 := slices.Clone(a32), slices.Clone(a64)
		if !sofab.BitsEqual(a32, b32) || !sofab.BitsEqual(a64, b64) {
			t.Fatalf("n=%d: identical arrays must be equal", n)
		}
		for pos := 0; pos < n; pos++ {
			for _, f := range flips32 {
				b := slices.Clone(a32)
				b[pos] = math.Float32frombits(math.Float32bits(b[pos]) ^ f)
				if got, want := sofab.BitsEqual(a32, b), refBits32(a32, b); got != want || got {
					t.Fatalf("fp32 n=%d pos=%d flip=%#x: got %v, want %v", n, pos, f, got, want)
				}
			}
			for _, f := range flips64 {
				b := slices.Clone(a64)
				b[pos] = math.Float64frombits(math.Float64bits(b[pos]) ^ f)
				if got, want := sofab.BitsEqual(a64, b), refBits64(a64, b); got != want || got {
					t.Fatalf("fp64 n=%d pos=%d flip=%#x: got %v, want %v", n, pos, f, got, want)
				}
			}
		}
	}
}
