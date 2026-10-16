#!/usr/bin/env bash
# Performance baseline and regression guard (issue #13, docs/performance.md).
#
#   scripts/perf.sh [--check]     measure and compare with perf/baseline.json (exit 1 on a regression)
#   scripts/perf.sh --update      measure and record a new perf/baseline.json (commit it with a reason)
#   scripts/perf.sh --run         measure only; print the table
#   ... --pr                      small history (ALM_PERF_COPIES=20 unless set) against
#                                 perf/baseline-pr.json: the CI size for pull requests
#                                 (issue #25). Re-record both baselines together:
#                                 scripts/perf.sh --update && scripts/perf.sh --update --pr
#
# Options: --out DIR (keep the result files there; default: a temp dir),
#          --retries N (re-measure up to N times when the check fails; default 1 —
#          a real regression reproduces, scheduler noise does not).
# Environment: ALM_PERF_COPIES (synthetic history size, default 80 copies of
# session-raw.log ≈ 600k lines), ALM_PERF_HOST (machine label in the results).
#
# Everything runs in isolated temp dirs (testhome / isoEnv): it never touches
# the real data root, control dir or service process, and uses only the
# fake adb (no device, no adb server).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE=check
PR=0
OUT=""
RETRIES=1
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check ;;
    --update) MODE=update ;;
    --run) MODE=run ;;
    --pr) PR=1 ;;
    --out) OUT="$2"; shift ;;
    --retries) RETRIES="$2"; shift ;;
    *) echo "usage: $0 [--check|--update|--run] [--pr] [--out DIR] [--retries N]" >&2; exit 2 ;;
  esac
  shift
done
BASELINE="$ROOT/perf/baseline.json"
if [ "$PR" = 1 ]; then
  BASELINE="$ROOT/perf/baseline-pr.json"
  export ALM_PERF_COPIES="${ALM_PERF_COPIES:-20}"
fi
echo "perf: baseline ${BASELINE#"$ROOT"/}, ALM_PERF_COPIES=${ALM_PERF_COPIES:-default}" >&2
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
  go run ./internal/perf/cmd/perfcheck -baseline "$BASELINE" -out "$OUT/merged.json" -md "$OUT/table.md" "$OUT"/inprocess.json "$OUT"/process.json
}

measure
case "$MODE" in
  run)
    go run ./internal/perf/cmd/perfcheck -update -baseline "$OUT/merged.json" -md "$OUT/table.md" "$OUT"/inprocess.json "$OUT"/process.json >/dev/null
    cat "$OUT/table.md" ;;
  update)
    go run ./internal/perf/cmd/perfcheck -update -baseline "$BASELINE" -md "$OUT/table.md" "$OUT"/inprocess.json "$OUT"/process.json
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
