package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// boundedVisitor is a generated-style visitor that enforces schema bounds at the
// header via the optional HeaderVisitor extension: array/fixlen field 15 has an
// element-count / maxlen bound of 4. It embeds baseV for the value callbacks it
// does not care about, exactly as generated code would.
type boundedVisitor struct{ baseV }

func (boundedVisitor) ArrayBegin(id sofab.ID, count int) error {
	if id == 15 && count > 4 {
		return sofab.ErrInvalidMsg
	}
	return nil
}

func (boundedVisitor) FixlenHeader(id sofab.ID, subtype, length int) error {
	if id == 15 && (subtype == subStr || subtype == subBlob) && length > 4 {
		return sofab.ErrInvalidMsg
	}
	return nil
}

// plainVisitor implements only Visitor (no HeaderVisitor), so it must behave
// exactly as before the header hooks existed — a truncated over-count is
// INCOMPLETE, since it never opts into a header-level bound.
type plainVisitor struct{ baseV }

// TestHeaderHookAntiFolding covers issue #53 / MESSAGE_SPEC §5.2: a field that is
// both a schema-bound violation (over-count / over-maxlen) and truncated MUST be
// INVALID, never INCOMPLETE (INVALID dominates INCOMPLETE — "anti-folding"). The
// bound is enforced at the header word by boundedVisitor before the truncation
// check, so it fires even when the payload never arrives. The control vectors
// (count/length == bound, truncated) must still be INCOMPLETE.
func TestHeaderHookAntiFolding(t *testing.T) {
	// array<u8>, id 15 → header 0x7b (matches the issue's reproduction table).
	const arrHdr = 0x7b
	// fixlen string, id 15 → header (15<<3)|0x2 = 0x7a.
	const strHdr = 0x7a

	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{
			// count 5 (>4), complete → already INVALID today; asserts the hook
			// does not regress the complete case.
			name: "over-count complete",
			in:   []byte{arrHdr, 5, 1, 2, 3, 4, 5},
			want: sofab.ErrInvalidMsg,
		},
		{
			// count 6 (>4), then EOF → the bug: was ErrIncomplete, must be INVALID.
			name: "over-count truncated",
			in:   []byte{arrHdr, 6, 1, 2},
			want: sofab.ErrInvalidMsg,
		},
		{
			// count 4 (== bound), then EOF → clean truncation, stays INCOMPLETE.
			name: "at-bound truncated",
			in:   []byte{arrHdr, 4, 1, 2},
			want: sofab.ErrIncomplete,
		},
		{
			// string len 6 (>4 maxlen), payload truncated → INVALID, not INCOMPLETE.
			// length word = (6<<3)|subStr = 0x32; only 2 of 6 payload bytes given.
			name: "over-maxlen string truncated",
			in:   []byte{strHdr, (6 << 3) | subStr, 'a', 'b'},
			want: sofab.ErrInvalidMsg,
		},
		{
			// string len 4 (== bound), payload truncated → stays INCOMPLETE.
			name: "at-maxlen string truncated",
			in:   []byte{strHdr, (4 << 3) | subStr, 'a', 'b'},
			want: sofab.ErrIncomplete,
		},
		{
			// blob len 6 (>4 maxlen), payload truncated → INVALID.
			name: "over-maxlen blob truncated",
			in:   []byte{strHdr, (6 << 3) | subBlob, 0x01, 0x02},
			want: sofab.ErrInvalidMsg,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// AcceptBytes (zero-copy cursor path).
			if err := sofab.AcceptBytes(c.in, boundedVisitor{}); !errors.Is(err, c.want) {
				t.Errorf("AcceptBytes: got %v, want %v", err, c.want)
			}
			// Accept (streaming path: slurp then cursor).
			if err := sofab.NewDecoder(bytes.NewReader(c.in)).Accept(boundedVisitor{}); !errors.Is(err, c.want) {
				t.Errorf("Accept: got %v, want %v", err, c.want)
			}
		})
	}
}

// TestHeaderHookBackwardCompat pins the additive contract: a visitor that does
// NOT implement HeaderVisitor is unaffected by the hooks. The over-count +
// truncated vector — INVALID for boundedVisitor above — stays INCOMPLETE here,
// because a visitor without a declared bound has nothing to reject at the header.
func TestHeaderHookBackwardCompat(t *testing.T) {
	in := []byte{0x7b, 6, 1, 2} // over-count (6) then EOF

	if err := sofab.AcceptBytes(in, plainVisitor{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("AcceptBytes: got %v, want ErrIncomplete", err)
	}
	if err := sofab.NewDecoder(bytes.NewReader(in)).Accept(plainVisitor{}); !errors.Is(err, sofab.ErrIncomplete) {
		t.Errorf("Accept: got %v, want ErrIncomplete", err)
	}
}
