#!/bin/bash
# 0.6.0 service + 0.7.0 client (version handoff, socket directory migration)
O=/workspace/v060-bin/android-log-mcp; N=/workspace/v071-bin/android-log-mcp
export XDG_CACHE_HOME=/workspace/scratch/c71b PATH=$HOME/android-sdk/platform-tools:$PATH
S=emulator-5556; ROOT=$XDG_CACHE_HOME/android-log-mcp; ts(){ date +%T; }
mk(){ adb -s $S shell log -t ALMUPG "$1"; echo "$(ts) logged $1"; }
R=U$(date +%s)
echo "== start 0.6.0 service"
$O call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -2 | cut -c1-200
$O service status 2>&1 | head -3 | cut -c1-200
P1=$(pgrep -f "$ROOT.*service --detached" | head -1); echo "0.6.0 pid $P1"
echo "control dir / sockets before:"; find $ROOT/run /tmp -maxdepth 2 \( -name '*.sock' -o -name 'service.*' \) -newermt "-10 min" 2>/dev/null | head; ls -la $ROOT/run
ss -xl 2>/dev/null | grep -E "alm|android-log|c4" | cut -c1-200
mk ${R}a-before
sleep 30
echo "== 0.7.0 client call (handoff expected)"
t0=$(date +%s.%N); $N call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -8 | cut -c1-220; t1=$(date +%s.%N); echo "took $(python3 -c "print(round($t1-$t0,1))")s"
mk ${R}b-after
$N service status 2>&1 | head -3 | cut -c1-200
echo "old 0.6.0 pid alive: $(kill -0 $P1 2>/dev/null && echo YES || echo no)"
echo "control dir / sockets after:"; ls -la $ROOT/run; ss -xl 2>/dev/null | grep -E "alm|android-log|c4" | cut -c1-200
grep -E "handoff|started|stopp|socket" $ROOT/log/service.log | tail -6 | cut -c1-250
echo "== second 0.7.0 client reuses (no new handoff)"; $N service status | head -1 | cut -c1-200
sleep 45
$N --offline $ROOT call logcat_search '{"tags":["ALMUPG"],"from":"1h","limit":50}' 2>&1 | grep -o "U[0-9]\{10\}[a-z0-9-]*" | sort | uniq -c
echo "== old 0.6.0 client against 0.7.0 service (protocol compat)"; $O call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -4 | cut -c1-220
