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
runtime stream core, meant to be driven by **generated code**: a schema-driven
generator emits one Go struct per message plus `Marshal` / `Unmarshal` methods
that call the [`Encoder`] / [`Decoder`] primitives here, the same way
protobuf-go's generated code calls its runtime.

### Requirements

Go **1.21+** (CI builds on `1.21` and current stable). The scalar value type is
64-bit, so varint lengths and wire bytes are identical across languages.

### Dependencies

**None** — standard library only (`bufio`, `encoding/binary`, `errors`, `io`,
`math`, `unicode/utf8`). No third-party modules, no `cgo`.

### Package name

The module path is `github.com/sofa-buffers/corelib-go`; the imported package is
`sofab`:

```bash
go get github.com/sofa-buffers/corelib-go
```

```go
import sofab "github.com/sofa-buffers/corelib-go"
```

## Why this design

| Goal | How |
|------|-----|
| Streaming **out** | [`Encoder`] writes to any `io.Writer`, buffering into a small internal slice flushed as it fills — a message can exceed RAM and stream straight to a socket or file. |
| Streaming **in** | [`Decoder`] is a pull parser over any `io.Reader`; `Next()` returns one field header at a time, never materializing the whole message. |
| Two decode styles | Pull with `Decoder.Next`, or implement [`Visitor`] and call `Decoder.Accept`, which binds each field into a struct member — what generated `Unmarshal` uses. `AcceptBytes` is the zero-copy form for a message already in a `[]byte`. |
| No dependencies | Standard library only, no `cgo`. |
| Sticky errors | The encoder records the first failure and turns later writes into no-ops, so generated `Marshal` can issue a run of writes and check once at `Flush`. |
| Generics for arrays | `WriteUnsignedArray[T]` / `ReadUnsignedArray[T]` (and signed variants) accept any `~uint8..~uint64` / `~int8..~int64` element type; float arrays have dedicated methods. |
| Forward/backward compatible | Unknown fields are consumed with `Skip()` — old readers tolerate new fields, new readers tolerate missing ones. |

## Usage

