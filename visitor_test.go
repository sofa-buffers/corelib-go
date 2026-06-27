package sofab_test

// Unit tests for the visitor-driven decode path (Decoder.Accept). Every shared
// vector is replayed through a recording visitor and compared field-for-field
// to the expected ops, plus error-propagation and malformed-input coverage.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// mustEncode runs fn and returns the bytes, for use in Example functions where
// there is no *testing.T.
func mustEncode(fn func(*sofab.Encoder)) []byte {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	fn(e)
	if err := e.Flush(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// baseV is a no-op Visitor; test visitors embed it and override what they need.
type baseV struct{}

func (baseV) Unsigned(sofab.ID, uint64) error                 { return nil }
func (baseV) Signed(sofab.ID, int64) error                    { return nil }
func (baseV) Float32(sofab.ID, float32) error                 { return nil }
func (baseV) Float64(sofab.ID, float64) error                 { return nil }
func (baseV) String(sofab.ID, string) error                   { return nil }
func (baseV) Bytes(sofab.ID, []byte) error                    { return nil }
func (baseV) UnsignedArray(sofab.ID, []uint64) error          { return nil }
func (baseV) SignedArray(sofab.ID, []int64) error             { return nil }
func (baseV) Float32Array(sofab.ID, []float32) error          { return nil }
func (baseV) Float64Array(sofab.ID, []float64) error          { return nil }
func (b baseV) BeginSequence(sofab.ID) (sofab.Visitor, error) { return b, nil }
func (baseV) EndSequence() error                              { return nil }

// --- canonical event formatting (shared by recorder and expectation) ---------

func evU(id sofab.ID, v uint64) string    { return fmt.Sprintf("u/%d/%d", id, v) }
func evS(id sofab.ID, v int64) string     { return fmt.Sprintf("s/%d/%d", id, v) }
func evF32(id sofab.ID, v float32) string { return fmt.Sprintf("f32/%d/%08x", id, math.Float32bits(v)) }
func evF64(id sofab.ID, v float64) string {
	return fmt.Sprintf("f64/%d/%016x", id, math.Float64bits(v))
}
func evStr(id sofab.ID, s string) string  { return fmt.Sprintf("str/%d/%s", id, s) }
func evBlob(id sofab.ID, b []byte) string { return fmt.Sprintf("blob/%d/%x", id, b) }

func joinNums[T any](v []T, f func(T) string) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = f(x)
	}
	return strings.Join(parts, ",")
}

func evAU(id sofab.ID, v []uint64) string {
	return fmt.Sprintf("au/%d/%s", id, joinNums(v, func(x uint64) string { return fmt.Sprintf("%d", x) }))
}
func evAS(id sofab.ID, v []int64) string {
	return fmt.Sprintf("as/%d/%s", id, joinNums(v, func(x int64) string { return fmt.Sprintf("%d", x) }))
}
func evAF32(id sofab.ID, v []float32) string {
	return fmt.Sprintf("af32/%d/%s", id, joinNums(v, func(x float32) string { return fmt.Sprintf("%08x", math.Float32bits(x)) }))
}
func evAF64(id sofab.ID, v []float64) string {
	return fmt.Sprintf("af64/%d/%s", id, joinNums(v, func(x float64) string { return fmt.Sprintf("%016x", math.Float64bits(x)) }))
}

// recorder logs every visited field into a shared slice (children append to the
// same log, so nesting flattens into begin/.../end order).
type recorder struct{ log *[]string }

func (r recorder) add(s string) { *r.log = append(*r.log, s) }

func (r recorder) Unsigned(id sofab.ID, v uint64) error        { r.add(evU(id, v)); return nil }
func (r recorder) Signed(id sofab.ID, v int64) error           { r.add(evS(id, v)); return nil }
func (r recorder) Float32(id sofab.ID, v float32) error        { r.add(evF32(id, v)); return nil }
func (r recorder) Float64(id sofab.ID, v float64) error        { r.add(evF64(id, v)); return nil }
func (r recorder) String(id sofab.ID, s string) error          { r.add(evStr(id, s)); return nil }
func (r recorder) Bytes(id sofab.ID, b []byte) error           { r.add(evBlob(id, b)); return nil }
func (r recorder) UnsignedArray(id sofab.ID, v []uint64) error { r.add(evAU(id, v)); return nil }
func (r recorder) SignedArray(id sofab.ID, v []int64) error    { r.add(evAS(id, v)); return nil }
func (r recorder) Float32Array(id sofab.ID, v []float32) error { r.add(evAF32(id, v)); return nil }
func (r recorder) Float64Array(id sofab.ID, v []float64) error { r.add(evAF64(id, v)); return nil }
func (r recorder) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	r.add(fmt.Sprintf("seqbegin/%d", id))
	return r, nil
}
func (r recorder) EndSequence() error { r.add("seqend"); return nil }

