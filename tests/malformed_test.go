package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// overArrayMax is the smallest count/length the wire format rejects outright:
// one past arrayMax (INT32_MAX). arrayMaxCount (limits_test.go) is the largest
// value that passes the range check; +1 is the first that fails it.
const overArrayMax = arrayMaxCount + 1

// malformedCase is one malformed-input vector plus the single verdict every
// decode surface owes it.
type malformedCase struct {
	in   []byte
	want error // ErrInvalidMsg or ErrIncomplete
}

// malformedCases is the one table of malformed and truncated inputs for the
// "malformed input" and "truncation" obligations of CORELIB_PLAN §7.2 (items 5
// and 6). Every decode surface reaches the same three-valued verdict on the same
// bytes (§5.2), so the table is declared once here and driven through each
// surface by runMalformedCases: the cursor path (Decoder.Feed and the zero-copy
// AcceptBytes) in TestVisitorMalformed, the reader-driven path
// (Decoder.Feed, byte at a time) in TestFeedMalformed, and the
// pull API (Next/Skip) in TestPullMalformed. A new case therefore lands on every
// surface by construction — it cannot be added to one copy and forgotten in
// another, which is how "overlong element vs truncation" below came to hold only
// the streaming path.
//
// Receiver-side limits are deliberately absent: those are ErrLimitExceeded and
// not a wire-format verdict (§6.2.1, limits_test.go). Oversized counts/lengths
// live here only in their structural form (over ID_MAX / over arrayMax).
var malformedCases = map[string]malformedCase{
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
	// The ID_MAX ceiling binds the sequence-end header too (§4.9, §6.2). The
	// end marker sits in a valid position — it closes the open sequence — so
	// its over-ceiling id is the sole reason this is INVALID, isolating the
	// bound from the dangling-end check. Bytes: 76 87 80 80 80 40 (id 2^31).
	"seq-end id above max": {append(vhdr(14, sofab.TypeSequenceStart), vhdr(sofab.IDMax+1, sofab.TypeSequenceEnd)...), sofab.ErrInvalidMsg},
	"truncated signed":     {append(vhdr(1, sofab.TypeVarintSigned), 0x80), sofab.ErrIncomplete},
	"signed array count":   {append(vhdr(1, sofab.TypeVarintArraySigned), 0x80), sofab.ErrIncomplete},
	"fixlen array count":   {append(vhdr(1, sofab.TypeFixlenArray), 0x80), sofab.ErrIncomplete},
	"fixlen array header":  {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), 0x80)...), sofab.ErrIncomplete},
	"fp64 array payload":   {append(vhdr(1, sofab.TypeFixlenArray), append(vbytes(1), append(vbytes((8<<3)|0x1), 0, 0, 0, 0, 0, 0, 0)...)...), sofab.ErrIncomplete},
	// Boundary cases first found on the cursor: a value expected exactly at
	// end-of-buffer, a varint that overflows 64 bits while reading the next
	// header, and a fixlen length past the cap. (A zero-count array is no longer
	// malformed — see TestVisitorEmptyArrays — so it is not listed here.)
	"value at buffer end":    {vhdr(1, sofab.TypeVarintUnsigned), sofab.ErrIncomplete},
	"header varint overflow": {bytes.Repeat([]byte{0x80}, 11), sofab.ErrInvalidMsg},
	"fixlen length over max": {append(vhdr(1, sofab.TypeFixlen), vbytes((uint64(sofab.IDMax+1)<<3)|subStr)...), sofab.ErrInvalidMsg},
	// count 11 over ten all-continuation bytes: INVALID (overlong element)
	// dominates the truncation, the reader-side twin of issue #66.
	"overlong element vs truncation": {append(vhdr(1, sofab.TypeVarintArrayUnsigned), append(vbytes(11), bytes.Repeat([]byte{0x80}, 10)...)...), sofab.ErrInvalidMsg},
}

