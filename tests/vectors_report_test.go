package sofab_test

// The shared vector suite's own accounting: how much of assets/test_vectors.json
// each scenario actually executed, and a loud inventory check so the corpus can
// never shrink -- or be truncated by a loader limit -- without CI saying so.
//
// Why this exists (corelib-go#136, upstream corelib-c-cpp#160): the shared file
// grew from 81 to 131 vectors, 58 of which now carry `skip_ids`. A suite that
// silently ran only part of that would still be green, so CORELIB_PLAN §7.1
// adoption is only credible if the run states what it ran. `go test` discards a
// passing package's stdout, so the summary is written both to stdout (visible
// under `go test -v`, and on any failure) and, when SOFAB_VECTOR_SUMMARY names a
// path, to that file -- which is how CI puts the numbers in its log without
// running the whole package verbosely.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// --- what the suite executed -------------------------------------------------

// A "check" is one assertion actually performed against a vector: one encoded
// byte string compared, one decoded event log compared, one declared capability
// validated. Vectors are counted once per scenario, so the same vector
// contributes to several rows.
type vecScenario struct {
	vectors int
	checks  int
}

var (
	vecTallyMu sync.Mutex
	vecTally   = map[string]*vecScenario{}
)

// vecRan records that `scenario` ran one more vector performing `checks`
// assertions.
func vecRan(scenario string, checks int) {
	vecTallyMu.Lock()
	defer vecTallyMu.Unlock()
	s := vecTally[scenario]
	if s == nil {
		s = &vecScenario{}
		vecTally[scenario] = s
	}
	s.vectors++
	s.checks += checks
}

// vecSummary renders the tally as the report line(s) the run output carries.
// Empty when no vector scenario ran (e.g. a `-run` filter that excludes them),
// so a narrow run does not claim coverage it never executed.
func vecSummary() string {
	vecTallyMu.Lock()
	defer vecTallyMu.Unlock()
	if len(vecTally) == 0 {
		return ""
	}
	names := make([]string, 0, len(vecTally))
	for n := range vecTally {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	totalVectors, totalChecks := 0, 0
	for _, n := range names {
		s := vecTally[n]
		totalVectors += s.vectors
		totalChecks += s.checks
		fmt.Fprintf(&b, "  %-26s %4d vectors %6d checks\n", n, s.vectors, s.checks)
	}
	head := fmt.Sprintf("shared vector suite: %d scenarios, %d vector runs, %d checks\n",
		len(names), totalVectors, totalChecks)
	return head + b.String()
}

// TestMain prints the tally after the package's tests have run. It is the only
// place that can: the counts are not known until every scenario has finished.
func TestMain(m *testing.M) {
	code := m.Run()
	if s := vecSummary(); s != "" {
		fmt.Print(s)
		if path := os.Getenv("SOFAB_VECTOR_SUMMARY"); path != "" {
			if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "SOFAB_VECTOR_SUMMARY=%s: %v\n", path, err)
				if code == 0 {
					code = 1
				}
			}
		}
	}
	os.Exit(code)
}

// --- the corpus itself -------------------------------------------------------

// Floors, not equalities: the shared file is regenerated upstream and may grow
// again, which must not fail this port. It must never SHRINK unnoticed, though
// -- that is exactly the silent-truncation failure mode corelib-c-cpp#160 found
// in the C loader's fixed MAXSKIP, where over-long `skip_ids` were quietly
// dropped and the vector passed while testing less than it claimed.
//
// Every floor below is the value MEASURED in the corpus as adopted, not the
// smaller "at least this much" figure issue #136 quotes for a loader to handle.
// A floor set below what the file actually carries guards nothing: with
// `minFieldIDSeen` at #136's 100001, a regression that lost the id_max vector's
// 2147483647 would still have passed.
const (
	minVectors     = 131 // the corpus adopted here
	minWithSkipIDs = 58  // vectors carrying skip_ids (was 8 before #136)
	// group "skip/matrix". The 36 is a VECTOR count: the matrix is packed into
	// 36 vectors named `skip_matrix_<skipped tier>_after_<read tier>` over the
	// six capability tiers (varint, fixlen, fp64, int_array, fixlen_array,
	// sequence), so a feature-reduced build runs the part it can represent. The
	// cross product itself is finer -- 100 (read, skipped) rows over the ten
	// skippable constructs -- and is checked as such by
	// TestSkipMatrixCoversEveryWireTypePair below, not inferred from the 36.
	minSkipMatrix           = 36
	minSkipMatrixPairs      = 100 // distinct (read, skipped) construct pairs
	minSkipMatrixConstructs = 10  // the eight wire types, fixlen split by subtype
	minSkipAxes             = 16  // group "skip": the axes beside the matrix
	minInvalidUTF8          = 1   // the §6.4 group
	minSkipIDsPerVec        = 9   // the longest skip_ids list the corpus carries

	// The widest values the corpus reaches anywhere, and -- for the two that
	// decide how much of the skip path is really crossed -- the widest it
	// reaches on a SKIPPED field. #136 asks a loader to survive ids up to
	// 100001, 130-element arrays and 130-byte payloads; the file goes further,
	// so the guard does too.
	minFieldIDSeen     = 2147483647 // vector id_max
	minArrayElements   = 200        // vector array_u8_large
	minPayloadBytes    = 130
	minSkippedIDSeen   = 100000 // skip_large_id: a three-byte header varint
	minSkippedElements = 130    // skip_long_int_arrays: a two-byte element count

	requiredGroupSkip = "skip"
)

