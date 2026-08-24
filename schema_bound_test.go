package sofab_test

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// Issue #80 / CORELIB_PLAN §6.2.1: a receiver-side cap (WithMaxArrayCount,
// WithMaxStringLen, WithMaxBlobLen) "MUST NOT be applied to a field the schema
// already bounds. There the schema bound governs and its violation is INVALID",
// and §6.3 adds that ErrLimitExceeded is "never raised for a field the schema
// bounds". The decoder learns which fields those are from the destination —
// SchemaBoundVisitor on the visitor surfaces, Decoder.SchemaBounded on the pull
// surface.

// declaredBound is the schema bound the test destinations declare on field 7:
// `count: 8` for its array, `maxlen: 8` for its string and blob. Field 3 is the
// schema-UNBOUNDED control, where a receiver cap is the only protection and
// must keep firing.
const declaredBound = 8

const (
	boundedID   sofab.ID = 7
	unboundedID sofab.ID = 3
)

// schemaBoundV is a generated-style destination for that schema: it declares
// the bound (SchemaBoundVisitor) and enforces it at the header (HeaderVisitor),
// which is the pairing generated code emits — the declaration is what stops the
// cap, the enforcement is what replaces it.
type schemaBoundV struct {
	baseV
	arr   []uint64
	str   string
	blob  []byte
	f32   []float32
	asked int // SchemaBound calls, to pin that it is asked only where it matters
}

var (
	_ sofab.HeaderVisitor      = (*schemaBoundV)(nil)
	_ sofab.SchemaBoundVisitor = (*schemaBoundV)(nil)
)

func (v *schemaBoundV) SchemaBound(id sofab.ID, what sofab.BoundKind) bool {
	v.asked++
	if id != boundedID {
		return false
	}
	switch what {
	case sofab.BoundArrayCount, sofab.BoundStringLen, sofab.BoundBlobLen:
		return true
	}
	return false
}

func (v *schemaBoundV) ArrayBegin(id sofab.ID, _ sofab.ArrayKind, count int) error {
	if id == boundedID && count > declaredBound {
		return sofab.ErrInvalidMsg
	}
	return nil
}

func (v *schemaBoundV) FixlenHeader(id sofab.ID, subtype, length int) error {
	if id == boundedID && (subtype == subStr || subtype == subBlob) && length > declaredBound {
		return sofab.ErrInvalidMsg
	}
	return nil
}

func (v *schemaBoundV) UnsignedArray(_ sofab.ID, a []uint64) error { v.arr = a; return nil }
func (v *schemaBoundV) String(_ sofab.ID, s string) error          { v.str = s; return nil }
func (v *schemaBoundV) Bytes(_ sofab.ID, b []byte) error           { v.blob = b; return nil }
func (v *schemaBoundV) Float32Array(_ sofab.ID, a []float32) error { v.f32 = a; return nil }
func (v *schemaBoundV) SignedArray(_ sofab.ID, a []int64) error    { return nil }
func (v *schemaBoundV) Float64Array(_ sofab.ID, a []float64) error { return nil }

// countOnlyV declares ONLY an array `count:` on field 7 — its string/blob are
// schema-unbounded. It is the BoundKind selectivity control: the same id must
// stay capped for the sizes the schema does not bound, which is also what keeps
// a §7.3 skip (a field arriving under a wire type the schema does not declare)
// protected by the cap.
type countOnlyV struct{ baseV }

func (countOnlyV) SchemaBound(id sofab.ID, what sofab.BoundKind) bool {
	return id == boundedID && what == sofab.BoundArrayCount
}

// acceptAll runs a message through every visitor surface, so a fix that lands on
// one kernel and not the other cannot pass. f builds a fresh destination per
// surface (the visitor is stateful).
func acceptAll(t *testing.T, msg []byte, opts []sofab.Option, f func() sofab.Visitor) map[string]error {
	t.Helper()
	out := map[string]error{}
	out["AcceptBytes"] = sofab.AcceptBytes(msg, f(), opts...)
	out["Accept"] = sofab.NewDecoder(bytes.NewReader(msg), opts...).Accept(f())
	out["AcceptStream"] = sofab.NewDecoder(bytes.NewReader(msg), opts...).AcceptStream(f())
	return out
}