// runMalformedCases drives every entry of malformedCases through one decode
// surface and holds it to the table's verdict. INVALID and INCOMPLETE are
// mutually exclusive outcomes (§5.2), so the verdict the case is *not* is
// asserted against as well: a regression that swaps them cannot pass.
func runMalformedCases(t *testing.T, decode func(in []byte, v any) error) {
	t.Helper()
	for name, c := range malformedCases {
		c := c
		t.Run(name, func(t *testing.T) {
			var log []string
			err := decode(c.in, recorder{&log})
			if !errors.Is(err, c.want) {
				t.Fatalf("decode = %v, want %v", err, c.want)
			}
			other := sofab.ErrIncomplete
			if errors.Is(c.want, sofab.ErrIncomplete) {
				other = sofab.ErrInvalidMsg
			}
			if errors.Is(err, other) {
				t.Fatalf("decode = %v, must not also match %v", err, other)
			}
		})
	}
}

// TestOversizedCountLengthInvalid covers the "oversized count / length" malformed
// category (CORELIB_PLAN §7): a fixlen length or an array element count strictly
// greater than arrayMax (INT32_MAX) is a structural wire-format violation and is
// rejected as ErrInvalidMsg on both the visitor and pull paths. This is distinct
// from the receiver-side WithMax* limits (ErrLimitExceeded, exercised by the
// TestPartB_* tests, which cap an otherwise-valid value) and from truncation
// (ErrIncomplete): the range check fires on the header varint alone, before any
// payload, so these inputs deliberately carry none. Complements the at-limit
// value 0x7FFF_FFFF exercised in limits_test.go, which passes the check.
//
// The count check lives in arrayCount (decoder.go / cursor.go), and every skip
// path routes its count through it too, so a skipped array is held to the same
// ceiling as a read one: the fixlen-array arm by issue #75
// (TestSkipFixlenArrayHeaderChecked), the integer-array arms by issue #77
// (TestSkipIntegerArrayCountChecked).
func TestOversizedCountLengthInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"fixlen blob length", append(vhdr(0, sofab.TypeFixlen), vbytes((overArrayMax<<3)|subBlob)...)},
		{"fixlen string length", append(vhdr(0, sofab.TypeFixlen), vbytes((overArrayMax<<3)|subStr)...)},
		{"unsigned array count", append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(overArrayMax)...)},
		{"signed array count", append(vhdr(0, sofab.TypeVarintArraySigned), vbytes(overArrayMax)...)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, surface := range surfaces {
				_, err := drainSkipping(surface, c.in)
				if !errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("%s = %v, want ErrInvalidMsg", surface, err)
				}
				// A wire-format violation, not truncation nor a receiver-side limit.
				if errors.Is(err, sofab.ErrIncomplete) || errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatalf("%s = %v, must not match ErrIncomplete/ErrLimitExceeded", surface, err)
				}
			}
		})
	}
}

