#!/bin/bash
# 0.6.0 service WITH stored data (logcat_search starts recording) -> 0.7.1 client handoff; first logcat_status text + timing
O=/workspace/v060-bin/android-log-mcp; N=/workspace/v071-bin/android-log-mcp
export XDG_CACHE_HOME=/workspace/scratch/c72u PATH=$HOME/android-sdk/platform-tools:$PATH
S=emulator-5556; ROOT=$XDG_CACHE_HOME/android-log-mcp; ts(){ date +%T; }
echo "== 0.6.0 service, log tool call (starts recording, retry until identity read)"
for i in 1 2 3 4 5 6 7 8; do t0=$(date +%s.%N); r=$($O call logcat_search "{\"device\":\"$S\",\"from\":\"5m\",\"limit\":2}" 2>&1 | head -3 | cut -c1-200); echo "$(ts) try $i: $(echo "$r" | tail -2 | tr '\n' '|' | cut -c1-200)"; echo "$r" | grep -q "not finished" || break; sleep 5; done
sleep 30
$O call logcat_status "{\"device\":\"$S\"}" 2>&1 | sed -n 1,6p | cut -c1-200
echo "== 0.7.1 client: FIRST logcat_status after handoff"
t0=$(date +%s.%N); $N call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -9 | cut -c1-230; t1=$(date +%s.%N); echo "$(ts) first status took $(python3 -c "print(round($t1-$t0,1))")s"
for d in 3 10; do sleep $d; echo "-- +${d}s status:"; $N call logcat_status "{\"device\":\"$S\"}" 2>&1 | grep -E "^recording|^  avd" | cut -c1-200; done
grep -E "stopping|started|device|identity" $ROOT/log/service.log | tail -8 | cut -c1-220
$N service stop
