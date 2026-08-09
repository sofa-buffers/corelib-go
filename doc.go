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
// NewEncoder is the io.Writer convenience form, and the only one that allocates
// a window of its own. The decoder offers three styles:
//
//   - Pull: call Decoder.Next to get the next field header, then a typed reader
//     (or Skip) to consume its value. It streams one field at a time, never
//     materializing the whole message. Best for hand-written, power-user code.
//   - Visitor: implement Visitor on the target type and call Decoder.Accept; the
//     decoder drives, binding each field straight into a struct member. This is
//     what a generated Decode<Name> uses. See the Decoding example below.
//   - Visitor over a reader: the same Visitor, driven by Decoder.AcceptStream,
//     which reads and dispatches each field as the io.Reader delivers it instead
//     of buffering the message first. This is what a generated
//     Decode<Name>From(io.Reader) uses.
//
// Accept reads the message into one contiguous buffer and parses it by advancing
// a cursor over it (the protobuf-style decode kernel), so for an in-memory source
// it is faster than the pull parser but does buffer the whole message.
// AcceptBytes is the zero-copy form when the message is already a []byte (e.g. a
// generated Decode<Name>). AcceptStream buffers nothing: peak memory is the
// largest single field, and — having no shared image to alias — it hands the
// visitor freshly read storage for strings and blobs alike, where Accept and
// AcceptBytes hand it blob values that are views into the buffer being parsed.
//
// A visitor may implement the optional extensions HeaderVisitor and
// ElemBoundVisitor (and SchemaBoundVisitor, StringPolicyVisitor) to have a
// schema bound reach the decoder; each is consulted on every visitor path, and a
// visitor that implements none decodes exactly as before.
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
// Decoding reports one of three outcomes (MESSAGE_SPEC §7), on both the pull and
// visitor paths, and identically for one-shot and streaming use:
//
//   - COMPLETE — the input ended exactly at a field boundary (a valid message).
//     Signalled by a nil error (Accept) or io.EOF at the top level (Next).
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
// A further sentinel sits beside them on the pull path: ErrTypeMismatch, returned
// when a typed reader is bound to a field of another wire type — or, for a
// fixlen, another subtype. It is not a verdict on the message: MESSAGE_SPEC §7.3
// skips such a field exactly like one with an unknown id, so the reader consumes
// the value, leaves the destination untouched, and the decode stays COMPLETE.
// CORELIB_PLAN §6.3 has no "invalid usage" code; what is left of caller error is
// ErrArgument — a typed reader called with no field waiting, or after the current
// value was already consumed. On the encode side it is the same sentinel for the
// same reason: an argument no valid field can be built from — an id past IDMax, a
// payload or element count past FIXLEN_MAX/ARRAY_MAX, a sequence opened past
// MaxDepth or closed with none open — is refused before any byte is written.
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
// # Decoding example (a hand-written Decode over the pull API)
//
// Generated code decodes through the visitor path instead — Decode<Name> runs
// AcceptBytes over the message and binds each field in the Visitor methods —
// but both surfaces reach the same verdict on the same bytes.
//
//	func (m *SensorReading) Decode(d *sofab.Decoder) error {
//		for {
//			f, err := d.Next()
//			if err == io.EOF {
//				return nil // end of the top-level message
//			}
//			if err != nil {
//				return err
//			}
//			switch {
//			case f.Type == sofab.TypeSequenceEnd:
//				return nil // end of this (sub-)message
//			case f.ID == 1:
//				v, _ := d.Unsigned()
//				m.ID = uint32(v)
//			case f.ID == 2:
//				v, _ := d.Signed()
//				m.Temperature = int32(v)
//			case f.ID == 3:
//				m.Name, _ = d.String()
//			case f.ID == 4:
//				m.Samples, _ = sofab.ReadUnsignedArray[uint16](d)
//			default:
//				d.Skip() // unknown field: forward/backward compatible
//			}
//		}
//	}
package sofab
