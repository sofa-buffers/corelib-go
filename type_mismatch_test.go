package sofab_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// MESSAGE_SPEC §7.3 / CORELIB_PLAN §6.3: a typed read whose declared type
// contradicts the field on the wire — a different wire type, or for a fixlen a
// different subtype — must be skipped exactly like an unknown id. It is neither
// InvalidMessage nor an argument error, the destination stays untouched, and the
// decode that meets nothing else stays COMPLETE (issue #79).

// wantMismatch asserts err is the §7.3 non-error and nothing else: not INVALID,
// not INCOMPLETE, not an argument error.
func wantMismatch(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, sofab.ErrTypeMismatch) {
		t.Fatalf("err = %v, want ErrTypeMismatch", err)
	}
	if errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("err = %v: a §7.3 mismatch must not be InvalidMessage", err)
	}
	if errors.Is(err, sofab.ErrIncomplete) || errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("err = %v: a §7.3 mismatch is neither INCOMPLETE nor an argument error", err)
	}
}

// TestTypeMismatchIssue79Repro is the reproducer from issue #79 verbatim: a
// fixlen blob read as a string and as an unsigned. One reported the well-formed
// message as malformed, the other as "invalid usage" — a code §6.3 removed.
func TestTypeMismatchIssue79Repro(t *testing.T) {
	b, err := hex.DecodeString("022341424344") // fixlen id 0, subtype blob, "ABCD"
	if err != nil {
		t.Fatal(err)
	}

	d := sofab.NewDecoder(bytes.NewReader(b))
	mustNext(t, d)
	s, err := d.String()
	wantMismatch(t, err)
	if s != "" {
		t.Fatalf("String = %q, want the destination left untouched", s)
	}
	// The field was consumed, so the decode ends COMPLETE at the top level.
	if _, err := d.Next(); err != io.EOF {
		t.Fatalf("Next after the skip = %v, want io.EOF (COMPLETE)", err)
	}

	d2 := sofab.NewDecoder(bytes.NewReader(b))
	mustNext(t, d2)
	v, err := d2.Unsigned()
	wantMismatch(t, err)
	if v != 0 {
		t.Fatalf("Unsigned = %d, want the destination left untouched", v)
	}
	if _, err := d2.Next(); err != io.EOF {
		t.Fatalf("Next after the skip = %v, want io.EOF (COMPLETE)", err)
	}
}

// mismatchReaders are the typed pull readers, each paired with the wire field it
// declares. Every reader run against any *other* row's field is a §7.3 mismatch.
type mismatchReader struct {
	name  string
	write func(*sofab.Encoder)              // the matching field, id 1
	read  func(*sofab.Decoder) (any, error) // the typed read
	zero  any                               // value the reader must return on a mismatch
}

func mismatchReaders() []mismatchReader {
	return []mismatchReader{
		{
			"Unsigned",
			func(e *sofab.Encoder) { e.WriteUnsigned(1, 42) },
			func(d *sofab.Decoder) (any, error) { return d.Unsigned() },
			uint64(0),
		},
		{
			"Signed",
			func(e *sofab.Encoder) { e.WriteSigned(1, -42) },
			func(d *sofab.Decoder) (any, error) { return d.Signed() },
			int64(0),
		},
		{
			"Float32",
			func(e *sofab.Encoder) { e.WriteFloat32(1, 1.5) },
			func(d *sofab.Decoder) (any, error) { return d.Float32() },
			float32(0),
		},
		{
			"Float64",
			func(e *sofab.Encoder) { e.WriteFloat64(1, 1.5) },
			func(d *sofab.Decoder) (any, error) { return d.Float64() },
			float64(0),
		},
		{
			"String",
			func(e *sofab.Encoder) { e.WriteString(1, "hi") },
			func(d *sofab.Decoder) (any, error) { return d.String() },
			"",
		},
		{
			"Bytes",
			func(e *sofab.Encoder) { e.WriteBytes(1, []byte("hi")) },
			func(d *sofab.Decoder) (any, error) { return d.Bytes() },
			[]byte(nil),
		},
		{
			"ReadUnsignedArray",
			func(e *sofab.Encoder) { sofab.WriteUnsignedArray(e, 1, []uint32{1, 2}) },
			func(d *sofab.Decoder) (any, error) { return sofab.ReadUnsignedArray[uint32](d) },
			[]uint32(nil),
		},
		{
			"ReadSignedArray",
			func(e *sofab.Encoder) { sofab.WriteSignedArray(e, 1, []int32{-1, 2}) },
			func(d *sofab.Decoder) (any, error) { return sofab.ReadSignedArray[int32](d) },
			[]int32(nil),
		},
		{
			"ReadFloat32Array",
			func(e *sofab.Encoder) { e.WriteFloat32Array(1, []float32{1, 2}) },
			func(d *sofab.Decoder) (any, error) { return d.ReadFloat32Array() },
			[]float32(nil),
		},
		{
			"ReadFloat64Array",
			func(e *sofab.Encoder) { e.WriteFloat64Array(1, []float64{1, 2}) },
			func(d *sofab.Decoder) (any, error) { return d.ReadFloat64Array() },
			[]float64(nil),
		},
	}
}

