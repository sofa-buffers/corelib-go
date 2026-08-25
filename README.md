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
features. One policy is configurable, and it changes no byte on the wire.

| Option | Default | Effect |
|--------|---------|--------|
| `WithStrictUTF8(bool)` (`SOFAB_STRICT_UTF8`) | on | Reject an invalid-UTF-8 `string`: `ErrArgument` on encode, `ErrInvalidMsg` where one is read on decode. Off stores and writes the bytes verbatim — never lossy, never replaced. |
| `-tags sofab_no_strict_utf8` | check compiled in | Footprint build: compiles the validator out everywhere — `Encoder.WriteString` and `UTF8Valid` alike — and wins over `WithStrictUTF8`. CI builds and tests this leg; the default build is the one tested against the shared vectors. |

Pass the options to `NewEncoder`, `NewDecoder` or `AcceptBytes`.

**A string is checked at the destination.** The decoder passes the wire bytes to
`Visitor.String` verbatim, and never builds a `string` of its own, because it
cannot tell a field the visitor binds from one it ignores — and a field that is
only skipped must never be validated.
Embed `sofab.StringCheck` to receive this
decode's policy, so flipping the option never means regenerating code:

```go
type Msg struct {
	sofab.VisitorBase
	sofab.StringCheck  // adds SetStringCheck + UTF8Valid
	pay  sofab.PayloadAcc
	Name string
}

func (m *Msg) String(id sofab.ID, total, offset int, chunk []byte) error {
	if id != 1 {
		return nil
	}
	b, done := m.pay.Take(total, offset, chunk) // sofab.PayloadAcc
	if !done {
		return nil // more pieces to come
	}
	if !m.UTF8Valid(b) { // this decode's policy, not the build's
		return sofab.ErrInvalidMsg
	}
	m.Name = string(b)
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
| Streaming **out** | `Encoder` writes into an output buffer drained as it fills. The buffer is the caller's (`NewEncoderBuffer` / `NewEncoderSink`, with a start offset for a framing header); `NewEncoder` over an `io.Writer` sizes a fixed scratch window for you, once, at construction. |
| Streaming **in** | `Decoder.Feed` takes bytes in chunks of any size — one byte included — and returns `Complete` / `Incomplete` / `Invalid` for everything consumed so far. A header, a varint, a payload or an array may be split across any number of calls; the machine suspends and resumes at any byte boundary, and there is no finish step. |
| One decode surface | The visitor, and nothing else — no pull parser, no iterator, no cursor — and behind it ONE implementation: `Feed`. `AcceptBytes` is a single `Feed` of a whole buffer and `Decoder.FeedFrom` is a read loop over your own scratch buffer. One surface is one place to be correct. |
| Nothing the decoder hands you is storage | A payload arrives in pieces (`FixlenBegin`, then `String`/`Bytes` with the total, this piece's offset and the piece); an array arrives as `ArrayBegin`, one callback per element, `ArrayEnd`. The decoder therefore sizes nothing from the wire and **allocates nothing at all after construction**. |
| No dependencies | Standard library only, no `cgo`. |
| Sticky errors | The encoder records the first failure and turns later writes into no-ops, so generated code can issue a run of writes and check once at `Flush`. |
| Generics for arrays | `WriteUnsignedArray[T]` / `WriteSignedArray[T]` accept any `~uint8..~uint64` / `~int8..~int64`; float arrays have dedicated methods. |
| Forward/backward compatible | A field the visitor binds nothing to is walked, not built: old readers tolerate new fields, new readers tolerate missing ones. Returning `nil` from `BeginSequence` declines a whole sub-tree. |
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

**The visitor is the only decode surface.** You implement `sofab.Visitor` on the
type the fields land in, and the decoder drives — writing each value straight
into a member you own. There is no pull parser, no iterator and no cursor: one
surface means every rule in the format is implemented once.

```go
type Point struct {
    sofab.VisitorBase // no-op defaults for the callbacks this message does not use
    X, Y int32
}

func (m *Point) Signed(id sofab.ID, v int64) error {
    switch id {
    case 1:
        m.X = int32(v)
    case 2:
        m.Y = int32(v)
    }
    return nil // an id this schema does not declare is simply not bound
}

