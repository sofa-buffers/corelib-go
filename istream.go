package sofab

import (
	"encoding/binary"
	"io"
	"math"
)

// The decoder: one resumable push state machine, and nothing else.
//
// CORELIB_PLAN §6.0 names the operation this file exists for — "feed(bytes)
// accepting arbitrarily small chunks, returning COMPLETE / INCOMPLETE / INVALID
// (§5.2). No finish/finalize step" — and §5.2 names the model: "push-feed /
// pull-read: the caller feeds raw bytes in arbitrarily small chunks. A header or
// payload may be split across many feed calls; the state machine suspends and
// resumes at ANY byte boundary."
//
// It is one machine because §5.3.1 allows exactly one decode surface. Every
// entry point below is a wrapper over Feed and shares its state, its checks and
// its verdicts: AcceptBytes is a single Feed of a whole buffer (§6.7.1 gives the
// one-shot path no exemption), and FeedFrom is a read loop over the caller's own
// scratch buffer. A second implementation of the same rules is the defect
// §5.3.1 was written about, and this package used to carry three.
//
// It allocates NOTHING after construction (§6.6). Every piece of state below is
// sized from a constant of the specification — the parse stack from MAX_DEPTH,
// the scalar landing zone from the widest fixlen element — and sized once, in
// init. No buffer, no accumulator and no destination is sized from a wire count
// or a wire length, anywhere: what a value needs storage for is handed to the
// visitor in pieces (§6.6.3) and the visitor's own destination takes it.

// Outcome is the three-valued decode outcome of CORELIB_PLAN §5.2.1, describing
// the bytes consumed SO FAR. It is a property of the decoder's own state at any
// byte boundary, which is why there is no finish or finalize step (§5.2.4): the
// value Feed returns IS the answer, and whether an Incomplete is acceptable is
// the caller's decision, taken from its own framing.
type Outcome uint8

const (
	// Incomplete: the bytes end INSIDE a field (or inside an open sequence). The
	// partial tail is retained and the next Feed resumes on it. This is not an
	// error: a streaming caller reads it as "feed me the next chunk", a caller
	// that has already delivered everything reads it as truncation.
	Incomplete Outcome = iota
	// Complete: the bytes end EXACTLY at a top-level field boundary, so a valid
	// message may end here. More valid fields may still extend it.
	Complete
	// Invalid: the bytes are malformed regardless of what follows (§5.2.2), or
	// the visitor rejected them. Terminal — every later Feed returns it again
	// with the same error, so a caller that keeps feeding cannot talk the
	// machine back into a verdict it has already reached (§5.2.3).
	Invalid
)

// String renders the outcome as the specification spells it.
func (o Outcome) String() string {
	switch o {
	case Complete:
		return "COMPLETE"
	case Incomplete:
		return "INCOMPLETE"
	default:
		return "INVALID"
	}
}

// step is where the machine suspended. Every state below can be entered at any
// byte boundary and resumed from the next chunk.
type step uint8

const (
	stHeader   step = iota // between fields: reading a field header varint
	stScalarU              // an unsigned scalar's varint
	stScalarS              // a signed scalar's varint
	stFixWord              // a fixlen field's length-and-subtype word
	stFixFp                // an fp32/fp64 scalar's 4 or 8 raw bytes
	stFixBytes             // a string/blob payload, delivered piece by piece
	stArrCount             // an array's element count varint
	stArrUElem             // one unsigned array element's varint
	stArrSElem             // one signed array element's varint
	stArrWord              // a fixlen array's fixlen_word
	stArrFp                // one fp32/fp64 array element's raw bytes
)

