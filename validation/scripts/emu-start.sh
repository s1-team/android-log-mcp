#!/bin/bash
# 启动无窗口模拟器（TCG 软件模拟；本机嵌套 KVM 会触发宿主内核 BUG，禁止使用 /dev/kvm）
# 用法: emu-start.sh [avd=api29] [port=5556] [extra emulator args...]
AVD=${1:-api29}; PORT=${2:-5556}; shift 2 2>/dev/null
export ANDROID_HOME="$HOME/android-sdk" ANDROID_SDK_ROOT="$HOME/android-sdk"
export PATH="$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$PATH"
rm -f "$HOME/.android/avd/$AVD.avd/"*.lock
T0=$(date +%s)
setsid nohup emulator -avd "$AVD" -port "$PORT" -no-window -no-audio -no-boot-anim \
  -gpu swiftshader_indirect -accel off -no-snapshot-save -no-metrics -feature -BluetoothEmulation "$@" \
  -qemu -accel tcg,thread=multi > "$HOME/emulator-$AVD.log" 2>&1 < /dev/null &
echo "started $AVD on emulator-$PORT, log ~/emulator-$AVD.log"
ANDROID_SERIAL=emulator-$PORT "$HOME/bin/wait-boot.sh" "$T0"
