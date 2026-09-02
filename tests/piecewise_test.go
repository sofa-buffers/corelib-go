package sofab_test

// The piecewise callback surface (CORELIB_PLAN §6.6.3) and the chunk lifetime
// that goes with it (§6.0), as checked properties.
//
// §6.6.3 forbids a callback that delivers a materialized aggregate, because
// building one obliges the codec to size storage from the wire. What replaces it
// is delivery "in pieces, with the payload's total, this piece's offset, and the
// caller's own buffer as arguments" for a payload, and one callback per element
// for an array. Those two shapes have a contract of their own — how many calls,
// in what order, carrying what — and this file is where it is pinned.

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// pieceLog records every callback the decoder makes, in order, in a form that
// shows the SHAPE of the delivery rather than the value it adds up to.
type pieceLog struct {
	sofab.VisitorBase
	ev []string
}

func (p *pieceLog) add(f string, a ...any) error {
	p.ev = append(p.ev, fmt.Sprintf(f, a...))
	return nil
}

func (p *pieceLog) FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, total int) error {
	return p.add("fixbegin(%d,%d,%d)", id, sub, total)
}

func (p *pieceLog) String(id sofab.ID, total, offset int, chunk []byte) error {
	return p.add("str(%d,%d,%d,%q)", id, total, offset, chunk)
}

func (p *pieceLog) Bytes(id sofab.ID, total, offset int, chunk []byte) error {
	return p.add("blob(%d,%d,%d,%x)", id, total, offset, chunk)
}

func (p *pieceLog) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
	return p.add("arrbegin(%d,%d,%d)", id, kind, count)
}

func (p *pieceLog) ArrayUnsigned(id sofab.ID, i int, v uint64) error {
	return p.add("au(%d,%d,%d)", id, i, v)
}

func (p *pieceLog) ArraySigned(id sofab.ID, i int, v int64) error {
	return p.add("as(%d,%d,%d)", id, i, v)
}

func (p *pieceLog) ArrayFloat32(id sofab.ID, i int, v float32) error {
	return p.add("af32(%d,%d,%08x)", id, i, math.Float32bits(v))
}

func (p *pieceLog) ArrayFloat64(id sofab.ID, i int, v float64) error {
	return p.add("af64(%d,%d,%016x)", id, i, math.Float64bits(v))
}

func (p *pieceLog) ArrayEnd(id sofab.ID) error { return p.add("arrend(%d)", id) }

func (p *pieceLog) Unsigned(id sofab.ID, v uint64) error { return p.add("u(%d,%d)", id, v) }

// TestPayloadArrivesInPieces pins the payload contract: FixlenBegin once, before
// any payload byte, then pieces whose offsets are contiguous and whose totals
// never move — one call when the payload arrives whole, as many as it took to
// feed when it does not.
func TestPayloadArrivesInPieces(t *testing.T) {
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteString(1, "sofabuffers")
		e.WriteBytes(2, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	})

	t.Run("whole", func(t *testing.T) {
		var p pieceLog
		if err := sofab.AcceptBytes(msg, &p); err != nil {
			t.Fatalf("AcceptBytes: %v", err)
		}
		want := []string{
			`fixbegin(1,2,11)`,
			`str(1,11,0,"sofabuffers")`,
			`fixbegin(2,3,4)`,
			`blob(2,4,0,deadbeef)`,
		}
		if strings.Join(p.ev, "|") != strings.Join(want, "|") {
			t.Fatalf("events = %v, want %v", p.ev, want)
		}
	})

	t.Run("one byte at a time", func(t *testing.T) {
		var p pieceLog
		d := sofab.NewDecoder(&p)
		var out sofab.Outcome
		for i := range msg {
			var err error
			if out, err = d.Feed(msg[i : i+1]); err != nil {
				t.Fatalf("Feed: %v", err)
			}
		}
		if out != sofab.Complete {
			t.Fatalf("last Feed = %v, want COMPLETE", out)
		}
		// One piece per byte of payload, offsets contiguous, totals constant —
		// and the string still spells the same word.
		var str, blob []byte
		strPieces, blobPieces := 0, 0
		for _, ev := range p.ev {
			switch {
			case strings.HasPrefix(ev, "str("):
				strPieces++
			case strings.HasPrefix(ev, "blob("):
				blobPieces++
			}
		}
		if strPieces != 11 || blobPieces != 4 {
			t.Fatalf("%d string pieces and %d blob pieces, want 11 and 4", strPieces, blobPieces)
		}
		// The same message through an assembling destination reads back whole.
		var c copyingV
		if err := feedIn(msg, 1, &c); err != nil {
			t.Fatalf("assembling feed: %v", err)
		}
		str, blob = []byte(c.str), c.blob
		if string(str) != "sofabuffers" || !bytes.Equal(blob, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
			t.Fatalf("assembled (%q, %x), want (\"sofabuffers\", deadbeef)", str, blob)
		}
	})
}