// TestArrayCountOverRemainingScansElements covers Crucible F-0053 (issue #66): an
// integer array whose declared element count exceeds the bytes remaining is
// truncated, but INVALID dominates INCOMPLETE (§5.2), so an element varint already
// provably overlong from the bytes in hand must be reported as INVALID rather than
// short-circuiting to INCOMPLETE on the count-vs-remaining pre-check.
//
// The array field carries an undeclared id, so the whole field is skipped by the
// corelib with no schema knowledge (§5.2) — the path any forward-compatible
// decoder must accept. Controls pin the neighbours: the same bytes one shorter
// stay INVALID via the ordinary element decode, a genuinely truncated (but
// well-formed) run stays INCOMPLETE, and enough legal elements decode cleanly.
func TestArrayCountOverRemainingScansElements(t *testing.T) {
	// Ten bytes each with the continuation flag set: an element varint that cannot
	// terminate before an eleventh byte, which exceeds the 10-byte/64-bit maximum.
	tenCont := bytes.Repeat([]byte{0x80}, 10)

	// hdr opens field id 8 (undeclared) as an unsigned integer array.
	hdr := vhdr(8, sofab.TypeVarintArrayUnsigned)

	cases := []struct {
		name string
		in   []byte
		want error // ErrInvalidMsg, ErrIncomplete, or nil for a clean decode
	}{
		{
			// The isolate: count 11 > 10 bytes remaining, and those 10 bytes are a
			// provably-overlong varint. INVALID, not INCOMPLETE.
			name: "count over remaining, overlong element",
			in:   append(append(hdr, vbytes(11)...), tenCont...),
			want: sofab.ErrInvalidMsg,
		},
		{
			// ctl_count10_same_bytes: count fits the bytes, so the ordinary element
			// decode reads the same ten bytes as one overlong varint — still INVALID.
			name: "count fits, overlong element",
			in:   append(append(hdr, vbytes(10)...), tenCont...),
			want: sofab.ErrInvalidMsg,
		},
		{
			// Count over remaining but every element in hand is well-formed: the
			// buffer simply runs out. Genuine truncation stays INCOMPLETE.
			name: "count over remaining, well-formed elements",
			in:   append(append(hdr, vbytes(11)...), bytes.Repeat([]byte{0x00}, 10)...),
			want: sofab.ErrIncomplete,
		},
		{
			// ctl_count11_enough_bytes: eleven legal one-byte elements decode cleanly.
			name: "count matches, well-formed elements",
			in:   append(append(hdr, vbytes(11)...), bytes.Repeat([]byte{0x00}, 11)...),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := acceptBytes(c.in, baseV{})
			if c.want == nil {
				if err != nil {
					t.Fatalf("AcceptBytes = %v, want clean decode", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("AcceptBytes = %v, want %v", err, c.want)
			}
			// INVALID and INCOMPLETE are mutually exclusive outcomes here; guard the
			// one this case is not, so a regression that swaps them is caught.
			other := sofab.ErrIncomplete
			if c.want == sofab.ErrIncomplete {
				other = sofab.ErrInvalidMsg
			}
			if errors.Is(err, other) {
				t.Fatalf("AcceptBytes = %v, must not also match %v", err, other)
			}
		})
	}
}

// TestDeepNestingEncoderWritesNoBytes pins the byte-level half of the encoder's
// MAX_DEPTH guarantee (§4.9): the rejected MaxDepth+1 open must return ErrArgument
// AND emit nothing, so a flushed stream never nests deeper than MaxDepth.
// TestDeepNestingRejected already covers the ErrArgument sentinel and both decode
// paths; this adds the "writes no bytes" assertion it cannot make against
// io.Discard, by opening the 256th sequence over a real buffer.
//
// A scalar is written at the bottom of the nest before the assertion: sequence
// headers are held back until a sequence gets content (MESSAGE_SPEC §2), so
// without it the 255 opens would emit nothing and "no growth" would be trivially
// true. The write commits all 255 headers, giving the assertion real bytes to
// compare against.
func TestDeepNestingEncoderWritesNoBytes(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	for i := 0; i < sofab.MaxDepth; i++ {
		if err := e.WriteSequenceBeginLazy(0); err != nil {
			t.Fatalf("begin %d = %v", i, err)
		}
	}
	if err := e.WriteUnsigned(1, 7); err != nil {
		t.Fatalf("write at depth %d = %v", sofab.MaxDepth, err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush = %v", err)
	}
	before := buf.Len()
	if before == 0 {
		t.Fatal("expected the committed sequence headers in the buffer, got none")
	}

	// The 256th open is rejected and must not add a byte to the buffer.
	if err := e.WriteSequenceBeginLazy(0); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("256th begin = %v, want ErrArgument", err)
	}
	if err := e.Flush(); err != nil {
		// A rejected write leaves a sticky ErrArgument on the encoder, but Flush of
		// zero new bytes must not manufacture output either way.
		if !errors.Is(err, sofab.ErrArgument) {
			t.Fatalf("flush after reject = %v", err)
		}
	}
	if after := buf.Len(); after != before {
		t.Fatalf("buffer grew from %d to %d bytes on rejected open, want no growth", before, after)
	}
}

// countingSkipV declines every sub-sequence and counts the fields it is handed,
// so a test can assert both the outcome and that no phantom field was produced
// past a rejected one.
type countingSkipV struct {
	baseV
	n int
}

func (v *countingSkipV) count() error { v.n++; return nil }

func (v *countingSkipV) Unsigned(sofab.ID, uint64) error        { return v.count() }
func (v *countingSkipV) Signed(sofab.ID, int64) error           { return v.count() }
func (v *countingSkipV) Float32(sofab.ID, float32) error        { return v.count() }
func (v *countingSkipV) Float64(sofab.ID, float64) error        { return v.count() }
func (v *countingSkipV) String(sofab.ID, string) error          { return v.count() }
func (v *countingSkipV) Bytes(sofab.ID, []byte) error           { return v.count() }
func (v *countingSkipV) UnsignedArray(sofab.ID, []uint64) error { return v.count() }
func (v *countingSkipV) SignedArray(sofab.ID, []int64) error    { return v.count() }
func (v *countingSkipV) Float32Array(sofab.ID, []float32) error { return v.count() }
func (v *countingSkipV) Float64Array(sofab.ID, []float64) error { return v.count() }
func (v *countingSkipV) BeginSequence(sofab.ID) (any, error) {
	v.n++
	return nil, nil
}

// drainSkipping decodes b on one entry point with a visitor that takes nothing
// it can decline, and returns how many fields were delivered plus the outcome.
// Skipping is where a framing check is easiest to lose: a decoder that treats a
// declined field as a plain length jump stops validating the word it jumps over.
func drainSkipping(surface string, b []byte, opts ...sofab.Option) (int, error) {
	v := &countingSkipV{}
	var err error
	switch surface {
	case "AcceptBytes":
		err = acceptBytes(b, v, opts...)
	case "Feed":
		err = feedIn(b, 0, v, opts...)
	case "Feed/1-byte":
		err = feedIn(b, 1, v, opts...)
	}
	return v.n, err
}

// TestSkipFixlenArrayHeaderChecked covers issue #75: skipping a fixlen array must
// apply the same header checks as reading one. skipValue used to read the element
// count as a raw varint and discard int(n*size) bytes, so a count past arrayMax
// (§6.2) was accepted and the product wrapped mod 2^64 — 2^61 elements of 8 bytes
// discarded nothing at all, the parser resynchronised mid-message and handed the
// caller a phantom field on a COMPLETE decode, while 2^60 elements turned the
// int conversion negative and surfaced a raw "bufio: negative count" outside the
// §6.3 result set.
//
// Both skip entry points are driven (Decoder.Skip and Next's auto-skip), and every
// case is pinned against AcceptBytes: the visitor paths always validated this
// header, so the two must agree on every input (§4.8, §5.2).
func TestSkipFixlenArrayHeaderChecked(t *testing.T) {
	hdr := vhdr(0, sofab.TypeFixlenArray)
	fp32Word := vbytes((4 << 3) | subFP32)
	fp64Word := vbytes((8 << 3) | subFP64)

	// arr builds a complete fixlen-array field from a count and a fixlen word.
	arr := func(count uint64, word []byte, payload ...byte) []byte {
		out := append(append([]byte{}, hdr...), vbytes(count)...)
		out = append(out, word...)
		return append(out, payload...)
	}

	cases := []struct {
		name string
		in   []byte
		want error // nil = clean decode to end of stream
	}{
		{
			// The isolate: count 2^61 × 8 bytes wraps to a zero-byte discard, so the
			// stream resynchronised on the payload and produced a second field.
			name: "count wraps the payload size to zero",
			in:   append(arr(1<<61, fp64Word), 0x00, 0x7F),
			want: sofab.ErrInvalidMsg,
		},
		{
			// count 2^60 × 8 = 2^63: int() goes negative and bufio.Discard panicked
			// its way out of the §6.3 taxonomy.
			name: "count makes the payload size negative",
			in:   append(arr(1<<60, fp64Word), 0x00),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "count one past arrayMax",
			in:   arr(overArrayMax, fp32Word),
			want: sofab.ErrInvalidMsg,
		},
		{
			// §4.8 admits only fp32/4 and fp64/8 as fixlen-array elements; a string
			// subtype is malformed regardless of the schema, exactly as the visitor
			// paths already ruled.
			name: "fixlen word carries a string subtype",
			in:   arr(1, vbytes((4<<3)|subStr), 'a', 'b', 'c', 'd'),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "fixlen word width contradicts its subtype",
			in:   arr(1, vbytes((8<<3)|subFP32), 0, 0, 0, 0, 0, 0, 0, 0),
			want: sofab.ErrInvalidMsg,
		},
		{
			// Truncation is still truncation: a well-formed header whose payload runs
			// out is INCOMPLETE, not INVALID.
			name: "payload truncated",
			in:   arr(2, fp64Word, 0, 0, 0, 0),
			want: sofab.ErrIncomplete,
		},
		{
			// Control: a legal array is skipped by exactly its payload length, so the
			// field that follows it is read at the right offset.
			name: "well-formed array skips its payload exactly",
			in: append(arr(2, fp32Word, 0, 0, 0, 0, 0, 0, 0, 0),
				append(vhdr(3, sofab.TypeVarintUnsigned), 0x7F)...),
			want: nil,
		},
		{
			// Control: an empty array still carries its fixlen word (§4.8) and no
			// payload.
			name: "empty array",
			in: append(arr(0, fp64Word),
				append(vhdr(3, sofab.TypeVarintUnsigned), 0x7F)...),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, surface := range surfaces {
				t.Run(surface, func(t *testing.T) {
					seen, err := drainSkipping(surface, c.in)
					if c.want == nil {
						if err != nil {
							t.Fatalf("%s = %v, want clean decode", surface, err)
						}
						if seen != 2 {
							t.Fatalf("%s saw %d fields, want the array then the id-3 unsigned field", surface, seen)
						}
						return
					}
					if !errors.Is(err, c.want) {
						t.Fatalf("%s = %v, want %v", surface, err, c.want)
					}
					// INVALID and INCOMPLETE are distinct outcomes (§5.2); guard the
					// one this case is not, so a regression that swaps them is caught.
					other := sofab.ErrIncomplete
					if c.want == sofab.ErrIncomplete {
						other = sofab.ErrInvalidMsg
					}
					if errors.Is(err, other) {
						t.Fatalf("%s = %v, must not also match %v", surface, err, other)
					}
					// A rejected array never yields a field past itself: the phantom
					// second field was the visible half of the wrap.
					if seen > 1 {
						t.Fatalf("%s delivered %d fields on a rejected array", surface, seen)
					}
				})
			}

			// The visitor path is the reference: it always checked this header.
			verr := acceptBytes(c.in, baseV{})
			if c.want == nil {
				if verr != nil {
					t.Fatalf("AcceptBytes = %v, want clean decode", verr)
				}
				return
			}
			if !errors.Is(verr, c.want) {
				t.Fatalf("AcceptBytes = %v, want %v (pull and visitor must agree)", verr, c.want)
			}
		})
	}
}

