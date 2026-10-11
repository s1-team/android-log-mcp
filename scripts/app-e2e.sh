#!/usr/bin/env bash
# End-to-end app scenarios with assertions on a running emulator or device
# (issue #27): launches, Java / native / abort / :remote crashes, a
# broadcast ANR and log markers, checked through `android-log-mcp call`.
#
#   scripts/app-e2e.sh SERIAL OUT_DIR
#
# Needs the test app installed (validation/logtest-app, package
# com.example.logtest). Starts the app, crashes it and logs markers; no other
# device change. Uses the per-user control dir of its environment: set
# XDG_CACHE_HOME (Linux) to a scratch directory unless you mean to test the
# real service (AGENTS.md §7).
#   env ANDROID_LOG_MCP_BIN: the binary (default: built from this checkout)
# Exit status: 0 when every check passed. Each check prints PASS / FAIL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERIAL="${1:?usage: app-e2e.sh SERIAL OUT_DIR}"
OUT="${2:?usage: app-e2e.sh SERIAL OUT_DIR}"
ADB="${ANDROID_LOG_MCP_ADB:-$(command -v adb || echo adb)}"
PKG=com.example.logtest
mkdir -p "$OUT"
if [ -n "${ANDROID_LOG_MCP_BIN:-}" ]; then
  BIN="$ANDROID_LOG_MCP_BIN"
else
  BIN="$OUT/android-log-mcp"
  (cd "$ROOT/server" && go build -o "$BIN" ./cmd/android-log-mcp) || exit 1
fi

FAILS=0
check() { # description, command...
  local what=$1; shift
  if "$@"; then echo "PASS $what"; else echo "FAIL $what"; FAILS=$((FAILS + 1)); fi
}
adbs() { "$ADB" -s "$SERIAL" shell "$@" | tr -d '\r'; }
call() { # name, tool, json → OUT/name.txt
  "$BIN" call "$2" "$3" > "$OUT/$1.txt" 2>"$OUT/$1.err" || { echo "call $2 $3 failed: $(tail -3 "$OUT/$1.err")"; return 1; }
}
step() { # name, wait-after-seconds, adb shell command...
  local n=$1 w=$2; shift 2
  { echo "### device $(adbs date '+%F %T')"; echo "\$ adb shell $*"; "$ADB" -s "$SERIAL" shell "$@"; echo "### exit=$?"; } > "$OUT/step-$n.txt" 2>&1
  sleep "$w"
}
has() { grep -qE -- "$2" "$OUT/$1.txt"; }

"$ADB" -s "$SERIAL" get-state >/dev/null || { echo "FAIL device $SERIAL not online"; exit 1; }
adbs pm path "$PKG" | grep -q package: || { echo "FAIL $PKG not installed"; exit 1; }
# start recording (the service starts on use), then remember device now
call status0 logcat_status "{\"device\":\"$SERIAL\"}" || exit 1
FROM="$(adbs date '+%Y-%m-%d %H:%M:%S')"
RUN="ALMCI$(date +%s)"
echo "run $RUN, device $SERIAL, from (device time) $FROM"
adbs am force-stop "$PKG"

A="am start -W -n $PKG/.MainActivity"
B="am broadcast -n $PKG/.TriggerReceiver --es what"
step 01_coldstart 15 $A
step 02_home 5 input keyevent KEYCODE_HOME
step 03_hotstart 15 $A
step 04_newintent 10 $A
step 05_mark 3 "log -t ALMCI_MARK '$RUN hello'; log -p e -t ALMCI_MARK '$RUN error mark'; log -t ALMCI_MARK '$RUN printable: café ✓ 中文'"
step 06_remote 20 $B remote
step 07_java_crash 25 am start -n "$PKG/.MainActivity" --es what crash_caused
step 08_coldstart 15 $A
step 09_native_crash 45 am start -n "$PKG/.MainActivity" --es what native
step 10_coldstart 15 $A
step 11_native_abort 45 am start -n "$PKG/.MainActivity" --es what native_abort
step 12_coldstart 15 $A
step 13_remote_crash 60 $B remote_crash
step 14_anr_broadcast 60 am broadcast --receiver-foreground -n "$PKG/.TriggerReceiver" --es what anr_broadcast
step 15_ps 1 ps -A -o PID,UID,NAME

D="\"device\":\"$SERIAL\""
call launches logcat_app_launches "{$D,\"package\":\"$PKG\",\"from\":\"$FROM\",\"limit\":20}" || FAILS=$((FAILS + 1))
check "a COLD launch of $PKG/.MainActivity" has launches "COLD +$PKG/\.MainActivity"
check "a HOT launch of $PKG/.MainActivity" has launches "HOT +$PKG/\.MainActivity"
check "a TTID for a launch" has launches "TTID \(time to initial display, system-measured\): [0-9]+ ms"

call crashes logcat_crashes "{$D,\"package\":\"$PKG\",\"from\":\"$FROM\",\"limit\":50}" || FAILS=$((FAILS + 1))
check "Java crash (wrapped, root cause shown)" has crashes "LogTest wrapped crash"
check "Java crash root cause" has crashes "repo not ready"
check "native SIGSEGV in liblogtest.so" has crashes "native .*SIGSEGV.*liblogtest\.so"
check "native SIGABRT with the abort message" has crashes "native .*SIGABRT.*LogTest deliberate abort"
check "Java crash in the :remote process" has crashes "java +pid [0-9]+ $PKG:remote"
check "broadcast ANR of $PKG" has crashes "anr +pid [0-9]+ $PKG "

call marks logcat_search "{$D,\"filter\":\"tag:ALMCI_MARK message:$RUN\",\"from\":\"$FROM\"}" || FAILS=$((FAILS + 1))
check "marker line" has marks "$RUN hello"
check "error marker at level E" has marks "[0-9] E .*ALMCI_MARK: $RUN error mark"
check "non-ASCII marker intact" has marks "$RUN printable: café ✓ 中文"
check "markers counted exactly (3)" has marks "^count: total=3 count_exact=true"

call status logcat_status "{$D,\"package\":\"$PKG\"}" || FAILS=$((FAILS + 1))
check "logcat_status lists $PKG:remote" has status "$PKG:remote"

echo "app-e2e: $FAILS failed check(s)"
[ "$FAILS" -eq 0 ]
