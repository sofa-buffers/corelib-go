package sofab

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
)

// Decoder decodes a Sofab byte stream read from an io.Reader into a Visitor.
//
// The VISITOR IS THE ONLY DECODE SURFACE (CORELIB_PLAN §5.3.1): "A port MUST
// NOT offer any second decode surface: no pull-parser, no iterator or
// next()-style API, no cursor, no convenience wrapper that decodes by another
// route." Accept and AcceptStream are the two ways to drive it — one over a
// slurped buffer, one straight off the reader — and they produce identical
// events for identical input. Everything below is the reader-side machinery
// they share; none of it is exported.
type Decoder struct {
	src      io.Reader     // original source; Accept slurps directly from it
	r        *bufio.Reader // reader window, created on the first AcceptStream
	lim      limits
	skipping int // declined sequences currently open (skip.go)
}

// NewDecoder returns a Decoder reading from r. The reader window AcceptStream
// needs is created on first use, so Accept — which reads the message into one
// contiguous buffer itself — does not pay for it.
//
// Optional decode limits (WithMaxArrayCount, WithMaxStringLen, WithMaxBlobLen)
// apply to both entry points; with none, no limits are enforced.
func NewDecoder(r io.Reader, opts ...Option) *Decoder {
	return &Decoder{src: r, lim: newLimits(opts)}
}

// asBufio reuses an existing *bufio.Reader source, otherwise wraps it once.
func asBufio(r io.Reader) *bufio.Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return br
	}
	return bufio.NewReader(r)
}

// readVarint reads a base-128 varint. If firstEOFok is true, an EOF before any
// byte is reported as io.EOF (a clean stream boundary); a mid-varint EOF is
// ErrIncomplete (INCOMPLETE, §7 — ran out of bytes mid-field, not malformed). A
// varint that exceeds 64 bits is ErrInvalidMsg (malformed).
//
// It decodes out of the reader's own buffer via Peek, then Discard, rather than
// pulling one byte at a time: bufio.ReadByte is a method call and a bounds test
// per byte, so a ten-byte varint cost ten of them before any decoding happened.
// Peek hands back a window into that same buffer with no copy, the shared
// unrolled decoder (varint.go) reads it, and Discard advances by exactly the
// bytes the varint used.
//
// Peek fills until it has maxVarintLen bytes or the source reports an error, so
// a short window means the stream really is ending — never merely a chunk
// boundary. That is what keeps a varint split across reads INCOMPLETE rather
// than misread, and nothing is consumed unless a whole varint was decoded.
func (d *Decoder) readVarint(firstEOFok bool) (uint64, error) {
	w, err := d.r.Peek(maxVarintLen)
	if len(w) >= maxVarintLen {
		v, np, ok := uvarintFast(w, 0)
		if !ok {
			return 0, ErrInvalidMsg // > 64 bits: overlong, malformed
		}
		d.r.Discard(np)
		return v, nil
	}
	if len(w) == 0 {
		if err != nil && err != io.EOF {
			return 0, err // a real reader failure, not a stream boundary
		}
		if firstEOFok {
			return 0, io.EOF // clean end of stream at a field boundary
		}
		return 0, ErrIncomplete // expected a varint, but the stream ended
	}
	v, np, st := uvarintTail(w, 0)
	switch st {
	case varintOK:
		d.r.Discard(np)
		return v, nil
	case varintOverflow:
		return 0, ErrInvalidMsg // varint > 64 bits: malformed
	}
	if err != nil && err != io.EOF {
		return 0, err
	}
	return 0, ErrIncomplete // ended mid-varint: truncated
}

// readVarintBatch decodes as many of dst's elements as it can straight out of
// the reader's buffer, and returns how many. Zero means the buffer is down to
// its last few bytes (or the stream is ending) and the caller should fall back
// to readVarint for the next element, which is where truncation and end-of-
// stream are judged.
//
// It exists because Peek and Discard are the pull parser's per-varint cost once
// the byte-at-a-time reads are gone. One Peek/Discard pair per batch amortizes
// that away, and the elements decode through the same shared kernel the visitor
// path uses (varint.go).
//
// Nothing is consumed beyond the elements actually decoded, so a batch that runs
// into the end of the buffer leaves the remaining bytes for the next call and
// the resume-at-any-boundary property is unchanged.
func (d *Decoder) readVarintBatch(dst []uint64) (int, error) {
	avail := d.r.Buffered()
	if avail < maxVarintLen {
		// Force a fill so a whole varint is present if the stream can supply
		// one; a short result here just means the caller takes the slow path.
		if _, err := d.r.Peek(maxVarintLen); err != nil {
			if err != io.EOF {
				return 0, err
			}
		}
		if avail = d.r.Buffered(); avail < maxVarintLen {
			return 0, nil
		}
	}
	w, err := d.r.Peek(avail)
	if err != nil {
		return 0, err
	}
	got, np, st := decodeUvarintRun(w, 0, dst)
	if st != varintOK {
		return 0, tailErr(st)
	}
	if np > 0 {
		d.r.Discard(np)
	}
	return got, nil
}

