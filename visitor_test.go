package sofab_test

// Unit tests for the visitor-driven decode path (Decoder.Accept). Every shared
// vector is replayed through a recording visitor and compared field-for-field
// to the expected ops, plus error-propagation and malformed-input coverage.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// TestAcceptBytesMatchesAccept proves the zero-copy buffer entry point produces
// the exact same event stream as the reader-backed Accept for every vector.
func TestAcceptBytesMatchesAccept(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Serialized.Hex)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			var got []string
			if err := sofab.AcceptBytes(raw, recorder{&got}); err != nil {
				t.Fatalf("AcceptBytes: %v", err)
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
	cases := map[string]struct {
		in   []byte
		want error
	}{
		"truncated unsigned":    {append(vhdr(1, sofab.TypeVarintUnsigned), 0x80), sofab.ErrIncomplete},
		"truncated fixlen":      {append(vhdr(1, sofab.TypeFixlen), 0x80), sofab.ErrIncomplete},
		"bad fixlen subtype":    {append(vhdr(1, sofab.TypeFixlen), vbytes((4<<3)|0x4)...), sofab.ErrInvalidMsg},
		"truncated array":       {append(vhdr(1, sofab.TypeVarintArrayUnsigned), append(vbytes(2), 0x01, 0x80)...), sofab.ErrIncomplete},
		"bad fixlen-array elem": {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), vbytes((2<<3)|0x0)...)...), sofab.ErrInvalidMsg},
		"dangling sequence end": {vhdr(0, sofab.TypeSequenceEnd), sofab.ErrInvalidMsg},
		"unterminated sequence": {vhdr(3, sofab.TypeSequenceStart), sofab.ErrIncomplete},
		"fp32 wrong length":     {append(vhdr(1, sofab.TypeFixlen), vbytes((2<<3)|0x0)...), sofab.ErrInvalidMsg},
		"fp64 wrong length":     {append(vhdr(1, sofab.TypeFixlen), vbytes((4<<3)|0x1)...), sofab.ErrInvalidMsg},
		"truncated fp32":        {append(vhdr(1, sofab.TypeFixlen), append(vbytes((4<<3)|0x0), 0xAA, 0xBB)...), sofab.ErrIncomplete},
		"truncated fp64":        {append(vhdr(1, sofab.TypeFixlen), append(vbytes((8<<3)|0x1), 0x01)...), sofab.ErrIncomplete},
		"truncated string":      {append(vhdr(1, sofab.TypeFixlen), append(vbytes((4<<3)|0x2), 'h', 'i')...), sofab.ErrIncomplete},
		"truncated blob":        {append(vhdr(1, sofab.TypeFixlen), append(vbytes((4<<3)|0x3), 0x01)...), sofab.ErrIncomplete},
		"fp32 array truncated":  {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((4<<3)|0x0), 0x00, 0x00)...)...), sofab.ErrIncomplete},
		"fp64 array bad elem":   {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), vbytes((4<<3)|0x1)...)...), sofab.ErrInvalidMsg},
		"signed array trunc":    {append(vhdr(1, sofab.TypeVarintArraySigned), append(vbytes(2), 0x02, 0x80)...), sofab.ErrIncomplete},
		"array count truncated": {append(vhdr(1, sofab.TypeVarintArrayUnsigned), 0x80), sofab.ErrIncomplete},
		"id above max":          {append(vhdr(sofab.IDMax+1, sofab.TypeVarintUnsigned), 0x00), sofab.ErrInvalidMsg},
		"truncated signed":      {append(vhdr(1, sofab.TypeVarintSigned), 0x80), sofab.ErrIncomplete},
		"signed array count":    {append(vhdr(1, sofab.TypeVarintArraySigned), 0x80), sofab.ErrIncomplete},
		"fixlen array count":    {append(vhdr(1, sofab.TypeFixlenArray), 0x80), sofab.ErrIncomplete},
		"fixlen array header":   {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...), sofab.ErrIncomplete},
		"fp64 array payload":    {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((8<<3)|0x1), 0, 0, 0, 0, 0, 0, 0)...)...), sofab.ErrIncomplete},
		// cursor-specific boundaries: a value expected exactly at end-of-buffer,
		// a varint that overflows 64 bits while reading the next header, and a
		// fixlen length past the cap. (A zero-count array is no longer malformed
		// — see TestVisitorEmptyArrays — so it is not listed here.)
		"value at buffer end":    {vhdr(1, sofab.TypeVarintUnsigned), sofab.ErrIncomplete},
		"header varint overflow": {bytes.Repeat([]byte{0x80}, 11), sofab.ErrInvalidMsg},
		"fixlen length over max": {append(vhdr(1, sofab.TypeFixlen), vbytes((uint64(sofab.IDMax+1)<<3)|subStr)...), sofab.ErrInvalidMsg},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var log []string
			if err := newDec(c.in).Accept(recorder{&log}); !errors.Is(err, c.want) {
				t.Fatalf("Accept = %v, want %v", err, c.want)
			}
		})
	}
}

