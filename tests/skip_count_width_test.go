package sofab_test

// Port-local skip cases for the VARINT WIDTH of a skipped field's element count
// and payload length.
//
// Why these exist beside the shared corpus (corelib-go#136): the shared vectors
// pin the skip path for every wire type -- the 36-vector 6x6 skip/matrix, plus
// the axes beside it -- but they reach a two-byte count or length only three
// times (130 elements in `skip_long_int_arrays`, twice, and in
// `skip_long_fixlen_array`; 130-byte payloads in `skip_long_fixlen_payloads`),
// and never reach a three-byte one. A decoder that read a skipped field's count
// or length as a SINGLE varint byte therefore fails just 2 of the 58 shared
// skip vectors, which is thin evidence for a mutation that mis-skips every
// large field on the wire. (Measured: seeding `n &= 0x7f` into the decoder's
// array-count read fails 2 shared skip vectors -- and every case below.)
//
// The corpus is copied verbatim from corelib-c-cpp and must never be extended
// here (CORELIB_PLAN §7.1/§8), so the extra width coverage lives in this port's
// own tests instead. Each case crosses a varint boundary -- 127/128 for one
// byte to two, 16383/16384 for two to three -- on the element count of an
// integer array, on the count and byte length of a fixlen array, and on the
// length in a `fixlen_word`, with a read anchor on each side of the skip. The
// anchor behind the skip is the detector: a length read one byte narrow leaves
// the decoder inside the skipped field and the anchor comes back wrong or the
// message never completes.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// The three ids every message below uses: a read anchor, the skipped field, a
// read anchor. Both anchors carry multi-byte values of their own, so a decoder
// that resynchronised onto the wrong offset cannot accidentally reproduce them.
const (
	skipWidthAnchorA   = sofab.ID(1)
	skipWidthSkippedID = sofab.ID(2)
	skipWidthAnchorB   = sofab.ID(3)

	skipWidthValueA = uint64(0x0BAD_C0DE)
	skipWidthValueB = uint64(0x1234_5678_9ABC)
)

// skipWidthRec records whole values, one line per delivered field. It declares
// only the aggregate callbacks these cases use; the adapter in aggregate_test.go
// supplies the rest.
type skipWidthRec struct{ log *[]string }

func (r skipWidthRec) Unsigned(id sofab.ID, v uint64) error {
	*r.log = append(*r.log, fmt.Sprintf("u/%d=%d", id, v))
	return nil
}

func (r skipWidthRec) UnsignedArray(id sofab.ID, a []uint64) error {
	*r.log = append(*r.log, fmt.Sprintf("ua/%d#%d", id, len(a)))
	return nil
}

func (r skipWidthRec) Float64Array(id sofab.ID, a []float64) error {
	*r.log = append(*r.log, fmt.Sprintf("f64a/%d#%d", id, len(a)))
	return nil
}

func (r skipWidthRec) Bytes(id sofab.ID, b []byte) error {
	*r.log = append(*r.log, fmt.Sprintf("blob/%d#%d", id, len(b)))
	return nil
}

func (r skipWidthRec) String(id sofab.ID, s string) error {
	*r.log = append(*r.log, fmt.Sprintf("str/%d#%d", id, len(s)))
	return nil
}

// skipWidthMsg frames one middle field between the two anchors.
func skipWidthMsg(t *testing.T, middle func(*sofab.Encoder) error) []byte {
	t.Helper()
	var out bytes.Buffer
	e := sofab.NewEncoder(&out)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	must(e.WriteUnsigned(skipWidthAnchorA, skipWidthValueA))
	must(middle(e))
	must(e.WriteUnsigned(skipWidthAnchorB, skipWidthValueB))
	must(e.Flush())
	return out.Bytes()
}

// widthU32s builds n elements that are themselves multi-byte varints, so the
// count is not the only width in play.
func widthU32s(n int) []uint32 {
	a := make([]uint32, n)
	for i := range a {
		a[i] = uint32(i)*7 + 300
	}
	return a
}

func widthF64s(n int) []float64 {
	a := make([]float64, n)
	for i := range a {
		a[i] = float64(i) + 0.5
	}
	return a
}

func widthBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%94 + 33) // printable, so the same buffer serves as a string
	}
	return b
}

