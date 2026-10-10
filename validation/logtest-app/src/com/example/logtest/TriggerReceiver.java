package com.example.logtest;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.os.SystemClock;
import android.util.Log;

/** am broadcast -n com.example.logtest/.TriggerReceiver --es what X ; what=anr_broadcast blocks onReceive 30s. */
public class TriggerReceiver extends BroadcastReceiver {
  @Override public void onReceive(Context ctx, Intent intent) {
    String what = intent.getStringExtra("what");
    Log.i("LogTest", "TriggerReceiver what=" + what);
    if ("anr_broadcast".equals(what)) {
      SystemClock.sleep(30_000);
      Log.w("LogTest", "receiver unblocked");
      return;
    }
    Triggers.run(ctx, what);
  }
}
