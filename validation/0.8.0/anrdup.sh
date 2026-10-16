#!/bin/bash
# Same device, same procedure, two binaries at once (v0.7.1 tag build and 0.8.0), separate scratch stores:
# 3 real ANRs (DropBox entries dated today) -> clock back 1 day -> real ANR + crashes -> clock forward -> compare ANR lists.
export PATH=$HOME/android-sdk/platform-tools:$PATH
B71=/workspace/v071tag-bin/android-log-mcp; B80=/workspace/v080-bin/android-log-mcp
X71=/workspace/scratch/rep071; X80=/workspace/scratch/rep080
D=emulator-5556; S="adb -s $D"; ts(){ date +%T; }; log(){ echo "$(ts) $*"; }
c71(){ XDG_CACHE_HOME=$X71 $B71 call "$@" 2>&1; }; c80(){ XDG_CACHE_HOME=$X80 $B80 call "$@" 2>&1; }
OFF0=$(( $($S shell date +%s | tr -d '\r') - $(date +%s) )); log "start auto_time=$($S shell settings get global auto_time|tr -d '\r') offset=${OFF0}s"
restore(){ $S root >/dev/null 2>&1; sleep 3; $S wait-for-device; $S shell date @$(( $(date +%s)+OFF0 )) >/dev/null 2>&1; $S shell settings put global auto_time 1; $S unroot >/dev/null 2>&1; sleep 3; $S wait-for-device
  log "RESTORED auto_time=$($S shell settings get global auto_time|tr -d '\r') uid=$($S shell id -u|tr -d '\r') offset=$(( $($S shell date +%s|tr -d '\r') - $(date +%s) ))s"; }
trap restore EXIT
caught(){ for i in $(seq 1 90); do c71 logcat_status "{\"device\":\"$D\"}" | head -1 | grep -q caught_up=1 && c80 logcat_status "{\"device\":\"$D\"}" | head -1 | grep -q caught_up=1 && return 0; sleep 3; done; return 1; }
anr(){ $S shell am force-stop com.example.logtest; sleep 2; $S shell am start -W -n com.example.logtest/.MainActivity >/dev/null; sleep 4; $S shell am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast >/dev/null; log "ANR triggered ($1)"; }
rm -rf $X71 $X80; mkdir -p $X71 $X80
c71 logcat_status "{\"device\":\"$D\"}" >/dev/null; c80 logcat_status "{\"device\":\"$D\"}" >/dev/null
caught && log "both caught up" || log "NOT caught up"
log "dropbox ANR entries before: $($S shell dumpsys dropbox 2>/dev/null | grep -c data_app_anr)"
for i in 1 2 3; do anr pre$i; sleep 80; done
caught; log "dropbox ANR entries after pre-ANRs: $($S shell dumpsys dropbox 2>/dev/null | grep data_app_anr | tail -5 | tr '\n' ';')"
$S shell settings put global auto_time 0; $S root >/dev/null 2>&1; sleep 4; $S wait-for-device
$S shell date @$(( $(date +%s)+OFF0-86400 )); log "clock set back 1 day: device $($S shell date +%F_%T|tr -d '\r')"
sleep 5; anr back; sleep 80
$S shell date @$(( $(date +%s)+OFF0 )); log "clock restored forward"; sleep 5
restore; trap - EXIT
sleep 30; caught
log "dropbox ANR entries now:"; $S shell dumpsys dropbox 2>/dev/null | grep data_app_anr | tail -8
mkdir -p /workspace/rel080/ev/anrdup
for v in 71 80; do c$v logcat_crashes '{"kinds":["anr"],"from":"3d","limit":50}' > /workspace/rel080/ev/anrdup/anr-$v.txt; c$v logcat_status "{\"device\":\"$D\"}" > /workspace/rel080/ev/anrdup/status-$v.txt; done
XDG_CACHE_HOME=$X71 $B71 service stop; XDG_CACHE_HOME=$X80 $B80 service stop
log done
