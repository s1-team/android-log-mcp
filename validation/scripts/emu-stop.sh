#!/bin/bash
# 用法: emu-stop.sh [serial=emulator-5556]
~/android-sdk/platform-tools/adb -s "${1:-emulator-5556}" emu kill
