package sofab_test

// Wire-level decode tests, one byte string per case, driven through EVERY
// visitor entry point.
//
// CORELIB_PLAN §5.3.1 makes the visitor the only decode surface — "every
// additional surface is a second implementation of every rule in this document"
// — so these cases used to run on the pull parser and are now run on all three
// entry points at once. That is the point of the clause: a guard added to one
// path and not another is the recurring defect, and this table is where it would
// show up as a disagreement rather than as a passing test on the surface the
// author happened to pick.

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func newDec(b []byte) *sofab.Decoder { return sofab.NewDecoder(bytes.NewReader(b)) }

// decodeAll drives one entry point by name and returns the recorded events.
func decodeAll(t *testing.T, surface string, in []byte) ([]string, error) {
	t.Helper()
	var log []string
	r := recorder{&log}
	var err error
	switch surface {
	case "AcceptBytes":
		err = sofab.AcceptBytes(in, r)
	case "Accept":
		err = newDec(in).Accept(r)
	case "AcceptStream":
		err = newDec(in).AcceptStream(r)
	default:
		t.Fatalf("unknown surface %q", surface)
	}
	return log, err
}

var surfaces = []string{"AcceptBytes", "Accept", "AcceptStream"}

// TestDecodeEveryValueKind is the value table: each row is a complete message,
// and every surface must produce exactly the listed events and reach COMPLETE.
func TestDecodeEveryValueKind(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want []string
	}{
		{"unsigned", []byte{0x00, 0x80, 0x01}, []string{evU(0, 128)}},
		{"unsigned max", []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01},
			[]string{evU(0, math.MaxUint64)}},
		{"signed", []byte{0x01, 0x53}, []string{evS(0, -42)}},
		{"fp32", []byte{0x02, 0x20, 0x00, 0x00, 0x80, 0x3F}, []string{evF32(0, 1)}},
		{"fp64", []byte{0x02, 0x41, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xF0, 0x3F},
			[]string{evF64(0, 1)}},
		{"string", []byte{0x02, 0x12, 'h', 'i'}, []string{evStr(0, "hi")}},
		{"string empty", []byte{0x02, 0x02}, []string{evStr(0, "")}},
		{"blob", []byte{0x02, 0x2B, 1, 2, 3, 4, 5}, []string{evBlob(0, []byte{1, 2, 3, 4, 5})}},
		{"unsigned array", []byte{0x03, 0x05, 0x01, 0x02, 0x03, 0x80, 0x80, 0x80, 0x80, 0x08,
			0xFF, 0xFF, 0xFF, 0xFF, 0x0F},
			[]string{evAU(0, []uint64{1, 2, 3, 0x8000_0000, 0xFFFF_FFFF})}},
		{"signed array", []byte{0x04, 0x05, 0x01, 0x03, 0x05, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F,
			0xFE, 0xFF, 0xFF, 0xFF, 0x0F},
			[]string{evAS(0, []int64{-1, -2, -3, -2147483648, 2147483647})}},
		{"fp32 array", []byte{0x05, 0x05, 0x20, 0x00, 0x00, 0x80, 0x3F, 0x00, 0x00, 0x00, 0x40,
			0x00, 0x00, 0x40, 0x40, 0xFF, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0x7F, 0x7F},
			[]string{evAF32(0, []float32{1, 2, 3, -math.MaxFloat32, math.MaxFloat32})}},
		// A zero-count array is valid (§4.7/§4.8) and decodes to an empty slice.
		{"empty unsigned array", []byte{0x03, 0x00}, []string{evAU(0, nil)}},
		// [u 0=42][seq 1: [u 0=42][s 2=-42]][s 2=-42]
		{"nested sequence", []byte{0x00, 0x2A, 0x0E, 0x00, 0x2A, 0x11, 0x53, 0x07, 0x11, 0x53},
			[]string{evU(0, 42), "seqbegin/1", evU(0, 42), evS(2, -42), "seqend", evS(2, -42)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				log, err := decodeAll(t, s, c.in)
				if err != nil {
					t.Fatalf("%s = %v, want COMPLETE", s, err)
				}
				if strings.Join(log, "|") != strings.Join(c.want, "|") {
					t.Fatalf("%s events = %v, want %v", s, log, c.want)
				}
			}
		})
	}
}

