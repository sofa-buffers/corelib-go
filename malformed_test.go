package sofab_test

import (
	"bytes"
	"errors"
	"io"
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
// The count check lives in arrayCount (decoder.go / cursor.go), so the pull cases
// below drive the typed readers, matching the visitor path's cursor.arrayCount.
// Every arm of skipValue routes its count through arrayCount as well, so a skipped
// array is held to the same ceiling as a read one: the fixlen-array arm by issue
// #75 (TestSkipFixlenArrayHeaderChecked), the integer-array arms by issue #77
// (TestSkipIntegerArrayCountChecked).
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

// drainPull drives the pull parser over b to the end of the stream, consuming
// every field's value, and returns the field headers it handed out plus the
// terminating error (nil for a clean EOF). autoSkip picks which of the two skip
// entry points is exercised: Decoder.Skip explicitly, or Next's auto-skip of an
// unconsumed value — both land in skipValue, and issue #75 was reachable from
// either.
func drainPull(b []byte, autoSkip bool, opts ...sofab.Option) ([]sofab.Field, error) {
	d := sofab.NewDecoder(bytes.NewReader(b), opts...)
	var seen []sofab.Field
	for {
		f, err := d.Next()
		if err == io.EOF {
			return seen, nil
		}
		if err != nil {
			return seen, err
		}
		seen = append(seen, f)
		if autoSkip {
			continue // let the next Next auto-skip the unconsumed value
		}
		if err := d.Skip(); err != nil {
			return seen, err
		}
	}
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
			for _, mode := range []struct {
				name     string
				autoSkip bool
			}{{"Skip", false}, {"NextAutoSkip", true}} {
				t.Run(mode.name, func(t *testing.T) {
					seen, err := drainPull(c.in, mode.autoSkip)
					if c.want == nil {
						if err != nil {
							t.Fatalf("pull = %v, want clean decode", err)
						}
						if len(seen) != 2 || seen[1].ID != 3 || seen[1].Type != sofab.TypeVarintUnsigned {
							t.Fatalf("pull saw %+v, want the array then the id-3 unsigned field", seen)
						}
						return
					}
					if !errors.Is(err, c.want) {
						t.Fatalf("pull = %v, want %v", err, c.want)
					}
					// INVALID and INCOMPLETE are distinct outcomes (§5.2); guard the
					// one this case is not, so a regression that swaps them is caught.
					other := sofab.ErrIncomplete
					if c.want == sofab.ErrIncomplete {
						other = sofab.ErrInvalidMsg
					}
					if errors.Is(err, other) {
						t.Fatalf("pull = %v, must not also match %v", err, other)
					}
					// A rejected array never yields a field past itself: the phantom
					// second field was the visible half of the wrap.
					if len(seen) != 1 {
						t.Fatalf("pull handed out %d fields on a rejected array: %+v", len(seen), seen)
					}
				})
			}

			// The visitor path is the reference: it always checked this header.
			verr := sofab.AcceptBytes(c.in, baseV{})
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
// case is pinned against AcceptBytes and AcceptStream: the visitor paths always
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
			for _, mode := range []struct {
				name     string
				autoSkip bool
			}{{"Skip", false}, {"NextAutoSkip", true}} {
				t.Run(mode.name, func(t *testing.T) {
					seen, err := drainPull(c.in, mode.autoSkip)
					if c.want == nil {
						if err != nil {
							t.Fatalf("pull = %v, want clean decode", err)
						}
						if len(seen) != 2 || seen[1].ID != 3 || seen[1].Type != sofab.TypeVarintUnsigned {
							t.Fatalf("pull saw %+v, want the array then the id-3 unsigned field", seen)
						}
						return
					}
					if !errors.Is(err, c.want) {
						t.Fatalf("pull = %v, want %v", err, c.want)
					}
					// INVALID and INCOMPLETE are distinct outcomes (§5.2); guard the
					// one this case is not, so a regression that swaps them is caught.
					other := sofab.ErrIncomplete
					if c.want == sofab.ErrIncomplete {
						other = sofab.ErrInvalidMsg
					}
					if errors.Is(err, other) {
						t.Fatalf("pull = %v, must not also match %v", err, other)
					}
					if len(seen) != 1 {
						t.Fatalf("pull handed out %d fields on a rejected array: %+v", len(seen), seen)
					}
				})
			}

			// The visitor paths are the reference: both always checked this count.
			verr := sofab.AcceptBytes(c.in, baseV{})
			serr := sofab.NewDecoder(bytes.NewReader(c.in)).AcceptStream(baseV{})
			if c.want == nil {
				if verr != nil {
					t.Fatalf("AcceptBytes = %v, want clean decode", verr)
				}
				if serr != nil {
					t.Fatalf("AcceptStream = %v, want clean decode", serr)
				}
				return
			}
			if !errors.Is(verr, c.want) {
				t.Fatalf("AcceptBytes = %v, want %v (pull and visitor must agree)", verr, c.want)
			}
			if !errors.Is(serr, c.want) {
				t.Fatalf("AcceptStream = %v, want %v (pull and visitor must agree)", serr, c.want)
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

	// Under a cap of 1 the two-element array is refused on both paths.
	if _, err := drainPull(in, false, sofab.WithMaxArrayCount(1)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("pull Skip = %v, want ErrLimitExceeded", err)
	} else if errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull Skip = %v, must not also match ErrInvalidMsg", err)
	}
	if err := sofab.AcceptBytes(in, baseV{}, sofab.WithMaxArrayCount(1)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("AcceptBytes = %v, want ErrLimitExceeded", err)
	}

	// At the cap it decodes cleanly on both.
	if _, err := drainPull(in, false, sofab.WithMaxArrayCount(2)); err != nil {
		t.Fatalf("pull Skip at the cap = %v, want clean decode", err)
	}
	if err := sofab.AcceptBytes(in, baseV{}, sofab.WithMaxArrayCount(2)); err != nil {
		t.Fatalf("AcceptBytes = %v, want clean decode", err)
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
// every case is pinned against AcceptBytes and AcceptStream: the visitor paths
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
			for _, mode := range []struct {
				name     string
				autoSkip bool
			}{{"Skip", false}, {"NextAutoSkip", true}} {
				t.Run(mode.name, func(t *testing.T) {
					seen, err := drainPull(c.in, mode.autoSkip)
					if c.want == nil {
						if err != nil {
							t.Fatalf("pull = %v, want clean decode", err)
						}
						if len(seen) != 2 || seen[1].ID != 3 || seen[1].Type != sofab.TypeVarintUnsigned {
							t.Fatalf("pull saw %+v, want the fixlen field then the id-3 unsigned field", seen)
						}
						return
					}
					if !errors.Is(err, c.want) {
						t.Fatalf("pull = %v, want %v", err, c.want)
					}
					// INVALID and INCOMPLETE are distinct outcomes (§5.2); guard the
					// one this case is not, so a regression that swaps them is caught.
					other := sofab.ErrIncomplete
					if c.want == sofab.ErrIncomplete {
						other = sofab.ErrInvalidMsg
					}
					if errors.Is(err, other) {
						t.Fatalf("pull = %v, must not also match %v", err, other)
					}
					// A rejected field never yields the field past itself: resyncing on
					// an unvalidated length was the visible half of the defect.
					if len(seen) != 1 {
						t.Fatalf("pull handed out %d fields on a rejected field: %+v", len(seen), seen)
					}
				})
			}

			// The visitor paths are the reference: both always validated this word.
			verr := sofab.AcceptBytes(c.in, baseV{})
			serr := sofab.NewDecoder(bytes.NewReader(c.in)).AcceptStream(baseV{})
			if c.want == nil {
				if verr != nil {
					t.Fatalf("AcceptBytes = %v, want clean decode", verr)
				}
				if serr != nil {
					t.Fatalf("AcceptStream = %v, want clean decode", serr)
				}
				return
			}
			if !errors.Is(verr, c.want) {
				t.Fatalf("AcceptBytes = %v, want %v (pull and visitor must agree)", verr, c.want)
			}
			if !errors.Is(serr, c.want) {
				t.Fatalf("AcceptStream = %v, want %v (pull and visitor must agree)", serr, c.want)
			}
		})
	}
}