// Decoder is the push decoder: bind a Visitor at construction, hand it bytes
// with Feed, and it calls one method per decoded field.
//
// Construct it ONCE and reuse it. Construction is the only allocating step
// (§6.6): the parse stack below is sized to MAX_DEPTH here and never grows, and
// Feed itself allocates nothing whatever the message says. Reset rebinds a
// visitor for the next message without allocating again.
//
// It is not safe for concurrent use, and one Decoder decodes one stream at a
// time.
type Decoder struct {
	lim limits

	st  step
	err error // the terminal INVALID latch (§5.2.3), nil while healthy

	// The varint accumulator: a partial varint is bounded working state (§6.6.2)
	// and the reason a header, a count or a scalar may be split across any
	// number of feeds.
	acc   uint64
	shift uint
	nb    int // bytes of the current varint consumed so far

	id ID // the field being decoded

	// The current fixlen payload or array run.
	total  uint64   // the payload's declared byte length / the array's count
	off    uint64   // payload bytes delivered so far
	remain uint64   // payload bytes, or elements, still to come
	idx    int      // element index within the current array
	wire   WireType // the array wire type being read (§4.8.1 defers the kind)
	kind   ArrayKind
	sub    FixlenSubtype
	width  int // 4 or 8, for a float scalar or a fixlen array element

	// fp is the landing zone §6.6.2 names: a float split across a chunk
	// boundary is assembled here so the visitor is handed it exactly once,
	// complete. Sized from the widest fixlen element, never from the wire.
	fp  [8]byte
	nfp int

	// The parse stack. Go takes the child-handler shape of §6.0, so the visitor
	// of every open scope has to be remembered until that scope closes — one
	// slot per open sequence, MAX_DEPTH of them, sized here and never grown.
	depth  int
	stack  [MaxDepth + 1]Visitor
	spDone [MaxDepth + 1]bool // this scope's StringCheck has been delivered

	// skipFrom is the depth at which the subtree the visitor declined was
	// opened, or -1 when nothing is being skipped. While skipping, no callback
	// fires and the receiver caps stand down — they bound what this consumer is
	// handed, and it is handed nothing (§6.2.1).
	skipFrom int
}

// NewDecoder returns a decoder that reports fields to v.
//
// Optional decode limits (WithMaxArrayCount, WithMaxStringLen, WithMaxBlobLen)
// and the WithStrictUTF8 policy apply to every message it decodes; with none, no
// caps are enforced.
func NewDecoder(v Visitor, opts ...Option) *Decoder {
	d := &Decoder{}
	d.init(v, newLimits(opts))
	return d
}

// init binds v and puts the machine at the start of a message. It is the whole
// of construction, and the whole of Reset.
func (d *Decoder) init(v Visitor, lim limits) {
	d.lim = lim
	d.st = stHeader
	d.err = nil
	d.acc, d.shift, d.nb = 0, 0, 0
	d.total, d.off, d.remain, d.idx, d.nfp = 0, 0, 0, 0, 0
	d.depth = 0
	d.stack[0] = v
	d.spDone[0] = false
	d.skipFrom = -1
}

// Reset rebinds the decoder to v and discards every trace of the message before
// it — the parse stack, any partial varint, any partial payload and the INVALID
// latch. It allocates nothing, which is what makes decoding many messages
// through one Decoder the §6.6 shape: construct once, Reset per message.
func (d *Decoder) Reset(v Visitor) { d.init(v, d.lim) }

// Status reports the outcome the last Feed returned, without feeding anything.
// It is not a finalize step (§5.2.4) — it reclassifies nothing, it only answers
// the same question again.
func (d *Decoder) Status() Outcome {
	if d.err != nil {
		return Invalid
	}
	if d.st == stHeader && d.nb == 0 && d.depth == 0 {
		return Complete
	}
	return Incomplete
}

// Err reports the reason a decode is Invalid, or nil.
func (d *Decoder) Err() error { return d.err }

// Feed hands the decoder the next chunk of the stream and returns the outcome
// for every byte consumed so far (§5.2.1). A chunk may be of any size, one byte
// included, and a field header, a varint, a payload or an array may be split
// across any number of calls.
//
// The error is non-nil exactly when the outcome is Invalid, and names why:
// ErrInvalidMsg for malformed bytes (§5.2.2), ErrLimitExceeded for a receiver
// cap (§6.2.1 — a policy category of its own, deliberately not folded into
// malformed), or the visitor's own error verbatim.
//
// chunk is borrowed only for the duration of this call (§6.0): once Feed
// returns, the caller may reuse or overwrite it and the decode is unaffected.
// Nothing is retained — a payload the visitor wants to keep, it copies inside
// its callback.
func (d *Decoder) Feed(chunk []byte) (Outcome, error) {
	if d.err != nil {
		return Invalid, d.err
	}
	if err := d.run(chunk); err != nil {
		return Invalid, err
	}
	return d.Status(), nil
}

