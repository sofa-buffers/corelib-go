package sofab

import (
	"encoding/binary"
	"io"
	"math"
	"unicode/utf8"
)

// Encoder writes Sofab fields to an io.Writer. It accumulates the message in an
// internal byte slice and writes it to the destination in one shot on Flush —
// advancing over a contiguous buffer instead of pushing each byte through a
// bufio.Writer interface call. Errors are sticky: once a write fails, subsequent
// writes are no-ops and the same error is returned, so generated Marshal code
// can issue a run of writes and check only the final Flush.
type Encoder struct {
	w     io.Writer
	buf   []byte
	err   error
	depth int
	// pending holds the ids of the innermost open sequences whose header has not
	// been written yet (WriteSequenceBeginLazy, MESSAGE_SPEC §2). It is always a
	// contiguous suffix of the open sequences — writing any field commits the
	// whole run at once — which is what lets WriteSequenceEnd simply drop the
	// last entry. It is truncated, never released, so its backing array is reused
	// for the encoder's lifetime; MaxDepth bounds it at 255 entries.
	//
	// It starts out backed by pendingInline, so the common nesting depth costs no
	// allocation at all; append() spills it to the heap on its own for a schema
	// that nests deeper, with no eager-framing fallback to get wrong. (The
	// self-reference means an Encoder must be used through the *Encoder that
	// NewEncoder returns, never copied by value — which is already the API.)
	pending       []ID
	pendingInline [lazySeqInline]ID
	lim           limits
}

// lazySeqInline is how many held-back sequence ids fit in the Encoder itself
// before the pending run has to spill to the heap. Not a wire-format or API
// limit — nesting deeper still works and still produces canonical bytes, it just
// allocates once. Sized for real schemas rather than MaxDepth so the Encoder
// stays small.
const lazySeqInline = 32

// NewEncoder returns an Encoder writing to w. Of the shared Options only
// WithStrictUTF8 affects encoding (the SOFAB_STRICT_UTF8 policy, §6.4, default
// ON); the receiver-side cap options (WithMaxArrayCount, WithMaxStringLen,
// WithMaxBlobLen) are decode-only and are accepted but ignored here.
func NewEncoder(w io.Writer, opts ...Option) *Encoder {
	e := &Encoder{w: w, buf: make([]byte, 0, 512), lim: newLimits(opts)}
	e.pending = e.pendingInline[:0]
	return e
}

// Err returns the first error encountered, if any.
func (e *Encoder) Err() error { return e.err }

// Flush writes any buffered bytes to the underlying writer in one Write.
func (e *Encoder) Flush() error {
	if e.err != nil {
		return e.err
	}
	if len(e.buf) > 0 {
		_, e.err = e.w.Write(e.buf)
		e.buf = e.buf[:0]
	}
	return e.err
}

// setErr records err as the sticky error, keeping the first one set so the
// original failure is what Err and Flush report.
func (e *Encoder) setErr(err error) {
	if e.err == nil {
		e.err = err
	}
}

// flushThreshold bounds how large the internal buffer grows before it is
// written out mid-stream. It mirrors bufio's capacity-triggered flush: small
// messages accumulate and go out in a single Write on Flush, while a large
// message (or a big blob) drives the destination multiple times and surfaces a
// write error immediately rather than only at Flush.
const flushThreshold = 4096

// maybeFlush writes the buffer out and resets it once it grows past the
// threshold, keeping memory bounded and preserving mid-stream write semantics.
//
// It is called after every varint and every raw write, so for a message that
// never reaches the threshold the check is pure overhead — and it is the common
// case, since flushThreshold is 4 KB. Keeping the body to a length test and a
// call makes it inlinable, so that common case costs a load and a compare
// instead of a function call; the write lives in flushBuffered, whose cost only
// applies when it actually fires. Moving the err test into the slow path is
// behaviour-preserving: with an error held, neither form writes anything.
func (e *Encoder) maybeFlush() {
	if len(e.buf) >= flushThreshold {
		e.flushBuffered()
	}
}

