# Visitor decode: contiguous-buffer optimization

The visitor decode path (`Decoder.Accept`) was reworked to parse over a single
contiguous buffer by advancing an integer cursor — the Go analogue of the
protobuf decode kernel's "advance a pointer over a flat byte range" — instead of
pulling one byte at a time through a `bufio.Reader`.

## Bottleneck

The previous decode path:

- read **one byte at a time** through `bufio.Reader.ReadByte()` — an indirect
  call plus a refill/bounds check per byte of every varint;
- did `make([]byte) + io.ReadFull` for every fixlen payload (string/blob/float);
- allocated a 4 KB `bufio` buffer per message in `NewDecoder`, even on the
  visitor path that never used it.

## Change

- **`cursor.go` (new)** — parses by advancing an index over one buffer. Varints
  decode in a tight local loop over `buf[i]` (no per-byte interface call);
  fixed/raw payloads and float arrays are taken as zero-copy subslices.
- **`Accept`** slurps the message into one contiguous buffer *once* (sized in a
  single `make`+`ReadFull` when the source reports `Len()`, otherwise
  `io.ReadAll`), then runs the cursor over it.
- **`AcceptBytes(buf, v)` (new)** — the zero-copy entry point that skips the
  slurp entirely, for callers that already hold the message in memory (e.g.
  generated `Unmarshal([]byte)` code). This is the max-throughput ceiling.
- **`bufio` is now created lazily** on the first `Next`, so the visitor path no
  longer pays for the reader buffer. The pull-parser path is unchanged.

## Results

Best of 8 runs, `GOMAXPROCS=1` (`go test -bench BenchmarkDecode -benchmem
-count=8 -cpu=1 -benchtime=300ms`). Benchmarks live in `decode_bench_test.go`.

| Workload | Metric | Baseline (byte-at-a-time reader) | `Accept` (slurp + cursor) | `AcceptBytes` (zero-copy) |
|---|---|---|---|---|
| **u64 × 1000** (~9.5 KB) | ns/op | 57,210 | 20,836 (**2.7×**) | 18,443 (**3.1×**) |
| | MB/s | 166 | 456 | 515 |
| | allocs/op | 4 | 3 | **1** |
| | B/op | 12,432 | 17,968 | 8,192 |
| **typical** (mixed, 37 B) | ns/op | 1,262 | 323 (**3.9×**) | 190 (**6.6×**) |
| | MB/s | 29 | 114 | 195 |
| | allocs/op | 7 | 4 | **2** |
| | B/op | 4,288 | 133 | 37 |

## Notes / tradeoff

- The small "typical" message wins most (**4–7×**): it was dominated by per-byte
  calls, the 4 KB `bufio` allocation, and per-field blob copies — all gone now
  (B/op 4288 → 37).
- For the large u64 array, `Accept`'s slurp adds a one-time input copy (B/op
  rises 12432 → 17968), inherent to reading from an `io.Reader`. **`AcceptBytes`
  removes that copy** (8192 B, 1 alloc) and is the real ceiling — prefer it when
  the message is already in memory.

All tests pass under `-race`; gofmt/vet clean; statement coverage 99.2%.
