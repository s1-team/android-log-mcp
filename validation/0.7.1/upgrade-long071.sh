#!/bin/bash
O=/workspace/v060-bin/android-log-mcp; N=/workspace/v071-bin/android-log-mcp
export XDG_CACHE_HOME=/workspace/scratch/x72xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/cache PATH=$HOME/android-sdk/platform-tools:$PATH
echo "cache dir length ${#XDG_CACHE_HOME}; primary socket would be $((${#XDG_CACHE_HOME}+33)) bytes (limit 100)"
echo "== 0.6.0 service"; $O call logcat_status "{\"device\":\"emulator-5556\"}" >/dev/null 2>&1; sleep 30; $O call logcat_status '{"device":"emulator-5556"}' 2>&1 | head -3 | cut -c1-200
ss -xl | grep -E "alm-|xxxxx" | cut -c1-200
echo "== 0.7.0 client"; $N call logcat_status '{"device":"emulator-5556"}' 2>&1 | head -8 | cut -c1-200
ss -xl | grep -E "alm-|xxxxx" | cut -c1-200
$N root 2>&1 | grep -i socket
ls -la /tmp/alm-$(id -u)/ 2>&1 | head; ls $XDG_CACHE_HOME/android-log-mcp/run
$N service stop
