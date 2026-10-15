#!/bin/bash
# real ANR (broadcast, logtest app) on combined 0.7.1 build; expect ONE anr entry per real ANR
B=/workspace/v071-bin/android-log-mcp; export XDG_CACHE_HOME=/workspace/scratch/c72n PATH=$HOME/android-sdk/platform-tools:$PATH
D=emulator-5556; S="adb -s $D"; ts(){ date +%T; }; call(){ $B call "$@" 2>&1; }
for i in 1 2 3 4 5 6 7 8; do r=$($B call logcat_search "{\"device\":\"$D\",\"from\":\"5m\",\"limit\":2}" 2>&1 | head -3); echo "$(ts) seed try $i"; echo "$r" | grep -q "not finished" || break; sleep 5; done
for i in $(seq 1 40); do call logcat_status "{\"device\":\"$D\"}" | grep -q "caught up" && { echo "$(ts) caught up"; break; }; sleep 3; done

for run in 1 2; do
echo "$(ts) run $run: trigger ANR"; T0=$(date +%s)
$S shell am force-stop com.example.logtest; sleep 2
$S shell am start -W -n com.example.logtest/.MainActivity >/dev/null; sleep 4
$S shell am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast >/dev/null
echo "$(ts) waiting 75s for ANR dialog + dump"; sleep 75
done
sleep 10
$S logcat -d -b events 2>/dev/null | grep -E "am_anr" | tail -4 | cut -c1-200
$S logcat -d 2>/dev/null | grep -E "ANR in|Dumping|Wrote stack traces" | tail -8 | cut -c1-200
call logcat_crashes '{"from":"30m","limit":20}' > anr-live-crashes.txt; cat anr-live-crashes.txt | cut -c1-230
echo "ANR entries listed: $(grep -c ' anr ' anr-live-crashes.txt)"
$B service stop
