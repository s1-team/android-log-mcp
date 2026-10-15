#!/bin/bash
cd /workspace/alm-wt/v071
echo "START e2e $(date +%T)" >> /workspace/alm-0.7.1-verify-progress.md
export XDG_CACHE_HOME=/workspace/scratch/c72f PATH=$HOME/android-sdk/platform-tools:$PATH ANDROID_LOG_MCP_BIN=/workspace/v071-bin/android-log-mcp
t0=$(date +%s); nice -n 10 bash scripts/service-e2e.sh emulator-5556 /tmp/alm-newroot71 > /workspace/ev/071/service-e2e.txt 2>&1; rc=$?
echo "END e2e rc=$rc secs=$(( $(date +%s)-t0 ))" >> /workspace/alm-0.7.1-verify-progress.md
echo "adb auto_time: $(adb -s emulator-5556 shell settings get global auto_time)" >> /workspace/alm-0.7.1-verify-progress.md
