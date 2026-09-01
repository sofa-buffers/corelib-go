package sofab_test

// The shared `sequence_growth` block — CORELIB_PLAN §7.2 item 8.
//
// A wrapper (sequence) array carries no element count on the wire: its length is
// *highest present id + 1* (MESSAGE_SPEC §5.1), so the size is known only once
// the array ends and the container GROWS as elements arrive. That is the one
// allocation shape where growth is conformant (§6.6) — and it happens in the
// collector layer (collectors.go), never in the codec (§6.6.1).
//
// Why this cannot be a vector, and therefore why the block is keyed by a
// DELIVERY SEQUENCE OF ELEMENT IDS rather than by bytes: two ports that grow
// differently emit identical bytes and reach identical outcomes, so no
// serialized.hex can tell them apart. The port builds the message itself from
// `deliver` and asserts `expect`.
//
// Asserted per case: the resulting container LENGTH and the OUTCOME, and
// nothing else. No allocator instrumentation — that is what makes the cases
// portable across eleven languages. Growth GEOMETRY (extending to at least
// id+1 rather than exactly id+1, so a sparse array does not cost O(n^2) copies)
// is the one property here needing an allocation-counting facility, and Go
// offers one — it is pinned by TestSequenceGrowthIsGeometric in
// collectors_test.go, which owns that half and is not repeated here.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// growthCap is THIS port's max_dyn_array_count for the block's run.
//
// The block never names an absolute boundary: a receiver cap is per-target
// configuration and §6.2.1 fixes no family-wide number, so every case's
// `id_from_cap` / `length_from_cap` is an OFFSET onto whatever the port picks
// (-1 -> cap-1, 0 -> cap). The cases assume a cap of at least 4; 4 is the
// smallest value that satisfies them, which keeps the messages small and the
// boundary arithmetic legible.
const growthCap = 4

// growsDynamicArrays is this port's answer to the `dynamic_arrays` capability
// the block gates on. It is NOT a wire capability like the tags in goCaps —
// which is why it lives here and not there — but a statement about how the port
// ALLOCATES: a wrapper-array collector writing into a Go slice grows it as
// elements arrive, so the Go core declares it and runs the block. A statically
// bounded profile (C, c-cpp, Rust no_std) declares it false and states that in
// its README instead (§7.2 item 8).
const growsDynamicArrays = true

// --- the block's shape (test_vectors_README.md) -------------------------------

type growthElem struct {
	ID        *int            `json:"id"`
	IDFromCap *int            `json:"id_from_cap"`
	Value     json.RawMessage `json:"value"`
}

type growthExpect struct {
	Outcome       string `json:"outcome"`
	Length        *int   `json:"length"`
	LengthFromCap *int   `json:"length_from_cap"`
	DefaultIDs    []int  `json:"default_ids"`
	Terminal      bool   `json:"terminal"`
	MaxLength     *int   `json:"max_length"`
}

type growthCase struct {
	Name        string       `json:"name"`
	Group       string       `json:"group"`
	Description string       `json:"description"`
	Requires    []string     `json:"requires"`
	FieldID     int          `json:"field_id"`
	ElementType string       `json:"element_type"`
	Deliver     []growthElem `json:"deliver"`
	Expect      growthExpect `json:"expect"`
}