// TestVisitorEmptyArrays confirms the visitor path delivers zero-count arrays as
// empty slices (§4.7/§4.8). An empty fixlen array still carries its fixlen_word,
// so its subtype survives on the wire: an empty fp64 array is delivered as
// Float64Array, not Float32Array.
func TestVisitorEmptyArrays(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		sofab.WriteUnsignedArray(e, 1, []uint32{})
		sofab.WriteSignedArray(e, 2, []int32{})
		e.WriteFloat32Array(3, nil)
		e.WriteFloat64Array(4, nil)
	})
	var log []string
	if err := newDec(in).Accept(recorder{&log}); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	want := []string{evAU(1, nil), evAS(2, nil), evAF32(3, nil), evAF64(4, nil)}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", log, want)
	}
}

// TestDeepNestingRejected covers MAX_DEPTH = 255 (§4.9/§6.2): the encoder refuses
// a 256th nested sequence, and both decode paths reject an over-deep adversarial
// message with ErrInvalidMsg rather than overflowing the Go stack.
func TestDeepNestingRejected(t *testing.T) {
	// Encoder caps at MaxDepth open sequences.
	e := sofab.NewEncoder(io.Discard)
	for i := 0; i < sofab.MaxDepth; i++ {
		if err := e.WriteSequenceBegin(0); err != nil {
			t.Fatalf("begin %d = %v", i, err)
		}
	}
	if err := e.WriteSequenceBegin(0); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("256th begin = %v, want ErrArgument", err)
	}

	// 0x06 = sequence start, id 0; a long run nests far past MaxDepth.
	deep := bytes.Repeat([]byte{0x06}, 100000)
	if err := sofab.AcceptBytes(deep, baseV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("AcceptBytes deep = %v, want ErrInvalidMsg", err)
	}
	d := newDec(deep)
	if _, err := d.Next(); err != nil {
		t.Fatalf("first Next = %v", err)
	}
	if err := d.Skip(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull Skip deep = %v, want ErrInvalidMsg", err)
	}
}

// TestMaxDepthRoundTrip confirms a message nested exactly MaxDepth deep still
// encodes and decodes on both paths.
func TestMaxDepthRoundTrip(t *testing.T) {
	got := encode(t, func(e *sofab.Encoder) {
		for i := 0; i < sofab.MaxDepth; i++ {
			e.WriteSequenceBegin(1)
		}
		e.WriteUnsigned(2, 7)
		for i := 0; i < sofab.MaxDepth; i++ {
			e.WriteSequenceEnd()
		}
	})
	if err := sofab.AcceptBytes(got, baseV{}); err != nil {
		t.Fatalf("AcceptBytes 255-deep = %v", err)
	}
	d := newDec(got)
	depth := 0
	for {
		f, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next = %v", err)
		}
		switch f.Type {
		case sofab.TypeSequenceStart:
			depth++
		case sofab.TypeSequenceEnd:
			depth--
		case sofab.TypeVarintUnsigned:
			if v, _ := d.Unsigned(); v != 7 {
				t.Fatalf("inner unsigned = %d, want 7", v)
			}
		}
	}
	if depth != 0 {
		t.Fatalf("unbalanced sequence depth = %d", depth)
	}
}

// TestInvalidUTF8Rejected covers §6.3: a string field whose payload is not valid
// UTF-8 is rejected as ErrInvalidMsg on both decode paths (blobs stay unchecked).
func TestInvalidUTF8Rejected(t *testing.T) {
	// fixlen string, length 1, payload 0xFF (an invalid UTF-8 byte).
	in := append(vhdr(1, sofab.TypeFixlen), append(vbytes((1<<3)|subStr), 0xFF)...)
	d := newDec(in)
	mustNext(t, d)
	if _, err := d.String(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull String invalid utf8 = %v, want ErrInvalidMsg", err)
	}
	if err := sofab.AcceptBytes(in, baseV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("visitor invalid utf8 = %v, want ErrInvalidMsg", err)
	}
	// A blob with the same payload is fine (blobs are opaque).
	blob := append(vhdr(1, sofab.TypeFixlen), append(vbytes((1<<3)|subBlob), 0xFF)...)
	if err := sofab.AcceptBytes(blob, baseV{}); err != nil {
		t.Fatalf("visitor blob 0xFF = %v, want nil", err)
	}
}

// lenReader reports a remaining length but never delivers those bytes, so
// Accept's sized slurp (the Len-aware fast path) hits a short read.
type lenReader struct{ n int }

func (r lenReader) Len() int               { return r.n }
func (lenReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestVisitorSlurpShortRead(t *testing.T) {
	if err := sofab.NewDecoder(lenReader{4}).Accept(recorder{new([]string)}); err == nil {
		t.Fatal("Accept = nil, want error from truncated sized slurp")
	}
}

func TestVisitorAcceptAfterPullParser(t *testing.T) {
	// Touch the reader through the pull parser first (sets up its buffer), then
	// decode the remainder via the visitor: Accept must slurp from the already
	// in-use reader rather than the raw source. An empty stream is the clean,
	// well-defined case (Next reports EOF, Accept then sees nothing).
	d := sofab.NewDecoder(bytes.NewReader(nil))
	if _, err := d.Next(); err == nil {
		t.Fatal("Next on empty = nil, want io.EOF")
	}
	if err := d.Accept(recorder{new([]string)}); err != nil {
		t.Fatalf("Accept after pull parser = %v, want nil", err)
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
