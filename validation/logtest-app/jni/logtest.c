#include <jni.h>
#include <stdlib.h>
#include <android/log.h>
#include <android/set_abort_message.h>

__attribute__((noinline)) static void deref(volatile int *p) { *p = 42; }

JNIEXPORT void JNICALL Java_com_example_logtest_Native_segv(JNIEnv *env, jclass c) {
  __android_log_print(ANDROID_LOG_ERROR, "LogTestNative", "about to SIGSEGV");
  deref((volatile int *)0);
}

JNIEXPORT void JNICALL Java_com_example_logtest_Native_abortWithMessage(JNIEnv *env, jclass c, jstring s) {
  const char *m = (*env)->GetStringUTFChars(env, s, 0);
  __android_log_print(ANDROID_LOG_ERROR, "LogTestNative", "about to abort: %s", m);
  android_set_abort_message(m);
  abort();
}