// TestSkipIntegerArrayCountChecked covers issue #77: skipping an integer array
// must hold its element count to the same ceiling as reading one. skipValue used
// to read the count of a varint array as a bare varint and then walk elements
// until the bytes ran out, so a count past arrayMax (§6.2) on a truncated array
// surfaced as ErrIncomplete — but the ceilings are properties of the wire format
// itself, so exceeding one is INVALID and INVALID dominates INCOMPLETE (§5.2). A
// receiver that treats INCOMPLETE as "wait for more bytes" would wait forever on
// a message that can never become valid.
//
// Both skip entry points are driven (Decoder.Skip and Next's auto-skip), and every
// case is pinned against AcceptBytes and Feed: the visitor paths always
// ran this count through arrayCount, so all three must agree on every input.
func TestSkipIntegerArrayCountChecked(t *testing.T) {
	// arr builds a complete integer-array field from a wire type, count and
	// element bytes.
	arr := func(wt sofab.WireType, count uint64, payload ...byte) []byte {
		out := append(append([]byte{}, vhdr(0, wt)...), vbytes(count)...)
		return append(out, payload...)
	}

	// tail is a well-formed field after the one under test: on a rejected field it
	// must never be handed out.
	tail := append(vhdr(3, sofab.TypeVarintUnsigned), 0x7F)

	cases := []struct {
		name string
		in   []byte
		want error // nil = clean decode to end of stream
	}{
		{
			// The isolate: count 2^32 with four one-byte elements behind it. The
			// element walk hit the end of the stream and reported truncation, hiding
			// a count the format can never admit.
			name: "unsigned count past arrayMax with a truncated payload",
			in:   arr(sofab.TypeVarintArrayUnsigned, 1<<32, 0x01, 0x01, 0x01, 0x01),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "signed count past arrayMax with a truncated payload",
			in:   arr(sofab.TypeVarintArraySigned, 1<<32, 0x01, 0x01, 0x01, 0x01),
			want: sofab.ErrInvalidMsg,
		},
		{
			// The boundary: one past arrayMax, nothing behind it.
			name: "unsigned count one past arrayMax",
			in:   arr(sofab.TypeVarintArrayUnsigned, overArrayMax),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "signed count one past arrayMax",
			in:   arr(sofab.TypeVarintArraySigned, overArrayMax),
			want: sofab.ErrInvalidMsg,
		},
		{
			// Truncation is still truncation: a count inside the ceiling whose
			// elements run out is INCOMPLETE, not INVALID.
			name: "payload truncated under the ceiling",
			in:   arr(sofab.TypeVarintArrayUnsigned, 4, 0x01, 0x01),
			want: sofab.ErrIncomplete,
		},
		{
			// Control: a legal array is skipped by exactly its elements, so the field
			// that follows it is read at the right offset.
			name: "well-formed array skips its elements exactly",
			in:   append(arr(sofab.TypeVarintArrayUnsigned, 3, 0x01, 0x80, 0x02, 0x7F), tail...),
			want: nil,
		},
		{
			name: "empty array",
			in:   append(arr(sofab.TypeVarintArraySigned, 0), tail...),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, surface := range surfaces {
				t.Run(surface, func(t *testing.T) {
					seen, err := drainSkipping(surface, c.in)
					if c.want == nil {
						if err != nil {
							t.Fatalf("%s = %v, want clean decode", surface, err)
						}
						if seen != 2 {
							t.Fatalf("%s saw %d fields, want the array then the id-3 unsigned field", surface, seen)
						}
						return
					}
					if !errors.Is(err, c.want) {
						t.Fatalf("%s = %v, want %v", surface, err, c.want)
					}
					// INVALID and INCOMPLETE are distinct outcomes (§5.2); guard the
					// one this case is not, so a regression that swaps them is caught.
					other := sofab.ErrIncomplete
					if c.want == sofab.ErrIncomplete {
						other = sofab.ErrInvalidMsg
					}
					if errors.Is(err, other) {
						t.Fatalf("%s = %v, must not also match %v", surface, err, other)
					}
					if seen > 1 {
						t.Fatalf("%s delivered %d fields on a rejected array", surface, seen)
					}
				})
			}

			// The visitor paths are the reference: both always checked this count.
			verr := acceptBytes(c.in, baseV{})
			serr := feedIn(c.in, 1, baseV{})
			if c.want == nil {
				if verr != nil {
					t.Fatalf("AcceptBytes = %v, want clean decode", verr)
				}
				if serr != nil {
					t.Fatalf("Feed/1-byte = %v, want clean decode", serr)
				}
				return
			}
			if !errors.Is(verr, c.want) {
				t.Fatalf("AcceptBytes = %v, want %v (pull and visitor must agree)", verr, c.want)
			}
			if !errors.Is(serr, c.want) {
				t.Fatalf("Feed = %v, want %v (pull and visitor must agree)", serr, c.want)
			}
		})
	}
}