func loadGrowthCases(t *testing.T) []growthCase {
	t.Helper()
	vf := loadVectors(t)
	if len(vf.SequenceGrowth) == 0 {
		t.Fatal("vector file carries no sequence_growth block")
	}
	var cases []growthCase
	if err := json.Unmarshal(vf.SequenceGrowth, &cases); err != nil {
		t.Fatalf("parse sequence_growth: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("sequence_growth block is empty")
	}
	return cases
}

// resolveGrowth turns a possibly cap-relative index into an absolute one.
// Exactly one of the two keys is present per the README; neither, or both, is a
// corrupt case rather than something to guess at.
func resolveGrowth(t *testing.T, abs, fromCap *int, what string) int {
	t.Helper()
	switch {
	case abs != nil && fromCap != nil:
		t.Fatalf("%s carries both an absolute and a cap-relative value", what)
	case abs != nil:
		return *abs
	case fromCap != nil:
		return growthCap + *fromCap
	}
	t.Fatalf("%s carries neither an absolute nor a cap-relative value", what)
	return 0
}

// --- the struct element -------------------------------------------------------

// growthStruct is the block's `element_type: "struct"`: a framed sub-sequence
// carrying one unsigned field at id 0. It stands in for a generated message
// type, which is how MessageSeq reaches a schema it cannot name.
type growthStruct struct {
	sofab.VisitorBase
	V uint64
}

func (g *growthStruct) Unsigned(_ sofab.ID, v uint64) error {
	g.V = v
	return nil
}

// --- building the message the delivery sequence describes ---------------------

func buildGrowth(t *testing.T, c growthCase) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)

	// The frame is KEPT even when empty: element presence is what carries the
	// array's length, so an empty wrapper is framed rather than omitted (§5.1).
	if err := e.WriteSequenceBeginLazy(sofab.ID(c.FieldID)); err != nil {
		t.Fatalf("%s: open wrapper: %v", c.Name, err)
	}
	for i, d := range c.Deliver {
		id := sofab.ID(resolveGrowth(t, d.ID, d.IDFromCap, "deliver["+strconv.Itoa(i)+"]"))
		switch c.ElementType {
		case "string":
			var s string
			if err := json.Unmarshal(d.Value, &s); err != nil {
				t.Fatalf("%s: deliver[%d].value is not a string: %v", c.Name, i, err)
			}
			if err := e.WriteString(id, s); err != nil {
				t.Fatalf("%s: write element %d: %v", c.Name, id, err)
			}
		case "struct":
			var n uint64
			if err := json.Unmarshal(d.Value, &n); err != nil {
				t.Fatalf("%s: deliver[%d].value is not an integer: %v", c.Name, i, err)
			}
			if err := e.WriteSequenceBeginLazy(id); err != nil {
				t.Fatalf("%s: open element %d: %v", c.Name, id, err)
			}
			if err := e.WriteUnsigned(0, n); err != nil {
				t.Fatalf("%s: write element %d field: %v", c.Name, id, err)
			}
			if err := e.WriteSequenceEndKeep(); err != nil {
				t.Fatalf("%s: close element %d: %v", c.Name, id, err)
			}
		default:
			t.Fatalf("%s: unknown element_type %q", c.Name, c.ElementType)
		}
	}
	if err := e.WriteSequenceEndKeep(); err != nil {
		t.Fatalf("%s: close wrapper: %v", c.Name, err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("%s: flush: %v", c.Name, err)
	}
	return buf.Bytes()
}

// growthCaps is the receiver-cap set the block runs under: the array count is
// this port's cap, and the payload bounds are generous, because the cases are
// about the element INDEX and nothing else. Bounds stays zero throughout — the
// schema declares no `count:` here, which is precisely the arrangement in which
// the receiver cap governs the index (§6.2.1).
var growthCaps = sofab.Caps{ArrayCount: growthCap, StringLen: 4096, BlobLen: 4096}

// --- the cases ----------------------------------------------------------------

