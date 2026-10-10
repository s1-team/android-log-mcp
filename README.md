# android-log-mcp

**English:** An MCP (Model Context Protocol) stdio server that gives AI coding agents (Claude Code, Codex CLI, Cursor, …) **read-only** access to Android logcat. It records every connected device in the background, searches with Android Studio's Logcat filter syntax (with strict error messages), groups Java/native crashes and ANRs (with DropBox traces/tombstones), and reconstructs app launch timelines (COLD/WARM/HOT, TTID). Run it with `npx -y android-log-mcp`; requires `adb`. Prebuilt binaries for Linux/macOS/Windows (x64, arm64). Docs below are in Chinese.

---

一个 MCP stdio 服务器，作用是让 AI agent（Claude Code / Codex / Cursor 等）读取 Android logcat。它是 **只读** 的：不清空日志，不启动或停止应用，也不改动设备。

- **后台持续录制**：每台已连接设备都会被自动录制，默认最多 4 台。logcat 会落盘，所以进程重启或换会话后，历史日志仍然可以查。
- **过滤语法**：沿用 Android Studio Logcat 的语法，例如 `tag:` `message~:` `-package:` `level:` `age:` `is:crash` `|` `&` 和括号。解析出错时会给出列号和修正提示，不会像 AS 那样静默退化成子串匹配。响应第一行会回显"过滤器被解析成了什么"。
- **崩溃与 ANR 整理**：自动识别 Java 崩溃、native 崩溃（tombstone）和 ANR，并按签名分组。ANR 会尽量从 `dumpsys dropbox` 补上 traces。
- **冷/温/热启动时间线**：依据 `START u0`、`am_proc_start`、`am_on_*_called`（API ≤ 29）/ `wm_on_*_called`（API ≥ 30）以及 `Displayed` / `Fully drawn` 整理。

## 快速开始（npx）

前提：Node.js ≥ 16（只用来下载和启动），adb（Android SDK platform-tools）。不需要 Go。

```bash
npx -y android-log-mcp            # 以 MCP stdio 服务器方式运行（由 MCP 客户端启动，不要手动交互）
npx -y android-log-mcp --version
npx -y android-log-mcp call logcat_status '{}'   # 调试：录制几秒后调用一次工具
```

npm 包本身只有一个很小的 Node 启动器，真正的程序是预编译好的 Go 二进制，按平台放在 `android-log-mcp-<os>-<cpu>` 可选依赖里，支持 linux/darwin/win32 × x64/arm64。启动器直接继承 stdio，不往 stdout 写任何东西。如果平台包没装上（例如用了 `--omit=optional`），会给出提示；也可以设置 `ANDROID_LOG_MCP_BINARY` 指向自己编译的二进制。

adb 的查找顺序：环境变量 `ANDROID_LOG_MCP_ADB` → `PATH` → `$ANDROID_HOME` / `$ANDROID_SDK_ROOT` 下的 `platform-tools` → 常见的 SDK 安装路径（macOS `~/Library/Android/sdk`、Linux `~/Android/Sdk`、Windows `%LOCALAPPDATA%\Android\Sdk`）。

## 配置 MCP 客户端

### Claude Code

```bash
claude mcp add android-log -- npx -y android-log-mcp
```

或者写进项目的 `.mcp.json`：

```json
{
  "mcpServers": {
    "android-log": { "command": "npx", "args": ["-y", "android-log-mcp"] }
  }
}
```

### Codex CLI（`~/.codex/config.toml`）

```toml
[mcp_servers.android-log]
command = "npx"
args = ["-y", "android-log-mcp"]
# env = { ANDROID_LOG_MCP_ADB = "/path/to/adb" }
```

### Cursor（`~/.cursor/mcp.json` 或项目内的 `.cursor/mcp.json`）

```json
{
  "mcpServers": {
    "android-log": { "command": "npx", "args": ["-y", "android-log-mcp"] }
  }
}
```

Windows 上如果客户端找不到 `npx`，把 command 改成 `"cmd"`，args 改成 `["/c", "npx", "-y", "android-log-mcp"]`。如果想固定版本，用 `android-log-mcp@0.1.0`。

### 从源码构建

需要 Go 1.24 及以上：

```bash
cd server && go build -o android-log-mcp ./cmd/android-log-mcp
```

然后在上面的配置里把 command 换成二进制的绝对路径，args 留空即可。

## 工具

