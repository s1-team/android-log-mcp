# android-log-mcp

> Project goals: [docs/GOALS.md](docs/GOALS.md) · Contributor / agent guidelines: [AGENTS.md](AGENTS.md)

**English:** An MCP (Model Context Protocol) stdio server that gives AI coding agents (Claude Code, Codex CLI, Cursor, …) **read-only** access to Android logcat. It records every connected device in the background, searches with Android Studio's Logcat filter syntax (with strict error messages), groups Java/native crashes and ANRs (with DropBox traces/tombstones), and reconstructs app launch timelines (COLD/WARM/HOT, TTID). Run it with `npx -y android-log-mcp`; requires `adb`. Prebuilt binaries for Linux/macOS/Windows (x64, arm64). Docs below are in Chinese.

---

一个 MCP stdio 服务器，作用是让 AI agent（Claude Code / Codex / Cursor 等）读取 Android logcat。它是 **只读** 的：不清空日志，不启动或停止应用，也不改动设备。

- **后台持续录制**：每台已连接设备都会被自动录制，默认最多 4 台。logcat 会落盘，所以进程重启或换会话后，历史日志仍然可以查。
- **过滤语法**：沿用 Android Studio Logcat 的语法，例如 `tag:` `message~:` `-package:` `level:` `age:` `is:crash` `|` `&` 和括号。解析出错时会给出列号和修正提示，不会像 AS 那样静默退化成子串匹配。响应第一行会回显"过滤器被解析成了什么"。
- **崩溃与 ANR 整理**：自动识别 Java 崩溃、native 崩溃（tombstone）和 ANR，并按签名分组。ANR 会尽量从 `dumpsys dropbox` 补上 traces。
- **冷/温/热启动时间线**：依据 `START u0`、`am_proc_start`、`am_on_*_called`（API ≤ 29）/ `wm_on_*_called`（API ≥ 30）以及 `Displayed` / `Fully drawn` 整理。

## 快速开始（npx）

前提：Node.js ≥ 16（只用来下载和启动），adb（Android SDK platform-tools）。不需要 Go。各版本的验证情况见「兼容性」。

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

Windows 上如果客户端找不到 `npx`，把 command 改成 `"cmd"`，args 改成 `["/c", "npx", "-y", "android-log-mcp"]`。如果想固定版本，用 `android-log-mcp@0.3.0`。

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

### 兼容性探测（`compat`）

```bash
android-log-mcp compat [--serial S] [--json | --raw]
```

只读地检查设备是否具备本工具依赖的 logcat / ps / pm / dumpsys 功能，并用录制时的解析器评估结果。退出码：0 = 支持（可能降级），1 = 缺少必需功能，2 = 用法或 adb 错误。详见「兼容性」。

### 诊断包（`report`）

需要把问题反馈给维护者时，用 `report` 子命令把诊断数据打成一个 zip（**默认打码敏感信息**）：

```bash
npx -y android-log-mcp report                                   # 所有已录制设备，最近 2 小时
npx -y android-log-mcp report --serial emulator-5556 --package com.foo \
    --since 30m --note "点登录按钮后闪退" -o foo-crash.zip
npx -y android-log-mcp report --from "2026-10-10 14:00" --to "2026-10-10 14:30" --note @repro.txt
npx -y android-log-mcp report --package com.foo --am-start com.foo/.MainActivity   # 会启动 App，见下文
```