// TestTypeMismatchEveryReaderSkipsAndResumes runs the full cross product: every
// typed reader against every non-matching field. Each pairing must report
// ErrTypeMismatch, leave the destination at its zero value, consume the field,
// and leave the decoder on the next field boundary so the following field still
// decodes and the message ends COMPLETE.
func TestTypeMismatchEveryReaderSkipsAndResumes(t *testing.T) {
	readers := mismatchReaders()
	for _, w := range readers {
		for _, r := range readers {
			if w.name == r.name {
				continue // the matching pairing is not a mismatch
			}
			t.Run(r.name+"/on/"+w.name, func(t *testing.T) {
				// The contradicting field, then a marker field that must still decode.
				in := encode(t, func(e *sofab.Encoder) {
					w.write(e)
					e.WriteUnsigned(2, 7)
				})
				d := newDec(in)
				mustNext(t, d)
				got, err := r.read(d)
				wantMismatch(t, err)
				if !equalAny(got, r.zero) {
					t.Fatalf("value = %#v, want the destination untouched (%#v)", got, r.zero)
				}
				f := mustNext(t, d)
				if f.ID != 2 || f.Type != sofab.TypeVarintUnsigned {
					t.Fatalf("resync field = %+v, want id 2 unsigned", f)
				}
				v, err := d.Unsigned()
				if err != nil || v != 7 {
					t.Fatalf("resync value = %v %v, want 7 nil", v, err)
				}
				if _, err := d.Next(); err != io.EOF {
					t.Fatalf("Next = %v, want io.EOF (the decode stays COMPLETE)", err)
				}
			})
		}
	}
}

// equalAny compares the reader results without pulling in reflect.DeepEqual's
// nil-vs-empty-slice ambiguity: an empty result is as untouched as a nil one.
func equalAny(got, want any) bool {
	switch g := got.(type) {
	case []byte:
		return len(g) == 0
	case []uint32:
		return len(g) == 0
	case []int32:
		return len(g) == 0
	case []float32:
		return len(g) == 0
	case []float64:
		return len(g) == 0
	default:
		return got == want
	}
}

// TestTypeMismatchFixlenSubtype pins the subtype half of §7.3: the wire type
// matches (fixlen, or a fixlen array) but the subtype names another declared
// type. The field is skipped, not reported as malformed — the same bytes decode
// fine for a peer whose schema declares the other type.
func TestTypeMismatchFixlenSubtype(t *testing.T) {
	cases := []struct {
		name  string
		write func(*sofab.Encoder)
		read  func(*sofab.Decoder) error
	}{
		{"String on a blob", func(e *sofab.Encoder) { e.WriteBytes(1, []byte("ABCD")) },
			func(d *sofab.Decoder) error { _, err := d.String(); return err }},
		{"Bytes on a string", func(e *sofab.Encoder) { e.WriteString(1, "ABCD") },
			func(d *sofab.Decoder) error { _, err := d.Bytes(); return err }},
		{"String on an fp64", func(e *sofab.Encoder) { e.WriteFloat64(1, 1.5) },
			func(d *sofab.Decoder) error { _, err := d.String(); return err }},
		{"Float32 on an fp64", func(e *sofab.Encoder) { e.WriteFloat64(1, 1.5) },
			func(d *sofab.Decoder) error { _, err := d.Float32(); return err }},
		{"Float64 on an fp32", func(e *sofab.Encoder) { e.WriteFloat32(1, 1.5) },
			func(d *sofab.Decoder) error { _, err := d.Float64(); return err }},
		{"Float64 on a string", func(e *sofab.Encoder) { e.WriteString(1, "ABCD") },
			func(d *sofab.Decoder) error { _, err := d.Float64(); return err }},
		{"ReadFloat32Array on an fp64 array", func(e *sofab.Encoder) { e.WriteFloat64Array(1, []float64{1, 2}) },
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err }},
		{"ReadFloat64Array on an fp32 array", func(e *sofab.Encoder) { e.WriteFloat32Array(1, []float32{1, 2}) },
			func(d *sofab.Decoder) error { _, err := d.ReadFloat64Array(); return err }},
		{"ReadFloat32Array on an empty fp64 array", func(e *sofab.Encoder) { e.WriteFloat64Array(1, nil) },
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := encode(t, func(e *sofab.Encoder) {
				c.write(e)
				e.WriteUnsigned(2, 7)
			})
			d := newDec(in)
			mustNext(t, d)
			wantMismatch(t, c.read(d))
			f := mustNext(t, d)
			if f.ID != 2 || f.Type != sofab.TypeVarintUnsigned {
				t.Fatalf("resync field = %+v, want id 2 unsigned", f)
			}
			if v, err := d.Unsigned(); err != nil || v != 7 {
				t.Fatalf("resync value = %v %v, want 7 nil", v, err)
			}
			if _, err := d.Next(); err != io.EOF {
				t.Fatalf("Next = %v, want io.EOF (the decode stays COMPLETE)", err)
			}
		})
	}
}

