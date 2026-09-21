package sofab_test

// The shared `header_limits_nested` block — the flat `header_limits` assertion
// ONE OR TWO SEQUENCE FRAMES DEEPER, and nothing else.
//
// Every case in the flat block puts its field at id 0 in the top-level scope, so
// one axis stays untested there: the IDENTICAL over-ceiling header delivered
// INSIDE AN OPEN SEQUENCE. This block is that axis and only that axis. The key
// set, the outcome vocabulary, the terminality rule and the pairing of each
// rejection with an in-cap control are inherited unchanged — which is why this
// file reads header_limits_test.go's headerCase, headerShape, headerCeilings,
// headerLeaf and feedHeaderCase rather than growing copies of them.
//
//	3e 02 a2 06   then EOF
//	^^ id 7, wire type 6 -- a sequence OPENS
//	   ^^ id 0, wire type 2 (fixlen), INSIDE that sequence
//	      ^^^^^ (100 << 3) | 2 -- a 100-byte STRING is declared
//	              ... and the message ends, with the sequence still open.
//
// WHY DEPTH IS ITS OWN AXIS. A ceiling is a property of the destination, and the
// destination at depth 1 is a DIFFERENT OBJECT from the one at depth 0 — it is
// whatever BeginSequence handed back. A port can wire its §6.2.1 caps into the
// top-level receiver and leave every nested one uncapped, and the flat block
// cannot see that: all of its fields arrive at depth 0. Depth 2 gets its own
// pair (frames [7, 3]) because one level is exactly the kind of thing that gets
// special-cased.
//
// AND WHY THE NEGATIVE CONTROL IS LOAD-BEARING HERE, not a formality as it
// nearly is in the flat block. These cases end with the frames STILL OPEN, so
// there is a SECOND, fully independent reason to answer INCOMPLETE. That cuts
// both ways:
//
//   - a port that never consults the ceiling answers INCOMPLETE for the
//     over-ceiling cases and FAILS the forward pass — caught;
//   - but a port that rejects for an unrelated reason (a depth guard, a
//     frame-count guard, a "sequence left open at end of input is malformed"
//     rule) answers INVALID or LimitExceeded and PASSES the forward pass while
//     never having reached the ceiling at all.
//
// Only TestHeaderLimitsNestedNegativeControl tells those two apart: it replays
// every rejection with THE SAME KIND OF CEILING LIFTED above what the case
// declares, and requires the answer to CHANGE. Without it a runner that never
// reached the ceiling is indistinguishable from one that did, however green it
// looks.
//
// Nothing in this port is process-global, so there is no configuration to
// restore after the block: Bounds and Caps are arguments to each collector
// (§6.2.1 — "Passing a limit in is not the codec holding one"), so a case's
// ceiling cannot outlive the destination it was built for, let alone leak into
// the next case.

import (
	"errors"
	"fmt"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

func loadHeaderNestedCases(t *testing.T) []headerCase {
	t.Helper()
	cases := loadHeaderBlock(t, "header_limits_nested", loadVectors(t).HeaderLimitsNested)
	for _, c := range cases {
		// `frames` IS the block. A case without it would run as a flat case
		// and silently assert the axis this file exists for is untested.
		if len(c.Frames) == 0 {
			t.Fatalf("%s: carries no non-empty `frames`; that key is what makes this block nested", c.Name)
		}
	}
	return cases
}

// --- the frame chain ----------------------------------------------------------

// headerFrame is one sequence scope of the chain `frames` names: it hands the
// scope opened at `want` to `inner` and lets the decoder skip everything else.
//
// It carries NO bound of its own — that is deliberate. The only ceiling in play
// must be the one the case configured on the leaf, so a frame that added a cap
// (or a schema count) would give a rejection a second possible author and blunt
// the negative control. Handing a child back from BeginSequence is this port's
// ORDINARY nested-sequence decode path (visitor.go: the §6.0 child-handler
// shape), which is what the block requires the field to arrive through.
type headerFrame struct {
	sofab.VisitorBase
	want  sofab.ID
	inner sofab.Visitor
}

func (f headerFrame) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	if id == f.want {
		return f.inner, nil
	}
	// Any other scope is of no interest; VisitorBase decodes it and delivers
	// nothing. (Returning nil would decline it, which is also correct here but
	// says something stronger than the case does.)
	return sofab.VisitorBase{}, nil
}