| 参数 | 含义 |
|---|---|
| `--serial S` | 只打包这台设备（可重复）；默认所有有录制数据的设备 |
| `--since 2h` / `--from` / `--to` | 时间窗口（设备本地时间，语法同工具的 `from`/`to`）。默认最近 2 小时；`--to` 缺省表示到最新一条 |
| `--package P` | 只保留该 App 的日志（按包名 / 进程名），以及正文中提到该包名的系统日志（`START u0`、`am_proc_start`、`Displayed` 等），并只保留它的崩溃、进程和 DropBox 条目 |
| `--max-size 50MB` | 日志数据（未压缩）上限，超出时从最旧的开始丢弃；丢弃数量写进 `manifest.json`，并在包内的 `meta.json` 里记成一个 "report size cap" 缺口 |
| `--note TEXT` / `--note @FILE` | 复现步骤或问题说明，存为 `note.txt`（同样会打码） |
| `--am-start PKG/Activity` | 执行 `am start -W -n` 并记录输出（对照数据）。**这会启动 App**，所以只有显式指定时才执行；执行后最多等 8 秒让录制进程把启动日志落盘 |
| `--redact-extra REGEX` | 追加打码规则（RE2，可重复；有分组时只替换第 1 组），类型记为 `custom` |
| `--no-redact` | 关闭打码（manifest 里会有警告） |
| `--keep-private-ip` | 不打码内网地址（10/8、172.16/12、192.168/16、链路本地、ULA） |
| `--keep-domain D` | 不打码域名 `D` 及其子域名（可重复），例如自家 API 的域名；值不写入 manifest |
| `--no-adb` | 不使用 adb：不采集实时设备信息和 DropBox（0.4 前叫 `--offline`） |
| `-o FILE` | 输出路径，默认 `./android-log-mcp-report-<时间>.zip`（路径打印到 stdout，摘要打印到 stderr） |

`report` 只读取数据目录，不会改动它，也不会开始录制；除 `--am-start` 外不会改变设备状态（只执行 `getprop`、`date`、`cat /proc/sys/kernel/random/boot_id`、`dumpsys dropbox --print`）。

zip 内容：

| 路径 | 内容 |
|---|---|
| `manifest.json` | 工具版本、生成时间、参数（`--note` / `--redact-extra` / `--keep-domain` 的值不写入）、每台设备的窗口和记录数、各文件大小、裁剪情况、各打码规则的命中次数和不同值个数、警告 |
| `note.txt` | `--note` 的内容 |
| `store/v1/<device-id>/` | 本工具自己的存储格式（`seg-*.jsonl`、`meta.json`、`crashes.jsonl`、`procs.jsonl`、`packages.json`），已按窗口/包名裁剪、序号重排为 1..N。可直接只读加载：`android-log-mcp --offline <解压目录>/store`（作为 MCP 服务器，或配合 `call` 调用工具） |
| `devices/<serial>/device.json` | `ro.build.fingerprint`、型号、厂商、Android 版本、SDK、时区和 `date +%z` 偏移、boot_id、设备与主机的时钟差（设备离线时取录制时保存的信息，并注明来源） |
| `devices/<serial>/logcat/<boot>.txt` | 同一批记录按 `logcat -v threadtime -v epoch -v uid -v usec -v printable` 格式重新输出，每次启动（boot）一个文件，可以直接用回放测试的 `device.Pipeline.ReplayText` 加载 |
| `devices/<serial>/dropbox/<tag>.txt` | 窗口内的 DropBox 条目（`data_app_crash`、`data_app_native_crash`、`data_app_anr`、`SYSTEM_TOMBSTONE` 及 `system_app_*`），格式同 `dumpsys dropbox --print`，可用 `Pipeline.ApplyDropbox` 加载 |
| `devices/<serial>/am-start.txt` | `--am-start` 的输出 |

```bash
unzip foo-crash.zip -d r
android-log-mcp --offline r/store call logcat_crashes '{"package":"com.foo"}'
```

#### 隐私提示

默认会把下列内容替换成 `[REDACTED:<类型>#n]`：同一个原值在整个包里（日志、崩溃、DropBox、note）对应同一个编号，方便关联；编号由每次随机生成、用完即丢的 HMAC 密钥决定，包里不保存原值，也无法反推。

