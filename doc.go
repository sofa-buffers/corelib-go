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
// generator emits one Go struct per message plus Marshal/Unmarshal methods that
// call the Encoder/Decoder primitives here. (This mirrors how protobuf-go
// generated code calls its runtime.) The generator itself is out of scope for
// this package.
//
// # Streaming
//
// Encoding targets an io.Writer, so the encoder never holds the whole message in
// memory — messages larger than RAM can be streamed straight to a socket or
// file. The decoder offers two styles:
//
//   - Pull: call Decoder.Next to get the next field header, then a typed reader
//     (or Skip) to consume its value. It streams one field at a time, never
//     materializing the whole message. Best for hand-written, power-user code.
//   - Visitor: implement Visitor on the target type and call Decoder.Accept; the
//     decoder drives, binding each field straight into a struct member. This is
//     what generated Unmarshal code uses. See the Decoding example below.
//
// The visitor path reads the message into one contiguous buffer and parses it by
// advancing a cursor over it (the protobuf-style decode kernel), so for an
// in-memory source it is faster than the pull parser but does buffer the whole
// message. AcceptBytes is the zero-copy form when the message is already a
// []byte (e.g. generated Unmarshal).
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
// # Encoding example (what generated Marshal code looks like)
//
//	func (m *SensorReading) Marshal(e *sofab.Encoder) error {
//		e.WriteUnsigned(1, uint64(m.ID))
//		e.WriteSigned(2, int64(m.Temperature))
//		e.WriteString(3, m.Name)
//		sofab.WriteUnsignedArray(e, 4, m.Samples)
//		return e.Flush()
//	}
//
// # Decoding example (what generated Unmarshal code looks like)
//
//	func (m *SensorReading) Unmarshal(d *sofab.Decoder) error {
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
