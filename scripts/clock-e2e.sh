#!/usr/bin/env bash
# End-to-end backward-clock matrix with assertions on a running emulator
# (issue #27 part 2 / design step 6): #48 search windows, addendum A(a)–(d),
# #54 launch straddle, #52 reconnect across a jump, #65 buffer-vs-store.
# Checked through `android-log-mcp call` (fmt=1).
#
#   scripts/clock-e2e.sh SERIAL OUT_DIR
#
# Needs the test app installed (com.example.logtest). Changes the device
# clock (auto_time off) and may take `adb root`; a trap restores auto_time,
# the host-synced clock and unroots. Uses the per-user control dir of its
# environment: set XDG_CACHE_HOME to a scratch directory (AGENTS.md §7).
#   env ANDROID_LOG_MCP_BIN: the binary (default: built from this checkout)
#   env ANDROID_LOG_MCP_MAX_AGE: default 30d so stages are not pruned mid-run
# Never run under nested KVM on the dev box (AGENTS.md §6); CI only.
# Exit status: 0 when every check passed. Each check prints PASS / FAIL.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERIAL="${1:?usage: clock-e2e.sh SERIAL OUT_DIR}"
OUT="$(mkdir -p "${2:?usage: clock-e2e.sh SERIAL OUT_DIR}" && cd "$2" && pwd)"
ADB="${ANDROID_LOG_MCP_ADB:-$(command -v adb || echo adb)}"
PKG=com.example.logtest
TAG=E2EBACK
export ANDROID_LOG_MCP_MAX_AGE="${ANDROID_LOG_MCP_MAX_AGE:-30d}"
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
# one shell command string (keeps quoting of markers intact)
adbsh() { "$ADB" -s "$SERIAL" shell "$1" | tr -d '\r'; }
call() { # name, tool, json → OUT/name.txt
  "$BIN" call "$2" "$3" > "$OUT/$1.txt" 2>"$OUT/$1.err" || {
    echo "call $2 failed: $(tail -3 "$OUT/$1.err")"
    return 1
  }
}
has() { grep -qE -- "$2" "$OUT/$1.txt"; }
count_re() { grep -cE -- "$2" "$OUT/$1.txt" || true; }

"$ADB" -s "$SERIAL" get-state >/dev/null || { echo "FAIL device $SERIAL not online"; exit 1; }
adbs pm path "$PKG" | grep -q package: || { echo "FAIL $PKG not installed"; exit 1; }

