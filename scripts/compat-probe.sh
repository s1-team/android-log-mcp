#!/usr/bin/env bash
# Compatibility probe for one connected Android device (read-only).
#
#   scripts/compat-probe.sh [SERIAL] [OUT_DIR]
#
# Runs `android-log-mcp compat` (the binary's own probe, evaluated with the
# recorder's parsers) and records host information. With OUT_DIR it also
# writes compat.txt, compat.json, probe-raw.txt and host.txt there.
#
# Binary: $ANDROID_LOG_MCP_BIN, else built from this checkout when Go is
# available, else `npx -y android-log-mcp`. Nothing is written to the device:
# the probe only runs getprop, logcat -d/-g, ps, pm list, dumpsys dropbox and
# help output (it never clears logs or starts apps).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERIAL="${1:-${ANDROID_SERIAL:-}}"
OUT="${2:-}"

if [ -n "${ANDROID_LOG_MCP_BIN:-}" ]; then
  BIN=("$ANDROID_LOG_MCP_BIN")
elif command -v go >/dev/null 2>&1; then
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  (cd "$ROOT/server" && go build -o "$tmp/android-log-mcp" ./cmd/android-log-mcp)
  BIN=("$tmp/android-log-mcp")
else
  BIN=(npx -y android-log-mcp)
fi
args=(compat)
[ -n "$SERIAL" ] && args+=(--serial "$SERIAL")

ADB="${ANDROID_LOG_MCP_ADB:-$(command -v adb || true)}"
host() {
  echo "date        $(date '+%Y-%m-%d %H:%M:%S %z')"
  echo "host        $(uname -srm 2>/dev/null || echo unknown)"
  echo "tool        $("${BIN[@]}" --version)"
  echo "node        $(node --version 2>/dev/null || echo 'not installed')"
  if [ -n "$ADB" ]; then
    echo "adb path    $ADB"
    "$ADB" version | sed 's/^/adb         /'
  fi
}

[ -z "$OUT" ] || mkdir -p "$OUT"
host
echo
status=0
summary="$("${BIN[@]}" "${args[@]}" 2>&1)" || status=$?
printf '%s\n' "$summary"
if [ -n "$OUT" ]; then
  printf '%s\n' "$summary" > "$OUT/compat.txt"
  host > "$OUT/host.txt"
  "${BIN[@]}" "${args[@]}" --json > "$OUT/compat.json" || true
  "${BIN[@]}" "${args[@]}" --raw > "$OUT/probe-raw.txt" || true
  echo "wrote $OUT/{compat.txt,compat.json,probe-raw.txt,host.txt}" >&2
fi
exit $status