// TestTypeMismatchOnSequenceMarker: a sequence start/end carries no value, so a
// typed reader on one cannot consume anything — but it is still the §7.3 case,
// not a caller mistake, and the caller skips the sub-tree with Skip as the
// unknown-id path already does.
func TestTypeMismatchOnSequenceMarker(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		e.WriteSequenceBeginLazy(1)
		e.WriteUnsigned(3, 9)
		e.WriteSequenceEnd()
		e.WriteUnsigned(2, 7)
	})
	d := newDec(in)
	f := mustNext(t, d)
	if f.Type != sofab.TypeSequenceStart {
		t.Fatalf("field = %+v, want a sequence start", f)
	}
	_, err := d.Unsigned()
	wantMismatch(t, err)
	// Nothing was consumed: the caller skips the whole sub-tree itself.
	if err := d.Skip(); err != nil {
		t.Fatalf("Skip = %v", err)
	}
	f = mustNext(t, d)
	if f.ID != 2 || f.Type != sofab.TypeVarintUnsigned {
		t.Fatalf("resync field = %+v, want id 2 unsigned", f)
	}
	if v, err := d.Unsigned(); err != nil || v != 7 {
		t.Fatalf("resync value = %v %v, want 7 nil", v, err)
	}

	// The closing marker reads the same way: the caller walks into the sequence
	// itself and meets the end header as an ordinary field.
	d2 := newDec(in)
	mustNext(t, d2)                          // sequence start
	mustNext(t, d2)                          // the u64 inside it
	if _, err := d2.Unsigned(); err != nil { // consume it
		t.Fatalf("inner Unsigned = %v", err)
	}
	f = mustNext(t, d2)
	if f.Type != sofab.TypeSequenceEnd {
		t.Fatalf("field = %+v, want a sequence end", f)
	}
	_, err = d2.Unsigned()
	wantMismatch(t, err)
	f = mustNext(t, d2)
	if f.ID != 2 || f.Type != sofab.TypeVarintUnsigned {
		t.Fatalf("resync field = %+v, want id 2 unsigned", f)
	}
	if v, err := d2.Unsigned(); err != nil || v != 7 {
		t.Fatalf("resync value = %v %v, want 7 nil", v, err)
	}
}

// TestTypedReaderArgumentErrors pins what is left of "caller mistake" after
// §6.3 retired the invalid-usage code: reading with no current field, or reading
// a value that was already consumed, is ErrArgument (InvalidArgument) — and
// never ErrTypeMismatch, which would claim the wire disagreed.
func TestTypedReaderArgumentErrors(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) { e.WriteUnsigned(1, 42) })

	// No Next call yet: there is no field to read.
	d := newDec(in)
	if _, err := d.Unsigned(); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("Unsigned before Next = %v, want ErrArgument", err)
	}
	if errors.Is(err0(d), sofab.ErrTypeMismatch) {
		t.Fatal("an absent field must not be reported as a wire-type mismatch")
	}

	// Value already consumed: the second read has nothing left.
	d2 := newDec(in)
	mustNext(t, d2)
	if v, err := d2.Unsigned(); err != nil || v != 42 {
		t.Fatalf("Unsigned = %v %v, want 42 nil", v, err)
	}
	if _, err := d2.Unsigned(); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("second Unsigned = %v, want ErrArgument", err)
	}
	if _, err := d2.String(); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("String after the value was consumed = %v, want ErrArgument", err)
	}
}

// err0 re-reads on a decoder that has no current field, for the negative
// assertion above.
func err0(d *sofab.Decoder) error {
	_, err := d.Unsigned()
	return err
}