func wantAll(t *testing.T, got map[string]error, want error) {
	t.Helper()
	for surface, err := range got {
		switch {
		case want == nil && err != nil:
			t.Errorf("%s = %v, want nil", surface, err)
		case want != nil && !errors.Is(err, want):
			t.Errorf("%s = %v, want %v", surface, err, want)
		}
	}
}

// TestSchemaBoundGovernsArrayCount is the issue's reproduction: field 7 declares
// `count: 8`, the message carries 5 elements, the deployment caps arrays at 2.
// The message is within its schema bound, so it must decode — on every surface.
func TestSchemaBoundGovernsArrayCount(t *testing.T) {
	msg := encodeUnsignedArray(t, boundedID, []uint64{1, 2, 3, 4, 5})
	opts := []sofab.Option{sofab.WithMaxArrayCount(2)}

	var v schemaBoundV
	if err := sofab.AcceptBytes(msg, &v, opts...); err != nil {
		t.Fatalf("AcceptBytes = %v, want nil (the schema bound governs)", err)
	}
	if len(v.arr) != 5 {
		t.Fatalf("delivered %d elements, want 5", len(v.arr))
	}
	wantAll(t, acceptAll(t, msg, opts, func() sofab.Visitor { return &schemaBoundV{} }), nil)
}

