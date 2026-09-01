package sofab_test

// Conformance tests driven by the shared, language-agnostic vector suite
// (assets/test_vectors.json, copied verbatim from corelib-c-cpp/assets, which
// generates the vectors and is their authoritative source of truth).
// Each vector is replayed through the encoder (bytes must equal serialized.hex)
// and fed through the decoder (recovered values must equal the fields).

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	sofab "github.com/sofa-buffers/corelib-go"
)

type vecField struct {
	Op          string            `json:"op"`
	ID          uint32            `json:"id"`
	Value       json.RawMessage   `json:"value"`
	ValueHex    string            `json:"value_hex"`
	ElementType string            `json:"element_type"`
	Values      []json.RawMessage `json:"values"`
}

type vector struct {
	Name       string     `json:"name"`
	Group      string     `json:"group"`
	Offset     int        `json:"offset"`
	Requires   []string   `json:"requires"`
	SkipIDs    []uint32   `json:"skip_ids"`
	Fields     []vecField `json:"fields"`
	Serialized struct {
		Length int    `json:"length"`
		Hex    string `json:"hex"`
	} `json:"serialized"`
}

// utf8Vector is one row of the shared file's `invalid_utf8` group: a `string`
// field whose payload is not valid UTF-8, with the outcome §6.4 requires on each
// side. The rows are shaped differently from `vectors` — one hand-written field
// rather than a field list — because they exist to pin a validation verdict, not
// a layout.
type utf8Vector struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	ID            uint32 `json:"id"`
	StringHex     string `json:"string_hex"`     // the raw (invalid) payload
	SerializedHex string `json:"serialized_hex"` // the whole field, header included
	DecodeOutcome string `json:"decode_outcome"` // "invalid"
	EncodeOutcome string `json:"encode_outcome"` // "invalid_argument"
}

type vectorFile struct {
	Format      string       `json:"format"`
	Version     int          `json:"version"`
	Vectors     []vector     `json:"vectors"`
	InvalidUTF8 []utf8Vector `json:"invalid_utf8"`

	// sequence_growth is the corpus for CORELIB_PLAN §7.2 item 8, which this
	// port does not run yet (tracked as #137). It is declared so the block is
	// explicitly TOLERATED and reportable rather than silently discarded by the
	// JSON decoder -- the shared file is copied verbatim (§7.1/§8), so a block
	// this port has no scenario for must not make the load fail or warn.
	SequenceGrowth json.RawMessage `json:"sequence_growth"`
}

// loadVectors reads the shared vector file.
//
// Which column this repo asserts: `serialized` — the primitive-layer ground
// truth, every field written explicitly and every sequence framed. It is the
// only form a corelib can produce, because a corelib has no message layer: it
// never sees a schema, a declared default, or a whole object, so it can never
// decide that a field equals its default and omit it.
//
// The file also carries a `serialized_sparse` column (the MESSAGE_SPEC §2 form,
// where a field equal to its declared default — including an all-default
// sequence field — is omitted). Nothing here reads it, deliberately: it is
// exercised by the *generator's* conformance drivers (sofabgen,
// tests/conformance/<lang>/run.sh), which generate the message layer that
// produces it. The vector struct above accordingly has no field for it, so its
// absence cannot be mistaken for coverage that exists.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "assets", "test_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if vf.Format != "sofabuffers-test-vectors" || vf.Version != sofab.APIVersion {
		t.Fatalf("unexpected vector file: format=%q version=%d (APIVersion=%d)",
			vf.Format, vf.Version, sofab.APIVersion)
	}
	if len(vf.Vectors) == 0 {
		t.Fatal("vector file carries no vectors")
	}
	// Nothing above imposes a size limit: ids, skip_ids, element counts and
	// payloads all decode into Go values that grow (an id too wide for uint32
	// would fail the unmarshal, loudly, rather than wrap). What CAN still go
	// unnoticed is a vector whose declared length and hex disagree, so check
	// that here -- the corpus is machine-generated, and a mismatch means the
	// file was hand-edited, which §7.1 forbids.
	for _, v := range vf.Vectors {
		if len(v.Serialized.Hex)%2 != 0 {
			t.Fatalf("%s: serialized.hex has an odd length (%d)", v.Name, len(v.Serialized.Hex))
		}
		if got := len(v.Serialized.Hex) / 2; got != v.Serialized.Length {
			t.Fatalf("%s: serialized.hex is %d bytes but serialized.length says %d",
				v.Name, got, v.Serialized.Length)
		}
	}
	return vf
}

