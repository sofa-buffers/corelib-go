package sofab_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// BeginSequence returning nil skips the subtree (#118).
//
// Before it meant that, a consumer with no destination for a scope had to hand
// over a no-op visitor — and Visitor has no optional methods, so every value in
// the discarded subtree was decoded and every string built before an empty
// method threw it away. These cases pin what nil buys and, just as importantly,
// what it must not change.
//
// Every case runs BOTH surfaces: AcceptBytes over a contiguous buffer and
// AcceptStream over a reader, because the two have separate parse loops and a
// skip that worked on only one of them would be a divergence, not a feature.
func bothPaths(t *testing.T, msg []byte, mk func() sofab.Visitor) (bufErr, streamErr error) {
	t.Helper()
	bufErr = sofab.AcceptBytes(msg, mk())
	streamErr = sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(mk())
	return
}

// declining records what it was handed, and refuses the scope at declineID.
type declining struct {
	log       *[]string
	declineID sofab.ID
	nested    bool // this instance is a child scope
}

func (d declining) note(s string) { *d.log = append(*d.log, s) }

func (d declining) Unsigned(id sofab.ID, v uint64) error   { d.note("u"); return nil }
func (d declining) Signed(sofab.ID, int64) error           { d.note("s"); return nil }
func (d declining) Float32(sofab.ID, float32) error        { d.note("f32"); return nil }
func (d declining) Float64(sofab.ID, float64) error        { d.note("f64"); return nil }
func (d declining) String(id sofab.ID, s string) error     { d.note("str:" + s); return nil }
func (d declining) Bytes(sofab.ID, []byte) error           { d.note("blob"); return nil }
func (d declining) UnsignedArray(sofab.ID, []uint64) error { d.note("ua"); return nil }
func (d declining) SignedArray(sofab.ID, []int64) error    { d.note("sa"); return nil }
func (d declining) Float32Array(sofab.ID, []float32) error { d.note("f32a"); return nil }
func (d declining) Float64Array(sofab.ID, []float64) error { d.note("f64a"); return nil }
func (d declining) EndSequence() error                     { d.note("end"); return nil }

func (d declining) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	d.note("begin")
	if id == d.declineID {
		return nil, nil // no destination for this subtree
	}
	return declining{log: d.log, declineID: d.declineID, nested: true}, nil
}

// A message with a good field, a declined subtree holding one of every shape
// (including a nested scope of its own), and a good field behind it.
func declinedSubtree(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	e := sofab.NewEncoder(&out)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	must(e.WriteUnsigned(1, 7))
	must(e.WriteSequenceBeginLazy(2)) // <- declined
	must(e.WriteUnsigned(1, 99))
	must(e.WriteString(2, "discarded"))
	must(e.WriteBytes(3, []byte{1, 2, 3}))
	must(sofab.WriteUnsignedArray(e, 4, []uint64{1, 2, 3}))
	must(e.WriteSequenceBeginLazy(5)) // a scope inside the declined one
	must(e.WriteUnsigned(1, 1))
	must(e.WriteSequenceEnd())
	must(e.WriteSequenceEnd())
	must(e.WriteUnsigned(9, 6))
	must(e.Flush())
	return out.Bytes()
}

func TestNilBeginSequenceDeliversNothingFromTheSubtree(t *testing.T) {
	msg := declinedSubtree(t)
	for _, tc := range []struct {
		name string
		run  func(sofab.Visitor) error
	}{
		{"buffer", func(v sofab.Visitor) error { return sofab.AcceptBytes(msg, v) }},
		{"stream", func(v sofab.Visitor) error {
			return sofab.NewDecoder(bytes.NewReader(msg)).AcceptStream(v)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log []string
			if err := tc.run(declining{log: &log, declineID: 2}); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// The root's two scalars and the one offer. Nothing from inside, no
			// second offer for the scope nested in it, and no EndSequence for a
			// scope the consumer never took.
			if got := strings.Join(log, ","); got != "u,begin,u" {
				t.Fatalf("event stream = %q, want %q", got, "u,begin,u")
			}
		})
	}
}

func TestDecliningIsNotTheSameAsAcceptingAndIgnoring(t *testing.T) {
	msg := declinedSubtree(t)
	var log []string
	// declineID 99 matches nothing, so the same subtree IS taken: this is the
	// control that shows the events above are absent because of the decline and
	// not because the message lacks them.
	if err := sofab.AcceptBytes(msg, declining{log: &log, declineID: 99}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := strings.Join(log, ",")
	want := "u,begin,u,str:discarded,blob,ua,begin,u,end,end,u"
	if got != want {
		t.Fatalf("control event stream = %q, want %q", got, want)
	}
}

// capped rejects nothing itself; the receiver limits are what is on trial.
type capped struct{ declineID sofab.ID }

func (capped) Unsigned(sofab.ID, uint64) error        { return nil }
func (capped) Signed(sofab.ID, int64) error           { return nil }
func (capped) Float32(sofab.ID, float32) error        { return nil }
func (capped) Float64(sofab.ID, float64) error        { return nil }
func (capped) String(sofab.ID, string) error          { return nil }
func (capped) Bytes(sofab.ID, []byte) error           { return nil }
func (capped) UnsignedArray(sofab.ID, []uint64) error { return nil }
func (capped) SignedArray(sofab.ID, []int64) error    { return nil }
func (capped) Float32Array(sofab.ID, []float32) error { return nil }
func (capped) Float64Array(sofab.ID, []float64) error { return nil }
func (capped) EndSequence() error                     { return nil }
func (c capped) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	if id == c.declineID {
		return nil, nil
	}
	return c, nil
}

