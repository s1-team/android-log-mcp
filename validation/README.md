# Android 模拟器测试床与 logcat MCP 实机验证（2026-10-10，Asia/Shanghai）

> 所有时长都是 **TCG 软件模拟** 下测得，只能说明“流程/格式”，不能当作性能数据。

## 0. 结论速览

- **KVM 不可用，且碰了会出事**：本机是 Firecracker 类 microVM，`/dev/kvm` 能打开、`KVM_CREATE_VM` 成功，但 `KVM_CREATE_VCPU` 触发宿主（box）内核 `kernel BUG at arch/x86/kvm/x86.c:702`（`kvm_spurious_fault` ← `vmx_vcpu_create`）。证据：`sudo dmesg`。**以后一律不要使用 /dev/kvm**（`-accel off`）。
  - 之前为了访问 /dev/kvm 做的改动：新建了 gid 103 的 `kvm` 组，把 box 加了进去，并 `chmod 666 /dev/kvm`。
- 模拟器改用 **TCG + 多线程**（`-accel off -qemu -accel tcg,thread=multi`）。不加 `thread=multi` 时只有 1 个核在跑，非常慢。
- **API 35**：单线程 TCG 用 1047 秒才 boot_completed，随后约 1.5 分钟 system_server 就挂了（DeadSystemException；同类问题在 API 29 上查到的原因是 Watchdog）。改用 MTTCG 后约 8 分钟仍未启动完成，为了把 CPU 让给 API 29，我把它关了。**API 35 没有拿到任何有效验证数据。**
- **API 29**（Android 10，`google_apis/x86_64`）：MTTCG 首次冷启动 430 秒，之后的冷启动约 190–200 秒。
  - 刚启动完时负载很重，system_server 会被 Watchdog 杀掉（`*** WATCHDOG KILLING SYSTEM PROCESS: Blocked in handler on ui thread`），然后自动重启。
  - 用 root 执行 `pm disable-user` 停用了 GMS、Chrome 等 17 个 Google 应用，之后就稳定了。做完后已经 `adb unroot`，**所有验证都以 shell 身份执行**。
- **快照**：`adb emu avd snapshot save ready` 能成功（约 4–6 秒，ram.bin 1.5 GB）。但两次用 `-snapshot ready` 恢复都失败了，报错 `Length mismatch: : 0x8e8000 in != 0x10000` 和 `0x2000 in != 0x10000`（`error while loading state ... device 'ram'`），模拟器随后退回冷启动。**TCG 下快照不可用。** 所以“恢复后旧日志是否还在、时钟是否跳变”**没有验证到**。证据：`api29/snapshot-*.txt`。

## 1. 环境

| 项目 | 值 |
|---|---|
| JDK | Temurin 17.0.20.1，位于 `~/jdk/current`。Debian 13 的 apt 源里没有 openjdk-17 |
| SDK | `~/android-sdk`：cmdline-tools(latest)、platform-tools 37.0.1（adb 1.0.41）、emulator 37.2.12、build-tools 35.0.0、platforms;android-35、ndk 27.3.13750724，以及 system-images android-35 和 android-29 的 google_apis/x86_64 |
| 环境变量 | 已写入 `~/.bashrc`：`JAVA_HOME`、`ANDROID_HOME`、`ANDROID_SDK_ROOT`、`PATH` |
| AVD | `api29`（pixel_3，2 GB 内存，4 核）、`api35`（pixel_6，2 GB 内存，6 核） |
| 其他 apt 包 | gdb、strace、zip |

### 启动和停止模拟器

```bash
~/bin/emu-start.sh api29 5556    # 后台无窗口启动（setsid nohup），等到 boot_completed 且 pm 可用；日志写到 ~/emulator-api29.log
~/bin/emu-stop.sh emulator-5556  # 等价于 adb -s emulator-5556 emu kill
```

`emu-start.sh` 实际执行的命令：

