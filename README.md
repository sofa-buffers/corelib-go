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
[`Encoder`] / [`Decoder`] primitives here, the same way protobuf-go's generated
code calls its runtime.

The wire format is specified, language-neutrally, in the
[SofaBuffers documentation](https://github.com/sofa-buffers/documentation). The
unit tests here use the exact byte vectors from `assets/test_vectors.json`,
copied verbatim from the
[C corelib](https://github.com/sofa-buffers/corelib-c-cpp)'s `assets/` folder
(their authoritative source), guaranteeing byte-for-byte interoperability with
the C, C++ and Rust implementations.

### Requirements

Go **1.21+** (the module declares `go 1.21`; CI builds on `1.21` and the current
stable release). The 64-bit scalar value type matches the C default
configuration, so varint lengths and wire bytes are identical across languages.

### Dependencies

**None** — the standard library only (`bufio`, `encoding/binary`, `errors`,
`io`, `math`, `unicode/utf8`). No third-party modules, no `cgo`.

### Package name

The module (`go get`) path is `github.com/sofa-buffers/corelib-go`. Consumers
import it and use it as package `sofab` (the code API namespace):

```bash
go get github.com/sofa-buffers/corelib-go
```

```go
import sofab "github.com/sofa-buffers/corelib-go"
```

## Why this design

| Goal | How |
|------|-----|
| Streaming **out** | [`Encoder`] writes to any `io.Writer`, buffering into a small internal slice that is flushed as it fills — so a message can exceed RAM and stream straight to a socket or file. |
| Streaming **in** | [`Decoder`] is a pull parser over any `io.Reader`; `Next()` returns one field header at a time, never materializing the whole message. |
| Two decode styles | Pull with `Decoder.Next` (true streaming — holds only one field at a time), or implement [`Visitor`] and call `Decoder.Accept`, which binds each field straight into a struct member — what generated `Unmarshal` code uses. `Accept` reads the whole message into one contiguous buffer for throughput; `AcceptBytes` is the zero-copy form for a message already in a `[]byte`. |
| No dependencies | Standard library only. No third-party modules, no `cgo`. |
| Sticky errors | The encoder records the first failure and turns later writes into no-ops, so generated `Marshal` code can issue a run of writes and check once at `Flush`. |
| Generics for arrays | `WriteUnsignedArray[T]` / `ReadUnsignedArray[T]` (and signed variants) accept any `~uint8..~uint64` / `~int8..~int64` element type; float arrays have dedicated methods. |
| Forward/backward compatible | Unknown fields are consumed with `Skip()` — old readers tolerate new fields, new readers tolerate missing ones. |
| 64-bit value type | Matches the C default configuration, so varint lengths and bytes are identical across languages. |

## Usage

The `Encoder`/`Decoder` are the streaming primitives; generated code (see the
[Generator example](#generator-example)) drives them for you. All snippets below
compile against the current API.

### Simple encode

```go
var buf bytes.Buffer
e := sofab.NewEncoder(&buf)
e.WriteUnsigned(1, 42)
e.WriteSigned(2, -7)
e.WriteString(3, "hi")
sofab.WriteUnsignedArray(e, 4, []uint16{10, 20, 30})
if err := e.Flush(); err != nil { /* ... */ }   // Flush pushes the tail
```

### Simple decode (pull parser)

```go
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

### Streaming output — `Encoder` over any `io.Writer` (the OStream)

`NewEncoder` takes an `io.Writer` *sink* (a socket, pipe, file, `gzip.Writer`,
…), not a fixed output buffer. Bytes accumulate in a small internal slice and are
written out as it fills, so a message larger than RAM streams straight to the
wire — nothing is held whole in memory:

```go
conn, _ := net.Dial("tcp", "collector:9000")
e := sofab.NewEncoder(conn)            // bytes flow to the wire as they fill
for i := uint32(0); i < 1_000_000; i++ {
	e.WriteUnsigned(sofab.ID(i%128), uint64(i))
}
e.Flush()                              // push the tail (and surface a late error)
```

### Streaming input — `Decoder` over any `io.Reader` (the IStream)

The decoder is symmetric: hand `NewDecoder` an `io.Reader` (socket, `os.Stdin`,
`gzip.Reader`, …) and pull fields with `Next()` as they arrive. The pull parser
holds only the value currently being delivered, so it can decode a source larger
than RAM and suspend/resume at any byte boundary. For an in-memory message you
can instead run the visitor path (`Accept`) for throughput:

```go
type sensor struct{ ID uint64; Name string }

// A Visitor implements one method per wire kind; unused ones are no-ops.
func (s *sensor) Unsigned(id sofab.ID, v uint64) error { if id == 1 { s.ID = v }; return nil }
func (s *sensor) String(id sofab.ID, v string) error   { if id == 2 { s.Name = v }; return nil }
// ... other Visitor methods ...

var s sensor
err := sofab.NewDecoder(r).Accept(&s) // reads the whole message, then dispatches
```

### Generator example

The most common real use case is driving the library through **generated
object code**: a schema compiled by the code generator becomes a Go struct with
`Marshal` / `Unmarshal` methods that delegate to the runtime here. The pattern —
including a nested message (which becomes a wire *sequence*) — is demonstrated
end-to-end in [`example_test.go`](example_test.go):

```go
type SensorReading struct {
	ID          uint32
	Temperature int32
	Name        string
	Samples     []uint16
	Calibration Calibration // nested message -> wire sequence
}

func (m *SensorReading) Marshal(e *sofab.Encoder) {
	e.WriteUnsigned(1, uint64(m.ID))
	e.WriteSigned(2, int64(m.Temperature))
	e.WriteString(3, m.Name)
	sofab.WriteUnsignedArray(e, 4, m.Samples)
	e.WriteSequenceBegin(5)          // open the nested Calibration sequence
	m.Calibration.marshal(e)
	e.WriteSequenceEnd()
}

func (m *SensorReading) Unmarshal(d *sofab.Decoder) error {
	for {
		f, err := d.Next()
		if err == io.EOF { return nil }
		if err != nil { return err }
		switch {
		case f.ID == 1: v, _ := d.Unsigned(); m.ID = uint32(v)
		case f.ID == 2: v, _ := d.Signed();   m.Temperature = int32(v)
		case f.ID == 3: m.Name, _ = d.String()
		case f.ID == 4: m.Samples, _ = sofab.ReadUnsignedArray[uint16](d)
		case f.ID == 5 && f.Type == sofab.TypeSequenceStart:
			if err := m.Calibration.unmarshal(d); err != nil { return err }
		default: d.Skip()
		}
	}
}
```

## API summary

### Encoding

[`Encoder`] (`sofab.NewEncoder(io.Writer)`) offers one typed `Write*` call per
wire kind: `WriteUnsigned` / `WriteSigned` (varint, signed via zigzag),
`WriteBool` (an unsigned `0`/`1`), `WriteFloat32` / `WriteFloat64`,
`WriteString`, `WriteBytes`, the float-array methods `WriteFloat32Array` /
`WriteFloat64Array`, and the generic package functions `WriteUnsignedArray[T]` /
`WriteSignedArray[T]` (any `~uint8..~uint64` / `~int8..~int64` element type,
narrowed to the 64-bit wire domain). Nested scopes are opened and closed with
`WriteSequenceBegin(id)` … `WriteSequenceEnd()` (bounded at `MaxDepth`). Empty
arrays are valid and encode as a header with a zero count. Errors are **sticky**:
the first failure is recorded, later writes become no-ops, and the error surfaces
at `Err()` or `Flush()` — so generated `Marshal` code writes a run of fields and
checks once. `Flush` must be called to push the buffered tail to the sink.

### Decoding

Two styles share the same wire grammar:

- **Pull** ([`Decoder`], `sofab.NewDecoder(io.Reader)`) — call `Next()` for the
  next [`Field`] header, then exactly one typed reader (`Unsigned`, `Signed`,
  `Bool`, `Float32`/`Float64`, `String`, `Bytes`, `ReadFloat32Array`/
  `ReadFloat64Array`, the generic `ReadUnsignedArray[T]`/`ReadSignedArray[T]`) or
  `Skip()` to consume the value. `Skip` on a sequence-start descends the whole
  nested scope. This is true streaming — one field is held at a time.
- **Visitor** ([`Visitor`]) — implement one typed method per wire kind and call
  `Decoder.Accept(v)` or the package function `AcceptBytes(buf, v)`; the decoder
  drives, dispatching each field (nested sequences descend into the visitor
  returned by `BeginSequence`). This is what generated `Unmarshal` code uses.
  Visitor array methods receive values widened to `[]uint64` / `[]int64` (or the
  concrete float slice); generated code narrows to its declared element width.

Unknown fields are simply skipped, giving forward/backward compatibility. A
fixlen *array* may only carry the `fp32` or `fp64` subtype; any other subtype
decodes to `ErrInvalidMsg`. Field ids must be `≤ IDMax` (`INT32_MAX`).

### Memory handling

This is the part that most affects how callers wire the library in. There is no
caller-supplied scratch buffer on either side.

**Encoder (write).** You hand `NewEncoder` an `io.Writer` sink — not a fixed
output buffer. The encoder accumulates bytes in a small internal slice and writes
them out in one `Write` when the slice grows past its flush threshold (and again
on `Flush`), so a message may exceed RAM and flow straight to the wire; the whole
encoded message is never held. Each write copies its bytes into that slice
(strings/blobs are *not* retained after the call returns), so the caller's source
slices/strings may be reused immediately. You **must call `Flush`** to push the
tail (and to surface a late write error); `Err` returns the first error without
flushing.

**Decoder (read).** The pull path streams; the visitor entry points buffer the
message for throughput. They also differ in whether returned bytes are copies the
caller owns or slices that alias internal storage:

| Path | Buffering | string / blob result | array & scalar result |
|------|-----------|-----------------------|-----------------------|
| `Decoder.Next` (pull) | streams one field at a time; whole message never held — decodes sources larger than RAM | `String()` and `Bytes()` both **allocate a fresh copy** the caller owns | freshly allocated `[]T` the caller owns |
| `Decoder.Accept` (visitor) | reads the whole message into one contiguous buffer, then runs a cursor over it | `String` → **fresh copy** (safe to keep); `Bytes` → **alias of the read buffer**, valid only during the call — a visitor that keeps it **must copy** | freshly allocated slice the visitor owns |
| `AcceptBytes` (visitor, zero-copy) | no slurp — the cursor advances directly over your `[]byte` | `String` → **fresh copy**; `Bytes` → **alias into the caller's `[]byte`**, so that buffer **must stay alive** as long as the blob is referenced | freshly allocated slice the visitor owns |

Takeaways: `Next` is the safe-by-default *and* streaming path — every value is a
copy the caller owns and the whole message is never held. `Accept` and
`AcceptBytes` are faster but buffer the whole message and only string values are
copied; **blob (`Bytes`) values alias** the read buffer (`Accept`) or the
caller's buffer (`AcceptBytes`), so a visitor that retains a blob past the call
(or the buffer's lifetime) must copy it. Numeric arrays are always freshly
allocated on every path.

> **Note on value width:** like the C default configuration, the scalar value
> type is 64-bit (`uint64` / `int64`), so varint encodings match byte-for-byte
> across the C, C++, Rust and Go implementations.

## Feature flags

Go always ships the **full format** — there are no build toggles, because the
desktop and server targets it is built for are not code-size constrained. The
`fixlen` (fp32/fp64/string/blob), `array`, `sequence` and `fp64` features are all
always on, and the scalar value type is fixed at 64-bit to match the C default
configuration.

## Build & test

```bash
go build ./...           # build
go vet ./...             # static analysis
go test ./...            # unit + roundtrip + example tests
go test ./... -race      # with the race detector
go test ./... -cover     # with coverage
```

Tests are split by concern: `vectors_test.go` (encode + decode against the shared
conformance suite `assets/test_vectors.json`), `streaming_test.go` (chunked /
byte-at-a-time streaming that resumes at any boundary), `encoder_test.go` and
`decoder_test.go` (byte-exact vs. the reference vectors, plus malformed-input
errors), `visitor_test.go` (the `Accept` / `AcceptBytes` visitor path),
`roundtrip_test.go` (encode→decode value preservation), and `example_test.go`
(the generated-code-style `Marshal` / `Unmarshal` walkthrough). The shared
`assets/test_vectors.json` is the cross-language source of truth, so output is
byte-identical to the C, C++ and Rust implementations. Coverage is reported by
the badge above.

## Benchmarks

`cmd/perfbench` mirrors the C/C++/Rust corelib benchmarks — same messages,
workloads, ids and values, printed in the shared format so the implementations
compare directly. Throughput is measured on **process CPU time** (user + system,
via `getrusage`), not wall-clock, matching the C tool's `clock()` and Rust's
`CLOCK_PROCESS_CPUTIME_ID`. Subcommands:

```bash
go run ./cmd/perfbench bench   # throughput table (MB/s, MB = 1e6) over a ~1s CPU-time loop
go run ./cmd/perfbench perf    # per-op cost (CPU time/op ns + MB/s) for the 12-field message
```

(`time` is accepted as an alias for `bench`.) There are also single-workload
subcommands (`encode_u64_array`, `encode_typical`, `decode_u64_array`,
`decode_typical`) that run one `//go:noinline` `run_*` function once with setup
excluded, so a Callgrind harness can toggle collection on `main.run_<workload>`
exactly as the C/C++/Rust tools do.

For a Go-native view, the decode path also has `go test` benchmarks in
`decode_bench_test.go` covering `Accept` and the zero-copy `AcceptBytes`:

```bash
go test -run '^$' -bench BenchmarkDecode -benchmem -count=8 -cpu=1 -benchtime=300ms
```

See [`results.md`](results.md) for a recorded run of the visitor-decode
optimization (contiguous-buffer cursor vs. the byte-at-a-time reader).

[`Encoder`]: https://sofa-buffers.github.io/corelib-go/
[`Decoder`]: https://sofa-buffers.github.io/corelib-go/
[`Visitor`]: https://sofa-buffers.github.io/corelib-go/
[`Field`]: https://sofa-buffers.github.io/corelib-go/
