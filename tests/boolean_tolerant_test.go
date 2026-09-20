package sofab_test

// The shared `boolean_tolerant` block — CORELIB_PLAN §4.4, "canonical on
// encode, tolerant on decode".
//
//	An encoder MUST write `true` as 1. A decoder MUST read EVERY value other
//	than 0 as true: such a value is not INVALID (§5.2), it is normalized away,
//	and a re-encode emits 1.
//
// A boolean has no wire type of its own — it is an unsigned varint (§4.4), and
// a boolean array rides the unsigned-varint array (§4.7). So every byte string
// in this block is ordinary, well-formed wire; what is under test is purely how
// the BOOLEAN surface interprets those bytes and what it writes back out.
//
// WHY THE POSITIVE VECTORS CANNOT REACH THIS. A vector's bytes are produced by
// replaying its `fields` through a conforming encoder, and a conforming encoder
// never emits a non-canonical boolean — `WriteBool` here writes 0 or 1 and
// nothing else. Bytes carrying 2, 256 or 2^64-1 at a boolean position only ever
// arrive from SOMEONE ELSE'S encoder, which is why the block is hand-authored
// and separate.
//
// THREE DEFECTS, THREE DIFFERENT ASSERTIONS — and this is the whole reason the
// runner does not stop at the outcome:
//
//	rejects 256 outright        -> caught by the OUTCOME assertion
//	truncates 256 to false      -> caught by the VALUE assertion; the outcome is
//	                               COMPLETE and looks perfect
//	stores the raw 2 unchanged  -> caught by the RE-ENCODE assertion; both the
//	                               outcome and any truthiness check pass, because
//	                               2 is true under every such test
//
// Both of the last two are live defects elsewhere in the family (upstream
// corelib-c-cpp#172 and sofa-buffers/generator#581), and the family-wide roll-out
// of this block is sofa-buffers/crucible#189.
//
// WHERE THE BOOLEAN SURFACE IS IN THIS PORT. The decode surface is Visitor
// (§5.3.1) and it is schema-agnostic: a scalar boolean arrives at
// Visitor.Unsigned carrying the wire value, and the destination — generated code,
// or the runner's boolScalar below — is where §4.4's zero test lives. That split
// is what this file asserts on the scalar path: the corelib must deliver the
// value UNTRUNCATED and UNREJECTED (boolScalar records the raw uint64 for
// exactly that), and the destination's boolean must then be false only for 0.
// The array path needs no such arrangement: BoolMatrixSeq IS this library's
// boolean-array read surface and applies `v != 0` itself (collectors.go), so a
// truncation or a missing normalization there is the library's own.
//
// On the write side both halves are the library's: Encoder.WriteBool for a
// scalar, and WriteUnsignedArray for an array — this port has no boolean-array
// writer, and §4.7 makes the element width an API concern that never reaches the
// wire, so the normalized 0/1 values go out through the narrowest width there is.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// --- the block's shape (test_vectors_README.md) -------------------------------

// boolExpect is the case's expectation: the NORMALIZED values, and the dense
// re-encode of exactly those values at the case's own field id.
type boolExpect struct {
	Outcome      string `json:"outcome"`       // "complete" everywhere in this block
	Values       []bool `json:"values"`        // one entry per element, in wire order
	ReencodedHex string `json:"reencoded_hex"` // what the encoder must emit
}

type boolCase struct {
	Name          string     `json:"name"`
	Group         string     `json:"group"`
	Description   string     `json:"description"`
	Requires      []string   `json:"requires"`
	ID            uint32     `json:"id"`
	SerializedHex string     `json:"serialized_hex"`
	Expect        boolExpect `json:"expect"`
}