| 工具 | 用途 | 主要参数 |
|---|---|---|
| `logcat_status` | 列出设备、录制状态、历史范围和缺口（gap），并返回一个"现在"游标；传 `package` 时还会显示安装状态和运行中的进程 | `device` `package` |
| `logcat_search` | 搜索日志。`view="lines"` 返回日志行（默认）；`view="summary"` 返回按级别、tag、进程的统计，以及重复消息、崩溃数和最新错误 | `filter` `package` `tags` `min_level` `from` `to` `limit` `context` `before` `after` `wait_ms` |
| `logcat_crashes` | 列出崩溃和 ANR（按签名分组）。只匹配到一条，或传了 `id` 时，返回完整详情：堆栈或 tombstone、崩溃前该 pid 的最后 40 行，ANR 还会附上主线程 trace 和持锁线程 | `package` `kinds`(java/native/anr) `from` `to` `id` `limit` |
| `logcat_app_launches` | 启动耗时时间线（COLD/WARM/HOT/UNKNOWN），默认看最近 30 分钟。每行 `+N ms` 是该日志相对启动请求的记录时刻；TTID / TTFD 单独一行，并注明来源（`Displayed` 或 `*_activity_launch_time`）和它被记录的时刻 | `package` `from` `to` `limit` |

- **时间参数**：`from` / `to` 可以写相对时间（如 `30m`、`2h`），也可以写设备本地时间（如 `14:05`、`2026-10-10 14:05:00`），还支持 ISO 和 epoch 格式。所有输出时间都是 **设备本地时区**，响应里会注明是哪个时区。
- **复现流程**：先调用 `logcat_status` 拿到 `cursor`，让用户复现问题，再调用 `logcat_search(after=cursor, wait_ms=20000)`。

过滤语法速查：

```
tag:OkHttp                 包含（大小写不敏感，AS 语义）
tag=:MainActivity          精确匹配
message~:"timeout \d+ms"   正则（RE2 语法，不支持回溯引用和环视，报错时会提示）
-tag:chatty                排除
level:warn  age:5m  is:crash  is:stacktrace
pid:1234  tid:1234  buffer:crash          （本项目扩展）
tag:A | tag:B   (tag:A | level:error) & package:com.foo
```

同一个 key 写多次默认是 OR 关系，例如 `tag:A tag:B` 等价于 A 或 B；不同 key 之间是 AND。不认识的 key、`&&` / `||`、未闭合的括号、未加引号的 `foo:bar` 都会直接报错。

### 调试子命令（`call`）

```bash
# 先录制 5 秒（可用 ANDROID_LOG_MCP_CALL_WAIT=20s 调整），然后调用一次工具并打印结果
npx -y android-log-mcp call logcat_search '{"filter":"level:error","limit":20}'
```

## 环境变量与数据目录

| 变量 | 含义 |
|---|---|
| `ANDROID_LOG_MCP_HOME` | 数据目录，也可以用 `--home` 指定。默认是 `<用户缓存目录>/android-log-mcp`，Linux 上即 `~/.cache/android-log-mcp` |
| `ANDROID_LOG_MCP_ADB` | adb 的路径 |
| `ANDROID_LOG_MCP_BINARY` | （仅 npm 启动器使用）不用平台包，改用这个路径下的二进制 |
| `ANDROID_LOG_MCP_MAX_DEVICES` | 最多同时录制几台设备，默认 4 |

每台设备的数据放在一个子目录里，内容包括：

- `seg-*.jsonl`：日志分段，每段 16 MiB
- `meta.json`：会话（boot_id）、缺口、时区等
- `crashes.jsonl`、`procs.jsonl`、`packages.json`

保留策略：每台设备总量不超过 512 MiB，且不超过 7 天，超出后按段删除最旧的。

**多个客户端同时运行**：同一台设备只有一个进程负责录制（通过 `writer.lock` 文件锁决定），其他进程跟随读取同一批文件。录制进程退出后，其他进程会自动接管录制。

## 已知限制