D="\"device\":\"$SERIAL\""
RUN="E2ECLK$(date +%s)"
OFF0=$(( $(adbs date +%s) - $(date +%s) ))
Z=$(adbs date +%z)
TZOFF=$(( 10#${Z:1:2} * 3600 + 10#${Z:3:2} * 60 ))
[ "${Z:0:1}" = - ] && TZOFF=$(( -TZOFF ))
fmt() { # epoch seconds → device wall clock string
  python3 -c "from datetime import datetime,timezone,timedelta; print(datetime.fromtimestamp($1, tz=timezone(timedelta(seconds=$TZOFF))).strftime('%Y-%m-%d %H:%M:%S'))"
}
devnow() { adbs date +%s; }
NOW() { echo $(( $(date +%s) + OFF0 )); }
log() { echo "clock-e2e: $*"; }
mark() { adbsh "log -t $TAG '$1'"; }

AT0=$(adbs settings get global auto_time)
ROOTED=0
restore() {
  "$ADB" -s "$SERIAL" root >/dev/null 2>&1 || true
  sleep 2
  "$ADB" -s "$SERIAL" wait-for-device >/dev/null 2>&1 || true
  adbsh "date @$(( $(date +%s) + OFF0 ))" >/dev/null 2>&1 || true
  adbs settings put global auto_time 1 >/dev/null 2>&1 || true
  "$ADB" -s "$SERIAL" unroot >/dev/null 2>&1 || true
  sleep 2
  "$ADB" -s "$SERIAL" wait-for-device >/dev/null 2>&1 || true
  log "restored auto_time=$(adbs settings get global auto_time) offset=$(( $(adbs date +%s) - $(date +%s) ))s (was $AT0)"
}
trap restore EXIT

wait_dev() {
  local i
  for i in $(seq 1 90); do
    [ "$("$ADB" -s "$SERIAL" get-state 2>/dev/null)" = device ] && adbs true 2>/dev/null && return 0
    sleep 2
  done
  log "device not back after adb root/unroot"
  return 1
}
caught() {
  local i
  for i in $(seq 1 90); do
    call st_tmp logcat_status "{$D}" || true
    has st_tmp 'caught_up=1' && return 0
    sleep 2
  done
  log "NOT caught up"
  return 1
}
setcmd() { # epoch_seconds label
  adbsh "cmd alarm set-time $(( $1 * 1000 ))" > "$OUT/set.$2.txt" 2>&1 || true
}
setroot() { # epoch_seconds label
  adbsh "date @$1" > "$OUT/set.$2.txt" 2>&1 || true
}
ensure_root() {
  if [ "$ROOTED" = 1 ]; then return 0; fi
  "$ADB" -s "$SERIAL" root >/dev/null 2>&1
  sleep 3
  wait_dev || return 1
  ROOTED=1
  caught || true
  log "rooted uid=$(adbs id -u)"
}

call status0 logcat_status "{$D,\"record_for\":\"90m\"}" || exit 1
caught || { echo "FAIL initial catch-up"; exit 1; }
adbs settings put global auto_time 0
log "run $RUN serial=$SERIAL offset=${OFF0}s tz=$Z auto_time=0"

# --- one matrix stage: markers + launch + Java crash, then (a)–(d) ---
stage() { # name
  local n=$1 t1 t2 day i cw=0
  sleep 3
  caught || true
  t1=$(devnow)
  mark "$RUN $n m1"
  adbs am force-stop "$PKG"
  adbs am start -W -n "$PKG/.MainActivity" > "$OUT/$n.amstart.txt" 2>&1 || true
  sleep 2
  adbs am start -n "$PKG/.MainActivity" --es what crash >/dev/null 2>&1 || true
  sleep 6
  mark "$RUN $n m2"
  t2=$(devnow)
  printf '%s %s %s\n' "$n" "$t1" "$t2" >> "$OUT/stages.txt"
  log "stage $n $(fmt "$t1") .. $(fmt "$t2")"
  sleep 3
  day=$(fmt "$t1" | cut -d' ' -f1)
  for i in $(seq 1 20); do
    call "$n.crashes" logcat_crashes "{$D,\"package\":\"$PKG\",\"from\":\"$(fmt $((t1 - 60)))\",\"to\":\"$(fmt $((t2 + 120)))\",\"limit\":20}" || true
    if has "$n.crashes" "LogTest deliberate Java crash|kind=java"; then
      cw=$((i * 2)); break
    fi
    sleep 2
  done
  echo "$n crash_wait_s=$cw" >> "$OUT/cwait.txt"

  call "$n.age5m" logcat_search "{$D,\"filter\":\"tag=:$TAG age:5m\",\"limit\":50}" || true
  call "$n.from5m" logcat_search "{$D,\"filter\":\"tag=:$TAG\",\"from\":\"5m\",\"limit\":50}" || true
  call "$n.abs" logcat_search "{$D,\"filter\":\"tag=:$TAG\",\"from\":\"$(fmt $((t1 - 60)))\",\"to\":\"$(fmt $((t2 + 60)))\",\"limit\":50}" || true
  call "$n.all" logcat_search "{$D,\"filter\":\"tag=:$TAG message:$RUN\",\"limit\":200}" || true
  call "$n.launches" logcat_app_launches "{$D,\"package\":\"$PKG\",\"from\":\"$(fmt $((t1 - 60)))\",\"to\":\"$(fmt $((t2 + 60)))\",\"limit\":20}" || true
  call "$n.status" logcat_status "{$D}" || true

  # (a) relative windows from device now (addendum A)
  check "$n (a) age:5m has m1" has "$n.age5m" "$RUN $n m1"
  check "$n (a) age:5m has m2" has "$n.age5m" "$RUN $n m2"
  check "$n (a) from=5m has m1" has "$n.from5m" "$RUN $n m1"
  check "$n (a) from=5m has m2" has "$n.from5m" "$RUN $n m2"
  # (b) absolute device-time window
  check "$n (b) abs has m1 once" [ "$(count_re "$n.abs" "$RUN $n m1")" = 1 ]
  check "$n (b) abs has m2 once" [ "$(count_re "$n.abs" "$RUN $n m2")" = 1 ]
  # (c) crash in the stage window (may land late; final pass below)
  if [ "$cw" != 0 ]; then
    check "$n (c) Java crash in stage window" has "$n.crashes" "LogTest deliberate Java crash|kind=java"
  else
    echo "NOTE $n (c) crash not yet in window (stream lag); checked in final"
  fi
  # (d) launch in the stage window
  check "$n (d) a launch of $PKG" has "$n.launches" "component=$PKG/"

  # after a jump, searches that span timelines should show clock_jumps=
  if [ "$n" != s0_now ]; then
    if has "$n.all" 'clock_jumps='; then
      check "$n clock_jumps in all-markers search" true
    else
      # first jump may not yet split this filter's matches if only post-jump markers match
      echo "NOTE $n no clock_jumps line in all-markers (ok if matches sit on one side)"
    fi
    if has "$n.age5m" 'clock_future matches='; then
      check "$n clock_future note after set-back" true
    fi
  fi
}

: > "$OUT/stages.txt"
: > "$OUT/cwait.txt"
: > "$OUT/straddles.txt"

# #48 matrix (design: back 10m / 3h / 2d / 1970, forward 1d / 1y; plus now)
stage s0_now

setcmd $(( $(NOW) - 600 )) s1
log "s1 cmd -10m dev=$(fmt "$(devnow)")"
stage s1_m10m

setcmd $(( $(NOW) - 3 * 3600 )) s2
log "s2 cmd -3h dev=$(fmt "$(devnow)")"
stage s2_m3h

ensure_root || { echo "FAIL adb root"; FAILS=$((FAILS + 1)); }
setroot $(( $(NOW) - 2 * 86400 )) s3
log "s3 root -2d dev=$(fmt "$(devnow)")"
stage s3_m2d

# 1970-01-05 00:00 UTC; kernels refuse a wall time below device uptime
setcmd 345600 s4
log "s4 cmd 1970-01-05 dev=$(fmt "$(devnow)")"
stage s4_1970

setcmd $(( $(NOW) + 86400 )) s5
log "s5 cmd +1d (future) dev=$(fmt "$(devnow)")"
stage s5_p1d

setcmd $(( $(NOW) + 365 * 86400 )) s5b
log "s5b cmd +1y (future) dev=$(fmt "$(devnow)")"
stage s5b_p1y

setcmd "$(NOW)" s6
log "s6 cmd back to now dev=$(fmt "$(devnow)")"
stage s6_now

# --- #54: launch whose steps straddle a clock jump ---
straddle() { # name method target_epoch
  local n=$1 how=$2 tgt=$3 t0 c0 i
  caught || true
  adbs am force-stop "$PKG"
  sleep 1
  t0=$(devnow)
  c0=$(adbsh "logcat -d -b system -t 400" | grep 'START u0.*logtest' | tail -1 || true)
  "$ADB" -s "$SERIAL" shell "am start -n $PKG/.MainActivity" >/dev/null 2>&1 &
  for i in $(seq 1 40); do
    [ "$(adbsh "logcat -d -b system -t 400" | grep 'START u0.*logtest' | tail -1 || true)" != "$c0" ] && break
    sleep 0.25
  done
  sleep 4
  if [ "$how" = cmd ]; then setcmd "$tgt" "$n"; else setroot "$tgt" "$n"; fi
  wait 2>/dev/null || true
  sleep 20
  caught || true
  printf '%s %s\n' "$n" "$t0" >> "$OUT/straddles.txt"
  log "straddle $n ($how) request ~$(fmt "$t0") -> $(fmt "$(devnow)")"
  call "$n.launch" logcat_app_launches "{$D,\"package\":\"$PKG\",\"from\":\"$(fmt $((t0 - 5)))\",\"to\":\"$(fmt $((t0 + 120)))\",\"limit\":10}" || true
  check "$n (#54) a launch of $PKG" has "$n.launch" "component=$PKG/"
  check "$n (#54) unreliable=clock_jump on a step or TTID" has "$n.launch" "unreliable=clock_jump"
}
straddle x1_cmd_m1d cmd $(( $(NOW) - 86400 ))
setcmd "$(NOW)" xnow
caught || true

# --- #52: stop service across a jump (set back while stopped, then past old time) ---
mark "$RUN recon before-stop"
sleep 2
"$BIN" service stop > "$OUT/recon.stop.txt" 2>&1 || true
sleep 1
setcmd $(( $(NOW) - 600 )) recon_back
mark "$RUN recon while-stopped"
sleep 2
call recon_status0 logcat_status "{$D,\"record_for\":\"30m\"}" || true
caught || true
mark "$RUN recon after-start"
sleep 3
setcmd "$(NOW)" recon_fwd
sleep 3
caught || true
call recon_marks logcat_search "{$D,\"filter\":\"tag=:$TAG message:$RUN\",\"limit\":20}" || true
check "#52 recon before-stop marker once" [ "$(count_re recon_marks "$RUN recon before-stop")" = 1 ]
check "#52 recon while-stopped marker once" [ "$(count_re recon_marks "$RUN recon while-stopped")" = 1 ]
check "#52 recon after-start marker once" [ "$(count_re recon_marks "$RUN recon after-start")" = 1 ]
check "#52 no duplicate recon markers" [ "$(count_re recon_marks "$RUN recon ")" = 3 ]

# --- final: every stage marker exactly once; crashes reachable ---
call final_all logcat_search "{$D,\"filter\":\"tag=:$TAG message:$RUN\",\"limit\":200}" || true
call final_status logcat_status "{$D}" || true
while read -r n t1 t2; do
  [ -n "$n" ] || continue
  check "final $n m1 once" [ "$(count_re final_all "$RUN $n m1")" = 1 ]
  check "final $n m2 once" [ "$(count_re final_all "$RUN $n m2")" = 1 ]
  call "final.$n.crashes" logcat_crashes "{$D,\"package\":\"$PKG\",\"from\":\"$(fmt $((t1 - 60)))\",\"to\":\"$(fmt $((t2 + 120)))\",\"limit\":20}" || true
  check "final $n (c) crash reachable" has "final.$n.crashes" "LogTest deliberate Java crash|kind=java"
done < "$OUT/stages.txt"
check "final clock_jumps reported" has final_all 'clock_jumps='
check "status lists clock_jumps" has final_status 'clock_jumps'

# --- #65: device buffer vs store for our markers (0 missing) ---
"$ADB" -s "$SERIAL" logcat -d -b main,system,events,crash -v threadtime -v epoch -v usec -v printable \
  > "$OUT/dump.all.txt" 2>/dev/null || true
grep -E "$TAG: $RUN " "$OUT/dump.all.txt" | sed -E "s/.*$TAG: //" | sort > "$OUT/dump.markers.txt" || true
grep -E "$TAG: $RUN " "$OUT/final_all.txt" | sed -E "s/.*$TAG: //" | sort > "$OUT/store.markers.txt" || true
: > "$OUT/missing.markers.txt"
while read -r m; do
  [ -z "$m" ] && continue
  grep -qxF "$m" "$OUT/store.markers.txt" || echo "$m" >> "$OUT/missing.markers.txt"
done < "$OUT/dump.markers.txt"
miss=$(wc -l < "$OUT/missing.markers.txt" | tr -d ' ')
store_n=$(wc -l < "$OUT/store.markers.txt" | tr -d ' ')
dump_n=$(wc -l < "$OUT/dump.markers.txt" | tr -d ' ')
check "#65 store has RUN markers (n=$store_n)" [ "$store_n" -ge 10 ]
check "#65 buffer-vs-store 0 missing markers (dump=$dump_n missing=$miss)" [ "$miss" = 0 ]
SDK=$(adbs getprop ro.build.version.sdk)
if [ "${SDK:-99}" -le 30 ] 2>/dev/null; then
  if has final_status 'code=recovered|repair reads'; then
    check "#65 repair/recovered note on API $SDK" true
  else
    echo "NOTE #65 no recovered note on API $SDK (buffer may not have been pruned)"
  fi
fi

dups=$(sort "$OUT/store.markers.txt" | uniq -d | wc -l | tr -d ' ')
check "0 duplicate markers in store (dups=$dups)" [ "$dups" = 0 ]

echo "clock-e2e: $FAILS failed check(s)"
[ "$FAILS" -eq 0 ]
