package sofab_test

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// arrayMaxCount is the largest count/length the decoder accepts before the
// arrayMax (INT32_MAX) range check — i.e. an allocation from it would be ~2 GB
// (bytes) to ~17 GB (uint64 elements). It passes the range check, so it exercises
// the eager-allocation path the hardening fixes rather than the range rejection.
const arrayMaxCount uint64 = 0x7FFF_FFFF

// bytesAllocated returns the heap bytes allocated while f runs.
func bytesAllocated(f func()) uint64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	f()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

// allocBudget is a generous ceiling: the hardened decoders allocate only KBs for
// these hostile inputs, while the pre-fix code attempted multi-GB allocations —
// so any regression trips this by three-plus orders of magnitude.
const allocBudget = 16 << 20 // 16 MiB

// TestPartA_NoEagerAllocFromWireCount is the Part A acceptance test (issue #40):
// a message that claims a ~2-billion element count / ~2 GB length but carries
// only a few payload bytes must fail fast — ErrIncomplete on the truncated
// payload — WITHOUT attempting the huge allocation the count would imply.
func TestPartA_NoEagerAllocFromWireCount(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"varint unsigned array", append(vhdr(0, sofab.TypeVarintArrayUnsigned),
			append(vbytes(arrayMaxCount), 0x00, 0x00)...)},
		{"varint signed array", append(vhdr(0, sofab.TypeVarintArraySigned),
			append(vbytes(arrayMaxCount), 0x00, 0x00)...)},
		{"fixlen blob length", append(vhdr(0, sofab.TypeFixlen),
			append(vbytes((arrayMaxCount<<3)|subBlob), 0x01, 0x02)...)},
		{"fixlen string length", append(vhdr(0, sofab.TypeFixlen),
			append(vbytes((arrayMaxCount<<3)|subStr), 'h', 'i')...)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A materializing destination on every entry point: recorder binds
			// every value, so the allocation path under test is the one exercised.
			for _, surface := range surfaces {
				var err error
				got := bytesAllocated(func() { _, err = decodeAll(t, surface, c.in) })
				if !errors.Is(err, sofab.ErrIncomplete) {
					t.Fatalf("%s = %v, want ErrIncomplete", surface, err)
				}
				if got > allocBudget {
					t.Fatalf("%s allocated %d bytes, want <= %d (eager alloc from wire count?)",
						surface, got, allocBudget)
				}
			}
		})
	}
}

// The Part B acceptance tests that lived here — TestPartB_ArrayCountLimit,
// _StringAndBlobLimit, _LimitEnforcedBeforePayload, _LimitDistinctFromInvalid —
// drove WithMaxArrayCount / WithMaxStringLen / WithMaxBlobLen, which no longer
// exist. CORELIB_PLAN §6.2.1 puts the numbers with the layer that knows the
// schema and the target — "The codec never invents a limit of its own and never
// clamps to one" — so what they asserted now belongs to the destination and is
// asserted in collectors_test.go (TestSeqReceiverCapIsPolicyNotInvalid and the
// tests around it), for the collectors, and to the generated visitor arms for a
// scalar or a native array.
//
// What is left to assert HERE is the negative: the codec applies no cap of its
// own, whatever the wire announces.

// TestCodecInventsNoLimit is the §6.2.1 negative on every entry point: a header
// announcing a size no deployment would accept — up to ARRAY_MAX itself — is the
// codec's business only as far as the format ceiling goes. The bytes are
// truncated, so the verdict is ErrIncomplete, and it must be ErrIncomplete
// rather than ErrLimitExceeded: nothing here holds a number to compare against,
// and a decoder that invented one would be deciding a policy that is not its to
// decide.
func TestCodecInventsNoLimit(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		// Array header + a count at ARRAY_MAX, no elements follow.
		{"array count", append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(arrayMaxCount)...)},
		// A string header claiming 2000 bytes, no payload follows.
		{"string length", append(vhdr(0, sofab.TypeFixlen), vbytes((2000<<3)|subStr)...)},
		// The blob twin.
		{"blob length", append(vhdr(0, sofab.TypeFixlen), vbytes((2000<<3)|subBlob)...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, surface := range surfaces {
				_, err := decodeAll(t, surface, c.in)
				if !errors.Is(err, sofab.ErrIncomplete) {
					t.Fatalf("%s = %v, want ErrIncomplete", surface, err)
				}
				if errors.Is(err, sofab.ErrLimitExceeded) {
					t.Fatalf("%s = %v: the codec must hold no cap of its own (§6.2.1)", surface, err)
				}
			}
		})
	}
}

// TestCodecDecodesAWholeOversizeField is the same rule with the payload actually
// present: a 4096-byte string and a 4096-element array are sizes a deployment
// might well refuse, and every one of them decodes here, because the refusal
// belongs to a destination that was handed a number and this one was not.
func TestCodecDecodesAWholeOversizeField(t *testing.T) {
	msgs := [][]byte{
		encodeUnsignedArray(t, 1, make([]uint64, 4096)),
		encodeString(t, 1, strings.Repeat("a", 4096)),
		encodeBlob(t, 1, bytes.Repeat([]byte{0xAB}, 4096)),
	}
	for i, msg := range msgs {
		if err := acceptBytes(msg, baseV{}); err != nil {
			t.Fatalf("case %d: AcceptBytes = %v, want nil", i, err)
		}
		if err := feedIn(msg, 1, baseV{}); err != nil {
			t.Fatalf("case %d: byte-at-a-time Feed = %v, want nil", i, err)
		}
	}
}

// TestLimitExceededStaysADistinctCategory keeps the §6.3 sentinel property that
// TestPartB_LimitDistinctFromInvalid used to hold: whoever raises it, a policy
// rejection is never conflated with a malformed message or a truncated one. The
// raiser is now a collector — the receiver cap is its RCap — and the decoder
// carries the error back out through the ordinary visitor path unchanged.
func TestLimitExceededStaysADistinctCategory(t *testing.T) {
	var out []string
	// Three elements, a receiver cap of two, and no schema bound in sight.
	raw := wrapperSeq(t, func(e *sofab.Encoder) {
		for i := 0; i < 3; i++ {
			_ = e.WriteString(sofab.ID(i), "x")
		}
	})
	err := collect(raw, sofab.NewStringSeq(&out, sofab.Bounds{}, capArray(2)))
	if !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("= %v, want ErrLimitExceeded", err)
	}
	if errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatal("ErrLimitExceeded must not match ErrInvalidMsg")
	}
	if errors.Is(err, sofab.ErrIncomplete) {
		t.Fatal("ErrLimitExceeded must not match ErrIncomplete")
	}
}

// --- encode helpers ----------------------------------------------------------

func encodeUnsignedArray(t *testing.T, id sofab.ID, a []uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := sofab.WriteUnsignedArray(e, id, a); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeString(t *testing.T, id sofab.ID, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteString(id, s); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeBlob(t *testing.T, id sofab.ID, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteBytes(id, b); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
