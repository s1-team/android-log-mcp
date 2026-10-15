#!/bin/bash
# real ANR (broadcast, logtest app) on build origin/0.7.x + 129 branch (local only); expect ONE anr entry per real ANR
B=/workspace/v129-bin/android-log-mcp; export XDG_CACHE_HOME=/workspace/scratch/c129 PATH=$HOME/android-sdk/platform-tools:$PATH
D=emulator-5556; S="adb -s $D"; ts(){ date +%T; }; call(){ $B call "$@" 2>&1; }
for i in 1 2 3 4 5 6 7 8; do r=$($B call logcat_search "{\"device\":\"$D\",\"from\":\"5m\",\"limit\":2}" 2>&1 | head -3); echo "$(ts) seed try $i"; echo "$r" | grep -q "not finished" || break; sleep 5; done
for i in $(seq 1 40); do call logcat_status "{\"device\":\"$D\"}" | grep -q "caught up" && { echo "$(ts) caught up"; break; }; sleep 3; done

for run in 1 2 3 4; do
echo "$(ts) run $run: trigger ANR"; T0=$(date +%s)
$S shell am force-stop com.example.logtest; sleep 2
$S shell am start -W -n com.example.logtest/.MainActivity >/dev/null; sleep 4
$S shell am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast >/dev/null
echo "$(ts) waiting 100s for ANR dialog + dump"; sleep 100
done
sleep 10
$S logcat -d -b events 2>/dev/null | grep -E "am_anr" | tail -4 | cut -c1-200
$S logcat -d 2>/dev/null | grep -E "ANR in|Dumping|Wrote stack traces" | tail -8 | cut -c1-200
call logcat_crashes '{"from":"30m","limit":20}' > anr129-crashes.txt; cat anr129-crashes.txt | cut -c1-230
echo "ANR entries listed: $(grep -c ' anr ' anr129-crashes.txt)"
$S shell dumpsys dropbox 2>/dev/null | grep -E "data_app_anr" | tail -6
$B service stop
