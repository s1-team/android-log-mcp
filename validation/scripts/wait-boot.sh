#!/bin/bash
# wait until sys.boot_completed=1 and package manager responds; print elapsed seconds from $1 (epoch start) 
start=${1:-$(date +%s)}
until [ "$(adb shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; do sleep 2; done
t1=$(date +%s)
until adb shell pm path android 2>/dev/null | grep -q package:; do sleep 1; done
t2=$(date +%s)
echo "boot_completed after $((t1-start))s, pm ready after $((t2-start))s"