func TestSequenceGrowth(t *testing.T) {
	if !growsDynamicArrays {
		t.Skip("port does not declare dynamic_arrays")
	}
	cases := loadGrowthCases(t)

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			raw := buildGrowth(t, c)

			var strs []string
			var structs []growthStruct
			var child sofab.Visitor
			if c.ElementType == "string" {
				child = sofab.NewStringSeq(&strs, sofab.Bounds{}, growthCaps)
			} else {
				child = sofab.NewMessageSeq[growthStruct, *growthStruct](&structs, sofab.Bounds{}, growthCaps)
			}

			err := collect(raw, child)
			length := len(strs)
			if c.ElementType == "struct" {
				length = len(structs)
			}

			switch c.Expect.Outcome {
			case "complete":
				if err != nil {
					t.Fatalf("decode: %v, want a clean decode", err)
				}
				want := resolveGrowth(t, c.Expect.Length, c.Expect.LengthFromCap, "expect")
				if length != want {
					t.Fatalf("container length %d, want %d", length, want)
				}
				// A gap below the cap holds the element default, and neither
				// shortens nor shifts the array (§5.1).
				for _, id := range c.Expect.DefaultIDs {
					if id >= length {
						t.Fatalf("default id %d past the container length %d", id, length)
					}
					if c.ElementType == "string" {
						if strs[id] != "" {
							t.Errorf("element %d = %q, want the element default", id, strs[id])
						}
					} else if structs[id].V != 0 {
						t.Errorf("element %d = %d, want the element default", id, structs[id].V)
					}
				}
			case "limit_exceeded":
				// A policy rejection, not INVALID: the bytes are well-formed
				// and the same message decodes under a looser cap (§6.2.1,
				// §6.3).
				if !errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatalf("decode: %v, want ErrLimitExceeded", err)
				}
				if errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatal("rejection reported as INVALID; the bytes are well-formed")
				}
				// The check runs BEFORE the container is extended, so the
				// length never passes what legitimately arrived — and the
				// rejection is terminal, so an element delivered after it does
				// not land either.
				if c.Expect.MaxLength != nil && length > *c.Expect.MaxLength {
					t.Fatalf("container length %d, want at most %d — extended toward the rejected index",
						length, *c.Expect.MaxLength)
				}
			default:
				t.Fatalf("unknown expected outcome %q", c.Expect.Outcome)
			}
		})
	}
}

// The block is the one place `requires` is honoured by a full-format port: the
// tag says how the port ALLOCATES, not what it can parse, so a statically
// bounded build must skip these cases even though it runs every vector
// (test_vectors_README.md, "Gating"). Pin that every case carries the tag, so a
// reader of this file cannot conclude the gating is optional.
func TestSequenceGrowthCasesAreGatedOnDynamicArrays(t *testing.T) {
	cases := loadGrowthCases(t)
	for _, c := range cases {
		found := false
		for _, r := range c.Requires {
			if r == "dynamic_arrays" {
				found = true
				continue
			}
			// Every other tag on a growth case is a wire capability, and this
			// port must support it or the case could not run at all.
			if !goCaps[r] {
				t.Errorf("%s: requires %q, which this port does not declare", c.Name, r)
			}
		}
		if !found {
			t.Errorf("%s: does not carry the dynamic_arrays tag", c.Name)
		}
	}
}

// An inventory guard in the shape of TestVectorFileInventory: floors, not
// equalities, so upstream growing the block does not fail this port, while a
// block that SHRANK — or a case kind that vanished — is caught.
func TestSequenceGrowthInventory(t *testing.T) {
	cases := loadGrowthCases(t)
	if len(cases) < 8 {
		t.Errorf("sequence_growth carries %d cases, want at least 8", len(cases))
	}
	groups := map[string]int{}
	kinds := map[string]int{}
	outcomes := map[string]int{}
	for _, c := range cases {
		groups[c.Group]++
		kinds[c.ElementType]++
		outcomes[c.Expect.Outcome]++
	}
	for _, g := range []string{"growth/index", "growth/gap", "growth/reject", "growth/length"} {
		if groups[g] == 0 {
			t.Errorf("no case in group %q", g)
		}
	}
	// Both element kinds are mandatory: a string element reaches the container
	// through the collector's leaf path and a struct element through its
	// sequence path, and a port can get one right and the other wrong.
	for _, k := range []string{"string", "struct"} {
		if kinds[k] == 0 {
			t.Errorf("no case with element_type %q", k)
		}
	}
	for _, o := range []string{"complete", "limit_exceeded"} {
		if outcomes[o] == 0 {
			t.Errorf("no case expecting outcome %q", o)
		}
	}
	t.Logf("[sequence-growth] %d cases: %d groups, kinds %v, outcomes %v",
		len(cases), len(groups), kinds, outcomes)
}
