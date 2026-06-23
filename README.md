<p align="center"><img src="assets/sofabuffers_logo.png" alt="SofaBuffers Logo" height="140"></p>

# SofaBuffers

<b>Structured Objects For Anyone</b><br>
<i>... so optimized, feels amazing.</i>

[Would you like to know more?](https://github.com/sofa-buffers)

## SofaBuffers Go library

[![CI](https://github.com/sofa-buffers/corelib-go/actions/workflows/ci.yml/badge.svg)](https://github.com/sofa-buffers/corelib-go/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fsofa-buffers%2Fcorelib-go%2Fbadges%2Fcoverage.json)](https://coveralls.io/github/sofa-buffers/corelib-go?branch=main)

[GitHub repository](https://github.com/sofa-buffers/corelib-go)

A **streaming**, **dependency-free** Go implementation of the SofaBuffers
(*Sofab*) serialization format — a compact, TLV-like binary format. It is the
**runtime stream core** (equivalent to the C `corelib`'s `istream` / `ostream`),
meant to be driven by **generated code**: a schema-driven code generator emits
one Go struct per message plus `Marshal` / `Unmarshal` methods that call the
primitives here, the same way protobuf-go's generated code calls its runtime.

The wire format is specified, language-neutrally, in the
[SofaBuffers documentation](https://github.com/sofa-buffers/documentation). The
unit tests here use the exact byte vectors from the
[C corelib](https://github.com/sofa-buffers/corelib-c-cpp)'s reference suite
(`test/c/test_ostream.c`) to guarantee byte-for-byte interoperability with the
C, C++ and Rust implementations.

Module path: `github.com/sofa-buffers/corelib-go` · package `sofab`.

```bash
go get github.com/sofa-buffers/corelib-go
```

## Why this design

| Goal | How |
|------|-----|
| Streaming **out** | [`Encoder`] writes to any `io.Writer` (buffered), so a message can exceed RAM and stream straight to a socket or file. |
| Streaming **in** | [`Decoder`] is a pull parser over any `io.Reader`; `Next()` returns one field header at a time, never materializing the whole message. |
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

## API summary

**Encoder** — methods: `WriteUnsigned`, `WriteSigned`, `WriteBool`,
`WriteFloat32`, `WriteFloat64`, `WriteString`, `WriteBytes`,
`WriteSequenceBegin` / `WriteSequenceEnd`, `WriteFloat32Array`,
`WriteFloat64Array`, `Flush`, `Err`; package functions `WriteUnsignedArray[T]`,
`WriteSignedArray[T]`.

**Decoder** — methods: `Next`, `Field`, `Unsigned`, `Signed`, `Bool`,
`Float32`, `Float64`, `String`, `Bytes`, `ReadFloat32Array`, `ReadFloat64Array`,
`Skip`; package functions `ReadUnsignedArray[T]`, `ReadSignedArray[T]`.

> **Note on value width:** like the C default configuration, the scalar value
> type is 64-bit (`uint64` / `int64`), so varint encodings match byte-for-byte
> across the C, C++, Rust and Go implementations.

## Layering vs. the C library

| C file | Go file | Status |
|--------|---------|--------|
| `sofab.h` (types/constants) | `types.go` (`WireType`, `ID`, errors, zigzag) | ported |
| `ostream.c` | `encoder.go` ([`Encoder`]) | ported |
| `istream.c` | `decoder.go` ([`Decoder`]) | ported (pull-parser model instead of bind-target callbacks) |
| `object.c` (descriptor transcoder) | — | not ported. The idiomatic Go equivalent is generated `Marshal` / `Unmarshal` code from a schema-driven generator; the streaming core above already covers serialize/deserialize. |

See `example_test.go` for a full generated-code-style `Marshal` / `Unmarshal`
example including a nested message (wire sequence), and `doc.go` for the
package-level documentation.

## Testing & coverage

```bash
go test ./...            # unit + roundtrip + example tests
go test ./... -race      # with the race detector
go test ./... -cover     # with coverage
go test ./... -v         # verbose
```

Tests are split by concern:

- `encoder_test.go` — encoder, byte-exact vs. the C vectors
- `decoder_test.go` — decoder over the same vectors + malformed-input errors
- `roundtrip_test.go` — encode→decode value preservation
- `example_test.go` — generated-code-style `Marshal` / `Unmarshal` walkthrough

Current coverage: **~76% of statements** (`go test -cover .`). All test vectors
are taken from the C reference implementation.

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
