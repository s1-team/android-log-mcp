# source me. Requires ANDROID_SERIAL set.
export PATH="$HOME/android-sdk/platform-tools:$PATH"
: "${ANDROID_SERIAL:?set ANDROID_SERIAL}"
V=/workspace/android-log-mcp/validation
API=$(adb shell getprop ro.build.version.sdk | tr -d '\r')
OUT=$V/api$API; FX=$V/fixtures/api$API
mkdir -p "$OUT" "$FX"
PKG=com.example.logtest
LOGFMT="-v threadtime -v epoch -v uid -v usec -v printable"
BUFS="-b main,system,crash,events"
# start a background logcat capture into $1 ; prints pid
cap_start() { adb logcat $BUFS $LOGFMT -T 1 > "$1" 2>&1 & echo $!; }
cap_stop() { sleep "${2:-2}"; kill "$1" 2>/dev/null; wait "$1" 2>/dev/null; }
