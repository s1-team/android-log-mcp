# v2 端到端验证输出（emulator-5556，API 29，TCG）

录制进程：`bash -c 'sleep infinity | server/android-log-mcp'`。部署时按原方式重启；中途为修复又重启过 3 次，最后一次是 pid 145433。
v1 的存储备份在 `~/.cache/android-log-mcp.v1-backup`；v2 第 1、2 次试跑的存储放在 `/tmp/android-log-mcp.v2-try*`。
触发命令及 `am start -W` 的输出见 `NN_*.cmd.txt`，脚本是 `../scripts/e2e-v2*.sh`、`../scripts/calls-v2*.sh`。

| 文件 | 内容 |
|---|---|
| status_start / crashes_start | 新存储首次接入：回放积压日志并读取 DropBox 历史。SIGABRT 只有一条（v2-try1 曾经出现 DropBox 和 logcat 各一条的重复，已修复） |
| launches.txt | COLD 10840 ms（与 am start -W 一致）/ HOT（onRestart，没有 Displayed）/ 两次 onNewIntent，标为 UNKNOWN（am start -W 也是 UNKNOWN） |
| launches2.txt | 第二次 COLD：TTID 14153 ms（与 am start -W 一致），Displayed 在 +15050 ms 才被记录 |
| crashes_all*.txt, crash_detail_*.txt | Java 崩溃：logcat、am_crash、DropBox 合并成一条；SIGSEGV / SIGABRT 各一条，没有 "Native crash" 的 java 条目；SIGABRT 不再显示 `--------`，位置是 `#02 liblogtest.so`；`:remote` 两次崩溃归为一组，和 "repo not ready" 分开 |
| crashes_native2.txt | 修复后的 native 崩溃：5 个来源（logcat、tombstone、am_crash、DropBox SYSTEM_TOMBSTONE、data_app_native_crash）合并成一条 |
| crashes_anr*.txt | 广播 ANR：am_anr、ANR in 块、DropBox traces（主线程停在 TriggerReceiver.onReceive → SystemClock.sleep） |
| status_pkg*.txt | 进程表：没有 `<pre-initialized>`、没有 pid reused、没有 crash_dump64，死亡原因准确（exited (signal 6/11)、killed: crash、died） |
| search_mark / search_printable | `log -t` 打出的行显示为 `?(uid shell)`；`café ✓ 中文` 能按原文匹配和显示 |
| search_tags*.txt | 过滤器回显带括号：`(tag == "LogTest" OR tag == "MyRepo") AND level >= INFO AND time in [...]` |
| search_debug_pkg.txt | DEBUG 行的进程显示为 `crash_dump`，但按包过滤时仍能查到 |
| reader_latency.txt | 另一个进程作为跟随读者，在 `adb shell log` 命令返回之前就拿到了这一行 |
| status_redeploy.txt | 设备时钟比主机慢 1 分 44 秒，状态里显示出来；不再误报 backlog |
| try1_*.txt | 第一次部署发现的两个问题，修复后加了回归测试：① 首个 ps 快照抢在积压中的 am_proc_died 之前，把 3418 误判为 "gone (not in ps)"；② DropBox 比 logcat 先读到，导致 SIGABRT 出现两条 |
| 08–10_*.cmd.txt | 这一轮失败：App "crashed too many times" 后被系统标成 bad process，后台广播全部被丢弃（`process is bad`）。先做一次前台启动（13_*），再用 14–16 重做 |