// headerNestedChain wraps a leaf in the frames the case names, OUTERMOST FIRST:
// frames[0] is read in the top-level scope, frames[1] inside that one, and the
// leaf sits at the innermost depth. Built back-to-front, so an off-by-one shows
// up as a rejection that never fires rather than as a chain that happens to work
// at depth 1 (trap 10).
func headerNestedChain(frames []int, leaf sofab.Visitor) sofab.Visitor {
	v := leaf
	for i := len(frames) - 1; i >= 0; i-- {
		v = headerFrame{want: sofab.ID(frames[i]), inner: v}
	}
	return v
}

// headerNestedDestination is headerDestination one chain deeper: the SAME leaf,
// with the SAME ceiling, reached through the sequences `frames` names.
func headerNestedDestination(t *testing.T, c headerCase, s headerShape) (sofab.Visitor, func() int) {
	t.Helper()
	leaf, bound := headerDestination(t, c, s)
	return headerNestedChain(c.Frames, leaf), bound
}

// headerNestedPayloadProbe is eight bytes of would-be payload — content every
// one of these cases' headers promised and none of them delivered. Fed after a
// terminal rejection it is the sharpest question §6.3 admits: a decoder that had
// merely paused would consume them and advance.
var headerNestedPayloadProbe = []byte("payloadd")

// --- the forward pass ---------------------------------------------------------