// --- raw-value parsing (json.RawMessage preserves full u64/i64 precision) ----

func pUint(t *testing.T, r json.RawMessage) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(string(r), 10, 64)
	if err != nil {
		t.Fatalf("parse uint %q: %v", r, err)
	}
	return v
}

func pInt(t *testing.T, r json.RawMessage) int64 {
	t.Helper()
	v, err := strconv.ParseInt(string(r), 10, 64)
	if err != nil {
		t.Fatalf("parse int %q: %v", r, err)
	}
	return v
}

func pFloat(t *testing.T, r json.RawMessage) float64 {
	t.Helper()
	s := string(r)
	if len(s) > 0 && s[0] == '"' { // "inf" / "-inf"
		switch s {
		case `"inf"`:
			return math.Inf(1)
		case `"-inf"`:
			return math.Inf(-1)
		}
		t.Fatalf("unexpected float literal %s", s)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parse float %q: %v", r, err)
	}
	return v
}

func pBool(t *testing.T, r json.RawMessage) bool {
	t.Helper()
	return string(r) == "true"
}

func pString(t *testing.T, r json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		t.Fatalf("parse string %q: %v", r, err)
	}
	return s
}

// --- encode side -------------------------------------------------------------

func encodeField(t *testing.T, e *sofab.Encoder, f vecField) {
	t.Helper()
	id := sofab.ID(f.ID)
	switch f.Op {
	case "unsigned":
		e.WriteUnsigned(id, pUint(t, f.Value))
	case "signed":
		e.WriteSigned(id, pInt(t, f.Value))
	case "boolean":
		e.WriteBool(id, pBool(t, f.Value))
	case "fp32":
		e.WriteFloat32(id, float32(pFloat(t, f.Value)))
	case "fp64":
		e.WriteFloat64(id, pFloat(t, f.Value))
	case "string":
		e.WriteString(id, pString(t, f.Value))
	case "blob":
		b, err := hex.DecodeString(f.ValueHex)
		if err != nil {
			t.Fatalf("blob hex: %v", err)
		}
		e.WriteBytes(id, b)
	case "array":
		encodeArray(t, e, id, f)
	case "sequence_begin":
		e.WriteSequenceBeginLazy(id)
	case "sequence_end":
		// A vector's `serialized` form is the primitive-layer ground truth and
		// always carries the frame, so every sequence closes with
		// WriteSequenceEndKeep: identical bytes once the sequence has content,
		// and the empty-sequence vectors keep their begin+end pair instead of
		// vanishing (MESSAGE_SPEC §2).
		e.WriteSequenceEndKeep()
	default:
		t.Fatalf("unknown op %q", f.Op)
	}
}

func encodeArray(t *testing.T, e *sofab.Encoder, id sofab.ID, f vecField) {
	t.Helper()
	switch f.ElementType {
	case "u8":
		a := make([]uint8, len(f.Values))
		for i, r := range f.Values {
			a[i] = uint8(pUint(t, r))
		}
		sofab.WriteUnsignedArray(e, id, a)
	case "u16":
		a := make([]uint16, len(f.Values))
		for i, r := range f.Values {
			a[i] = uint16(pUint(t, r))
		}
		sofab.WriteUnsignedArray(e, id, a)
	case "u32":
		a := make([]uint32, len(f.Values))
		for i, r := range f.Values {
			a[i] = uint32(pUint(t, r))
		}
		sofab.WriteUnsignedArray(e, id, a)
	case "u64":
		a := make([]uint64, len(f.Values))
		for i, r := range f.Values {
			a[i] = pUint(t, r)
		}
		sofab.WriteUnsignedArray(e, id, a)
	case "i8":
		a := make([]int8, len(f.Values))
		for i, r := range f.Values {
			a[i] = int8(pInt(t, r))
		}
		sofab.WriteSignedArray(e, id, a)
	case "i16":
		a := make([]int16, len(f.Values))
		for i, r := range f.Values {
			a[i] = int16(pInt(t, r))
		}
		sofab.WriteSignedArray(e, id, a)
	case "i32":
		a := make([]int32, len(f.Values))
		for i, r := range f.Values {
			a[i] = int32(pInt(t, r))
		}
		sofab.WriteSignedArray(e, id, a)
	case "i64":
		a := make([]int64, len(f.Values))
		for i, r := range f.Values {
			a[i] = pInt(t, r)
		}
		sofab.WriteSignedArray(e, id, a)
	case "fp32":
		a := make([]float32, len(f.Values))
		for i, r := range f.Values {
			a[i] = float32(pFloat(t, r))
		}
		e.WriteFloat32Array(id, a)
	case "fp64":
		a := make([]float64, len(f.Values))
		for i, r := range f.Values {
			a[i] = pFloat(t, r)
		}
		e.WriteFloat64Array(id, a)
	default:
		t.Fatalf("unknown element_type %q", f.ElementType)
	}
}