// TestFixlenReservedSubtypeTypedReaders pins the typed pull readers on the same
// words: a reserved subtype or a wrong-width float is INVALID there too, and is
// never confused with the ErrTypeMismatch skip a caller gets when the declared
// type simply disagrees with a well-formed field (§7.3).
func TestFixlenReservedSubtypeTypedReaders(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		read func(*sofab.Decoder) error
	}{
		{
			name: "String on a reserved subtype",
			in:   append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|0x4), 'A', 'B', 'C', 'D')...),
			read: func(d *sofab.Decoder) error { _, err := d.String(); return err },
		},
		{
			name: "Bytes on a reserved subtype",
			in:   append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|0x5), 'A', 'B', 'C', 'D')...),
			read: func(d *sofab.Decoder) error { _, err := d.Bytes(); return err },
		},
		{
			name: "Float32 on a width-8 fp32",
			in:   append(vhdr(0, sofab.TypeFixlen), append(vbytes((8<<3)|subFP32), 0, 0, 0, 0, 0, 0, 0, 0)...),
			read: func(d *sofab.Decoder) error { _, err := d.Float32(); return err },
		},
		{
			name: "Float64 on a width-4 fp64",
			in:   append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subFP64), 0, 0, 0, 0)...),
			read: func(d *sofab.Decoder) error { _, err := d.Float64(); return err },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDec(c.in)
			if _, err := d.Next(); err != nil {
				t.Fatalf("Next = %v", err)
			}
			err := c.read(d)
			if !errors.Is(err, sofab.ErrInvalidMsg) {
				t.Fatalf("read = %v, want ErrInvalidMsg", err)
			}
			if errors.Is(err, sofab.ErrTypeMismatch) || errors.Is(err, sofab.ErrIncomplete) {
				t.Fatalf("read = %v, must not match ErrTypeMismatch/ErrIncomplete", err)
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

	// Under a cap of 1 the two-element array is refused on both paths.
	if _, err := drainPull(in, false, sofab.WithMaxArrayCount(1)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("pull Skip = %v, want ErrLimitExceeded", err)
	} else if errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("pull Skip = %v, must not also match ErrInvalidMsg", err)
	}
	if err := sofab.AcceptBytes(in, baseV{}, sofab.WithMaxArrayCount(1)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("AcceptBytes = %v, want ErrLimitExceeded", err)
	}

	// At the cap it decodes cleanly on both.
	if _, err := drainPull(in, false, sofab.WithMaxArrayCount(2)); err != nil {
		t.Fatalf("pull Skip at the cap = %v, want clean decode", err)
	}
	if err := sofab.AcceptBytes(in, baseV{}, sofab.WithMaxArrayCount(2)); err != nil {
		t.Fatalf("AcceptBytes at the cap = %v, want clean decode", err)
	}
}