// TestSchemaBoundGovernsFixlenArrayCount is the same for a fixlen (fp32) array,
// whose count word is read one step earlier than its element subtype (§4.8).
func TestSchemaBoundGovernsFixlenArrayCount(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteFloat32Array(boundedID, []float32{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	msg := buf.Bytes()

	var v schemaBoundV
	if err := sofab.AcceptBytes(msg, &v, sofab.WithMaxArrayCount(2)); err != nil {
		t.Fatalf("AcceptBytes = %v, want nil", err)
	}
	if len(v.f32) != 5 {
		t.Fatalf("delivered %d elements, want 5", len(v.f32))
	}
	wantAll(t, acceptAll(t, msg, []sofab.Option{sofab.WithMaxArrayCount(2)},
		func() sofab.Visitor { return &schemaBoundV{} }), nil)
}

// TestSchemaBoundGovernsStringAndBlobLen covers the maxlen half: a 5-byte
// string/blob on the schema-bounded field decodes under a 2-byte cap.
func TestSchemaBoundGovernsStringAndBlobLen(t *testing.T) {
	str := encodeString(t, boundedID, "hello")
	blob := encodeBlob(t, boundedID, []byte{1, 2, 3, 4, 5})

	var sv schemaBoundV
	if err := sofab.AcceptBytes(str, &sv, sofab.WithMaxStringLen(2)); err != nil {
		t.Fatalf("string AcceptBytes = %v, want nil", err)
	}
	if sv.str != "hello" {
		t.Fatalf("string = %q, want %q", sv.str, "hello")
	}
	var bv schemaBoundV
	if err := sofab.AcceptBytes(blob, &bv, sofab.WithMaxBlobLen(2)); err != nil {
		t.Fatalf("blob AcceptBytes = %v, want nil", err)
	}
	if len(bv.blob) != 5 {
		t.Fatalf("blob = %d bytes, want 5", len(bv.blob))
	}

	wantAll(t, acceptAll(t, str, []sofab.Option{sofab.WithMaxStringLen(2)},
		func() sofab.Visitor { return &schemaBoundV{} }), nil)
	wantAll(t, acceptAll(t, blob, []sofab.Option{sofab.WithMaxBlobLen(2)},
		func() sofab.Visitor { return &schemaBoundV{} }), nil)
}

// TestReceiverLimitCapsUnboundedField is the other half of §6.2.1: on a field
// the schema does NOT bound, the cap is the only protection there is and must
// still fire — both for a destination that declares bounds elsewhere and for one
// that implements no extension at all.
func TestReceiverLimitCapsUnboundedField(t *testing.T) {
	arr := encodeUnsignedArray(t, unboundedID, []uint64{1, 2, 3, 4, 5})
	str := encodeString(t, unboundedID, "hello")
	blob := encodeBlob(t, unboundedID, []byte{1, 2, 3, 4, 5})

	cases := []struct {
		name string
		msg  []byte
		opt  sofab.Option
	}{
		{"array", arr, sofab.WithMaxArrayCount(2)},
		{"string", str, sofab.WithMaxStringLen(2)},
		{"blob", blob, sofab.WithMaxBlobLen(2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []sofab.Option{tc.opt}
			wantAll(t, acceptAll(t, tc.msg, opts, func() sofab.Visitor { return &schemaBoundV{} }),
				sofab.ErrLimitExceeded)
			// A visitor with no extension at all is unchanged by this fix.
			wantAll(t, acceptAll(t, tc.msg, opts, func() sofab.Visitor { return baseV{} }),
				sofab.ErrLimitExceeded)
		})
	}
}

// TestSchemaBoundKindSelectivity pins that the exemption is per (id, BoundKind),
// not per id: countOnlyV declares only the array `count:` on field 7, so a
// string on the same id stays capped. That is also the §7.3 rule — a field
// arriving under a shape the schema does not declare for that id is asked about
// a bound that does not exist, answers false, and keeps the cap.
func TestSchemaBoundKindSelectivity(t *testing.T) {
	arr := encodeUnsignedArray(t, boundedID, []uint64{1, 2, 3, 4, 5})
	str := encodeString(t, boundedID, "hello")

	wantAll(t, acceptAll(t, arr, []sofab.Option{sofab.WithMaxArrayCount(2)},
		func() sofab.Visitor { return countOnlyV{} }), nil)
	wantAll(t, acceptAll(t, str, []sofab.Option{sofab.WithMaxStringLen(2)},
		func() sofab.Visitor { return countOnlyV{} }), sofab.ErrLimitExceeded)
}

// TestSchemaBoundOverBoundIsInvalidNotLimit is the reverse error the issue
// names: with the cap out of the way, a header past the SCHEMA bound must be
// INVALID (MESSAGE_SPEC §7.1) — the destination's own header hook decides it —
// and never ErrLimitExceeded, whichever of the two numbers is smaller. The
// payloads are truncated on purpose, so the verdict can only come from the
// header (§5.2 anti-folding).
func TestSchemaBoundOverBoundIsInvalidNotLimit(t *testing.T) {
	// array id 7, count 9 (> the declared 8), two elements present.
	arr := append(vhdr(boundedID, sofab.TypeVarintArrayUnsigned), 9, 1, 2)
	// string id 7, length 9 (> the declared 8), two payload bytes present.
	str := append(vhdr(boundedID, sofab.TypeFixlen), (9<<3)|subStr, 'a', 'b')

	for _, tc := range []struct {
		name string
		msg  []byte
		opt  sofab.Option
	}{
		{"array", arr, sofab.WithMaxArrayCount(2)},
		{"string", str, sofab.WithMaxStringLen(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := acceptAll(t, tc.msg, []sofab.Option{tc.opt},
				func() sofab.Visitor { return &schemaBoundV{} })
			wantAll(t, got, sofab.ErrInvalidMsg)
			for surface, err := range got {
				if errors.Is(err, sofab.ErrLimitExceeded) {
					t.Errorf("%s = ErrLimitExceeded, want ErrInvalidMsg (§6.3)", surface)
				}
			}
		})
	}
}

// TestSchemaBoundAskedOnlyWhenCapWouldFire pins the maxspeed property the hook
// is designed around: it is consulted only after a configured cap has been
// exceeded, so a decode with no limits — or one comfortably inside them — never
// pays for the question, not even the type assertion.
func TestSchemaBoundAskedOnlyWhenCapWouldFire(t *testing.T) {
	msg := encodeUnsignedArray(t, boundedID, []uint64{1, 2, 3, 4, 5})

	var none schemaBoundV
	if err := sofab.AcceptBytes(msg, &none); err != nil {
		t.Fatalf("no-limit decode = %v", err)
	}
	if none.asked != 0 {
		t.Errorf("no-limit decode asked SchemaBound %d times, want 0", none.asked)
	}

	var within schemaBoundV
	if err := sofab.AcceptBytes(msg, &within, sofab.WithMaxArrayCount(100)); err != nil {
		t.Fatalf("within-limit decode = %v", err)
	}
	if within.asked != 0 {
		t.Errorf("within-limit decode asked SchemaBound %d times, want 0", within.asked)
	}

	var over schemaBoundV
	if err := sofab.AcceptBytes(msg, &over, sofab.WithMaxArrayCount(2)); err != nil {
		t.Fatalf("over-limit decode = %v", err)
	}
	if over.asked != 1 {
		t.Errorf("over-limit decode asked SchemaBound %d times, want 1 (once per field)", over.asked)
	}
}

// TestSchemaBoundCostsNoAllocation pins the other half of that maxspeed
// property: carrying the schema-bound source through the header reads must not
// add an allocation. It is a real hazard — an earlier draft memoized the type
// assertion in a scope-local cache, and the pointer to it made the cache escape,
// costing one heap allocation per decode scope. A destination that implements the
// extension must allocate exactly as one that does not.
func TestSchemaBoundCostsNoAllocation(t *testing.T) {
	msg := encodeUnsignedArray(t, boundedID, []uint64{1, 2, 3, 4, 5})

	plain := testing.AllocsPerRun(200, func() {
		if err := sofab.AcceptBytes(msg, baseV{}); err != nil {
			t.Fatal(err)
		}
	})
	bound := testing.AllocsPerRun(200, func() {
		if err := sofab.AcceptBytes(msg, &schemaBoundV{}); err != nil {
			t.Fatal(err)
		}
	})
	// The bound destination is a pointer receiver the test allocates itself, so
	// allow that one; anything beyond it is decoder overhead this fix must not add.
	if bound > plain+1 {
		t.Errorf("decode allocates %v with SchemaBoundVisitor vs %v without", bound, plain)
	}
}

// TestSchemaBoundInNestedSequence checks the hook reaches a nested scope's own
// visitor: the bound belongs to the destination the sequence descends into, not
// to the top-level one, and each scope resolves its own.
func TestSchemaBoundInNestedSequence(t *testing.T) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	if err := e.WriteSequenceBeginLazy(1); err != nil {
		t.Fatal(err)
	}
	if err := sofab.WriteUnsignedArray(e, boundedID, []uint64{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteSequenceEnd(); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	msg := buf.Bytes()

	wantAll(t, acceptAll(t, msg, []sofab.Option{sofab.WithMaxArrayCount(2)},
		func() sofab.Visitor { return &nestingV{} }), nil)
	// The same nested array on the schema-unbounded id is still capped.
	buf.Reset()
	e = sofab.NewEncoder(&buf)
	if err := e.WriteSequenceBeginLazy(1); err != nil {
		t.Fatal(err)
	}
	if err := sofab.WriteUnsignedArray(e, unboundedID, []uint64{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteSequenceEnd(); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	wantAll(t, acceptAll(t, buf.Bytes(), []sofab.Option{sofab.WithMaxArrayCount(2)},
		func() sofab.Visitor { return &nestingV{} }), sofab.ErrLimitExceeded)
}

// nestingV is a top-level destination with no bounds of its own that descends
// into a schemaBoundV for the nested scope — the generated shape for a sequence
// field whose message type declares the bound.
type nestingV struct{ baseV }

func (nestingV) BeginSequence(sofab.ID) (sofab.Visitor, error) { return &schemaBoundV{}, nil }