// TestEmptyPayloadIsOnePiece pins the one case with no payload byte to hang a
// callback on: an empty string or blob still reaches the destination, exactly
// once, with total 0. A destination that only ever hears about a field through
// its pieces would otherwise never learn the field was present at all.
func TestEmptyPayloadIsOnePiece(t *testing.T) {
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteString(1, "")
		e.WriteBytes(2, nil)
	})
	var p pieceLog
	if err := sofab.AcceptBytes(msg, &p); err != nil {
		t.Fatalf("AcceptBytes: %v", err)
	}
	want := []string{`fixbegin(1,2,0)`, `str(1,0,0,"")`, `fixbegin(2,3,0)`, `blob(2,0,0,)`}
	if strings.Join(p.ev, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", p.ev, want)
	}
}

// TestEmptyPayloadInADeclinedScopeStillCompletes is the same rule from the skip
// side, and it is a real trap: an empty payload has no byte for the machine to
// consume, so a decoder that waits for one after declining the scope reports
// INCOMPLETE for a message that ended exactly at a field boundary.
func TestEmptyPayloadInADeclinedScopeStillCompletes(t *testing.T) {
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteUnsigned(9, 1)
		e.WriteString(2, "")
		e.WriteBytes(3, nil)
		e.WriteSequenceEnd()
	})
	for _, chunk := range []int{0, 1} {
		p := &pieceLog{}
		if err := feedIn(msg, chunk, &decliner{p}); err != nil {
			t.Fatalf("chunk %d: %v, want COMPLETE", chunk, err)
		}
		if len(p.ev) != 0 {
			t.Fatalf("chunk %d: a declined scope delivered %v", chunk, p.ev)
		}
	}
	// And an empty payload as the very last field of an undeclined message is
	// COMPLETE too, delivered as its one piece.
	flat := mustEncode(func(e *sofab.Encoder) { e.WriteString(1, "") })
	p := &pieceLog{}
	if err := feedIn(flat, 1, p); err != nil {
		t.Fatalf("trailing empty string: %v", err)
	}
	if len(p.ev) != 2 {
		t.Fatalf("events = %v, want the header and one empty piece", p.ev)
	}
}

// TestArrayIsBegunOnceAndEndedOnce pins the array contract: ArrayBegin exactly
// once per FIELD and before the first element, one element callback per element,
// ArrayEnd once the declared count has been delivered — and no ArrayEnd at all
// for an array the message truncates, since the count was never reached.
func TestArrayIsBegunOnceAndEndedOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  []byte
		want []string
		err  error
	}{
		{
			name: "unsigned",
			msg:  mustEncode(func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 1, []uint64{7, 8}) }),
			want: []string{"arrbegin(1,0,2)", "au(1,0,7)", "au(1,1,8)", "arrend(1)"},
		},
		{
			name: "signed",
			msg:  mustEncode(func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 1, []int64{-7}) }),
			want: []string{"arrbegin(1,1,1)", "as(1,0,-7)", "arrend(1)"},
		},
		{
			name: "fp32",
			msg:  mustEncode(func(e *sofab.Encoder) { e.WriteFloat32Array(1, []float32{1.5}) }),
			want: []string{"arrbegin(1,2,1)", "af32(1,0,3fc00000)", "arrend(1)"},
		},
		{
			name: "fp64",
			msg:  mustEncode(func(e *sofab.Encoder) { e.WriteFloat64Array(1, []float64{2.5}) }),
			want: []string{"arrbegin(1,3,1)", "af64(1,0,4004000000000000)", "arrend(1)"},
		},
		{
			// count == 0 is no exception: the array still reports its kind, and
			// is followed by no element at all.
			name: "empty unsigned",
			msg:  mustEncode(func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 1, []uint64{}) }),
			want: []string{"arrbegin(1,0,0)", "arrend(1)"},
		},
		{
			// An empty fixlen array still carries its fixlen_word (§4.8), so the
			// kind is known and announced.
			name: "empty fp64",
			msg:  mustEncode(func(e *sofab.Encoder) { e.WriteFloat64Array(1, []float64{}) }),
			want: []string{"arrbegin(1,3,0)", "arrend(1)"},
		},
		{
			// Truncated: the elements that arrived are delivered, and ArrayEnd
			// never fires.
			name: "truncated",
			msg:  []byte{(1 << 3) | 3, 4, 7, 8},
			want: []string{"arrbegin(1,0,4)", "au(1,0,7)", "au(1,1,8)"},
			err:  sofab.ErrIncomplete,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{0, 1} {
				var p pieceLog
				err := feedIn(tc.msg, chunk, &p)
				if tc.err == nil && err != nil {
					t.Fatalf("chunk %d: %v", chunk, err)
				}
				if tc.err != nil && !errors.Is(err, tc.err) {
					t.Fatalf("chunk %d: %v, want %v", chunk, err, tc.err)
				}
				if strings.Join(p.ev, "|") != strings.Join(tc.want, "|") {
					t.Fatalf("chunk %d: events = %v, want %v", chunk, p.ev, tc.want)
				}
			}
		})
	}
}