func loadBooleanCases(t *testing.T) []boolCase {
	t.Helper()
	vf := loadVectors(t)
	if len(vf.BooleanTolerant) == 0 {
		t.Fatal("vector file carries no boolean_tolerant block; re-copy assets/test_vectors.json " +
			"verbatim from corelib-c-cpp (CORELIB_PLAN §7.1/§8)")
	}
	var cases []boolCase
	if err := json.Unmarshal(vf.BooleanTolerant, &cases); err != nil {
		t.Fatalf("parse boolean_tolerant: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("boolean_tolerant block is empty; a runner that iterates nothing passes while testing nothing")
	}
	return cases
}

// booleanTolerantCap reports whether this build satisfies one `requires` tag.
//
// IN THIS BLOCK AN UNSATISFIED TAG MEANS REJECT, NOT SKIP — the vector rule, not
// the header_limits rule. §4.4 lifts the width bound the TYPE carries, never the
// one a BUILD has: under a 32-bit accumulator (§6.2.2, "scalar value width
// 32-bit") a boolean carrying 2^64-1 overflows before any boolean rule can
// apply, and §6.2 makes rejecting it the conformant answer. Skipping the case
// would assert nothing in precisely the build most likely to answer `true` by
// truncation.
//
// This port has no compile-time feature switches: the Go core ships the whole
// format, so goCaps is complete, every tag is satisfied and the reject path
// below is unreachable here. The gate is written anyway — it is three lines, and
// it is what a profile added later would need. An UNRECOGNIZED tag is satisfied
// too (forward compatibility, as the reference runner does), so a tag the corpus
// adds upstream cannot silently turn a positive case into a rejection.
func booleanTolerantCap(tag string) bool {
	supported, known := goCaps[tag]
	if !known {
		return true
	}
	return supported
}

// booleanCaseGated reports the first tag this build does not satisfy.
func booleanCaseGated(c boolCase) (string, bool) {
	for _, tag := range c.Requires {
		if !booleanTolerantCap(tag) {
			return tag, true
		}
	}
	return "", false
}

// --- what the case's bytes actually carry -------------------------------------

// boolWire is the case read off its own bytes: the field id, whether the
// boolean sits in an array, and the RAW unsigned values at the boolean
// position(s) — the numbers before §4.4 normalizes them.
//
// Reading them is not bookkeeping. The raw values are what the scalar assertion
// compares the corelib's delivery against, and re-deriving them from the bytes
// is what keeps the case's own expectation honest: a case whose `values` did not
// agree with `(raw != 0)` would be a corrupt case, not a defect to chase.
type boolWire struct {
	id    sofab.ID
	array bool
	raw   []uint64
}

func readBoolWire(t *testing.T, c boolCase) boolWire {
	t.Helper()
	b, err := hex.DecodeString(c.SerializedHex)
	if err != nil {
		t.Fatalf("%s: serialized_hex is not hex: %v", c.Name, err)
	}
	h, off := headerVarint(t, c.Name, b, 0)
	w := boolWire{id: sofab.ID(h >> 3)}
	switch sofab.WireType(h & 0x07) {
	case sofab.TypeVarintUnsigned:
		var v uint64
		v, off = headerVarint(t, c.Name, b, off)
		w.raw = []uint64{v}
	case sofab.TypeVarintArrayUnsigned:
		var n uint64
		n, off = headerVarint(t, c.Name, b, off)
		w.array = true
		for i := uint64(0); i < n; i++ {
			var v uint64
			v, off = headerVarint(t, c.Name, b, off)
			w.raw = append(w.raw, v)
		}
	default:
		t.Fatalf("%s: wire type %d carries no boolean; §4.4 puts a boolean on 0b000 and a boolean array on 0b011",
			c.Name, h&0x07)
	}
	if off != len(b) {
		t.Fatalf("%s: %d byte(s) left over after the field; the case is a complete message",
			c.Name, len(b)-off)
	}

	// Cross-checks against what the case SAYS. A disagreement means the block
	// was hand-edited, which §7.1 forbids — never a library defect.
	if uint32(w.id) != c.ID {
		t.Fatalf("%s: bytes carry id %d, the case says id %d", c.Name, w.id, c.ID)
	}
	if len(w.raw) != len(c.Expect.Values) {
		t.Fatalf("%s: bytes carry %d value(s), expect.values has %d",
			c.Name, len(w.raw), len(c.Expect.Values))
	}
	if w.array != (len(c.Expect.Values) > 1) {
		t.Fatalf("%s: wire is array=%v but expect.values has %d entr(ies); the runner picks the "+
			"read surface by that count", c.Name, w.array, len(c.Expect.Values))
	}
	for i, v := range w.raw {
		if (v != 0) != c.Expect.Values[i] {
			t.Fatalf("%s: element %d is %d on the wire but the case expects %v; §4.4 makes 0 the only false",
				c.Name, i, v, c.Expect.Values[i])
		}
	}
	return w
}

// --- the destinations ---------------------------------------------------------

// boolScalar is the scalar boolean destination, in the shape generated code
// binds a `boolean` field in: the value arrives at Visitor.Unsigned (§4.4 puts a
// boolean on the unsigned wire type, and the codec is schema-agnostic — it
// cannot know the field is a boolean), and the destination applies §4.4's zero
// test. `got` is what a re-encode is then driven from.
//
// `raw` is kept beside it so a failure can say WHICH half broke: a corelib that
// truncated 256 into a byte before the callback hands over 0, and the assertion
// on raw names that directly instead of leaving a bare "wanted true, got false".
// Nothing is asserted in the callback itself (§5.3.1 runs it inside the decoder,
// where a t.Fatal would abandon the stream mid-message); it records, and the
// test asserts after the feed returns.
type boolScalar struct {
	sofab.VisitorBase
	id   sofab.ID
	got  bool // poisoned by the caller before the feed
	raw  uint64
	seen int
}

func (d *boolScalar) Unsigned(id sofab.ID, v uint64) error {
	if id != d.id {
		return nil
	}
	d.seen++
	d.raw = v
	d.got = v != 0 // §4.4: every value other than 0 is true
	return nil
}

// boolArray wraps this library's own boolean-array read surface, BoolMatrixSeq,
// and records the element count the wire ANNOUNCED so it can be compared with
// the number of elements that actually arrived — a decoder that delivers fewer
// elements than its own count word declares is caught here and nowhere else.
type boolArray struct {
	sofab.Visitor // the BoolMatrixSeq collector; every other callback goes straight through
	announced     int
	begins        int
}

func (d *boolArray) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
	d.begins++
	d.announced = count
	return d.Visitor.ArrayBegin(id, kind, count)
}

