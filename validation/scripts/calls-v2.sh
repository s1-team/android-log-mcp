#!/bin/bash
C=/workspace/android-log-mcp/validation/scripts/call2.sh
$C launches logcat_app_launches '{"package":"com.example.logtest","from":"2026-10-10 03:42:30"}'
$C crashes_all logcat_crashes '{"package":"com.example.logtest","from":"2026-10-10 03:42:30"}'
$C crashes_java logcat_crashes '{"package":"com.example.logtest","kinds":["java"],"from":"2026-10-10 03:42:30"}'
$C crashes_native logcat_crashes '{"package":"com.example.logtest","kinds":["native"],"from":"2026-10-10 03:42:30"}'
$C crashes_anr logcat_crashes '{"package":"com.example.logtest","kinds":["anr"],"from":"2026-10-10 03:42:30"}'
$C crashes_history logcat_crashes '{"package":"com.example.logtest"}'
$C status_pkg logcat_status '{"package":"com.example.logtest"}'
$C search_tags logcat_search '{"filter":"tag=:LogTest tag=:MyRepo level:I","from":"2026-10-10 03:46:00","limit":15}'
$C search_mark logcat_search '{"filter":"tag:LOGMCP_MARK","from":"2026-10-10 03:48:00"}'
$C search_printable logcat_search '{"filter":"message:\"café ✓ 中文\"","from":"2026-10-10 03:48:00"}'
$C search_debug_pkg logcat_search '{"package":"com.example.logtest","filter":"tag:DEBUG","from":"2026-10-10 03:42:30","limit":12}'
$C summary_pkg logcat_search '{"package":"com.example.logtest","view":"summary","from":"2026-10-10 03:42:30"}'
echo CALLS-DONE
