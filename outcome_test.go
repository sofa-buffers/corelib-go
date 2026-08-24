package sofab_test

import (
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// TestDecodeOutcome locks the three-valued, finish-less decode outcome
// (MESSAGE_SPEC §7): COMPLETE (nil), INCOMPLETE (ErrIncomplete), INVALID
// (ErrInvalidMsg) — reported identically on the visitor and pull paths. A lone
// dangling 0x80 (continuation bit set, no terminating byte) is the canonical
// INCOMPLETE case: it must be neither accepted as complete nor rejected as
// malformed. ErrIncomplete and ErrInvalidMsg are distinct sentinels, so a
// truncated stream is never conflated with a malformed one.
func TestDecodeOutcome(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error // nil = COMPLETE
	}{
		// COMPLETE — ends exactly at a field boundary.
		{name: "empty message", in: []byte{}, want: nil},
		{name: "one unsigned field", in: append(vhdr(0, sofab.TypeVarintUnsigned), 0x07), want: nil},

		// INCOMPLETE — ends inside a field (§7). Not an error.
		{name: "lone dangling 0x80", in: []byte{0x80}, want: sofab.ErrIncomplete},
		{name: "unterminated value varint", in: append(vhdr(0, sofab.TypeVarintUnsigned), 0x80), want: sofab.ErrIncomplete},
		{name: "short fixlen payload", in: append(vhdr(0, sofab.TypeFixlen), append(vbytes((4<<3)|subStr), 'h', 'i')...), want: sofab.ErrIncomplete},
		{name: "unclosed sequence", in: vhdr(0, sofab.TypeSequenceStart), want: sofab.ErrIncomplete},

		// INVALID — malformed regardless of what follows.
		{name: "varint over 64 bits", in: append(vhdr(0, sofab.TypeVarintUnsigned),
			0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80), want: sofab.ErrInvalidMsg},
		{name: "dangling sequence end", in: vhdr(0, sofab.TypeSequenceEnd), want: sofab.ErrInvalidMsg},
	}

	check := func(t *testing.T, path string, err, want error) {
		t.Helper()
		if want == nil {
			if err != nil {
				t.Fatalf("%s = %v, want COMPLETE (nil)", path, err)
			}
			return
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s = %v, want %v", path, err, want)
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The visitor is the only decode surface (§5.3.1), so the outcome is
			// checked on all three of its entry points: they must agree byte for
			// byte, which is the divergence class §5.3.1 exists to prevent.
			check(t, "AcceptBytes", sofab.AcceptBytes(c.in, baseV{}), c.want)
			check(t, "Accept", newDec(c.in).Accept(baseV{}), c.want)
			check(t, "AcceptStream", newDec(c.in).AcceptStream(baseV{}), c.want)
		})
	}
}
