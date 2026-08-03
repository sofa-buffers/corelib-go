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
// The count check lives in arrayCount (decoder.go / cursor.go) and is reached via
// the typed array readers, not pull Skip — skipValue reads the count as a raw
// varint, so the pull cases below drive the typed readers, matching the visitor
// path's cursor.arrayCount.
func TestOversizedCountLengthInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		// pull reads the current field with the reader that exercises the range
		// check (readFixlenHeader for fixlen, arrayCount for arrays).
		pull func(*sofab.Decoder) error
	}{
		{
			name: "fixlen blob length",
			in:   append(vhdr(0, sofab.TypeFixlen), vbytes((overArrayMax<<3)|subBlob)...),
			pull: func(d *sofab.Decoder) error { _, err := d.Bytes(); return err },
		},
		{
			name: "fixlen string length",
			in:   append(vhdr(0, sofab.TypeFixlen), vbytes((overArrayMax<<3)|subStr)...),
			pull: func(d *sofab.Decoder) error { _, err := d.String(); return err },
		},
		{
			name: "unsigned array count",
			in:   append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(overArrayMax)...),
			pull: func(d *sofab.Decoder) error { _, err := sofab.ReadUnsignedArray[uint64](d); return err },
		},
		{
			name: "signed array count",
			in:   append(vhdr(0, sofab.TypeVarintArraySigned), vbytes(overArrayMax)...),
			pull: func(d *sofab.Decoder) error { _, err := sofab.ReadSignedArray[int64](d); return err },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Visitor path (what generated Unmarshal uses).
			verr := sofab.AcceptBytes(c.in, baseV{})
			if !errors.Is(verr, sofab.ErrInvalidMsg) {
				t.Fatalf("AcceptBytes = %v, want ErrInvalidMsg", verr)
			}
			// A wire-format violation, not truncation nor a receiver-side limit.
			if errors.Is(verr, sofab.ErrIncomplete) || errors.Is(verr, sofab.ErrLimitExceeded) {
				t.Fatalf("AcceptBytes = %v, must not match ErrIncomplete/ErrLimitExceeded", verr)
			}

			// Pull path: drive Next then the matching typed reader.
			d := newDec(c.in)
			if _, err := d.Next(); err != nil {
				t.Fatalf("pull Next = %v", err)
			}
			perr := c.pull(d)
			if !errors.Is(perr, sofab.ErrInvalidMsg) {
				t.Fatalf("pull = %v, want ErrInvalidMsg", perr)
			}
			if errors.Is(perr, sofab.ErrIncomplete) || errors.Is(perr, sofab.ErrLimitExceeded) {
				t.Fatalf("pull = %v, must not match ErrIncomplete/ErrLimitExceeded", perr)
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
			err := sofab.AcceptBytes(c.in, baseV{})
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
