// Package sofab is the Go core library for the SofaBuffers (Sofab) serialization
// format — a compact, streaming, TLV-like binary format. See ARCHITECTURE.md at
// the repository root for the language-neutral wire-format specification; this
// package reproduces it byte-for-byte (the tests use the C reference vectors).
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
// Encoding targets an io.Writer and decoding reads from an io.Reader, so neither
// side needs to hold the whole message in memory — messages larger than RAM can
// be streamed. The decoder is a pull parser: call Decoder.Next to get the next
// field header, then a typed reader (or Skip) to consume its value.
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
