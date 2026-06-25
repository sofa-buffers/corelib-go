package sofab_test

// Conformance tests driven by the shared, language-agnostic vector suite
// (assets/test_vectors.json, copied verbatim from the documentation repo).
// Each vector is replayed through the encoder (bytes must equal serialized.hex)
// and fed through the decoder (recovered values must equal the fields).

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"

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