// expectLog builds the canonical event log a correct visitor must produce.
func expectLog(t *testing.T, fields []vecField) []string {
	t.Helper()
	var log []string
	for _, f := range fields {
		id := sofab.ID(f.ID)
		switch f.Op {
		case "unsigned":
			log = append(log, evU(id, pUint(t, f.Value)))
		case "signed":
			log = append(log, evS(id, pInt(t, f.Value)))
		case "boolean": // bool is encoded/decoded as an unsigned 0/1
			var u uint64
			if pBool(t, f.Value) {
				u = 1
			}
			log = append(log, evU(id, u))
		case "fp32":
			log = append(log, evF32(id, float32(pFloat(t, f.Value))))
		case "fp64":
			log = append(log, evF64(id, pFloat(t, f.Value)))
		case "string":
			log = append(log, evStr(id, pString(t, f.Value)))
		case "blob":
			b, _ := hex.DecodeString(f.ValueHex)
			log = append(log, evBlob(id, b))
		case "array":
			log = append(log, expectArray(t, f))
		case "sequence_begin":
			log = append(log, fmt.Sprintf("seqbegin/%d", id))
		case "sequence_end":
			log = append(log, "seqend")
		default:
			t.Fatalf("unknown op %q", f.Op)
		}
	}
	return log
}

func expectArray(t *testing.T, f vecField) string {
	t.Helper()
	id := sofab.ID(f.ID)
	switch f.ElementType {
	case "u8", "u16", "u32", "u64":
		v := make([]uint64, len(f.Values))
		for i, r := range f.Values {
			v[i] = pUint(t, r)
		}
		return evAU(id, v)
	case "i8", "i16", "i32", "i64":
		v := make([]int64, len(f.Values))
		for i, r := range f.Values {
			v[i] = pInt(t, r)
		}
		return evAS(id, v)
	case "fp32":
		v := make([]float32, len(f.Values))
		for i, r := range f.Values {
			v[i] = float32(pFloat(t, r))
		}
		return evAF32(id, v)
	case "fp64":
		v := make([]float64, len(f.Values))
		for i, r := range f.Values {
			v[i] = pFloat(t, r)
		}
		return evAF64(id, v)
	default:
		t.Fatalf("unknown element_type %q", f.ElementType)
		return ""
	}
}

func TestVisitorDecodesAllVectors(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Serialized.Hex)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			var got []string
			if err := newDec(raw).Accept(recorder{&got}); err != nil {
				t.Fatalf("Accept: %v", err)
			}
			want := expectLog(t, v.Fields)
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("event mismatch\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

// failOn returns its sentinel error from the named visitor method, so we can
// prove Accept surfaces a visitor error from every callback verbatim.
type failOn struct {
	which string
	err   error
}

func (f failOn) hit(name string) error {
	if f.which == name {
		return f.err
	}
	return nil
}
func (f failOn) Unsigned(sofab.ID, uint64) error        { return f.hit("Unsigned") }
func (f failOn) Signed(sofab.ID, int64) error           { return f.hit("Signed") }
func (f failOn) Float32(sofab.ID, float32) error        { return f.hit("Float32") }
func (f failOn) Float64(sofab.ID, float64) error        { return f.hit("Float64") }
func (f failOn) String(sofab.ID, string) error          { return f.hit("String") }
func (f failOn) Bytes(sofab.ID, []byte) error           { return f.hit("Bytes") }
func (f failOn) UnsignedArray(sofab.ID, []uint64) error { return f.hit("UnsignedArray") }
func (f failOn) SignedArray(sofab.ID, []int64) error    { return f.hit("SignedArray") }
func (f failOn) Float32Array(sofab.ID, []float32) error { return f.hit("Float32Array") }
func (f failOn) Float64Array(sofab.ID, []float64) error { return f.hit("Float64Array") }
func (f failOn) BeginSequence(sofab.ID) (sofab.Visitor, error) {
	if f.which == "BeginSequence" {
		return nil, f.err
	}
	return f, nil
}
func (f failOn) EndSequence() error { return f.hit("EndSequence") }

func TestVisitorPropagatesErrors(t *testing.T) {
	seq := func(e *sofab.Encoder) { e.WriteSequenceBegin(1); e.WriteSequenceEnd() }
	build := map[string]func(*sofab.Encoder){
		"Unsigned":      func(e *sofab.Encoder) { e.WriteUnsigned(1, 5) },
		"Signed":        func(e *sofab.Encoder) { e.WriteSigned(1, -5) },
		"Float32":       func(e *sofab.Encoder) { e.WriteFloat32(1, 1.5) },
		"Float64":       func(e *sofab.Encoder) { e.WriteFloat64(1, 1.5) },
		"String":        func(e *sofab.Encoder) { e.WriteString(1, "x") },
		"Bytes":         func(e *sofab.Encoder) { e.WriteBytes(1, []byte{1}) },
		"UnsignedArray": func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 1, []uint32{1}) },
		"SignedArray":   func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 1, []int32{-1}) },
		"Float32Array":  func(e *sofab.Encoder) { e.WriteFloat32Array(1, []float32{1}) },
		"Float64Array":  func(e *sofab.Encoder) { e.WriteFloat64Array(1, []float64{1}) },
		"BeginSequence": seq,
		"EndSequence":   seq,
	}
	sentinel := errors.New("stop")
	for method, fn := range build {
		t.Run(method, func(t *testing.T) {
			msg := encode(t, fn)
			if err := newDec(msg).Accept(failOn{which: method, err: sentinel}); !errors.Is(err, sentinel) {
				t.Fatalf("Accept = %v, want sentinel", err)
			}
		})
	}
}

