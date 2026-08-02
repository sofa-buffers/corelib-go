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
	w io.Writer
	// buf is a fixed window, not an append target: len(buf) is its capacity and
	// n is how much of it is used. Appending byte-by-byte cost a length/capacity
	// test, a possible growslice call, and a three-word slice-header store per
	// byte — 84 % of the array-encode profile. Writing through an index instead
	// makes the hot path a store plus an integer bump, with one capacity test per
	// field rather than per byte, and never rewrites the slice header (so no
	// write-barrier check either).
	//
	// The window grows by doubling up to flushThreshold and then stops: past
	// that it is drained to w instead, which is what keeps memory bounded and
	// preserves mid-stream flushing.
	buf   []byte
	n     int
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
	e := &Encoder{w: w, buf: make([]byte, encBufInit), lim: newLimits(opts)}
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
	if e.n > 0 {
		_, e.err = e.w.Write(e.buf[:e.n])
		e.n = 0
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

// encBufInit is the initial window size. Most messages are small, so the
// encoder starts here and doubles on demand rather than committing
// flushThreshold bytes to every Encoder that will never need them.
const encBufInit = 512

// room reports whether k bytes can be written at the current position, making
// space if not: it grows the window while that is still allowed, and otherwise
// drains it to the writer. It returns false only when the encoder holds an
// error, so callers can write unconditionally after a true.
//
// This is the one capacity test on the hot path, and it is per field (or per
// batch of array elements) rather than per byte. Everything that can fail lives
// in the out-of-line makeRoom, keeping room itself small enough to inline.
func (e *Encoder) room(k int) bool {
	if len(e.buf)-e.n >= k {
		return e.err == nil
	}
	return e.makeRoom(k)
}

// makeRoom is the out-of-line half of room: grow, or flush, or both.
//
// go:noinline is load-bearing, not a hint: inlined, its body pushes room past
// the inline budget and the fast path becomes a call again.
//
//go:noinline
func (e *Encoder) makeRoom(k int) bool {
	if e.err != nil {
		return false
	}
	// Grow first while the window is still below the threshold: a small message
	// then accumulates entirely and leaves in a single Write on Flush, exactly
	// as the append-based buffer did. Doubling from encBufInit lands on
	// flushThreshold exactly (both are powers of two), so the window converges
	// there and stops growing.
	if len(e.buf) < flushThreshold && len(e.buf)-e.n < k {
		size := len(e.buf)
		for size < flushThreshold && size-e.n < k {
			size *= 2
		}
		grown := make([]byte, size)
		copy(grown, e.buf[:e.n])
		e.buf = grown
		if len(e.buf)-e.n >= k {
			return true
		}
	}
	// At the cap and still too tight: drain, which is the mid-stream flush.
	if e.n > 0 {
		if _, err := e.w.Write(e.buf[:e.n]); err != nil {
			e.err = err
			return false
		}
		e.n = 0
	}
	// A single field wider than the whole window. putRaw routes anything past
	// flushThreshold straight to the sink, so this only rounds a sub-threshold
	// window up to the request.
	if len(e.buf) < k {
		size := len(e.buf)
		for size < k {
			size *= 2
		}
		e.buf = make([]byte, size)
	}
	return true
}

// flushBuffered drains the window to the writer. Used where a flush is wanted
// for its own sake — ordering a direct write behind the buffered bytes.
//
//go:noinline
func (e *Encoder) flushBuffered() {
	if e.err == nil && e.n > 0 {
		_, e.err = e.w.Write(e.buf[:e.n])
		e.n = 0
	}
}

// putVarint writes v as a base-128 varint, least-significant 7-bit group first.
// It is a no-op once the encoder holds a sticky error.
//
// It is the reserve-then-write pair: room makes space, putVarintFit writes into
// it. Callers that already reserve — every Write* method, via writeHeaderRoom —
// skip this and call putVarintFit directly, which is what removes the second
// capacity test from a scalar field.
//
// No separate flush test is needed afterwards: room() is what drains a full
// window, so the mid-stream write still happens, one reserve earlier.
func (e *Encoder) putVarint(v uint64) {
	if !e.room(maxVarintLen) {
		return
	}
	e.putVarintFit(v)
}

// putRaw writes data verbatim (no length prefix). It is a no-op once the
// encoder holds a sticky error.
//
// A payload past the window cap is written straight to the sink after the
// window is drained, so the byte stream is unchanged while an oversized blob
// never forces the buffer to grow to its size.
func (e *Encoder) putRaw(data []byte) {
	if len(data) > flushThreshold {
		e.putRawLarge(data)
		return
	}
	if !e.room(len(data)) {
		return
	}
	e.n += copy(e.buf[e.n:], data)
}

// putRawLarge writes a payload bigger than the whole window: drain what is
// buffered first so ordering holds, then hand the payload to the sink directly.
//
//go:noinline
func (e *Encoder) putRawLarge(data []byte) {
	e.flushBuffered()
	if e.err != nil {
		return
	}
	if _, err := e.w.Write(data); err != nil {
		e.err = err
	}
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
	e.writeHeaderRoom(id, t, 0)
}

// writeHeaderRoom is writeHeader with the field's payload reserved alongside the
// header, and reports whether the caller may go on to write it.
//
// extra is how many further bytes the caller will write with no capacity test of
// its own. A scalar field is a header varint plus a value varint, so reserving
// both together turns two capacity tests, two error tests and two bounds checks
// into one of each — measured about a fifth of the typical-message encode. extra
// must not exceed flushThreshold-maxVarintLen; every caller passes a small
// constant.
func (e *Encoder) writeHeaderRoom(id ID, t WireType, extra int) bool {
	// The three conditions that make a header anything other than "reserve and
	// write" are peeled into writeHeaderSlow so this stays under the inline
	// budget: a field header then costs no call at all.
	if e.err != nil || id > IDMax || len(e.pending) != 0 {
		return e.writeHeaderSlow(id, t, extra)
	}
	if !e.room(maxVarintLen + extra) {
		return false
	}
	e.putVarintFit((uint64(id) << 3) | uint64(t))
	return true
}

// writeHeaderSlow handles a held error, an out-of-range id, and a pending lazy
// sequence run. It is the out-of-line half of writeHeaderRoom.
//
//go:noinline
func (e *Encoder) writeHeaderSlow(id ID, t WireType, extra int) bool {
	if e.err != nil {
		return false
	}
	if id > IDMax {
		e.err = ErrArgument
		return false
	}
	// Committing writes the enclosing sequence headers, consuming room, so the
	// reservation below has to come after it.
	e.commitPending()
	if !e.room(maxVarintLen + extra) {
		return false
	}
	e.putVarintFit((uint64(id) << 3) | uint64(t))
	return true
}

// putVarintFit writes a varint into room the caller has already reserved. It is
// the no-capacity-test half of putVarint; calling it without that reservation
// panics on the slice bound rather than corrupting the stream.
//
// The single-byte case is the body, and the multi-byte call is pushed into
// putVarintFitSlow, so this stays under the inline budget: nearly every varint a
// message writes — field headers for id < 16, small counts, small scalars — then
// costs a store and an increment at the call site with no call at all.
func (e *Encoder) putVarintFit(v uint64) {
	if v < 0x80 {
		e.buf[e.n] = byte(v)
		e.n++
		return
	}
	e.putVarintFitSlow(v)
}

// putVarintFitSlow is the out-of-line multi-byte half of putVarintFit.
//
// go:noinline is load-bearing: inlined, the putUvarint call it holds pushes
// putVarintFit back over the budget and the fast path becomes a call again.
//
//go:noinline
func (e *Encoder) putVarintFitSlow(v uint64) {
	e.n = putUvarint(e.buf, e.n, v)
}

// commitPending writes out the held-back sequence headers, outermost first. It
// runs at most once per non-default sequence, never per field — the per-field
// cost of lazy framing is the length test in writeHeaderRoom that routes here.
//
// It needs no go:noinline of its own: its callers are already out of line
// (writeHeaderSlow) or off the hot path (WriteSequenceEndKeep), so inlining it
// cannot push a hot function over the budget.
func (e *Encoder) commitPending() {
	for _, id := range e.pending {
		e.putVarint((uint64(id) << 3) | uint64(TypeSequenceStart))
	}
	e.pending = e.pending[:0]
}

// WriteUnsigned writes an unsigned-integer field.
func (e *Encoder) WriteUnsigned(id ID, v uint64) error {
	if e.writeHeaderRoom(id, TypeVarintUnsigned, maxVarintLen) {
		e.putVarintFit(v)
	}
	return e.err
}

// WriteSigned writes a signed-integer field (zigzag + varint).
func (e *Encoder) WriteSigned(id ID, v int64) error {
	if e.writeHeaderRoom(id, TypeVarintSigned, maxVarintLen) {
		e.putVarintFit(zigzagEncode(v))
	}
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
	if e.writeHeaderRoom(id, TypeFixlen, maxVarintLen) {
		e.putVarintFit((uint64(len(data)) << 3) | sub)
		e.putRaw(data)
	}
}

// WriteFloat32 writes a 32-bit float field.
//
// The payload goes straight into the window rather than through a stack array
// and putRaw: the array escaped to the heap (putRaw can hand a payload to the
// out-of-line sink path), so the temporary cost an allocation per float on top
// of the copy.
func (e *Encoder) WriteFloat32(id ID, f float32) error {
	if e.writeHeaderRoom(id, TypeFixlen, maxVarintLen+4) {
		e.putVarintFit((4 << 3) | fixFp32)
		binary.LittleEndian.PutUint32(e.buf[e.n:], math.Float32bits(f))
		e.n += 4
	}
	return e.err
}

// WriteFloat64 writes a 64-bit float field. See WriteFloat32 on the direct
// window write.
func (e *Encoder) WriteFloat64(id ID, f float64) error {
	if e.writeHeaderRoom(id, TypeFixlen, maxVarintLen+8) {
		e.putVarintFit((8 << 3) | fixFp64)
		binary.LittleEndian.PutUint64(e.buf[e.n:], math.Float64bits(f))
		e.n += 8
	}
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
	if e.writeHeaderRoom(id, TypeFixlen, maxVarintLen) {
		e.putVarintFit((uint64(len(s)) << 3) | fixStr)
		e.putRawString(s)
	}
	return e.err
}

// putRawString is putRaw for a string, copied straight from the string with no
// []byte(s) round trip.
func (e *Encoder) putRawString(s string) {
	if len(s) > flushThreshold {
		e.putRawStringLarge(s)
		return
	}
	if !e.room(len(s)) {
		return
	}
	e.n += copy(e.buf[e.n:], s)
}

// putRawStringLarge is putRawLarge for a string. It prefers the sink's
// WriteString (bytes.Buffer, strings.Builder, bufio.Writer all have one) so an
// oversized payload still costs no []byte(s) copy.
//
//go:noinline
func (e *Encoder) putRawStringLarge(s string) {
	e.flushBuffered()
	if e.err != nil {
		return
	}
	var err error
	if sw, ok := e.w.(io.StringWriter); ok {
		_, err = sw.WriteString(s)
	} else {
		_, err = e.w.Write([]byte(s))
	}
	if err != nil {
		e.err = err
	}
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
	if !e.writeHeaderRoom(id, TypeVarintArrayUnsigned, maxVarintLen) {
		return e.err
	}
	e.putVarintFit(uint64(len(a)))
	return putUvarintRun(e, a)
}

// WriteSignedArray writes an array of signed integers. An empty array is valid
// and emits exactly [header][count=0] (§4.7).
func WriteSignedArray[T Signed](e *Encoder, id ID, a []T) error {
	if !e.writeHeaderRoom(id, TypeVarintArraySigned, maxVarintLen) {
		return e.err
	}
	e.putVarintFit(uint64(len(a)))
	return putZigzagRun(e, a)
}

// putUvarintRun and putZigzagRun write an integer array's elements.
//
// The varint writer is unrolled INTO each loop rather than called per element:
// calling putUvarint cost argument setup, a call/return pair and a re-derived
// window on every element — roughly a quarter of the array-encode profile — so
// the bulk path pays none of it.
//
// The destination advances as a slice, and `len(w) >= maxVarintLen` is carried
// in the LOOP CONDITION. That is what makes the ten stores bounds-check-free:
// the prove pass knows w[0]..w[9] are in range, so no per-element slice is cut
// from the window. It doubles as the refill test — the inner loop simply ends
// when the window can no longer hold a full varint, and the outer loop drains
// and re-enters.
//
// The two are a deliberate specialization pair, differing ONLY in how an element
// maps to its wire value. Sharing one body and branching on the mapping was
// measured 6-7 % slower whichever side of the inner loop the branch sat on.
// Everything after that mapping is the same unrolled writer as putUvarint
// (varint.go), which stays the single-value entry point for headers, counts and
// scalars. All three copies must encode identically; TestVarintWritersAgree
// holds them to that, so a change made to one and not the others fails the suite
// rather than corrupting the wire.
func putUvarintRun[T Unsigned](e *Encoder, a []T) error {
	i := 0
	for i < len(a) {
		if !e.room(maxVarintLen) {
			return e.err
		}
		buf := e.buf
		w := buf[e.n:]
		for i < len(a) && len(w) >= maxVarintLen {
			x := uint64(a[i])
			i++
			if x < 0x80 {
				w[0] = byte(x)
				w = w[1:]
				continue
			}
			w[0] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[1] = byte(x)
				w = w[2:]
				continue
			}
			w[1] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[2] = byte(x)
				w = w[3:]
				continue
			}
			w[2] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[3] = byte(x)
				w = w[4:]
				continue
			}
			w[3] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[4] = byte(x)
				w = w[5:]
				continue
			}
			w[4] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[5] = byte(x)
				w = w[6:]
				continue
			}
			w[5] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[6] = byte(x)
				w = w[7:]
				continue
			}
			w[6] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[7] = byte(x)
				w = w[8:]
				continue
			}
			w[7] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[8] = byte(x)
				w = w[9:]
				continue
			}
			w[8] = byte(x) | 0x80
			x >>= 7
			w[9] = byte(x)
			w = w[10:]
		}
		e.n = len(buf) - len(w)
	}
	return e.err
}

