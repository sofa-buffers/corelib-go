package sofab_test

// Benchmarks for BitsEqual against slices.Equal, the IEEE compare the
// generated omission test used before generator#636. Short arrays are the
// common case (every serialize and every isDefault call runs one); the long
// sizes show the block compare. "first" differs in the first element, the
// commonest way for a field to leave its default; "last" differs only in the
// final element, so the whole array is walked before the answer.

import (
	"fmt"
	"slices"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

var benchSizes = []int{2, 3, 16, 17, 32, 256, 4096}

var floatBenchSink bool

// benchPair builds two arrays of n elements, differing at index diff (-1: none).
func benchPair[E float32 | float64](n, diff int) (a, b []E) {
	a = make([]E, n)
	b = make([]E, n)
	for i := range a {
		a[i] = E(i) + 0.5
		b[i] = a[i]
	}
	if diff >= 0 {
		b[diff] += 1
	}
	return a, b
}

func benchEqual[E float32 | float64](b *testing.B, eq func(x, y []E) bool) {
	for _, n := range benchSizes {
		for _, c := range []struct {
			name string
			diff int
		}{{"equal", -1}, {"first", 0}, {"last", n - 1}} {
			x, y := benchPair[E](n, c.diff)
			b.Run(fmt.Sprintf("n=%d/%s", n, c.name), func(b *testing.B) {
				var r bool
				for i := 0; i < b.N; i++ {
					r = eq(x, y)
				}
				floatBenchSink = r
			})
		}
	}
}

func BenchmarkBitsEqualFloat32(b *testing.B) {
	benchEqual(b, func(x, y []float32) bool { return sofab.BitsEqual(x, y) })
}
func BenchmarkBitsEqualFloat64(b *testing.B) {
	benchEqual(b, func(x, y []float64) bool { return sofab.BitsEqual(x, y) })
}
func BenchmarkSlicesEqualFloat32(b *testing.B) {
	benchEqual(b, func(x, y []float32) bool { return slices.Equal(x, y) })
}
func BenchmarkSlicesEqualFloat64(b *testing.B) {
	benchEqual(b, func(x, y []float64) bool { return slices.Equal(x, y) })
}

// BenchmarkBitsEqualLiteral is the shape generated code has: a field slice
// against a literal default.
func BenchmarkBitsEqualLiteral(b *testing.B) {
	f := []float32{0, 1.5}
	var r bool
	for i := 0; i < b.N; i++ {
		r = sofab.BitsEqual(f, []float32{0, 1.5})
	}
	floatBenchSink = r
}
func BenchmarkSlicesEqualLiteral(b *testing.B) {
	f := []float32{0, 1.5}
	var r bool
	for i := 0; i < b.N; i++ {
		r = slices.Equal(f, []float32{0, 1.5})
	}
	floatBenchSink = r
}