// TestSkipCrossesVarintWidths skips one field per case and requires both
// anchors back, exactly. Every case is run through a single buffer and fed one
// byte at a time, so the count or length varint is also split across a chunk
// boundary (§7.2 item 4).
func TestSkipCrossesVarintWidths(t *testing.T) {
	// The boundary sizes: last one-byte varint, first two-byte, one past it,
	// last two-byte, first three-byte.
	for _, n := range []int{0, 1, 127, 128, 129, 16383, 16384} {
		n := n
		t.Run(fmt.Sprintf("int_array/%d", n), func(t *testing.T) {
			msg := skipWidthMsg(t, func(e *sofab.Encoder) error {
				return sofab.WriteUnsignedArray(e, skipWidthSkippedID, widthU32s(n))
			})
			runSkipWidth(t, msg, fmt.Sprintf("ua/%d#%d", skipWidthSkippedID, n))
		})
	}
	// A fixlen array: the element width comes from the fixlen_word, so both the
	// count and the byte length (8 x count) cross a boundary here.
	for _, n := range []int{15, 16, 127, 128, 2047, 2048} {
		n := n
		t.Run(fmt.Sprintf("fixlen_array_fp64/%d", n), func(t *testing.T) {
			msg := skipWidthMsg(t, func(e *sofab.Encoder) error {
				return e.WriteFloat64Array(skipWidthSkippedID, widthF64s(n))
			})
			runSkipWidth(t, msg, fmt.Sprintf("f64a/%d#%d", skipWidthSkippedID, n))
		})
	}
	// A fixlen payload: the length in the fixlen_word, at the same boundaries.
	for _, n := range []int{0, 127, 128, 129, 16383, 16384} {
		n := n
		t.Run(fmt.Sprintf("blob/%d", n), func(t *testing.T) {
			msg := skipWidthMsg(t, func(e *sofab.Encoder) error {
				return e.WriteBytes(skipWidthSkippedID, widthBytes(n))
			})
			runSkipWidth(t, msg, fmt.Sprintf("blob/%d#%d", skipWidthSkippedID, n))
		})
		t.Run(fmt.Sprintf("string/%d", n), func(t *testing.T) {
			msg := skipWidthMsg(t, func(e *sofab.Encoder) error {
				return e.WriteString(skipWidthSkippedID, string(widthBytes(n)))
			})
			runSkipWidth(t, msg, fmt.Sprintf("str/%d#%d", skipWidthSkippedID, n))
		})
	}
}

// runSkipWidth decodes msg twice per entry point: once with the middle field
// skipped (the case under test) and once with nothing skipped (the control that
// shows the field really is there, so a "skip" that swallowed the message would
// not pass as success).
func runSkipWidth(t *testing.T, msg []byte, kept string) {
	t.Helper()
	anchors := []string{
		fmt.Sprintf("u/%d=%d", skipWidthAnchorA, skipWidthValueA),
		fmt.Sprintf("u/%d=%d", skipWidthAnchorB, skipWidthValueB),
	}
	wantSkipped := strings.Join(anchors, "|")
	wantKept := strings.Join([]string{anchors[0], kept, anchors[1]}, "|")

	skip := map[uint32]bool{uint32(skipWidthSkippedID): true}
	for _, ep := range []struct {
		name string
		run  func(sofab.Visitor) error
	}{
		{"AcceptBytes", func(v sofab.Visitor) error { return acceptBytes(msg, v) }},
		{"Feed/1-byte", func(v sofab.Visitor) error { return feedIn(msg, 1, v) }},
	} {
		t.Run(ep.name, func(t *testing.T) {
			var log []string
			// Skipped: the destination has nothing bound to the id, so the
			// decoder must find the next field from the wire alone.
			dest := &skipUnreadV{skip: skip, in: asVisitor(skipWidthRec{log: &log})}
			if err := ep.run(dest); err != nil {
				t.Fatalf("skipped decode = %v, want COMPLETE", err)
			}
			if got := strings.Join(log, "|"); got != wantSkipped {
				t.Fatalf("skipped events =\n %s\nwant\n %s", got, wantSkipped)
			}

			log = nil
			if err := ep.run(asVisitor(skipWidthRec{log: &log})); err != nil {
				t.Fatalf("control decode = %v, want COMPLETE", err)
			}
			if got := strings.Join(log, "|"); got != wantKept {
				t.Fatalf("control events =\n %s\nwant\n %s", got, wantKept)
			}
		})
	}
}
