#!/bin/bash
C=/workspace/android-log-mcp/validation/scripts/call2.sh
$C launches2 logcat_app_launches '{"package":"com.example.logtest","from":"2026-10-10 03:56:00"}'
$C crashes_all2 logcat_crashes '{"package":"com.example.logtest","from":"2026-10-10 03:42:30"}'
$C crashes_anr2 logcat_crashes '{"package":"com.example.logtest","kinds":["anr"],"from":"2026-10-10 03:56:00"}'
$C crash_detail_native logcat_crashes '{"id":"cr8"}'
$C crash_detail_abort logcat_crashes '{"id":"cr9"}'
$C crash_detail_java logcat_crashes '{"id":"cr7"}'
$C status_pkg2 logcat_status '{"package":"com.example.logtest"}'
$C search_tags2 logcat_search '{"filter":"tag=:LogTest tag=:MyRepo level:I","from":"2026-10-10 03:57:30","limit":15}'
$C summary_pkg2 logcat_search '{"package":"com.example.logtest","view":"summary","from":"2026-10-10 03:56:00"}'
echo CALLS-DONE