- **没有独立 daemon**：录制随 MCP 进程存在，所有 MCP 客户端都关闭后录制就停止。下次启动时会用 `logcat -T <上次时间>` 续录并去重。如果设备的 ring buffer 在此期间已经被覆盖，丢失的部分会被记为缺口，并在 `status` 和搜索结果里标出。`Tools.Call(name, rawArgs)` 是预留的接口，以后可以换成"薄代理 + 常驻 daemon"。
- **设备 ring buffer 很小**：模拟器上每个 buffer 默认只有 2 MiB（`logcat -g`），繁忙时几分钟就会被覆盖。录制开始之前的日志能拿到多少取决于它；更早的崩溃只能从 DropBox 补（`logcat_crashes` 会注明来源）。
- **不使用设备端过滤**：API 29 的 logcat 没有 `--uid`，`--pid` 也只认一个，所以本工具总是录制全部 buffer（main、system、crash、events），按包、pid 和 uid 的过滤都在主机端完成。
- **读取延迟（刷新策略）**：录制进程每收到一批日志就写入并 flush（不等缓冲区满），另有约 800 ms 的延迟队列用来等 pid→进程映射。所以跟随读取的进程（另一个 MCP 客户端）通常在约 1 秒内就能看到新日志。v1 实测时出现过读者落后约 3.5 分钟的情况：根因是设备端积压，即 TCG 软件模拟下 logcat 首次 `-D` 导出整个 buffer 很慢；另外旧版本的写入缓冲区要攒满才落盘，也会加剧延迟。现在 `logcat_status` 会区分三种情况并明确说明：录制进程停写、设备端积压（最新日志落后于设备时钟，但仍在持续写入）、设备本身安静。
- **设备时钟漂移**：TCG 软件模拟的模拟器，设备时钟可能比主机慢一分多钟（v2 实测：同一时刻相差 −104 s）。所以录制进程每 30 秒会重新测一次设备时钟（`date +%s.%N`），跟随读取的进程用这个最新的测量值。"现在"、相对时间（如 `30m`）和延迟判断都以这个估计为准。偏差超过 5 秒时，`logcat_status` 会显示出来。v1 看到的"读者落后 3.5 分钟"，很可能主要就是这个漂移：当时设备时间只在启动时测过一次。
- **时区**：以设备上 `date +%z` 的偏移为准。`persist.sys.timezone` 只在偏移一致时才用作显示名（例如模拟器报 Asia/Kuching，但主机是 Asia/Shanghai，两者都是 +08:00）；偏移不一致时按 `UTC±hhmm` 显示并给出说明。所有输出都会标注偏移。
- **`-v printable`**：logcat 会把非 ASCII 字节转义成 `\ooo`（新版本是 `\xHH`）。本工具在入库前把转义还原成 UTF-8，搜索、匹配和显示都用原文；无效的 UTF-8 序列保留转义写法。旧版本存下的记录在读取时也会还原。`\b` 这类 C 转义保持原样。
- **合并多行消息**：消息头（时间到微秒、pid、tid、级别、tag、buffer）完全相同的连续行会合并成一条多行消息。如果两条不同消息的这些字段恰好完全一样，也会被合并。
- **logcat 输出本身不严格按时间排序**：多个 buffer 合并时可能有几秒的乱序。搜索结果按录制顺序（seq）展示；启动分析会先按时间排序再处理。
- **启动时间线**：
  - API 29 只有 `am_on_*_called [0,class,reason]`，没有回调耗时；API 30 及以上是 `wm_on_*_called`，带耗时。两种都支持。onX 的时间是回调返回的时刻；Application.onCreate 没有对应的系统事件。
  - COLD/WARM/HOT 是推断的：有 `am_proc_start` 为 COLD；没有新进程但有 onCreate 为 WARM；只有 onRestart/onStart/onResume 为 HOT。HOT 启动不会有 `Displayed` 行。intent 被直接投递给已在前台的实例（onPause→onNewIntent→onResume）时标为 UNKNOWN，这和 `am start -W` 的结果一致。
  - TTID 以 `Displayed` 为准，没有时用 `*_activity_launch_time`。`Displayed` 在慢设备上可能比 onResume 晚十几秒以上才记录，所以向后最多看 180 秒，不依赖日志顺序。
- **崩溃**：
  - API 29 的 `am_crash` 字段顺序是 `[pid,user,process,…]`，新版本是 `[user,pid,…]`，两种都能解析。
  - native 崩溃对应的 `am_crash("Native crash")` 带的是 system_server 的 pid，只会并入同进程的 native 条目，不会单独列出。
  - tombstone 的 DEBUG 行来自 crash_dump64 进程（uid 和 App 相同），按 uid 归到 App 包下，但进程名标为 `crash_dump`。
  - 崩溃位置取第一个 App 帧（`/data/` 下的库），没有时取 #00。
  - 系统判定 App "crashed too many times" 后会把它标成 bad process，此后后台广播都会被直接丢弃（`process is bad`），直到有一次前台启动。这是系统行为，本工具只会如实显示。
  - 首次接入时会读 DropBox 里的历史（data_app_crash、data_app_native_crash、SYSTEM_TOMBSTONE、data_app_anr），先读到的 DropBox 条目和后来回放到的 logcat 证据会合并成一条。