// TestDecodeMalformedOnEverySurface is the §5.2.2 half of the same table: each
// row must be INVALID on every surface, never a crash and never INCOMPLETE.
func TestDecodeMalformedOnEverySurface(t *testing.T) {
	// A header whose id field is IDMax+1, wire type unsigned.
	var overID bytes.Buffer
	for h := (uint64(sofab.IDMax) + 1) << 3; ; {
		b := byte(h & 0x7F)
		h >>= 7
		if h != 0 {
			b |= 0x80
		}
		overID.WriteByte(b)
		if h == 0 {
			break
		}
	}
	overID.WriteByte(0x00)

	for _, c := range []struct {
		name string
		in   []byte
	}{
		{"varint over 64 bits", []byte{0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}},
		{"dangling sequence end", []byte{0x07}},
		{"id above ID_MAX", overID.Bytes()},
		{"fp32 fixlen of the wrong length", []byte{0x02, (2 << 3) | 0x00, 0xAA, 0xBB}},
		{"fp64 fixlen of the wrong length", []byte{0x02, (7 << 3) | 0x01, 1, 2, 3, 4, 5, 6, 7}},
		{"reserved fixlen subtype", []byte{0x02, (1 << 3) | 0x04, 0xAA}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				if _, err := decodeAll(t, s, c.in); !errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("%s = %v, want ErrInvalidMsg", s, err)
				}
			}
		})
	}
}

// TestDecodeTolerance is §7.2 item 5b: input that is non-canonical but
// well-formed decodes to the value it denotes on every surface.
func TestDecodeTolerance(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want []string
	}{
		// A non-minimal varint at a field header (§4.1.2).
		{"non-minimal field header", []byte{0x80, 0x00, 0x2A}, []string{evU(0, 42)}},
		// A non-minimal varint in the value itself.
		{"non-minimal value", []byte{0x00, 0x80, 0x80, 0x00}, []string{evU(0, 0)}},
		// A sequence-end header whose id is non-zero but within ID_MAX (§4.9):
		// it closes the innermost sequence and the id is discarded.
		{"sequence end with a non-zero id", []byte{0x06, 0x00, 0x2A, 0x3F},
			[]string{"seqbegin/0", evU(0, 42), "seqend"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				log, err := decodeAll(t, s, c.in)
				if err != nil {
					t.Fatalf("%s = %v, want COMPLETE", s, err)
				}
				if strings.Join(log, "|") != strings.Join(c.want, "|") {
					t.Fatalf("%s events = %v, want %v", s, log, c.want)
				}
			}
		})
	}
}

// TestDecodeSkipsWhatTheVisitorDeclines is §7.2 item 7: a visitor that binds
// only some ids must resync correctly onto the field after the ones it ignored,
// and a declined sub-sequence must be walked whole.
func TestDecodeSkipsWhatTheVisitorDeclines(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 100)
		e.WriteString(2, "skip me")
		e.WriteSequenceBeginLazy(3)
		e.WriteUnsigned(1, 7)
		e.WriteBytes(2, []byte{9, 9, 9})
		e.WriteSequenceEndKeep()
		e.WriteSigned(4, -5)
	})

	for _, s := range surfaces {
		t.Run(s, func(t *testing.T) {
			var log []string
			var err error
			v := &declineSeqV{log: &log}
			switch s {
			case "AcceptBytes":
				err = sofab.AcceptBytes(in, v)
			case "Accept":
				err = newDec(in).Accept(v)
			case "AcceptStream":
				err = newDec(in).AcceptStream(v)
			}
			if err != nil {
				t.Fatalf("%s = %v, want COMPLETE", s, err)
			}
			want := []string{evU(1, 100), evStr(2, "skip me"), evS(4, -5)}
			if strings.Join(log, "|") != strings.Join(want, "|") {
				t.Fatalf("%s events = %v, want %v", s, log, want)
			}
		})
	}
}