func TestHeaderLimitsNested(t *testing.T) {
	cases := loadHeaderNestedCases(t)
	ran, gated, deepest := 0, 0, 0

	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			// An unsatisfied tag is a SKIP here, for every tag, exactly as in
			// the flat block: these cases assert a rejection WITH A CATEGORY,
			// and a build that cannot represent the construct would reject for
			// an unrelated reason and appear to pass while testing nothing.
			for _, tag := range c.Requires {
				if !headerLimitsCap(tag) {
					gated++
					t.Logf("gated by %q: %s", tag, c.Description)
					t.Skipf("port does not declare %q", tag)
				}
			}
			ran++
			if len(c.Frames) > deepest {
				deepest = len(c.Frames)
			}
			checks := 2

			shape := readHeaderShape(t, c)
			dest, bound := headerNestedDestination(t, c, shape)
			d, out, err := feedHeaderCase(t, c, dest)

			switch c.Expect.Outcome {
			case "incomplete":
				// THE IN-CAP CONTROL. Here it is doing double duty: it proves
				// the port does not reject everything nested AND that an open
				// frame on its own is not a rejection.
				if out != sofab.Incomplete || err != nil {
					t.Fatalf("outcome %v (err %v), want INCOMPLETE — the ceiling admits %d at depth %d\n%s",
						out, err, c.Declared, len(c.Frames), c.Description)
				}
				if c.Expect.Terminal {
					t.Fatal("an INCOMPLETE case cannot be terminal: it is precisely the state more bytes lift")
				}
			case "limit_exceeded":
				if out != sofab.Invalid {
					t.Fatalf("outcome %v (err %v), want the ceiling's rejection at the length word, %d frame(s) deep\n%s",
						out, err, len(c.Frames), c.Description)
				}
				if !errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatalf("decode: %v, want ErrLimitExceeded", err)
				}
				// The pair that carries the block: nested_string_over_cap and
				// nested_string_schema_bounded are the SAME BYTES at the SAME
				// DEPTH and differ only in which ceiling the case configured. A
				// port that collapses the two categories passes seven of eight.
				if errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatal("rejection reported as INVALID; the bytes are well-formed")
				}
				if errors.Is(err, sofab.ErrArgument) {
					t.Fatal("rejection reported as a caller defect; the cap the case configured never reached this depth")
				}
			case "invalid":
				if out != sofab.Invalid {
					t.Fatalf("outcome %v (err %v), want INVALID at the length word, %d frame(s) deep\n%s",
						out, err, len(c.Frames), c.Description)
				}
				if !errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("decode: %v, want ErrInvalidMsg", err)
				}
				if errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatal("schema breach reported as a receiver-cap rejection; §6.2.1 forbids a cap on a schema-bounded field")
				}
			case "complete":
				// Not used by this block today; mapped so an added case cannot
				// silently fall through to the error below.
				t.Fatalf("outcome %v (err %v): this block carries no COMPLETE case; a new one needs a runner", out, err)
			default:
				t.Fatalf("unknown expected outcome %q", c.Expect.Outcome)
			}

			if c.Expect.Terminal {
				checks++
				// §6.3: the rejection is TERMINAL — further input cannot lift
				// it. Feed the payload the header promised and watch the
				// decoder answer again; asking it to repeat a latched status
				// would prove nothing, because a decoder that had consumed
				// these bytes and moved on would look terminal too.
				out2, err2 := d.Feed(headerNestedPayloadProbe)
				if out2 != sofab.Invalid {
					t.Fatalf("a further feed of would-be payload answered %v; the rejection is terminal and must re-raise", out2)
				}
				if err2 != err {
					t.Fatalf("a further feed answered %v, want the same verdict re-raised (%v)", err2, err)
				}
				if d.Err() != err {
					t.Fatalf("Err() = %v after the further feed, want the latched %v", d.Err(), err)
				}
			}
			// "Rejected, never clamped" (§6.2.1). Checked AFTER the terminality
			// feed, so a destination that materialized a truncated value late is
			// caught too — and on the controls, where the message simply ends
			// before a payload byte arrives.
			if n := bound(); n != 0 {
				t.Errorf("destination bound %d value(s); no payload byte was ever delivered", n)
			}
			vecRan("header-limits-nested", checks)
		})
	}

	// `ran` and `gated` are REPORTED, and their sum pinned: a mis-spelled
	// capability name or a probe that answered "unsupported" by accident would
	// otherwise turn this whole file into a no-op that reports green.
	if ran+gated != len(cases) {
		t.Errorf("%d cases, but %d ran and %d were gated", len(cases), ran, gated)
	}
	if ran > 0 && deepest < 2 {
		t.Errorf("deepest frame chain that ran is %d; cases 7 and 8 exist because one level may be special-cased", deepest)
	}
	t.Logf("[header-limits-nested] %d cases: %d run, %d gated, deepest chain %d frame(s)",
		len(cases), ran, gated, deepest)
}

// --- the negative control ------------------------------------------------------

