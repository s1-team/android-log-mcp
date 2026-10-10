package com.example.logtest;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;
import android.widget.Button;
import android.widget.LinearLayout;

public class MainActivity extends Activity {
  @Override protected void onCreate(Bundle b) {
    super.onCreate(b);
    Log.v("LogTest", "onCreate verbose");
    Log.d("LogTest", "onCreate debug pid=" + android.os.Process.myPid() + " uid=" + android.os.Process.myUid());
    Log.i("MyRepo", "feed loaded items=20");
    Log.d("OkHttp", "--> GET https://api.example.com/v1/feed");
    Log.d("OkHttp", "<-- 200 https://api.example.com/v1/feed (312ms)");
    Log.w("MyRepo", "token refresh failed", new java.io.IOException("token endpoint failed", new IllegalStateException("Unable to resolve host \"push.example.com\"")));
    Log.e("LogTest", "multi-line message\nline two\n\tline three (tab)");
    LinearLayout ll = new LinearLayout(this);
    ll.setOrientation(LinearLayout.VERTICAL);
    for (String w : new String[]{"crash", "crash_caused", "native", "native_abort", "anr", "remote", "remote_crash", "spam"}) {
      Button btn = new Button(this);
      btn.setText(w);
      btn.setOnClickListener(v -> Triggers.run(this, w));
      ll.addView(btn);
    }
    setContentView(ll);
  }

  @Override protected void onStart() { super.onStart(); Log.d("LogTest", "onStart"); }

  @Override protected void onResume() {
    super.onResume();
    Log.i("LogTest", "onResume");
    final String what = getIntent().getStringExtra("what");
    if (what != null) {
      getIntent().removeExtra("what");
      // run after resume completes so launch events are logged first
      new Handler(Looper.getMainLooper()).postDelayed(() -> Triggers.run(this, what), 300);
    }
  }

  @Override protected void onNewIntent(Intent i) { super.onNewIntent(i); setIntent(i); }
}
