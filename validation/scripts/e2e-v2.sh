#!/bin/bash
# v2 end-to-end scenarios on emulator-5556 (app triggers only; same commands as validation/api29/*.cmd.txt)
. /workspace/android-log-mcp/validation/scripts/env.sh
D=/workspace/android-log-mcp/validation/mcp-v2
step() { # name, wait-after-seconds, command...
  local n=$1 w=$2; shift 2
  { echo "### host $(date '+%F %T %z') device $(adb -s emulator-5556 shell date +%F_%T.%N_%z | tr -d '\r')"
    echo "\$ adb shell $*"
    adb -s emulator-5556 shell "$@"; echo "### exit=$? end device $(adb -s emulator-5556 shell date +%F_%T.%N | tr -d '\r')"; } > $D/$n.cmd.txt 2>&1
  cat $D/$n.cmd.txt; sleep $w
}
step 01_coldstart 15 am start -W -n com.example.logtest/.MainActivity
step 02_home 5 input keyevent KEYCODE_HOME
step 03_hotstart 15 am start -W -n com.example.logtest/.MainActivity
step 04_newintent 10 am start -W -n com.example.logtest/.MainActivity
step 05_java_crash 25 am start -n com.example.logtest/.MainActivity --es what crash_caused
step 06_native_crash 45 am start -n com.example.logtest/.MainActivity --es what native
step 07_native_abort 45 am broadcast -n com.example.logtest/.TriggerReceiver --es what native_abort
step 08_remote 20 am broadcast -n com.example.logtest/.TriggerReceiver --es what remote
step 09_remote_crash 60 am broadcast -n com.example.logtest/.TriggerReceiver --es what remote_crash
step 10_anr_broadcast 60 am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast
step 11_mark 3 'log -t LOGMCP_MARK "hello mark from shell"; log -p e -t LOGMCP_MARK "error mark"; log -t LOGMCP_MARK "printable: café ✓ 中文"'
step 12_ps 1 ps -A -o PID,UID,NAME '| grep -E "logtest|PID"'
echo E2E-DONE