// FeedFrom drains r into the decoder, feeding it in chunks of scratch, and
// returns the outcome after the reader reported EOF.
//
// It is a WRAPPER over Feed, not a second decode surface (§5.3.1): it holds no
// state of its own, applies no rule of its own, and produces the same events
// the same Feed calls would. The chunk buffer is the CALLER's, because §6.6
// leaves input storage to the caller — this package sizes no buffer from a
// stream.
//
// A non-EOF reader error surfaces verbatim, with the outcome the bytes read so
// far had reached.
func (d *Decoder) FeedFrom(r io.Reader, scratch []byte) (Outcome, error) {
	if len(scratch) == 0 {
		return d.Status(), ErrArgument
	}
	for {
		n, rerr := r.Read(scratch[:])
		if n > 0 {
			out, err := d.Feed(scratch[:n])
			if err != nil {
				return out, err
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return d.Status(), nil
			}
			return d.Status(), rerr
		}
	}
}

// AcceptBytes decodes a complete message held in one contiguous buffer,
// reporting each field to v. It is the one-shot convenience of §6.1 and is a
// SINGLE Feed of the whole buffer — the same machine, the same checks, the same
// callbacks, in the same pieces. §6.7.1 is explicit that the one-shot path gets
// no memory exemption, and it gets none here: buf is borrowed for the call and
// the payload windows handed to v are windows into it.
//
// It returns nil for COMPLETE, ErrIncomplete for INCOMPLETE (the buffer ended
// inside a field or inside an open sequence), and the INVALID reason otherwise.
// An empty buffer is the valid empty message: nil, with no callback at all.
//
// A caller decoding many messages should build one Decoder and Reset it instead:
// AcceptBytes constructs a decoder per call, which is the only allocation on
// either path.
func AcceptBytes(buf []byte, v Visitor, opts ...Option) error {
	var d Decoder
	d.init(v, newLimits(opts))
	out, err := d.Feed(buf)
	switch out {
	case Complete:
		return nil
	case Incomplete:
		return ErrIncomplete
	}
	return err
}

// fail latches the terminal verdict and returns it. INVALID is terminal (§5.2.3)
// so the latch outlives the call: a caller that ignores the error and feeds on
// gets the same verdict back rather than a decode resumed mid-message.
func (d *Decoder) fail(err error) error {
	if d.err == nil {
		d.err = err
	}
	return d.err
}

// cur is the visitor fields are being reported to, or nil while a declined
// subtree is being consumed.
func (d *Decoder) cur() Visitor {
	if d.skipFrom >= 0 {
		return nil
	}
	return d.stack[d.depth]
}

// beginVarint arms the accumulator for the next varint.
func (d *Decoder) beginVarint() { d.acc, d.shift, d.nb = 0, 0, 0 }

// varint consumes bytes of one varint from in[i:], returning the position it
// reached and whether the varint completed. An incomplete varint leaves its
// bytes in the accumulator for the next chunk and carries no verdict at all
// (§4.1.1) — it is not a construct the decoder has read.
//
// The fast path is the whole reason the split exists: with the accumulator empty
// and at least maxVarintLen bytes in hand, the unrolled shared decoder
// (varint.go) reads the varint with no per-byte end test, which is what a
// contiguous AcceptBytes hits for every header, scalar and element in the
// message. Only the last few bytes of a chunk fall into the resumable loop.
func (d *Decoder) varint(in []byte, i int) (int, bool) {
	if d.nb == 0 && len(in)-i >= maxVarintLen {
		v, np, ok := uvarintFast(in, i)
		if !ok {
			d.fail(ErrInvalidMsg) // > 64 bits: malformed, never truncated
			return i, false
		}
		d.acc, d.nb = v, np-i
		return np, true
	}
	return d.varintResume(in, i)
}

