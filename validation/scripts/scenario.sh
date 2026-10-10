#!/bin/bash
# usage: scenario.sh <name> <wait_secs> <cmd...>   (runs cmd via adb shell, captures logcat during it)
export ANDROID_SERIAL=${ANDROID_SERIAL:-emulator-5556}
source /workspace/android-log-mcp/validation/scripts/common.sh
name=$1; w=$2; shift 2
f=$FX/$name.logcat.txt
adb logcat $BUFS $LOGFMT -T 1 > "$f" 2>&1 & cp=$!
sleep 2
echo "### host $(date '+%F %T %z') device $(adb shell date '+%F_%T.%N_%z' | tr -d '\r')" > $OUT/$name.cmd.txt
echo "\$ adb shell $*" >> $OUT/$name.cmd.txt
adb shell "$@" >> $OUT/$name.cmd.txt 2>&1
echo "### exit=$? end device $(adb shell date '+%F_%T.%N' | tr -d '\r')" >> $OUT/$name.cmd.txt
sleep "$w"; kill $cp; wait $cp 2>/dev/null
echo "$f: $(wc -l < $f) lines"; cat $OUT/$name.cmd.txt