// flushBuffered writes the accumulated bytes out and resets the buffer. It is
// the out-of-line half of maybeFlush; do not call it on the hot path.
//
// go:noinline is load-bearing, not a hint: without it the compiler inlines this
// body back into maybeFlush, whose cost then exceeds the inline budget again and
// the split buys nothing.
//
//go:noinline
func (e *Encoder) flushBuffered() {
	if e.err == nil {
		_, e.err = e.w.Write(e.buf)
		e.buf = e.buf[:0]
	}
}

// putVarint writes v as a base-128 varint, least-significant 7-bit group first.
// It is a no-op once the encoder holds a sticky error.
func (e *Encoder) putVarint(v uint64) {
	if e.err != nil {
		return
	}
	// Hold the slice header in a local across the loop: appending straight to
	// e.buf reloads and stores the three-word header on every byte. Appending
	// the bytes one at a time beats building them in a local array and
	// appending that slice once — measured: the slice form routes short varints
	// through memmove and costs more than it saves.
	buf := e.buf
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	e.buf = append(buf, byte(v))
	e.maybeFlush()
}

// putRaw writes data verbatim (no length prefix). It is a no-op once the
// encoder holds a sticky error.
func (e *Encoder) putRaw(data []byte) {
	if e.err != nil {
		return
	}
	e.buf = append(e.buf, data...)
	e.maybeFlush()
}

// writeHeader writes a field header, the varint (id<<3 | type). It sets
// ErrArgument when id exceeds IDMax and is a no-op once an error is held.
//
// It is the single choke point every field write passes through — every
// Write* method below reaches the wire through it — so it is also where a
// held-back sequence run is committed: the field about to be written is
// content, which proves every enclosing sequence differs from its default and
// must be framed after all (MESSAGE_SPEC §2).
//
// The commit is unconditional on the wire type, because by construction no
// framing marker can arrive here with a run still pending: TypeSequenceStart is
// never routed through writeHeader at all (WriteSequenceBeginLazy only pushes an
// id, and commitPending emits the run's headers itself), and of the two closers
// WriteSequenceEndKeep commits the run before writing its marker while
// WriteSequenceEnd reaches here only once the run is already empty. An earlier
// `t != TypeSequenceStart && t != TypeSequenceEnd` guard here was therefore dead
// — and left the false impression that a marker might legitimately show up
// mid-run — so it is gone rather than kept as decoration.
func (e *Encoder) writeHeader(id ID, t WireType) {
	if e.err != nil {
		return
	}
	if id > IDMax {
		e.err = ErrArgument
		return
	}
	if len(e.pending) != 0 {
		e.commitPending()
	}
	e.putVarint((uint64(id) << 3) | uint64(t))
}

// commitPending writes out the held-back sequence headers, outermost first. It
// runs at most once per non-default sequence, never per field — the per-field
// cost of lazy framing is the length test in writeHeader that guards this call.
//
// Deliberately NOT marked go:noinline, unlike flushBuffered above: that
// annotation exists to keep its *caller* (maybeFlush) inside the inline budget,
// whereas writeHeader is over the budget either way, so forcing a call here
// would only add one. Measured 11 Ir/op cheaper inlined on the typical-message
// workload of bench/run_callgrind.sh.
func (e *Encoder) commitPending() {
	for _, id := range e.pending {
		e.putVarint((uint64(id) << 3) | uint64(TypeSequenceStart))
	}
	e.pending = e.pending[:0]
}

// WriteUnsigned writes an unsigned-integer field.
func (e *Encoder) WriteUnsigned(id ID, v uint64) error {
	e.writeHeader(id, TypeVarintUnsigned)
	e.putVarint(v)
	return e.err
}

// WriteSigned writes a signed-integer field (zigzag + varint).
func (e *Encoder) WriteSigned(id ID, v int64) error {
	e.writeHeader(id, TypeVarintSigned)
	e.putVarint(zigzagEncode(v))
	return e.err
}