// readFixlenHeaderFor is readFixlenHeader with the field id and the schema-bound
// source supplied by the caller, as the visitor stream surface must: there the
// scope's visitor answers whether the schema bounds this maxlen, not a per-field
// flag.
func (d *Decoder) readFixlenHeaderFor(id ID, sb schemaBound) (length uint64, sub uint64, err error) {
	h, err := d.readVarint(false)
	if err != nil {
		return 0, 0, err
	}
	length = h >> 3
	sub = h & 0x07
	if length > arrayMax {
		return 0, 0, ErrInvalidMsg
	}
	// §4.6 defines subtypes 0x0-0x3 only — 0x4-0x7 are reserved — and pins the
	// float widths: fp32 carries 4 payload bytes, fp64 carries 8; strings and
	// blobs take any length. §6.3 names a reserved subtype and a wrong-width
	// fp32/fp64 as InvalidMessage. This is framing, not schema, so it holds
	// whether or not the field is materialised: the check lives here so that
	// every pull consumer — Float32, Float64, String, Bytes, Skip and Next's
	// auto-skip — inherits it from one place, and a skipped field is a length
	// jump over a validated word rather than one over an attacker-chosen stride
	// the parser resynchronises on (issue #76). The visitor paths already ruled
	// the same way in their subtype switch (cursor.acceptFixlen,
	// acceptStreamFixlen), so all three surfaces now agree on every word.
	if err := checkFixlenSubtype(sub, length); err != nil {
		return 0, 0, err
	}
	if d.skipping == 0 {
		if err := d.lim.checkFixlen(sub, length, id, sb); err != nil {
			return 0, 0, err
		}
	}
	return length, sub, nil
}

// checkFixlenSubtype validates a fixlen word's subtype and its declared width
// against §4.6. It is pure framing: it never consults the schema or the
// receiver-side limits (those come after it, as ErrLimitExceeded).
func checkFixlenSubtype(sub, length uint64) error {
	switch sub {
	case fixFp32:
		if length != 4 {
			return ErrInvalidMsg
		}
	case fixFp64:
		if length != 8 {
			return ErrInvalidMsg
		}
	case fixStr, fixBlob:
		// Any length, including zero.
	default:
		return ErrInvalidMsg // reserved subtype 0x4-0x7
	}
	return nil
}

// readRaw reads exactly n bytes. A short read (the stream ending early) is
// reported as ErrIncomplete — the payload was truncated mid-field (§7), not
// malformed.
//
// It never pre-allocates the full n bytes from the (untrusted) claimed length:
// up to readRawChunk it sizes the buffer once — the common path for ordinary
// fields — but a larger claim is grown as bytes actually arrive, so a hostile
// length costs memory only in proportion to the bytes really delivered before
// the stream ends (amplification hardening, issue #40).
func (d *Decoder) readRaw(n uint64) ([]byte, error) {
	if n <= readRawChunk {
		buf := make([]byte, n)
		if _, err := io.ReadFull(d.r, buf); err != nil {
			return nil, eofToIncomplete(err)
		}
		return buf, nil
	}
	buf := make([]byte, 0, readRawChunk)
	tmp := make([]byte, readRawChunk)
	for uint64(len(buf)) < n {
		want := min(n-uint64(len(buf)), uint64(readRawChunk))
		m, err := io.ReadFull(d.r, tmp[:want])
		buf = append(buf, tmp[:m]...)
		if err != nil {
			return nil, eofToIncomplete(err)
		}
	}
	return buf, nil
}

// readRawChunk bounds the largest slice readRaw allocates up front; past it the
// buffer grows incrementally as bytes arrive.
const readRawChunk = 1 << 16

