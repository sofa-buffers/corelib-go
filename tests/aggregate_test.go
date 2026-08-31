package sofab_test

// agg — the test-side adapter from the corelib's PIECEWISE decode surface to
// whole values.
//
// CORELIB_PLAN §6.6.3 forbids the codec to deliver a materialized aggregate: a
// callback carrying a whole string, blob or element list obliges the codec to
// build it, and the only size available to build it from is the wire's. So the
// corelib delivers a payload as FixlenBegin plus String/Bytes pieces, and an
// array as ArrayBegin, one element callback per element, ArrayEnd — and
// assembling those back into a value is the DESTINATION's job, on the
// destination's own storage.
//
// This type is that destination, written once for the whole test suite. It is
// exactly the shape generated Go code takes, which is why the assertions the
// suite made about whole values are still the right assertions to make: what
// moved is who allocates, not what the wire means.
//
// It dispatches to whichever of the aggregate methods the wrapped value has,
// through the optional interfaces below — so a test visitor declares only the
// callbacks its case is about, exactly as before.

import (
	sofab "github.com/sofa-buffers/corelib-go"
)

type aggUnsigned interface{ Unsigned(sofab.ID, uint64) error }
type aggSigned interface{ Signed(sofab.ID, int64) error }
type aggFloat32 interface{ Float32(sofab.ID, float32) error }
type aggFloat64 interface{ Float64(sofab.ID, float64) error }
type aggString interface{ String(sofab.ID, string) error }
type aggBytes interface{ Bytes(sofab.ID, []byte) error }
type aggUArray interface {
	UnsignedArray(sofab.ID, []uint64) error
}
type aggSArray interface{ SignedArray(sofab.ID, []int64) error }
type aggF32Array interface {
	Float32Array(sofab.ID, []float32) error
}
type aggF64Array interface {
	Float64Array(sofab.ID, []float64) error
}
type aggBegin interface {
	BeginSequence(sofab.ID) (any, error)
}
type aggEnd interface{ EndSequence() error }

// aggHeader is the header hook the corelib used to offer as the optional
// HeaderVisitor extension. It is now two ordinary Visitor methods (ArrayBegin,
// FixlenBegin), and this is the test-side forward to the old spelling.
type aggHeader interface {
	ArrayBegin(sofab.ID, sofab.ArrayKind, int) error
	FixlenHeader(sofab.ID, int, int) error
}

// aggElemBound is the element-width bound the corelib used to offer as the
// optional ElemBoundVisitor extension. With elements delivered one at a time
// the destination applies it itself, as it goes — which is what keeps an
// over-width element INVALID rather than INCOMPLETE when the array is then
// truncated. This is that check, on the destination's side of the callback.
type aggElemBound interface {
	ArrayElemBound(sofab.ID, sofab.ArrayKind) (min, max int64, ok bool)
}

// agg wraps one destination. Construct it with aggOf.
type agg struct {
	v any

	pay  []byte // the string/blob payload being assembled
	kind sofab.ArrayKind
	us   []uint64
	ss   []int64
	f32  []float32
	f64  []float64

	lo, hi   int64
	bounded  bool
	boundSig bool
}

func aggOf(v any) sofab.Visitor { return &agg{v: v} }

// asVisitor hands a destination to the decoder: one already written against the
// corelib's own piecewise surface goes straight through, an aggregate-shaped one
// gets an adapter.
func asVisitor(v any) sofab.Visitor {
	if vv, ok := v.(sofab.Visitor); ok {
		return vv
	}
	return aggOf(v)
}

// acceptBytes is sofab.AcceptBytes with asVisitor applied, so the suite's
// aggregate-shaped destinations keep working unchanged.
func acceptBytes(in []byte, v any, opts ...sofab.Option) error {
	return sofab.AcceptBytes(in, asVisitor(v), opts...)
}

func (a *agg) Unsigned(id sofab.ID, v uint64) error {
	if h, ok := a.v.(aggUnsigned); ok {
		return h.Unsigned(id, v)
	}
	return nil
}

func (a *agg) Signed(id sofab.ID, v int64) error {
	if h, ok := a.v.(aggSigned); ok {
		return h.Signed(id, v)
	}
	return nil
}