// WriteBool writes a boolean as an unsigned 0/1.
func (e *Encoder) WriteBool(id ID, b bool) error {
	if b {
		return e.WriteUnsigned(id, 1)
	}
	return e.WriteUnsigned(id, 0)
}

// writeFixlen writes a fixed-length field: the header, then a length-and-subtype
// varint (len(data)<<3 | sub), then the raw bytes. sub selects float/string/blob.
func (e *Encoder) writeFixlen(id ID, data []byte, sub uint64) {
	e.writeHeader(id, TypeFixlen)
	e.putVarint((uint64(len(data)) << 3) | sub)
	e.putRaw(data)
}

// WriteFloat32 writes a 32-bit float field.
func (e *Encoder) WriteFloat32(id ID, f float32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(f))
	e.writeFixlen(id, b[:], fixFp32)
	return e.err
}

// WriteFloat64 writes a 64-bit float field.
func (e *Encoder) WriteFloat64(id ID, f float64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(f))
	e.writeFixlen(id, b[:], fixFp64)
	return e.err
}

// WriteString writes a string field (raw UTF-8 bytes, no NUL on the wire). The
// bytes are appended straight from the string, with no []byte(s) copy.
//
// When strict UTF-8 is enabled (SOFAB_STRICT_UTF8, the default; §6.4), a Go
// string that is not valid UTF-8 — a byte container can hold arbitrary bytes —
// is refused with ErrArgument (§6.3) and no bytes are written, enforcing the
// producer-side MUST NOT of MESSAGE_SPEC §8. With it disabled
// (WithStrictUTF8(false)) the bytes are written verbatim. utf8.ValidString
// correctly rejects overlong encodings, surrogate code points, and code points
// above U+10FFFF, while accepting an embedded NUL.
func (e *Encoder) WriteString(id ID, s string) error {
	if e.err != nil {
		return e.err
	}
	if e.lim.strictUTF8 && !utf8.ValidString(s) {
		e.setErr(ErrArgument)
		return e.err
	}
	e.writeHeader(id, TypeFixlen)
	e.putVarint((uint64(len(s)) << 3) | fixStr)
	if e.err == nil {
		e.buf = append(e.buf, s...)
		e.maybeFlush()
	}
	return e.err
}

// WriteBytes writes a binary blob field.
func (e *Encoder) WriteBytes(id ID, data []byte) error {
	e.writeFixlen(id, data, fixBlob)
	return e.err
}

// WriteSequenceBeginLazy opens a nested sequence with the given field id and
// holds its header back until the sequence turns out to have content.
//
// MESSAGE_SPEC §2 omits a sequence-typed field whose value equals its declared
// default, and "not one child was written" is exactly that condition — evaluated
// per child field, recursively, for free, because the message layer already
// omits every child equal to its own default. A sequence closed with nothing in
// it therefore emits nothing instead of a two-byte empty frame, and an
// all-default message encodes to the empty byte string. The predicate is never a
// byte image of the object, so struct padding cannot influence it and a non-zero
// nested default is handled by the caller's ordinary per-field test.
//
// The header cannot simply be written and rolled back later: the ids are
// encoder state, not buffer content, which is what keeps the bytes identical
// when the buffer flushes mid-message (a flush can never split a pending run).
//
// This is the only way to open a sequence. How it closes decides whether a
// contentless one survives: WriteSequenceEnd drops it, WriteSequenceEndKeep
// forces the frame out.
//
// Opening would-be sequence number MaxDepth+1 is rejected with ErrArgument and
// writes no bytes, so the wire never nests deeper than MaxDepth (§4.9).
func (e *Encoder) WriteSequenceBeginLazy(id ID) error {
	if e.err != nil {
		return e.err
	}
	if e.depth >= MaxDepth || id > IDMax {
		e.setErr(ErrArgument)
		return e.err
	}
	e.pending = append(e.pending, id)
	e.depth++
	return e.err
}

