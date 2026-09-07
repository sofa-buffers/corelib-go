package sofab_test

// The shared `header_limits` block — CORELIB_PLAN §6.2.1 / §6.3, MESSAGE_SPEC §5.2.
//
// It carries the TRUNCATED OVER-CEILING HEADER: bytes that DECLARE a length or
// a count and then END, with not one payload byte behind them.
//
//	02 a2 06   then EOF
//	^^ id 0, wire type 2 (fixlen)
//	   ^^^^^ the length word (100 << 3) | 2 -- a 100-byte STRING is declared
//	           ... and the message ends.
//
// A conformant decoder answers AT THAT WORD, before the payload is asked for,
// so the answer is the ceiling's and it is TERMINAL. INCOMPLETE is wrong here:
// §5.2.1 defines it as the outcome more bytes CAN change, and after a ceiling
// has fired nothing can (§6.3 calls the rejection terminal; §6.2.1 imports
// §5.2.3's reason for deciding at the header by name).
//
// WHICH CEILING SPEAKS IS THE SUBJECT, and the two give opposite answers on the
// same word — a case carries `schema` or `limits`, never both, because §6.2.1
// forbids applying a receiver cap to a field the schema already bounds:
//
//	"schema": { "maxlen": N }   -> a breach is INVALID          (MESSAGE_SPEC §7.1)
//	"limits": { "max_dyn_…": N } -> a breach is LIMIT_EXCEEDED   (CORELIB_PLAN §6.2.1)
//
// header_string_schema_bounded and header_string_over_cap carry the IDENTICAL
// bytes and differ only in which ceiling the case configures; that pair is what
// keeps the two categories apart, and TestHeaderLimitsInventory pins that it is
// still in the block.
//
// Where the ceiling lives in this port. §6.2.1 keeps the numbers out of the
// codec ("the numbers and the allocation are not the codec's"), so the guard is
// in the DESTINATION: the collector layer's StringSeq.FixlenBegin /
// BlobSeq.FixlenBegin / UnsignedMatrixSeq.ArrayBegin, each routing its length or
// count word through overLen. Binding a collector as the destination is
// therefore not a convenience here — it is what makes the block exercise this
// library's own guard rather than an assertion written in the test.
//
// The two bounds are handed over separately and exclusively, exactly as the
// block states them: a `schema` case configures Bounds.ElemLen and NO cap at
// all, a `limits` case configures the Caps entry and NO schema bound. Neither
// route can borrow the other's number, and a port that consulted the missing one
// would get ErrArgument (§6.3's caller defect) rather than an accidental pass.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// carriesReceiverCaps is this port's answer to the `receiver_caps` capability
// the block gates on. Like `dynamic_arrays` on the growth block — and unlike the
// wire-construct tags in goCaps — it is a PROFILE capability: a port declares it
// when its generated code carries §6.2.1 receiver caps DISTINCT from schema
// bounds. This one does. Caps (max_dyn_array_count / max_dyn_string_len /
// max_dyn_blob_len) and Bounds (`count:` / `maxlen:`) are separate arguments to
// every collector, they are mutually exclusive per bound, and a breach of one is
// ErrLimitExceeded where a breach of the other is ErrInvalidMsg.
const carriesReceiverCaps = true

// headerLimitsCap reports whether this port satisfies one `requires` tag of the
// block.
//
// IN THIS BLOCK AN UNSATISFIED TAG MEANS SKIP, FOR EVERY TAG — which is not how
// a *vector* treats one. A vector's unsatisfied wire-construct tag turns the
// vector into a negative case, because a build compiled without the construct
// rejects any message carrying it. These cases already assert a rejection WITH A
// SPECIFIC CATEGORY, so such a build would reject for an unrelated reason and
// appear to pass while testing nothing.
func headerLimitsCap(tag string) bool {
	if tag == "receiver_caps" {
		return carriesReceiverCaps
	}
	return goCaps[tag]
}

// --- the block's shape (test_vectors_README.md) -------------------------------