| 类型 | 规则 |
|---|---|
| `token` | `Authorization:` / `Proxy-Authorization:` 头、`Bearer` / `Basic` 后面的值 |
| `jwt` | `eyJ….eyJ….…` 形式的 JWT |
| `secret` | 常见密钥前缀：`ghp_` `gho_` `ghu_` `ghs_` `ghr_` `github_pat_` `npm_` `sk-`（含 `sk-proj-` 等） `AKIA` / `ASIA` `AIza` `xox?-` `glpat-`、PEM 私钥头 |
| `credential` | 键名含 api_key / secret / token / password / passwd / access_key / private_key / credential 的 `key=value`、`key: value`、JSON 字段，以及 URL query 里的同名参数 |
| `url_param` | URL query 里的 `sig` `signature` `code` `key` `auth` `ticket` `sid` `session` `otp` `nonce` 以及 AWS / GCS 签名参数 |
| `password` | URL 里的 `scheme://user:password@host` |
| `cookie` | `Cookie:` / `Set-Cookie:` 的值 |
| `email` | 邮箱地址 |
| `phone` | 中国大陆 11 位手机号（含 `+86`），以及带 `+` 的国际号码 |
| `id_card` | 18 位居民身份证号（校验出生日期和 GB 11643 校验位） |
| `bank_card` | 13–19 位银行卡号（Luhn 校验 + 卡组织号段；不带分隔符的纯数字串还要求 15/16/19 位、键名不像计数器） |
| `device_id` | IMEI、IMSI、ICCID、MEID、序列号、Android ID、广告 ID（`key=value` 形式） |
| `ipv4` / `ipv6` | IP 地址（不含 127.0.0.1、::1、0.0.0.0）；`--keep-private-ip` 保留内网地址 |
| `mac` | MAC 地址（不含全 0 / 广播地址） |
| `domain` | 域名 / 主机名，见下文；`--keep-domain` 保留指定域名 |

`domain` 规则分两种情况：

- **URL 里的主机**（`https://`、`http://`、`wss://`、`ftp://`、`postgres://` 等任意网络 scheme）：只替换主机部分，scheme、`user:` 、端口、路径和 query 保持原样，比如 `GET https://api.foo.com:8443/v1/login?page=2` → `GET https://[REDACTED:domain#1]:8443/v1/login?page=2`。这里不限顶级域名。IP 字面量仍归 `ipv4` / `ipv6`；`content://`、`file://`、`android.resource://`、`intent:` 等非网络 scheme 不处理；`app://com.foo.app:10108` 这类以包名作 authority 的也不处理。与 `password`、`credential`、`url_param` 可同时生效：`postgres://bob:[REDACTED:password#1]@[REDACTED:domain#1]:5432/app`。
- **不在 URL 里的裸主机名**（`Unable to resolve host "cdn.foo.com"`、`failed to connect to www.foo.com/1.2.3.4`、`apn: epc.foo.com`、`sub.example.cn`）：只认小写、至少两段、最后一段在一份精选的公共顶级域名列表里（com、net、org、io、co、ai、app、dev、cn、uk、jp、de 等常见通用和国家顶级域名），并且第一段不能是 `com` / `org` / `android` / `java` / `ro` / `persist` 等反向域名 / 属性前缀。所以包名和类名（`com.example.logtest`、`android.app.ActivityThread`、`java.lang.RuntimeException`）、Java 堆栈帧、文件名（`Foo.java`、`libc.so`、`base.apk`、`*.odex`、`*.xml`、`*.json`）、版本号、IP、系统属性（`ro.build.fingerprint`、`persist.sys.*`）、intent action（`android.intent.action.MAIN`）都不会被误判。

取舍：裸主机名用的是精选列表，不是完整的 IANA 列表。也是常见文件扩展名或单词的顶级域名（`.so` `.md` `.py` `.sh` `.rs` `.id` `.in` `.it` `.is` `.to` `.top` `.site` `.online` `.store` `.cloud` 等）不在列表里，所以 `cdn.foo.so` 这种主机名只有出现在 URL 里时才会被打码。邮箱整体归 `email`，不会再单独拆出域名。

**不会打码**的：时间戳、pid、tid、uid、级别、tag、包名、进程名、设备属性（fingerprint 等），以及正文里的包名 / 类名 / 文件名 / 属性名（见上面的 `domain` 规则）。为了不误伤日志结构，规则会排除：epoch 时间戳和 ns 计数、十六进制值和地址（`0x…`、寄存器 / 内存转储列）、native 堆栈帧（`#01 pc …`）、Java 堆栈帧、版本号（`versionName=1.2.3.4`、`Chrome/83.0.41.10`、`Radio: 1.0.0.0`）、HIDL 库名（`android.hardware.foo@1.0.so`）、`schedstat` 等数字列表，以及键名是 time / size / bytes / count / duration / …Ms 之类的数字。这些都有单元测试，并在 API 29 的真实日志上回归。

