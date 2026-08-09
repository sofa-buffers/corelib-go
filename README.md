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
| Streaming **out** | [`Encoder`] writes into an output buffer drained as it fills — a message can exceed RAM and stream straight to a socket or file. The buffer is the caller's (`NewEncoderBuffer` / `NewEncoderSink`, with a start offset for a framing header, CORELIB_PLAN §5.1); `NewEncoder` over any `io.Writer` is the convenience form that allocates one for you. |
| Streaming **in** | [`Decoder`] is a pull parser over any `io.Reader`; `Next()` returns one field header at a time, never materializing the whole message. |
| Two decode styles | Pull with `Decoder.Next`, or implement [`Visitor`] and call `Decoder.Accept`, which binds each field into a struct member — what generated `Unmarshal` uses. `AcceptBytes` is the zero-copy form for a message already in a `[]byte`. |
| No dependencies | Standard library only, no `cgo`. |
| Sticky errors | The encoder records the first failure and turns later writes into no-ops, so generated `Marshal` can issue a run of writes and check once at `Flush`. |
| Generics for arrays | `WriteUnsignedArray[T]` / `ReadUnsignedArray[T]` (and signed variants) accept any `~uint8..~uint64` / `~int8..~int64` element type; float arrays have dedicated methods. On decode `T` doubles as the declared-width bound (MESSAGE_SPEC §7.1) — see [below](#an-integer-arrays-declared-element-width). |
| Forward/backward compatible | Unknown fields are consumed with `Skip()` — old readers tolerate new fields, new readers tolerate missing ones. |
| Canonical sequence framing | `WriteSequenceBeginLazy` holds a sequence header back until the sequence gets content, so an all-default sequence **field** is omitted rather than framed empty (MESSAGE_SPEC §2) — in one forward pass, without buffering the sub-message. A wrapper-array **element** keeps its frame even when all-default, since element presence is what carries a dynamic array's length (§5.1); it closes with `WriteSequenceEndKeep`, which forces the frame out. The hold-back has no depth bound of its own: the pending run grows with the nesting, so every sequence up to `MaxDepth` is canonical (CORELIB_PLAN §6). |

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

#### What the encoder refuses to write

The encoder never hands back bytes no decoder could read: a write that cannot
produce a valid field produces **nothing** and reports `ErrArgument`
(`InvalidArgument`, CORELIB_PLAN §6.3). The error is sticky like any other, so
generated `Marshal` code still checks once at `Flush`.

| Refused | Why |
|---|---|
| an `id` past `IDMax` | `ID_MAX` binds every field header (CORELIB_PLAN §6.2). |
| a `string`/blob longer than `FIXLEN_MAX`, or an array longer than `ARRAY_MAX` (both 2³¹−1) | a >2 GiB payload is representable in Go but not on the wire: the length or count word would go out looking well-formed and every decoder — this package's own included — would reject the message as `ErrInvalidMsg` (§6.2). |
| opening a sequence past `MaxDepth` (255) | a message nesting deeper is `INVALID` (§4.9). |
| closing a sequence when none is open | a bare `0x07` is an unbalanced sequence end, `INVALID` for every decoder (§4.9, §6.3) — the framing balance is the encoder's to keep in both directions. |
| a non-UTF-8 `string` (unless the check is off) | see [Feature flags](#feature-flags) (§6.4). |

None of these is a receiver-side limit: they are properties of the wire format,
so they hold for every build and none of them is configurable.

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

#### Serialize into a buffer you own

`NewEncoder` allocates that window itself, which is convenient but is not the
model the format is specified against: CORELIB_PLAN §5.1 has the **caller** own
the output buffer, so the encoder can write straight into a packet — behind a
framing header, without a second buffer and without a copy. Two constructors take
one, and both accept a **start offset** that reserves room at the front:

```go
// (a) no sink: the buffer holds the message, or the encode reports ErrBufferFull.
buf := make([]byte, 4+maxSize)
e, _ := sofab.NewEncoderBuffer(buf, 4)           // 4 bytes reserved for a length header
e.WriteUnsigned(1, 42)
if err := e.Flush(); err != nil { /* ErrBufferFull if it did not fit */ }
msg := e.Bytes()                                 // the bytes written, inside buf
binary.BigEndian.PutUint32(buf[:4], uint32(len(msg)))
packet := buf[:4+len(msg)]                       // one buffer, no copy

// (b) with a flush sink: the buffer may be arbitrarily smaller than the message.
scratch := make([]byte, 512)                     // >= sofab.MinOutputBuffer
e, _ = sofab.NewEncoderSink(scratch, 0, func(_ *sofab.Encoder, b []byte) error {
    _, err := conn.Write(b)                      // this sink copies, so it just returns
    return err
})
```

A sink may instead **take** the buffer it is handed — queue it for an
asynchronous write, hand it to a transport — provided it installs a replacement
with `SetBuffer` before returning. Returning without one means it copied, and the
encoder writes on into the same buffer at offset 0. Passing the *same* buffer
back to `SetBuffer` is a new installation like any other, which is how a sink
re-arms its header room for every flushed unit:

```go
e, _ := sofab.NewEncoderSink(pool.Get(), 4, func(enc *sofab.Encoder, b []byte) error {
    queue <- b                                   // the transport owns b now ...
    return enc.SetBuffer(pool.Get(), 4)          // ... so hand the encoder another
})
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

`Next` also owns the stream's **nesting state**, so the pull loop above needs no
depth bookkeeping of its own: it counts the sequences the stream opens, refuses
the one that would nest past `MaxDepth` (255, CORELIB_PLAN §4.9/§6.2), and
refuses a sequence-end marker that closes nothing — both `ErrInvalidMsg` (§6.3).
The count belongs to the decoder, not to one call, so `Skip()` over a sub-tree is
held to the same absolute ceiling, and a pull loop reaches the same verdict as
`Accept` / `AcceptBytes` on the same bytes. A balanced end marker is still
delivered as an ordinary `TypeSequenceEnd` header — with `ID` always **0**,
whatever id the header on the wire spelled: an end marker's id carries no
information, so the decoder **discards** it (§4.9) rather than let a
sender-chosen number reach a pull loop that switches on `f.ID`. Discarded is not
unvalidated: an id above `IDMax` is `ErrInvalidMsg` on wire type 7 exactly as
anywhere else (§6.2).

#### An integer array's declared element width

`ReadUnsignedArray[T]` / `ReadSignedArray[T]` take the schema's element type as
`T`, and that type is a **validity bound**, not a cast: an element the declared
width cannot hold is `ErrInvalidMsg` (MESSAGE_SPEC §7.1), never quietly masked
down. An `array<u8>` carrying `300` is a rejected message, not `44`:

```go
v, err := sofab.ReadUnsignedArray[uint8](d)
// 300 on the wire => err = ErrInvalidMsg, v = nil
```

Each element is judged as it decodes, so an over-width element that is fully on
the wire outranks a truncation behind it (§5.2: INVALID beats INCOMPLETE), and
`uint64` / `int64` — which span the value domain — cost nothing, since no wire
value can breach them. The visitor surface hands over a whole `[]uint64` at once
and so cannot see the width itself; there the bound travels in through the
optional `ElemBoundVisitor` (`ArrayElemBound(id, kind)`), which generated code
implements, and both surfaces then reach the same verdict on the same bytes.
`NarrowUnsigned` / `NarrowSigned` are plain conversions for that visitor path and
assume the bound has already been applied.

#### Receiver-side limits stop at a schema bound

`WithMaxArrayCount` / `WithMaxStringLen` / `WithMaxBlobLen` cap what a decode will
materialize from a size the *sender* chose. They are deployment configuration,
not schema: exceeding one is `ErrLimitExceeded` — a policy rejection, never
`ErrInvalidMsg`, since the same bytes decode under a looser cap — and they are
enforced at the count/length word, before any element slice or payload buffer.

They apply to **schema-unbounded fields only**. Where the schema states a
`count:`/`maxlen:`, CORELIB_PLAN §6.2.1 has that bound govern and its violation is
`ErrInvalidMsg` (MESSAGE_SPEC §7.1); §6.3 adds that `ErrLimitExceeded` is *never*
raised for such a field. Only the schema knows which fields those are, so the
destination says so:

```go
// Generated code declares the bound...
func (m *Msg) SchemaBound(id sofab.ID, what sofab.BoundKind) bool {
    return id == 4 && what == sofab.BoundArrayCount   // schema: count: 10000
}

// ...and enforces it at the header, which is what replaces the cap.
func (m *Msg) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
    if id == 4 && count > 10000 { return sofab.ErrInvalidMsg }
    return nil
}
```

With that, a 5000-element array on field 4 decodes even under
`WithMaxArrayCount(1000)`, while every field the schema leaves unbounded stays
capped. `BoundKind` names the size being asked about — `BoundArrayCount`,
`BoundStringLen`, `BoundBlobLen` — so the exemption is per `(id, kind)`, and a
field arriving under a shape the schema does not declare for that id (the §7.3
skip) keeps the cap. Answering `true` is a promise to enforce: the cap no longer
stands between an untrusted header and the allocation it implies.

`SchemaBoundVisitor` is a separate optional interface, like `HeaderVisitor` and
`ElemBoundVisitor`, so a visitor that does not implement it decodes exactly as
before. It is consulted only *after* a configured cap has been exceeded — a decode
with no limits set never pays for it, not even the type assertion. On the pull
surface the caller knows the schema instead, and says so per field:

```go
f, _ := d.Next()
if f.ID == 4 { d.SchemaBounded() }          // covers this field only
v, err := sofab.ReadUnsignedArray[uint32](d)
```

#### When the reader's type disagrees with the wire

A typed reader bound to a field of another type — a different wire type, or for a
`fixlen` a different subtype (`fp32`/`fp64`/`string`/`blob`) — returns
**`ErrTypeMismatch`**. That is **not** an error about the message: MESSAGE_SPEC
§7.3 says such a field is **skipped exactly like one with an unknown id**, so the
reader consumes the value, leaves your destination untouched, and parks the
decoder on the next field boundary — the loop simply continues with `Next`, and a
decode that meets nothing else still ends `COMPLETE` (`io.EOF` at the top level).
The same bytes decode fine for a peer whose schema declares the other type, which
is why they are neither `ErrInvalidMsg` nor a caller error (CORELIB_PLAN §6.3
removed the "invalid usage" code outright — there is no `ErrUsage`):

```go
case f.ID == 3:
    s, err := d.String()
    if errors.Is(err, sofab.ErrTypeMismatch) { break }  // a peer sent another type: keep the default
    if err != nil { /* ErrInvalidMsg / ErrIncomplete / ErrLimitExceeded */ }
    _ = s
```

The one exception is a sequence start/end, which carries no value: there
`ErrTypeMismatch` consumes nothing and the caller skips the sub-tree with `Skip()`
itself, exactly as it does for an unknown id.

Framing is judged first and still wins: a reserved fixlen subtype, a wrong-width
`fp32`/`fp64`, an element word that is not `fp32`/4 or `fp64`/8, a count past
`ARRAY_MAX` — all `ErrInvalidMsg`, mismatch or not. And a mismatched field that
runs off the end while being skipped is `ErrIncomplete`, the same verdict the
matching read would give. What is left for **`ErrArgument`** is the genuine caller
mistake: on this surface, a typed reader called with no field waiting, or after the
current value was already consumed — and on the encode side, an argument no valid
field can be built from ([above](#what-the-encoder-refuses-to-write)).

Generated code decodes through `Accept` / `AcceptBytes` and applies §7.3 in its
own field switch, so it never sees `ErrTypeMismatch`; both surfaces reach the same
verdict on the same bytes.

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

**Encoder — output buffer.** Who owns it depends on which constructor you use.
Each write copies its bytes into the active buffer, so caller source
strings/slices may be reused immediately on every form, and you **must call
`Flush`** to push the tail and surface a late write error.

| Constructor | Who owns the buffer | When it fills |
|---|---|---|
| `NewEncoder(w)` | this package: a 512 B window, growing on demand to 4 KiB | flushed to `w`, then reused |
| `NewEncoderBuffer(buf, offset)` | you | nothing to flush to — the encode stops with `ErrBufferFull` |
| `NewEncoderSink(buf, offset, sink)` | you | handed to `sink`, then reused (or replaced, see `SetBuffer`) |

A caller-supplied buffer is **never grown, reallocated or replaced** behind your
back, and the encoder writes only between `offset` and the end of it. `Bytes()`
returns what has been written into the active buffer since it was installed — the
whole message, for a sink-less encoder that had room for it.

**`MIN_OUTPUT_BUFFER` = 20** (`sofab.MinOutputBuffer`). This is the smallest
buffer accepted **for streaming**: `len(buf)-offset` must be at least 20 for a
buffer installed *with a sink*, both at `NewEncoderSink` and at every mid-stream
`SetBuffer`, and an undersized one is rejected there with `ErrArgument` rather
than partway through a message. The value is what this encoder reserves as one
contiguous piece — a field header varint plus a 64-bit value varint, 2 × 10 — and
it never splits an atomic unit; a `string`/blob payload it does split freely, so
a payload far larger than the buffer streams through it. A buffer installed
**without** a sink is subject to **no minimum**: no flush can occur, so a message
that encodes to two bytes encodes into a two-byte buffer.

**Pass-through.** A `string`/blob payload larger than the buffer is normally
copied through it. With the caller's permission it is instead handed to the sink
**directly** (after the buffered bytes, so wire ordering is unchanged), saving a
pass over the payload — the dominant cost of encoding a large blob. Such a call
hands the sink memory that is **not** the output buffer: it is borrowed for the
duration of the call and must not be retained, and it is never a buffer handover
(`SetBuffer` is rejected while the permission is granted, since a sink cannot
tell the two calls apart). It is **off by default everywhere** — for
`NewEncoderSink` and for `NewEncoder` alike, whose `io.Writer` is a sink like any
other — so a destination that was not told it may receive foreign memory never
does. Grant it with `WithPassThrough(true)`; the bytes on the wire are identical
either way, so it is purely a permission about what the destination is handed.

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

Go always ships the **full format** — there are no build-time toggles for wire
features. The configurable policies are **strict UTF-8 validation** and the
encoder's **pass-through permission**; neither changes a byte on the wire:

| Option | Default | Effect |
|--------|---------|--------|
| `WithStrictUTF8(bool)` (`SOFAB_STRICT_UTF8`) | on | Passed to `NewEncoder`, `NewDecoder` or `AcceptBytes`. On: an invalid-UTF-8 `string` is rejected — `ErrArgument` on encode, `ErrInvalidMsg` where a string is read on decode. Off: bytes are stored/written verbatim (never lossy). It reaches every path a string is materialized on, the visitor destination included (below). |
| `WithPassThrough(bool)` | off | Whether a `string`/blob payload larger than the buffer may be handed to the sink directly instead of being copied through it (CORELIB_PLAN §5.1) — see [Memory handling](#memory-handling) for what a sink then owes. |
| `-tags sofab_no_strict_utf8` | off (check compiled in) | Compiles the validator out for footprint builds (§6.4 "compiled OFF means the validation code is not compiled in"). It folds the check away **everywhere**, not only in `Utf8Valid`: `Decoder.String` and `Encoder.WriteString` stop validating too, and it wins over `WithStrictUTF8` — the compile-time gate is checked first, so the option cannot resurrect a check that is not in the binary. OFF is still constrained: bytes are stored and written verbatim, never replaced. A documented non-strict build; CI builds and tests it (`no-strict-utf8`), and the **default** build remains the one that is conformance-tested against the shared vectors. |

**Where validation happens on decode.** A Go `string` is a byte-container type,
so validation runs where the payload is *materialized into a destination* and
nowhere else (CORELIB_PLAN §6.4, normative — a skipped field is a length jump
over bytes that are never inspected):

* `Decoder.String` is a materializing read by construction, so it validates
  internally, under `WithStrictUTF8`. `Decoder.Skip` stays a pure discard.
* `Accept` / `AcceptBytes` / `AcceptStream` hand the wire bytes to
  `Visitor.String` verbatim. The cursor cannot tell a field the visitor binds
  from one it ignores — an undeclared id, or a field whose wire type contradicts
  the schema (MESSAGE_SPEC §7.3), arrives at the same callback — so the consumer
  validates at the destination and returns `ErrInvalidMsg` on false. Generated
  code emits that check in each arm that binds a `string`.

**Reaching the option from a visitor destination.** §6.4 requires both halves of
the gate to sit inside the check, so flipping `WithStrictUTF8` never means
regenerating or rebuilding. The destination therefore gets the *decode's* policy
handed to it:

```go
type Msg struct {
	sofab.StringCheck // gives Msg SetStringCheck + Utf8Valid
	Name string
}

func (m *Msg) String(id sofab.ID, v string) error {
	switch id {
	case 1:
		if !m.Utf8Valid([]byte(v)) { // this decode's policy, not the build's
			return sofab.ErrInvalidMsg
		}
		m.Name = v
	}
	return nil
}
```

A visitor implementing `StringPolicyVisitor` (embedding `sofab.StringCheck` is
the one-line way) is handed the resolved `StringCheck` before its scope's first
string — nested sequence visitors included. The zero value is **strict**, so a
destination that is never handed a policy validates rather than silently
accepting. Memory-wise it adds one `bool` to the visitor and no allocation: the
value is a plain struct, the type assertion is made at most once per scope, and
only at a scope that actually carries a string.

The package-level `Utf8Valid(b []byte) bool` primitive stays exported and is the
**always-strict** form — a package-level function has no decode to read the
option from — so destinations written against it keep compiling and keep
rejecting, whatever the option says. "Always" here means against the *runtime*
option only: the compile-time gate comes first, so a `sofab_no_strict_utf8`
build folds this primitive to `true` as well.

Framing is checked on every field regardless: the fixlen word, the reserved
subtype rejection, `ARRAY_MAX`, `MAX_DEPTH`, varint overflow, and the exact
`length`-byte advance. Only the *content* check moves.

It is a validation policy only, never a wire-format switch, so peers with
different settings still interoperate on all valid data (CORELIB_PLAN §6.4).

## Build & test

```bash
go build ./...           # build
go vet ./...             # static analysis
go test ./...            # unit + roundtrip + example tests
go test ./... -race      # with the race detector
go test ./... -cover     # with coverage

# The documented footprint build (§6.4: the validator is compiled out).
# CI runs this leg too, so it stays compiling and green.
go test -tags sofab_no_strict_utf8 ./...
```

Tests cover the shared conformance suite (`vectors_test.go`, including the
`invalid_utf8` group), chunked/byte-at-a-time streaming that resumes at any
boundary (`streaming_test.go`), byte-exact encode/decode and malformed-input
errors, the visitor path (`visitor_test.go`), roundtrip value preservation, and
the generated-code-style walkthrough (`example_test.go`).

The suite runs in **both** UTF-8 build configurations. Every UTF-8 assertion is
written against `utf8CheckCompiled`, a per-build constant declared in
`utf8_build_on_test.go` / `utf8_build_off_test.go`, so one test body states the
default build's reject and the footprint build's accept-verbatim contract; each
file also carries the tests that only make sense in its own build.

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