// headerCeiling is the ceiling a case configures: exactly one of `schema` and
// `limits`, under the key naming the bound.
type headerCeiling struct {
	MaxDynStringLen  *int `json:"max_dyn_string_len"`
	MaxDynBlobLen    *int `json:"max_dyn_blob_len"`
	MaxDynArrayCount *int `json:"max_dyn_array_count"`
	MaxLen           *int `json:"maxlen"`
}

type headerExpect struct {
	Outcome  string `json:"outcome"`
	Terminal bool   `json:"terminal"`
}

type headerCase struct {
	Name        string         `json:"name"`
	Group       string         `json:"group"`
	Description string         `json:"description"`
	Requires    []string       `json:"requires"`
	FieldID     int            `json:"field_id"`
	Declared    uint64         `json:"declared"`
	Limits      *headerCeiling `json:"limits"`
	Schema      *headerCeiling `json:"schema"`
	Serialized  string         `json:"serialized"`
	Chunks      []string       `json:"chunks"`
	Expect      headerExpect   `json:"expect"`
}

func loadHeaderCases(t *testing.T) []headerCase {
	t.Helper()
	vf := loadVectors(t)
	if len(vf.HeaderLimits) == 0 {
		t.Fatal("vector file carries no header_limits block")
	}
	var cases []headerCase
	if err := json.Unmarshal(vf.HeaderLimits, &cases); err != nil {
		t.Fatalf("parse header_limits: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("header_limits block is empty")
	}
	return cases
}

// --- reading the header the case is made of ----------------------------------

// The bounds here are ABSOLUTE, not cap-relative as in sequence_growth: the case
// IS a fixed byte string, so the declared number is baked into the varint and
// the case instead TELLS the port which ceiling to configure. What the case does
// not spell out is which CONSTRUCT the header opens, so read it off the bytes —
// the one description that cannot drift from them.

// headerVarint reads one varint at off, returning its value and the offset past
// it. The header of a case is complete in `serialized` even when `chunks` splits
// it, so a truncated varint here is a corrupt case.
func headerVarint(t *testing.T, name string, b []byte, off int) (uint64, int) {
	t.Helper()
	var v uint64
	var shift uint
	for off < len(b) {
		c := b[off]
		off++
		v |= uint64(c&0x7F) << shift
		if c&0x80 == 0 {
			return v, off
		}
		shift += 7
		if shift > 63 {
			t.Fatalf("%s: varint wider than 64 bits", name)
		}
	}
	t.Fatalf("%s: serialized ends inside a varint", name)
	return 0, 0
}

// headerShape is what the case's bytes declare: the construct whose ceiling is
// under test, the field id, and the length or count word's value.
type headerShape struct {
	construct string // "string" | "blob" | "array"
	id        sofab.ID
	declared  uint64
}

func readHeaderShape(t *testing.T, c headerCase) headerShape {
	t.Helper()
	raw, err := hex.DecodeString(c.Serialized)
	if err != nil {
		t.Fatalf("%s: serialized is not hex: %v", c.Name, err)
	}
	h, off := headerVarint(t, c.Name, raw, 0)
	s := headerShape{id: sofab.ID(h >> 3)}
	switch sofab.WireType(h & 0x07) {
	case sofab.TypeFixlen:
		w, _ := headerVarint(t, c.Name, raw, off)
		s.declared = w >> 3
		switch sofab.FixlenSubtype(w & 0x07) {
		case sofab.FixlenStr:
			s.construct = "string"
		case sofab.FixlenBlob:
			s.construct = "blob"
		default:
			t.Fatalf("%s: fixlen subtype %d carries no ceiling of its own", c.Name, w&0x07)
		}
	case sofab.TypeVarintArrayUnsigned, sofab.TypeVarintArraySigned:
		s.declared, _ = headerVarint(t, c.Name, raw, off)
		s.construct = "array"
	default:
		t.Fatalf("%s: wire type %d is not a construct with a length or count header", c.Name, h&0x07)
	}
	// The case states both the id and the number its bytes declare; disagreement
	// means the block was hand-edited, which §7.1 forbids.
	if int(s.id) != c.FieldID {
		t.Fatalf("%s: bytes carry id %d, the case says field_id %d", c.Name, s.id, c.FieldID)
	}
	if s.declared != c.Declared {
		t.Fatalf("%s: bytes declare %d, the case says declared %d", c.Name, s.declared, c.Declared)
	}
	return s
}

// --- the destination the ceiling lives in -------------------------------------

// headerDestination builds the collector this case's ceiling is configured on,
// and returns it together with a closure reporting how many values it bound.
//
// The two ceilings are wired in mutually exclusive:
//
//   - `schema` -> Bounds.ElemLen (a string/blob maxlen) or the row's Bounds.Count
//     (an array count), with Caps LEFT EMPTY. A breach is ErrInvalidMsg.
//   - `limits` -> the matching Caps entry, with the schema bound LEFT AT ZERO.
//     A breach is ErrLimitExceeded.
//
// The OUTER bound is Bounds{Count: field_id + 1} in both cases and is not the
// subject: these cases are top-level fields, so the id is a position the schema
// declares, never an index a receiver cap has to bound. Keeping it on the schema
// route is what leaves the case's own ceiling as the only cap in play.
func headerDestination(t *testing.T, c headerCase, s headerShape) (sofab.Visitor, func() int) {
	t.Helper()
	if (c.Schema == nil) == (c.Limits == nil) {
		t.Fatalf("%s: a case carries exactly one of `schema` and `limits` (§6.2.1)", c.Name)
	}
	outer := sofab.Bounds{Count: c.FieldID + 1}
	elem := sofab.Bounds{Count: c.FieldID + 1}
	row := sofab.Bounds{}
	var caps sofab.Caps

	if c.Schema != nil {
		if c.Schema.MaxLen == nil {
			t.Fatalf("%s: `schema` states no maxlen", c.Name)
		}
		switch s.construct {
		case "array":
			row.Count = *c.Schema.MaxLen
		default:
			elem.ElemLen = *c.Schema.MaxLen
		}
	} else {
		switch {
		case c.Limits.MaxDynStringLen != nil:
			caps.StringLen = *c.Limits.MaxDynStringLen
		case c.Limits.MaxDynBlobLen != nil:
			caps.BlobLen = *c.Limits.MaxDynBlobLen
		case c.Limits.MaxDynArrayCount != nil:
			caps.ArrayCount = *c.Limits.MaxDynArrayCount
		default:
			t.Fatalf("%s: `limits` names no §6.2.1 cap", c.Name)
		}
	}

	switch s.construct {
	case "string":
		var out []string
		return sofab.NewStringSeq(&out, elem, caps), func() int { return len(out) }
	case "blob":
		var out [][]byte
		return sofab.NewBlobSeq(&out, elem, caps), func() int { return len(out) }
	case "array":
		var out [][]uint64
		return sofab.NewUnsignedMatrixSeq[uint64](&out, outer, row, caps, 0), func() int { return len(out) }
	}
	t.Fatalf("%s: no destination for construct %q", c.Name, s.construct)
	return nil, nil
}

// --- feeding, and the verdict --------------------------------------------------

// feedHeaderCase feeds the case — as `chunks` where the case splits the bytes,
// otherwise as one feed of `serialized` — and returns the decoder together with
// the outcome of the last feed.
//
// The chunked case is not decoration: header_string_over_cap_split divides the
// LENGTH VARINT ITSELF, so the ceiling fires on a word no single feed delivered
// whole. The verdict is a property of the bytes and not of how they were chunked
// (§7.2 item 4).
func feedHeaderCase(t *testing.T, c headerCase, v sofab.Visitor) (*sofab.Decoder, sofab.Outcome, error) {
	t.Helper()
	pieces := c.Chunks
	if len(pieces) == 0 {
		pieces = []string{c.Serialized}
	}
	d := sofab.NewDecoder(v)
	var out sofab.Outcome
	var err error
	for i, p := range pieces {
		b, derr := hex.DecodeString(p)
		if derr != nil {
			t.Fatalf("%s: chunk %d is not hex: %v", c.Name, i, derr)
		}
		out, err = d.Feed(b)
	}
	return d, out, err
}

// terminalProbe is a COMPLETE, valid message: one unsigned field at id 0 with
// value 0. Fed after a terminal rejection it is the sharpest question available
// — a decoder that consumed it would answer COMPLETE, so anything but the
// re-raised verdict is visible.
var terminalProbe = []byte{0x00, 0x00}

func TestHeaderLimits(t *testing.T) {
	cases := loadHeaderCases(t)
	ran, skipped := 0, 0

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, tag := range c.Requires {
				if !headerLimitsCap(tag) {
					skipped++
					t.Skipf("port does not declare %q", tag)
				}
			}
			ran++
			// One "check" per assertion this case really made: its outcome, the
			// re-raise where the rejection is terminal, and that nothing was
			// bound.
			checks := 2

			shape := readHeaderShape(t, c)
			dest, bound := headerDestination(t, c, shape)
			d, out, err := feedHeaderCase(t, c, dest)

			switch c.Expect.Outcome {
			case "incomplete":
				// THE IN-CAP CONTROL, and not filler: the same shape at a length
				// the ceiling ADMITS. A port that rejects every short read passes
				// every rejection case in this block and is badly broken.
				if out != sofab.Incomplete || err != nil {
					t.Fatalf("outcome %v (err %v), want INCOMPLETE — the ceiling admits %d",
						out, err, c.Declared)
				}
				if c.Expect.Terminal {
					t.Fatal("an INCOMPLETE case cannot be terminal: it is precisely the state more bytes lift")
				}
			case "limit_exceeded":
				// A policy rejection, not INVALID: the bytes are well-formed and
				// the same header decodes under a looser cap (§6.2.1, §6.3).
				if out != sofab.Invalid {
					t.Fatalf("outcome %v (err %v), want INVALID/LimitExceeded at the length word", out, err)
				}
				if !errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatalf("decode: %v, want ErrLimitExceeded", err)
				}
				if errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatal("rejection reported as INVALID; the bytes are well-formed")
				}
				if errors.Is(err, sofab.ErrArgument) {
					t.Fatal("rejection reported as a caller defect; the cap the case configured was not consulted")
				}
			case "invalid":
				// The schema bound is a statement about VALIDITY (§7.1), so the
				// same word that is a cap breach one case over is malformed here.
				if out != sofab.Invalid {
					t.Fatalf("outcome %v (err %v), want INVALID at the length word", out, err)
				}
				if !errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("decode: %v, want ErrInvalidMsg", err)
				}
				if errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatal("schema breach reported as a receiver-cap rejection; §6.2.1 forbids a cap on a schema-bounded field")
				}
			default:
				t.Fatalf("unknown expected outcome %q", c.Expect.Outcome)
			}

			if c.Expect.Terminal {
				checks++
				// The rejection is terminal: a FURTHER feed RE-RAISES rather
				// than consuming (§6.3, §5.2.3). The probe is a whole valid
				// message, so a decoder that resumed would answer COMPLETE.
				out2, err2 := d.Feed(terminalProbe)
				if out2 != sofab.Invalid {
					t.Fatalf("a further feed answered %v; the rejection is terminal and must re-raise", out2)
				}
				if err2 != err {
					t.Fatalf("a further feed answered %v, want the same verdict re-raised (%v)", err2, err)
				}
				if d.Err() != err {
					t.Fatalf("Err() = %v after the further feed, want the latched %v", d.Err(), err)
				}
			}
			// Nothing was bound in any of these cases: the rejections answer at
			// the word, and the controls end before a payload byte arrives.
			if n := bound(); n != 0 {
				t.Errorf("destination bound %d value(s); no payload byte was ever delivered", n)
			}
			vecRan("header-limits", checks)
		})
	}

	t.Logf("[header-limits] %d cases: %d run, %d skipped", len(cases), ran, skipped)
}

