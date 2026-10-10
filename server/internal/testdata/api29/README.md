# 测试 fixtures（真实设备输出，未加工）

设备：Android 模拟器 `emulator-5556`，Android 10（API 29），`google_apis/x86_64`，**TCG 软件模拟（无 KVM）**，时区 Asia/Kuching (+08:00)。
测试 App：`com.example.logtest`（uid 10146），源码在 `/workspace/logtest-app`。

（原位于 `validation/fixtures/api29/`，现为测试数据唯一副本。）

所有 `*.logcat.txt` / `session*-raw.log` 的采集命令完全一致：

    adb -s emulator-5556 logcat -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable -T 1

（`-T 1`：从当前末尾开始流式读取。每个场景文件 = 触发前 2 秒开始，到触发后 N 秒结束；对应的触发命令与 `am start -W` 输出在 `validation/api29/<场景>.cmd.txt`。）

格式要点（解析器必须处理）：
- 行首有右对齐的空格：`         1791572202.589379 10146  6713  6713 V LogTest : ...`
- uid 列：≤5 字符的用户名显示名字（`root`、`shell`），其余为数字（`1000`、`10146`）。
- 多行消息被拆成多行，每行头部（时间到微秒、uid、pid、tid、级别、tag）完全相同。
- 多 buffer 合并输出时时间戳 **不单调**（例：auditd 行比前后行晚/早数秒）。
- `--------- beginning of <buffer>` 分隔行。
- `-v printable` 把非可打印字节转成八进制转义：`Updating clock: 3\356\270\20108`。
- 制表符原样保留（不转义）。

| 文件 | 内容 |
|---|---|
| coldstart1.logcat.txt | 冷启动（am start -W 超时；Displayed 在采集窗口之外，见 session-raw.log） |
| coldstart.logcat.txt | 冷启动完整：START u0 → am_proc_start/Start proc → App 日志（含 W+堆栈+Caused by、多行 E）→ am_on_create/start/resume_called → am_activity_launch_time + Displayed |
| coldstart_after_reboot.logcat.txt | 重启后冷启动（am start -W 超时，但日志完整） |
| hotstart.logcat.txt | HOT 启动：am_on_restart/start/resume_called，**没有 Displayed** |
| warmstart_FAILED_was_onNewIntent.logcat.txt | 想做 WARM 失败（BACK 未生效，变成 onNewIntent 到顶部实例），LaunchState UNKNOWN (0) |
| java_crash.logcat.txt | Java 崩溃（RuntimeException 包 IllegalStateException）：FATAL EXCEPTION 块、am_crash、Force finishing、am_proc_died |
| crash-buffer-dump.txt | 崩溃后 `logcat -b crash -d`（同格式） |
| native_crash.logcat.txt | SIGSEGV：libc Fatal signal（App pid）+ crash_dump64/DEBUG tombstone（**另一个 pid，但 uid=App uid 10146**）+ tombstoned + am_crash("Native crash") + SYSTEM_TOMBSTONE 拷贝到 DropBox |
| native_abort.logcat.txt | abort()：Abort message、多帧 backtrace |
| anr_broadcast.logcat.txt | 前台广播超时 ANR：BroadcastQueue Timeout、am_anr、"Dumping to /data/anr/…"、"ANR in" 块（含 CPU usage） |
| anr_input_NO_ANR.logcat.txt | 主线程阻塞 30s + input 事件，**未**触发 ANR（TCG 下 input 命令本身就慢，事件到达时已解除阻塞） |
| remote.logcat.txt | 启动 `:remote` 进程：am_proc_start/Start proc `com.example.logtest:remote` + 该进程日志 |
| remote_crash.logcat.txt | `:remote` 进程崩溃两次（服务被重启后再崩） |
| dropbox-data_app_crash.txt | `dumpsys dropbox --print data_app_crash`（shell 身份） |
| dropbox-data_app_native_crash.txt | `dumpsys dropbox --print data_app_native_crash` |
| dropbox-SYSTEM_TOMBSTONE.txt | `dumpsys dropbox --print SYSTEM_TOMBSTONE`（完整 tombstone，含全部线程） |
| dropbox-data_app_anr.txt | `dumpsys dropbox --print data_app_anr`：**traces 内联**（主线程在 TriggerReceiver.onReceive → SystemClock.sleep） |
| session-raw.log | 02:55–03:10 全程连续采集（以上 coldstart1…remote_crash 都在其中） |
| session2-raw.log | 第一次模拟器重启后 03:15–03:16 |
| session3-raw.log | 第二次重启后 03:21–03:27（coldstart_after_reboot/hotstart/native_abort 都在其中） |