The `Encoder` / `Decoder` are the streaming primitives; the four use cases below —
serialize a message that fits in one buffer, serialize one too large for the
buffer, deserialize a whole message, and deserialize one arriving in chunks —
mirror the generated-code path (see [Code generator](#code-generator)).

### Serialize

Write fields into an `Encoder` over an in-memory buffer and `Flush` to push the
tail:

```go
var buf bytes.Buffer
e := sofab.NewEncoder(&buf)
e.WriteUnsigned(1, 42)
e.WriteSigned(2, -7)
e.WriteString(3, "hi")
if err := e.Flush(); err != nil { /* ... */ }   // Flush pushes the tail
msg := buf.Bytes()
```

### Serialize stream

`NewEncoder` takes any `io.Writer` sink (socket, pipe, file, `gzip.Writer`, …) and
buffers into a small internal slice flushed as it fills, so a message larger than
RAM streams straight to the wire:

```go
conn, _ := net.Dial("tcp", "collector:9000")
e := sofab.NewEncoder(conn)            // bytes flow to the wire as the buffer fills
for i := uint32(0); i < 1_000_000; i++ {
    e.WriteUnsigned(sofab.ID(i%128), uint64(i))
}
e.Flush()                              // push the tail (and surface a late error)
```

### Deserialize

`Decoder` is a pull parser: `Next()` returns one field header at a time; read the
value with a typed accessor, or `Skip()` an unknown field:

```go
d := sofab.NewDecoder(bytes.NewReader(msg))
for {
    f, err := d.Next()
    if err == io.EOF { break }
    if err != nil { /* ... */ }
    switch {
    case f.ID == 1: v, _ := d.Unsigned(); _ = v
    case f.ID == 2: v, _ := d.Signed();   _ = v
    case f.ID == 3: s, _ := d.String();   _ = s
    default:        d.Skip()   // unknown field
    }
}
```

### Deserialize stream

The very same loop reads a stream: hand `NewDecoder` any `io.Reader` and it refills
on demand, so it decodes correctly whether the bytes arrive all at once or a few at
a time — the chunk boundaries live in the reader, not your code:

```go
d := sofab.NewDecoder(conn)            // any io.Reader: socket, pipe, gzip.Reader, os.Stdin
for {
    f, err := d.Next()                 // refills from the reader on demand
    if err == io.EOF { break }
    if err != nil { /* ... */ }
    switch {
    case f.ID == 1: id, _ := d.Unsigned(); _ = id
    default:        d.Skip()
    }
}
```

### Code generator

The common real use is driving the runtime through **generated object code**:
`sofabgen` emits one struct per message with a private `marshal`, a public
`Encode`, and a package-level `Decode<Name>` built on the `sofab.Visitor` methods.
A hand-written stand-in, encoded then decoded:

```go
// generated by: sofabgen --lang go
type Point struct {
    _visitorBase                 // default no-op Visitor methods
    X int32 `json:"x"`
    Y int32 `json:"y"`
}

func (m *Point) marshal(e *sofab.Encoder) { e.WriteSigned(1, int64(m.X)); e.WriteSigned(2, int64(m.Y)) }

func (m *Point) Signed(id sofab.ID, v int64) error {
    switch id { case 1: m.X = int32(v); case 2: m.Y = int32(v) }
    return nil
}

func (m *Point) Encode() ([]byte, error) {
    var buf bytes.Buffer
    e := sofab.NewEncoder(&buf)
    m.marshal(e)
    if err := e.Flush(); err != nil { return nil, err }
    return buf.Bytes(), nil
}

func DecodePoint(data []byte) (*Point, error) {
    m := &Point{}
    if err := sofab.AcceptBytes(data, m); err != nil { return nil, err }
    return m, nil
}

// use:
wire, _ := (&Point{X: 3, Y: 4}).Encode()
got, _ := DecodePoint(wire)              // got.X == 3, got.Y == 4
```

## Memory handling

Buffer ownership is the part that most affects how callers wire the library in.

**Encoder.** You hand `NewEncoder` an `io.Writer` sink, not a fixed output
buffer. Bytes accumulate in a small internal slice and flush to the writer as it
fills (and on `Flush`), so the whole encoded message is never held. Each write
copies its bytes into that slice, so caller source strings/slices may be reused
immediately. You **must call `Flush`** to push the tail and surface a late write
error.

**Decoder.** The pull path (`Next`) is safe-by-default *and* streaming: `String()`
and `Bytes()` both return fresh copies the caller owns. `Accept` / `AcceptBytes`
buffer the whole message and are faster, but only string values are copied — blob
(`Bytes`) values **alias** the read buffer (`Accept`) or the caller's `[]byte`
(`AcceptBytes`), so a visitor keeping a blob past the call must copy it. Numeric
arrays are always freshly allocated on every path.

| Path | `String` | `Bytes` (blob) |
|------|----------|----------------|
| `Next` (pull, streaming) | fresh copy | fresh copy |
| `Accept` | fresh copy | aliases read buffer — copy to keep |
| `AcceptBytes` | fresh copy | aliases caller's `[]byte` — keep it alive |

## Feature flags

Go always ships the **full format** — no build toggles.

## Build & test

```bash
go build ./...           # build
go vet ./...             # static analysis
go test ./...            # unit + roundtrip + example tests
go test ./... -race      # with the race detector
go test ./... -cover     # with coverage
```

Tests cover the shared conformance suite (`vectors_test.go`), chunked/byte-at-a-time
streaming that resumes at any boundary (`streaming_test.go`), byte-exact encode/decode
and malformed-input errors, the visitor path (`visitor_test.go`), roundtrip value
preservation, and the generated-code-style walkthrough (`example_test.go`).

## Benchmarks

`cmd/perfbench` runs the shared corelib benchmark workloads, printed in the common
format so implementations compare directly. Throughput is measured on process CPU
time (user + system, via `getrusage`), not wall-clock. Subcommands:

```bash
go run ./cmd/perfbench bench   # throughput table (MB/s, MB = 1e6) over a ~1s CPU-time loop
go run ./cmd/perfbench perf    # per-op cost (CPU time/op ns + MB/s) for the 12-field message
```

Single-workload subcommands (`encode_u64_array`, `encode_typical`,
`decode_u64_array`, `decode_typical`) run one `//go:noinline` `run_*` function once
with setup excluded, so a Callgrind harness can toggle collection on
`main.run_<workload>`. The decode path also has `go test` benchmarks in
`decode_bench_test.go`:

```bash
go test -run '^$' -bench BenchmarkDecode -benchmem -count=8 -cpu=1 -benchtime=300ms
```
