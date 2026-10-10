package com.example.logtest;

import android.content.Context;
import android.content.Intent;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.util.Log;

/** Shared trigger dispatch. what = crash | crash_caused | native | native_abort | anr | remote | remote_crash | spam */
final class Triggers {
  static final String TAG = "LogTest";

  static void run(Context ctx, String what) {
    if (what == null) return;
    Log.i(TAG, "trigger what=" + what + " pid=" + android.os.Process.myPid());
    switch (what) {
      case "crash":
        throw new RuntimeException("LogTest deliberate Java crash");
      case "crash_caused":
        try { new Repo().load(); } catch (IllegalStateException e) {
          throw new RuntimeException("LogTest wrapped crash", e);
        }
        break;
      case "native":
        Native.segv();
        break;
      case "native_abort":
        Native.abortWithMessage("LogTest deliberate abort()");
        break;
      case "anr": {
        // Block the main thread; input events dispatched meanwhile cause an input ANR (5s).
        Log.w(TAG, "blocking main thread for 30s (ANR test)");
        SystemClock.sleep(30_000);
        Log.w(TAG, "main thread unblocked");
        break;
      }
      case "remote":
        ctx.startService(new Intent(ctx, RemoteService.class));
        break;
      case "remote_crash":
        ctx.startService(new Intent(ctx, RemoteService.class).putExtra("what", "crash"));
        break;
      case "spam":
        for (int i = 0; i < 50; i++) Log.d("OkHttp", "<-- 200 https://api.example.com/v1/feed (12ms)");
        break;
      default:
        Log.e(TAG, "unknown trigger " + what);
    }
  }

  static final class Repo {
    void load() { throw new IllegalStateException("repo not ready"); }
  }
}