// TestHeaderLimitsNestedNegativeControl is the proof that the rejections above
// come from THE CEILING THE CASE CONFIGURED and not from anything else this port
// might think of a truncated message with open frames.
//
// Each rejection is replayed with the SAME KIND of ceiling lifted far above what
// the case declares — a `schema` case gets a lifted schema bound and NO cap, a
// `limits` case a lifted cap and NO schema bound — and the answer must CHANGE.
// Lifting only the kind the case states is what keeps the control honest:
// lifting both would let a schema case pass for the cap's reason (trap 4).
//
// The assertion is INEQUALITY, not "now INCOMPLETE". What is being proved is
// that the ceiling caused the rejection, not what the alternative answer is —
// though in practice it is INCOMPLETE, because a frame is open. The observed
// answer is logged either way, so the run output says what actually happened.
//
// The flat block's one exemption — its amplification case declares 1 GiB, which
// cannot be lifted past without inviting the allocation §6.2.1 exists to prevent
// — does NOT apply here: no case in this block declares a size above the lift,
// so every rejection must be covered. The guard below keeps that true if
// upstream ever adds one.
func TestHeaderLimitsNestedNegativeControl(t *testing.T) {
	// Far above every `declared` in the block (100), and small enough that
	// lifting to it cannot provoke an absurd allocation.
	const lift = 65536

	cases := loadHeaderNestedCases(t)
	checked, changed, unliftable := 0, 0, 0
	rejections := 0

	for _, c := range cases {
		if c.Expect.Outcome == "incomplete" {
			continue // only a rejection can be shown to depend on its ceiling
		}
		rejections++
		// The gate is evaluated by the same helper as the forward pass, so the
		// two passes cannot disagree about which cases exist (trap 3).
		gated := false
		for _, tag := range c.Requires {
			if !headerLimitsCap(tag) {
				gated = true
			}
		}
		if gated {
			continue
		}
		if c.Declared > lift {
			// Would need a lift past the ceiling itself; say so rather than
			// count it as covered.
			unliftable++
			t.Logf("%s: declares %d, above the %d lift — not covered by this control", c.Name, c.Declared, lift)
			continue
		}
		checked++

		shape := readHeaderShape(t, c)
		// The SAME leaf, the SAME chain, one ceiling moved. Building the bounds
		// from headerCeilings and then raising the one the case stated is what
		// makes "lifted" mean exactly that, instead of a second destination
		// wired by hand.
		elem, row, caps := headerCeilings(t, c, shape)
		switch {
		case c.Schema != nil:
			// A lifted SCHEMA bound, and still no cap: overLen consults the cap
			// only where the schema states nothing, so this really is the same
			// route with a larger number.
			if row.Count > 0 {
				row.Count = lift
			}
			if elem.ElemLen > 0 {
				elem.ElemLen = lift
			}
		default:
			// A lifted RECEIVER CAP, and still no schema bound.
			if caps.StringLen > 0 {
				caps.StringLen = lift
			}
			if caps.BlobLen > 0 {
				caps.BlobLen = lift
			}
			if caps.ArrayCount > 0 {
				caps.ArrayCount = lift
			}
		}
		leaf, bound := headerLeaf(t, c.Name, shape, c.FieldID, elem, row, caps)
		_, out, err := feedHeaderCase(t, c, headerNestedChain(c.Frames, leaf))

		// The forward pass pinned this case's rejection as INVALID-with-a-
		// sentinel; "changed" therefore means the outcome is no longer that
		// rejection.
		stillRejected := out == sofab.Invalid &&
			(errors.Is(err, sofab.ErrLimitExceeded) || errors.Is(err, sofab.ErrInvalidMsg))
		if stillRejected {
			t.Errorf("%s: with the %s lifted to %d the answer is unchanged (%v, err %v) — "+
				"the rejection does not come from the ceiling the case configured, "+
				"so the forward pass proves nothing about it\n%s",
				c.Name, headerNestedCeilingKind(c), lift, out, err, c.Description)
			continue
		}
		changed++
		if n := bound(); n != 0 {
			t.Errorf("%s: lifted run bound %d value(s) although no payload byte arrived", c.Name, n)
		}
		t.Logf("%s: %s lifted to %d -> %v (err %v)", c.Name, headerNestedCeilingKind(c), lift, out, err)
	}

	// A control that examined nothing is the failure mode this count exists to
	// catch (trap 3). Four rejections are in the block today and all four are
	// liftable, so a full-capability port checks four; a port with no receiver
	// caps checks the one schema-bounded rejection.
	if checked == 0 {
		t.Fatal("the negative control examined no case; it proves nothing")
	}
	if checked != changed {
		t.Errorf("checked %d rejections but only %d changed answer when the ceiling was lifted", checked, changed)
	}
	want := 1 // the schema-bounded rejection needs no cap and always runs
	if carriesReceiverCaps {
		want = rejections - unliftable
	}
	if checked != want {
		t.Errorf("the control checked %d of %d rejections (%d unliftable); expected %d for this port's capabilities",
			checked, rejections, unliftable, want)
	}
	t.Logf("[header-limits-nested] negative control: %d rejections, %d checked, %d changed answer, %d unliftable",
		rejections, checked, changed, unliftable)
}