// TestFloatSplitAcrossChunksIsBitExact drives the landing zone §6.6.2 allows: a
// four- or eight-byte float cut at every interior offset must reach the visitor
// exactly once, complete, with its wire bits intact — a signaling NaN included
// (§6.5), which is the value a decoder that rebuilt the float arithmetically
// would quietly turn into a quiet one.
func TestFloatSplitAcrossChunksIsBitExact(t *testing.T) {
	const sNaN32 = 0x7F800001
	const sNaN64 = 0x7FF0000000000001
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteFloat32(1, math.Float32frombits(sNaN32))
		e.WriteFloat64(2, math.Float64frombits(sNaN64))
		e.WriteFloat32Array(3, []float32{math.Float32frombits(sNaN32)})
	})
	want := []string{
		fmt.Sprintf("f32(%08x)", uint32(sNaN32)),
		fmt.Sprintf("f64(%016x)", uint64(sNaN64)),
		"arrbegin", fmt.Sprintf("af32(%08x)", uint32(sNaN32)), "arrend",
	}
	for cut := 1; cut < len(msg); cut++ {
		var b bitLog
		d := sofab.NewDecoder(&b)
		if _, err := d.Feed(msg[:cut]); err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		out, err := d.Feed(msg[cut:])
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if out != sofab.Complete {
			t.Fatalf("cut %d: last Feed = %v, want COMPLETE", cut, out)
		}
		if strings.Join(b.ev, "|") != strings.Join(want, "|") {
			t.Fatalf("cut %d: events = %v, want %v", cut, b.ev, want)
		}
	}
}

type bitLog struct {
	sofab.VisitorBase
	ev []string
}

func (b *bitLog) Float32(_ sofab.ID, v float32) error {
	b.ev = append(b.ev, fmt.Sprintf("f32(%08x)", math.Float32bits(v)))
	return nil
}

func (b *bitLog) Float64(_ sofab.ID, v float64) error {
	b.ev = append(b.ev, fmt.Sprintf("f64(%016x)", math.Float64bits(v)))
	return nil
}

func (b *bitLog) ArrayBegin(sofab.ID, sofab.ArrayKind, int) error {
	b.ev = append(b.ev, "arrbegin")
	return nil
}

func (b *bitLog) ArrayFloat32(_ sofab.ID, _ int, v float32) error {
	b.ev = append(b.ev, fmt.Sprintf("af32(%08x)", math.Float32bits(v)))
	return nil
}

func (b *bitLog) ArrayEnd(sofab.ID) error {
	b.ev = append(b.ev, "arrend")
	return nil
}