// TestSkipIntegerArrayHonoursArrayCountLimit pins the receiver-side half of the
// same count word (§6.2.1): a skipped integer array goes through the configured
// WithMaxArrayCount cap, reported as ErrLimitExceeded and never conflated with
// ErrInvalidMsg — matching what the visitor path already did with the identical
// bytes and options. The fixlen-array twin is
// TestSkipFixlenArrayHonoursArrayCountLimit.
func TestSkipIntegerArrayHonoursArrayCountLimit(t *testing.T) {
	in := append(append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(2)...), 0x01, 0x02)

	// Under a cap of 1 the two-element array is refused on every surface.
	for _, surface := range surfaces {
		if _, err := drainSkipping(surface, in, sofab.WithMaxArrayCount(1)); !errors.Is(err, sofab.ErrLimitExceeded) {
			t.Fatalf("%s = %v, want ErrLimitExceeded", surface, err)
		} else if errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s = %v, must not also match ErrInvalidMsg", surface, err)
		}
		// At the cap it decodes cleanly.
		if _, err := drainSkipping(surface, in, sofab.WithMaxArrayCount(2)); err != nil {
			t.Fatalf("%s at the cap = %v, want clean decode", surface, err)
		}
	}
}

// TestSkipFixlenHeaderChecked covers issue #76: skipping a plain fixlen field
// must apply the same framing checks to its length-and-subtype word as reading
// one does. §4.6 defines subtypes 0x0-0x3 only — 0x4-0x7 are reserved — and
// fp32 carries exactly 4 payload bytes, fp64 exactly 8; §6.3 names both "a
// reserved fixlen subtype" and "a wrong-width fp32/fp64 fixlen" as
// InvalidMessage. These are framing checks, not schema checks, so they hold
// whether or not the field is materialised: skipValue used to discard the
// subtype and treat any word as a plain length jump, so `for { Next(); Skip() }`
// reported COMPLETE on bytes both visitor paths reject.
//
// Both skip entry points are driven (Decoder.Skip and Next's auto-skip), and
// every case is pinned against AcceptBytes and Feed: the visitor paths
// always validated this word, so all three must agree on every input (§5.2).
func TestSkipFixlenHeaderChecked(t *testing.T) {
	hdr := vhdr(0, sofab.TypeFixlen)

	// fix builds a complete fixlen field from a length-and-subtype word plus
	// payload bytes.
	fix := func(length, sub uint64, payload ...byte) []byte {
		out := append(append([]byte{}, hdr...), vbytes((length<<3)|sub)...)
		return append(out, payload...)
	}

	// tail is a well-formed field after the one under test: on a rejected field
	// it must never be handed out.
	tail := append(vhdr(3, sofab.TypeVarintUnsigned), 0x7F)

	cases := []struct {
		name string
		in   []byte
		want error // nil = clean decode to end of stream
	}{
		{
			// The isolate from §6.3's own example list: word (4<<3)|0x4.
			name: "reserved subtype 0x4",
			in:   fix(4, 0x4, 'A', 'B', 'C', 'D'),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "reserved subtype 0x5",
			in:   fix(1, 0x5, 'A'),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "reserved subtype 0x6",
			in:   fix(0, 0x6),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "reserved subtype 0x7",
			in:   fix(2, 0x7, 'A', 'B'),
			want: sofab.ErrInvalidMsg,
		},
		{
			// The second §6.3 example: fp32 declared 8 bytes wide.
			name: "fp32 with width 8",
			in:   fix(8, subFP32, 'A', 'B', 'C', 'D', 'A', 'B', 'C', 'D'),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "fp32 with width 0",
			in:   fix(0, subFP32),
			want: sofab.ErrInvalidMsg,
		},
		{
			name: "fp64 with width 4",
			in:   fix(4, subFP64, 0, 0, 0, 0),
			want: sofab.ErrInvalidMsg,
		},
		{
			// Truncation is still truncation: a well-formed word whose payload runs
			// out is INCOMPLETE, not INVALID.
			name: "string payload truncated",
			in:   fix(4, subStr, 'h', 'i'),
			want: sofab.ErrIncomplete,
		},
		{
			// Control: the exact-width floats skip by exactly their payload, so the
			// field that follows is read at the right offset.
			name: "fp32 skips its payload exactly",
			in:   append(fix(4, subFP32, 0, 0, 0, 0), tail...),
			want: nil,
		},
		{
			name: "fp64 skips its payload exactly",
			in:   append(fix(8, subFP64, 0, 0, 0, 0, 0, 0, 0, 0), tail...),
			want: nil,
		},
		{
			// Controls: strings and blobs take any length, including zero.
			name: "string of arbitrary length",
			in:   append(fix(3, subStr, 'a', 'b', 'c'), tail...),
			want: nil,
		},
		{
			name: "empty blob",
			in:   append(fix(0, subBlob), tail...),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, surface := range surfaces {
				t.Run(surface, func(t *testing.T) {
					seen, err := drainSkipping(surface, c.in)
					if c.want == nil {
						if err != nil {
							t.Fatalf("%s = %v, want clean decode", surface, err)
						}
						if seen != 2 {
							t.Fatalf("%s saw %d fields, want the fixlen field then the id-3 unsigned field", surface, seen)
						}
						return
					}
					if !errors.Is(err, c.want) {
						t.Fatalf("%s = %v, want %v", surface, err, c.want)
					}
					// INVALID and INCOMPLETE are distinct outcomes (§5.2); guard the
					// one this case is not, so a regression that swaps them is caught.
					other := sofab.ErrIncomplete
					if c.want == sofab.ErrIncomplete {
						other = sofab.ErrInvalidMsg
					}
					if errors.Is(err, other) {
						t.Fatalf("%s = %v, must not also match %v", surface, err, other)
					}
					// A rejected field never yields the field past itself: resyncing on
					// an unvalidated length was the visible half of the defect.
					if seen > 1 {
						t.Fatalf("%s delivered %d fields on a rejected field", surface, seen)
					}
				})
			}

			// The visitor paths are the reference: both always validated this word.
			verr := acceptBytes(c.in, baseV{})
			serr := feedIn(c.in, 1, baseV{})
			if c.want == nil {
				if verr != nil {
					t.Fatalf("AcceptBytes = %v, want clean decode", verr)
				}
				if serr != nil {
					t.Fatalf("Feed/1-byte = %v, want clean decode", serr)
				}
				return
			}
			if !errors.Is(verr, c.want) {
				t.Fatalf("AcceptBytes = %v, want %v (pull and visitor must agree)", verr, c.want)
			}
			if !errors.Is(serr, c.want) {
				t.Fatalf("Feed = %v, want %v (pull and visitor must agree)", serr, c.want)
			}
		})
	}
}