// TestHeaderLimitsNegativeControl is the standing proof that the verdicts above
// come from the guard and not from something incidental about a header with no
// payload behind it.
//
// Every rejection case is replayed with THE CEILINGS LIFTED — no schema bound,
// and receiver caps generous enough that nothing this block declares can reach
// them — and must fall back to INCOMPLETE, the ordinary answer for a message
// that ends inside a field. A port whose rejections survived the lift would be
// rejecting short reads for an unrelated reason and passing this block by
// accident.
//
// The one admissible exception is a FORMAT ceiling (§6.2), which is not a
// receiver cap and cannot be lifted: a declared number past INT32_MAX is INVALID
// whatever the destination says (the decoder's own arrayMax, mirroring
// SOFAB_ARRAY_MAX / SOFAB_FIXLEN_MAX). No case in the block reaches it today —
// the amplification case declares 1 GiB, comfortably inside it — so the branch
// exists to keep this test honest rather than brittle if upstream adds one.
func TestHeaderLimitsNegativeControl(t *testing.T) {
	const formatCeiling = 0x7FFF_FFFF // INT32_MAX; the decoder's own arrayMax

	cases := loadHeaderCases(t)
	lifted := sofab.Caps{ArrayCount: formatCeiling, StringLen: formatCeiling, BlobLen: formatCeiling}
	rejections, fellBack, byCeiling := 0, 0, 0

	for _, c := range cases {
		if c.Expect.Outcome == "incomplete" {
			continue
		}
		skip := false
		for _, tag := range c.Requires {
			if !headerLimitsCap(tag) {
				skip = true
			}
		}
		if skip {
			continue
		}
		rejections++

		shape := readHeaderShape(t, c)
		var dest sofab.Visitor
		none := sofab.Bounds{Count: c.FieldID + 1}
		switch shape.construct {
		case "string":
			var out []string
			dest = sofab.NewStringSeq(&out, none, lifted)
		case "blob":
			var out [][]byte
			dest = sofab.NewBlobSeq(&out, none, lifted)
		default:
			var out [][]uint64
			dest = sofab.NewUnsignedMatrixSeq[uint64](&out, none, sofab.Bounds{}, lifted, 0)
		}
		_, out, err := feedHeaderCase(t, c, dest)

		switch {
		case out == sofab.Incomplete && err == nil:
			fellBack++
		case out == sofab.Invalid && c.Declared > formatCeiling:
			// A format ceiling, which no configuration lifts.
			byCeiling++
		default:
			t.Errorf("%s: with the ceilings lifted the outcome is %v (err %v), want INCOMPLETE — "+
				"the rejection does not come from the ceiling the case configured", c.Name, out, err)
		}
	}

	if rejections == 0 {
		t.Fatal("no rejection case ran; the negative control proves nothing")
	}
	t.Logf("[header-limits] negative control: %d rejections, %d fell back to INCOMPLETE, "+
		"%d held by the format ceiling", rejections, fellBack, byCeiling)
}

