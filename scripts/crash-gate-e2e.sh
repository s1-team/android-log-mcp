#!/usr/bin/env bash
# Crash/ANR gate on a running emulator (R2-PR1 gate, issue #233): one crash
# store row per crash even when the service is killed between the evidence
# sources (logcat crash buffer, am_crash/am_anr, tombstone, DropBox) and the
# device clock jumped back.
#
#   scripts/crash-gate-e2e.sh SERIAL OUT_DIR [required|control]
#
#   required (default): service SIGKILLed right after the first row of the
#     kind appears, then restarted. E3 native, E3 native_abort (restart),
#     E1 Java crash and E2 ANR (anr_broadcast), both restarted with the clock
#     set back one day (restored, service restarted again).
#     Asserts: >=1 row of the kind, exactly one row per crashed pid, ids
#     written before the kill still present afterwards.
#   control: the same native / native_abort crashes with NO restart; exactly
#     one row each (#233, fixed by PR #235). Informational in CI until #235
#     merges (env CRASH_GATE_CONTROL_REQUIRED=1 makes it required).
#
# Changes the device clock (auto_time off, adb root); a trap restores it. Own
# XDG_CACHE_HOME per scenario under OUT_DIR/scratch. CI only (AGENTS.md §6).
#   env ANDROID_LOG_MCP_BIN: the binary (default server/android-log-mcp)
# Exit status: 0 when every scenario passed. Writes OUT_DIR/verdicts.tsv.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERIAL="${1:?usage: crash-gate-e2e.sh SERIAL OUT_DIR [required|control]}"
OUT="$(mkdir -p "${2:?usage: crash-gate-e2e.sh SERIAL OUT_DIR [required|control]}" && cd "$2" && pwd)"
MODE="${3:-required}"
B="${ANDROID_LOG_MCP_BIN:-$ROOT/server/android-log-mcp}"
ADB="$(command -v adb || echo adb)"
S=("$ADB" -s "$SERIAL")
PKG=com.example.logtest
SCR="$OUT/scratch"; mkdir -p "$SCR"
: > "$OUT/verdicts.tsv"
log() { echo "$(date +%T) $*" | tee -a "$OUT/gate.log"; }
dev() { "${S[@]}" shell "$@" | tr -d '\r'; }
OFF0=$(( $(dev date +%s) - $(date +%s) ))
restore() {
  "${S[@]}" root >/dev/null 2>&1; sleep 3; "${S[@]}" wait-for-device
  "${S[@]}" shell "date @$(( $(date +%s) + OFF0 ))" >/dev/null 2>&1
  "${S[@]}" shell settings put global auto_time 1 >/dev/null 2>&1
  "${S[@]}" unroot >/dev/null 2>&1; sleep 3; "${S[@]}" wait-for-device
  log "RESTORED auto_time=$(dev settings get global auto_time) offset=$(( $(dev date +%s) - $(date +%s) ))s"
}
trap restore EXIT

rows() { # crash root, known ids (comma) → one line per unknown row
  python3 - "$1" "$2" <<'PY'
import json,sys,glob
root,known=sys.argv[1],set(filter(None,sys.argv[2].split(',')))
rows={}
for f in glob.glob(root+'/v1/*/crashes.jsonl'):
    for l in open(f):
        try: d=json.loads(l)
        except Exception: continue
        rows[d['id']]=d
for i,d in sorted(rows.items()):
    if i in known: continue
    print("%s kind=%s pid=%s sources=%s"%(i,d['kind'],d.get('pid'),'+'.join(d.get('sources',[]))))
PY
}
allids() { rows "$1" "" | cut -d' ' -f1 | paste -sd, -; }
caught() { local i; for i in $(seq 1 80); do "$B" call logcat_status "{\"device\":\"$SERIAL\"}" 2>&1 | head -1 | grep -q "caught_up=1" && return 0; sleep 3; done; return 1; }
svcpid() { "$B" service status --json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("pid",0))' 2>/dev/null || echo 0; }
trig() {
  "${S[@]}" shell am force-stop $PKG; sleep 2
  if [ "$1" = anr_broadcast ]; then
    "${S[@]}" shell am start -W -n $PKG/.MainActivity >/dev/null; sleep 4
    "${S[@]}" shell am broadcast --receiver-foreground -n $PKG/.TriggerReceiver --es what anr_broadcast >/dev/null
  else "${S[@]}" shell am start -n $PKG/.MainActivity --es what "$1" >/dev/null; fi
}

