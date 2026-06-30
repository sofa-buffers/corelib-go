<p align="center"><img src="assets/sofabuffers_logo.png" alt="SofaBuffers" height="140"></p>

# SofaBuffers

<b>Structured Objects For Anyone</b><br>
<i>... so optimized, feels amazing.</i>

[Would you like to know more?](https://github.com/sofa-buffers)

## SofaBuffers Go library

[![CI](https://github.com/sofa-buffers/corelib-go/actions/workflows/ci.yml/badge.svg)](https://github.com/sofa-buffers/corelib-go/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fsofa-buffers%2Fcorelib-go%2Fbadges%2Fcoverage.json)](https://github.com/sofa-buffers/corelib-go/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-godoc-blue)](https://sofa-buffers.github.io/corelib-go/)

[GitHub repository](https://github.com/sofa-buffers/corelib-go)

A **streaming**, **dependency-free** Go implementation of the SofaBuffers
(*Sofab*) serialization format — a compact, TLV-like binary format. It is the
**runtime stream core** (equivalent to the C `corelib`'s `istream` / `ostream`),
meant to be driven by **generated code**: a schema-driven code generator emits
one Go struct per message plus `Marshal` / `Unmarshal` methods that call the
primitives here, the same way protobuf-go's generated code calls its runtime.

The wire format is specified, language-neutrally, in the
[SofaBuffers documentation](https://github.com/sofa-buffers/documentation). The
unit tests here use the exact byte vectors from `assets/test_vectors.json`,
copied verbatim from the
[C corelib](https://github.com/sofa-buffers/corelib-c-cpp)'s `assets/` folder
(which generates them and is their authoritative source) to guarantee
byte-for-byte interoperability with the C, C++ and Rust implementations.

Module path: `github.com/sofa-buffers/corelib-go` · package `sofab`. Requires Go 1.21+.

```bash
go get github.com/sofa-buffers/corelib-go
```

## Why this design

| Goal | How |
|------|-----|
| Streaming **out** | [`Encoder`] writes to any `io.Writer` (buffered), so a message can exceed RAM and stream straight to a socket or file. |
| Streaming **in** | [`Decoder`] is a pull parser over any `io.Reader`; `Next()` returns one field header at a time, never materializing the whole message. |
| Two decode styles | Pull with `Decoder.Next` (power users, true streaming — holds only one field at a time), or implement [`Visitor`] and call `Decoder.Accept` — the visitor binds each field straight into a struct member, which is what generated `Unmarshal` code uses. `Accept` reads the message into one contiguous buffer and runs a cursor over it (the throughput path), so it does hold the whole message; `AcceptBytes` is the zero-copy form for a message already in an in-memory `[]byte`. For decoding a source larger than RAM, use the pull parser. |
| No dependencies | Standard library only (`bufio`, `encoding/binary`, `io`, `math`, `errors`). No third-party modules, no `cgo`. |
| Sticky errors | The encoder records the first failure and turns later writes into no-ops, so generated `Marshal` code can issue a run of writes and check once at `Flush`. |
| Generics for arrays | `WriteUnsignedArray[T]` / `ReadUnsignedArray[T]` (and signed variants) accept any `~uint8..~uint64` / `~int8..~int64` element type; float arrays have dedicated methods. |
| Forward/backward compatible | Unknown fields are consumed with `Skip()` — old readers tolerate new fields, new readers tolerate missing ones. |
| 64-bit value type | Matches the C default configuration, so varint lengths and bytes are identical across languages. |

## Usage

```go
import (
	"bytes"
	"io"

	sofab "github.com/sofa-buffers/corelib-go"
)

// ---- encode ----
var buf bytes.Buffer
e := sofab.NewEncoder(&buf)
e.WriteUnsigned(1, 42)
e.WriteSigned(2, -7)
e.WriteString(3, "hi")
sofab.WriteUnsignedArray(e, 4, []uint16{10, 20, 30})
if err := e.Flush(); err != nil { /* ... */ }

// ---- decode (pull parser) ----
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
	default:        d.Skip() // unknown field
	}
}
```

### Streaming a message larger than RAM

Because the encoder targets an `io.Writer`, the "buffer" can be a socket, a
pipe, or a file — nothing is held whole in memory:

```go
conn, _ := net.Dial("tcp", "collector:9000")
e := sofab.NewEncoder(conn)            // bytes flow to the wire as they fill
for i := uint32(0); i < 1_000_000; i++ {
	e.WriteUnsigned(sofab.ID(i%128), uint64(i))
}
e.Flush()                              // push the tail
```

The decoder is symmetric: hand `NewDecoder` an `io.Reader` (socket, `os.Stdin`,
`gzip.Reader`, ...) and pull fields with `Next()` as they arrive.

### Decoding with a visitor

Generated `Unmarshal` code doesn't write a `Next` loop — it implements
[`Visitor`] and lets `Decoder.Accept` drive, binding each field straight into a
struct member. Nested sequences descend into the visitor returned by
`BeginSequence`:

```go
type sensor struct{ ID uint64; Name string }

func (s *sensor) Unsigned(id sofab.ID, v uint64) error { if id == 1 { s.ID = v }; return nil }
func (s *sensor) String(id sofab.ID, v string) error   { if id == 2 { s.Name = v }; return nil }
// ... other Visitor methods (no-ops for fields this type ignores) ...

var s sensor
err := sofab.NewDecoder(r).Accept(&s)
```

The pull parser (`Next`) is the streaming decode path: it holds only the value
of the field currently being delivered — never the whole message — so it can
decode a source larger than RAM and suspend/resume at any byte boundary. The
visitor path is the throughput path: `Accept` reads the whole message into one
contiguous buffer and advances the decode kernel (a cursor) over it, so it does
buffer the entire message in memory. When the message is already a `[]byte`,
`sofab.AcceptBytes(buf, v)` parses it in place with no copy — the zero-copy path
generated `Unmarshal` code uses (see **Memory handling** below).

## API summary

### Write operations

[`Encoder`] (`sofab.NewEncoder(io.Writer)`) — methods: `WriteUnsigned`,
`WriteSigned`, `WriteBool`, `WriteFloat32`, `WriteFloat64`, `WriteString`,
`WriteBytes`, `WriteSequenceBegin` / `WriteSequenceEnd`, `WriteFloat32Array`,
`WriteFloat64Array`, `Flush`, `Err`; package functions `WriteUnsignedArray[T]`,
`WriteSignedArray[T]`.

| Wire kind | Encoder call | Source type |
|-----------|--------------|-------------|
| unsigned int | `WriteUnsigned(id, v)` | `uint64` |
| signed int | `WriteSigned(id, v)` | `int64` (zigzag) |
| bool | `WriteBool(id, b)` | `bool` (as `0`/`1` unsigned) |
| fp32 / fp64 | `WriteFloat32` / `WriteFloat64` | `float32` / `float64` |
| string | `WriteString(id, s)` | `string` |
| blob | `WriteBytes(id, data)` | `[]byte` |
| unsigned array | `WriteUnsignedArray(e, id, a)` | `[]T`, `T` any `~uint8..~uint64` |
| signed array | `WriteSignedArray(e, id, a)` | `[]T`, `T` any `~int8..~int64` |
| fp32 / fp64 array | `WriteFloat32Array` / `WriteFloat64Array` | `[]float32` / `[]float64` |
| nested sequence | `WriteSequenceBegin(id)` … `WriteSequenceEnd()` | — |

Array writers reject an empty slice with `ErrArgument`; the float arrays are
methods (no generic element type), the integer arrays are package functions.

### Read operations

Two decode styles share the same wire grammar. The **pull** style ([`Decoder`],
`sofab.NewDecoder(io.Reader)`): call `Next` for the next [`Field`] header, then
exactly one typed reader (or `Skip`) to consume its value. The **visitor** style:
implement [`Visitor`] and call `Accept` / `AcceptBytes`; the decoder drives,
calling one typed method per field.

| Wire kind | Pull reader → destination | Visitor method → destination |
|-----------|---------------------------|------------------------------|
| header | `Next() (Field, error)`, `Field() Field` | (driven internally) |
| unsigned int | `Unsigned() (uint64, …)` | `Unsigned(id, uint64)` |
| signed int | `Signed() (int64, …)` | `Signed(id, int64)` |
| bool | `Bool() (bool, …)` | (via `Unsigned`, `0`/`1`) |
| fp32 / fp64 | `Float32()` / `Float64()` → `float32` / `float64` | `Float32(id, float32)` / `Float64(id, float64)` |
| string | `String() (string, …)` | `String(id, string)` |
| blob | `Bytes() ([]byte, …)` | `Bytes(id, []byte)` |
| unsigned array | `ReadUnsignedArray[T](d) ([]T, …)` | `UnsignedArray(id, []uint64)` |
| signed array | `ReadSignedArray[T](d) ([]T, …)` | `SignedArray(id, []int64)` |
| fp32 / fp64 array | `ReadFloat32Array()` / `ReadFloat64Array()` → `[]float32` / `[]float64` | `Float32Array(id, []float32)` / `Float64Array(id, []float64)` |
| sequence descend | `Next` returns a `TypeSequenceStart`/`End` header | `BeginSequence(id) (Visitor, …)` / `EndSequence()` |
| skip unknown | `Skip()` (descends a whole nested sequence) | return a no-op from the relevant method |

Visitor array methods always receive the value widened to the 64-bit domain
(`[]uint64` / `[]int64`) or the concrete float slice; generated code narrows to
its declared element width. The pull array helpers narrow during the read via the
generic `T`. Entry points: `Accept(v Visitor) error` (reads the whole message
from the `Decoder`'s `io.Reader` into one buffer, then dispatches) and the
package function `AcceptBytes(buf []byte, v Visitor) error` (zero-copy over an
in-memory message).

### Allowed types

The array helpers are generic over the element width; everything else is a fixed
typed method.

| Family | Constraint / type | Allowed widths |
|--------|-------------------|----------------|
| unsigned int + arrays | `Unsigned` = `~uint8 \| ~uint16 \| ~uint32 \| ~uint64` | u8, u16, u32, u64 |
| signed int + arrays | `Signed` = `~int8 \| ~int16 \| ~int32 \| ~int64` | i8, i16, i32, i64 |
| floats + float arrays | concrete `float32` / `float64` | fp32, fp64 |
| string | concrete `string` | — |
| blob | concrete `[]byte` | — |

The generic constraints use `~`, so named types with those underlying kinds
(e.g. `type Celsius int32`) are accepted. **Disallowed:** a fixlen *array* may
only carry the `fp32` (4-byte) or `fp64` (8-byte) subtype — string- and
blob-element (dynamic-subtype) fixlen arrays are not part of the format and a
fixlen-array header with any other subtype/size decodes to `ErrInvalidMsg`.
Scalar values are 64-bit (`uint64` / `int64`) to match the C default config, and
field ids must be `≤ IDMax` (`INT32_MAX`).

### Memory handling

This is the part that most affects how callers wire the library in. There is no
caller-supplied scratch buffer on either side: the **encoder** owns an internal
`bufio.Writer` over your sink, and the **decoder** owns its window.

**Encoder (write).** You hand `NewEncoder` an `io.Writer` (a `bytes.Buffer`, a
socket, a file, `gzip.Writer`, …) — the *sink*, not a fixed output buffer. The
encoder wraps it in a `bufio.Writer` and streams bytes out as that buffer fills,
so a message may exceed RAM and flow straight to the wire; the library never
holds the whole encoded message. Each write copies its bytes into the bufio
buffer (strings/blobs are not retained after the call returns), so the caller's
source slices/strings may be reused immediately. Errors are **sticky**: the first
failure is recorded and later writes become no-ops, so generated `Marshal` code
issues a run of writes and checks once. You **must call `Flush`** to push the tail
of the bufio buffer to the sink (and to surface a late write error); `Err`
returns the first error without flushing.

**Decoder (read).** The pull path streams; the visitor entry points buffer the
message for throughput. They also differ in whether returned bytes are copies the
caller owns or slices that alias internal storage:

| Path | Input | Buffering | string / blob result | array & scalar result |
|------|-------|-----------|-----------------------|-----------------------|
| `Decoder.Next` (pull) | any `io.Reader` (lazily wrapped in `bufio.Reader`) | streams one field at a time; whole message never held — decodes sources larger than RAM | **fresh copy** — `String()` and `Bytes()` allocate and the caller owns the result | freshly allocated `[]T` the caller owns |
| `Decoder.Accept` (visitor) | the `Decoder`'s `io.Reader` | reads the whole message into one contiguous buffer, then runs a cursor over it — the whole message is held in memory | `String` → **fresh copy** (safe to keep); `Bytes` → **alias of the read buffer**, valid only for the duration of that call — a visitor that keeps it **must copy** | freshly allocated slice the visitor owns |
| `AcceptBytes` (visitor, zero-copy) | a caller-owned `[]byte` | no copy — the parser advances directly over your buffer | `String` → **fresh copy**; `Bytes` → **alias into the caller's `[]byte`** — zero-copy, so the input buffer **must stay alive** as long as the blob is referenced | freshly allocated slice the visitor owns |

Takeaways: `Next` is the safe-by-default *and* streaming path — every value is a
copy the caller owns and the whole message is never held. `Accept` and
`AcceptBytes` are faster but buffer the whole message (`Accept` slurps it;
`AcceptBytes` requires it up front) and only string values are copied; **blob
(`Bytes`) values alias** — the read buffer (`Accept`) or the caller's buffer
(`AcceptBytes`) — so a visitor that retains a blob past the call (or past the
buffer's lifetime) must copy it. Numeric arrays are always freshly allocated on
every path.

> **Note on value width:** like the C default configuration, the scalar value
> type is 64-bit (`uint64` / `int64`), so varint encodings match byte-for-byte
> across the C, C++, Rust and Go implementations.

## Feature flags

Go always ships the full format — there are no build toggles, because the desktop
and server targets it is built for are not code-size constrained.

| Feature | State |
|---------|-------|
| `fixlen` (fp32 / fp64, string, blob) | always on |
| `array` (unsigned / signed / fixlen arrays) | always on |
| `sequence` (nested scopes) | always on |
| `fp64` | always on |

The scalar value type is 64-bit (`uint64` / `int64`), matching the C default
configuration so the wire image and varint lengths are identical across ports.

## Layering vs. the C library

| C file | Go file | Status |
|--------|---------|--------|
| `sofab.h` (types/constants) | `types.go` (`WireType`, `ID`, errors, zigzag) | ported |
| `ostream.c` | `encoder.go` ([`Encoder`]) | ported |
| `istream.c` | `decoder.go` ([`Decoder`]) + `visitor.go` ([`Visitor`]) | ported (both a pull parser and a visitor/push model) |
| `object.c` (descriptor transcoder) | — | not ported. The idiomatic Go equivalent is generated `Marshal` / `Unmarshal` code from a schema-driven generator; the streaming core above already covers serialize/deserialize. |

See `example_test.go` for a full generated-code-style `Marshal` / `Unmarshal`
example including a nested message (wire sequence), and `doc.go` for the
package-level documentation.

## Build & test

```bash
go build ./...           # build
go vet ./...             # static analysis
go test ./...            # unit + roundtrip + example tests
go test ./... -race      # with the race detector
go test ./... -cover     # with coverage
```

Tests are split by concern:

- `vectors_test.go` — encode + decode against the shared conformance suite
  (`assets/test_vectors.json`, copied verbatim from the `documentation` repo)
- `streaming_test.go` — chunked streaming: small-buffer encode and byte-at-a-time
  / odd-sized decode resume at any boundary
- `encoder_test.go` — encoder, byte-exact vs. the reference vectors
- `decoder_test.go` — decoder over the same vectors + malformed-input errors
- `roundtrip_test.go` — encode→decode value preservation
- `example_test.go` — generated-code-style `Marshal` / `Unmarshal` walkthrough

Coverage is reported by the badge above (well over the 90% bar). The shared
`assets/test_vectors.json` is the cross-language source of truth, so output is
byte-identical to the C, C++ and Rust implementations.

## Benchmarks

`cmd/perfbench` mirrors the C/C++/Rust corelib benchmarks — same messages, same
workloads, same ids and values — so the implementations can be compared
directly. It has two modes:

```bash
go run ./cmd/perfbench time            # real wall-clock throughput, MB/s (MB = 1e6)
go run ./cmd/perfbench encode_u64_array   # single workload, for Callgrind --toggle-collect
```

The named-workload mode exposes `//go:noinline` `run_*` functions so a Callgrind
harness can toggle collection on `main.run_<workload>` exactly as the C/C++/Rust
tools do (setup excluded). The `time` mode reports real wall-clock throughput on
the current machine; numbers vary with CPU speed, load and build flags.

For the Go-native view, the decode path also has `go test` benchmarks
(`decode_bench_test.go`) covering `Accept` and the zero-copy `AcceptBytes`:

```bash
go test -run '^$' -bench BenchmarkDecode -benchmem -count=8 -cpu=1 -benchtime=300ms
```