// varintResume is the byte-at-a-time half of varint: the tail of a chunk, and
// every varint that a chunk boundary splits. Value and overflow semantics are
// uvarintFast's exactly (varint.go) — the tenth byte carries one payload bit and
// must terminate, and anything else is the >64-bit malformed case.
//
//go:noinline
func (d *Decoder) varintResume(in []byte, i int) (int, bool) {
	for ; i < len(in); i++ {
		x := uint64(in[i])
		if d.shift == 63 {
			if x > 1 {
				d.fail(ErrInvalidMsg)
				return i, false
			}
			d.acc |= x << 63
			d.nb++
			return i + 1, true
		}
		d.acc |= (x & 0x7F) << d.shift
		d.nb++
		if x < 0x80 {
			return i + 1, true
		}
		d.shift += 7
	}
	return i, false
}

// run is the state machine. It consumes as much of in as it can and suspends
// wherever the bytes run out.
func (d *Decoder) run(in []byte) error {
	i := 0
	for {
		switch d.st {
		case stHeader:
			np, ok := d.varint(in, i)
			i = np
			if d.err != nil {
				return d.err
			}
			if !ok {
				return nil // suspended mid-header, or exactly out of bytes
			}
			h := d.acc
			d.beginVarint()
			// The id ceiling binds every header without exception, the
			// sequence-end marker included: its id is discarded (§4.9) but
			// discarded is not unvalidated, so an id above ID_MAX is INVALID here
			// as anywhere else (§5.2, §6.2).
			if h>>3 > uint64(IDMax) {
				return d.fail(ErrInvalidMsg)
			}
			d.id = ID(h >> 3)
			if err := d.header(WireType(h & 0x07)); err != nil {
				return err
			}

		case stScalarU:
			np, ok := d.varint(in, i)
			i = np
			if d.err != nil {
				return d.err
			}
			if !ok {
				return nil
			}
			v := d.acc
			d.beginVarint()
			d.st = stHeader
			if cur := d.cur(); cur != nil {
				if err := cur.Unsigned(d.id, v); err != nil {
					return d.fail(err)
				}
			}

		case stScalarS:
			np, ok := d.varint(in, i)
			i = np
			if d.err != nil {
				return d.err
			}
			if !ok {
				return nil
			}
			v := zigzagDecode(d.acc)
			d.beginVarint()
			d.st = stHeader
			if cur := d.cur(); cur != nil {
				if err := cur.Signed(d.id, v); err != nil {
					return d.fail(err)
				}
			}

		case stFixWord:
			np, ok := d.varint(in, i)
			i = np
			if d.err != nil {
				return d.err
			}
			if !ok {
				return nil
			}
			w := d.acc
			d.beginVarint()
			if err := d.fixlenWord(w); err != nil {
				return err
			}

		case stFixFp:
			var v uint64
			np, ok := d.fpStep(in, i, &v)
			i = np
			if !ok {
				return nil
			}
			d.st = stHeader
			if cur := d.cur(); cur != nil {
				if err := d.deliverFloat(cur, v); err != nil {
					return d.fail(err)
				}
			}

		case stFixBytes:
			n := uint64(len(in) - i)
			if n == 0 {
				return nil
			}
			if n > d.remain {
				n = d.remain
			}
			piece := in[i : i+int(n)]
			i += int(n)
			off := d.off
			d.off += n
			d.remain -= n
			if d.remain == 0 {
				d.st = stHeader
			}
			if cur := d.cur(); cur != nil {
				if err := d.deliverPiece(cur, int(off), piece); err != nil {
					return d.fail(err)
				}
			}

		case stArrCount:
			np, ok := d.varint(in, i)
			i = np
			if d.err != nil {
				return d.err
			}
			if !ok {
				return nil
			}
			n := d.acc
			d.beginVarint()
			if err := d.arrayCount(n); err != nil {
				return err
			}

		case stArrUElem, stArrSElem:
			np, err := d.arrayElements(in, i)
			i = np
			if err != nil {
				return err
			}
			if d.st == stArrUElem || d.st == stArrSElem {
				return nil // suspended mid-element
			}

		case stArrWord:
			np, ok := d.varint(in, i)
			i = np
			if d.err != nil {
				return d.err
			}
			if !ok {
				return nil
			}
			w := d.acc
			d.beginVarint()
			if err := d.fixlenArrayWord(w); err != nil {
				return err
			}

		case stArrFp:
			np, err := d.arrayFloats(in, i)
			i = np
			if err != nil {
				return err
			}
			if d.st == stArrFp {
				return nil // suspended mid-element
			}
		}
	}
}