**打码是基于模式的，不能保证完全**：自定义格式的 token、姓名、地址、不带键名的随机串等识别不了。分享前请先检查 zip 内容；需要时用 `--redact-extra` 追加规则。`--no-redact` 生成的包可能含有 token、密码和个人信息，只应发给可信的人。

## Upgrading to 0.4.0 (storage layout; issues #5, #7)

> **Breaking:** 0.4.0 uses a new data layout. **Old data is neither migrated nor read.** Logs recorded by 0.3 or earlier are not visible in 0.4, and 0.3 report zips cannot be loaded with `--offline`.

- Data now lives in `<user cache dir>/android-log-mcp/v1/<device-id>/` (Linux: `~/.cache/android-log-mcp/v1/…`). The device ID is stable: `ro.serialno` for a physical device (the same over USB and Wi-Fi), `avd-<name>` for an emulator.
- The old directories `~/.cache/android-log-mcp/<serial>/` (for example `emulator-5554/`) are ignored, not deleted. Delete them yourself when you no longer need them, together with any manual backups such as `~/.cache/android-log-mcp.v1-backup/`.
- `--offline <dir>` is now the only way to open a dataset read-only, for example an unzipped report: `android-log-mcp --offline r/store`. `--home r/store --offline` no longer works. The report flag that skips adb is now `--no-adb`.
- `ANDROID_LOG_MCP_HOME` / `--home` relocates the data root and records normally. A device that another process records (any root) is not recorded twice; it is read from that process's data directory.

简要说明（中文）：0.4.0 换了新的数据目录结构（`v1/<设备ID>/`），**旧数据不会迁移，也不会被读取**；旧的 `~/.cache/android-log-mcp/<serial>/` 目录会被忽略（不删除），不需要的话请手动删除。只读打开数据集（例如解压后的诊断包）只用 `--offline <目录>`；`report` 不用 adb 的开关改名为 `--no-adb`。`ANDROID_LOG_MCP_HOME` / `--home` 只是把数据根目录换个位置，照常录制；同一台设备在同一用户下只由一个进程录制。

## 升级到 0.3.0

> **默认日志保留期从 7 天改成了 1 天。** 升级后第一次启动时，超过 1 天的日志分段，以及超过 1 天的崩溃、DropBox 和已结束进程记录，会被**删除**。
> 想保持原来的行为，请在 MCP 客户端配置里设置 `ANDROID_LOG_MCP_MAX_AGE=7d`（Codex 写在 `env = { ANDROID_LOG_MCP_MAX_AGE = "7d" }`，其他客户端写在 `"env": {"ANDROID_LOG_MCP_MAX_AGE": "7d"}`）。

0.3.0 的其他变化：
- **内存上限**：最近日志缓存默认 20000 条或 16 MiB，历史缓存 32 MiB（按块 LRU）。空闲 2 分钟后释放缓存；只读进程按需加载。
- **磁盘保护**：辅助文件定期压缩；数据目录总量上限 2 GiB；剩余空间低于 512 MiB 时暂停录制，恢复后自动续录。
- 新增的环境变量见下表。

## 环境变量与数据目录

