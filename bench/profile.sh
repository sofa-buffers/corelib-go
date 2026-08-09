#!/usr/bin/env bash
#
# SofaBuffers Go — per-function instruction breakdown for one benchmark workload.
#
# A development aid alongside run_callgrind.sh, not part of the cross-language
# benchmark suite (BENCH_SPEC.md): run_callgrind.sh reports the Ir/op number the
# central harness parses, this reports where those instructions went. Same
# Callgrind setup and the same noinline main.run_<workload> toggle, so the totals
# agree; only the reporting differs.
#
# Add --auto=yes below (or run callgrind_annotate on the kept output file) for
# line-level attribution inside a function.
#
# Prereqs: valgrind, go.
# Usage:   bash bench/profile.sh <workload> [topN]
#          run without arguments to list the workloads (they come from
#          `perfbench workloads`, so this script carries no copy of the suite).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if ! command -v valgrind >/dev/null 2>&1; then
    echo "error: valgrind not found (needed for instruction counts)." >&2
    exit 1
fi

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

go build -o "$OUT/perfbench" ./cmd/perfbench

W="${1:-}"
TOP="${2:-25}"

if [ -z "$W" ] || ! "$OUT/perfbench" workloads | cut -f1 | grep -qx -- "$W"; then
    [ -n "$W" ] && echo "error: unknown workload '$W'." >&2
    echo "workloads:" >&2
    "$OUT/perfbench" workloads | sed 's/^/  /; s/\t/  --  /' >&2
    exit 1
fi

# Same runtime taming as run_callgrind.sh: one OS thread, no async preemption,
# no GC during the measured op, so a single op is deterministic under Valgrind.
GOMAXPROCS=1 GODEBUG=asyncpreemptoff=1 GOGC=off \
    valgrind --quiet --tool=callgrind --collect-atstart=no \
    --toggle-collect="main.run_$W" \
    --callgrind-out-file="$OUT/cg.out" "$OUT/perfbench" "$W" >/dev/null 2>&1

echo "=== $W: $(grep -m1 '^summary:' "$OUT/cg.out" | awk '{print $2}') Ir/op ==="
callgrind_annotate --threshold=99 "$OUT/cg.out" 2>/dev/null \
    | sed -n '/Ir *file:function/,/^$/p' | head -n "$TOP"