// headerNestedCeilingKind names which ceiling a case configured, for the control's
// messages.
func headerNestedCeilingKind(c headerCase) string {
	if c.Schema != nil {
		return "schema bound"
	}
	return "receiver cap"
}

// --- the guard on the block ----------------------------------------------------

// TestHeaderLimitsNestedInventory pins the block's STRUCTURE rather than its
// contents, in the shape of TestHeaderLimitsInventory: floors and properties, so
// upstream growing the block does not fail this port, while a block that shrank
// — or lost one of the properties it exists for — is caught.
func TestHeaderLimitsNestedInventory(t *testing.T) {
	cases := loadHeaderNestedCases(t)
	if len(cases) < 8 {
		t.Errorf("header_limits_nested carries %d cases, want at least 8", len(cases))
	}

	outcomes := map[string]int{}
	depths := map[int]int{}
	// Each rejection must be paired with an in-cap control ON THE SAME CEILING
	// AT THE SAME DEPTH; dropping those as "redundant" would let a port that
	// rejects everything nested pass every rejection in the block.
	rejected := map[string]string{}
	admitted := map[string]bool{}
	byBytes := map[string]map[string]bool{}

	for _, c := range cases {
		if c.Group != "limits/header-nested" {
			t.Errorf("%s: group %q, want \"limits/header-nested\"", c.Name, c.Group)
		}
		shape := readHeaderShape(t, c)
		outcomes[c.Expect.Outcome]++
		depths[len(c.Frames)]++
		for _, f := range c.Frames {
			if f < 0 {
				t.Errorf("%s: frame id %d is not a field id", c.Name, f)
			}
		}
		if c.Expect.Outcome != "incomplete" && !c.Expect.Terminal {
			t.Errorf("%s: expects %q but is not marked terminal; a ceiling's rejection is terminal (§6.3)",
				c.Name, c.Expect.Outcome)
		}
		// The gating contract, as in the flat block: a case that configures a
		// §6.2.1 cap needs the profile tag, and a schema-bounded case must NOT
		// carry it, or a profile without receiver caps would skip the very pair
		// that keeps the two rejection categories apart.
		tags := map[string]bool{}
		for _, tag := range c.Requires {
			tags[tag] = true
		}
		if !tags["sequence"] {
			t.Errorf("%s: nested by construction but does not require \"sequence\"", c.Name)
		}
		if c.Limits != nil && !tags["receiver_caps"] {
			t.Errorf("%s: configures a receiver cap but does not carry the receiver_caps tag", c.Name)
		}
		if c.Schema != nil && tags["receiver_caps"] {
			t.Errorf("%s: is schema-bounded and needs no receiver cap, but carries the receiver_caps tag", c.Name)
		}

		key := fmt.Sprintf("%v/%s", c.Frames, headerCeilingKey(t, c, shape))
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

	for _, o := range []string{"limit_exceeded", "invalid", "incomplete"} {
		if outcomes[o] == 0 {
			t.Errorf("no case expecting outcome %q", o)
		}
	}
	if depths[2] == 0 {
		t.Error("no case nests two frames deep; one level is exactly what gets special-cased")
	}
	for key, name := range rejected {
		if !admitted[key] {
			t.Errorf("%s rejects on ceiling %q with no in-cap control at the same depth; "+
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
			"that pair is what keeps INVALID and LIMIT_EXCEEDED apart one frame down")
	}

	t.Logf("[header-limits-nested] %d cases: outcomes %v, frame depths %v", len(cases), outcomes, depths)
}
