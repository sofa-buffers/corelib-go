package sofab

import "io"

// Visitor is the push/visitor counterpart to the pull parser (Decoder.Next):
// the decoder drives, calling a typed method per field. Generated code
// implements Visitor on the target struct and binds each field straight into a
// member — so a generated object can be deserialized without the caller ever
// writing a Next/Skip loop. Nested sequences descend into the visitor returned
// by BeginSequence (typically the nested generated object).
//
// Array methods receive the values widened to the 64-bit value domain (or the
// concrete float slice); the generated code narrows to its declared element
// width.
type Visitor interface {
	Unsigned(id ID, v uint64) error
	Signed(id ID, v int64) error
	Float32(id ID, v float32) error
	Float64(id ID, v float64) error
	String(id ID, s string) error
	Bytes(id ID, b []byte) error
	UnsignedArray(id ID, v []uint64) error
	SignedArray(id ID, v []int64) error
	Float32Array(id ID, v []float32) error
	Float64Array(id ID, v []float64) error
	// BeginSequence returns the visitor that receives the nested scope's fields.
	BeginSequence(id ID) (Visitor, error)
	// EndSequence is called on that nested visitor once its scope closes, so a
	// generated nested object can finalize itself.
	EndSequence() error
}

// Accept decodes the entire top-level stream into v. It slurps the remaining
// input into one contiguous buffer and advances a cursor over it (see cursor),
// so dispatch never re-enters the io.Reader per byte. It returns nil at a clean
// end of stream, or a malformed-message error on bad input. A non-EOF reader
// error surfaces verbatim.
func (d *Decoder) Accept(v Visitor) error {
	buf, err := d.slurp()
	if err != nil {
		return err
	}
	c := cursor{buf: buf, lim: d.lim}
	return c.accept(v, 0)
}

// AcceptBytes decodes a complete message already held in one contiguous buffer,
// dispatching each field to v. It is the zero-copy form of Accept: the cursor
// advances directly over buf with no input slurp, so it is the fastest entry
// point when the message is already in memory (e.g. generated Unmarshal code).
// buf is not retained, but byte/blob fields handed to v alias it, so the visitor
// must copy any it keeps past the call.
//
// Optional decode limits (WithMaxArrayCount, WithMaxStringLen, WithMaxBlobLen)
// may be supplied; with none, no limits are enforced.
func AcceptBytes(buf []byte, v Visitor, opts ...Option) error {
	c := cursor{buf: buf, lim: newLimits(opts)}
	return c.accept(v, 0)
}

// slurp reads everything still pending into a single buffer. When the source
// reports its remaining length (bytes.Reader, bytes.Buffer, strings.Reader) the
// buffer is sized and filled in one shot; otherwise it falls back to io.ReadAll.
// Anything already buffered by a prior Next is honored.
func (d *Decoder) slurp() ([]byte, error) {
	var r io.Reader = d.src
	if d.r != nil {
		r = d.r
	}
	if l, ok := r.(interface{ Len() int }); ok {
		buf := make([]byte, l.Len())
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(r)
}
