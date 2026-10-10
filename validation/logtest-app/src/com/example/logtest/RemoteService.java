package com.example.logtest;

import android.app.Service;
import android.content.Intent;
import android.os.IBinder;
import android.util.Log;

public class RemoteService extends Service {
  @Override public void onCreate() {
    super.onCreate();
    Log.i("LogTest", "RemoteService.onCreate pid=" + android.os.Process.myPid());
  }
  @Override public int onStartCommand(Intent intent, int flags, int startId) {
    Log.d("MyRepo", "remote onStartCommand startId=" + startId);
    if (intent != null && "crash".equals(intent.getStringExtra("what"))) {
      throw new IllegalStateException("LogTest crash in :remote");
    }
    return START_NOT_STICKY;
  }
  @Override public IBinder onBind(Intent i) { return null; }
}
