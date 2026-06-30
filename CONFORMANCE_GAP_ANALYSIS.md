# SofaBuffers `corelib-go` — Conformance Gap Analysis & Remediation Plan

Audit of the Go core-library port against the language-neutral specification
(`CORELIB_PLAN.md`, §13 Conformance Checklist as the spine, supported by
§4–§12). Every item below was verified by reading source, not inferred from
names. The port is byte-correct on the wire for non-empty messages and very well
tested; the gaps are concentrated in (a) **rejection of the now-legal
zero-count arrays** (encode *and* both decode paths), (b) a missing/unenforced
`MAX_DEPTH` limit, (c) the visitor decode path buffering the whole message while
the README claims it streams, and (d) a devcontainer container-name mismatch.

> Note: no Go toolchain is available in this workspace, so tests were **not
> executed**; the testing assessment is from source review of the `*_test.go`
> files plus the committed `results.md` (claims 99.2% statement coverage).

## Spec revision

This refresh re-audits against the updated `CORELIB_PLAN.md` (commit `dcb85d6`,
2026-06-30): **zero-length arrays and empty sequences are now explicitly legal on
the wire.** Specifically:

- **§4.7** — `element_count` range is now `0 .. 2,147,483,647` (was `1..`). A
  zero-count integer array is valid and fully specified as exactly
  `[ header_varint ] [ element_count_varint = 0 ]`, nothing after. Whether an
  explicit empty array is distinguished from an absent field is now a
  **code-generator** concern, **not** a wire-level one.
- **§4.8** — a zero-count fixlen array (fp32/fp64) carries **no `fixlen_word` and
  no payload**: exactly `[ header_varint ] [ element_count_varint = 0 ]`.
- **§4.9** — an **empty sequence** (`sequence start` immediately followed by its
  `0x07` end) is legal and a decoder **must** accept it.

### What changed vs the previous revision

- **Item 6 (§4.7–4.8 arrays): PASS → GAP.** The previous audit treated
  "arrays are never empty (count ≥ 1)" as correct and scored the port's
  rejection of empty arrays as compliant. Under the updated spec that rejection
  is **non-conformant**. The Go port rejects zero-count arrays on **encode**
  (`encoder.go:155,169,183,203` → `ErrArgument`) and on **both decode paths**
  (`decoder.go:323` and `cursor.go:76` → `ErrInvalidMsg`), and additionally
  mishandles the *no-`fixlen_word`* rule for zero-count fixlen arrays in skip and
  read. New finding **R1** (HIGH) tracks this; it is the headline regression
  introduced by the spec change.
- **Item 7 (§4.9 sequences): re-verified, still GAP — but for `MAX_DEPTH` only.**
  The newly-mandated **empty sequence** behavior is **already correctly
  supported** by the port (encode, pull-skip, and visitor all handle it; the
  three shared `*empty_sequence*` vectors pass). The GAP remains solely the
  missing/unenforced `MAX_DEPTH` = 255 (finding **R2**, unchanged).
- **Item 12 (§7 tests): PASS → PARTIAL.** The shared vectors — including
  `empty_sequence`, `nested_empty_sequences`, and `empty_sequence_between_fields`
  — still all pass. However, the port's **own** unit tests now actively assert
  the spec-forbidden behavior (empty array ⇒ `ErrArgument`, zero-count array
  decode ⇒ `ErrInvalidMsg`: `encoder_test.go:197`, `coverage_test.go:107`,
  `visitor_test.go:313`), and there is no positive zero-count-array vector or
  test. The suite therefore encodes the obsolete wire assumption. Folded into R1.
- **Unaffected findings carried forward unchanged:** `MAX_DEPTH` (R2, was R1),
  visitor/generated decode buffers the whole message (R3, was R2), README false
  streaming claims (R4, was R3), devcontainer running-container name (R5, was
  R4), invalid-UTF-8 not rejected (R6, was R5), and doc/asset-provenance hygiene
  (R7, was R6). None of these touch the zero-length/empty-sequence rules.

## Summary

| Status | Count |
|--------|------|
| PASS | 11 |
| PARTIAL | 5 |
| GAP | 2 |
| **Total** | **18** |

