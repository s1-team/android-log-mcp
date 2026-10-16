#!/bin/bash
# SIGSTOP the real service (0.8.0 build, production timings): automatic replacement, service stop --force, no data loss
B=/workspace/v080-bin/android-log-mcp; export XDG_CACHE_HOME=/workspace/scratch/r080h PATH=$HOME/android-sdk/platform-tools:$PATH
S=emulator-5556; ROOT=$XDG_CACHE_HOME/android-log-mcp
ts(){ date +%T; }
mk(){ adb -s $S shell log -t ALMHUNG "$1"; echo "$(ts) logged $1"; }
cnt(){ $B --offline $ROOT call logcat_search "{\"tags\":[\"ALMHUNG\"],\"from\":\"1h\",\"limit\":200}" 2>&1 | grep -o "H[0-9]*[a-e][a-z0-9-]*" | sort | uniq -c | tr '\n' ' '; echo; }
pid(){ $B service status --json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("pid",0))'; }
caught(){ for i in $(seq 1 80); do $B call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -1 | grep -q "caught_up=1" && return 0; sleep 3; done; return 1; }
R=H$(date +%s)
echo "== A: SIGSTOP + automatic replacement (run $R)"
$B call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -1 | cut -c1-200
caught && echo "$(ts) caught up" || echo "$(ts) NOT caught up"
P1=$(pid); echo "$(ts) service pid $P1"; mk ${R}a
sleep 30
kill -STOP $P1; echo "$(ts) SIGSTOP $P1 ($(ps -o stat= -p $P1))"
mk ${R}b-during-stop
t0=$(date +%s.%N)
$B call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -9 | cut -c1-220
t1=$(date +%s.%N); echo "$(ts) client call took $(python3 -c "print(round($t1-$t0,1))")s"
P2=$(pid); echo "$(ts) new service pid $P2; old pid alive: $(kill -0 $P1 2>/dev/null && echo YES stat=$(ps -o stat= -p $P1) || echo no)"
grep -E "replac|silent|hung|stop" $ROOT/log/service.log | tail -4 | cut -c1-250
mk ${R}c-after
echo "orphan adb children of old pid:"; pgrep -P $P1 -a | cut -c1-100
caught && echo "$(ts) caught up after replace"; sleep 20
echo "== counts (expect each 1):"; cnt
echo "== B: SIGSTOP + service stop --force"
P2=$(pid); kill -STOP $P2; echo "$(ts) SIGSTOP $P2"; mk ${R}d-during-stop2
$B service status 2>&1 | head -3 | cut -c1-250
t0=$(date +%s.%N); $B service stop --force 2>&1 | head -5 | cut -c1-250; t1=$(date +%s.%N); echo "stop --force took $(python3 -c "print(round($t1-$t0,1))")s; pid alive: $(kill -0 $P2 2>/dev/null && echo YES || echo no)"
echo "lock holder / adb children left:"; pgrep -P $P2 -a | cut -c1-100; fuser $ROOT/run/service.lock 2>&1 | head -2
mk ${R}e-after-stop
$B call logcat_status "{\"device\":\"$S\"}" 2>&1 | head -4 | cut -c1-200
echo "new pid $(pid)"; caught && echo "$(ts) caught up"; sleep 30
echo "== final counts (expect each 1 of a,b,c,d,e):"; cnt
$B service stop