# run NAME TRIGGER KIND restart|control [clock]
run() {
  local N=$1 T=$2 K=$3 M=$4 CK=${5:-} R="$SCR/$N" F="$OUT/$N" i seen=0 PRE SP
  export XDG_CACHE_HOME="$R"; rm -rf "$R"; mkdir -p "$R" "$F"
  local ROOTD="$R/android-log-mcp"
  "$B" call logcat_status "{\"device\":\"$SERIAL\"}" >/dev/null 2>&1
  caught && log "$N caught up" || log "$N NOT caught up"; sleep 5
  local K0; K0=$(allids "$ROOTD")
  if [ -n "$CK" ]; then
    "${S[@]}" shell settings put global auto_time 0; "${S[@]}" root >/dev/null 2>&1; sleep 4; "${S[@]}" wait-for-device
    "${S[@]}" shell "date @$(( $(date +%s) + OFF0 - 86400 ))" >/dev/null
    log "$N clock set back 1 day: $(dev date +%F_%T)"; sleep 3
  fi
  trig "$T"; log "$N trigger $T"
  for i in $(seq 1 600); do rows "$ROOTD" "$K0" | grep -q "kind=$K " && { seen=1; break; }; sleep 0.1; done
  log "$N first $K row seen=$seen after $i polls"
  PRE=$(rows "$ROOTD" "$K0"); echo "$PRE" > "$F/pre-restart-rows.txt"
  if [ "$M" = restart ]; then
    SP=$(svcpid); kill -9 "$SP" 2>/dev/null; sleep 2
    log "$N service $SP killed (SIGKILL); rows at kill:"; echo "$PRE" | tee -a "$OUT/gate.log"
    "$B" call logcat_status "{\"device\":\"$SERIAL\"}" >/dev/null 2>&1; log "$N service restarted: $(svcpid)"
  fi
  if [ "$K" = anr ]; then sleep 40; else sleep 15; fi
  if [ -n "$CK" ] && [ "$M" = restart ]; then
    restore; sleep 3; "$B" service stop >/dev/null 2>&1; sleep 2
    "$B" call logcat_status "{\"device\":\"$SERIAL\"}" >/dev/null 2>&1; log "$N second service restart after clock restore"
  fi
  caught; sleep 15
  rows "$ROOTD" "$K0" > "$F/post-rows.txt"
  "$B" call logcat_crashes '{"from":"3d","limit":30}' > "$F/logcat_crashes.txt" 2>&1
  cp "$ROOTD"/v1/*/crashes.jsonl "$F/crashes.jsonl" 2>/dev/null; cp "$ROOTD/log/service.log" "$F/service.log" 2>/dev/null
  "$B" service stop >/dev/null 2>&1
  python3 - "$F" "$K" "$seen" > "$F/verdict.txt" <<'PY'
import sys,re
F,K,seen=sys.argv[1],sys.argv[2],sys.argv[3]
rd=lambda n:[l for l in open(F+'/'+n).read().splitlines() if l.strip()]
pre,post=rd('pre-restart-rows.txt'),rd('post-rows.txt')
po={l.split()[0] for l in post}
rows=[l for l in post if ' kind=%s '%K in l]
pids=[re.search(r'pid=(\w+)',l).group(1) for l in rows]
kept=all(l.split()[0] in po for l in pre)
print("rows(kind=%s)=%d distinct_pids=%d first_row_seen=%s pre_ids_kept=%s"%(K,len(rows),len(set(pids)),seen=='1',kept))
ok=seen=='1' and len(rows)>=1 and len(rows)==len(set(pids)) and kept
print("VERDICT %s"%("PASS" if ok else "FAIL"))
PY
  cat "$F/verdict.txt" | tee -a "$OUT/gate.log"; cat "$F/post-rows.txt" >> "$OUT/gate.log"
  printf '%s\t%s\t%s\n' "$N" "$(grep -o 'VERDICT .*' "$F/verdict.txt" | cut -d' ' -f2)" "$(head -1 "$F/verdict.txt")" >> "$OUT/verdicts.tsv"
}

"${S[@]}" shell pm path $PKG | grep -q package: || { echo "FAIL $PKG not installed"; exit 1; }
case "$MODE" in
  required)
    run e3-native-restart native native restart
    run e3-abort-restart native_abort native restart
    run e1-java-clock-restart crash_caused java restart clock
    run e2-anr-clock-restart anr_broadcast anr restart clock ;;
  control)
    run e3-native-control native native control
    run e3-abort-control native_abort native control ;;
  *) echo "usage: mode required|control"; exit 2 ;;
esac
cat "$OUT/verdicts.tsv"
! grep -q $'\tFAIL\t' "$OUT/verdicts.tsv"