func TestVisitorReaderError(t *testing.T) {
	// A non-EOF reader error must surface verbatim (errReader is in coverage_test.go).
	sentinel := errors.New("io boom")
	var log []string
	if err := sofab.NewDecoder(errReader{sentinel}).Accept(recorder{&log}); !errors.Is(err, sentinel) {
		t.Fatalf("Accept = %v, want sentinel", err)
	}
}

func TestVisitorMalformed(t *testing.T) {
	cases := map[string][]byte{
		"truncated unsigned":    append(vhdr(1, sofab.TypeVarintUnsigned), 0x80),
		"truncated fixlen":      append(vhdr(1, sofab.TypeFixlen), 0x80),
		"bad fixlen subtype":    append(vhdr(1, sofab.TypeFixlen), vbytes((4<<3)|0x4)...),
		"truncated array":       append(vhdr(1, sofab.TypeVarintArrayUnsigned), append(vbytes(2), 0x01, 0x80)...),
		"bad fixlen-array elem": append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), vbytes((2<<3)|0x0)...)...),
		"dangling sequence end": vhdr(0, sofab.TypeSequenceEnd),
		"unterminated sequence": vhdr(3, sofab.TypeSequenceStart),
		"fp32 wrong length":     append(vhdr(1, sofab.TypeFixlen), vbytes((2<<3)|0x0)...),
		"fp64 wrong length":     append(vhdr(1, sofab.TypeFixlen), vbytes((4<<3)|0x1)...),
		"truncated fp32":        append(vhdr(1, sofab.TypeFixlen), append(vbytes((4<<3)|0x0), 0xAA, 0xBB)...),
		"truncated fp64":        append(vhdr(1, sofab.TypeFixlen), append(vbytes((8<<3)|0x1), 0x01)...),
		"truncated string":      append(vhdr(1, sofab.TypeFixlen), append(vbytes((4<<3)|0x2), 'h', 'i')...),
		"truncated blob":        append(vhdr(1, sofab.TypeFixlen), append(vbytes((4<<3)|0x3), 0x01)...),
		"fp32 array truncated":  append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((4<<3)|0x0), 0x00, 0x00)...)...),
		"fp64 array bad elem":   append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), vbytes((4<<3)|0x1)...)...),
		"signed array trunc":    append(vhdr(1, sofab.TypeVarintArraySigned), append(vbytes(2), 0x02, 0x80)...),
		"array count truncated": append(vhdr(1, sofab.TypeVarintArrayUnsigned), 0x80),
		"id above max":          append(vhdr(sofab.IDMax+1, sofab.TypeVarintUnsigned), 0x00),
		"truncated signed":      append(vhdr(1, sofab.TypeVarintSigned), 0x80),
		"signed array count":    append(vhdr(1, sofab.TypeVarintArraySigned), 0x80),
		"fixlen array count":    append(vhdr(1, sofab.TypeFixlenArray), 0x80),
		"fixlen array header":   append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...),
		"fp64 array payload":    append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((8<<3)|0x1), 0, 0, 0, 0, 0, 0, 0)...)...),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var log []string
			if err := newDec(in).Accept(recorder{&log}); !errors.Is(err, sofab.ErrInvalidMsg) {
				t.Fatalf("Accept = %v, want ErrInvalidMsg", err)
			}
		})
	}
}

// Example_visitor shows how generated Unmarshal code consumes a message with
// the visitor: each field binds straight into a struct member.
func Example_visitor() {
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteUnsigned(1, 7)
		e.WriteString(2, "Ada")
	})

	var p struct {
		ID   uint64
		Name string
	}
	_ = newDec(msg).Accept(&fieldBinder{id: &p.ID, name: &p.Name})
	fmt.Printf("id=%d name=%s\n", p.ID, p.Name)
	// Output: id=7 name=Ada
}

type fieldBinder struct {
	baseV
	id   *uint64
	name *string
}

func (b *fieldBinder) Unsigned(id sofab.ID, v uint64) error {
	if id == 1 {
		*b.id = v
	}
	return nil
}
func (b *fieldBinder) String(id sofab.ID, s string) error {
	if id == 2 {
		*b.name = s
	}
	return nil
}