| 变量 | 含义 |
|---|---|
| `ANDROID_LOG_MCP_HOME` | Relocates the data root (also `--home`; default `<user cache dir>/android-log-mcp`, Linux `~/.cache/android-log-mcp`). The process records normally into it; owner locks stay in the fixed per-user `<user cache dir>/android-log-mcp/owners/`, so a device recorded by another process (any root) is only read. To open a dataset read-only use `--offline <dir>`. 数据根目录换位置，照常录制 |
| `ANDROID_LOG_MCP_ADB` | adb 的路径 |
| `ANDROID_LOG_MCP_BINARY` | （仅 npm 启动器使用）不用平台包，改用这个路径下的二进制 |
| `ANDROID_LOG_MCP_MAX_DEVICES` | 最多同时录制几台设备，默认 4 |
| `ANDROID_LOG_MCP_MEM_RECORDS` | 每台设备在内存里缓存的最近日志条数，默认 20000。更早的日志查询时从磁盘读 |
| `ANDROID_LOG_MCP_MEM_MB` | 最近日志缓存的字节上限（估算值，单位 MiB），默认 16。超长行不会把内存撑大 |
| `ANDROID_LOG_MCP_HISTORY_CACHE_MB` | 历史查询时，已解码日志块缓存的字节上限（每台设备，单位 MiB），默认 32。按块（约 512 KiB 的分段内容）做 LRU 淘汰 |
| `ANDROID_LOG_MCP_IDLE_RELEASE` | 多久没有查询就释放缓存，默认 `2m`；设为 `0` 表示不释放。释放时历史缓存清空；只读进程连最近日志缓存一起释放，下次查询再按需加载 |
| `ANDROID_LOG_MCP_MAX_DEVICE_MB` | 每台设备的日志上限（MiB），默认 512 |
| `ANDROID_LOG_MCP_MAX_AGE` | 日志保留多久，默认 `1d`。接受 Go 时长（`72h`、`90m`）或天数（`1d`、`7d`、`1.5d`）。无效值或非正值会在 stderr 打一条警告并使用默认值。崩溃、`dropbox_seen` 和已结束进程这些辅助记录也按这个保留期清理。`logcat_status` 会显示当前生效的值 |
| `ANDROID_LOG_MCP_MAX_TOTAL_MB` | 整个数据目录的上限（MiB），默认 2048；设为 `0` 表示不限制。Counts everything the tool manages under the root: all devices and format versions, `tmp/`, `run/`, `owners/` (directories of older versions are ignored and not counted) |
| `ANDROID_LOG_MCP_MIN_FREE_MB` | 数据目录所在磁盘的剩余空间低于这个值（MiB）时暂停录制，默认 512；恢复到 2 倍以上时自动续录。设为 `0` 表示关闭 |

### Data directory layout (0.4+)

```
<root>/v1/<device-id>/           device data, format 1 (other vN/ formats are swept like unowned data)
<root>/run/                      runtime state (state files whose pid is dead or socket is undialable, and orphan sockets, are removed)
<root>/tmp/                      scratch files, removed after 1 hour
<cache>/android-log-mcp/owners/<device-id>.lock
                                 owner lock + JSON {pid, version, data_dir, addr, home, boot_id, since};
                                 fixed per user (os.UserCacheDir), whatever the root
```

`<root>` is `<cache>/android-log-mcp` unless `ANDROID_LOG_MCP_HOME` / `--home` relocates it. Anything else under it, such as pre-0.4 `<serial>/` directories or `legacy/`, is ignored: not read, not counted, not deleted.

**Device ID** (`device.ResolveID`, probed with one read-only `adb shell` that also reads the boot id):
1. Emulator (`ro.kernel.qemu=1`, `ro.boot.qemu=1` or serial `emulator-N`): `avd-<name>` from `ro.boot.qemu.avd_name` (API 30+) or `adb emu avd name` (older images). `ro.serialno` is identical on many AVDs and console ports are reused, so neither is used.
2. Physical device: `ro.serialno` (or `ro.boot.serialno`), unless empty, `unknown` or a placeholder such as `0123456789ABCDEF` / all one character.
3. Otherwise `fp-` + the first 12 hex digits of sha256(manufacturer|model|android_id); without a readable android_id, sha256(manufacturer|model|fingerprint|adb serial), which then changes with the transport.
4. Collision: if another device that is connected right now already uses the ID with a different boot id (same AVD started twice with `-read-only`, duplicated serials), this one is filed as `<id>~<adb serial>` (e.g. `avd-api29~emulator-5556`) and `logcat_status` says why. Two connections with the same boot id (USB + Wi-Fi adb) are one device. Trade-off: the suffixed directory follows the adb serial, so that instance's history continues only while it keeps the address; when the first instance is gone, the next instance of that AVD uses the plain ID again (one history per AVD, which is what the ID stands for). Anything instance-specific (boot id, emulator pid) would split the history on every reboot instead.