// TestVectorFileInventory is the guard on the corpus, and the one place that
// states its shape. It fails loudly rather than testing less: a fixed limit that
// truncated `skip_ids`, a payload, an element count or an id would show up here
// as a floor no longer met.
//
// Nothing in this port imposes such a limit -- the loader decodes into Go slices
// and maps, which grow -- but "we have no cap" is a claim worth checking rather
// than asserting, because a future struct field of the wrong width (an id in a
// uint16, say) would fail the JSON decode, and a future hand-added cap would
// fail here.
func TestVectorFileInventory(t *testing.T) {
	vf := loadVectors(t)

	withSkipIDs, longestSkipList := 0, 0
	var maxID, maxSkippedID uint32
	maxElements, maxPayload, maxSkippedElements := 0, 0, 0
	groups := map[string]int{}

	for _, v := range vf.Vectors {
		groups[v.Group]++
		skipped := make(map[uint32]bool, len(v.SkipIDs))
		for _, id := range v.SkipIDs {
			skipped[id] = true
			if id > maxSkippedID {
				maxSkippedID = id
			}
		}
		if n := len(v.SkipIDs); n > 0 {
			withSkipIDs++
			if n > longestSkipList {
				longestSkipList = n
			}
		}
		for _, f := range v.Fields {
			if f.Op != "sequence_end" && f.ID > maxID {
				maxID = f.ID
			}
			if n := len(f.Values); n > maxElements {
				maxElements = n
			}
			if n := len(f.Values); f.Op != "sequence_end" && skipped[f.ID] && n > maxSkippedElements {
				maxSkippedElements = n
			}
			switch f.Op {
			case "string":
				if n := len(pString(t, f.Value)); n > maxPayload {
					maxPayload = n
				}
			case "blob":
				if n := len(f.ValueHex) / 2; n > maxPayload {
					maxPayload = n
				}
			}
		}
	}

	for _, c := range []struct {
		what string
		got  int
		min  int
	}{
		{"vectors", len(vf.Vectors), minVectors},
		{"vectors with skip_ids", withSkipIDs, minWithSkipIDs},
		{`group "skip/matrix"`, groups["skip/matrix"], minSkipMatrix},
		{`group "skip"`, groups[requiredGroupSkip], minSkipAxes},
		{"invalid_utf8 rows", len(vf.InvalidUTF8), minInvalidUTF8},
		{"longest skip_ids list", longestSkipList, minSkipIDsPerVec},
		{"largest field id", int(maxID), minFieldIDSeen},
		{"largest array", maxElements, minArrayElements},
		{"largest string/blob payload", maxPayload, minPayloadBytes},
		{"largest skipped field id", int(maxSkippedID), minSkippedIDSeen},
		{"largest skipped array", maxSkippedElements, minSkippedElements},
	} {
		if c.got < c.min {
			t.Errorf("%s = %d, want at least %d -- the corpus shrank, or the loader "+
				"is truncating it (re-copy assets/test_vectors.json verbatim from "+
				"corelib-c-cpp; CORELIB_PLAN §7.1/§8)", c.what, c.got, c.min)
		}
	}

	// Every skipped id must name a field the vector actually carries. A skip_ids
	// entry with no field behind it would quietly skip nothing.
	names := map[string]bool{}
	for _, v := range vf.Vectors {
		if v.Name == "" {
			t.Errorf("vector in group %q has no name", v.Group)
		}
		if names[v.Name] {
			t.Errorf("duplicate vector name %q", v.Name)
		}
		names[v.Name] = true

		present := map[uint32]bool{}
		for _, f := range v.Fields {
			if f.Op != "sequence_end" {
				present[f.ID] = true
			}
		}
		for _, id := range v.SkipIDs {
			if !present[id] {
				t.Errorf("%s: skip_ids names id %d, which the vector does not carry", v.Name, id)
			}
		}
	}

	t.Logf("corpus: %d vectors (%d with skip_ids), groups %v, longest skip_ids %d, "+
		"largest id %d, largest array %d, largest payload %d bytes; on the skip "+
		"path: largest skipped id %d, largest skipped array %d elements",
		len(vf.Vectors), withSkipIDs, groups, longestSkipList, maxID, maxElements, maxPayload,
		maxSkippedID, maxSkippedElements)

	// §7.2 item 8 (sequence growth) is run by sequence_growth_test.go. Assert
	// the block is present rather than merely logging it: the shared file is
	// copied verbatim (§7.1/§8), so a copy that lost the block would silently
	// reduce this port's coverage to items 1-7 again.
	if len(vf.SequenceGrowth) == 0 {
		t.Error("top-level sequence_growth block missing; §7.2 item 8 has no corpus to run")
	} else {
		t.Logf("top-level sequence_growth block present (%d bytes) and run by "+
			"sequence_growth_test.go (§7.2 item 8)", len(vf.SequenceGrowth))
	}
}

