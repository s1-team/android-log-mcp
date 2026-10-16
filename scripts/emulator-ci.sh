#!/usr/bin/env bash
# The nightly emulator validation (issue #27, .github/workflows/emulator.yml)
# on one running emulator. Every step runs, even after a failure; each
# writes OUT/<step>/log.txt and result.txt, and OUT/results.tsv collects
# step, result, seconds and the first failing check.
#
#   scripts/emulator-ci.sh SERIAL OUT_DIR
#
# Meant for a throwaway CI emulator: it clears the device log buffers and
# installs the test app (validation/logtest-app/build/logtest.apk). Includes
# the backward-clock matrix (scripts/clock-e2e.sh). Every
# android-log-mcp process runs with XDG_CACHE_HOME under OUT_DIR/scratch,
# never the real per-user control dir.
#   env ANDROID_LOG_MCP_BIN: the binary (default server/android-log-mcp)
# Exit status: 0 when every step passed.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERIAL="${1:?usage: emulator-ci.sh SERIAL OUT_DIR}"
OUT="$(mkdir -p "${2:?usage: emulator-ci.sh SERIAL OUT_DIR}" && cd "$2" && pwd)"
export ANDROID_LOG_MCP_BIN="${ANDROID_LOG_MCP_BIN:-$ROOT/server/android-log-mcp}"
ADB="$(command -v adb || echo adb)"
APK="$ROOT/validation/logtest-app/build/logtest.apk"
SCRATCH="$OUT/scratch"
mkdir -p "$SCRATCH/svc" "$SCRATCH/app" "$SCRATCH/clock"
: > "$OUT/results.tsv"
FAILED=0
cd "$ROOT"

step() { # name, command...
  local name=$1; shift
  local dir="$OUT/$name" t0 rc res first
  mkdir -p "$dir"
  echo "::group::$name"
  t0=$(date +%s)
  "$@" > "$dir/log.txt" 2>&1; rc=$?
  tail -40 "$dir/log.txt"
  echo "::endgroup::"
  if [ $rc -eq 0 ]; then res=pass; first=""; else
    res="fail (exit $rc)"; FAILED=$((FAILED + 1))
    first=$(grep -m1 -E '(^|[^A-Z])FAIL' "$dir/log.txt" | cut -c1-200)
    [ -n "$first" ] || first=$(tail -1 "$dir/log.txt" | cut -c1-200)
  fi
  echo "$res" > "$dir/result.txt"
  printf '%s\t%s\t%s\t%s\n' "$name" "$res" "$(( $(date +%s) - t0 ))" "$first" >> "$OUT/results.tsv"
}

prepare() {
  "$ADB" -s "$SERIAL" wait-for-device
  "$ADB" -s "$SERIAL" shell getprop ro.build.version.sdk
  # a restored snapshot starts with the device clock of the snapshot; wait
  # until the emulator synced it to host time
  local i d
  for i in $(seq 1 60); do
    d=$(( $("$ADB" -s "$SERIAL" shell date +%s | tr -d '\r') - $(date +%s) ))
    [ "${d#-}" -le 2 ] && break
    sleep 1
  done
  echo "device clock − host clock: ${d}s after ${i}s"
  [ "${d#-}" -le 2 ] || { echo "FAIL device clock not synced to host time (${d}s)"; return 1; }
  # the snapshot also carries old log lines: start from empty buffers
  "$ADB" -s "$SERIAL" logcat -b all -c || true
  "$ADB" -s "$SERIAL" install -r -t "$APK"
}

compat() {
  scripts/compat-probe.sh "$SERIAL" "$OUT/compat/probe" || return 1
  # fixture staleness: summary only, never a failure
  local ref="validation/compat/api29-emulator-5556/compat.txt"
  if [ "$("$ADB" -s "$SERIAL" shell getprop ro.build.version.sdk | tr -d '\r')" = 29 ] && [ -f "$ref" ]; then
    norm() { sed -E 's/[0-9]+/#/g' "$1" | grep -vE '^(date|host|tool|adb) '; }
    diff <(norm "$ref") <(norm "$OUT/compat/probe/compat.txt") > "$OUT/compat/fixture-diff.txt" || true
    echo "compat.txt vs $ref (digits normalized): $(grep -c '^[<>]' "$OUT/compat/fixture-diff.txt") changed lines"
  fi
}

service_e2e() {
  XDG_CACHE_HOME="$SCRATCH/svc" scripts/service-e2e.sh "$SERIAL" "$SCRATCH/svc-newroot"
}

report_e2e() {
  XDG_CACHE_HOME="$SCRATCH/app" scripts/e2e-report.sh "$SERIAL" "$OUT/report/evidence"
}

app_e2e() {
  XDG_CACHE_HOME="$SCRATCH/app" scripts/app-e2e.sh "$SERIAL" "$OUT/app/calls"
}

clock_e2e() {
  XDG_CACHE_HOME="$SCRATCH/clock" scripts/clock-e2e.sh "$SERIAL" "$OUT/clock"
}

# fixture candidates (artifacts, not committed) and the tag inventory
# against the committed API 29 fixtures: summary only
fixtures() {
  local d="$OUT/fixtures"
  "$ADB" -s "$SERIAL" logcat -d -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable > "$d/logcat.txt"
  "$ADB" -s "$SERIAL" shell dumpsys dropbox --print > "$d/dropbox.txt" 2>&1 || true
  "$ADB" -s "$SERIAL" shell getprop > "$d/getprop.txt"
  tags() { grep -ohE ' [a-z]{2}_[a-z_]+ ?: ' "$@" | sed -E 's/ ?: $//; s/^ //' | sort -u; }
  tags "$d/logcat.txt" > "$d/event-tags.txt"
  tags server/internal/testdata/api29/*.logcat.txt > "$d/event-tags-committed.txt"
  comm -3 "$d/event-tags-committed.txt" "$d/event-tags.txt" > "$d/event-tags-diff.txt"
  echo "event tags: $(wc -l < "$d/event-tags.txt") seen; only in committed fixtures / only in this run:"
  cat "$d/event-tags-diff.txt"
}

stop_services() {
  local x
  for x in svc app clock; do XDG_CACHE_HOME="$SCRATCH/$x" "$ANDROID_LOG_MCP_BIN" service stop || true; done
  # the service logs are evidence
  find "$SCRATCH" -name '*.log' -path '*android-log-mcp*' | while read -r f; do
    cp "$f" "$OUT/service-logs/$(printf '%s' "${f#"$SCRATCH"/}" | tr / _)"
  done
}

mkdir -p "$OUT/service-logs"
step prepare prepare
step compat compat
step service-e2e service_e2e
step report-e2e report_e2e
step app-e2e app_e2e
step clock-e2e clock_e2e
step fixtures fixtures
stop_services > "$OUT/service-logs/stop.txt" 2>&1
column -t -s $'\t' "$OUT/results.tsv" || cat "$OUT/results.tsv"
[ "$FAILED" -eq 0 ]