// TestPullNextEnforcesMaxDepth covers issue #78: MAX_DEPTH = 255 (§4.9/§6.2) is a
// property of the wire format, so it binds the pull surface exactly as it binds
// the two visitor kernels. Decoder carried no depth state at all: Next handed
// back sequence-start headers for as long as the stream offered them, so a plain
// pull loop walked an arbitrarily deep message to COMPLETE while AcceptBytes on
// the same bytes returned ErrInvalidMsg — the two surfaces disagreed on whether
// the message was well-formed. A generated decoder driving Next therefore had no
// ceiling on the nesting an attacker could impose on its own scope stack.
func TestPullNextEnforcesMaxDepth(t *testing.T) {
	// 0x06 = sequence start id 0, 0x07 = sequence end id 0.
	deep := append(bytes.Repeat([]byte{0x06}, 1000), bytes.Repeat([]byte{0x07}, 1000)...)

	// The visitor path is the reference outcome for these bytes.
	if err := sofab.AcceptBytes(deep, baseV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("AcceptBytes deep = %v, want ErrInvalidMsg", err)
	}

	// A pull loop that only calls Next must reach the same verdict, and must do
	// so exactly at the 256th open: MaxDepth nested sequences are legal.
	d := newDec(deep)
	for i := 0; i < sofab.MaxDepth; i++ {
		f, err := d.Next()
		if err != nil {
			t.Fatalf("Next at depth %d = %v, want a sequence start", i+1, err)
		}
		if f.Type != sofab.TypeSequenceStart {
			t.Fatalf("Next at depth %d = type %v, want TypeSequenceStart", i+1, f.Type)
		}
	}
	if _, err := d.Next(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("Next opening depth %d = %v, want ErrInvalidMsg", sofab.MaxDepth+1, err)
	}

	// And the whole-stream drain agrees, on both the Skip and the auto-skip leg.
	for _, autoSkip := range []bool{false, true} {
		if _, err := drainPull(deep, autoSkip); !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("drainPull(autoSkip=%v) = %v, want ErrInvalidMsg", autoSkip, err)
		}
	}
}

// TestPullSkipDepthIsAbsolute pins the second half of issue #78: Skip's own
// counter was relative to the scope the skip started in, so nesting already
// established by earlier Next calls was invisible to it. Descending 200 scopes by
// hand and then skipping a 200-deep sub-tree reached a total wire depth of 400
// and still returned nil. The depth ceiling belongs to the decoder, not to one
// call, so Skip now inherits it from Next.
func TestPullSkipDepthIsAbsolute(t *testing.T) {
	const half = 200
	in := append(bytes.Repeat([]byte{0x06}, 2*half), bytes.Repeat([]byte{0x07}, 2*half)...)

	d := newDec(in)
	for i := 0; i < half; i++ {
		if _, err := d.Next(); err != nil {
			t.Fatalf("Next at depth %d = %v", i+1, err)
		}
	}
	// The scope opened here sits at wire depth half+1; skipping it walks another
	// half-1 opens, i.e. past MaxDepth in absolute terms.
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next at depth %d = %v", half+1, err)
	}
	if err := d.Skip(); !errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatalf("Skip of a sub-tree crossing MaxDepth = %v, want ErrInvalidMsg", err)
	}
}

// TestPullDanglingSequenceEnd covers the other §6.3 InvalidMessage case issue #78
// found open on the pull surface: "a sequence-end marker with no open sequence".
// Both visitor kernels reject it; Next used to return it as an ordinary header,
// so a pull loop reported COMPLETE on bytes the same library called malformed.
func TestPullDanglingSequenceEnd(t *testing.T) {
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
			if err := sofab.AcceptBytes(tc.in, baseV{}); !errors.Is(err, sofab.ErrInvalidMsg) {
				t.Fatalf("AcceptBytes = %v, want ErrInvalidMsg", err)
			}
			for _, autoSkip := range []bool{false, true} {
				if _, err := drainPull(tc.in, autoSkip); !errors.Is(err, sofab.ErrInvalidMsg) {
					t.Fatalf("drainPull(autoSkip=%v) = %v, want ErrInvalidMsg", autoSkip, err)
				}
			}
		})
	}
}