func (a *agg) Float32(id sofab.ID, v float32) error {
	if h, ok := a.v.(aggFloat32); ok {
		return h.Float32(id, v)
	}
	return nil
}

func (a *agg) Float64(id sofab.ID, v float64) error {
	if h, ok := a.v.(aggFloat64); ok {
		return h.Float64(id, v)
	}
	return nil
}

func (a *agg) FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, total int) error {
	a.pay = a.pay[:0]
	if h, ok := a.v.(aggHeader); ok {
		return h.FixlenHeader(id, int(sub), total)
	}
	return nil
}

func (a *agg) String(id sofab.ID, total, offset int, chunk []byte) error {
	a.pay = append(a.pay, chunk...)
	if offset+len(chunk) < total {
		return nil
	}
	if h, ok := a.v.(aggString); ok {
		return h.String(id, string(a.pay))
	}
	return nil
}

func (a *agg) Bytes(id sofab.ID, total, offset int, chunk []byte) error {
	a.pay = append(a.pay, chunk...)
	if offset+len(chunk) < total {
		return nil
	}
	if h, ok := a.v.(aggBytes); ok {
		return h.Bytes(id, a.pay)
	}
	return nil
}

func (a *agg) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
	a.kind = kind
	a.us, a.ss, a.f32, a.f64 = a.us[:0], a.ss[:0], a.f32[:0], a.f64[:0]
	a.bounded = false
	if b, ok := a.v.(aggElemBound); ok {
		lo, hi, on := b.ArrayElemBound(id, kind)
		a.lo, a.hi, a.bounded = lo, hi, on
		a.boundSig = kind == sofab.ArraySigned
	}
	if h, ok := a.v.(aggHeader); ok {
		return h.ArrayBegin(id, kind, count)
	}
	return nil
}

func (a *agg) ArrayUnsigned(_ sofab.ID, _ int, v uint64) error {
	if a.bounded && !a.boundSig && v > uint64(a.hi) {
		return sofab.ErrInvalidMsg
	}
	a.us = append(a.us, v)
	return nil
}

func (a *agg) ArraySigned(_ sofab.ID, _ int, v int64) error {
	if a.bounded && a.boundSig && (v < a.lo || v > a.hi) {
		return sofab.ErrInvalidMsg
	}
	a.ss = append(a.ss, v)
	return nil
}

func (a *agg) ArrayFloat32(_ sofab.ID, _ int, v float32) error {
	a.f32 = append(a.f32, v)
	return nil
}

func (a *agg) ArrayFloat64(_ sofab.ID, _ int, v float64) error {
	a.f64 = append(a.f64, v)
	return nil
}

func (a *agg) ArrayEnd(id sofab.ID) error {
	switch a.kind {
	case sofab.ArrayUnsigned:
		if h, ok := a.v.(aggUArray); ok {
			return h.UnsignedArray(id, a.us)
		}
	case sofab.ArraySigned:
		if h, ok := a.v.(aggSArray); ok {
			return h.SignedArray(id, a.ss)
		}
	case sofab.ArrayFp32:
		if h, ok := a.v.(aggF32Array); ok {
			return h.Float32Array(id, a.f32)
		}
	default:
		if h, ok := a.v.(aggF64Array); ok {
			return h.Float64Array(id, a.f64)
		}
	}
	return nil
}

func (a *agg) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
	h, ok := a.v.(aggBegin)
	if !ok {
		return &agg{}, nil
	}
	child, err := h.BeginSequence(id)
	if err != nil {
		return nil, err
	}
	if child == nil {
		return nil, nil
	}
	// A destination that already speaks the corelib's own surface (a collector)
	// is handed through unwrapped; anything else gets an adapter of its own.
	if v, ok := child.(sofab.Visitor); ok {
		return v, nil
	}
	return &agg{v: child}, nil
}

func (a *agg) EndSequence() error {
	if h, ok := a.v.(aggEnd); ok {
		return h.EndSequence()
	}
	return nil
}

// SetStringCheck forwards the one optional extension the corelib offers, so a
// destination behind the adapter keeps receiving it.
func (a *agg) SetStringCheck(p sofab.StringCheck) {
	if h, ok := a.v.(sofab.StringPolicyVisitor); ok {
		h.SetStringCheck(p)
	}
}