- **ANR traces 和 native 崩溃的完整 tombstone**：依赖 `dumpsys dropbox`。DropBox 可能限流，或者 shell 没有读取权限。拿不到时，结果会说明原因，并建议你自己运行 `adb bugreport`。读取 `/data/anr` 和 `/data/tombstones` 需要 root，本工具不会去读。
- **进程表**：
  - 进程生命周期来自 `am_proc_start`、`am_proc_died`、`am_kill`、`has died`、zygote 的 `exited due to signal`、force-stop，以及每 30 秒一次的 `ps -A -o PID,UID,NAME` 快照。
  - 快照中刚 fork 的进程名可能是 `<pre-initialized>`：这种条目不会被当作新进程，等拿到真名后再更新。
  - 快照中如果出现一个和某个存活进程同名、但没有对应 `am_proc_start` 的新 pid，就不当作 App 进程。这种 pid 通常是 debuggerd 在 native 崩溃时 fork 出来的短命子进程。
  - 一份完整快照里没有出现的存活条目会被关闭，原因标为 `gone (not in ps)`。如果日志流还在回放积压（落后快照超过 30 秒），快照不会用来关闭条目，以免抢在积压中的死亡事件之前。
  - boot_id 变化时，上一次启动中仍存活的进程会以 `device rebooted` 关闭，并在 `logcat_status` 的 "before the last reboot" 中列出。
  - `log -t` 等 shell 命令打出的行，进程短命且查不到名字，会显示为 `?(uid shell)`。
- **存储格式**：JSONL 没有压缩。Windows 上的文件锁只是尽力而为（O_EXCL 锁文件加过期判断），未在 Windows 上测试。
- **设备时钟估算**：设备与主机的时钟偏移用 `date +%s` 估算，精度约为 1 秒加一次 adb 往返的时间，只用于"现在"和相对时间的换算。日志时间戳本身直接来自设备。

## 已验证与未验证

- **已验证**：
  - `go test ./...` 全部通过，其中移植了 Android Studio 的过滤器解析器测试。
  - 回归测试会把 `server/internal/testdata/api29/`（API 29 模拟器的真实输出）里的 logcat 和 DropBox 数据，经过和线上相同的 ingest pipeline（`device.Pipeline`）回放，再调用 MCP 工具断言输出（`server/internal/mcpserver/replay_test.go`）。
  - 在 emulator-5556（Android 10 / API 29，TCG）上做了端到端验证：冷启动、HOT 启动、onNewIntent、Java 崩溃、SIGSEGV、SIGABRT、`:remote` 进程及其崩溃、广播 ANR、`log -t` 标记，以及非 ASCII 字符的处理。输出保存在 `validation/mcp-v2/`。
- **未验证**：
  - Android 11 及以上的 `wm_*` 事件格式：只有单元测试。
  - WARM 启动：v1 和 v2 实测都没能构造出来。
  - adb 不支持 `track-devices` 时退化为轮询。
  - 多设备同时录制。
  - Windows 和 macOS。

## 仓库结构

| 目录 | 内容 |
|---|---|
| `server/` | Go 模块 `github.com/s1crab/android-log-mcp/server`，包括源码、单元测试和回归测试 fixture（`internal/testdata/api29`） |
| `npm/` | npm 启动器和包模板。用 `scripts/build-npm.sh [版本] [--pack]` 交叉编译并组装所有包 |
| `validation/` | 在 API 29 模拟器上的实测记录：结论、触发脚本、测试 App 源码、MCP 工具的实际输出（v1 在 `mcp/`，v2 在 `mcp-v2/`） |
| `docs/` | 调研和设计文档 |
| `.github/workflows/` | CI（测试）和 release（打 `v*` tag 时发布到 npm） |

## 发布（维护者）

1. 修改 `npm/VERSION`，然后打 tag 并 push：`git tag v0.1.0 && git push origin v0.1.0`。
2. `release.yml` 会依次跑测试、交叉编译、`npm publish --provenance`。先发 6 个平台包，再发主包。需要仓库里配置 secret `NPM_TOKEN`（npm 的 Automation 或 Granular token）。

本地验证：`scripts/build-npm.sh 0.1.0 --pack`，生成的 tarball 在 `npm/dist/tarballs/`。

## 第三方参考

`server/third_party_ref/` 里是 AOSP 和 Android Studio 的源码片段（过滤器语法、logcat 格式、DropBox），均为 Apache-2.0 许可，只作为移植和对照的参考，不参与编译。

## License

MIT © s1crab。`server/third_party_ref/` 里是 Apache-2.0 授权的参考源码，不参与编译。
