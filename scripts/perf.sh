#!/usr/bin/env bash
# Performance baseline and regression guard (issue #13, docs/performance.md).
#
#   scripts/perf.sh [--check]     measure and compare with perf/baseline.json (exit 1 on a regression)
#   scripts/perf.sh --update      measure and record a new perf/baseline.json (commit it with a reason)
#   scripts/perf.sh --run         measure only; print the table
#
# Options: --out DIR (keep the result files there; default: a temp dir),
#          --retries N (re-measure up to N times when the check fails; default 1 —
#          a real regression reproduces, scheduler noise does not).
# Environment: ALM_PERF_COPIES (synthetic history size, default 80 copies of
# session-raw.log ≈ 600k lines), ALM_PERF_HOST (machine label in the results).
#
# Everything runs in isolated temp dirs (testhome / isoEnv): it never touches
# the real data root, control dir or background service, and uses only the
# fake adb (no device, no adb server).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE=check
OUT=""
RETRIES=1
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check ;;
    --update) MODE=update ;;
    --run) MODE=run ;;
    --out) OUT="$2"; shift ;;
    --retries) RETRIES="$2"; shift ;;
    *) echo "usage: $0 [--check|--update|--run] [--out DIR] [--retries N]" >&2; exit 2 ;;
  esac
  shift
done
CLEAN=()
cleanup() { [ ${#CLEAN[@]} -eq 0 ] || rm -rf "${CLEAN[@]}"; }
trap cleanup EXIT
if [ -z "$OUT" ]; then
  OUT="$(mktemp -d "${TMPDIR:-/tmp}/alm-perf.XXXXXX")"
  CLEAN+=("$OUT")
fi
mkdir -p "$OUT"
cd "$ROOT/server"
export ALM_PERF_COMMIT="${ALM_PERF_COMMIT:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || true)}"
# belt and braces: the tests isolate themselves, but never let a manual run
# reach the user's cache dir
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/alm-perf-home.XXXXXX")"
CLEAN+=("$SCRATCH")
export XDG_CACHE_HOME="$SCRATCH/cache"

measure() {
  rm -f "$OUT"/inprocess.json "$OUT"/process.json
  ALM_PERF_OUT="$OUT" go test -count=1 -timeout 20m -run '^TestPerf(InProcess|Process)$' ./internal/perf ./cmd/android-log-mcp
}

check() {
  go run ./internal/perf/cmd/perfcheck -baseline "$ROOT/perf/baseline.json" -out "$OUT/merged.json" -md "$OUT/table.md" "$OUT"/inprocess.json "$OUT"/process.json
}

measure
case "$MODE" in
  run)
    go run ./internal/perf/cmd/perfcheck -update -baseline "$OUT/merged.json" -md "$OUT/table.md" "$OUT"/inprocess.json "$OUT"/process.json >/dev/null
    cat "$OUT/table.md" ;;
  update)
    go run ./internal/perf/cmd/perfcheck -update -baseline "$ROOT/perf/baseline.json" -md "$OUT/table.md" "$OUT"/inprocess.json "$OUT"/process.json
    cat "$OUT/table.md" ;;
  check)
    n=0
    until check; do
      n=$((n + 1))
      if [ "$n" -gt "$RETRIES" ]; then exit 1; fi
      echo "--- re-measuring ($n/$RETRIES) to rule out noise ---"
      measure
    done ;;
esac
