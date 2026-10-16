#!/bin/bash
# Manual (no-Gradle) build: javac -> d8 -> aapt2 link -> zip -> zipalign -> apksigner
set -euo pipefail
cd "$(dirname "$0")"
# Overrides (CI): ANDROID_HOME, NDK, BT (build-tools dir), AJ (android.jar), JAVA_HOME.
# Defaults: the dev box versions, else the newest installed one.
SDK=${ANDROID_HOME:-$HOME/android-sdk}
newest() { ls -d "$@" 2>/dev/null | sort -V | tail -1; }
NDK=${NDK:-$SDK/ndk/27.3.13750724}; [ -d "$NDK" ] || NDK=$(newest "$SDK"/ndk/*)
BT=${BT:-$SDK/build-tools/35.0.0}; [ -d "$BT" ] || BT=$(newest "$SDK"/build-tools/*)
AJ=${AJ:-$SDK/platforms/android-35/android.jar}; [ -f "$AJ" ] || AJ=$(newest "$SDK"/platforms/android-*/android.jar)
export PATH="${JAVA_HOME:+$JAVA_HOME/bin:}$HOME/jdk/current/bin:$PATH"
echo "build: NDK=$NDK BT=$BT AJ=$AJ" >&2
rm -rf build && mkdir -p build/classes build/dex build/apk/lib/x86_64
javac -nowarn -source 11 -target 11 -cp $AJ -d build/classes $(find src -name '*.java') 2>&1 | grep -v '^warning' || true
$BT/d8 --min-api 29 --lib $AJ --output build/dex $(find build/classes -name '*.class')
$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin/x86_64-linux-android29-clang -O0 -g -shared -fPIC -o build/apk/lib/x86_64/liblogtest.so jni/logtest.c -llog
$BT/aapt2 link --manifest AndroidManifest.xml -I $AJ -o build/base.apk
cp build/dex/classes.dex build/apk/ && (cd build/apk && zip -q -r ../base.apk classes.dex lib)
$BT/zipalign -f -P 16 4 build/base.apk build/aligned.apk
[ -f debug.keystore ] || keytool -genkeypair -keystore debug.keystore -storepass android -keypass android -alias androiddebugkey -keyalg RSA -keysize 2048 -validity 10000 -dname "CN=Android Debug,O=Android,C=US"
$BT/apksigner sign --ks debug.keystore --ks-pass pass:android --out build/logtest.apk build/aligned.apk
echo "built $(pwd)/build/logtest.apk"