m := &Point{}
if err := sofab.AcceptBytes(msg, m); err != nil {
    /* ErrInvalidMsg / ErrIncomplete / ErrLimitExceeded */
}
```

**Aggregates arrive in pieces**, because a callback carrying a whole value would
oblige the decoder to build it from a size the sender chose:

| Field | Callbacks, in order |
|---|---|
| `string` / `blob` | `FixlenBegin(id, subtype, total)`, then one or more `String`/`Bytes(id, total, offset, chunk)` — one call when the payload arrives whole, as many as it took to feed when it does not. An empty payload is one call with `total == 0`. |
| any array | `ArrayBegin(id, kind, count)`, then `ArrayUnsigned` / `ArraySigned` / `ArrayFloat32` / `ArrayFloat64` per element, then `ArrayEnd(id)`. An empty array is `ArrayBegin` straight to `ArrayEnd`; a truncated one never reaches `ArrayEnd`. |

`sofab.PayloadAcc` assembles a payload out of its pieces if you want it whole.
`FixlenBegin` and `ArrayBegin` fire **before any payload byte or element**, which
is where a schema `maxlen:`/`count:` belongs: a message that breaches one and is
then truncated must still be `ErrInvalidMsg`.

The decoder owns the stream's nesting state, so nothing in your callbacks does
depth bookkeeping: it refuses a sequence nesting past `MaxDepth` (255) and an end
marker that closes nothing, both `ErrInvalidMsg`. A sequence-end marker's id is
discarded (§4.9), so `EndSequence` carries none.

Three decode behaviours are worth knowing before you write the callbacks:

* **A field the destination does not bind is not an error.** An unknown id, and
  a field whose wire type contradicts the one your schema declares
  (MESSAGE_SPEC §7.3), simply land in no arm: the destination is untouched and
  the decode stays valid — the same bytes decode fine for a peer whose schema
  declares the other type. Framing still wins: a reserved fixlen subtype or a
  count past `ARRAY_MAX` is `ErrInvalidMsg` either way.
* **An integer array's element type bounds validity.** Elements arrive one at a
  time, widened to 64 bits, and narrowing them is yours to check: an element the
  schema's width cannot hold must be `ErrInvalidMsg`, never quietly masked down.
  Check it in `ArrayUnsigned` / `ArraySigned` as the element goes past — that is
  what keeps a message INVALID rather than INCOMPLETE when the array behind the
  offending element is truncated.
* **Receiver-side limits are yours to apply** (below).

**A sub-tree you have no destination for is declined whole.** Return `nil` from
`BeginSequence` and nothing under it is delivered and nothing under it is built,
however deep it goes — the bytes are still parsed, so the field after it resyncs
exactly.

```go
func (m *Point) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
    if id == 7 {
        return &m.Meta, nil // descend into the nested object
    }
    return nil, nil // decline: walk it, build nothing
}
```

#### Receiver-side limits

`max_dyn_array_count` / `max_dyn_string_len` / `max_dyn_blob_len` cap what a
decode will materialize from a size the *sender* chose. Exceeding one is
`ErrLimitExceeded` — deployment policy, never `ErrInvalidMsg`, since the same
bytes decode under a looser cap.

**The decoder holds no cap and there is no option that gives it one.** The
numbers belong to the layer that knows the schema and the deployment, so the
*destination* applies them, at the count or length word, before any allocation.
Where the schema states a `count:`/`maxlen:` that bound governs instead, and its
violation is `ErrInvalidMsg`; the two are never both in play on one field.

For a scalar or a native array that is your own callback, one `if` per arm:

```go
func (m *Msg) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
    switch id {
    case 4: // schema: count: 10000
        if count > 10000 { return sofab.ErrInvalidMsg }
    case 5: // schema declares no count
        if count > maxDynArrayCount { return sofab.ErrLimitExceeded }
    }
    return nil
}
```

For a wrapper array the collector owns the whole thing, so the numbers are
fields on it: `Cap`/`ElemMax` are the schema bounds and `RCap`/`RElemMax` the
receiver caps beside them, with the matrix collectors adding `RowCount`/`RowCap`
for a row's own element count. A schema field is non-positive when the schema
declares none; the R field beside it is then consulted, and a non-positive R
field falls back to `ARRAY_MAX` — the format ceiling, never "unlimited".

```go
&sofab.StringSeq{
    Out: &m.Tags,
    Cap: -1, ElemMax: -1, // schema declares neither
    RCap: maxDynArrayCount, RElemMax: maxDynStringLen,
}
```

A field the destination never binds — an unknown id, or a wire type that
contradicts the schema — is capped by nothing, which is the point: it is walked,
not materialized, so a decode that steps over an over-cap field it was never
going to read stays complete.

### Deserialize stream

The same visitor reads a stream, through the surface everything else is built
on: hand `Feed` whatever bytes you have. Every call returns the outcome for the
bytes consumed so far, so there is no finish step — the status *is* the answer,
and whether an `Incomplete` is a truncation is your framing's call, not the
decoder's.

```go
d := sofab.NewDecoder(m) // m is the *Point above; construct once, Reset per message
for {
    n, err := conn.Read(buf)
    if n > 0 {
        switch out, derr := d.Feed(buf[:n]); out {
        case sofab.Invalid:
            return derr // ErrInvalidMsg / ErrLimitExceeded / your own visitor error
        case sofab.Complete:
            // a valid message may end here; more valid fields could extend it
        case sofab.Incomplete:
            // the bytes end inside a field — feed the next chunk
        }
    }
    if err != nil {
        break
    }
}
```

`Decoder.FeedFrom(r, scratch)` is that loop, for an `io.Reader`. The scratch
buffer is yours: this package sizes no buffer from a stream.

**One decoder, many messages.** Construction is the only allocating step;
`Reset(v)` rebinds a destination and clears every trace of the message before it,
allocating nothing. `AcceptBytes` constructs one per call, which is the single
allocation on that path.

Whatever a callback receives is valid only until it returns — see
[Memory handling](#memory-handling).

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
`PayloadAcc` for the pieces a payload arrives in. Each takes its bounds as
fields — the schema's `count:`/`maxlen:` and the receiver caps beside them (see
[Receiver-side limits](#receiver-side-limits)) — so a corelib that knows no
schema still applies the schema's numbers.

## Memory handling

**Encoder — output buffer.** Every write copies its bytes into the active
buffer, so caller strings and slices may be reused immediately on every form.
You **must call `Flush`** to push the tail and surface a late write error.

| Constructor | Who owns the buffer | When it fills |
|---|---|---|
| `NewEncoder(w)` | this package: a fixed 512 B scratch window, sized once at construction and never grown | flushed to `w`, then reused |
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

A sink is **only ever** handed memory inside the installed output buffer. There
is no second case to handle: a `string` or blob payload is copied through the
buffer however large it is, and there is no option that changes that.

**Decoder — input bytes, and nothing that outlives the call.** The bytes being
parsed are yours: the chunk you passed to `Feed`, or the scratch buffer
`FeedFrom` reads into. A chunk is borrowed only for the duration of that one
call — once it returns you may reuse, overwrite or free it and the decode is
unaffected.

**Whatever a callback receives is valid only until that callback returns** — a
caller that keeps a value copies it first. That holds on the one-shot path
exactly as on the streaming one; no value the decoder produces is readable after
the call that delivered it, and there is no payload-position getter and no
"valid until the next feed" value. A `String`/`Bytes` chunk is a window into the
bytes *you* fed, so keeping it means copying it — exactly as `io.Reader.Read`'s
contract works.

**The codec allocates nothing after construction, on either side.** No buffer,
no accumulator and no destination is sized from a wire count or a wire length,
anywhere. That is what the piecewise callback surface buys: a value that needs
storage is handed over in pieces and the *destination's* storage takes it.
`TestNoAllocationsAfterConstruction` asserts zero for every encoder form and
`TestDecodeAllocatesNothingAfterConstruction` asserts zero for a decode — a
small message and a 12 KB one, whole and one byte at a time.

The decoder's own state is sized once, in `NewDecoder`: the parse stack to
`MaxDepth`, the landing zone to the widest fixlen element. Nothing on the wire
chooses those sizes and nothing grows afterwards. `AcceptBytes` constructs a
decoder per call — the one allocation on that path, and it does not move with
the message (`TestAcceptBytesAllocatesOnlyItsDecoder`).

`collectors.go` and `payload.go` are the **static helper layer**: they ship here
for reuse by the generated layer, are never used by the codec itself, and
allocate on the generated layer's behalf. A wrapper array's length is only known
when the array ends, so `StringSeq`/`BlobSeq`/`MessageSeq` grow their container
as elements arrive — geometrically, via Go's `append`, so a sparse array does
not cost O(n²) copies (`TestSequenceGrowthIsGeometric`). That is the one
allocation shape where growth is correct, and it is deliberately outside the
codec. It is also why the receiver caps live on the collectors: a cap bounds an
allocation, and this is the layer that makes one.

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
byte-exact encode/decode, the visitor path (`visitor_test.go`), the push surface
and its chunk invariance (`feed_test.go`), roundtrip value preservation, and a
generated-code-style walkthrough (`example_test.go`). The malformed and
truncated inputs are declared once as `malformedCases` in `malformed_test.go`
and driven at every chunking, so a new case holds them all by construction. `alloc_conformance_test.go` carries the
allocation measurement, and `readme_example_test.go` compiles and runs the
generated-code example in this README so the docs cannot drift from the API.

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
| `decode: blob 1MB` | fed to `Feed` in 4096-byte chunks and copied into a destination sized once at setup, so peak memory is that destination and not the message |
| `decode: composite` / `decode: composite skip-all` | the whole message read into a visitor, versus one that declines every sub-sequence |

The two `blob 1MB` rows are bandwidth-bound — a million of their bytes are
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
