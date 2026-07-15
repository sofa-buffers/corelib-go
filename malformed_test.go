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

// TestDeepNestingEncoderWritesNoBytes pins the byte-level half of the encoder's
// MAX_DEPTH guarantee (§4.9): the rejected MaxDepth+1 open must return ErrArgument
// AND emit nothing, so a flushed stream never nests deeper than MaxDepth.
// TestDeepNestingRejected already covers the ErrArgument sentinel and both decode
// paths; this adds the "writes no bytes" assertion it cannot make against
// io.Discard, by opening the 256th sequence over a real buffer.
func TestDeepNestingEncoderWritesNoBytes(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	for i := 0; i < sofab.MaxDepth; i++ {
		if err := e.WriteSequenceBegin(0); err != nil {
			t.Fatalf("begin %d = %v", i, err)
		}
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush = %v", err)
	}
	before := buf.Len()

	// The 256th open is rejected and must not add a byte to the buffer.
	if err := e.WriteSequenceBegin(0); !errors.Is(err, sofab.ErrArgument) {
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
