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
(*Sofab*) serialization format — a compact, TLV-like binary format. Both sides
work in arbitrarily small chunks, so a message may be larger than RAM. This is
the runtime core; the usual caller is code `sofabgen` generates from a schema,
the way protobuf-go's generated code calls its runtime.

### Requirements

Go **1.21+** (CI builds on `1.21` and current stable). The module path is
`github.com/sofa-buffers/corelib-go`; the imported package is `sofab`.

```bash
go get github.com/sofa-buffers/corelib-go
```

```go
import sofab "github.com/sofa-buffers/corelib-go"
```

### Dependencies

**None** — standard library only (`bufio`, `encoding/binary`, `errors`, `io`,
`math`, `unicode/utf8`). No third-party modules, no `cgo`.

### Feature flags

Go always ships the full wire format; there are no build-time toggles for wire
features. Two policies are configurable, and neither changes a byte on the wire.

| Option | Default | Effect |
|--------|---------|--------|
| `WithStrictUTF8(bool)` (`SOFAB_STRICT_UTF8`) | on | Reject an invalid-UTF-8 `string`: `ErrArgument` on encode, `ErrInvalidMsg` where one is read on decode. Off stores and writes the bytes verbatim — never lossy, never replaced. |
| `WithPassThrough(bool)` | off | Allow a `string`/blob payload larger than the buffer to reach the sink directly instead of being copied through it — see [Memory handling](#memory-handling). |
| `-tags sofab_no_strict_utf8` | check compiled in | Footprint build: compiles the validator out everywhere — `Decoder.String`, `Encoder.WriteString` and `UTF8Valid` alike — and wins over `WithStrictUTF8`. CI builds and tests this leg; the default build is the one tested against the shared vectors. |

Pass the options to `NewEncoder`, `NewDecoder` or `AcceptBytes`.

**A string handed to a visitor is checked at the destination.** `Decoder.String`
validates internally. `Accept` / `AcceptBytes` / `AcceptStream` pass the wire
bytes to `Visitor.String` verbatim, because the cursor cannot tell a field the
visitor binds from one it ignores. Embed `sofab.StringCheck` to receive this
decode's policy, so flipping the option never means regenerating code:

```go
type Msg struct {
	sofab.StringCheck // adds SetStringCheck + UTF8Valid
	Name string
}

func (m *Msg) String(id sofab.ID, v string) error {
	if id == 1 {
		if !m.UTF8Valid([]byte(v)) { // this decode's policy, not the build's
			return sofab.ErrInvalidMsg
		}
		m.Name = v
	}
	return nil
}
```

The zero value is strict, so a destination that is handed no policy validates
anyway. The package-level `UTF8Valid(b []byte) bool` is the always-strict form.
Framing — the fixlen word, reserved subtypes, `ARRAY_MAX`, `MaxDepth`, varint
overflow — is checked on every field regardless; only the content check moves.

## Why this design

| Goal | How |
|------|-----|
| Streaming **out** | `Encoder` writes into an output buffer drained as it fills. The buffer is the caller's (`NewEncoderBuffer` / `NewEncoderSink`, with a start offset for a framing header); `NewEncoder` over an `io.Writer` allocates one for you. |
| Streaming **in** | `Decoder` is a pull parser over any `io.Reader`; `Next()` returns one field header at a time. |
| Three decode styles | Pull with `Decoder.Next`; implement `Visitor` and call `Decoder.Accept`, which binds each field into a struct member, with `AcceptBytes` as its zero-copy form for a message already in a `[]byte`; or `Decoder.AcceptStream`, the same visitor events driven off a reader with a peak memory of one field. |
| No dependencies | Standard library only, no `cgo`. |
| Sticky errors | The encoder records the first failure and turns later writes into no-ops, so generated code can issue a run of writes and check once at `Flush`. |
| Generics for arrays | `WriteUnsignedArray[T]` / `ReadUnsignedArray[T]` (and the signed variants) accept any `~uint8..~uint64` / `~int8..~int64`; float arrays have dedicated methods. On decode `T` is also the declared-width bound — see [Deserialize](#deserialize). |
| Forward/backward compatible | Unknown fields are consumed with `Skip()`: old readers tolerate new fields, new readers tolerate missing ones. |
| Canonical sequence framing | `WriteSequenceBeginLazy` holds a sequence header back until the sequence gets content, so an all-default sequence **field** is omitted rather than framed empty — in one forward pass, without buffering. A wrapper-array **element** keeps its frame even when all-default, since element presence carries the array's length; it closes with `WriteSequenceEndKeep`. |

## Usage

The four cases below — encode a message that fits one buffer, encode one that
does not, decode a whole message, decode one arriving in chunks — mirror what
generated code does (see [Code generator](#code-generator)).

### Serialize

Write fields into an `Encoder` and `Flush` to push the tail:

```go
var buf bytes.Buffer
e := sofab.NewEncoder(&buf)
e.WriteUnsigned(1, 42)
e.WriteSigned(2, -7)
e.WriteString(3, "hi")
if err := e.Flush(); err != nil { /* ... */ }
msg := buf.Bytes()
```

A write that could not produce a valid field writes **nothing** and reports
`ErrArgument`. The error is sticky, so generated code still checks once at
`Flush`. These are properties of the wire format and hold for every build:

| Refused | |
|---|---|
| an `id` past `IDMax` | `ID_MAX` binds every field header |
| a `string`/blob past `FIXLEN_MAX`, or an array past `ARRAY_MAX` (both 2³¹−1) | representable in Go, not on the wire |
| opening a sequence past `MaxDepth` (255) | a message nesting deeper is invalid |
| closing a sequence when none is open | a bare `0x07` is an unbalanced end marker |
| a non-UTF-8 `string`, unless the check is off | see [Feature flags](#feature-flags) |

### Serialize stream

`NewEncoder` takes any `io.Writer` — socket, pipe, file, `gzip.Writer` — and
flushes an internal window as it fills:

```go
conn, _ := net.Dial("tcp", "collector:9000")
e := sofab.NewEncoder(conn)
for i := uint32(0); i < 1_000_000; i++ {
    e.WriteUnsigned(sofab.ID(i%128), uint64(i))
}
e.Flush() // push the tail, and surface a late write error
```

#### Serialize into a buffer you own

Two constructors take the caller's buffer instead, both with a start offset that
reserves room at the front — enough to encode straight into a packet behind its
framing header, with no second buffer and no copy:

```go
// (a) no sink: the buffer holds the message, or the encode reports ErrBufferFull.
buf := make([]byte, 4+maxSize)
e, _ := sofab.NewEncoderBuffer(buf, 4)  // 4 bytes reserved for a length header
e.WriteUnsigned(1, 42)
if err := e.Flush(); err != nil { /* ErrBufferFull if it did not fit */ }
binary.BigEndian.PutUint32(buf[:4], uint32(len(e.Bytes())))

// (b) with a flush sink: the buffer may be arbitrarily smaller than the message.
scratch := make([]byte, 512)            // >= sofab.MinOutputBuffer
e, _ = sofab.NewEncoderSink(scratch, 0, func(_ *sofab.Encoder, b []byte) error {
    _, err := conn.Write(b)             // this sink copies, so it just returns
    return err
})
```

A sink may instead **take** the buffer it is handed, provided it installs a
replacement with `SetBuffer` before returning. Returning without one means it
copied, and the encoder writes on into the same buffer at offset 0:

```go
e, _ := sofab.NewEncoderSink(pool.Get(), 4, func(enc *sofab.Encoder, b []byte) error {
    queue <- b                          // the transport owns b now ...
    return enc.SetBuffer(pool.Get(), 4) // ... so hand the encoder another
})
```

### Deserialize

`Decoder` is a pull parser: `Next()` returns one field header at a time; read
the value with a typed accessor, or `Skip()` it:

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

`Next` owns the stream's nesting state, so the loop needs no depth bookkeeping:
it refuses a sequence nesting past `MaxDepth` (255) and an end marker that
closes nothing, both `ErrInvalidMsg`. A balanced end marker arrives as a
`TypeSequenceEnd` header with `ID` always **0**.

Three decode behaviours are worth knowing before you write the loop:

* **An integer array's element type bounds validity.**
  `ReadUnsignedArray[T]` / `ReadSignedArray[T]` take the schema's element type
  as `T`, and an element the width cannot hold is `ErrInvalidMsg`, never quietly
  masked down — `300` read as `[]uint8` rejects the message rather than yielding
  `44`. On the visitor surface the bound arrives through `ElemBoundVisitor`.
* **A reader bound to the wrong wire type is not an error.** It returns
  `ErrTypeMismatch`: the field is skipped exactly like one with an unknown id,
  your destination is untouched, and the decode stays valid — the same bytes
  decode fine for a peer whose schema declares the other type. Framing still
  wins: a reserved fixlen subtype or a count past `ARRAY_MAX` is `ErrInvalidMsg`
  either way.
* **Receiver-side limits stop at a schema bound** (below).

```go
case f.ID == 3:
    s, err := d.String()
    if errors.Is(err, sofab.ErrTypeMismatch) { break } // keep the default
    if err != nil { /* ErrInvalidMsg / ErrIncomplete / ErrLimitExceeded */ }
    _ = s
```

#### Receiver-side limits

`WithMaxArrayCount` / `WithMaxStringLen` / `WithMaxBlobLen` cap what a decode
will materialize from a size the *sender* chose. Exceeding one is
`ErrLimitExceeded` — deployment policy, never `ErrInvalidMsg`, since the same
bytes decode under a looser cap — and it is enforced at the count or length
word, before any allocation.

They apply to schema-**unbounded** fields only. Where the schema states a
`count:`/`maxlen:`, that bound governs and its violation is `ErrInvalidMsg`.
Only the schema knows which fields those are, so the destination says so, and
saying so is a promise to enforce:

```go
// Generated code declares the bound ...
func (m *Msg) SchemaBound(id sofab.ID, what sofab.BoundKind) bool {
    return id == 4 && what == sofab.BoundArrayCount // schema: count: 10000
}

// ... and enforces it at the header, which is what replaces the cap.
func (m *Msg) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
    if id == 4 && count > 10000 { return sofab.ErrInvalidMsg }
    return nil
}
```

A 5000-element array on field 4 then decodes even under
`WithMaxArrayCount(1000)`, while every unbounded field stays capped. On the pull
surface the caller knows the schema and says so per field with
`d.SchemaBounded()`.

### Deserialize stream

The same loop reads a stream: hand `NewDecoder` any `io.Reader` and it refills
on demand, so the chunk boundaries live in the reader rather than in your code.

```go
d := sofab.NewDecoder(conn)  // socket, pipe, gzip.Reader, os.Stdin
for {
    f, err := d.Next()
    if err == io.EOF { break }
    if err != nil { /* ... */ }
    switch {
    case f.ID == 1: id, _ := d.Unsigned(); _ = id
    default:        d.Skip()
    }
}
```

A visitor streams too. `Accept` reads the whole message into one buffer before
parsing it; `AcceptStream` reads and dispatches each field as the reader
delivers it, so peak memory is the largest single field. Same events, same
verdict:

```go
d := sofab.NewDecoder(conn)
m := &Point{}
if err := d.AcceptStream(m); err != nil {
    /* ErrInvalidMsg / ErrIncomplete / a reader error, verbatim */
}
```

Every `string` and blob it hands the visitor is fresh storage the visitor may
keep — see [Memory handling](#memory-handling).

### Code generator

The common case is driving the runtime through generated object code. Per
message `sofabgen` emits the streaming pair `Serialize(*sofab.Encoder)` and the
`sofab.Visitor` methods, plus the one-shot `Encode()` / `Decode<Name>(data)`
wrappers and their streaming twins `EncodeTo(io.Writer)` /
`Decode<Name>From(io.Reader)`. A hand-written stand-in:

```go
// generated by: sofabgen --lang go
type Point struct {
	_visitorBase       // default no-op Visitor methods
	X            int32 `json:"x"`
	Y            int32 `json:"y"`
}

func (m *Point) Serialize(e *sofab.Encoder) {
	e.WriteSigned(1, int64(m.X))
	e.WriteSigned(2, int64(m.Y))
}

func (m *Point) Signed(id sofab.ID, v int64) error {
	switch id {
	case 1:
		m.X = int32(v)
	case 2:
		m.Y = int32(v)
	}
	return nil
}

func (m *Point) Encode() ([]byte, error) {
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	m.Serialize(e)
	if err := e.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func DecodePoint(data []byte) (*Point, error) {
	m := &Point{}
	if err := sofab.AcceptBytes(data, m); err != nil {
		return nil, err
	}
	return m, nil
}
// use:
wire, _ := (&Point{X: 3, Y: 4}).Encode()
got, _ := DecodePoint(wire) // got.X == 3, got.Y == 4
```

Generated code brings no array machinery of its own. An array of strings, blobs,
structs/unions or arrays arrives as a nested sequence whose child ids are the
element indices, and rebuilding a slice from those events is the same code for
every schema, so it lives here: `VisitorBase`, `StringSeq` / `BlobSeq` /
`MessageSeq` / `NestedSeq`, the matrix collectors with `PlaceRow`, and
`PayloadAcc` for a payload arriving in chunks. The generator switches over in
[generator#345](https://github.com/sofa-buffers/generator/issues/345); both
spellings compile against this package meanwhile.

## Memory handling

**Encoder — output buffer.** Every write copies its bytes into the active
buffer, so caller strings and slices may be reused immediately on every form.
You **must call `Flush`** to push the tail and surface a late write error.

| Constructor | Who owns the buffer | When it fills |
|---|---|---|
| `NewEncoder(w)` | this package: a 512 B window, grown once to 4 KiB by the first message that outgrows it | flushed to `w`, then reused |
| `NewEncoderBuffer(buf, offset)` | you | nothing to flush to — the encode stops with `ErrBufferFull` |
| `NewEncoderSink(buf, offset, sink)` | you | handed to `sink`, then reused, or replaced via `SetBuffer` |

A caller-supplied buffer is never grown, reallocated or replaced behind your
back, and the encoder writes only between `offset` and the end of it. `Bytes()`
returns what has been written into the active buffer since it was installed.

**`MIN_OUTPUT_BUFFER` = 20** (`sofab.MinOutputBuffer`) is the smallest buffer
accepted **for streaming**: `len(buf)-offset` must be at least 20 for a buffer
installed *with a sink*, at `NewEncoderSink` and at every mid-stream
`SetBuffer`, and an undersized one is rejected there with `ErrArgument` rather
than partway through a message. The value is what this encoder reserves as one
contiguous piece — a field header varint plus a 64-bit value varint, 2 × 10. It
never splits an atomic unit; a `string`/blob payload it splits freely, so a
payload far larger than the buffer streams through it. A buffer installed
**without** a sink has no minimum: a message that encodes to two bytes encodes
into a two-byte buffer.

**Pass-through** (`WithPassThrough`, off by default everywhere) hands a
`string`/blob payload larger than the buffer to the sink directly, after the
buffered bytes so wire order is unchanged. Such a call hands the sink memory
that is **not** the output buffer: it is borrowed for the duration of the call
and must not be retained, and it is never a buffer handover — `SetBuffer` is
rejected while the permission is granted, since a sink cannot tell the two calls
apart. The bytes on the wire are identical either way.

**Decoder.** The pull path is safe by default and streaming: `String()` and
`Bytes()` both return fresh copies. `Accept` / `AcceptBytes` buffer the whole
message and are faster, but only strings are copied — blob values alias the read
buffer or the caller's `[]byte`. `AcceptStream` buffers no message at all, so
there is nothing to alias and both its strings and blobs are fresh storage.

| Path | `String` | `Bytes` (blob) |
|------|----------|----------------|
| `Next` (pull, streaming) | fresh copy | fresh copy |
| `Accept` | fresh copy | aliases read buffer — copy to keep |
| `AcceptBytes` | fresh copy | aliases caller's `[]byte` — keep it alive |
| `AcceptStream` (visitor, streaming) | fresh copy | fresh copy |

Numeric arrays are always freshly allocated, but only the output slice is: no
reader path allocates per element. Varint elements are decoded in batches out of
the reader's own buffer and fp32/fp64 elements are read in place, so a
1000-element array costs that slice's growth and nothing else.

## Build & test

```bash
go build ./...           # build
go vet ./...             # static analysis
go test ./...            # unit + roundtrip + example tests
go test ./... -race      # with the race detector
go test ./... -cover     # with coverage

# The documented footprint build, with the validator compiled out. CI runs it too.
go test -tags sofab_no_strict_utf8 ./...
```

Tests cover the shared conformance suite (`vectors_test.go`), chunked and
byte-at-a-time streaming that resumes at any boundary (`streaming_test.go`),
byte-exact encode/decode, the visitor path (`visitor_test.go`), roundtrip value
preservation, and a generated-code-style walkthrough (`example_test.go`). The
malformed and truncated inputs are declared once as `malformedCases` in
`malformed_test.go` and driven through every decode surface, so a new case holds
all three by construction. The docs are tested too: `readme_shape_test.go`,
`closed_names_test.go` and `docs_decode_paths_test.go` check this README against
the structure, the closed generated-object name set, and the decode entry points
the package actually exports.

## Benchmarks

`cmd/perfbench` runs the shared corelib workloads in the common output format,
so implementations compare directly. Throughput is measured on process CPU time
(user + system), not wall-clock.

```bash
go run ./cmd/perfbench bench   # throughput table (MB/s, MB = 1e6) over a ~1s CPU-time loop
go run ./cmd/perfbench perf    # per-op cost (ns/op + MB/s) for the 12-field message
```

`bench` prints one row per shared dataset, each driven through the API a real
caller uses:

| Row | How it is driven |
|---|---|
| `encode: u64 array (1000)`, `encode: typical message`, `encode: composite` | a caller-supplied buffer (`NewEncoderBuffer`), no sink |
| `encode: blob 1MB one-shot` | one caller buffer of exactly 1,000,005 bytes, no sink |
| `encode: blob 1MB streaming` | a 4096-byte caller buffer with a flush sink, so the megabyte is copied through it in ~245 flushes |
| `encode: blob 1MB passthrough` | the same with `WithPassThrough(true)`: the payload goes to the sink directly |
| `decode: blob 1MB` | fed to `AcceptStream` in 4096-byte chunks, so peak memory is one field |
| `decode: composite` / `decode: composite skip-all` | the whole message read into a visitor, versus a pull loop that `Skip`s every field |

The three `blob 1MB` rows are bandwidth-bound — a million of their bytes are
payload — so read them against each other rather than as a statement about the
library. The `composite` message is the one that reaches what the flat datasets
do not: a wrapper array of 64 string elements, 320 bytes of 1- to 4-byte UTF-8,
nesting at depth 3, a field equal to its default that the encoder must omit, and
a two-byte field header. Its encoded size is 956 bytes on every port, as the
`perf` message's is 170 and the blob's 1,000,005 — the three cross-language
parity checks.

### Instruction cost

`run_callgrind.sh` is the third tool of the shared set, reporting
machine-independent instructions per op. Every workload is also a single-shot
subcommand (`perfbench workloads` lists them) that runs the op warm first, so
the reported number is a steady-state cost with setup excluded.

```bash
bash bench/run_callgrind.sh           # Ir/op table over every workload
bash bench/profile.sh decode_typical  # per-function breakdown of one workload
```

Both need `valgrind`, which the dev container installs. `go test` benchmarks in
`decode_bench_test.go` and `encode_bench_test.go` drive two workloads through
each API shape with `-benchmem`, so an allocation added to a hot path shows up
as a number:

```bash
go test -run '^$' -bench . -benchmem -count=8 -cpu=1 -benchtime=300ms
```

Use `-count=8` with `benchstat`: on a shared machine a single run varies by more
than most codec changes are worth.
