#!/bin/bash
# part 3: native crash + abort again after the DropBox-merge fix
. /workspace/android-log-mcp/validation/scripts/env.sh
D=/workspace/android-log-mcp/validation/mcp-v2
step() { local n=$1 w=$2; shift 2
  { echo "### host $(date '+%F %T %z') device $(adb -s emulator-5556 shell date +%F_%T.%N_%z | tr -d '\r')"
    echo "\$ adb shell $*"
    adb -s emulator-5556 shell "$@"; echo "### exit=$? end device $(adb -s emulator-5556 shell date +%F_%T.%N | tr -d '\r')"; } > $D/$n.cmd.txt 2>&1
  cat $D/$n.cmd.txt; sleep $w; }
step 18_native_crash2 60 am start -n com.example.logtest/.MainActivity --es what native
step 19_ps 1 ps -A -o PID,UID,NAME '| grep -E "logtest|crash_dump|PID"'
C=/workspace/android-log-mcp/validation/scripts/call2.sh
$C crashes_native2 logcat_crashes '{"package":"com.example.logtest","kinds":["native"],"from":"2026-10-10 04:00:00"}'
$C status_pkg3 logcat_status '{"package":"com.example.logtest"}'
echo E2E-DONE