// arrayElements decodes an integer array's remaining elements out of in[i:],
// staying in ONE loop for the whole run: the visitor is resolved once and the
// outer switch is not re-entered per element.
//
// Two loops, and the first is the reason the split pays. While a declined
// subtree is being consumed nothing is delivered, so the elements only have to
// be PARSED — no callback, no bookkeeping, just the shared unrolled varint
// decoder stepping over them.
func (d *Decoder) arrayElements(in []byte, i int) (int, error) {
	signed := d.st == stArrSElem
	cur := d.cur()
	if cur == nil {
		for d.remain > 0 && d.nb == 0 && len(in)-i >= maxVarintLen {
			_, np, ok := uvarintFast(in, i)
			if !ok {
				return i, d.fail(ErrInvalidMsg)
			}
			i, d.remain = np, d.remain-1
		}
	}
	for d.remain > 0 {
		np, ok := d.varint(in, i)
		i = np
		if d.err != nil {
			return i, d.err
		}
		if !ok {
			return i, nil // suspended mid-element; d.st is unchanged
		}
		v := d.acc
		d.beginVarint()
		idx := d.idx
		d.idx++
		d.remain--
		if cur == nil {
			continue
		}
		var err error
		if signed {
			err = cur.ArraySigned(d.id, idx, zigzagDecode(v))
		} else {
			err = cur.ArrayUnsigned(d.id, idx, v)
		}
		if err != nil {
			return i, d.fail(err)
		}
	}
	d.st = stHeader
	if cur != nil {
		if err := cur.ArrayEnd(d.id); err != nil {
			return i, d.fail(err)
		}
	}
	return i, nil
}

// arrayFloats is arrayElements for a fixlen array: fixed-width elements, so a
// declined run is a length jump and a delivered one reads straight out of the
// caller's chunk until the last few bytes of it.
func (d *Decoder) arrayFloats(in []byte, i int) (int, error) {
	cur := d.cur()
	if cur == nil && d.nfp == 0 {
		avail := uint64(len(in)-i) / uint64(d.width)
		if avail > d.remain {
			avail = d.remain
		}
		i += int(avail) * d.width
		d.remain -= avail
	}
	for d.remain > 0 {
		var bits uint64
		np, ok := d.fpStep(in, i, &bits)
		i = np
		if !ok {
			return i, nil
		}
		idx := d.idx
		d.idx++
		d.remain--
		if cur == nil {
			continue
		}
		if err := d.deliverArrayFloat(cur, idx, bits); err != nil {
			return i, d.fail(err)
		}
	}
	d.st = stHeader
	if cur != nil {
		if err := cur.ArrayEnd(d.id); err != nil {
			return i, d.fail(err)
		}
	}
	return i, nil
}

// header routes a decoded field header to the state that reads its value.
func (d *Decoder) header(t WireType) error {
	switch t {
	case TypeVarintUnsigned:
		d.st = stScalarU
	case TypeVarintSigned:
		d.st = stScalarS
	case TypeFixlen:
		d.st = stFixWord
	case TypeVarintArrayUnsigned:
		d.wire, d.kind = t, ArrayUnsigned
		d.st = stArrCount
	case TypeVarintArraySigned:
		d.wire, d.kind = t, ArraySigned
		d.st = stArrCount
	case TypeFixlenArray:
		// The kind stays undecided until the fixlen_word (§4.8.1).
		d.wire = t
		d.st = stArrCount
	case TypeSequenceStart:
		return d.openSequence()
	case TypeSequenceEnd:
		return d.closeSequence()
	default:
		return d.fail(ErrInvalidMsg)
	}
	return nil
}

// openSequence descends one level, asking the current visitor for the child that
// receives the nested scope. A nil child declines the subtree: everything under
// it is parsed and nothing in it is delivered, and no EndSequence fires for a
// scope the consumer never took.
func (d *Decoder) openSequence() error {
	if d.depth >= MaxDepth {
		return d.fail(ErrInvalidMsg) // nesting past MAX_DEPTH (§4.9)
	}
	var child Visitor
	if cur := d.cur(); cur != nil {
		c, err := cur.BeginSequence(d.id)
		if err != nil {
			return d.fail(err)
		}
		if c == nil {
			d.skipFrom = d.depth // this scope's contents are declined
		}
		child = c
	}
	d.depth++
	d.stack[d.depth] = child
	d.spDone[d.depth] = false
	return nil
}