// putZigzagRun is putUvarintRun for signed elements (zigzag, §4.2). See there.
func putZigzagRun[T Signed](e *Encoder, a []T) error {
	i := 0
	for i < len(a) {
		if !e.room(maxVarintLen) {
			return e.err
		}
		buf := e.buf
		w := buf[e.n:]
		for i < len(a) && len(w) >= maxVarintLen {
			x := zigzagEncode(int64(a[i]))
			i++
			if x < 0x80 {
				w[0] = byte(x)
				w = w[1:]
				continue
			}
			w[0] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[1] = byte(x)
				w = w[2:]
				continue
			}
			w[1] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[2] = byte(x)
				w = w[3:]
				continue
			}
			w[2] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[3] = byte(x)
				w = w[4:]
				continue
			}
			w[3] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[4] = byte(x)
				w = w[5:]
				continue
			}
			w[4] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[5] = byte(x)
				w = w[6:]
				continue
			}
			w[5] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[6] = byte(x)
				w = w[7:]
				continue
			}
			w[6] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[7] = byte(x)
				w = w[8:]
				continue
			}
			w[7] = byte(x) | 0x80
			x >>= 7
			if x < 0x80 {
				w[8] = byte(x)
				w = w[9:]
				continue
			}
			w[8] = byte(x) | 0x80
			x >>= 7
			w[9] = byte(x)
			w = w[10:]
		}
		e.n = len(buf) - len(w)
	}
	return e.err
}