// TestTypeMismatchDoesNotHideMalformedFraming: framing errors keep winning. A
// reserved fixlen subtype or a wrong-width float is INVALID (§4.6/§6.3) even
// when the reader's declared type would not have matched anyway — a mismatch
// verdict must never launder malformed bytes into a skip.
func TestTypeMismatchDoesNotHideMalformedFraming(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		read func(*sofab.Decoder) error
	}{
		{
			"String on a reserved subtype",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|0x4), 'A', 'B', 'C', 'D')...),
			func(d *sofab.Decoder) error { _, err := d.String(); return err },
		},
		{
			"Float32 on a width-8 fp32",
			append(vhdr(0, sofab.TypeFixlen), append(vbytes((8<<3)|subFP32), 0, 0, 0, 0, 0, 0, 0, 0)...),
			func(d *sofab.Decoder) error { _, err := d.Float32(); return err },
		},
		{
			"ReadFloat32Array on a string-subtype element word",
			append(append(vhdr(0, sofab.TypeFixlenArray), vbytes(1)...),
				append(vbytes((4<<3)|subStr), 'A', 'B', 'C', 'D')...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat32Array(); return err },
		},
		{
			"ReadFloat64Array on a width-4 fp64 element word",
			append(append(vhdr(0, sofab.TypeFixlenArray), vbytes(1)...),
				append(vbytes((4<<3)|subFP64), 0, 0, 0, 0)...),
			func(d *sofab.Decoder) error { _, err := d.ReadFloat64Array(); return err },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDec(c.in)
			mustNext(t, d)
			err := c.read(d)
			if !errors.Is(err, sofab.ErrInvalidMsg) {
				t.Fatalf("read = %v, want ErrInvalidMsg", err)
			}
			if errors.Is(err, sofab.ErrTypeMismatch) {
				t.Fatalf("read = %v: malformed framing must not be reported as a §7.3 skip", err)
			}
		})
	}
}

// TestTypeMismatchTruncatedPayloadIsIncomplete: skipping a mismatched field is a
// real consume, so running out of bytes mid-payload is INCOMPLETE — the outcome
// the same truncation gets on the matching read — not a mismatch that silently
// swallowed the tail.
func TestTypeMismatchTruncatedPayloadIsIncomplete(t *testing.T) {
	// fixlen id 0, subtype blob, declared length 4, only two payload bytes.
	in := append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subBlob), 'A', 'B')...)
	d := newDec(in)
	mustNext(t, d)
	if _, err := d.String(); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("String on a truncated blob = %v, want ErrIncomplete", err)
	}

	// Same for a mismatch on the wire type rather than the subtype, where the
	// skip walks the whole value: the truncation wins over the mismatch verdict.
	dw := newDec(in)
	mustNext(t, dw)
	if _, err := dw.Unsigned(); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("Unsigned on a truncated fixlen = %v, want ErrIncomplete", err)
	}

	// Same for a fixlen array whose elements are cut short.
	in2 := append(append(vhdr(0, sofab.TypeFixlenArray), vbytes(2)...),
		append(vbytes((8<<3)|subFP64), 0, 0, 0, 0, 0, 0, 0, 0)...)
	d2 := newDec(in2)
	mustNext(t, d2)
	if _, err := d2.ReadFloat32Array(); !errors.Is(err, sofab.ErrIncomplete) {
		t.Fatalf("ReadFloat32Array on a truncated fp64 array = %v, want ErrIncomplete", err)
	}
}

// TestTypeMismatchPullMatchesVisitorVerdict: the two decode surfaces must agree
// on the same bytes. The visitor path never reports a type mismatch at all (the
// schema lives in the generated code above it), so a message the pull reader
// mis-binds must stay a clean COMPLETE there — which is exactly what makes the
// old ErrInvalidMsg a divergence between the surfaces.
func TestTypeMismatchPullMatchesVisitorVerdict(t *testing.T) {
	in := encode(t, func(e *sofab.Encoder) {
		e.WriteBytes(1, []byte("ABCD"))
		e.WriteUnsigned(2, 7)
	})
	if err := sofab.AcceptBytes(in, baseV{}); err != nil {
		t.Fatalf("AcceptBytes = %v, want nil (COMPLETE)", err)
	}

	d := newDec(in)
	mustNext(t, d)
	wantMismatch(t, func() error { _, err := d.String(); return err }())
	mustNext(t, d)
	if _, err := d.Unsigned(); err != nil {
		t.Fatalf("Unsigned = %v", err)
	}
	if _, err := d.Next(); err != io.EOF {
		t.Fatalf("Next = %v, want io.EOF (COMPLETE, as the visitor path reports)", err)
	}
}