// TestEveryChunkIsScrubbedAfterFeed is §7.2 item 4's chunk-lifetime bullet, and
// it is the one item on that list that could not be written before this port had
// a feed at all: "Overwrite every chunk after `feed` returns — scrub it with a
// fill byte, or free it — and assert the decoded message is unchanged."
//
// The chunk is scrubbed IMMEDIATELY after each Feed, not once at the end, so a
// decoder that kept a slice into a fed chunk reads back the fill pattern on the
// next call rather than surviving to the end of the message.
func TestEveryChunkIsScrubbedAfterFeed(t *testing.T) {
	const str = "a payload long enough to be cut many times over"
	blob := bytes.Repeat([]byte{0x5A, 0xA5}, 40)
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 0xDEADBEEF)
		e.WriteString(2, str)
		e.WriteBytes(3, blob)
		sofab.WriteUnsignedArray(e, 4, []uint64{1, 2, 3, 4, 5})
		e.WriteFloat64(5, 2.5)
	})

	for _, chunk := range []int{1, 3, 7, 16} {
		t.Run(fmt.Sprintf("chunk-%d", chunk), func(t *testing.T) {
			var v copyingV
			d := sofab.NewDecoder(&v)
			scratch := make([]byte, chunk)
			var out sofab.Outcome
			for i := 0; i < len(msg); i += chunk {
				end := i + chunk
				if end > len(msg) {
					end = len(msg)
				}
				n := copy(scratch, msg[i:end])
				var err error
				if out, err = d.Feed(scratch[:n]); err != nil {
					t.Fatalf("Feed: %v", err)
				}
				// The caller reuses its receive buffer the instant Feed returns.
				for k := range scratch {
					scratch[k] = 0xFF
				}
			}
			if out != sofab.Complete {
				t.Fatalf("last Feed = %v, want COMPLETE", out)
			}
			if v.str != str {
				t.Errorf("string = %q after every chunk was scrubbed, want %q", v.str, str)
			}
			if !bytes.Equal(v.blob, blob) {
				t.Errorf("blob = %x after every chunk was scrubbed, want %x", v.blob, blob)
			}
		})
	}
}

// TestDeclinedSubtreeDeliversNothing pins the skip: a scope BeginSequence
// returned nil for delivers no callback of any kind, however deep, and its own
// EndSequence never fires — while the field after it resyncs exactly.
func TestDeclinedSubtreeDeliversNothing(t *testing.T) {
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 10)
		e.WriteSequenceBeginLazy(2)
		e.WriteString(1, "invisible")
		sofab.WriteUnsignedArray(e, 2, []uint64{1, 2, 3})
		e.WriteFloat64Array(3, []float64{1.5})
		e.WriteSequenceBeginLazy(4)
		e.WriteBytes(1, []byte{9, 9})
		e.WriteSequenceEnd()
		e.WriteSequenceEnd()
		e.WriteUnsigned(3, 30)
	})

	for _, chunk := range []int{0, 1, 5} {
		p := &pieceLog{}
		if err := feedIn(msg, chunk, &decliner{p}); err != nil {
			t.Fatalf("chunk %d: %v", chunk, err)
		}
		want := []string{"u(1,10)", "u(3,30)"}
		if strings.Join(p.ev, "|") != strings.Join(want, "|") {
			t.Fatalf("chunk %d: events = %v, want %v", chunk, p.ev, want)
		}
	}

	// Nothing inside a declined scope is capped (§6.2.1: "A skipped field is
	// never capped"), and it holds structurally rather than by a check: a
	// receiver cap lives in a destination callback, and the assertion above is
	// that no callback fires here at all.
}

// decliner forwards the top level to inner and declines every nested scope.
type decliner struct{ inner *pieceLog }

func (d *decliner) Unsigned(id sofab.ID, v uint64) error { return d.inner.Unsigned(id, v) }
func (d *decliner) Signed(sofab.ID, int64) error         { return nil }
func (d *decliner) Float32(sofab.ID, float32) error      { return nil }
func (d *decliner) Float64(sofab.ID, float64) error      { return nil }

func (d *decliner) FixlenBegin(id sofab.ID, s sofab.FixlenSubtype, n int) error {
	return d.inner.FixlenBegin(id, s, n)
}

func (d *decliner) String(id sofab.ID, t, o int, c []byte) error {
	return d.inner.String(id, t, o, c)
}

func (d *decliner) Bytes(id sofab.ID, t, o int, c []byte) error {
	return d.inner.Bytes(id, t, o, c)
}

func (d *decliner) ArrayBegin(id sofab.ID, k sofab.ArrayKind, n int) error {
	return d.inner.ArrayBegin(id, k, n)
}
func (d *decliner) ArrayUnsigned(id sofab.ID, i int, v uint64) error {
	return d.inner.ArrayUnsigned(id, i, v)
}
func (d *decliner) ArraySigned(sofab.ID, int, int64) error    { return nil }
func (d *decliner) ArrayFloat32(sofab.ID, int, float32) error { return nil }
func (d *decliner) ArrayFloat64(sofab.ID, int, float64) error { return nil }
func (d *decliner) ArrayEnd(id sofab.ID) error                { return d.inner.ArrayEnd(id) }

func (d *decliner) BeginSequence(sofab.ID) (sofab.Visitor, error) { return nil, nil }
func (d *decliner) EndSequence() error                            { return nil }