**Startup sweep and periodic maintenance** (every minute; never in `--offline`): unowned device directories of every format version lose segments older than `ANDROID_LOG_MCP_MAX_AGE` (including their newest one) and are removed once nothing in them is newer; then the total cap removes the oldest segments across everything (each current-format device keeps its newest segment). Directories written by a live process (its owner lock or `writer.lock` is held) are left to that process. Only known file names are deleted from device directories.

每台设备的数据放在一个子目录里（0.4 起为 `v1/<设备ID>/`），内容包括：

- `seg-*.jsonl`：日志分段，每段 16 MiB
- `meta.json`：会话（boot_id）、缺口、时区等
- `crashes.jsonl`、`procs.jsonl`、`packages.json`

保留策略：
- 每台设备总量不超过 512 MiB，且只保留最近 1 天（`ANDROID_LOG_MCP_MAX_AGE`），超出后按段删除最旧的。
  - 一个分段要等到它最新的一行也超过保留期才会被删除。
  - 正在写入的分段最多跨越保留期的 1/4（日志很少的设备会提前换段），所以日志最长保留约 1.25 倍保留期。
  - 录制进程每分钟检查一次。
- 整个数据目录（包括其他格式版本、tmp、run、owners；0.4 以前的旧目录不计入、不处理）不超过 2 GiB。超出时跨设备按段删除最旧的，当前格式下每台设备的最新一段保留；只清理本进程负责录制的设备，或者当前没有进程在录制的设备。
- 没人录制的设备目录（`v<N>/` 下）也按保留期删除，整个目录过期后删掉。
- 辅助文件定期压缩（启动约 30 秒后一次，之后每 10 分钟一次）：
  - `procs.jsonl`、`crashes.jsonl` 每个条目只保留最新版本；
  - 早于保留期的已结束进程、崩溃和 `dropbox_seen` 记录会被删除；早于保留期的 DropBox 条目也不再导入；
  - 遗留的 `*.tmp` 文件也会清掉。
- `--offline <目录>`（只读浏览数据集，例如解压后的诊断包）不录制、不清理、不创建任何文件。
- 磁盘剩余空间低于 512 MiB 时暂停录制（`logcat_status` 会显示原因），并记一个缺口；剩余空间恢复到 1 GiB 以上后用 `logcat -T` 自动续录。

内存：
- 每台设备在内存里只缓存最近 20000 条日志（同时不超过约 16 MiB），更早的日志查询时从磁盘按块读取，并放进一个 32 MiB 的 LRU 缓存。
- 2 分钟没有查询就释放历史缓存。
- 只读进程（另一个进程在录制）启动时不预读日志，第一次查询时才加载，空闲后再释放。
- `logcat_status` 的 `memory:` 一行显示当前的缓存占用。

**Several clients at once:** each device is recorded by exactly one process per user, decided by `owners/<device-id>.lock` in the fixed per-user owners directory (shared by all roots). Other processes, whatever their root, read the owner's data directory, and `logcat_status` names the owner (pid, version, relocated root, data directory). When the owner exits, another process takes over within about 10 seconds and records into its own root.

**多个客户端同时运行**：同一用户下每台设备只有一个进程负责录制（由固定的每用户目录 `<cache>/android-log-mcp/owners/<设备ID>.lock` 决定，与数据根目录无关），其他进程不论根目录在哪都只读跟随录制者的数据目录，`logcat_status` 会写明录制者的 pid、版本和数据目录。录制进程退出后，其他进程约 10 秒内接管，录到自己的根目录。

## 已知限制

- **没有独立 daemon**：录制随 MCP 进程存在，所有 MCP 客户端都关闭后录制就停止。下次启动时会用 `logcat -T <上次时间>` 续录并去重。如果设备的 ring buffer 在此期间已经被覆盖，丢失的部分会被记为缺口，并在 `status` 和搜索结果里标出。`Tools.Call(name, rawArgs)` 是预留的接口，以后可以换成"薄代理 + 常驻 daemon"（计划见 issue #3）。在那之前，每个 MCP 客户端都会有一个进程；不负责录制的进程空闲时只占十几 MiB（见上面的「内存」）。
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

## 兼容性

分两层：**已验证** 是真的跑过的组合；**设计支持、未验证** 是代码按 AOSP 行为做了处理（多数有单元测试），但还没在对应环境上运行过。