// The receiver's caps bound what this consumer is handed. A declined scope hands
// it nothing, so they do not fire there — and they still fire outside it, which
// is the half that makes the first half a rule rather than a hole.
func TestReceiverCapsStandDownInsideADeclinedSubtree(t *testing.T) {
	var out bytes.Buffer
	e := sofab.NewEncoder(&out)
	_ = e.WriteSequenceBeginLazy(2)
	_ = e.WriteString(1, strings.Repeat("x", 64))
	_ = e.WriteSequenceEnd()
	_ = e.Flush()
	msg := out.Bytes()

	lim := []sofab.Option{sofab.WithMaxStringLen(8)}

	if err := sofab.AcceptBytes(msg, capped{declineID: 2}, lim...); err != nil {
		t.Fatalf("a cap must not fire on a string inside a declined subtree: %v", err)
	}
	if err := sofab.NewDecoder(bytes.NewReader(msg), lim...).AcceptStream(capped{declineID: 2}); err != nil {
		t.Fatalf("stream: same, got %v", err)
	}
	// ...and the same bytes with the scope TAKEN are over the cap.
	if err := sofab.AcceptBytes(msg, capped{declineID: 99}, lim...); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("a taken scope must still be capped, got %v", err)
	}
}

// Format ceilings are not the consumer's to waive: they bound what the wire may
// express. A reserved fixlen subtype is malformed wherever it appears.
func TestFormatCeilingsStillApplyInsideADeclinedSubtree(t *testing.T) {
	// 16  field 2, sequence start
	// 02  element id 0, fixlen
	// 04  fixlen word: length 0, subtype 4 — reserved (§4.6)
	msg := []byte{0x16, 0x02, 0x04}
	bufErr, streamErr := bothPaths(t, msg, func() sofab.Visitor { return capped{declineID: 2} })
	if !errors.Is(bufErr, sofab.ErrInvalidMsg) {
		t.Fatalf("buffer: want ErrInvalidMsg, got %v", bufErr)
	}
	if !errors.Is(streamErr, sofab.ErrInvalidMsg) {
		t.Fatalf("stream: want ErrInvalidMsg, got %v", streamErr)
	}
}

// A declined scope is still an OPEN scope: the message has to close it.
func TestTruncationInsideADeclinedSubtreeIsIncomplete(t *testing.T) {
	var out bytes.Buffer
	e := sofab.NewEncoder(&out)
	_ = e.WriteSequenceBeginLazy(2)
	_ = e.WriteUnsigned(1, 1)
	_ = e.WriteSequenceEnd()
	_ = e.Flush()
	full := out.Bytes()
	truncated := full[:len(full)-1] // drop the sequence end

	bufErr, streamErr := bothPaths(t, truncated, func() sofab.Visitor { return capped{declineID: 2} })
	if !errors.Is(bufErr, sofab.ErrIncomplete) {
		t.Fatalf("buffer: want ErrIncomplete, got %v", bufErr)
	}
	if !errors.Is(streamErr, sofab.ErrIncomplete) {
		t.Fatalf("stream: want ErrIncomplete, got %v", streamErr)
	}
}

// The measurement §8 asks for: what declining a subtree actually saves, against
// the no-op visitor that was the only way to decline one before.
//
// The payload is deliberately string-heavy, because a string is where the old
// way paid most: Go's Visitor has no optional methods, so every value was
// decoded and every string COPIED out of the buffer (`string(b)` allocates)
// before an empty method dropped it.
func benchDeclinedPayload() []byte {
	var out bytes.Buffer
	e := sofab.NewEncoder(&out)
	_ = e.WriteUnsigned(1, 7)
	_ = e.WriteSequenceBeginLazy(2)
	for i := 0; i < 200; i++ {
		_ = e.WriteString(sofab.ID(i%64), strings.Repeat("payload", 8))
	}
	_ = e.WriteSequenceEnd()
	_ = e.WriteUnsigned(9, 6)
	_ = e.Flush()
	return out.Bytes()
}

// noop is what a consumer had to hand over before nil meant skip.
type noop struct{}

func (noop) Unsigned(sofab.ID, uint64) error        { return nil }
func (noop) Signed(sofab.ID, int64) error           { return nil }
func (noop) Float32(sofab.ID, float32) error        { return nil }
func (noop) Float64(sofab.ID, float64) error        { return nil }
func (noop) String(sofab.ID, string) error          { return nil }
func (noop) Bytes(sofab.ID, []byte) error           { return nil }
func (noop) UnsignedArray(sofab.ID, []uint64) error { return nil }
func (noop) SignedArray(sofab.ID, []int64) error    { return nil }
func (noop) Float32Array(sofab.ID, []float32) error { return nil }
func (noop) Float64Array(sofab.ID, []float64) error { return nil }
func (noop) EndSequence() error                     { return nil }

// theOldWay accepts the scope and hands it a visitor that throws everything away.
type theOldWay struct{ noop }

// takeAll is the no-op visitor a consumer had to hand over: it accepts every
// scope, including nested ones, and drops everything.
type takeAll struct{ noop }

func (t takeAll) BeginSequence(sofab.ID) (sofab.Visitor, error) { return t, nil }

func (t theOldWay) BeginSequence(sofab.ID) (sofab.Visitor, error) { return takeAll{}, nil }

// theNewWay declines it.
type theNewWay struct{ noop }

func (theNewWay) BeginSequence(sofab.ID) (sofab.Visitor, error) { return nil, nil }

func BenchmarkDeclinedSubtreeOldWay(b *testing.B) {
	msg := benchDeclinedPayload()
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := sofab.AcceptBytes(msg, theOldWay{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeclinedSubtreeNewWay(b *testing.B) {
	msg := benchDeclinedPayload()
	b.SetBytes(int64(len(msg)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := sofab.AcceptBytes(msg, theNewWay{}); err != nil {
			b.Fatal(err)
		}
	}
}
