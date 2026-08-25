package sofab_test

// MESSAGE_SPEC §7.3 / CORELIB_PLAN §6.3: a field whose wire type contradicts
// what the destination declares is NOT AN ERROR AT ALL. It "MUST be skipped like
// an unknown id, leaving the destination untouched. It is neither
// InvalidMessage nor an argument error, and a decode that meets nothing else
// stays COMPLETE. There is therefore no code for 'invalid usage'."
//
// The port used to carry an ErrTypeMismatch sentinel for this, because the pull
// surface had to report the skip to its caller per read. With the visitor as the
// only decode surface (§5.3.1) there is nothing to report: the visitor's own
// field switch simply does not bind the value, and the decode runs on. This file
// pins that, on every entry point, for every wire type against every other.

import (
	"encoding/hex"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// declaredV is the shape generated code has: one arm per declared field, keyed
// on BOTH the id and the callback the declared type arrives on. A value that
// reaches the wrong callback for its id lands in no arm — which is exactly the
// §7.3 skip, expressed in the destination rather than in the codec.
type declaredV struct {
	baseV
	// id 1 is declared `unsigned`, id 2 `string`, id 3 `fp64`.
	u    uint64
	s    string
	f    float64
	seen int
}

func (v *declaredV) Unsigned(id sofab.ID, x uint64) error {
	if id == 1 {
		v.u, v.seen = x, v.seen+1
	}
	return nil
}

func (v *declaredV) String(id sofab.ID, s string) error {
	if id == 2 {
		v.s, v.seen = s, v.seen+1
	}
	return nil
}

func (v *declaredV) Float64(id sofab.ID, f float64) error {
	if id == 3 {
		v.f, v.seen = f, v.seen+1
	}
	return nil
}

// TestMistypedFieldIsSkippedAndTheDecodeStaysComplete is the §7.3 case: a peer
// sends id 1 as a blob, id 2 as an unsigned and id 3 as an fp32, none of which
// the destination declares. Nothing binds, nothing is reported, and the decode
// reaches COMPLETE — including the trailing field, so resync is proven too.
func TestMistypedFieldIsSkippedAndTheDecodeStaysComplete(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		e.WriteBytes(1, []byte("ABCD")) // declared unsigned
		e.WriteUnsigned(2, 7)           // declared string
		e.WriteFloat32(3, 1.5)          // declared fp64
		e.WriteUnsigned(1, 42)          // the declared type, after the mismatches
		e.WriteString(2, "ok")          //
		e.WriteFloat64(3, 2.5)          //
	})

	for _, s := range surfaces {
		t.Run(s, func(t *testing.T) {
			v := &declaredV{}
			var err error
			switch s {
			case "AcceptBytes":
				err = acceptBytes(in, v)
			case "Feed":
				err = feedIn(in, 0, v)
			case "Feed/1-byte":
				err = feedIn(in, 1, v)
			}
			if err != nil {
				t.Fatalf("%s = %v, want COMPLETE (§7.3 is a skip, not a verdict)", s, err)
			}
			if v.seen != 3 {
				t.Errorf("%s bound %d fields, want the 3 declared ones", s, v.seen)
			}
			if v.u != 42 || v.s != "ok" || v.f != 2.5 {
				t.Errorf("%s bound (%d, %q, %v), want (42, \"ok\", 2.5)", s, v.u, v.s, v.f)
			}
		})
	}
}

// TestMistypedFixlenSubtypeIsSkipped is the subtype half: a fixlen field whose
// SUBTYPE contradicts the declared one is the same §7.3 case, not INVALID —
// unlike a subtype that no schema could have declared, which is a format
// violation (§5.2.2, see TestDecodeMalformedOnEverySurface).
func TestMistypedFixlenSubtypeIsSkipped(t *testing.T) {
	// fixlen id 0, subtype blob, "ABCD" — read by a destination declaring string.
	in, err := hex.DecodeString("022341424344")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range surfaces {
		t.Run(s, func(t *testing.T) {
			v := &declaredV{}
			var derr error
			switch s {
			case "AcceptBytes":
				derr = acceptBytes(in, v)
			case "Feed":
				derr = feedIn(in, 0, v)
			case "Feed/1-byte":
				derr = feedIn(in, 1, v)
			}
			if derr != nil {
				t.Fatalf("%s = %v, want COMPLETE", s, derr)
			}
			if v.seen != 0 || v.s != "" {
				t.Errorf("%s bound something: the destination must be untouched", s)
			}
		})
	}
}

// TestMistypedFieldDoesNotHideMalformedFraming: a §7.3 skip is a jump over a
// VALIDATED word. A field the destination does not bind whose framing is
// malformed is still INVALID — the skip must not become a resync on an
// attacker-chosen stride.
func TestMistypedFieldDoesNotHideMalformedFraming(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
	}{
		// fixlen id 1 with a RESERVED subtype (0x4), which no schema declares.
		{"reserved subtype", []byte{0x0A, (1 << 3) | 0x04, 0xAA, 0x08, 0x2A}},
		// fixlen id 1 declaring fp32 at length 2.
		{"fp32 at the wrong width", []byte{0x0A, (2 << 3) | 0x00, 0xAA, 0xBB, 0x08, 0x2A}},
		// an array whose count exceeds the format ceiling.
		{"count past ARRAY_MAX", []byte{0x0B, 0x80, 0x80, 0x80, 0x80, 0x10}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range surfaces {
				if _, err := decodeAll(t, s, c.in); err == nil {
					t.Fatalf("%s = COMPLETE, want the framing violation reported", s)
				}
			}
		})
	}
}