func TestVectorEncode(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			var buf bytes.Buffer
			e := sofab.NewEncoder(&buf)
			for _, f := range v.Fields {
				encodeField(t, e, f)
			}
			if err := e.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			got := hex.EncodeToString(buf.Bytes())
			if got != v.Serialized.Hex {
				t.Fatalf("encode mismatch\n got: %s\nwant: %s", got, v.Serialized.Hex)
			}
			if buf.Len() != v.Serialized.Length {
				t.Fatalf("length = %d, want %d", buf.Len(), v.Serialized.Length)
			}
			vecRan("encode", 2) // bytes, length
		})
	}
}

// TestVectorEncodeOverCallerBuffer replays every vector through a
// caller-supplied output buffer (CORELIB_PLAN §7.2 item 1: "at the given
// offset"), in the two shapes §5.1 requires — sink-less, sized to hold the
// message, and streamed through a buffer of exactly MinOutputBuffer. Both must
// reproduce the vector's bytes exactly, which is what makes "any buffer at or
// above the minimum produces output byte-identical to the one-shot path" a
// checked property across the whole corpus rather than one sample message.
func TestVectorEncodeOverCallerBuffer(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			// (a) no sink: the buffer holds the message and nothing is flushed.
			buf := make([]byte, v.Offset+v.Serialized.Length)
			e, err := sofab.NewEncoderBuffer(buf, v.Offset)
			if err != nil {
				t.Fatalf("NewEncoderBuffer(offset %d): %v", v.Offset, err)
			}
			for _, f := range v.Fields {
				encodeField(t, e, f)
			}
			if err := e.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if got := hex.EncodeToString(e.Bytes()); got != v.Serialized.Hex {
				t.Fatalf("caller-buffer encode mismatch\n got: %s\nwant: %s", got, v.Serialized.Hex)
			}

			// (b) streamed through the declared minimum, driving the sink.
			var out bytes.Buffer
			small := make([]byte, v.Offset+sofab.MinOutputBuffer)
			se, err := sofab.NewEncoderSink(small, v.Offset, func(_ *sofab.Encoder, b []byte) error {
				out.Write(b)
				return nil
			})
			if err != nil {
				t.Fatalf("NewEncoderSink(offset %d): %v", v.Offset, err)
			}
			for _, f := range v.Fields {
				encodeField(t, se, f)
			}
			if err := se.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if got := hex.EncodeToString(out.Bytes()); got != v.Serialized.Hex {
				t.Fatalf("min-buffer encode mismatch\n got: %s\nwant: %s", got, v.Serialized.Hex)
			}
			vecRan("encode/caller-buffer", 2) // no-sink bytes, min-buffer bytes
		})
	}
}

// --- decode side -------------------------------------------------------------

// --- skip-ids scenario -------------------------------------------------------

// advancePastSequence returns the index just after the sequence_end that matches
// the sequence_begin at fields[start], accounting for nested sequences. Used to
// jump over a whole sub-sequence in the flat field list once the decoder has
// skipped it wholesale.
func advancePastSequence(fields []vecField, start int) int {
	depth := 0
	for i := start; i < len(fields); i++ {
		switch fields[i].Op {
		case "sequence_begin":
			depth++
		case "sequence_end":
			depth--
		}
		if depth == 0 {
			return i + 1
		}
	}
	return len(fields)
}

// skipIDsV is the destination the skip-ids scenario decodes into: it records
// every field it is handed EXCEPT the ids the scenario skips, and declines a
// sub-sequence whose id is skipped so the whole sub-tree is walked without
// being delivered (§6.0, §7.2 item 7).
type skipIDsV struct {
	skip map[uint32]bool
	log  *[]string
}