// closeSequence returns to the enclosing scope, finalizing the child that owned
// this one. A sequence-end with nothing open is a dangling marker (§4.9).
func (d *Decoder) closeSequence() error {
	if d.depth == 0 {
		return d.fail(ErrInvalidMsg)
	}
	child := d.stack[d.depth]
	d.stack[d.depth] = nil // drop the reference: the codec keeps nothing
	d.depth--
	if d.skipFrom >= 0 {
		if d.depth == d.skipFrom {
			d.skipFrom = -1 // the declined subtree ends here
		}
		return nil
	}
	if child == nil {
		return nil
	}
	if err := child.EndSequence(); err != nil {
		return d.fail(err)
	}
	return nil
}

// fixlenWord validates a fixlen field's length-and-subtype word and announces
// the field, before any payload byte is consumed. §5.2.3 requires exactly that:
// a construct is validated where its describing bytes are read, so a message
// truncated after the word is judged on the word.
func (d *Decoder) fixlenWord(w uint64) error {
	n := w >> 3
	sub := FixlenSubtype(w & 0x07)
	if n > arrayMax {
		return d.fail(ErrInvalidMsg)
	}
	switch sub {
	case FixlenFp32:
		if n != 4 {
			return d.fail(ErrInvalidMsg)
		}
		d.width, d.remain, d.nfp = 4, 4, 0
		d.st = stFixFp
	case FixlenFp64:
		if n != 8 {
			return d.fail(ErrInvalidMsg)
		}
		d.width, d.remain, d.nfp = 8, 8, 0
		d.st = stFixFp
	case FixlenStr, FixlenBlob:
		if d.skipFrom < 0 {
			if err := d.lim.checkFixlen(uint64(sub), n, d.id, schemaBound{v: d.stack[d.depth]}); err != nil {
				return d.fail(err)
			}
		}
		d.total, d.off, d.remain = n, 0, n
		d.st = stFixBytes
	default:
		return d.fail(ErrInvalidMsg) // reserved subtype 0x4-0x7 (§4.6)
	}
	d.sub = sub
	// An empty string or blob has no payload byte for the loop to consume, so
	// the field ends HERE. That has to be settled before the skip check below,
	// or a declined empty payload would leave the machine waiting in stFixBytes
	// for a byte that is never coming — and report INCOMPLETE for a message that
	// is complete.
	empty := d.st == stFixBytes && d.remain == 0
	if empty {
		d.st = stHeader
	}
	cur := d.cur()
	if cur == nil {
		return nil
	}
	// A string field is where this scope's SOFAB_STRICT_UTF8 policy has to have
	// arrived, since the destination is what validates (§6.4): hand it over
	// once per scope, at the first string, and only to a visitor that takes one.
	if sub == FixlenStr && !d.spDone[d.depth] {
		d.spDone[d.depth] = true
		if sp, ok := cur.(StringPolicyVisitor); ok {
			sp.SetStringCheck(d.lim.stringCheck())
		}
	}
	if err := cur.FixlenBegin(d.id, sub, int(n)); err != nil {
		return d.fail(err)
	}
	// Its single piece is delivered here instead, so every payload reaches the
	// destination through at least one call, total == 0 included.
	if empty {
		if err := d.deliverPiece(cur, 0, in0[:]); err != nil {
			return d.fail(err)
		}
	}
	return nil
}

// in0 is the empty payload piece. A package-level zero-length array, so
// delivering it neither allocates nor aliases anything.
var in0 [0]byte

// deliverPiece hands one piece of the current string/blob payload to cur.
func (d *Decoder) deliverPiece(cur Visitor, off int, piece []byte) error {
	if d.sub == FixlenStr {
		return cur.String(d.id, int(d.total), off, piece)
	}
	return cur.Bytes(d.id, int(d.total), off, piece)
}

