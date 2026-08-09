package sofab

import (
	"encoding/binary"
	"io"
	"math"
	"unicode/utf8"
)

// Encoder writes Sofab fields into an output buffer, draining it downstream as
// it fills. It advances over a contiguous buffer instead of pushing each byte
// through a bufio.Writer interface call. Errors are sticky: once a write fails,
// subsequent writes are no-ops and the same error is returned, so a generated
// Serialize can issue a run of writes and check only the final Flush.
//
// There are three ways to give it somewhere to write (CORELIB_PLAN §5.1):
//
//   - NewEncoderBuffer — a caller-supplied buffer with no sink. It either holds
//     the message or reports ErrBufferFull; Bytes returns what was written.
//   - NewEncoderSink — a caller-supplied buffer plus a flush sink, handed the
//     accumulated bytes whenever the buffer fills. The sink may install a
//     replacement buffer with SetBuffer (the zero-copy handover).
//   - NewEncoder — an io.Writer, the idiomatic Go convenience form. It is the
//     only one that allocates a buffer of its own.
//
// An Encoder is not safe for concurrent use, and a sink must not write fields
// through the Encoder it is called from.
type Encoder struct {
	w    io.Writer // io.Writer form only; nil for a caller-supplied buffer
	sink Sink      // caller-supplied buffer with a sink; nil otherwise
	// buf is a fixed window, not an append target: len(buf) is its capacity and
	// n is how much of it is used. Appending byte-by-byte costs a length/capacity
	// test, a possible growslice call, and a three-word slice-header store per
	// byte. Writing through an index instead makes the hot path a store plus an
	// integer bump, with one capacity test per field rather than per byte, and
	// never rewrites the slice header (so no write-barrier check either).
	//
	// A caller-supplied buffer is never grown or reallocated (§5.1): it drains to
	// the sink, or reports ErrBufferFull when there is none. The window of the
	// io.Writer form (owned) grows once, from encBufInit straight to
	// flushThreshold, and then stops: past that it is drained to w instead, which
	// is what keeps memory bounded and preserves mid-stream flushing.
	buf []byte
	n   int
	// start is where the bytes accumulated since the current installation begin:
	// the offset that installation was given, and 0 after the first flush,
	// because the start offset belongs to the installation and is consumed by it
	// (§5.1). A flush hands the sink buf[start:n].
	start int
	err   error
	depth int
	// owned marks buf as this package's own window (the io.Writer form), the one
	// buffer an Encoder may grow. A caller-supplied buffer never is.
	owned bool
	// passThrough is the caller's permission to hand a string/blob run to the
	// sink directly instead of copying it through the output buffer (§5.1). It is
	// off for every encoder form unless WithPassThrough(true) granted it: a
	// destination that was not told it may receive foreign memory never does.
	passThrough bool
	// installed records that the sink called SetBuffer during the flush it is
	// currently in — i.e. that it took the buffer and replaced it, rather than
	// copying and returning (§5.1). drain clears it before every sink call.
	installed bool
	// staged is the exact-fit tail path of a sink-less caller-supplied buffer (see
	// stage), nil unless the encoder has entered it. It hangs off a pointer rather
	// than sitting in the struct because it is dead weight for every other form:
	// the io.Writer window and the sink can never enter it, yet its ~60 bytes made
	// every Encoder that much larger to allocate, zero and GC-scan. Entering it
	// costs one allocation, on a path that has already run out of caller buffer.
	staged *stagedTail
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

// stagedTail is the sink-less caller-supplied buffer's exact-fit tail: dst is
// the caller's buffer with the cursor it had when staging began, and scratch is
// the small window the encoder produces the last stretch into. See stage.
type stagedTail struct {
	dst     []byte
	n       int
	start   int
	scratch [MinOutputBuffer]byte
}

// lazySeqInline is how many held-back sequence ids fit in the Encoder itself
// before the pending run has to spill to the heap. Not a wire-format or API
// limit — nesting deeper still works and still produces canonical bytes, it just
// allocates once. Sized for the depth real schemas nest to, not for MaxDepth:
// every Encoder pays this array's bytes on every allocation, so a value that
// covers the realistic case and lets append() handle the rest is the cheaper
// trade.
const lazySeqInline = 8

// MinOutputBuffer is the smallest output buffer this package accepts for
// streaming (CORELIB_PLAN §5.1): buflen-offset must be at least this many bytes
// for a buffer installed together with a flush sink, at construction and at
// every mid-stream SetBuffer. Any buffer at or above it works and produces
// byte-identical output.
//
// The value follows from writeHeaderRoom: this encoder never splits an atomic
// unit, and the widest run it reserves as one piece is a field header varint
// (10 bytes at ceil(64/7)) together with the value varint of a 64-bit scalar
// (10 more). Every other reservation is smaller — a fixlen length word and an
// element count are both bounded by 2³¹−1 (§6.2) and so at most 5 bytes, and a
// float element is 4 or 8.
//
// A buffer installed WITHOUT a sink is subject to no minimum: no flush can
// occur, so nothing can be split, and a two-byte message encodes into a
// two-byte buffer. The constant is a streaming precondition, never a floor on
// the one-shot path.
const MinOutputBuffer = 2 * maxVarintLen

// Sink is a flush callback: it receives the bytes the encoder has accumulated in
// the active output buffer, in wire order, and returns an error to abort the
// encode (the error becomes the Encoder's sticky error).
//
// What the callback leaves behind decides who owns the buffer (§5.1):
//
//   - Returning without installing a buffer means the sink COPIED what it was
//     handed. The active buffer stays active and the encoder resumes writing
//     into it at offset 0.
//   - A sink that TAKES the buffer — queues it for an asynchronous write, hands
//     it to a transport — must install a replacement with e.SetBuffer before
//     returning; otherwise the encoder would keep writing into storage the
//     transport now owns.
//
// Passing the same buffer back to SetBuffer is a new installation like any
// other, which is how a sink re-arms a start offset and gets framing-header room
// in every flushed unit.
//
// With the pass-through permission granted (WithPassThrough) the callback may
// also be handed a string/blob payload that is not the output buffer at all.
// That memory is borrowed for the duration of the call and must not be retained;
// such a call is not a buffer handover, and SetBuffer is rejected under the
// permission (§5.1).
type Sink func(e *Encoder, b []byte) error

// NewEncoder returns an Encoder writing to w, allocating a small window of its
// own. It is the idiomatic Go convenience form, layered on the caller-supplied
// buffer model below; NewEncoderBuffer / NewEncoderSink are the forms in which
// the caller owns the storage (§5.1).
//
// Of the shared Options only WithStrictUTF8 and WithPassThrough affect encoding
// (the SOFAB_STRICT_UTF8 policy, §6.4, default ON); the receiver-side cap
// options (WithMaxArrayCount, WithMaxStringLen, WithMaxBlobLen) are decode-only
// and are accepted but ignored here.
//
// Pass-through of a string/blob payload larger than the window (§5.1) is OFF
// here as it is everywhere: w is a sink like any other, and §5.1 makes the
// permission the caller's to give rather than the constructor's to assume — an
// io.Writer whose Write is handed the caller's own payload is still handed
// memory it was never told to expect, even though io.Writer's contract forbids
// retaining it. Pass WithPassThrough(true) to grant it; the bytes are identical
// either way. This form is the one where granting it costs nothing extra, since
// the window is this package's and no sink can take it, so the rule that
// pass-through excludes SetBuffer takes nothing away here.
func NewEncoder(w io.Writer, opts ...Option) *Encoder {
	e := &Encoder{w: w, buf: make([]byte, encBufInit), owned: true, lim: newLimits(opts)}
	e.passThrough = e.lim.passThrough
	e.pending = e.pendingInline[:0]
	return e
}

// NewEncoderBuffer returns an Encoder writing into the caller's buffer, starting
// at offset — room the caller keeps for a framing header the encoder must not
// touch — with no flush sink (§5.1).
//
// With nowhere to drain to, the buffer either holds the whole message or the
// encode fails with ErrBufferFull; nothing is grown, reallocated, or reported as
// complete when it is not. Bytes returns what has been written so far. This is
// the shape a caller uses with a size derived from the schema (the generated
// MAX_SIZE), so no minimum applies: offset must lie within buf, and that is all.
//
// SetBuffer installs the next buffer once this one is full, after the caller has
// taken its Bytes.
func NewEncoderBuffer(buf []byte, offset int, opts ...Option) (*Encoder, error) {
	if offset < 0 || offset > len(buf) {
		return nil, ErrArgument
	}
	return newBufferEncoder(buf, offset, nil, opts), nil
}

// NewEncoderSink returns an Encoder writing into the caller's buffer, starting at
// offset, and draining it through sink whenever it fills (§5.1).
//
// The buffer is caller-owned: it is never grown or reallocated, and a message
// larger than it simply drives sink repeatedly. len(buf)-offset must be at least
// MinOutputBuffer — a buffer installed with a sink is rejected here, where it is
// handed over, rather than partway through a message — and sink must be non-nil.
//
// Pass-through (WithPassThrough(true)) is OFF by default: without it the sink is
// only ever handed memory inside the installed buffer.
func NewEncoderSink(buf []byte, offset int, sink Sink, opts ...Option) (*Encoder, error) {
	if sink == nil || offset < 0 || offset > len(buf) || len(buf)-offset < MinOutputBuffer {
		return nil, ErrArgument
	}
	return newBufferEncoder(buf, offset, sink, opts), nil
}

// newBufferEncoder is the shared tail of the two caller-supplied-buffer
// constructors, once their arguments have been checked.
func newBufferEncoder(buf []byte, offset int, sink Sink, opts []Option) *Encoder {
	e := &Encoder{sink: sink, buf: buf, n: offset, start: offset, lim: newLimits(opts)}
	e.passThrough = sink != nil && e.lim.passThrough
	e.pending = e.pendingInline[:0]
	return e
}

// SetBuffer installs a new output buffer mid-stream, starting at offset (§5.1).
// It is how a sink that took the buffer it was handed supplies a replacement
// before returning, and how a caller driving a sink-less encoder continues into
// fresh storage once it has taken the previous buffer's Bytes.
//
// Each call begins a new installation whose cursor starts at that call's offset,
// so passing the same buffer back re-arms the header room for the next flushed
// unit. Bytes already written into the previous buffer are the caller's to deal
// with — inside a sink they are exactly the ones just handed over; outside one,
// read them with Bytes first.
//
// It is rejected with ErrArgument, without installing anything, when:
// offset lies outside buf; a sink is installed and len(buf)-offset is below
// MinOutputBuffer; the encoder is the io.Writer form, whose window is not the
// caller's to replace; or the pass-through permission is granted, since a sink
// that may be handed foreign memory must never take a buffer (§5.1).
func (e *Encoder) SetBuffer(buf []byte, offset int) error {
	if e.owned || e.passThrough || offset < 0 || offset > len(buf) ||
		(e.sink != nil && len(buf)-offset < MinOutputBuffer) {
		return e.rejectArgument()
	}
	if e.err != nil {
		return e.err
	}
	e.buf, e.n, e.start = buf, offset, offset
	e.staged = nil // a fresh buffer ends any staged tail
	e.installed = true
	return nil
}

// Bytes returns the bytes written into the active output buffer since it was
// installed — the whole message for a sink-less encoder that had room for it,
// and whatever has not been flushed yet otherwise. The result aliases the
// caller's buffer and stays valid until the next write.
//
// On a sink-less encoder it first materializes anything still staged (see
// stage), so a message whose tail did not fit sets ErrBufferFull here rather
// than returning a short result silently. Check Err (or Flush) before using it.
func (e *Encoder) Bytes() []byte {
	if s := e.staged; s != nil {
		e.spill()
		return s.dst[s.start:s.n]
	}
	return e.buf[e.start:e.n]
}

// Err returns the first error encountered, if any.
func (e *Encoder) Err() error { return e.err }

// Flush drains any buffered bytes downstream — one Write to the io.Writer, or
// one call to the sink. On a sink-less encoder there is nowhere to drain to: the
// message is in the caller's buffer, where Bytes returns it, and all Flush does
// is materialize a staged tail (see stage) so a buffer that turned out too small
// is reported here too.
func (e *Encoder) Flush() error {
	if e.err != nil {
		return e.err
	}
	if e.w == nil && e.sink == nil {
		if e.staged != nil {
			e.spill()
		}
		return e.err
	}
	e.drain()
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
	if !e.owned {
		// A caller-supplied buffer is never grown or reallocated (§5.1): drain it
		// and write on into it. MinOutputBuffer is what guarantees the retry
		// succeeds for a sink-installed buffer, since k never exceeds it.
		if e.sink == nil {
			return e.stage(k)
		}
		if !e.drain() {
			return false
		}
		if len(e.buf)-e.n >= k {
			return true
		}
		e.err = ErrBufferFull
		return false
	}
	// Grow first while the window is still below the threshold: a small message
	// then accumulates entirely and leaves in a single Write on Flush, exactly
	// as the append-based buffer did.
	//
	// The growth is ONE STEP to the cap, not a doubling ladder. A message that
	// outgrows the initial window has already shown it is not a small message, and
	// each rung of the ladder cost an allocation plus a copy of everything written
	// so far — encoding a 1000-element u64 array climbed 512→1024→2048→4096 and
	// paid four of each. The cap is untouched, so the ceiling on memory is
	// untouched: the window still never exceeds flushThreshold for a message that
	// fits it, and past that it drains to w rather than growing.
	if len(e.buf) < flushThreshold && len(e.buf)-e.n < k {
		grown := make([]byte, flushThreshold)
		copy(grown, e.buf[:e.n])
		e.buf = grown
		if len(e.buf)-e.n >= k {
			return true
		}
	}
	// At the cap and still too tight: drain, which is the mid-stream flush.
	if !e.drain() {
		return false
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

// drain hands the bytes accumulated since the current installation — buf[start:n]
// — downstream, and reports whether the encoder may go on writing. It is the one
// place a flush boundary falls, so it is also where the §5.1 handover contract
// is applied.
//
// After a sink returns without installing a buffer it copied, and the encoder
// resumes in the same buffer at offset 0: the start offset belonged to the
// installation and is consumed by it. A sink that took the buffer installed a
// replacement (SetBuffer), and that installation's own offset and cursor stand.
//
//go:noinline
func (e *Encoder) drain() bool {
	if e.err != nil {
		return false
	}
	if e.n <= e.start {
		return true
	}
	b := e.buf[e.start:e.n]
	if e.w != nil {
		if _, err := e.w.Write(b); err != nil {
			e.err = err
			return false
		}
		e.start, e.n = 0, 0
		return true
	}
	if e.sink == nil {
		// A caller-supplied buffer with nowhere to drain to: full is full (§5.1).
		e.err = ErrBufferFull
		return false
	}
	e.installed = false
	if err := e.sink(e, b); err != nil {
		e.setErr(err)
		return false
	}
	if e.err != nil { // e.g. a SetBuffer the sink made and this package rejected
		return false
	}
	if !e.installed {
		e.start, e.n = 0, 0
	}
	return true
}

// stage keeps a sink-less caller-supplied buffer EXACT: the reservation a writer
// makes is an upper bound, and the bytes it goes on to write are usually far
// fewer, so running out of reserved room must not be mistaken for running out of
// buffer. §5.1 is explicit that a buffer installed without a sink is subject to
// no minimum — a message that encodes to two bytes encodes into a two-byte
// buffer — and a reservation of up to MinOutputBuffer would otherwise impose one.
//
// So the last stretch of such a buffer is produced into a small scratch inside
// the Encoder (tail, not a heap allocation, and never an output buffer in §5.1's
// sense) and copied out exactly as far as it reaches. Only the request that no
// longer fits switches over — everything before it was written straight into the
// caller's buffer, which is the point of handing one in — and the switch is
// one-way, since the room left only ever shrinks from here.
//
//go:noinline
func (e *Encoder) stage(k int) bool {
	if e.staged == nil {
		s := &stagedTail{dst: e.buf, n: e.n, start: e.start}
		e.staged = s
		e.buf, e.n, e.start = s.scratch[:], 0, 0
		return true // k <= MinOutputBuffer == len(scratch), see writeHeaderRoom
	}
	return e.spill()
}

// spill copies the staged bytes into the caller's buffer, or reports that they
// do not fit. This is where a sink-less encode runs out of room, and it does so
// on the bytes actually produced rather than on a reservation.
func (e *Encoder) spill() bool {
	s := e.staged
	if e.n > len(s.dst)-s.n {
		e.err = ErrBufferFull
		return false
	}
	s.n += copy(s.dst[s.n:], e.buf[:e.n])
	e.n = 0
	return true
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
// The fast path is the payload that fits the room left in the buffer; everything
// else — a run that has to be split across flushes, a window that has to grow,
// a payload handed to the sink whole — is out of line in putRawSlow.
func (e *Encoder) putRaw(data []byte) {
	if e.err == nil && len(data) <= len(e.buf)-e.n {
		e.n += copy(e.buf[e.n:], data)
		return
	}
	e.putRawSlow(data)
}

// putRawSlow is the out-of-line half of putRaw: a payload that does not fit the
// room left in the buffer.
//
// A string or blob payload is a DIVISIBLE run (§5.1) — a flush boundary may fall
// anywhere inside it — which is what lets it be written through a buffer
// arbitrarily smaller than itself. That is the loop at the bottom, and it is the
// only place this encoder splits anything: every other run it writes is atomic
// and lands contiguously, which is what MinOutputBuffer declares.
//
//go:noinline
func (e *Encoder) putRawSlow(data []byte) {
	if e.err != nil {
		return
	}
	if e.passThrough && len(data) > e.passThroughFloor() {
		e.putRawLarge(data)
		return
	}
	if e.owned && len(data) <= flushThreshold {
		// The window is this package's to grow, and a sub-threshold payload is
		// what it grows for: one contiguous copy rather than a split run.
		if !e.room(len(data)) {
			return
		}
		e.n += copy(e.buf[e.n:], data)
		return
	}
	for {
		c := copy(e.buf[e.n:], data)
		e.n += c
		data = data[c:]
		if len(data) == 0 {
			return
		}
		if !e.makeRoom(1) {
			return
		}
	}
}

// passThroughFloor is the payload size from which handing the run to the sink
// beats copying it through the buffer: bigger than the buffer it would have to
// cross. For the owned window that is its cap rather than its current size, so
// growing the window stays the cheaper answer up to that point.
func (e *Encoder) passThroughFloor() int {
	if e.owned {
		return flushThreshold
	}
	return len(e.buf)
}

// putRawLarge hands a payload to the sink directly instead of copying it through
// the output buffer — the pass-through of §5.1, permitted here only because the
// caller granted it. The buffered bytes are drained first, so what the sink
// receives stays in wire order.
//
// The memory is borrowed for the duration of the call: this is not a buffer
// handover, and SetBuffer is rejected while the permission is granted, so a sink
// cannot mistake the payload for a buffer it may take.
//
//go:noinline
func (e *Encoder) putRawLarge(data []byte) {
	if !e.drain() {
		return
	}
	if e.w != nil {
		if _, err := e.w.Write(data); err != nil {
			e.err = err
		}
		return
	}
	if err := e.sink(e, data); err != nil {
		e.setErr(err)
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
// into one of each.
//
// extra must not exceed MinOutputBuffer-maxVarintLen: this reservation is the
// widest run the encoder demands contiguously, so it is exactly what the
// declared minimum promises a caller-supplied buffer can hold (§5.1). A larger
// one would make the smallest workable buffer bigger than the constant says, and
// the spec caps that constant at 20. Every caller passes a small constant, and
// TestEncodeIntoMinOutputBuffer fails the moment one of them grows past it.
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

// overCeiling reports whether a caller-supplied payload length or element count
// exceeds the format-wide FIXLEN_MAX / ARRAY_MAX ceiling (§6.2) — 2³¹−1 for
// both, the same arrayMax the decoder enforces on the wire word.
//
// A field past it cannot be encoded, only mis-encoded: the length/count word
// would go out looking well-formed and every conformant decoder, this package's
// own included, would reject the message as INVALID. So the writers test it on
// the same argument branch that already tests id > IDMax — before a single byte
// is written — and the verdict is the same ErrArgument (§6.3). Where int is 32
// bits the comparison is trivially false and the compiler drops it.
func overCeiling(n int) bool { return uint64(n) > arrayMax }

// rejectArgument records a caller-argument rejection as the sticky error and
// reports it. It is out of line so a guard that calls it costs one
// well-predicted compare on the hot path and nothing else.
//
//go:noinline
func (e *Encoder) rejectArgument() error {
	e.setErr(ErrArgument)
	return e.err
}

// writeFixlen writes a fixed-length field: the header, then a length-and-subtype
// varint (len(data)<<3 | sub), then the raw bytes. sub selects float/string/blob.
// A payload past FIXLEN_MAX is refused with ErrArgument and writes nothing.
func (e *Encoder) writeFixlen(id ID, data []byte, sub uint64) {
	if overCeiling(len(data)) {
		e.rejectArgument()
		return
	}
	if e.writeHeaderRoom(id, TypeFixlen, maxVarintLen) {
		e.putVarintFit((uint64(len(data)) << 3) | sub)
		e.putRaw(data)
	}
}

// fixlenWordFloat is how many bytes a float field's fixlen word occupies: one.
// The word is (len<<3)|subtype with len 4 or 8, so (4<<3)|fixFp32 = 0x20 and
// (8<<3)|fixFp64 = 0x41 — both below 0x80, hence single-byte varints.
//
// Reserving the exact width rather than maxVarintLen is what keeps a float
// field's single reservation inside MinOutputBuffer: 10+1+8 = 19 for an fp64,
// where 10+10+8 would have demanded 28 and made the declared minimum a lie.
const fixlenWordFloat = 1

// maxCountLen is the widest varint an array's element count occupies. A count is
// bounded by ARRAY_MAX = 2³¹−1 (§6.2, enforced by overCeiling before anything is
// written), so five 7-bit groups always suffice — the same reason as
// fixlenWordFloat, and the two together keep a float array's reservation at
// 10+5+1 rather than 30.
const maxCountLen = 5

// WriteFloat32 writes a 32-bit float field.
//
// The payload goes straight into the window rather than through a stack array
// and putRaw: the array escaped to the heap (putRaw can hand a payload to the
// out-of-line sink path), so the temporary cost an allocation per float on top
// of the copy.
func (e *Encoder) WriteFloat32(id ID, f float32) error {
	if e.writeHeaderRoom(id, TypeFixlen, fixlenWordFloat+4) {
		e.putVarintFit((4 << 3) | fixFp32)
		binary.LittleEndian.PutUint32(e.buf[e.n:], math.Float32bits(f))
		e.n += 4
	}
	return e.err
}

// WriteFloat64 writes a 64-bit float field. See WriteFloat32 on the direct
// window write.
func (e *Encoder) WriteFloat64(id ID, f float64) error {
	if e.writeHeaderRoom(id, TypeFixlen, fixlenWordFloat+8) {
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
// producer-side MUST NOT of MESSAGE_SPEC §8. With it disabled —
// WithStrictUTF8(false), or a `sofab_no_strict_utf8` build, in which the
// validator is not compiled in at all — the bytes are written verbatim. utf8.ValidString
// correctly rejects overlong encodings, surrogate code points, and code points
// above U+10FFFF, while accepting an embedded NUL.
//
// A string past FIXLEN_MAX is refused with ErrArgument, and refused first: the
// ceiling is a comparison, the UTF-8 scan is a pass over the whole string, and a
// string that cannot be encoded at all need not be validated.
func (e *Encoder) WriteString(id ID, s string) error {
	if e.err != nil {
		return e.err
	}
	if overCeiling(len(s)) {
		return e.rejectArgument()
	}
	if e.lim.strictUTF8On() && !utf8.ValidString(s) {
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
// []byte(s) round trip. See putRawSlow: a string payload is a divisible run and
// is split across flushes when it does not fit.
func (e *Encoder) putRawString(s string) {
	if e.err == nil && len(s) <= len(e.buf)-e.n {
		e.n += copy(e.buf[e.n:], s)
		return
	}
	e.putRawStringSlow(s)
}

// putRawStringSlow is the out-of-line half of putRawString. See putRawSlow,
// whose structure it mirrors, with one difference: pass-through is offered only
// to the io.Writer form. Go's string already holds the UTF-8 payload, so §5.1
// permits handing it over — but a Sink takes a []byte, and converting the string
// to one is the very copy pass-through exists to avoid, so a caller-supplied
// buffer copies the payload through the buffer instead (§5.1 leaves the port
// free to: the bytes are identical either way).
//
//go:noinline
func (e *Encoder) putRawStringSlow(s string) {
	if e.err != nil {
		return
	}
	if e.w != nil && e.passThrough && len(s) > flushThreshold {
		e.putRawStringLarge(s)
		return
	}
	if e.owned && len(s) <= flushThreshold {
		if !e.room(len(s)) {
			return
		}
		e.n += copy(e.buf[e.n:], s)
		return
	}
	for {
		c := copy(e.buf[e.n:], s)
		e.n += c
		s = s[c:]
		if len(s) == 0 {
			return
		}
		if !e.makeRoom(1) {
			return
		}
	}
}

// putRawStringLarge is putRawLarge for a string. It prefers the writer's
// WriteString (bytes.Buffer, strings.Builder, bufio.Writer all have one) so an
// oversized payload still costs no []byte(s) copy.
//
//go:noinline
func (e *Encoder) putRawStringLarge(s string) {
	if !e.drain() {
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
//
// Closing when nothing is open is rejected with ErrArgument and writes no bytes:
// a bare 0x07 is an unbalanced sequence end, which every decoder must reject as
// INVALID (§4.9, §6.3). It is the closing-direction twin of the MaxDepth guard
// in WriteSequenceBeginLazy — the encoder never emits a frame it cannot balance,
// in either direction.
func (e *Encoder) WriteSequenceEnd() error {
	if e.err != nil {
		return e.err
	}
	if e.depth == 0 {
		return e.rejectArgument()
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
	if e.err == nil {
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
//
// Closing when nothing is open is rejected with ErrArgument and writes no bytes,
// exactly as in WriteSequenceEnd: the frame this one insists on emitting is
// precisely the one there is no opening for.
func (e *Encoder) WriteSequenceEndKeep() error {
	if e.err != nil {
		return e.err
	}
	if e.depth == 0 {
		return e.rejectArgument()
	}
	if len(e.pending) != 0 {
		e.commitPending()
	}
	e.writeHeader(0, TypeSequenceEnd)
	if e.err == nil {
		e.depth--
	}
	return e.err
}

// WriteUnsignedArray writes an array of unsigned integers. An empty array is
// valid and emits exactly [header][count=0] (§4.7); one longer than ARRAY_MAX is
// refused with ErrArgument and emits nothing.
func WriteUnsignedArray[T Unsigned](e *Encoder, id ID, a []T) error {
	if overCeiling(len(a)) {
		return e.rejectArgument()
	}
	if !e.writeHeaderRoom(id, TypeVarintArrayUnsigned, maxVarintLen) {
		return e.err
	}
	e.putVarintFit(uint64(len(a)))
	return putUvarintRun(e, a)
}

// WriteSignedArray writes an array of signed integers. An empty array is valid
// and emits exactly [header][count=0] (§4.7); one longer than ARRAY_MAX is
// refused with ErrArgument and emits nothing.
func WriteSignedArray[T Signed](e *Encoder, id ID, a []T) error {
	if overCeiling(len(a)) {
		return e.rejectArgument()
	}
	if !e.writeHeaderRoom(id, TypeVarintArraySigned, maxVarintLen) {
		return e.err
	}
	e.putVarintFit(uint64(len(a)))
	return putZigzagRun(e, a)
}

// putUvarintRun and putZigzagRun write an integer array's elements.
//
// The varint writer is unrolled INTO each loop rather than called per element:
// calling putUvarint costs argument setup, a call/return pair and a re-derived
// window on every element, so the bulk path pays none of it.
//
// The destination advances as a slice, and `len(w) >= maxVarintLen` is carried
// in the LOOP CONDITION. That is what makes the ten stores bounds-check-free:
// the prove pass knows w[0]..w[9] are in range, so no per-element slice is cut
// from the window. It doubles as the refill test — the inner loop simply ends
// when the window can no longer hold a full varint, and the outer loop drains
// and re-enters.
//
// The two are a deliberate specialization pair, differing ONLY in how an element
// maps to its wire value. Sharing one body and branching on the mapping is
// slower whichever side of the inner loop the branch sits on, so the mapping is
// fixed at compile time here instead.
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
// An array longer than ARRAY_MAX is refused with ErrArgument and emits nothing.
func (e *Encoder) WriteFloat32Array(id ID, a []float32) error {
	if overCeiling(len(a)) {
		return e.rejectArgument()
	}
	if !e.writeHeaderRoom(id, TypeFixlenArray, maxCountLen+fixlenWordFloat) {
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
// An array longer than ARRAY_MAX is refused with ErrArgument and emits nothing.
func (e *Encoder) WriteFloat64Array(id ID, a []float64) error {
	if overCeiling(len(a)) {
		return e.rejectArgument()
	}
	if !e.writeHeaderRoom(id, TypeFixlenArray, maxCountLen+fixlenWordFloat) {
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
