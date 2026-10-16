#!/bin/bash
# real device clock back-jump (adb shell date) + real Java/native crash + ANR, 0.8.0 service; trap restores auto_time=1 and unroots
B=/workspace/v080fix-bin/android-log-mcp; export XDG_CACHE_HOME=/workspace/scratch/r080k PATH=$HOME/android-sdk/platform-tools:$PATH
D=emulator-5556; S="adb -s $D"; ROOT=$XDG_CACHE_HOME/android-log-mcp; ts(){ date +%T; }
log(){ echo "$(ts) $*"; }
AT0=$($S shell settings get global auto_time | tr -d '\r'); OFF0=$(( $($S shell date +%s | tr -d '\r') - $(date +%s) ))
log "start auto_time=$AT0 device-host offset=${OFF0}s uid=$($S shell id -u | tr -d '\r')"
restore(){ $S root >/dev/null 2>&1; sleep 3; $S wait-for-device; $S shell date @$(( $(date +%s)+OFF0 )) >/dev/null 2>&1; $S shell settings put global auto_time 1; $S unroot >/dev/null 2>&1; sleep 3; $S wait-for-device
  log "RESTORED auto_time=$($S shell settings get global auto_time | tr -d '\r') uid=$($S shell id -u | tr -d '\r') device-host offset=$(( $($S shell date +%s | tr -d '\r') - $(date +%s) ))s"; }
trap restore EXIT
call(){ $B call "$@" 2>&1; }
caught(){ for i in $(seq 1 60); do call logcat_status "{\"device\":\"$D\"}" | head -1 | grep -q "caught_up=1" && return 0; sleep 3; done; return 1; }
trig(){ $S shell am force-stop com.example.logtest; sleep 2
  if [ "$1" = anr_broadcast ]; then $S shell am start -W -n com.example.logtest/.MainActivity >/dev/null; sleep 4; $S shell am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast >/dev/null
  else $S shell am start -W -n com.example.logtest/.MainActivity --es what $1 >/dev/null; fi; log "trigger $1"; }
mk(){ $S shell log -t ALMJUMP8 "$1"; log "marker $1 (device $($S shell date +%F_%T | tr -d '\r'))"; }
call logcat_status "{\"device\":\"$D\"}" >/dev/null; caught && log "caught up" || log "NOT caught up (60x3s)"
mk J0-before; trig crash_caused; sleep 15; mk J0b
$S shell settings put global auto_time 0; $S root >/dev/null 2>&1; sleep 4; $S wait-for-device
$S shell date @$(( $(date +%s)+OFF0-86400 )) ; log "clock set back 1 day: device $($S shell date +%F_%T | tr -d '\r')"
sleep 5; mk J1-after-back; trig crash_caused; sleep 15; mk J1b; trig native; sleep 20; mk J1c
trig anr_broadcast; sleep 40; mk J1d-after-anr
$S shell date @$(( $(date +%s)+OFF0 )); log "clock restored forward: device $($S shell date +%F_%T | tr -d '\r')"
sleep 5; mk J2-forward; trig crash_caused; sleep 15; mk J2b
restore; trap - EXIT
sleep 20; caught; sleep 5
mkdir -p /workspace/rel080/ev/clockjump080fix
call logcat_status "{\"device\":\"$D\"}" > /workspace/rel080/ev/clockjump080fix/status.txt
call logcat_search '{"tags":["ALMJUMP8"],"from":"3d","limit":50}' > /workspace/rel080/ev/clockjump080fix/markers.txt
call logcat_crashes '{"from":"3d","limit":20}' > /workspace/rel080/ev/clockjump080fix/crashes.txt
call logcat_search '{"view":"summary","from":"3d"}' > /workspace/rel080/ev/clockjump080fix/summary.txt
log done