func (v skipIDsV) rec(id sofab.ID, s string) error {
	if !v.skip[uint32(id)] {
		*v.log = append(*v.log, s)
	}
	return nil
}

func (v skipIDsV) Unsigned(id sofab.ID, x uint64) error { return v.rec(id, evU(id, x)) }
func (v skipIDsV) Signed(id sofab.ID, x int64) error    { return v.rec(id, evS(id, x)) }
func (v skipIDsV) Float32(id sofab.ID, x float32) error { return v.rec(id, evF32(id, x)) }
func (v skipIDsV) Float64(id sofab.ID, x float64) error { return v.rec(id, evF64(id, x)) }
func (v skipIDsV) String(id sofab.ID, x string) error   { return v.rec(id, evStr(id, x)) }
func (v skipIDsV) Bytes(id sofab.ID, x []byte) error    { return v.rec(id, evBlob(id, x)) }
func (v skipIDsV) UnsignedArray(id sofab.ID, x []uint64) error {
	return v.rec(id, evAU(id, x))
}
func (v skipIDsV) SignedArray(id sofab.ID, x []int64) error {
	return v.rec(id, evAS(id, x))
}
func (v skipIDsV) Float32Array(id sofab.ID, x []float32) error {
	return v.rec(id, evAF32(id, x))
}
func (v skipIDsV) Float64Array(id sofab.ID, x []float64) error {
	return v.rec(id, evAF64(id, x))
}

func (v skipIDsV) BeginSequence(id sofab.ID) (any, error) {
	if v.skip[uint32(id)] {
		return nil, nil // decline the whole sub-tree
	}
	*v.log = append(*v.log, fmt.Sprintf("seqbegin/%d", id))
	return v, nil
}

func (v skipIDsV) EndSequence() error {
	*v.log = append(*v.log, "seqend")
	return nil
}

// skipUnreadV is the second, stronger skip destination: it sits on the
// corelib's OWN decode surface (§5.3.1), with the aggregate adapter behind it,
// and drops a skipped id BEFORE any assembly happens — no FixlenBegin, no
// payload piece appended, no array element accumulated, no begin/end forwarded.
// The field is genuinely left unread, which is what `skip_ids` means on a push
// API where a destination "skips" by having nothing bound to the id.
//
// skipIDsV above filters the recorded log instead, which is the weaker half:
// the adapter still assembles the skipped value. Running both matters because
// only this one leaves the decoder to locate the next field from the wire
// alone — so a skip that consumes one byte too many or too few surfaces as a
// wrong anchor value rather than being papered over by the destination.
//
// A skipped SEQUENCE id is declined outright (BeginSequence returns nil), which
// is the decoder's own skip path: the sub-tree is walked to its end marker at
// any depth and nothing inside it is delivered.
type skipUnreadV struct {
	skip map[uint32]bool
	in   sofab.Visitor // assembles the fields that are kept
}

func (v *skipUnreadV) drop(id sofab.ID) bool { return v.skip[uint32(id)] }

func (v *skipUnreadV) Unsigned(id sofab.ID, x uint64) error {
	if v.drop(id) {
		return nil
	}
	return v.in.Unsigned(id, x)
}

func (v *skipUnreadV) Signed(id sofab.ID, x int64) error {
	if v.drop(id) {
		return nil
	}
	return v.in.Signed(id, x)
}

func (v *skipUnreadV) Float32(id sofab.ID, x float32) error {
	if v.drop(id) {
		return nil
	}
	return v.in.Float32(id, x)
}

func (v *skipUnreadV) Float64(id sofab.ID, x float64) error {
	if v.drop(id) {
		return nil
	}
	return v.in.Float64(id, x)
}

func (v *skipUnreadV) FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, total int) error {
	if v.drop(id) {
		return nil
	}
	return v.in.FixlenBegin(id, sub, total)
}

func (v *skipUnreadV) String(id sofab.ID, total, offset int, chunk []byte) error {
	if v.drop(id) {
		return nil
	}
	return v.in.String(id, total, offset, chunk)
}

func (v *skipUnreadV) Bytes(id sofab.ID, total, offset int, chunk []byte) error {
	if v.drop(id) {
		return nil
	}
	return v.in.Bytes(id, total, offset, chunk)
}

func (v *skipUnreadV) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
	if v.drop(id) {
		return nil
	}
	return v.in.ArrayBegin(id, kind, count)
}