// vecConstruct names the skippable construct a field sits on -- the granularity
// the skip matrix is built at, which is the wire type except that `fixlen` is
// split by subtype because the decoder branches on it (§4.6) and an array is
// split by whether its elements are varints or fixed-width (§4.7 vs §4.8).
// `boolean` is an unsigned varint on the wire (§4.4), so it is not its own row.
func vecConstruct(f vecField) string {
	switch f.Op {
	case "unsigned", "boolean":
		return "varint<unsigned>"
	case "signed":
		return "varint<signed>"
	case "fp32", "fp64", "string", "blob":
		return "fixlen<" + f.Op + ">"
	case "sequence_begin":
		return "sequence"
	case "array":
		switch {
		case strings.HasPrefix(f.ElementType, "fp"):
			return "array<fixlen>"
		case strings.HasPrefix(f.ElementType, "i"):
			return "array<signed>"
		default:
			return "array<unsigned>"
		}
	}
	return "unknown<" + f.Op + ">"
}

// TestSkipMatrixCoversEveryWireTypePair checks the claim the matrix is named
// for, rather than trusting the vector count to imply it: every ordered pair of
// skippable constructs appears as a `[read P][skipped S][anchor]` row somewhere
// in group "skip/matrix". 36 vectors carrying the right 100 rows and 36 vectors
// carrying the same row a hundred times are indistinguishable by count alone,
// and only the first tests what the group exists to test.
func TestSkipMatrixCoversEveryWireTypePair(t *testing.T) {
	vf := loadVectors(t)

	pairs := map[[2]string]bool{}
	readSeen, skippedSeen := map[string]bool{}, map[string]bool{}
	rows := 0
	for _, v := range vf.Vectors {
		if v.Group != "skip/matrix" {
			continue
		}
		skip := make(map[uint32]bool, len(v.SkipIDs))
		for _, id := range v.SkipIDs {
			skip[id] = true
		}
		// Top-level fields only: a matrix row is a chain of three top-level
		// fields, and a sequence's contents are one construct ("sequence"), not
		// a run of them.
		var top []vecField
		for i := 0; i < len(v.Fields); {
			f := v.Fields[i]
			if f.Op == "sequence_begin" {
				top = append(top, f)
				i = advancePastSequence(v.Fields, i)
				continue
			}
			if f.Op != "sequence_end" {
				top = append(top, f)
			}
			i++
		}
		for i, f := range top {
			if i == 0 || !skip[f.ID] {
				continue
			}
			rows++
			read, skipped := vecConstruct(top[i-1]), vecConstruct(f)
			pairs[[2]string{read, skipped}] = true
			readSeen[read] = true
			skippedSeen[skipped] = true
		}
	}

	if len(pairs) < minSkipMatrixPairs {
		t.Errorf("skip/matrix covers %d distinct (read, skipped) pairs in %d rows, "+
			"want at least %d -- re-copy assets/test_vectors.json verbatim from "+
			"corelib-c-cpp (CORELIB_PLAN §7.1/§8)", len(pairs), rows, minSkipMatrixPairs)
	}
	for what, seen := range map[string]map[string]bool{"read": readSeen, "skipped": skippedSeen} {
		if len(seen) < minSkipMatrixConstructs {
			names := make([]string, 0, len(seen))
			for n := range seen {
				names = append(names, n)
			}
			sort.Strings(names)
			t.Errorf("skip/matrix reaches only %d distinct constructs on the %s side (%v), "+
				"want at least %d", len(seen), what, names, minSkipMatrixConstructs)
		}
	}
	t.Logf("skip/matrix: %d rows, %d distinct (read, skipped) pairs over %d constructs",
		rows, len(pairs), len(skippedSeen))
}