(Previous revision: PASS 13 / PARTIAL 4 / GAP 1. Δ: item 6 PASS→GAP, item 12
PASS→PARTIAL.)

### Zero-length array & empty-sequence support at a glance

| Construct | Encode | Pull decode | Visitor decode | Skip | Verdict |
|-----------|--------|-------------|----------------|------|---------|
| Zero-count **unsigned** array (`0b011`) | **Rejected** `ErrArgument` (`encoder.go:155`) | **Rejected** `ErrInvalidMsg` (`decoder.go:323`) | **Rejected** `ErrInvalidMsg` (`cursor.go:76`) | OK (count-0 loop runs 0×, `decoder.go:294`) | **NON-CONFORMANT** |
| Zero-count **signed** array (`0b100`) | **Rejected** `ErrArgument` (`encoder.go:169`) | **Rejected** (`decoder.go:323`) | **Rejected** (`cursor.go:76`) | OK (`decoder.go:294`) | **NON-CONFORMANT** |
| Zero-count **fixlen** array (`0b101`, no `fixlen_word`/payload) | **Rejected** `ErrArgument` (`encoder.go:183,203`) | **Rejected** (`decoder.go:323`); even if allowed, `ReadFloat32/64Array` would read the absent `fixlen_word` (`decoder.go:380,408`) | **Rejected** (`cursor.go:76`); `acceptFixlenArray` would also read the absent word (`cursor.go:224`) | **Broken**: `skipValue` reads the absent `fixlen_word` then discards `n×size` (`decoder.go:300-311`) → desyncs | **NON-CONFORMANT** |
| **Empty sequence** (`start` + `0x07`) | OK (`encoder.go:142-151`) | OK via `Next`/`Skip` (`decoder.go:242-265`) | OK via `accept` (`cursor.go:156-171`) | OK | **CONFORMANT** (vectors `empty_sequence`, `nested_empty_sequences`, `empty_sequence_between_fields` pass) |

## Per-checklist-item results