func (v *skipUnreadV) ArrayUnsigned(id sofab.ID, i int, x uint64) error {
	if v.drop(id) {
		return nil
	}
	return v.in.ArrayUnsigned(id, i, x)
}

func (v *skipUnreadV) ArraySigned(id sofab.ID, i int, x int64) error {
	if v.drop(id) {
		return nil
	}
	return v.in.ArraySigned(id, i, x)
}

func (v *skipUnreadV) ArrayFloat32(id sofab.ID, i int, x float32) error {
	if v.drop(id) {
		return nil
	}
	return v.in.ArrayFloat32(id, i, x)
}

func (v *skipUnreadV) ArrayFloat64(id sofab.ID, i int, x float64) error {
	if v.drop(id) {
		return nil
	}
	return v.in.ArrayFloat64(id, i, x)
}

func (v *skipUnreadV) ArrayEnd(id sofab.ID) error {
	if v.drop(id) {
		return nil
	}
	return v.in.ArrayEnd(id)
}

func (v *skipUnreadV) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	if v.drop(id) {
		return nil, nil // the decoder walks the whole sub-tree and delivers none of it
	}
	child, err := v.in.BeginSequence(id)
	if err != nil || child == nil {
		return child, err
	}
	// The same ids are skipped at every nesting level, so the child is filtered
	// exactly like its parent.
	return &skipUnreadV{skip: v.skip, in: child}, nil
}

func (v *skipUnreadV) EndSequence() error { return v.in.EndSequence() }

// SetStringCheck forwards the decode's UTF-8 policy to the destination behind
// the filter, which is where §6.4 has it applied.
func (v *skipUnreadV) SetStringCheck(p sofab.StringCheck) {
	if sp, ok := v.in.(sofab.StringPolicyVisitor); ok {
		sp.SetStringCheck(p)
	}
}

// expectSkipped is expectLog with the skipped ids — and, for a skipped
// sequence, its whole sub-tree — removed.
func expectSkipped(t *testing.T, fields []vecField, skip map[uint32]bool) []string {
	t.Helper()
	var kept []vecField
	for i := 0; i < len(fields); {
		f := fields[i]
		if f.Op != "sequence_end" && skip[f.ID] {
			if f.Op == "sequence_begin" {
				i = advancePastSequence(fields, i)
			} else {
				i++
			}
			continue
		}
		kept = append(kept, f)
		i++
	}
	return expectLog(t, kept)
}

// skipEntryPoints is how many decode entry points the skip-ids scenario replays
// each vector through (see `runs` below). Three of them feed one byte at a
// time, which is where a resync bug that a single-buffer feed hides shows up;
// two use the destination that leaves a skipped field entirely unread.
const skipEntryPoints = 6

