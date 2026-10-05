package sofab_test

// Benchmarks for BitsEqual against slices.Equal, the IEEE compare the
// generated omission test used before generator#636. Short arrays are the
// common case (every serialize and every isDefault call runs one); the long
// sizes show the block compare. "last" differs only in the final element, so
// the whole array is walked before the answer.

import (
	"fmt"
	"slices"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

var benchSizes = []int{2, 3, 16, 256, 4096}

var floatBenchSink bool

func benchPair[E float32 | float64](n int, last bool) (a, b []E) {
	a = make([]E, n)
	b = make([]E, n)
	for i := range a {
		a[i] = E(i) + 0.5
		b[i] = a[i]
	}
	if last {
		b[n-1] += 1
	}
	return a, b
}

func benchEqual[E float32 | float64](b *testing.B, eq func(x, y []E) bool) {
	for _, n := range benchSizes {
		for _, last := range []bool{false, true} {
			name := fmt.Sprintf("n=%d/equal", n)
			if last {
				name = fmt.Sprintf("n=%d/last", n)
			}
			x, y := benchPair[E](n, last)
			b.Run(name, func(b *testing.B) {
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