// deliverFloat hands over a completed fp32/fp64 scalar, bit-exact (§6.5): the
// wire bits are moved into the float without arithmetic, so a signaling NaN
// survives.
func (d *Decoder) deliverFloat(cur Visitor, bits uint64) error {
	if d.width == 4 {
		return cur.Float32(d.id, math.Float32frombits(uint32(bits)))
	}
	return cur.Float64(d.id, math.Float64frombits(bits))
}

// deliverArrayFloat is deliverFloat for one element of a fixlen array.
func (d *Decoder) deliverArrayFloat(cur Visitor, idx int, bits uint64) error {
	if d.width == 4 {
		return cur.ArrayFloat32(d.id, idx, math.Float32frombits(uint32(bits)))
	}
	return cur.ArrayFloat64(d.id, idx, math.Float64frombits(bits))
}

// fpStep assembles one fixed-width float from in[i:], across as many chunks as
// it takes. The whole-width case is the common one and reads straight out of the
// caller's chunk; only a float a chunk boundary splits touches the landing zone.
func (d *Decoder) fpStep(in []byte, i int, out *uint64) (int, bool) {
	w := d.width
	if d.nfp == 0 && len(in)-i >= w {
		if w == 4 {
			*out = uint64(binary.LittleEndian.Uint32(in[i : i+4]))
		} else {
			*out = binary.LittleEndian.Uint64(in[i : i+8])
		}
		return i + w, true
	}
	for d.nfp < w && i < len(in) {
		d.fp[d.nfp] = in[i]
		d.nfp++
		i++
	}
	if d.nfp < w {
		return i, false
	}
	if w == 4 {
		*out = uint64(binary.LittleEndian.Uint32(d.fp[:4]))
	} else {
		*out = binary.LittleEndian.Uint64(d.fp[:8])
	}
	d.nfp = 0
	return i, true
}

// arrayCount validates an array's leading element count and, for an integer
// array, announces the array. Zero is valid — an empty array (§4.7/§4.8); only
// a count past ARRAY_MAX is malformed.
//
// A fixlen array is NOT announced here: §4.8.1 puts its fixlen_word next, and
// only that word says what the elements are. The format ceiling and the receiver
// cap still fire here, on the count alone, because neither depends on the kind.
func (d *Decoder) arrayCount(n uint64) error {
	if n > arrayMax {
		return d.fail(ErrInvalidMsg)
	}
	if d.skipFrom < 0 {
		if err := d.lim.checkArrayCount(n, d.id, schemaBound{v: d.stack[d.depth]}); err != nil {
			return d.fail(err)
		}
	}
	d.total, d.remain, d.idx = n, n, 0
	switch d.wire {
	case TypeFixlenArray:
		d.st = stArrWord
		return nil
	case TypeVarintArrayUnsigned:
		d.st = stArrUElem
	default:
		d.st = stArrSElem
	}
	return d.announceArray()
}

// announceArray fires ArrayBegin, and ArrayEnd straight after it when the array
// is empty: an empty array reports its kind like any other and is followed by no
// element at all.
func (d *Decoder) announceArray() error {
	cur := d.cur()
	if cur == nil {
		if d.remain == 0 {
			d.st = stHeader
		}
		return nil
	}
	if err := cur.ArrayBegin(d.id, d.kind, int(d.total)); err != nil {
		return d.fail(err)
	}
	if d.remain == 0 {
		d.st = stHeader
		if err := cur.ArrayEnd(d.id); err != nil {
			return d.fail(err)
		}
	}
	return nil
}

// fixlenArrayWord validates a fixlen array's element word and announces the
// array with the kind that word names.
//
// The word is a FORMAT matter first: §4.8 admits only fp32/4 and fp64/8 as
// fixlen-array elements, so a string or blob subtype, or a width mismatch, is
// malformed regardless of the schema — INVALID here, and never routed to the
// §7.3 skip path even though its subtype also contradicts whatever was declared.
func (d *Decoder) fixlenArrayWord(w uint64) error {
	sub := w & 0x07
	size := w >> 3
	switch {
	case sub == uint64(FixlenFp32) && size == 4:
		d.kind, d.width = ArrayFp32, 4
	case sub == uint64(FixlenFp64) && size == 8:
		d.kind, d.width = ArrayFp64, 8
	default:
		return d.fail(ErrInvalidMsg)
	}
	d.nfp = 0
	d.st = stArrFp
	return d.announceArray()
}