// declineSeqV records top-level scalars and declines every sub-sequence, which
// is how a consumer says "no destination for this subtree" (§6.0).
type declineSeqV struct {
	baseV
	log *[]string
}

func (v *declineSeqV) Unsigned(id sofab.ID, x uint64) error {
	*v.log = append(*v.log, evU(id, x))
	return nil
}
func (v *declineSeqV) Signed(id sofab.ID, x int64) error {
	*v.log = append(*v.log, evS(id, x))
	return nil
}
func (v *declineSeqV) String(id sofab.ID, s string) error {
	*v.log = append(*v.log, evStr(id, s))
	return nil
}
func (v *declineSeqV) BeginSequence(sofab.ID) (sofab.Visitor, error) { return nil, nil }

// TestNoPartialEvaluationOfATruncatedFixlenWord is §7.2 item 6's dedicated
// case: a `fixlen_word` cut after its first byte, with that byte carrying a
// RESERVED subtype (0x4–0x7).
//
// The subtype is already settled by the low three bits, so an implementation
// that evaluates it early answers INVALID where §4.1.1 requires INCOMPLETE —
// "an unfinished varint carries no verdict at all". Nothing else in the suite
// reaches this rule: the dangling 0x80 of the ordinary truncation cases carries
// no settled sub-field to peek at.
func TestNoPartialEvaluationOfATruncatedFixlenWord(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
	}{
		// id 0, fixlen; the word's first byte is 0x84: continuation set, low
		// three bits = subtype 0x4, which is reserved. The word never finishes.
		{"reserved subtype 0x4", []byte{0x02, 0x84}},
		{"reserved subtype 0x5", []byte{0x02, 0x85}},
		{"reserved subtype 0x6", []byte{0x02, 0x86}},
		{"reserved subtype 0x7", []byte{0x02, 0x87}},
		// The same rule one level down: a fixlen ARRAY's element word, cut after
		// a first byte that already spells a reserved subtype.
		{"fixlen array element word", []byte{0x05, 0x01, 0x84}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				err := func() error { _, e := decodeAll(t, s, c.in); return e }()
				if !errors.Is(err, sofab.ErrIncomplete) {
					t.Fatalf("%s = %v, want ErrIncomplete (§4.1.1: an unfinished "+
						"varint carries no verdict, not even from a settled subtype)", s, err)
				}
				if errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("%s = %v: the reserved subtype was evaluated before "+
						"the word finished", s, err)
				}
			}
		})
	}
}

// TestNonMinimalFixlenWordAndElementCount completes §7.2 item 5b. The tolerance
// cases already covered were a non-minimal field header, a non-minimal value and
// a non-minimal sequence-end id; item 5b also names a non-minimal `fixlen_word`
// and a non-minimal ELEMENT COUNT, neither of which any decode surface exercised.
func TestNonMinimalFixlenWordAndElementCount(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want []string
	}{
		{
			// id 0, fixlen; word = (3<<3)|str = 0x1A, spelled non-minimally as
			// 0x9A 0x00, then "abc".
			"non-minimal fixlen_word",
			[]byte{0x02, 0x9A, 0x00, 'a', 'b', 'c'},
			[]string{evStr(0, "abc")},
		},
		{
			// id 1, unsigned array; count = 1 spelled non-minimally as 0x81 0x00,
			// then one element.
			"non-minimal element count",
			[]byte{0x0B, 0x81, 0x00, 0x01},
			[]string{evAU(1, []uint64{1})},
		},
		{
			// The fixlen-array form: count 1 non-minimal, then a canonical fp32
			// element word and its four payload bytes.
			"non-minimal fixlen array count",
			[]byte{0x0D, 0x81, 0x00, 0x20, 0x00, 0x00, 0x80, 0x3F},
			[]string{evAF32(1, []float32{1})},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				log, err := decodeAll(t, s, c.in)
				if err != nil {
					t.Fatalf("%s = %v, want COMPLETE (§4.1.2: non-minimal is "+
						"well-formed and normalized away)", s, err)
				}
				if strings.Join(log, "|") != strings.Join(c.want, "|") {
					t.Fatalf("%s events = %v, want %v", s, log, c.want)
				}
			}
		})
	}
}
