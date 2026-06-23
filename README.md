# sofab — SofaBuffers Go core library

A Go implementation of the SofaBuffers (*Sofab*) serialization format — a
compact, streaming, TLV-like binary format. This is the **runtime stream core**
(equivalent to the C corelib's `istream`/`ostream`); it is meant to be driven by
**generated code**: a schema-driven code generator emits one Go struct per
message plus `Marshal`/`Unmarshal` methods that call the primitives here, the
same way protobuf-go generated code calls its runtime.

The wire format is specified in [`../ARCHITECTURE.md`](../ARCHITECTURE.md) and is
reproduced byte-for-byte — the tests use the exact vectors from the C reference
suite (`../test/c/test_ostream.c`), so output is interoperable with the C, C++
and Rust implementations.

Module path: `github.com/sofa-buffers/corelib-go` · package `sofab`.

## Design

- **Streaming both ways.** Encoding targets an `io.Writer`, decoding reads from
  an `io.Reader`, so neither side holds the whole message — messages larger than
  RAM stream naturally.
- **Pull decoder.** `Decoder.Next()` returns the next field header; the caller
  then calls a typed reader (or `Skip`) to consume the value. This maps directly
  onto generated `switch id { ... }` dispatch and gives forward/backward
  compatibility (unknown fields are skipped).
- **Generics for arrays.** `WriteUnsignedArray[T]` / `ReadUnsignedArray[T]` (and
  the signed variants) accept any `~uint8..~uint64` / `~int8..~int64` element
  type; float arrays have dedicated methods.
- **64-bit value type**, matching the C default configuration, so varint lengths
  and bytes are identical across languages.

## Usage

```go
// encode
var buf bytes.Buffer
e := sofab.NewEncoder(&buf)
e.WriteUnsigned(1, 42)
e.WriteSigned(2, -7)
e.WriteString(3, "hi")
sofab.WriteUnsignedArray(e, 4, []uint16{10, 20, 30})
if err := e.Flush(); err != nil { /* ... */ }

// decode
d := sofab.NewDecoder(&buf)
for {
    f, err := d.Next()
    if err == io.EOF { break }
    if err != nil { /* ... */ }
    switch {
    case f.ID == 1: v, _ := d.Unsigned(); _ = v
    case f.ID == 2: v, _ := d.Signed();   _ = v
    case f.ID == 3: s, _ := d.String();   _ = s
    case f.ID == 4: a, _ := sofab.ReadUnsignedArray[uint16](d); _ = a
    default:        d.Skip()
    }
}
```

See `example_test.go` for a full generated-code-style `Marshal`/`Unmarshal`
example including a nested message (wire sequence), and `doc.go` for the
package-level documentation.

## API summary

Encoder: `WriteUnsigned`, `WriteSigned`, `WriteBool`, `WriteFloat32`,
`WriteFloat64`, `WriteString`, `WriteBytes`, `WriteSequenceBegin/End`, `Flush`;
package functions `WriteUnsignedArray[T]`, `WriteSignedArray[T]`, methods
`WriteFloat32Array`, `WriteFloat64Array`.

Decoder: `Next`, `Unsigned`, `Signed`, `Bool`, `Float32`, `Float64`, `String`,
`Bytes`, `Skip`; package functions `ReadUnsignedArray[T]`, `ReadSignedArray[T]`,
methods `ReadFloat32Array`, `ReadFloat64Array`.

## Tests

```bash
go test ./...            # unit + roundtrip + example tests
go test ./... -cover     # with coverage
go test ./... -v         # verbose
```

All test vectors are taken from the C reference implementation.