// fixedWindow returns a window into the reader's OWN buffer holding at least
// width bytes — usually more, everything currently buffered — without consuming
// anything. The caller decodes as many fixed-width elements out of it as it
// wants and then Discards exactly the bytes it used, so a run of fp32/fp64
// values costs one Peek/Discard pair per buffer-full instead of one heap
// allocation per element (issue #85). It is the fixed-width twin of what
// readVarint/readVarintBatch already do for varints, and it allocates nothing:
// this is a maxspeed corelib, so a value the reader already holds is decoded in
// place rather than copied into a fresh slice first.
//
// A stream that ends with fewer than width bytes left is a payload truncated
// mid-element: ErrIncomplete (§5.2/§7), never malformed. A genuine reader
// failure is returned verbatim. Nothing is consumed on either.
//
// width is at most 8, well under bufio's minimum buffer size, so Peek can always
// satisfy it when the stream can.
func (d *Decoder) fixedWindow(width int) ([]byte, error) {
	if avail := d.r.Buffered(); avail >= width {
		// Already buffered: Peek reads nothing and cannot fail.
		return d.r.Peek(avail)
	}
	w, err := d.r.Peek(width)
	if len(w) >= width {
		return w, nil
	}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err // a real reader failure, not a stream boundary
	}
	return nil, ErrIncomplete
}

// readFixed32 consumes one little-endian 4-byte value; readFixed64 the 8-byte
// form. Both decode straight out of the reader's buffer (fixedWindow), so a
// scalar fp32/fp64 field allocates nothing (issue #85).
func (d *Decoder) readFixed32() (uint32, error) {
	w, err := d.fixedWindow(4)
	if err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(w)
	d.r.Discard(4)
	return v, nil
}

func (d *Decoder) readFixed64() (uint64, error) {
	w, err := d.fixedWindow(8)
	if err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(w)
	d.r.Discard(8)
	return v, nil
}

// readFloat32Elements reads n fp32 array elements, readFloat64Elements the fp64
// form. Elements are decoded in batches straight out of the reader's buffer — as
// many as it currently holds, never past the declared count — so the only
// allocation left is the output slice's own growth (issue #85). This mirrors the
// batch/tail split readUnsignedElements uses for varint arrays.
//
// Neither pre-allocates from the untrusted count (initialArrayCap, issue #40),
// and neither consumes a partial element: a stream ending mid-array surfaces
// ErrIncomplete from fixedWindow, with the elements decoded so far returned
// alongside it exactly as the per-element reads did.
func (d *Decoder) readFloat32Elements(n uint64) ([]float32, error) {
	out := make([]float32, 0, initialArrayCap(n))
	for uint64(len(out)) < n {
		w, err := d.fixedWindow(4)
		if err != nil {
			return out, err
		}
		k := len(w) / 4
		if left := n - uint64(len(out)); uint64(k) > left {
			k = int(left)
		}
		// The batch is consumed as an advancing window with the width in the loop
		// condition, so the four-byte load needs no bounds check — see
		// cursor.acceptFixlenArray, which decodes the same elements the same way.
		for b := w[:k*4]; len(b) >= 4; b = b[4:] {
			out = append(out, math.Float32frombits(binary.LittleEndian.Uint32(b)))
		}
		d.r.Discard(k * 4)
	}
	return out, nil
}

func (d *Decoder) readFloat64Elements(n uint64) ([]float64, error) {
	out := make([]float64, 0, initialArrayCap(n))
	for uint64(len(out)) < n {
		w, err := d.fixedWindow(8)
		if err != nil {
			return out, err
		}
		k := len(w) / 8
		if left := n - uint64(len(out)); uint64(k) > left {
			k = int(left)
		}
		// See readFloat32Elements on the advancing window.
		for b := w[:k*8]; len(b) >= 8; b = b[8:] {
			out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(b)))
		}
		d.r.Discard(k * 8)
	}
	return out, nil
}

// initialArrayCap chooses a starting capacity for a decoded array that never
// pre-allocates from the untrusted wire count: the slice grows via append as
// elements actually decode, so a hostile count costs memory only in proportion
// to the bytes delivered before the stream ends (amplification hardening,
// issue #40).
func initialArrayCap(n uint64) int {
	const cap0 = 64
	if n < cap0 {
		return int(n)
	}
	return cap0
}

// arrayCountFor is arrayCount with the field id and the schema-bound source
// supplied by the caller — the form the visitor stream surface needs, where the
// scope's visitor answers whether the schema bounds this count (§6.2.1).
func (d *Decoder) arrayCountFor(id ID, sb schemaBound) (uint64, error) {
	n, err := d.readVarint(false)
	if err != nil {
		return 0, err
	}
	if n > arrayMax {
		return 0, ErrInvalidMsg
	}
	if d.skipping == 0 {
		if err := d.lim.checkArrayCount(n, id, sb); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// eofToIncomplete maps an end-of-stream error hit mid-value to ErrIncomplete
// (the payload was truncated mid-field, §7 — INCOMPLETE, not malformed), passing
// any other error through unchanged.
func eofToIncomplete(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return ErrIncomplete
	}
	return err
}
