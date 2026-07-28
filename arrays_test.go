package sofab_test

import (
	"math"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func TestTrimTail(t *testing.T) {
	// Only the TRAILING run goes; an interior default stays.
	if got := sofab.TrimTail([]uint32{1, 0, 2, 0, 0}, 0); len(got) != 3 || got[2] != 2 {
		t.Errorf("TrimTail = %v, want [1 0 2]", got)
	}
	// An all-default array trims to empty. The FIELD is then omitted entirely if
	// its declared default is the empty collection; if that default is non-empty,
	// the explicitly empty array must still reach the wire (the wrapper closes
	// with WriteSequenceEndKeep), because absence would reconstruct the non-empty
	// default instead (MESSAGE_SPEC §2).
	if got := sofab.TrimTail([]uint32{0, 0, 0, 0}, 0); len(got) != 0 {
		t.Errorf("all-default array should trim to empty, got %v", got)
	}
	if got := sofab.TrimTail([]uint32{}, 0); len(got) != 0 {
		t.Errorf("empty slice should stay empty, got %v", got)
	}
	// The element default is not necessarily zero.
	if got := sofab.TrimTail([]uint8{7, 1, 7, 7}, 7); len(got) != 2 {
		t.Errorf("non-zero default: TrimTail = %v, want [7 1]", got)
	}
}

// A trailing -0.0 must survive. == would trim it (since -0.0 == 0.0) and the
// re-encoded bytes would differ from the ones received — a §4.6 violation.
func TestTrimTailFloatComparesBits(t *testing.T) {
	if got := sofab.TrimTailFloat32([]float32{1, float32(math.Copysign(0, -1))}); len(got) != 2 {
		t.Errorf("-0.0 is not the element default, got %v", got)
	}
	if got := sofab.TrimTailFloat64([]float64{1, math.Copysign(0, -1)}); len(got) != 2 {
		t.Errorf("-0.0 is not the element default, got %v", got)
	}
	// +0.0 is.
	if got := sofab.TrimTailFloat32([]float32{1, 0, 0}); len(got) != 1 {
		t.Errorf("TrimTailFloat32 = %v, want [1]", got)
	}
	// NaN is never the default.
	if got := sofab.TrimTailFloat64([]float64{math.NaN()}); len(got) != 1 {
		t.Errorf("NaN must not be trimmed, got %v", got)
	}
}

func TestPadTo(t *testing.T) {
	if got := sofab.PadTo([]uint32{1, 2}, 4, 0); len(got) != 4 || got[3] != 0 {
		t.Errorf("PadTo = %v, want [1 2 0 0]", got)
	}
	// Already at or past the target: unchanged.
	if got := sofab.PadTo([]uint32{1, 2, 3, 4}, 4, 0); len(got) != 4 {
		t.Errorf("PadTo must not shrink, got %v", got)
	}
	if got := sofab.PadTo([]uint32{1, 2, 3, 4, 5}, 4, 0); len(got) != 5 {
		t.Errorf("PadTo must not truncate, got %v", got)
	}
}

// TrimTail then PadTo is the encode/decode round trip of a fixed-count array:
// whatever the encoder elides, the decoder must put back.
func TestTrimThenPadRestoresTheCount(t *testing.T) {
	orig := []uint32{7, 8, 0, 0, 0}
	back := sofab.PadTo(sofab.TrimTail(orig, 0), len(orig), 0)
	if len(back) != len(orig) {
		t.Fatalf("round trip changed the length: %d -> %d", len(orig), len(back))
	}
	for i := range orig {
		if back[i] != orig[i] {
			t.Errorf("element %d: got %d, want %d", i, back[i], orig[i])
		}
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
