package com.example.logtest;
public final class Native {
  static { System.loadLibrary("logtest"); }
  public static native void segv();
  public static native void abortWithMessage(String msg);
}