### 已验证

| 方面 | 已验证的组合 | 怎么验证的 |
|---|---|---|
| Node / npx | Node 20（本机 `npx -y android-log-mcp`）；Node 16.20 本机跑启动器和 MCP 握手；CI 每次提交在 ubuntu 上跑 Node 16 / 18 / 20 / 22 / 当前 LTS / current，在 macOS、Windows 上跑 Node 22 | CI 的 `npx-compat` job：安装打包好的 npm tarball，执行 `npx android-log-mcp --version`，再通过 stdio 做 MCP 握手（initialize → tools/list → logcat_status，`--offline <空目录>`）（`npm/test/mcp_smoke.js`） |
| 主机 OS / 架构 | linux x64：完整运行（后台录制、端到端验证）。linux x64、macOS（arm64）、Windows x64：单元测试和回归测试（`go test ./...`），以及上面的 npx 冒烟测试 | `.github/workflows/ci.yml` |
| Android | API 29（Android 10，x86_64 模拟器，TCG 软件模拟）：端到端验证 + `compat` 探测 13 项全部通过 | `validation/mcp-v2/`、`validation/report-e2e/`、`validation/compat/api29-emulator-5556/` |
| adb | platform-tools 37.0.1（adb 1.0.41），`track-devices` 正常 | 同上 |

### 设计支持、尚未验证

| 方面 | 设计范围 | 说明 |
|---|---|---|
| Node | `engines: node >=16`（启动器只用 `child_process.spawn` 和 `require.resolve`） | Node 14 及以下没测过，不支持 |
| 主机 OS / 架构 | npm 平台包：linux / darwin / win32 × x64 / arm64 | darwin x64、linux arm64、win32 arm64 的二进制只是交叉编译出来，从没运行过；macOS 和 Windows 上从没接过真机或模拟器录制（Windows 的文件锁是尽力而为，见「已知限制」） |
| Android API | **下限 API 24（Android 7.0）**：录制依赖 logcat 的 `-v uid` / `-v epoch` / `-v usec` / `-v printable` 和 `crash` buffer | 只在 API 29 上实际运行过。API 26 以下没有 `ps -A -o` 时用旧版 `ps`；没有 `pm list packages -U` 时用 `cmd package list packages -U`，再不行就只按进程名归属。这些分支有单元测试，没有实机验证 |
| API 30 – 35 | 生命周期事件 `wm_on_*_called`（带耗时）、`am_crash` 字段顺序 `[user,pid,…]`、`logcat --uid` | 单元测试覆盖了两种事件格式和两种字段顺序（`server/internal/analysis/apilevels_test.go`、`server/internal/compat`）。本机没有 KVM，跑不了 API 30+ 的模拟器，所以没有实测 |
| adb | 支持 `track-devices` 的 platform-tools | 不支持时每 2 秒轮询 `adb devices`，未实测 |

各 API 版本的差异和本工具的处理：

| 差异 | API 29 | API 30+ | 处理 |
|---|---|---|---|
| `logcat --uid` | 没有 | 较新版本有 | 不使用：所有过滤都在主机端完成 |
| 多个 `--pid` | 只认一个 | — | 不使用 |
| Activity 生命周期事件 | `am_on_*_called [user,class,reason]`，没有耗时 | `wm_on_*_called [token,class,reason,耗时]` | 两种都解析；没有耗时时启动时间线会注明 |
| `am_crash` 字段顺序 | 实际输出 `[pid,user,…]`（设备上的 event-log-tags 却声明为 `[user,pid,…]`） | AOSP 声明为 `[user,pid,…]` | 按数值判断哪个是 pid，两种顺序都有测试 |
| `Displayed` 的 tag | `ActivityTaskManager` | `ActivityTaskManager` | 也接受旧版的 `ActivityManager` |

想在别的设备或 API 版本上确认，可以运行兼容性探测（只读：`getprop`、`logcat -d/-g`、`ps`、`pm list`、`dumpsys dropbox` 和帮助输出，不清日志、不启动 App）：

```bash
android-log-mcp compat --serial <SERIAL>        # 或 npx -y android-log-mcp compat（v0.2.1 起）
scripts/compat-probe.sh <SERIAL> out/            # 从源码构建并把结果写进 out/
```