// TestVectorSkipIDs drives the skip-ids decode scenario: for every vector that
// carries skip_ids -- 58 of them since corelib-c-cpp#160 was adopted here, up
// from 8 -- the listed field ids are left unread at every nesting level, so the
// decoder auto-skips the field whatever its wire type, and the whole sub-tree
// when the id names a sequence. Everything else must still decode to its exact
// value and the message must be fully consumed. Run all-at-once AND
// one-byte-at-a-time, so every skip is also exercised across chunk boundaries
// (CORELIB_PLAN §7.2 items 4 and 7).
func TestVectorSkipIDs(t *testing.T) {
	vf := loadVectors(t)
	ran := 0
	for _, v := range vf.Vectors {
		if len(v.SkipIDs) == 0 {
			continue
		}
		ran++
		skip := make(map[uint32]bool, len(v.SkipIDs))
		for _, id := range v.SkipIDs {
			skip[id] = true
		}
		raw, err := hex.DecodeString(v.Serialized.Hex)
		if err != nil {
			t.Fatalf("%s: hex: %v", v.Name, err)
		}
		want := strings.Join(expectSkipped(t, v.Fields, skip), "|")

		// All three entry points, plus a one-byte reader, so skipping is proven
		// to resume across any read boundary.
		runs := map[string]func(any) error{
			"AcceptBytes": func(vis any) error { return acceptBytes(raw, vis) },
			"Feed":        func(vis any) error { return feedIn(raw, 0, vis) },
			"Feed/1-byte": func(vis any) error {
				return feedIn(raw, 1, vis)
			},
			"FeedFrom/one-byte": func(vis any) error {
				return feedFrom(iotest.OneByteReader(bytes.NewReader(raw)), 1, vis)
			},
		}
		// The same two chunkings again, but through the destination that leaves
		// a skipped field genuinely unread (skipUnreadV) instead of filtering it
		// out of the log afterwards.
		unread := func(vis any) sofab.Visitor {
			return &skipUnreadV{skip: skip, in: asVisitor(vis)}
		}
		runs["AcceptBytes/unread"] = func(vis any) error {
			return acceptBytes(raw, unread(vis))
		}
		runs["Feed/1-byte/unread"] = func(vis any) error {
			return feedIn(raw, 1, unread(vis))
		}
		if len(runs) != skipEntryPoints {
			t.Fatalf("skipEntryPoints = %d, but %d entry points are wired up", skipEntryPoints, len(runs))
		}
		checks := 0
		for name, run := range runs {
			t.Run(v.Name+"/"+name, func(t *testing.T) {
				var log []string
				if err := run(skipIDsV{skip: skip, log: &log}); err != nil {
					t.Fatalf("%s = %v, want COMPLETE", name, err)
				}
				checks++ // the message was fully consumed (COMPLETE)
				if got := strings.Join(log, "|"); got != want {
					t.Fatalf("%s events =\n %s\nwant\n %s", name, got, want)
				}
				checks++ // every surviving field decoded to its exact value
			})
		}
		vecRan("decode/skip-ids", checks)
	}
	if ran == 0 {
		t.Fatal("no vectors carried skip_ids; expected the suite to exercise the skip-ids scenario")
	}
	t.Logf("skip-ids scenario: %d of %d vectors x %d entry points (three of them byte-at-a-time)",
		ran, len(vf.Vectors), skipEntryPoints)
}

// --- requires (capability tags) ----------------------------------------------

// goCaps is the set of optional wire-format capabilities this library supports.
// The Go core ships the full format with no build toggles, so it supports every
// capability a vector can declare — and therefore runs every vector regardless
// of its "requires". Per the test-vector README, a full-feature implementation
// ignores "requires" and runs all vectors; the tags only let a feature-reduced
// build skip what it cannot represent. We still validate the tags below.
var goCaps = map[string]bool{
	"fixlen": true, "array": true, "sequence": true, "fp64": true, "int64": true,
}

// Boundaries that decide the int64 capability, mirroring the generator: a value,
// array element, or field-header id that does not fit the 32-bit value domain
// requires int64. The id cap is the largest id whose (id<<3)|type header still
// fits in a uint32 varint, i.e. (2^32-1)>>3.
const (
	capU32Max  = 0xFFFFFFFF
	capI32Max  = 0x7FFFFFFF
	capI32Min  = -0x80000000
	capIDCap32 = 0x1FFFFFFF
)

func needsInt64U(x uint64) bool { return x > capU32Max }
func needsInt64I(x int64) bool  { return x > capI32Max || x < capI32Min }

// deriveRequires recomputes the capability tags a vector needs from its fields,
// the same way the generator derives them, so the declared "requires" cannot
// silently drift from the actual content.
func deriveRequires(t *testing.T, v vector) map[string]bool {
	t.Helper()
	caps := map[string]bool{}
	for _, f := range v.Fields {
		switch f.Op {
		case "fp32", "string", "blob":
			caps["fixlen"] = true
		case "fp64":
			caps["fixlen"] = true
			caps["fp64"] = true
		case "sequence_begin":
			caps["sequence"] = true
		case "unsigned":
			if needsInt64U(pUint(t, f.Value)) {
				caps["int64"] = true
			}
		case "signed":
			if needsInt64I(pInt(t, f.Value)) {
				caps["int64"] = true
			}
		case "array":
			caps["array"] = true
			switch f.ElementType {
			case "fp32":
				caps["fixlen"] = true
			case "fp64":
				caps["fixlen"] = true
				caps["fp64"] = true
			}
			for _, r := range f.Values {
				switch f.ElementType[0] {
				case 'u':
					if needsInt64U(pUint(t, r)) {
						caps["int64"] = true
					}
				case 'i':
					if needsInt64I(pInt(t, r)) {
						caps["int64"] = true
					}
				}
			}
		}
		// id-driven int64: the (id<<3)|type header must fit in a uint32 varint.
		if f.Op != "sequence_end" && uint64(f.ID) > capIDCap32 {
			caps["int64"] = true
		}
	}
	return caps
}

