#!/bin/bash
# part 2: after "crashed too many times" the system discards background broadcasts
# ("process is bad"); a foreground am start clears that state.
. /workspace/android-log-mcp/validation/scripts/env.sh
D=/workspace/android-log-mcp/validation/mcp-v2
step() { local n=$1 w=$2; shift 2
  { echo "### host $(date '+%F %T %z') device $(adb -s emulator-5556 shell date +%F_%T.%N_%z | tr -d '\r')"
    echo "\$ adb shell $*"
    adb -s emulator-5556 shell "$@"; echo "### exit=$? end device $(adb -s emulator-5556 shell date +%F_%T.%N | tr -d '\r')"; } > $D/$n.cmd.txt 2>&1
  cat $D/$n.cmd.txt; sleep $w; }
step 13_coldstart2 15 am start -W -n com.example.logtest/.MainActivity
step 14_remote 20 am broadcast -n com.example.logtest/.TriggerReceiver --es what remote
step 15_remote_crash 60 am broadcast -n com.example.logtest/.TriggerReceiver --es what remote_crash
step 16_anr_broadcast 60 am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast
step 17_ps 1 ps -A -o PID,UID,NAME '| grep -E "logtest|PID"'
echo E2E-DONE
