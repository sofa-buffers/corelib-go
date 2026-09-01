// Package sofab is the Go core library for the SofaBuffers (Sofab) serialization
// format — a compact, streaming, TLV-like binary format. See the language-neutral
// wire-format specification in the SofaBuffers documentation repository
// (https://github.com/sofa-buffers/documentation); this package reproduces it
// byte-for-byte (the tests use the shared C-generated reference vectors).
//
// # Layers
//
// This package is the runtime stream core, equivalent to the C corelib's
// istream/ostream. It is consumed by *generated code*: a schema-driven code
// generator emits one Go struct per message plus the Serialize/Visitor pair that
// drives the Encoder/Decoder primitives here, wrapped in the one-shot
// Encode/Decode<Name> helpers. Those names are fixed by CORELIB_PLAN §6.1.1,
// which closes the generated layer's name set to encode/decode/try_decode/
// serialize/deserialize/decoder and admits no second spelling beside them.
// (This mirrors how protobuf-go generated code calls its runtime.) The
// generator itself is out of scope for this package.
//
// # Streaming
//
// Encoding writes into an output buffer that is drained as it fills, so the
// encoder never holds the whole message in memory — messages larger than RAM can
// be streamed straight to a socket or file. That buffer is the caller's:
// NewEncoderBuffer takes one with no sink (it holds the message, or reports
// ErrBufferFull), NewEncoderSink takes one plus a flush callback, and both take
// a start offset that leaves room at the front for a framing header
// (CORELIB_PLAN §5.1; MinOutputBuffer says how small such a buffer may be).
// NewEncoder is the io.Writer convenience form; it sizes a fixed scratch window
// once, at construction, and never grows it.
//
// THE VISITOR IS THE ONLY DECODE SURFACE (CORELIB_PLAN §5.3.1): implement
// Visitor on the target type and let the decoder drive, binding each field
// straight into a member the caller owns. There is no pull parser, no iterator,
// no cursor — one surface means one place to be correct, and behind the surface
// there is one implementation of it: a resumable PUSH state machine.
//
// Decoder.Feed is that machine (§5.2, §6.0). Hand it bytes in chunks of any
// size — one byte included — and it returns the outcome for everything consumed
// so far: Complete, Incomplete or Invalid. A field header, a varint, a payload
// or an array may be split across any number of calls; the machine suspends and
// resumes at any byte boundary. There is no finish or finalize step: the value
// Feed returns is the answer (§5.2.4).
//
// Two wrappers sit on it, and neither is a second surface — they hold no state,
// apply no rule and produce the same events the same Feed calls would:
//
//   - AcceptBytes — one Feed of a complete message already in one []byte (what
//     a generated Decode<Name> uses). §6.7.1 gives the one-shot path no memory
//     exemption, and it takes none.
//   - Decoder.FeedFrom — drain an io.Reader, feeding it in chunks of the
//     CALLER's scratch buffer. This package sizes no buffer from a stream.
//
// NOTHING THE DECODER HANDS OVER IS STORAGE (§6.6.3). A string or a blob
// arrives as FixlenBegin (the total, before any payload byte) and then one or
// more String / Bytes calls carrying the total, this piece's offset and a window
// into the caller's own fed bytes. An array arrives as ArrayBegin, one element
// callback per element, ArrayEnd. Building a value out of that is the
// destination's business, and its storage is the destination's own — which is
// what lets the codec allocate nothing at all after construction (§6.6).
//
// Whatever a visitor callback receives is valid only until that callback
// returns; a caller that keeps a value copies it first (§6.7). That holds on the
// one-shot path exactly as on the streaming one.
//
// A visitor may implement the optional extension StringPolicyVisitor, to receive
// the decode's UTF-8 policy; a visitor that does not decodes exactly as before.
//
// # Collectors
//
// A wrapper-sequence array — one whose elements are strings, blobs,
// structs/unions or arrays — reaches a visitor as a nested sequence whose child
// ids are the array indices (MESSAGE_SPEC §5.1). Turning that back into a slice
// is the same code for every schema, so it lives here rather than in every
// generated package: StringSeq, BlobSeq, MessageSeq, NestedSeq, the matrix
// collectors and PlaceRow, all built on the no-op VisitorBase. Every bound
// travels as a CONSTRUCTOR ARGUMENT — a Bounds for what the schema declares, a
// Caps for the §6.2.1 receiver caps, plus the declared element width — so a
// generated BeginSequence arm is one line handing back the collector its field
// is bound to, and a receiver cap left out is a compile error rather than a
// decode that runs uncapped. PayloadAcc is the same idea one level down: it
// assembles a string or blob payload out of the pieces the decoder delivers.
//
// # Sequence framing (omitting an all-default sequence)
//
// MESSAGE_SPEC §2 omits a sequence-typed *field* whose value equals its declared
// default instead of emitting an empty begin/end frame — but a wrapper-array
// *element* keeps its frame even when all-default, because element presence is
// what carries a dynamic array's length (§5.1). Whether a sequence is emitted
// therefore depends on what its children turn out to be, while its header must
// precede them, and the format exists to be streamed, so the sub-message must
// not be buffered to find out.
//
// Encoder.WriteSequenceBeginLazy resolves that by holding the header back: the
// first field write inside the sequence emits the whole run of held-back
// headers, outermost first. Since a generated Serialize already omits every
// child equal to its default, "not one child was written" is exactly "the object
// equals its declared default" — per child field, recursively, for free, and
// never as a byte comparison, so struct padding cannot influence it.
//
// The closer is a static property of the position in the schema, not of the
// value:
//
//   - Encoder.WriteSequenceEnd — a struct/union field, and an array wrapper
//     whose declared default is the empty collection: a contentless sequence
//     vanishes, header and end marker both.
//   - Encoder.WriteSequenceEndKeep — a wrapper-array element, and an array field
//     that differs from a non-empty declared default: the frame is emitted even
//     with no content.
//
// The two failure directions are not symmetric, which makes EndKeep the safe
// default when a call site is ambiguous: using it where End would do costs one
// non-canonical empty frame that every decoder normalizes away, while the
// reverse silently changes an array's length.
//
// The hold-back reaches the full MaxDepth: the run of held-back ids grows with
// the nesting, with no fixed window past which this package would give up and
// frame eagerly, so its output is canonical at every depth — what CORELIB_PLAN
// §6 requires of an implementation that can allocate. (Only a heap-free profile
// may bound the run and emit the empty frames beyond that bound; such output is
// still well-formed and decodes to the same value, it is simply not canonical.)
//
// # Decode outcome (three-valued, finish-less)
//
// Decoding reports one of three outcomes (MESSAGE_SPEC §7), identically on all
// three entry points and for one-shot and streaming use alike:
//
//   - COMPLETE — the input ended exactly at a field boundary (a valid message).
//     Signalled by a nil error.
//   - INCOMPLETE — the input ended *inside* a field (an unterminated varint, a
//     short fixlen/array payload, or an unclosed sequence). Signalled by
//     ErrIncomplete. This is NOT a malformed-message error: the bytes so far are
//     valid and more input could complete them. Like io.EOF, it is an outcome,
//     not a failure — the *caller* owns end-of-input and decides, from its own
//     framing (length prefix, datagram boundary, EOF), whether a trailing
//     ErrIncomplete is a truncation error. There is no finish/finalize step.
//   - INVALID — the bytes are malformed regardless of what follows. Signalled by
//     ErrInvalidMsg.
//
// Test the two with errors.Is; they are distinct sentinels, so a truncated
// stream is never conflated with a malformed one.
//
// A field whose wire type contradicts what the destination declares is NOT an
// error at all (CORELIB_PLAN §6.3): MESSAGE_SPEC §7.3 skips it exactly like one
// with an unknown id, leaving the destination untouched, and the decode stays
// COMPLETE. There is no code for it and no sentinel — the visitor's own field
// switch simply does not bind it.
//
// "Untouched" is the whole of it, and it binds the collectors in collectors.go
// as much as a generated field switch: a declined field is neither measured
// against this field's bounds nor written to this field's destination. A
// collector that declines an element at its header must therefore stay declined
// for the rest of that element's callbacks — see rowGate, which is what carries
// a matrix row's verdict from ArrayBegin to the ArrayEnd that would place it.
//
// CORELIB_PLAN §6.3 has no "invalid usage" code either; what is left of caller
// error is ErrArgument — an argument no valid field can be built from: an id past
// IDMax, a payload or element count past FIXLEN_MAX/ARRAY_MAX, a sequence opened
// past MaxDepth or closed with none open. All are refused before any byte is
// written.
//
// # Encoding example (what a generated Serialize looks like)
//
//	func (m *SensorReading) Serialize(e *sofab.Encoder) {
//		e.WriteUnsigned(1, uint64(m.ID))
//		e.WriteSigned(2, int64(m.Temperature))
//		e.WriteString(3, m.Name)
//		sofab.WriteUnsignedArray(e, 4, m.Samples)
//	}
//
// Errors are sticky, so the one-shot wrapper over it checks once at the end:
//
//	func (m *SensorReading) Encode() ([]byte, error) {
//		var buf bytes.Buffer
//		e := sofab.NewEncoder(&buf)
//		m.Serialize(e)
//		if err := e.Flush(); err != nil {
//			return nil, err
//		}
//		return buf.Bytes(), nil
//	}
//
// # Decoding example (what a generated Decode looks like)
//
//	func (m *SensorReading) Unsigned(id sofab.ID, v uint64) error {
//		switch id {
//		case 1:
//			m.ID = uint32(v)
//		}
//		return nil // an id this schema does not declare is simply not bound
//	}
//
//	func (m *SensorReading) Signed(id sofab.ID, v int64) error {
//		switch id {
//		case 2:
//			m.Temperature = int32(v)
//		}
//		return nil
//	}
//
//	func (m *SensorReading) String(id sofab.ID, s string) error {
//		switch id {
//		case 3:
//			m.Name = s // Go's string conversion has already copied it
//		}
//		return nil
//	}
//
//	func (m *SensorReading) UnsignedArray(id sofab.ID, v []uint64) error {
//		switch id {
//		case 4:
//			m.Samples = make([]uint16, len(v))
//			for i, x := range v {
//				if x > math.MaxUint16 {
//					return sofab.ErrInvalidMsg // the schema's declared width (§7.1)
//				}
//				m.Samples[i] = uint16(x)
//			}
//		}
//		return nil
//	}
//
//	func DecodeSensorReading(buf []byte) (*SensorReading, error) {
//		m := &SensorReading{}
//		return m, sofab.AcceptBytes(buf, m)
//	}
//
// A sub-sequence the consumer has no destination for is declined whole by
// returning a nil visitor from BeginSequence: nothing under it is delivered and
// nothing under it is built, however deep it goes.
package sofab