// TestHeaderLimitsCasesAreGated pins the block's gating, which is the opposite
// of a vector's: an unsatisfied tag here means SKIP (see headerLimitsCap). A
// reader of this file must not be able to conclude the gating is optional.
func TestHeaderLimitsCasesAreGated(t *testing.T) {
	cases := loadHeaderCases(t)
	for _, c := range cases {
		caps := map[string]bool{}
		for _, tag := range c.Requires {
			caps[tag] = true
			if !headerLimitsCap(tag) {
				t.Logf("%s: requires %q, which this port does not declare — the case is skipped", c.Name, tag)
			}
		}
		// A case that configures a §6.2.1 receiver cap needs the profile
		// capability; one bounded by the schema needs no cap and must not
		// demand it, or a profile that refuses schema-unbounded fields at
		// generate time would skip the pair that keeps the categories apart.
		if c.Limits != nil && !caps["receiver_caps"] {
			t.Errorf("%s: configures a receiver cap but does not carry the receiver_caps tag", c.Name)
		}
		if c.Schema != nil && caps["receiver_caps"] {
			t.Errorf("%s: is schema-bounded and needs no receiver cap, but carries the receiver_caps tag", c.Name)
		}
	}
}

// TestHeaderLimitsInventory is the guard on the block, in the shape of
// TestSequenceGrowthInventory: floors and structure, never equalities, so
// upstream growing the block does not fail this port while a block that SHRANK
// — or lost one of the properties it exists for — is caught.
func TestHeaderLimitsInventory(t *testing.T) {
	cases := loadHeaderCases(t)
	if len(cases) < 10 {
		t.Errorf("header_limits carries %d cases, want at least 10", len(cases))
	}

	groups := map[string]int{}
	outcomes := map[string]int{}
	constructs := map[string]int{}
	// Every rejection must be paired with an in-cap control on THE SAME
	// ceiling, or the block proves nothing: a port that rejects every short
	// read would pass all of them.
	rejected := map[string]string{}
	admitted := map[string]bool{}
	// The identical-bytes pair: the same header under the two ceilings.
	byBytes := map[string]map[string]bool{}
	chunked := 0

	for _, c := range cases {
		groups[c.Group]++
		outcomes[c.Expect.Outcome]++
		shape := readHeaderShape(t, c)
		constructs[shape.construct]++
		if len(c.Chunks) > 0 {
			chunked++
		}
		if c.Expect.Outcome != "incomplete" && !c.Expect.Terminal {
			t.Errorf("%s: expects %q but is not marked terminal; a ceiling's rejection is terminal (§6.3)",
				c.Name, c.Expect.Outcome)
		}
		key := headerCeilingKey(t, c, shape)
		if c.Expect.Outcome == "incomplete" {
			admitted[key] = true
		} else {
			rejected[key] = c.Name
		}
		if byBytes[c.Serialized] == nil {
			byBytes[c.Serialized] = map[string]bool{}
		}
		byBytes[c.Serialized][c.Expect.Outcome] = true
	}

	if groups["limits/header"] == 0 {
		t.Error(`no case in group "limits/header"`)
	}
	for _, o := range []string{"limit_exceeded", "invalid", "incomplete"} {
		if outcomes[o] == 0 {
			t.Errorf("no case expecting outcome %q", o)
		}
	}
	// A length ceiling and a count ceiling are different code paths, and a port
	// can wire one and miss the other — §6.2.1 keeps string and blob on separate
	// caps for exactly that reason.
	for _, k := range []string{"string", "blob", "array"} {
		if constructs[k] == 0 {
			t.Errorf("no case whose header declares a %s", k)
		}
	}
	if chunked == 0 {
		t.Error("no case splits the header across chunks; the verdict must not depend on the chunking (§7.2 item 4)")
	}
	for key, name := range rejected {
		if !admitted[key] {
			t.Errorf("%s rejects on ceiling %q with no in-cap control at a length that ceiling admits; "+
				"treat a missing control as a bug in the block", name, key)
		}
	}
	pair := false
	for _, seen := range byBytes {
		if seen["invalid"] && seen["limit_exceeded"] {
			pair = true
		}
	}
	if !pair {
		t.Error("no two cases carry identical bytes under the two different ceilings; " +
			"that pair is what keeps INVALID and LIMIT_EXCEEDED apart")
	}

	t.Logf("[header-limits] %d cases: groups %v, outcomes %v, constructs %v, %d chunked",
		len(cases), groups, outcomes, constructs, chunked)
}

// headerCeilingKey names the ceiling a case configures, so a rejection can be
// matched with the in-cap control that shares it.
func headerCeilingKey(t *testing.T, c headerCase, s headerShape) string {
	t.Helper()
	switch {
	case c.Schema != nil && c.Schema.MaxLen != nil:
		return fmt.Sprintf("schema.maxlen=%d/%s", *c.Schema.MaxLen, s.construct)
	case c.Limits != nil && c.Limits.MaxDynStringLen != nil:
		return fmt.Sprintf("max_dyn_string_len=%d", *c.Limits.MaxDynStringLen)
	case c.Limits != nil && c.Limits.MaxDynBlobLen != nil:
		return fmt.Sprintf("max_dyn_blob_len=%d", *c.Limits.MaxDynBlobLen)
	case c.Limits != nil && c.Limits.MaxDynArrayCount != nil:
		return fmt.Sprintf("max_dyn_array_count=%d", *c.Limits.MaxDynArrayCount)
	}
	t.Fatalf("%s: configures no ceiling", c.Name)
	return ""
}
