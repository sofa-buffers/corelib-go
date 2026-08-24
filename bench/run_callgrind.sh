#!/usr/bin/env bash
#
# SofaBuffers Go — machine-independent instruction cost.
#
# Runs each benchmark workload once under Callgrind and reports instructions
# retired per operation (Ir/op). Unlike wall-clock or CPU time, instruction
# counts are deterministic and independent of the host's clock speed and
# scheduler, so the numbers compare across machines (and against the C/C++/
# Rust/Python/TypeScript tools — the workloads, ids and values are identical).
#
# The `perfbench` tool exposes each workload as a noinline `main.run_<workload>`
# function that performs exactly one op (setup excluded); this drives it under
# `--collect-atstart=no --toggle-collect=main.run_<workload>`, so the reported
# Ir is one op's instruction count directly — no rep-count subtraction (native
# symbols, unlike the JIT/interpreted ports).
#
# The workload list and its row labels come from `perfbench workloads`, so this
# script never carries a second copy of the suite: a row added to the table in
# cmd/perfbench/main.go appears here on the next run.
#
# The `blob 1MB` rows are where the instruction count earns its keep: the delta
# between one-shot and streaming is the divisible-run cost (CORELIB_PLAN §5.1.3)
# with the host's memory subsystem taken out of it — what the flush machinery
# costs. Under MB/s that difference drowns in memory bandwidth; here it does not.
#
# The Go runtime is tamed so the single op is deterministic under Valgrind:
# GOMAXPROCS=1 (one OS thread), GODEBUG=asyncpreemptoff=1 (no preemption signal
# storms, which Valgrind serializes oddly), GOGC=off (no GC during the op).
#
# Prereqs: valgrind, go. Builds perfbench into a temp dir.
# Usage:   bash bench/run_callgrind.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if ! command -v valgrind >/dev/null 2>&1; then
    echo "error: valgrind not found (needed for instruction counts)." >&2
    echo "       install it, e.g.  apt-get install valgrind" >&2
    exit 1
fi

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
BIN="$OUT/perfbench"

echo ">> building perfbench ..." >&2
go build -o "$BIN" ./cmd/perfbench

run_cg() { # $1 workload
    GOMAXPROCS=1 GODEBUG=asyncpreemptoff=1 GOGC=off \
        valgrind --quiet --tool=callgrind --collect-atstart=no \
        --toggle-collect="main.run_$1" \
        --callgrind-out-file="$OUT/$1.out" "$BIN" "$1" \
        >/dev/null 2>"$OUT/$1.log"
}

ir_of()    { grep -m1 '^summary:' "$OUT/$1.out" 2>/dev/null | awk '{print $2}'; }
bytes_of() { grep -ohE 'used=[0-9]+' "$OUT/$1.log" 2>/dev/null | head -1 | cut -d= -f2; }

echo ">> Measuring instructions/op under Callgrind (this is slow) ..." >&2
echo
echo "==============================================================================="
echo " SofaBuffers Go instruction cost   (Callgrind, Ir/op)"
echo " instructions/op: lower is better. Deterministic & machine-independent."
echo "==============================================================================="
printf "%-28s %16s %9s\n" "Workload" "instr/op" "bytes"
printf "%-28s %16s %9s\n" "--------" "--------" "-----"

while IFS=$'\t' read -r verb label; do
    [ -n "$verb" ] || continue
    run_cg "$verb"
    ir="$(ir_of "$verb")"; b="$(bytes_of "$verb")"
    printf "%-28s %16s %9s\n" "$label" "${ir:--}" "${b:--}"
done < <("$BIN" workloads)

echo
echo "Ir = instructions retired (Callgrind). Independent of CPU clock and OS"
echo "scheduling; depends only on the executed code, so it compares across machines."
echo "blob 1MB: read one-shot vs streaming against each other."
