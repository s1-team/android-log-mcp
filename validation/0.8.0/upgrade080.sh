#!/bin/bash
# old service (with stored data) -> 0.8.0 client handoff, for 0.6.0 and 0.7.x services; first logcat_status; socket dir migration
N=/workspace/v080-bin/android-log-mcp
export PATH=$HOME/android-sdk/platform-tools:$PATH
S=emulator-5556; ts(){ date +%T; }
for OLD in /workspace/v060-bin/android-log-mcp /workspace/v071-bin/android-log-mcp; do
  V=$($OLD --version 2>&1|head -1); export XDG_CACHE_HOME=/workspace/scratch/r080u-$(basename $(dirname $OLD)); ROOT=$XDG_CACHE_HOME/android-log-mcp
  echo "=== old service $OLD ($V) -> new $($N --version)"; rm -rf $XDG_CACHE_HOME; mkdir -p $XDG_CACHE_HOME
  $OLD call logcat_status "{\"device\":\"$S\"}" >/dev/null 2>&1; sleep 5
  for i in $(seq 1 40); do $OLD call logcat_search "{\"device\":\"$S\",\"from\":\"5m\",\"limit\":2}" 2>&1 | head -3 | grep -qiE "caught up|^ok|^empty|^\[|^[0-9]" && break; sleep 3; done
  sleep 30
  echo "old service status:"; $OLD service status 2>&1 | head -2 | cut -c1-200
  adb -s $S shell log -t ALMUPG "before-handoff-$V"
  echo "--- FIRST logcat_status from the new binary"
  t0=$(date +%s.%N); $N call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -12 | cut -c1-230; t1=$(date +%s.%N); echo "$(ts) first status took $(python3 -c "print(round($t1-$t0,1))")s"
  for d in 3 10 30; do sleep $d; echo "-- +${d}s:"; $N call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -1 | cut -c1-200; done
  echo "new service:"; $N service status 2>&1 | head -2 | cut -c1-200
  echo "sockets:"; ls -la $ROOT/run 2>&1 | head; ss -xl | grep -E "alm-|$(basename $XDG_CACHE_HOME)" | cut -c1-200
  echo "marker count:"; $N call logcat_search '{"tags":["ALMUPG"],"from":"10m"}' 2>&1 | grep -c "before-handoff-$V"
  grep -E "older|replac|stopping|started" $ROOT/log/service.log | tail -6 | cut -c1-220
  $N service stop
done