```bash
emulator -avd api29 -port 5556 -no-window -no-audio -no-boot-anim -gpu swiftshader_indirect \
  -accel off -no-snapshot-save -no-metrics -feature -BluetoothEmulation -qemu -accel tcg,thread=multi
```

- 快照恢复的命令是在上面基础上加 `-snapshot ready`，但在 TCG 下会失败，见第 0 节。
- 重启后建议执行：`adb shell svc power stayon true; adb shell wm dismiss-keyguard`。
- 当前状态：**emulator-5556（api29）正在运行**，测试 App 已安装。
- 后台还在运行两个进程：一个常驻录制的 `android-log-mcp` 进程（`sleep infinity | android-log-mcp`，pid 79104），以及 `scripts/track.py`（把 track-devices 写进 `api29/track-devices.txt`）。

## 2. 测试 App（`/workspace/logtest-app`，不用 Gradle：javac → d8 → aapt2 → zip → zipalign → apksigner，见 `build.sh`）

- 包名 `com.example.logtest`，uid 10146。
- 组件：MainActivity（主进程）、RemoteService（`:remote` 进程）、导出的 TriggerReceiver。
- native 部分是 `liblogtest.so`（NDK r27d）。

| 场景 | 触发命令 | 结果 |
|---|---|---|
| a 启动日志 | `am start -W -n com.example.logtest/.MainActivity` | onCreate 中打 V/D/I/W/E，tag 为 LogTest、MyRepo、OkHttp；W 带完整堆栈和 Caused by；E 是多行消息，含制表符 |
| b Java 崩溃 | `... --es what crash_caused`（或 `crash`） | FATAL EXCEPTION，带 Caused by ✅ |
| c native 崩溃 | `--es what native`（SIGSEGV 空指针）/ `native_abort`（abort 并带 abort message） | tombstone ✅ |
| d ANR | `am broadcast --receiver-foreground -n com.example.logtest/.TriggerReceiver --es what anr_broadcast` | 前台广播 10 秒超时 → ANR ✅ |
| d' input ANR | `--es what anr`，然后用 input 发事件 | ❌ TCG 下没能触发 |
| e :remote 进程 | `am broadcast -n com.example.logtest/.TriggerReceiver --es what remote`（`remote_crash` 让 :remote 崩溃） | ✅ |

注意：`Log.w(tag, msg, UnknownHostException)` **不会打印堆栈**（Log.getStackTraceString 对 UnknownHostException 特殊处理，返回空串）。所以 App 改用了 IOException 包一层。design-v3 输出示例里 UnknownHostException 带堆栈的写法，在真机上不会出现。

## 3. 验证项（API 29，shell 身份）

原始输出：`api29/probes.txt`；fixtures 见 `../server/internal/testdata/api29/README.md`（测试数据唯一副本）；MCP 工具输出在 `mcp/`。

