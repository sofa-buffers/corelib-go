package sofab_test

// Conformance tests driven by the shared, language-agnostic vector suite
// (assets/test_vectors.json, copied verbatim from the documentation repo).
// Each vector is replayed through the encoder (bytes must equal serialized.hex)
// and fed through the decoder (recovered values must equal the fields).

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"strconv"
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

type vectorFile struct {
	Format  string   `json:"format"`
	Version int      `json:"version"`
	Vectors []vector `json:"vectors"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("assets/test_vectors.json")
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
		e.WriteSequenceBegin(id)
	case "sequence_end":
		e.WriteSequenceEnd()
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
		})
	}
}

// --- decode side -------------------------------------------------------------

func decodeField(t *testing.T, d *sofab.Decoder, f vecField) {
	t.Helper()
	fd, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	switch f.Op {
	case "sequence_begin":
		if fd.Type != sofab.TypeSequenceStart || fd.ID != sofab.ID(f.ID) {
			t.Fatalf("seq begin: got %+v", fd)
		}
		return
	case "sequence_end":
		if fd.Type != sofab.TypeSequenceEnd {
			t.Fatalf("seq end: got %+v", fd)
		}
		return
	}
	if fd.ID != sofab.ID(f.ID) {
		t.Fatalf("id = %d, want %d (op %s)", fd.ID, f.ID, f.Op)
	}
	switch f.Op {
	case "unsigned":
		got, err := d.Unsigned()
		if err != nil || got != pUint(t, f.Value) {
			t.Fatalf("unsigned = %d, %v; want %d", got, err, pUint(t, f.Value))
		}
	case "boolean":
		got, err := d.Bool()
		if err != nil || got != pBool(t, f.Value) {
			t.Fatalf("bool = %v, %v; want %v", got, err, pBool(t, f.Value))
		}
	case "signed":
		got, err := d.Signed()
		if err != nil || got != pInt(t, f.Value) {
			t.Fatalf("signed = %d, %v; want %d", got, err, pInt(t, f.Value))
		}
	case "fp32":
		got, err := d.Float32()
		want := float32(pFloat(t, f.Value))
		if err != nil || math.Float32bits(got) != math.Float32bits(want) {
			t.Fatalf("fp32 = %v, %v; want %v", got, err, want)
		}
	case "fp64":
		got, err := d.Float64()
		want := pFloat(t, f.Value)
		if err != nil || math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("fp64 = %v, %v; want %v", got, err, want)
		}
	case "string":
		got, err := d.String()
		if err != nil || got != pString(t, f.Value) {
			t.Fatalf("string = %q, %v; want %q", got, err, pString(t, f.Value))
		}
	case "blob":
		got, err := d.Bytes()
		want, _ := hex.DecodeString(f.ValueHex)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("blob = %x, %v; want %x", got, err, want)
		}
	case "array":
		decodeArray(t, d, f)
	default:
		t.Fatalf("unknown op %q", f.Op)
	}
}

func decodeArray(t *testing.T, d *sofab.Decoder, f vecField) {
	t.Helper()
	switch f.ElementType {
	case "u8":
		got, err := sofab.ReadUnsignedArray[uint8](d)
		wantU(t, err, len(got), f, func(i int) uint64 { return uint64(got[i]) })
	case "u16":
		got, err := sofab.ReadUnsignedArray[uint16](d)
		wantU(t, err, len(got), f, func(i int) uint64 { return uint64(got[i]) })
	case "u32":
		got, err := sofab.ReadUnsignedArray[uint32](d)
		wantU(t, err, len(got), f, func(i int) uint64 { return uint64(got[i]) })
	case "u64":
		got, err := sofab.ReadUnsignedArray[uint64](d)
		wantU(t, err, len(got), f, func(i int) uint64 { return got[i] })
	case "i8":
		got, err := sofab.ReadSignedArray[int8](d)
		wantI(t, err, len(got), f, func(i int) int64 { return int64(got[i]) })
	case "i16":
		got, err := sofab.ReadSignedArray[int16](d)
		wantI(t, err, len(got), f, func(i int) int64 { return int64(got[i]) })
	case "i32":
		got, err := sofab.ReadSignedArray[int32](d)
		wantI(t, err, len(got), f, func(i int) int64 { return int64(got[i]) })
	case "i64":
		got, err := sofab.ReadSignedArray[int64](d)
		wantI(t, err, len(got), f, func(i int) int64 { return got[i] })
	case "fp32":
		got, err := d.ReadFloat32Array()
		if err != nil || len(got) != len(f.Values) {
			t.Fatalf("fp32 array len = %d, %v; want %d", len(got), err, len(f.Values))
		}
		for i, r := range f.Values {
			if math.Float32bits(got[i]) != math.Float32bits(float32(pFloat(t, r))) {
				t.Fatalf("fp32[%d] = %v, want %v", i, got[i], pFloat(t, r))
			}
		}
	case "fp64":
		got, err := d.ReadFloat64Array()
		if err != nil || len(got) != len(f.Values) {
			t.Fatalf("fp64 array len = %d, %v; want %d", len(got), err, len(f.Values))
		}
		for i, r := range f.Values {
			if math.Float64bits(got[i]) != math.Float64bits(pFloat(t, r)) {
				t.Fatalf("fp64[%d] = %v, want %v", i, got[i], pFloat(t, r))
			}
		}
	default:
		t.Fatalf("unknown element_type %q", f.ElementType)
	}
}

func wantU(t *testing.T, err error, n int, f vecField, get func(int) uint64) {
	t.Helper()
	if err != nil || n != len(f.Values) {
		t.Fatalf("%s array len = %d, %v; want %d", f.ElementType, n, err, len(f.Values))
	}
	for i, r := range f.Values {
		if get(i) != pUint(t, r) {
			t.Fatalf("%s[%d] = %d, want %d", f.ElementType, i, get(i), pUint(t, r))
		}
	}
}

func wantI(t *testing.T, err error, n int, f vecField, get func(int) int64) {
	t.Helper()
	if err != nil || n != len(f.Values) {
		t.Fatalf("%s array len = %d, %v; want %d", f.ElementType, n, err, len(f.Values))
	}
	for i, r := range f.Values {
		if get(i) != pInt(t, r) {
			t.Fatalf("%s[%d] = %d, want %d", f.ElementType, i, get(i), pInt(t, r))
		}
	}
}

func TestVectorDecode(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Serialized.Hex)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			d := sofab.NewDecoder(bytes.NewReader(raw))
			for _, f := range v.Fields {
				decodeField(t, d, f)
			}
		})
	}
}

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

// decodeSkipping replays the message but leaves every id in skip unread, letting
// the decoder skip the field — and, when the id names a sequence, the whole
// nested sub-sequence at any depth. Non-skipped fields are still verified, and
// the stream must end exactly (io.EOF) once the structure is exhausted.
func decodeSkipping(t *testing.T, d *sofab.Decoder, v vector, skip map[uint32]bool) {
	t.Helper()
	fields := v.Fields
	for i := 0; i < len(fields); {
		f := fields[i]
		if f.Op != "sequence_end" && skip[f.ID] {
			fd, err := d.Next()
			if err != nil {
				t.Fatalf("Next (skip id %d): %v", f.ID, err)
			}
			if fd.ID != sofab.ID(f.ID) {
				t.Fatalf("skip: id = %d, want %d", fd.ID, f.ID)
			}
			if err := d.Skip(); err != nil {
				t.Fatalf("Skip id %d: %v", f.ID, err)
			}
			if f.Op == "sequence_begin" {
				i = advancePastSequence(fields, i)
			} else {
				i++
			}
			continue
		}
		decodeField(t, d, f)
		i++
	}
	if _, err := d.Next(); err != io.EOF {
		t.Fatalf("message not fully consumed after skip-ids: Next = %v, want io.EOF", err)
	}
}

// TestVectorSkipIDs drives the skip-ids decode scenario: for every vector that
// carries skip_ids, the listed field ids are skipped (at every nesting level)
// while the rest decode normally. Run all-at-once and one-byte-at-a-time to prove
// skipping resumes across any read boundary (the chunked variant).
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
		readers := map[string]func() io.Reader{
			"all-at-once": func() io.Reader { return bytes.NewReader(raw) },
			"one-byte":    func() io.Reader { return iotest.OneByteReader(bytes.NewReader(raw)) },
		}
		for name, mk := range readers {
			t.Run(v.Name+"/"+name, func(t *testing.T) {
				decodeSkipping(t, sofab.NewDecoder(mk()), v, skip)
			})
		}
	}
	if ran == 0 {
		t.Fatal("no vectors carried skip_ids; expected the suite to exercise the skip-ids scenario")
	}
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
		})
	}
	if withRequires == 0 {
		t.Fatal("no vectors declared requires; expected the suite to exercise the capability tags")
	}
}