// WriteSequenceEnd closes the most recently opened nested sequence, letting it
// vanish if it received no content.
//
// Use it wherever absence encodes the same value as an empty frame: a
// struct/union field, and an array field whose declared default is the empty
// collection (MESSAGE_SPEC §2). Where the frame must be visible, close with
// WriteSequenceEndKeep instead.
func (e *Encoder) WriteSequenceEnd() error {
	if e.err != nil {
		return e.err
	}
	if n := len(e.pending); n != 0 {
		// The innermost open sequence is the last held-back one (pending is a
		// contiguous suffix of the open sequences), and it never got content:
		// drop it — header and end marker both.
		e.pending = e.pending[:n-1]
		e.depth--
		return e.err
	}
	e.writeHeader(0, TypeSequenceEnd)
	if e.err == nil && e.depth > 0 {
		e.depth--
	}
	return e.err
}

// WriteSequenceEndKeep closes the most recently opened nested sequence, keeping
// its frame even when it received no content.
//
// It behaves like a write: it first emits any held-back headers — this frame's
// and every enclosing one's — and then the end marker, so an empty sequence
// still reaches the wire as begin+end.
//
// Required wherever the frame carries information beyond its contents:
//
//   - a wrapper-array element (struct/union/nested row): element presence is
//     what carries a dynamic array's length — highest present id + 1 (§5.1) — so
//     dropping an all-default element would change the decoded length, not just
//     the bytes;
//   - an array field already known to differ from a non-empty declared default:
//     absence would reconstruct that default, so the empty frame is the only
//     encoding of "explicitly empty" (§2, §3).
//
// The two failure directions are not symmetric, which is why this is the safe
// choice when a call site is ambiguous: using it where WriteSequenceEnd would do
// costs one non-canonical empty frame that every decoder normalizes away, while
// the reverse silently changes an array's length.
func (e *Encoder) WriteSequenceEndKeep() error {
	if e.err != nil {
		return e.err
	}
	if len(e.pending) != 0 {
		e.commitPending()
	}
	e.writeHeader(0, TypeSequenceEnd)
	if e.err == nil && e.depth > 0 {
		e.depth--
	}
	return e.err
}

// WriteUnsignedArray writes an array of unsigned integers. An empty array is
// valid and emits exactly [header][count=0] (§4.7).
func WriteUnsignedArray[T Unsigned](e *Encoder, id ID, a []T) error {
	e.writeHeader(id, TypeVarintArrayUnsigned)
	e.putVarint(uint64(len(a)))
	for _, x := range a {
		e.putVarint(uint64(x))
	}
	return e.err
}

// WriteSignedArray writes an array of signed integers. An empty array is valid
// and emits exactly [header][count=0] (§4.7).
func WriteSignedArray[T Signed](e *Encoder, id ID, a []T) error {
	e.writeHeader(id, TypeVarintArraySigned)
	e.putVarint(uint64(len(a)))
	for _, x := range a {
		e.putVarint(zigzagEncode(int64(x)))
	}
	return e.err
}

// WriteFloat32Array writes an array of 32-bit floats. An empty array is valid
// and emits exactly [header][count=0][fixlen_word] — the fixlen_word is always
// present (even when empty) so the element subtype is never ambiguous (§4.8).
func (e *Encoder) WriteFloat32Array(id ID, a []float32) error {
	e.writeHeader(id, TypeFixlenArray)
	e.putVarint(uint64(len(a)))
	e.putVarint((4 << 3) | fixFp32)
	var b [4]byte
	for _, f := range a {
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(f))
		e.putRaw(b[:])
	}
	return e.err
}

// WriteFloat64Array writes an array of 64-bit floats. An empty array is valid
// and emits exactly [header][count=0][fixlen_word] — the fixlen_word is always
// present (even when empty) so the element subtype is never ambiguous (§4.8).
func (e *Encoder) WriteFloat64Array(id ID, a []float64) error {
	e.writeHeader(id, TypeFixlenArray)
	e.putVarint(uint64(len(a)))
	e.putVarint((8 << 3) | fixFp64)
	var b [8]byte
	for _, f := range a {
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(f))
		e.putRaw(b[:])
	}
	return e.err
}