// --- the positive path --------------------------------------------------------

// decodeBooleanCase feeds one case — in `chunk`-byte pieces, 0 meaning one feed
// of the whole message — through the port's boolean read surface, and returns
// the values as they sit in the DECODE DESTINATION afterwards.
//
// The destination is poisoned first (§8.4 of the block's spec): every slot is
// set to the complement of what the case expects, so a decoder that never writes
// it at all fails instead of passing case 1 against a zero-initialized buffer.
func decodeBooleanCase(t *testing.T, c boolCase, w boolWire, chunk int) []bool {
	t.Helper()
	raw, err := hex.DecodeString(c.SerializedHex)
	if err != nil {
		t.Fatalf("%s: serialized_hex: %v", c.Name, err)
	}
	want := c.Expect.Values

	poison := make([]bool, len(want))
	for i := range poison {
		poison[i] = !want[i]
	}

	var dest sofab.Visitor
	var read func() []bool
	var after func()

	if w.array {
		// The row is bounded by the SCHEMA (`count:`), never by a receiver cap:
		// the case states its element count, so the number is the schema's and a
		// cap would be the wrong category (§6.2.1). out is pre-filled with the
		// poisoned row, which PlaceRow overwrites at the field's index.
		out := [][]bool{append([]bool(nil), poison...)}
		coll := sofab.NewBoolMatrixSeq(&out, sofab.Bounds{Count: int(c.ID) + 1},
			sofab.Bounds{Count: len(want)}, sofab.Caps{})
		d := &boolArray{Visitor: coll}
		dest = d
		read = func() []bool {
			if len(out) != 1 {
				t.Fatalf("%s: destination holds %d row(s), want exactly the one at id %d",
					c.Name, len(out), c.ID)
			}
			return out[0]
		}
		after = func() {
			if d.begins != 1 {
				t.Fatalf("%s: ArrayBegin fired %d time(s), want exactly once", c.Name, d.begins)
			}
			if d.announced != len(want) {
				t.Errorf("%s: the count word announced %d element(s), the case carries %d",
					c.Name, d.announced, len(want))
			}
		}
	} else {
		d := &boolScalar{id: sofab.ID(c.ID), got: poison[0]}
		dest = d
		read = func() []bool { return []bool{d.got} }
		after = func() {
			if d.seen != 1 {
				t.Fatalf("%s: the field at id %d arrived %d time(s), want exactly once",
					c.Name, c.ID, d.seen)
			}
			// The corelib's own half of §4.4 on this path: the varint reaches
			// the destination WHOLE. A value masked to the destination's width
			// before the zero test is how 256 silently becomes false.
			if d.raw != w.raw[0] {
				t.Errorf("%s: the decoder delivered %d, the wire carries %d — the value was "+
					"truncated or rewritten before the boolean rule could apply",
					c.Name, d.raw, w.raw[0])
			}
		}
	}

	// A FRESH decoder per feed: a terminal verdict or a retained tail from an
	// earlier case must not reach this one (§5.2.3).
	dec := sofab.NewDecoder(dest)
	out := sofab.Complete
	if chunk <= 0 {
		chunk = len(raw)
	}
	for i := 0; i < len(raw); i += chunk {
		end := min(i+chunk, len(raw))
		out, err = dec.Feed(raw[i:end])
		if err != nil {
			t.Fatalf("%s: feed: %v, want COMPLETE — a tolerated value is not a rejected one (§4.4)",
				c.Name, err)
		}
	}
	if out != sofab.Complete {
		t.Fatalf("%s: outcome %v, want COMPLETE; the bytes are a whole, well-formed message", c.Name, out)
	}
	after()

	got := read()
	if len(got) != len(want) {
		t.Fatalf("%s: destination holds %d value(s), want %d", c.Name, len(got), len(want))
	}
	for i := range want {
		// Strict equality against a real boolean, never a truthiness test: `2`
		// is true under every such test in every language, and that is the
		// defect the re-encode below is the only other witness to.
		if got[i] != want[i] {
			t.Fatalf("%s: value %d = %v, want %v (wire carries %d)",
				c.Name, i, got[i], want[i], w.raw[i])
		}
	}
	return got
}

