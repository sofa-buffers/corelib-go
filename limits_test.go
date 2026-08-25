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

// TestPartB_ArrayCountLimit is the Part B acceptance test for WithMaxArrayCount:
// with the limit set, an otherwise-valid message whose array exceeds it fails
// with ErrLimitExceeded; the identical message decodes cleanly with no limit,
// and a message exactly at the limit is accepted.
func TestPartB_ArrayCountLimit(t *testing.T) {
	const limit = 65536
	over := encodeUnsignedArray(t, 1, make([]uint64, limit+1))
	at := encodeUnsignedArray(t, 1, make([]uint64, limit))

	// No limit: the oversize message decodes fine (default = today's behavior).
	if err := acceptBytes(over, baseV{}); err != nil {
		t.Fatalf("no-limit decode = %v, want nil", err)
	}
	// With the limit, the same bytes are rejected as ErrLimitExceeded.
	if err := acceptBytes(over, baseV{}, sofab.WithMaxArrayCount(limit)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("over-limit visitor = %v, want ErrLimitExceeded", err)
	}
	// Exactly at the limit is allowed (reject is strictly greater-than).
	if err := acceptBytes(at, baseV{}, sofab.WithMaxArrayCount(limit)); err != nil {
		t.Fatalf("at-limit visitor = %v, want nil", err)
	}
	// The chunked feed enforces the same limit on the same bytes: the cap is
	// judged at the count word, so it does not depend on where a chunk boundary
	// fell.
	if err := feedIn(over, 1, baseV{}, sofab.WithMaxArrayCount(limit)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("over-limit Feed = %v, want ErrLimitExceeded", err)
	}
}

// TestPartB_StringAndBlobLimit checks WithMaxStringLen / WithMaxBlobLen the same
// way: over the limit rejects with ErrLimitExceeded, no limit and at-limit pass.
func TestPartB_StringAndBlobLimit(t *testing.T) {
	const limit = 1024
	strOver := encodeString(t, 1, strings.Repeat("a", limit+1))
	strAt := encodeString(t, 1, strings.Repeat("a", limit))
	blobOver := encodeBlob(t, 1, bytes.Repeat([]byte{0xAB}, limit+1))
	blobAt := encodeBlob(t, 1, bytes.Repeat([]byte{0xAB}, limit))

	// String.
	if err := acceptBytes(strOver, baseV{}); err != nil {
		t.Fatalf("no-limit string decode = %v, want nil", err)
	}
	if err := acceptBytes(strOver, baseV{}, sofab.WithMaxStringLen(limit)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("over-limit string = %v, want ErrLimitExceeded", err)
	}
	if err := acceptBytes(strAt, baseV{}, sofab.WithMaxStringLen(limit)); err != nil {
		t.Fatalf("at-limit string = %v, want nil", err)
	}
	// A blob limit does not restrict a string, and vice versa.
	if err := acceptBytes(strOver, baseV{}, sofab.WithMaxBlobLen(limit)); err != nil {
		t.Fatalf("string under blob-only limit = %v, want nil", err)
	}

	// Blob.
	if err := acceptBytes(blobOver, baseV{}, sofab.WithMaxBlobLen(limit)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("over-limit blob = %v, want ErrLimitExceeded", err)
	}
	if err := acceptBytes(blobAt, baseV{}, sofab.WithMaxBlobLen(limit)); err != nil {
		t.Fatalf("at-limit blob = %v, want nil", err)
	}

	// The chunked feed, blob.
	if err := feedIn(blobOver, 1, baseV{}, sofab.WithMaxBlobLen(limit)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("over-limit blob Feed = %v, want ErrLimitExceeded", err)
	}
}

// TestPartB_LimitEnforcedBeforePayload proves the limit is applied at the header,
// before any payload is buffered: a header claiming an oversize count/length with
// NO payload bytes at all still fails with ErrLimitExceeded (not ErrIncomplete).
func TestPartB_LimitEnforcedBeforePayload(t *testing.T) {
	// Array header + count, no elements follow.
	arr := append(vhdr(0, sofab.TypeVarintArrayUnsigned), vbytes(arrayMaxCount)...)
	if err := acceptBytes(arr, baseV{}, sofab.WithMaxArrayCount(1000)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("array header-only = %v, want ErrLimitExceeded", err)
	}
	// String header claiming 2000 bytes, no payload follows.
	str := append(vhdr(0, sofab.TypeFixlen), vbytes((2000<<3)|subStr)...)
	if err := acceptBytes(str, baseV{}, sofab.WithMaxStringLen(1000)); !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("string header-only = %v, want ErrLimitExceeded", err)
	}
}

// TestPartB_LimitDistinctFromInvalid locks the sentinel semantics: a limit
// rejection is ErrLimitExceeded and is NOT confused with ErrInvalidMsg or
// ErrIncomplete (differential fuzzing must not read it as a conformance
// divergence). A non-positive limit means unlimited.
func TestPartB_LimitDistinctFromInvalid(t *testing.T) {
	over := encodeUnsignedArray(t, 1, make([]uint64, 11))
	err := acceptBytes(over, baseV{}, sofab.WithMaxArrayCount(10))
	if !errors.Is(err, sofab.ErrLimitExceeded) {
		t.Fatalf("= %v, want ErrLimitExceeded", err)
	}
	if errors.Is(err, sofab.ErrInvalidMsg) {
		t.Fatal("ErrLimitExceeded must not match ErrInvalidMsg")
	}
	if errors.Is(err, sofab.ErrIncomplete) {
		t.Fatal("ErrLimitExceeded must not match ErrIncomplete")
	}
	// A non-positive limit is treated as no limit at all.
	if err := acceptBytes(over, baseV{}, sofab.WithMaxArrayCount(0)); err != nil {
		t.Fatalf("zero (unlimited) limit = %v, want nil", err)
	}
	if err := acceptBytes(over, baseV{}, sofab.WithMaxArrayCount(-1)); err != nil {
		t.Fatalf("negative (unlimited) limit = %v, want nil", err)
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