它会用录制时同样的解析器检查每一项，输出 `SUPPORTED` / `SUPPORTED (degraded)` / `NOT SUPPORTED`。欢迎把结果发回来，方法见 `validation/README.md`。

## 已验证与未验证

- **已验证**：
  - `go test ./...` 全部通过，其中移植了 Android Studio 的过滤器解析器测试。
  - 回归测试会把 `server/internal/testdata/api29/`（API 29 模拟器的真实输出）里的 logcat 和 DropBox 数据，经过和线上相同的 ingest pipeline（`device.Pipeline`）回放，再调用 MCP 工具断言输出（`server/internal/mcpserver/replay_test.go`）。
  - `report`：每条打码规则都有正反例单元测试（`server/internal/redact`），并在 API 29 全部真实日志上确认时间、pid、tid、级别、tag 不变；回放测试会把 fixture 打成包，再分别按数据目录和原始 logcat + DropBox 文本加载回来，确认崩溃和启动时间线和原始录制一致（`server/internal/mcpserver/report_replay_test.go`）。在 emulator-5556 上用 `scripts/e2e-report.sh` 注入 21 条假密钥（15 种类型）和 4 条结构反例，打包后全部打码、日志头完整、离线加载正常，记录在 `validation/report-e2e/`。
  - 在 emulator-5556（Android 10 / API 29，TCG）上做了端到端验证：冷启动、HOT 启动、onNewIntent、Java 崩溃、SIGSEGV、SIGABRT、`:remote` 进程及其崩溃、广播 ANR、`log -t` 标记，以及非 ASCII 字符的处理。输出保存在 `validation/mcp-v2/`。
- **未验证**：
  - Android 11 及以上的 `wm_*` 事件格式：只有单元测试。
  - WARM 启动：v1 和 v2 实测都没能构造出来。
  - adb 不支持 `track-devices` 时退化为轮询。
  - 多设备同时录制。
  - 在 Windows 和 macOS 上连设备录制（CI 只跑单元测试和 npx 冒烟测试，见「兼容性」）。

## 仓库结构

| 目录 | 内容 |
|---|---|
| `server/` | Go 模块 `github.com/s1crab/android-log-mcp/server`，包括源码、单元测试和回归测试 fixture（`internal/testdata/api29`） |
| `npm/` | npm 启动器和包模板。用 `scripts/build-npm.sh [版本] [--pack]` 交叉编译并组装所有包 |
| `scripts/` | `build-npm.sh`（打 npm 包）、`e2e-report.sh`（在真机 / 模拟器上验证 `report` 的打码）、`compat-probe.sh`（设备兼容性探测） |
| `validation/` | 在 API 29 模拟器上的实测记录：结论、触发脚本、测试 App 源码、MCP 工具的实际输出（v1 在 `mcp/`，v2 在 `mcp-v2/`） |
| `docs/` | 项目目标（`GOALS.md`）、调研和设计文档 |
| `AGENTS.md` | 给 agent 和贡献者的工作约定（流程、发版、版本规则、安全规则） |
| `.github/workflows/` | CI（测试）和 release（打 `v*` tag 时发布到 npm） |

## 发布（维护者）

1. 修改 `npm/VERSION`（以及 `server/internal/mcpserver/server.go` 里的默认 `Version`），然后打 tag 并 push：`git tag v0.3.0 && git push origin v0.3.0`。
2. `release.yml` 会依次跑测试、交叉编译、`npm publish --provenance`。先发 6 个平台包，再发主包。需要仓库里配置 secret `NPM_TOKEN`（npm 的 Automation 或 Granular token）。

本地验证：`scripts/build-npm.sh 0.2.0 --pack`，生成的 tarball 在 `npm/dist/tarballs/`。

## 第三方参考

`server/third_party_ref/` 里是 AOSP 和 Android Studio 的源码片段（过滤器语法、logcat 格式、DropBox），均为 Apache-2.0 许可，只作为移植和对照的参考，不参与编译。

## License

MIT © s1crab。`server/third_party_ref/` 里是 Apache-2.0 授权的参考源码，不参与编译。