// reencodeBooleanCase writes the values AS THEY CAME OUT OF THE DECODE back at
// the case's field id, through this port's boolean write surface, and compares
// the bytes with `reencoded_hex`.
//
// `got` is the decode destination's content and never expect.values: feeding the
// JSON expectation back in would make this comparison trivially true and leave
// the decode half unverified.
func reencodeBooleanCase(t *testing.T, c boolCase, w boolWire, got []bool) {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	id := sofab.ID(c.ID)

	var err error
	if w.array {
		// No boolean-array writer exists here, and none is needed: §4.7 makes
		// the element width an API concern that never reaches the wire, so the
		// normalized 0/1 values go out at the narrowest width the port offers.
		elems := make([]uint8, len(got))
		for i, v := range got {
			if v {
				elems[i] = 1
			}
		}
		err = sofab.WriteUnsignedArray(e, id, elems)
	} else {
		err = e.WriteBool(id, got[0])
	}
	if err != nil {
		t.Fatalf("%s: re-encode: %v", c.Name, err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("%s: flush: %v", c.Name, err)
	}

	if hexOut := hex.EncodeToString(buf.Bytes()); hexOut != c.Expect.ReencodedHex {
		t.Fatalf("%s: re-encoded %s, want %s — §4.4 is canonical on encode: a tolerated value is "+
			"NORMALIZED, so the re-encode emits 1 and never the value that was read (wire: %s)",
			c.Name, hexOut, c.Expect.ReencodedHex, c.SerializedHex)
	}
	if got, want := buf.Len(), len(c.Expect.ReencodedHex)/2; got != want {
		t.Fatalf("%s: re-encode is %d bytes, want %d", c.Name, got, want)
	}
}

// --- the reject path (an unsatisfied `requires`) ------------------------------

// rejectBooleanCase is what an unsatisfied tag means HERE: the message must be
// refused, with the §5.2.2 category and terminally. Unreachable in this port
// (goCaps is complete — see booleanTolerantCap) and kept so a narrowed profile
// would inherit the assertion rather than a skip.
func rejectBooleanCase(t *testing.T, c boolCase) {
	t.Helper()
	raw, err := hex.DecodeString(c.SerializedHex)
	if err != nil {
		t.Fatalf("%s: serialized_hex: %v", c.Name, err)
	}
	// A destination that binds nothing: what is asserted is the VERDICT, not
	// what was delivered before the offending value.
	d := sofab.NewDecoder(sofab.VisitorBase{})
	out, ferr := d.Feed(raw)
	if out != sofab.Invalid {
		t.Fatalf("%s: outcome %v, want INVALID — a value past this build's accumulator is §5.2.2 INVALID",
			c.Name, out)
	}
	if !errors.Is(ferr, sofab.ErrInvalidMsg) {
		t.Fatalf("%s: decode: %v, want ErrInvalidMsg", c.Name, ferr)
	}
	if errors.Is(ferr, sofab.ErrLimitExceeded) {
		t.Fatalf("%s: a width overflow was reported as a receiver-cap rejection; that tier is §6.2.1 policy",
			c.Name)
	}
	// Terminal: one further byte must not lift it (§5.2.3).
	if out2, err2 := d.Feed([]byte{0x00}); out2 != sofab.Invalid || err2 != ferr {
		t.Fatalf("%s: a further feed answered %v (%v); the rejection is terminal and must re-raise",
			c.Name, out2, err2)
	}
}

// --- the runner ---------------------------------------------------------------

// TestBooleanTolerant runs every case in the block: decode through the boolean
// read surface, then re-encode the DECODED values through the boolean write
// surface. Both halves always — see the file header for which defect each one is
// the only witness to.
func TestBooleanTolerant(t *testing.T) {
	cases := loadBooleanCases(t)
	found, decoded, rejected, checks := len(cases), 0, 0, 0

	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if c.Group != "boolean/tolerant" {
				t.Errorf("group %q, want \"boolean/tolerant\"", c.Group)
			}
			if tag, gated := booleanCaseGated(c); gated {
				// Counted as RUN, not skipped: the rejection is the assertion
				// this build owes the case.
				rejected++
				checks++
				t.Logf("requires %q, which this build does not carry: the message must be rejected", tag)
				rejectBooleanCase(t, c)
				// Its own row in the captured summary, which is what makes the
				// decoded/rejected split legible in a feature-matrix build: a
				// reduced build shows a non-zero rejected row, a full build
				// shows none at all.
				vecRan("boolean-tolerant/rejected", 1)
				return
			}
			decoded++

			// Read rather than assumed: the block is `complete` throughout
			// today, and a future case with another outcome must fail loudly
			// here instead of being mis-run.
			if c.Expect.Outcome != "complete" {
				t.Fatalf("unexpected outcome %q; this runner implements the `complete` path only",
					c.Expect.Outcome)
			}
			if len(c.Expect.Values) == 0 {
				t.Fatalf("case carries no expected values")
			}

			w := readBoolWire(t, c)

			// One feed of everything, and — because cases 6 and 8 carry
			// ten-byte varints — the same message one byte at a time, where an
			// accumulator that does not survive a feed boundary shows up
			// (§7.2 item 4). The values and the verdict must be identical.
			got := decodeBooleanCase(t, c, w, 0)
			checks++
			gotChunked := decodeBooleanCase(t, c, w, 1)
			checks++
			for i := range got {
				if got[i] != gotChunked[i] {
					t.Fatalf("value %d = %v fed whole but %v fed one byte at a time",
						i, got[i], gotChunked[i])
				}
			}

			reencodeBooleanCase(t, c, w, got)
			checks++

			vecRan("boolean-tolerant", 3) // decode, chunked decode, re-encode
		})
	}

	if found == 0 {
		t.Fatal("boolean_tolerant found no cases; a run that iterates nothing is a failure, not a pass")
	}
	if decoded+rejected != found {
		t.Errorf("%d case(s) found but %d decoded + %d rejected", found, decoded, rejected)
	}
	t.Logf("[boolean_tolerant] %d found: %d decoded, %d rejected, %d checks",
		found, decoded, rejected, checks)
	fmt.Printf("[boolean_tolerant] %d found: %d decoded, %d rejected, %d checks\n",
		found, decoded, rejected, checks)
}