| # | Checklist item (§13) | Status | Evidence | Notes |
|---|----------------------|--------|----------|-------|
| 1 | All public symbols under `sofab` namespace (§6) | PASS | `types.go:1`, `encoder.go:1`, `decoder.go:1`, `visitor.go:1`, `cursor.go:1` all `package sofab`; exported `Encoder`/`Decoder`/`Visitor`/`WireType`/`ID`/errors live there | Module path `github.com/sofa-buffers/corelib-go`, imported as `sofab` (`go.mod:1`, README:32). |
| 2 | API version constant/getter returns `1` (§6) | PASS | `types.go:14` `const APIVersion = 1` | Cross-checked against the vector file `version` in `vectors_test.go:60`. |
| 3 | Varint & zig-zag match §4.1–4.2 | PASS | varint enc `encoder.go:46-60`, dec `decoder.go:81-104` & `cursor.go:24-46` (overflow guard `shift>=64`); zigzag `types.go:71-74` | Boundary-tested `encoder_test.go:47-70`, `decoder_test.go:28-44`, overflow `decoder_test.go:175-181`. |
| 4 | Field header `(id<<3)\|type` & all 8 wire types (§4.3) | PASS | header pack `encoder.go:72-81`, unpack `decoder.go:57-71` / `cursor.go:96-101`; tags `types.go:22-31` match normative table | All 8 types exercised across `encoder_test.go` / `decoder_test.go` / vectors. |
| 5 | Fixlen word `(length<<3)\|sub`, LE floats, UTF-8 string w/o terminator, blobs (§4.6) | PASS | word `encoder.go:107-111`, decode `decoder.go:106-120` / `cursor.go:58-69`; LE floats `encoder.go:113-127`; string no NUL `encoder.go:130-133`, `encoder_test.go:87-94`; zero-length string/blob OK (`encoder_test.go:92,101`, `decoder_test.go:73`) | Strings are not validated as UTF-8 (`decoder.go:205-211` does `string(buf)`); §6.3 lists invalid UTF-8 as `InvalidMessage`. See R6 (low). |
| 6 | Integer arrays + fixlen arrays single shared word; no dynamic subtypes; **zero-count arrays legal** (§4.7–4.8) | **GAP** | int arrays `encoder.go:154-179` / `decoder.go:330-369`; fixlen array single word `encoder.go:182-213`; subtype guard rejects non-fp32/fp64 `cursor.go:230-252`, `decoder.go:384,412` | Non-empty arrays are byte-correct, **but zero-count arrays are now legal and the port rejects them everywhere**: encode `ErrArgument` (`encoder.go:155,169,183,203`); decode `ErrInvalidMsg` via `arrayCount` (`decoder.go:318-327`, `cursor.go:71-80`). Zero-count **fixlen** array also violates §4.8's *no-`fixlen_word`* rule: `skipValue` (`decoder.go:300-311`), `ReadFloat32/64Array` (`decoder.go:380,408`) and `acceptFixlenArray` (`cursor.go:224`) unconditionally read a `fixlen_word` that is absent for count 0. See R1 (high). |
| 7 | Sequence framing, fresh scope, single-byte `0x07` end, **empty sequence accepted**, skip-by-walking with depth tracking, **reject nesting beyond `MAX_DEPTH`=255** (§4.9) | **GAP** | framing `encoder.go:142-151`, `0x07` end `encoder.go:148-150`; skip-walk `decoder.go:242-271`; visitor recursion `cursor.go:156-166`; empty-sequence handled on encode + pull (`decoder.go:244-265`) + visitor (`cursor.go:156-171`), vectors `empty_sequence`/`nested_empty_sequences`/`empty_sequence_between_fields` pass | **Empty-sequence requirement is satisfied.** The remaining gap is `MAX_DEPTH`: no constant anywhere (grep: none); encoder never caps `sequence_begin` depth; decoder/visitor never reject over-deep nesting; `cursor.accept` recurses on the Go stack → unbounded-recursion DoS. See R2 (high). |
| 8 | Streaming encode into smaller-than-message buffer via flush/sink + mid-stream buffer swap (§5.1) | PASS | sink `encoder.go:14-34`; multi-flush proven `streaming_test.go:29-61` (sink driven ≥2× for a 1000-elem array, byte-identical to one-shot) | Idiomatic Go uses an `io.Writer` sink (§5.3 permits this); fixed-buffer "buffer swap", start-offset and `BufferFull` are N/A — buffer-full surfaces as the writer's error (`types.go:59`). |
| 9 | Streaming decode via small chunks, push/pull, lazy field binding, auto-skip (§5.2) | PARTIAL | pull path streams & is byte-at-a-time tested (`streaming_test.go:63-95` one-byte/half readers; `vectors_test.go:484-492`); auto-skip `decoder.go:48-51` | The **visitor/push path does not stream**: `Decoder.Accept` slurps the entire message into one buffer (`visitor.go:38-45,62-75`), confirmed by `results.md:23-28`. Only the pull parser satisfies incremental decode. See R3. |
| 10 | Error reporting per §6.3 baseline | PASS | `types.go:60-68` `ErrArgument`/`ErrUsage`/`ErrInvalidMsg`; `OK`=nil; extensive `errors.Is` tests in `decoder_test.go`, `coverage_test.go` | `BufferFull` intentionally delegated to the `io.Writer` error (allowed by §6.3 "language-specific"). Invalid UTF-8 not mapped to `InvalidMessage` (see R6). Note: empty array currently mapped to `ErrArgument` is no longer a valid usage error (see R1). |
| 11 | Streaming primitives suffice for a thin generated-object layer that *also* streams; one-shot helpers are thin wrappers (§6.1) | PARTIAL | encode streams via sink; pull-based generated `Unmarshal` streams (`example_test.go:54-105`, `doc.go:44-72`) | The visitor path generated code is documented to use (`README:104-128`) buffers the whole message (`visitor.go`), contradicting §6.1 "never fully buffered". No `serialize()/deserialize()` convenience helpers exist (generated layer is out of scope, acknowledged `README:262-269`). See R3. |
| 12 | All shared vectors pass encode+decode, plus chunked, roundtrip, malformed, skip (§7) | PARTIAL | encode/decode `vectors_test.go:223-405`; chunked `streaming_test.go`; roundtrip `roundtrip_test.go`; malformed `decoder_test.go:167-222`, `coverage_test.go:162-293`, `visitor_test.go:284-325`; skip `coverage_test.go:297-395`, skip-ids `vectors_test.go:468-497`; 67 vectors incl. the 3 empty-sequence vectors | All shared vectors pass (incl. `empty_sequence`*). **But the suite locks in the now-obsolete wire assumption**: `encoder_test.go:197` and `coverage_test.go:107` assert empty array ⇒ `ErrArgument`; `visitor_test.go:313` ("array count zero") asserts decode ⇒ `ErrInvalidMsg` — both forbidden by updated §4.7–4.8. No positive zero-count-array vector/test exists. **Not executed in this audit** (no Go toolchain); `results.md:58` claims 99.2% coverage. Folded into R1. |
| 13 | `assets/` populated per §8 (branding + `test_vectors.json`) | PASS | `assets/sofabuffers_logo.png`, `assets/sofabuffers_icon.png`, `assets/test_vectors.json` (`format=sofabuffers-test-vectors`, `version=1`, 67 vectors) | Provenance wording differs from spec: `vectors_test.go:4` / README:27-30 say "documentation repo" / "C `test_ostream.c`"; §8 says copy from `corelib-c-cpp/assets`. File content/format is correct. See R7 (low). |
| 14 | README follows family format with badges + required sections (§9) | PARTIAL | header/logo/tagline `README:1-8`; CI+Coverage+Docs badges `README:12-14`; all §9 sections present (Why / Usage incl. larger-than-RAM / API summary / Feature flags / Build & test / Benchmarks) | "Why this design" and "Decoding with a visitor" make **false streaming claims** about `Accept` ("fully streaming", "small refillable window", "sync.Pool", ">64 KiB dropped"; `README:44,121-128`) that the code (`visitor.go`) and `results.md` contradict. See R4. |
| 15 | `perf` (CPU-independent) + `bench` (MB/s) tools present and runnable (§10) | PASS | `cmd/perfbench/main.go`: `bench` MB/s `runBench` (:230), `perf` per-op `runPerf` (:394); cycles/op reported unavailable (:340, consistent with Java/C#/TS ports) | `BENCH_SPEC.md` (named in §10 as the single source of truth for workloads) is absent from the repo. See R7 (low). |
| 16 | `.devcontainer/` complete; extensions incl. `anthropic.claude-code`; `.devcontainer/.env` gitignored (§11) | PARTIAL | all 6 files present; `devcontainer.json:11` lists `golang.go` + `anthropic.claude-code`; `.env` gitignored (`.devcontainer/.gitignore:5`, only `.env.example` tracked); `runArgs --env-file` (`devcontainer.json:7`) | **Container name wrong**: `start.sh:17` `--name sofa-go-dev` and `attach.sh:4` `docker exec ... sofa-go-dev`; §11.3 requires the running container name `go-devcontainer` (image tag is correctly `go-devcontainer`). See R5. |
| 17 | `ci.yml` builds & tests on push + PR; matrix where it matters; coverage report + badge (§12.1) | PASS | `ci.yml:3-8` push+PR; matrix `go: ['1.21','stable']`, `fail-fast:false` (:21-25); vet/build/`-race` test (:47-54); coverage job + badge JSON published to `badges` branch (:56-99); badge wired `README:13` | "debug+release" build N/A in Go; coverage published as a shields endpoint ("or equivalent" per §12.1) rather than Codecov — acceptable. |
| 18 | `docs.yml` generates HTML docs & publishes to Pages via Actions (no `gh-pages`); Docs badge links to it (§12.2) | PASS | `docs.yml:5-8` push-to-main only; godoc static export (:40-78); `upload-pages-artifact@v3` (:80-82) + `deploy-pages@v4` (:93-95); `permissions: pages/id-token` (:11-14); badge → `https://sofa-buffers.github.io/corelib-go/` (`README:14`) | Matches the Actions-based deployment requirement exactly. |

---

## Remediation Plan

Ordered by severity. (Constraint: this audit adds/updates only this file; the
fixes below are for a follow-up change.)

### R1 — Support zero-count arrays & empty-array wire forms (HIGH)

**Problem.** Updated §4.7 makes `element_count == 0` a valid, fully-specified
empty array, and §4.8 specifies that a zero-count fixlen array carries **no
`fixlen_word` and no payload**. The Go port treats empty arrays as an error on
every path, so it can neither produce nor consume a valid wire message that any
conformant port (e.g. the C reference) may emit — a cross-language interop break.

Concretely:
1. **Encode rejects empty arrays.** `WriteUnsignedArray` (`encoder.go:155`),
   `WriteSignedArray` (:169), `WriteFloat32Array` (:183), `WriteFloat64Array`
   (:203) all `setErr(ErrArgument)` when `len(a) == 0`.
2. **Decode rejects zero counts.** `arrayCount` returns `ErrInvalidMsg` when
   `n == 0` on **both** the pull (`decoder.go:318-327`) and visitor
   (`cursor.go:71-80`) paths, affecting `ReadUnsignedArray`/`ReadSignedArray`/
   `ReadFloat32Array`/`ReadFloat64Array` and the visitor array cases.
3. **Zero-count fixlen array mis-frames.** Even if the count gate were removed,
   the fixlen-array skip and read paths unconditionally read a `fixlen_word`
   after the count — which is **absent** for count 0 (§4.8) — and would consume
   the *next* field's header instead: `skipValue` (`decoder.go:300-311`),
   `ReadFloat32Array`/`ReadFloat64Array` (`decoder.go:380,408`),
   `acceptFixlenArray` (`cursor.go:224`).
4. **Tests lock in the old behavior.** `encoder_test.go:197`
   (`TestEmptyArrayIsArgumentError`), `coverage_test.go:107`
   (`TestEmptyArraysAreArgumentErrors`), and `visitor_test.go:313`
   ("array count zero" ⇒ `ErrInvalidMsg`) all assert the now-forbidden
   rejection and must be inverted.

Note: empty *sequences* already work; the absent-vs-empty distinction is now
explicitly a code-generator concern (§4.7), so the corelib must simply emit/parse
count-0 faithfully.

**Fix.**
1. Encoder: drop the `len(a) == 0` guards; emit
   `[ header ][ count_varint = 0 ]` for empty integer arrays, and for empty
   fixlen arrays emit **only** `[ header ][ count_varint = 0 ]` (no `fixlen_word`,
   no payload). Keep the per-call `id > IDMax` check.
2. Pull decoder: in `arrayCount` (`decoder.go:318-327`) remove the `n == 0`
   rejection (keep `> arrayMax`). In the fixlen-array read/skip paths
   (`decoder.go:300-311,380,408`) **return an empty result without reading a
   `fixlen_word`** when `n == 0`.
3. Visitor decoder: mirror the same in `arrayCount` (`cursor.go:71-80`) and
   `acceptFixlenArray` (`cursor.go:219-254`) — short-circuit to an empty slice on
   `n == 0` and skip the `fixlen_word` read; deliver an empty slice to the
   visitor.
4. Tests: invert the three tests above to assert empty arrays **encode** to
   `[header][0]` (and empty fixlen arrays to exactly two varints), and that they
   **decode/roundtrip/skip** back to empty slices on both pull and visitor paths.
   Add positive vectors/roundtrips for empty unsigned/signed/fp32/fp64 arrays.

**Files.** `encoder.go`, `decoder.go`, `cursor.go`; tests in `encoder_test.go`,
`decoder_test.go`, `coverage_test.go`, `visitor_test.go`, `roundtrip_test.go`.

**Acceptance criteria.**
- Encoding an empty unsigned/signed array yields exactly `[header][0x00]`; an
  empty fp32/fp64 array yields exactly `[header][0x00]` with **no** `fixlen_word`.
- Decoding those byte forms returns an empty (non-error) slice on both the pull
  and visitor paths; skipping them resyncs correctly on the following field.
- A roundtrip of an empty array of each of the four element kinds is byte-stable.
- No remaining test asserts that an empty array is an error.

### R2 — Define and enforce `MAX_DEPTH` = 255 (HIGH)

**Problem.** §4.9/§6.2 mandate a maximum nested-sequence depth of 255: an
encoder must not open more than 255 sequences and a decoder must reject deeper
nesting with `InvalidMessage` rather than risk unbounded recursion/stack growth.
The port defines no such constant and enforces no limit. `WriteSequenceBegin`
(`encoder.go:142`) tracks no depth; the pull decoder's `Skip` (`decoder.go:242-271`)
walks with an uncapped `int depth`; and `cursor.accept` (`cursor.go:156-166`)
descends via real Go **stack recursion** with no cap — a hostile message of
deeply nested `sequence_begin` bytes can exhaust the stack (DoS / crash), the
exact failure §4.9 exists to prevent. (Empty/nested-empty sequences are handled
correctly; this is purely about the depth bound.)

**Fix.**
1. Add `const MaxDepth = 255` to `types.go` (exported, alongside `IDMax`).
2. Encoder: track open-sequence depth; in `WriteSequenceBegin` return an error
   when opening would exceed `MaxDepth`; decrement in `WriteSequenceEnd`.
3. Pull decoder: cap the `depth` counter in `Skip`; return `ErrInvalidMsg` past
   255.
4. Visitor decoder: thread a depth argument through `cursor.accept`'s sequence
   recursion and return `ErrInvalidMsg` before exceeding 255 — converting the
   unbounded recursion into a bounded, well-defined rejection.

**Files.** `types.go`, `encoder.go`, `decoder.go`, `cursor.go`, plus tests in
`decoder_test.go`/`visitor_test.go`.

**Acceptance criteria.**
- `MaxDepth == 255` is exported.
- Encoding a 256th nested `sequence_begin` returns a non-nil error and writes no
  malformed bytes.
- Decoding a message nested 256 deep returns `ErrInvalidMsg` on **both** the
  pull and visitor paths, with no stack overflow (a 100k-deep adversarial input
  must error, not crash).
- A message nested exactly 255 deep still encodes and decodes successfully.

### R3 — Make visitor/generated decode stream (or correct the contract) (MEDIUM)

**Problem.** §5.2/§6.1 require the generated-object decode path to consume
arbitrarily small `feed` chunks and bind fields incrementally, "never fully
buffered". `Decoder.Accept` — the path the README and `doc.go` say generated
`Unmarshal` uses — instead slurps the **entire** message into one contiguous
buffer before parsing (`visitor.go:38-45`, `slurp` :62-75; confirmed
`results.md:23-28`). So a generated decoder built on `Accept` holds the whole
message in memory, defeating input-side streaming for the primary (generated)
consumer. The pull parser does stream, so the capability exists but not on the
documented generated path.

**Fix (pick one, in preference order).**
1. Re-implement `Accept` over a small refillable window fed from the
   `io.Reader` (the design the README already describes) so the visitor path
   streams without buffering the whole message; keep `AcceptBytes` as the
   zero-copy in-memory entry point.
2. If buffering `Accept` is an intentional perf choice, instead document the
   pull parser (`Decoder.Next`) as the canonical streaming generated path and
   stop presenting `Accept` as streaming (folds into R4).

**Files.** `visitor.go`, `cursor.go` (option 1); `README.md`/`doc.go` (option 2,
overlaps R4); tests in `streaming_test.go`.

**Acceptance criteria.**
- A documented generated-style decode path consumes a message via a
  one-byte-at-a-time reader without ever allocating a buffer proportional to the
  whole message (option 1), OR the docs unambiguously route streaming decode
  through the pull parser and no longer claim `Accept` streams (option 2).
- `streaming_test.go` gains a test asserting the chosen streaming decode path's
  peak buffer stays bounded (independent of message size) for a large message.

### R4 — Fix false streaming claims in the README (MEDIUM)

**Problem.** The README asserts, for `Decoder.Accept`: "Accept is also fully
streaming: it walks a small refillable window and never holds the whole message"
(`README:44`) and "Accept drives the decode kernel over a refillable window
(recycled across calls via a `sync.Pool`) … windows larger than 64 KiB are
dropped" (`README:121-128`). None of this exists in the code: `Accept` slurps
the whole message (`visitor.go`), there is no window, no `sync.Pool`, no 64 KiB
cap. `doc.go:30-32` and `results.md:23-28` correctly state the opposite, so the
README is internally inconsistent with the rest of the repo.

**Fix.** Either land R3 option 1 (making the claims true) or rewrite the README
"Why this design" row and "Decoding with a visitor" paragraph to describe the
actual behavior: `Accept`/`AcceptBytes` buffer the message and are the
throughput path; `Decoder.Next` is the streaming path. Remove the
`sync.Pool`/64 KiB/refillable-window language and the "Accept is also fully
streaming" claim.

**Files.** `README.md` (and keep `doc.go` as the source of truth).

**Acceptance criteria.** No README statement about decode streaming contradicts
`visitor.go`; the streaming guarantee is attributed to the path that actually
provides it; `Memory handling` table rows match the code.

### R5 — Devcontainer running-container name → `go-devcontainer` (MEDIUM)

**Problem.** §11.3 (and the task) require the running container name to follow
`<lang>-devcontainer`, i.e. `go-devcontainer`. `start.sh:17` names it
`sofa-go-dev` (`--name sofa-go-dev`) and `attach.sh:4` attaches to
`sofa-go-dev`, so `attach.sh` only works by coincidence of matching the wrong
name. The image **tag** is correctly `go-devcontainer` (`build.sh:6`,
`start.sh:22`).

**Fix.** Change `--name sofa-go-dev` to `--name go-devcontainer` in `start.sh`
and `docker exec -it sofa-go-dev` to `go-devcontainer` in `attach.sh`.

**Files.** `.devcontainer/start.sh`, `.devcontainer/attach.sh`.

**Acceptance criteria.** `start.sh` launches a container named `go-devcontainer`;
`attach.sh` attaches to `go-devcontainer`; names match the §11.3 table.

### R6 — Reject invalid UTF-8 strings with `InvalidMessage` (LOW)

**Problem.** §6.3 lists "invalid UTF-8" among the `InvalidMessage` conditions.
`Decoder.String` (`decoder.go:205-211`) and `cursor.acceptFixlen`'s `fixStr`
case (`cursor.go:202-207`) convert the payload with `string(buf)` and never
validate, so a string field carrying non-UTF-8 bytes decodes silently instead of
erroring. (Low impact: the JSON vectors can't carry invalid UTF-8, so this is
untested ground, and per-field validation has a small cost.)

**Fix.** Validate decoded string payloads with `utf8.Valid` on both decode paths
and return `ErrInvalidMsg` on failure. If validation cost matters, gate it but
default it on to match the baseline. (Blobs stay unvalidated by design.)

**Files.** `decoder.go`, `cursor.go`; tests in `decoder_test.go`/`visitor_test.go`.

**Acceptance criteria.** A `fixlen` string field with an invalid UTF-8 byte
sequence returns `ErrInvalidMsg` on `String()`, `Accept`, and `AcceptBytes`;
valid UTF-8 (incl. multi-byte) still decodes.

### R7 — Documentation/asset-provenance hygiene (LOW)

**Problem.** Two minor spec-fidelity nits, neither affecting wire behavior:
(a) `BENCH_SPEC.md`, named in §10 as the single source of truth for benchmark
workloads/timing/output, is absent, so `cmd/perfbench`'s workloads can't be
checked against a spec; and `doc.go:2` references an `ARCHITECTURE.md` at the
repo root that does not exist. (b) The vector-file provenance is described as the
"documentation repo" / C `test_ostream.c` (`vectors_test.go:4`, `README:27-30`)
whereas §8 specifies copying `test_vectors.json` from `corelib-c-cpp/assets`.

**Fix.** Add `BENCH_SPEC.md` (or align with the cross-language one) and either
add `ARCHITECTURE.md` or fix the `doc.go` reference to point at the published
spec. Correct the `test_vectors.json` provenance wording to name
`corelib-c-cpp/assets` as the source of truth.

**Files.** `BENCH_SPEC.md` (new), `doc.go`, `vectors_test.go` comment, `README.md`.

**Acceptance criteria.** Every doc cross-reference resolves to a real file; the
benchmark workloads cite a present spec; the vector provenance matches §8.

---

*Generated as part of a conformance audit of `corelib-go` against
`CORELIB_PLAN.md` §13 (spec revision `dcb85d6`, 2026-06-30). This document is
additive only; no existing source, test, or config file was modified.*