func sameCaps(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestVectorRequires exercises the new "requires" capability tags. For the Go
// core (full format), every declared capability must be one it supports, so no
// vector is ever skipped — the encode/decode/skip-ids scenarios above iterate
// the whole suite unconditionally, which is the documented full-feature
// behavior. It also re-derives each vector's capabilities from its fields and
// asserts they match the declared "requires", so the tags can't drift.
func TestVectorRequires(t *testing.T) {
	vf := loadVectors(t)
	withRequires := 0
	for _, v := range vf.Vectors {
		if len(v.Requires) > 0 {
			withRequires++
		}
		t.Run(v.Name, func(t *testing.T) {
			for _, c := range v.Requires {
				if !goCaps[c] {
					t.Fatalf("vector requires %q, which this full-format build should support", c)
				}
			}
			want := make(map[string]bool, len(v.Requires))
			for _, c := range v.Requires {
				want[c] = true
			}
			if got := deriveRequires(t, v); !sameCaps(got, want) {
				t.Fatalf("requires mismatch: declared %v, derived from fields %v", want, got)
			}
			vecRan("requires", len(v.Requires)+1)
		})
	}
	if withRequires == 0 {
		t.Fatal("no vectors declared requires; expected the suite to exercise the capability tags")
	}
}

// --- the shared invalid-UTF-8 group ------------------------------------------

// TestVectorInvalidUTF8 replays the shared file's `invalid_utf8` group, which
// nothing in this repo read before (issue #88): the vectors were shipped in
// assets/test_vectors.json and silently unused, so the one corpus that states
// the §6.4 verdict in a language-agnostic form was not asserting anything here.
//
// It is deliberately written against the build rather than against the ON
// verdict alone. §6.4 gives a footprint build permission to compile the
// validator out, and the identical corpus then has to hold the other half of
// the contract: not "anything goes", but accepted with the wire bytes kept
// VERBATIM — the same bytes the vector declares, on both sides, never a
// replacement character and never an empty value. Whichever build CI runs, one
// of the two verdicts is checked over every row.
func TestVectorInvalidUTF8(t *testing.T) {
	vf := loadVectors(t)
	if len(vf.InvalidUTF8) == 0 {
		t.Fatal("no invalid_utf8 vectors; the shared file is expected to carry the §6.4 group")
	}
	for _, v := range vf.InvalidUTF8 {
		t.Run(v.Name, func(t *testing.T) {
			if v.DecodeOutcome != "invalid" || v.EncodeOutcome != "invalid_argument" {
				t.Fatalf("unexpected outcomes: decode=%q encode=%q", v.DecodeOutcome, v.EncodeOutcome)
			}
			payload, err := hex.DecodeString(v.StringHex)
			if err != nil {
				t.Fatalf("string_hex: %v", err)
			}
			wire, err := hex.DecodeString(v.SerializedHex)
			if err != nil {
				t.Fatalf("serialized_hex: %v", err)
			}
			id := sofab.ID(v.ID)

			// Encode: ErrArgument, symmetrically with decode (§6.4). With the
			// validator compiled out the bytes must reproduce the vector.
			var buf bytes.Buffer
			e := sofab.NewEncoder(&buf)
			checkUTF8Encode(t, "WriteString", e.WriteString(id, string(payload)))
			if !utf8CheckCompiled {
				if err := e.Flush(); err != nil {
					t.Fatalf("flush: %v", err)
				}
				if got := hex.EncodeToString(buf.Bytes()); got != v.SerializedHex {
					t.Fatalf("encode mismatch\n got: %s\nwant: %s", got, v.SerializedHex)
				}
			}

			// Decode: the destination is where the check runs (§6.4.3).
			bind := &bindStrV{id: id}
			checkUTF8Decode(t, "visitor destination", acceptBytes(wire, bind))
			if !utf8CheckCompiled && bind.got != string(payload) {
				t.Fatalf("destination bound % X, want % X verbatim", bind.got, payload)
			}

			// A visitor with no destination for the id skips the payload, and a
			// skip is never validated — in either build (§6.4).
			if err := acceptBytes(wire, &bindStrV{id: id + 1}); err != nil {
				t.Fatalf("skipped payload = %v, want nil", err)
			}
			vecRan("invalid-utf8", 4) // outcomes, encode verdict, decode verdict, skipped payload
		})
	}
}