// TestBooleanTolerantInventory is the guard on the block, in the shape of
// TestHeaderLimitsInventory: floors and structure, never equalities, so upstream
// growing the block cannot fail this port while a block that SHRANK — or lost
// the property it exists for — is caught.
func TestBooleanTolerantInventory(t *testing.T) {
	cases := loadBooleanCases(t)
	if len(cases) < minBooleanTolerant {
		t.Errorf("boolean_tolerant carries %d cases, want at least %d -- re-copy "+
			"assets/test_vectors.json verbatim from corelib-c-cpp (CORELIB_PLAN §7.1/§8)",
			len(cases), minBooleanTolerant)
	}

	names := map[string]bool{}
	scalars, arrays, tagged := 0, 0, 0
	var widest uint64
	var widestElem uint64
	normalized := 0 // cases whose re-encode differs from the bytes read

	for _, c := range cases {
		if names[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		names[c.Name] = true
		if c.Expect.Outcome != "complete" {
			t.Errorf("%s: outcome %q; a tolerated value is not a rejected one (§4.4)",
				c.Name, c.Expect.Outcome)
		}
		if len(c.Requires) > 0 {
			tagged++
		}
		w := readBoolWire(t, c)
		if w.array {
			arrays++
			for _, v := range w.raw {
				if v > widestElem {
					widestElem = v
				}
			}
		} else {
			scalars++
			if w.raw[0] > widest {
				widest = w.raw[0]
			}
		}
		if c.Expect.ReencodedHex != c.SerializedHex {
			normalized++
		}
	}

	if scalars == 0 {
		t.Error("no scalar case; the scalar read surface is untested")
	}
	if arrays == 0 {
		t.Error("no array case; a runner that lost them loses the element-level half of §4.4")
	}
	if tagged == 0 {
		t.Error("no case carries `requires`; the gate (reject, not skip) is then never described")
	}
	if normalized == 0 {
		t.Error("every case re-encodes to the bytes it was read from; nothing then proves " +
			"normalization, which is the half only the re-encode can see")
	}
	// The truncation traps: a scalar past one byte, and an array element past
	// one byte. Without them a decoder that masks the accumulated varint down to
	// its destination width passes the whole block.
	if widest <= 0xFF {
		t.Errorf("the widest scalar is %d; a case above 255 is what catches a value masked to "+
			"the destination's width before the zero test", widest)
	}
	if widestElem <= 0xFF {
		t.Errorf("the widest array element is %d; same trap, one level down", widestElem)
	}

	t.Logf("[boolean_tolerant] %d cases: %d scalar, %d array, %d tagged, %d normalized on re-encode; "+
		"widest scalar %d, widest element %d",
		len(cases), scalars, arrays, tagged, normalized, widest, widestElem)
}

// TestBooleanTolerantEncodeIsCanonical is the other half of §4.4, asserted
// against this port's encoder directly rather than through a case: whatever a
// caller hands WriteBool, the bytes are the canonical 0 or 1. The block can only
// observe this for values a decode produced; this covers the surface itself.
func TestBooleanTolerantEncodeIsCanonical(t *testing.T) {
	for _, tc := range []struct {
		v    bool
		want string
	}{{false, "0000"}, {true, "0001"}} {
		var buf bytes.Buffer
		e := sofab.NewEncoder(&buf)
		if err := e.WriteBool(0, tc.v); err != nil {
			t.Fatalf("WriteBool(%v): %v", tc.v, err)
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if got := hex.EncodeToString(buf.Bytes()); got != tc.want {
			t.Errorf("WriteBool(%v) = %s, want %s", tc.v, got, tc.want)
		}
	}
}