// WriteFloat32Array writes an array of 32-bit floats. An empty array is valid
// and emits exactly [header][count=0][fixlen_word] — the fixlen_word is always
// present (even when empty) so the element subtype is never ambiguous (§4.8).
func (e *Encoder) WriteFloat32Array(id ID, a []float32) error {
	if !e.writeHeaderRoom(id, TypeFixlenArray, 2*maxVarintLen) {
		return e.err
	}
	e.putVarintFit(uint64(len(a)))
	e.putVarintFit((4 << 3) | fixFp32)
	// Same advancing-window shape as the varint runs: carrying the width test
	// in the loop condition both proves the store in bounds and serves as the
	// refill test, so no per-element slice is cut from the window.
	i := 0
	for i < len(a) {
		if !e.room(4) {
			return e.err
		}
		buf := e.buf
		w := buf[e.n:]
		for i < len(a) && len(w) >= 4 {
			binary.LittleEndian.PutUint32(w, math.Float32bits(a[i]))
			w = w[4:]
			i++
		}
		e.n = len(buf) - len(w)
	}
	return e.err
}

// WriteFloat64Array writes an array of 64-bit floats. An empty array is valid
// and emits exactly [header][count=0][fixlen_word] — the fixlen_word is always
// present (even when empty) so the element subtype is never ambiguous (§4.8).
func (e *Encoder) WriteFloat64Array(id ID, a []float64) error {
	if !e.writeHeaderRoom(id, TypeFixlenArray, 2*maxVarintLen) {
		return e.err
	}
	e.putVarintFit(uint64(len(a)))
	e.putVarintFit((8 << 3) | fixFp64)
	// Same advancing-window shape as the varint runs: carrying the width test
	// in the loop condition both proves the store in bounds and serves as the
	// refill test, so no per-element slice is cut from the window.
	i := 0
	for i < len(a) {
		if !e.room(8) {
			return e.err
		}
		buf := e.buf
		w := buf[e.n:]
		for i < len(a) && len(w) >= 8 {
			binary.LittleEndian.PutUint64(w, math.Float64bits(a[i]))
			w = w[8:]
			i++
		}
		e.n = len(buf) - len(w)
	}
	return e.err
}