// TestSkipFixlenArrayHonoursArrayCountLimit pins the receiver-side half of the
// same header (§6.2.1): the count word of a skipped fixlen array goes through the
// configured WithMaxArrayCount cap, reported as ErrLimitExceeded and never
// conflated with ErrInvalidMsg — matching what the visitor path already did with
// the identical bytes and options.
func TestSkipFixlenArrayHonoursArrayCountLimit(t *testing.T) {
	in := append(append(vhdr(0, sofab.TypeFixlenArray), vbytes(2)...),
		append(vbytes((4<<3)|subFP32), 0, 0, 0, 0, 0, 0, 0, 0)...)

	// Under a cap of 1 the two-element array is refused on every surface.
	for _, surface := range surfaces {
		if _, err := drainSkipping(surface, in, sofab.WithMaxArrayCount(1)); !errors.Is(err, sofab.ErrLimitExceeded) {
			t.Fatalf("%s = %v, want ErrLimitExceeded", surface, err)
		} else if errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s = %v, must not also match ErrInvalidMsg", surface, err)
		}
		// At the cap it decodes cleanly.
		if _, err := drainSkipping(surface, in, sofab.WithMaxArrayCount(2)); err != nil {
			t.Fatalf("%s at the cap = %v, want clean decode", surface, err)
		}
	}
}

// TestDanglingSequenceEnd is the §5.2.2 row "a sequence-end marker with no open
// sequence". It must be INVALID however the message is walked — including where
// the visitor takes nothing, so the walk is a pure skip.
func TestDanglingSequenceEnd(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"lone end", []byte{0x07}},
		{"end after a balanced sequence", []byte{0x06, 0x07, 0x07}},
		{"end after a scalar", []byte{0x00, 0x7F, 0x07}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, surface := range surfaces {
				if _, err := drainSkipping(surface, tc.in); !errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("%s = %v, want ErrInvalidMsg", surface, err)
				}
			}
		})
	}
}