| # | 项目 | 结果 | 证据 |
|---|---|---|---|
| 1 | `-b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable` 的格式 | ✅ 格式为 `<空格右对齐>sssss.uuuuuu UID PID TID L TAG : msg`；多行消息每行头部完全相同；不同 buffer 交错输出时时间戳不单调 | fixtures/api29/*.logcat.txt |
| 2 | 以 shell 身份运行时 `-v uid` 是否给出真实 uid | ✅ 是真实 uid。App 显示 `10146`；system 显示 `1000`；`root` 和 `shell` 显示成名字。**tombstone 的 DEBUG 行来自 crash_dump64（pid 不同），但 uid 是 App 的 10146**，可以靠 uid 归属到 App | native_crash.logcat.txt 第 152 行起 |
| 3 | `logcat --uid` | ❌ **API 29 不支持**：`Unrecognized Option`，帮助里也没有这个选项 | probes.txt |
| 4 | `--pid` | 只能给单个 pid。写两次 `--pid=a --pid=b` 时**只有最后一个生效**；`--pid=a,b` 报 `pid 7815,8076 out of range` | probes.txt |
| 5 | `ps -A -o PID,UID,NAME` | ✅ 可用，多进程显示为 `com.example.logtest:remote`。启动瞬间可能抓到 `<pre-initialized>` 这个名字 | probes.txt, ps-after-remote-crash.txt |
| 6 | `pm list packages -U` | ✅ 输出 `package:com.example.logtest uid:10146`；`cmd package list packages -U --uid 10146` 也可用 | probes.txt |
| 7 | 冷启动（events 标签） | ⚠️ **API 29 上没有 `wm_on_*_called`**，对应事件是 `am_on_create_called` / `am_on_start_called` / `am_on_resume_called` / `am_on_restart_called` / `am_on_paused_called`，格式 `[0,<class>,<reason>]`，**不带耗时字段**。`am_activity_launch_time`（不是 `wm_activity_launch_time`）、`ActivityTaskManager: Displayed …: +18s692ms`（不带 " for user"）、`am_proc_start`、`Start proc` 都存在 | coldstart.logcat.txt |
| 7a | TCG 下的冷启动耗时 | START 51.854 → am_proc_start +3.72s → am_on_create +13.47s → am_on_resume **+13.68s**；Displayed +18692ms，等于 `am start -W` 的 TotalTime 18692 / WaitTime 18845，LaunchState COLD。重启后那次：START → onResume +17.52s，Displayed +22707ms（`am start -W` 超时） | api29/coldstart*.cmd.txt |
| 7b | HOT 启动 | am_on_restart → am_on_start → am_on_resume（+0.97s），**没有 Displayed，也没有 am_activity_launch_time**；`am start -W` 报 HOT，TotalTime 2542 | hotstart.* |
| 7c | Displayed 行的写入时机 | Displayed 往往比 onResume 晚十几秒才写进日志（TCG），所以短采集窗口会漏掉它 | coldstart1 |
| 8 | Java 崩溃 | ✅ 在 crash buffer 中：`FATAL EXCEPTION: main` / `Process: …, PID:` / 堆栈 / Caused by，所有行时间戳相同。events 中 `am_crash` 在 API 29 上的格式是 **`[pid, userId, process, flags, 异常类, msg, file, line]`**，异常类取的是 **root cause**（IllegalStateException），不是外层异常 | java_crash.logcat.txt |
| 9 | native 崩溃 | ✅ `F libc: Fatal signal 11 (SIGSEGV)…`（App pid）；`F DEBUG` 整段 tombstone（crash_dump64 的 pid，App 的 uid）；`tombstoned: Tombstone written to /data/tombstones/tombstone_00`；`am_crash [<system_server pid>,0,com.example.logtest,…,Native crash,Segmentation fault,unknown,0]`（**pid 字段是 system_server 的 pid**）；DEBUG 行在 crash buffer 中（已核对）。SIGSEGV 的 backtrace 在 TCG 下只有 1 帧，abort 有 3 帧 | native_crash / native_abort / crash-buffer-native-check.txt |
| 10 | dropbox 中的崩溃 | ✅ shell 可读 `data_app_crash`（完整 Java 堆栈）和 `data_app_native_crash`（tombstone 头部和 backtrace）。另外还有 **`SYSTEM_TOMBSTONE`**（compressed text，98 KB，含全部线程） | dropbox-*.txt |
| 11 | ANR | ✅ `am_anr [0,7815,com.example.logtest,flags,reason]`，`ANR in com.example.logtest` 块（含 PID、Reason、Load、CPU usage），以及 `Dumping to /data/anr/anr_…`。**`dumpsys dropbox --print data_app_anr` 内联了完整 traces**（shell 可读，25 KB 压缩），主线程栈可以直接读出。`/data/anr` 能列目录，但文件权限是 600 system，读不了；`/data/tombstones` 报 Permission denied；`dumpsys activity anr` 不是合法命令；`logcat -L` 报 `logcat read failure` | anr_broadcast.*, dropbox-data_app_anr.txt |
| 12 | 时间和时区 | `date +%z` 输出 `+0800`，`getprop persist.sys.timezone` 输出 **Asia/Kuching**（模拟器自动选的，不是 Asia/Shanghai，但偏移相同）。/data/anr 下早期文件名用的是 UTC 时间，后期是本地时间 | probes.txt |
| 13 | boot_id | ✅ `cat /proc/sys/kernel/random/boot_id` 可读，每次冷启动都会变：07a6d2d3… → bf71fe22… → ccb368e7… | pre-snapshot*.txt, snapshot-restore.txt |
| 14 | `adb track-devices` | ✅ 推送格式是 4 位十六进制长度加 `serial\tstate\n`；列表为空时推送 `0000`。重启过程中依次看到 device → offline → 0000 → offline → device | api29/track-devices.txt |
| 15 | 模拟器重启后重连 | ✅ 见第 4 节，MCP 会开新 session 并记录一个 gap | mcp/status_after_reboot.txt |
| 16 | 用 `log -t TAG msg` 打标记 | ✅ shell 可用，uid 显示为 `shell`；`-p e` 可以指定级别 | probes.txt |
| 17 | 环形缓冲区大小 | 模拟器上 main、system、crash 各 2 MiB（`logcat -g`），**不是**设计文档里说的 256 KB | probes.txt |
| 18 | `-v printable` | 非可打印的 UTF-8 字节会变成八进制转义（`\356\270\201`）；制表符不转义 | session3-raw.log |

## 4. android-log-mcp v1 实测（`android-log-mcp call …`，设备 emulator-5556）

**结果正确的部分：**
- package 归属：包括 `com.example.logtest:remote` 在内都正确，`package` 参数会匹配该包的所有进程（`mcp/search_tags.txt`、`search_pkgparam.txt`）。
- 多行消息合并，Caused by 链完整。
- `level:debug`、`min_level`、from/to 窗口、`level:E`（扩展写法）都正常。
- 未知 key 会报错并给出列号和建议（`filter_err.txt`）。
- `view:"summary"` 正常（`summary.txt`）。
- `after` + `wait_ms`：等待期间用 `log -t` 打的标记能被及时返回（`wait_after.txt`）。
- Java 崩溃详情：完整堆栈，加上崩溃前 40 行，来源同时有 logcat 和 dropbox（`crash_detail_cr18.txt`）。
- native 崩溃：tombstone 被正确归到 pid 7736，并附带崩溃前的日志（`crashes_native.txt`）。
- **ANR 完整**：reason、CPU 信息、来自 dropbox 的主线程 trace，以及 ANR 前的日志（`crashes_anr.txt`）。
- 重启检测：boot_id 变化后开了新 session，并标出 gap（`03:10:33–03:11:41, device rebooted`）。

**发现的问题（按严重程度排序，没有改代码）：**

1. **logcat_app_launches 在 API 29 上拿不到 onCreate/onStart/onResume。**
   - 原因是只认 `wm_on_*_called`，而 Android 10 上的事件名是 `am_on_*_called`，并且没有耗时字段。
   - 后果：每次都显示 `MainActivity.onCreate not found`；HOT 启动（只有 am_on_restart/start/resume，没有 Displayed）被判成 **UNKNOWN**，`am start -W` 给的是 HOT。
   - 证据：`launches2.txt`、`launches3.txt`。
2. **logcat_crashes 把 API 29 的 am_crash 解析错了，导致重复条目。**
   - 字段顺序是 `[pid,user,…]`，工具把 pid 解析成了 0，所以没能和 FATAL EXCEPTION 合并，多出 `cr19`、`cr23` 这类 “pid 0 … only am_crash event recorded” 的条目。
   - native 崩溃对应的 `am_crash(Native crash)` 被当成 **java** 类型的崩溃另列一条（cr21、cr27）。它的 pid 字段其实是 system_server 的 pid。
   - 分组签名也错了：cr23（"LogTest crash in :remote"）被标成 “same signature also at 03:01:32 (cr19)”，而 cr19 的消息是 "repo not ready"。
   - 证据：`crashes_java.txt`、`crashes_all.txt`、`crashes_after_reboot.txt`。
3. **logcat_status 的进程列表有脏数据。**
   - 已被 force-stop 的 pid 6713 仍以 `<pre-initialized>` 的名字显示为 running（来自 ps 种子）。
   - 出现了错误的 “pid reused” 条目（`pid 7182 (?–02:59:15, pid reused)`）。
   - crash_dump64（pid 7763，App uid）被列为该包正在运行的进程，还有一个凭空出现的 pid 7765。
   - 重启前仍存活的 7815、8076 在重启后既不在 running 里，也不在 recently dead 里。
   - 证据：`status_pkg*.txt`、`status_after_reboot.txt`。
4. 第一次 `call logcat_status`（读者进程，写者是另一个进程）时，显示的历史只到 02:56:57，而设备时间已经是 03:00:31，**滞后约 3.5 分钟**。半分钟后再调用就追上了。原因不明，只出现过一次（`status_pkg.txt` 对比 `status2.txt`）。
5. 过滤器回显没有括号：`tag == "LogTest" OR tag == "MyRepo" AND time in [...]`，读起来像 `A OR (B AND time)`。实际返回的结果是对的（`search_tags.txt`）。
6. `-v printable` 产生的八进制转义没有还原，原样输出，导致含非 ASCII 字符的消息无法按原文匹配（`printable_escape.txt`）。
7. 小问题：
   - SIGABRT 显示 `fault addr --------`，崩溃位置写的是 libc 帧（syscall+24），而不是首个 App 帧（liblogtest.so）。
   - tombstone 的 DEBUG 行进程名显示为 `com.example.logtest(?)`，实际是 crash_dump64 打的。
   - `log -t` 打出的行进程名显示为 `?`。
   - 启动时间线里 “+34938 ms Displayed (TTID) / system reports +22707 ms” 两个数字并列，容易让人误解。

## 5. 与 design-v3 / research-v2 假设不一致的地方

1. `wm_on_*_called` 和 `wm_activity_launch_time` 在 API 29 上不存在，需要兼容 `am_on_*_called` 和 `am_activity_launch_time`，并且旧版本没有回调耗时字段。HOT 启动没有 Displayed，冷/温/热判定要改为以 `am_on_restart_called` 等事件为依据。
2. `am_crash` 的字段顺序随版本变化（API 29：pid 在前）；native 崩溃也会产生一条 `am_crash`，其 pid 字段是 system_server 的 pid。
3. `logcat --uid` 在 API 29 上不存在；`--pid` 只支持单个。
4. 仅用 pid 无法把 tombstone 的 DEBUG 行归到 App，但这些行的 **uid 就是 App 的 uid**，可以用来归属。
5. 环形缓冲区的默认大小因设备而异（模拟器上 2 MiB）。
6. dropbox 里还有 `SYSTEM_TOMBSTONE`，是完整 tombstone，可以作为 native 崩溃的补充来源。
7. `Log.x(…, UnknownHostException)` 没有堆栈（design 示例里有）。
8. 时区可能不是用户预期的 IANA 名字（这次是 Asia/Kuching），只有偏移是可靠的。

## 6. 未完成或未知

- API 35 的所有项目，包括 `-v uid`、`--uid`、wm_* 事件和 Android 14+ 的格式，都没有验证。
- 快照恢复（TCG 下失败）、恢复后的时钟跳变和旧日志是否保留。
- input 类型的 ANR，以及 WARM 启动。
- `logcat -L` 失败，/data/anr 读不了，这两条与 design 的预期一致。

## report 子命令（v0.2.0）

`android-log-mcp report` 的打码端到端验证在 `report-e2e/`（`scripts/e2e-report.sh`）：在 emulator-5556 上注入 16 种假密钥（含域名），打包后全部打码，日志头完整，报告可以离线加载。
