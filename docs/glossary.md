# Glossary: processes and roles

Several kinds of process are involved when android-log-mcp runs: our own, the Android SDK's and the device's. This page fixes the words for them. Issues, pull requests, docs, code comments, logs and tool output use these terms (see [AGENTS.md](../AGENTS.md)). Decided in #33.

## Terms

| Term | 中文 | Meaning |
|---|---|---|
| **MCP process** | MCP 进程 | The `android-log-mcp` that an MCP host starts per session. It speaks MCP over stdio, records nothing and forwards every tool call to the service process (starting it when needed). |
| **service process** (short: **service**) | 后台服务进程（简称服务） | `android-log-mcp service`: one per user. It does all recording, file writes and cleanup (sweep, retention, `set-root`, `report`). |
| **CLI process** | CLI 进程 | A command run by hand: `call`, `record`, `report`, `root`, `set-root`, `service status`, `install`, … |
| **adb server** | adb server | The Android SDK's resident adb process (port 5037), shared with Android Studio and other tools. We use it and never kill it. |
| **adb child** | adb 子进程 | An adb client process that the service process starts (`adb logcat`, `adb shell`, `adb track-devices`, …). These end with the device or the service process (#24). |
| **device process** | 设备进程 | A process on the phone or emulator, e.g. the app being debugged or logd. Log lines, crashes and the process table are about device processes. |
| **MCP host** | MCP 宿主 | The agent tool that starts MCP processes from its configuration: Codex, Cursor, Claude Code, … |

Related words:

- **MCP stdio server**: the product category (what android-log-mcp is, and what an MCP host's configuration calls a "server"). A running instance is an MCP process.
- **android-log-mcp process**: any of our own processes when the role does not matter (MCP, service or CLI process), e.g. "a `writer.lock` held by another android-log-mcp process".
- **app process**: a device process of a given app (`com.foo`, `com.foo:remote`).

## Rules

- Never write a bare "process", "server", "daemon", "client" or "服务": qualify it with one of the terms above. In English, "service" alone is the accepted short form of "service process" once the context is clear.
- Do not say "MCP client" for the agent tool: it is the **MCP host**. (In the MCP specification the host contains MCP clients; we never need that distinction.)
- Do not call the MCP process a "thin client", "proxy" or "server" in prose; say what it does ("forwards to the service process").
- "daemon" is only used when quoting older documents (research-v2 called the recorder a daemon); today that role is the service process.

## Clocks and times (#48)

Two clocks are involved: the Android device's and the host's (the machine that runs the MCP, service and CLI processes). They can differ by any amount, and the device clock can be changed at any moment (auto_time, a manual set, expiry testing).

| Term | 中文 | Meaning |
|---|---|---|
| **device time** | 设备时间 | A time on the Android device's clock: the timestamp of a logcat line, and every time-valued tool parameter (`from`, `to`, `age:`). |
| **host time** | 主机时间 | A time on the host's clock: file rotation, retention, compaction, "last seen", pins, the clock-jump `host=` field. Never used to select log lines. |
| **device now** | 设备当前时间 | The device clock's current reading: host now + clock offset. **observed** while recording with an observation in the last 10 s; **estimated** from an older observation (quiet or disconnected device); **unknown** when never observed (relative values count back from the newest stored line). |
| **host now** | 主机当前时间 | The host clock's current reading. |
| **clock offset** | 时钟偏移 | device time − host time. Taken from one device clock read at connect, then from live lines (device time − host arrival time); never from replayed (backfilled) lines. |
| **clock jump** | 时钟跳变 | A change of the clock offset by more than 60 s (the device clock was set). Recorded with the seq of the first line after it. |
| **arrival order** | 到达顺序 | The order lines were recorded in (seq). Lines are stored and paged in arrival order; after a clock jump device times are not monotonic in it. |

Rules: never write a bare "time" or "now" in tool descriptions, parameters, output, errors or the CHANGELOG; say device time / host time, device now / host now. One clock per parameter: all query parameters are device time; a `host:` prefix is rejected with an explanation.

## Old wording → new term (not renamed yet)

Code identifiers and user-facing output were **not** changed with the glossary: renaming them is a separate decision. The list below maps what you will still see to the term to use in prose.

| Where | Current wording | Means |
|---|---|---|
| Go package / types | `internal/service`, `service.Client`, `ClientOptions`, `Info.Clients`, `ipc` role `cli` | service process; the connection code used by MCP and CLI processes; connected MCP and CLI processes |
| JSON (`service.json`, `service status --json`) | `"clients"` | number of connected MCP and CLI processes |
| `service status` / `logcat_status` text | `clients N`, `N client(s) connected`, `no client, no pin and no recorded device`, `exits … after there are no clients …` | connected MCP and CLI processes |
| `logcat_status` / service log | `client: version …, protocol … (older than the service; compatible)`; `stops with its MCP client (in-process); the other clients then start a new one`; `inside MCP client pid N` | the MCP or CLI process; the MCP process that runs the embedded service |
| Errors | `Upgrade this client …`, `cannot serve this client`, `handoff refused: the client …`, `another client has been starting the service …`, `replaced by a newer client …` | the MCP or CLI process |
| `report` README / errors / `--home` error | `point an MCP client at …`, `the next MCP client starts the new version`, `drop --home from your MCP client configuration`, `moved for every client` | MCP host; MCP process; every MCP and CLI process |
| `install` output | `MCP client config (run the binary directly)` | MCP host configuration |
| Flag help | `service --detached`: `started by a client (internal)` | started by an MCP or CLI process |
| CI step name | `MCP stdio handshake through the background service` | service process |
| Other docs | "background service" in `server/README.md`, `docs/performance.md`, `validation/` | service process |
| Released CHANGELOG sections | "MCP client", "background service", "thin client" | unchanged: released notes are history |

## 中文摘要

本项目统一使用以下术语：**MCP 进程**（MCP 宿主按会话启动的 `android-log-mcp`，走 stdio，不录制，只把调用转发给后台服务进程）、**后台服务进程**（简称服务，`android-log-mcp service`，每用户一个，负责全部录制、写文件和清理）、**CLI 进程**（手动运行的 `call`、`record`、`report`、`root`、`set-root` 等）、**adb server**（Android SDK 的常驻进程，端口 5037，与 Android Studio 共用，只用不杀）、**adb 子进程**（后台服务进程启动的 `adb logcat`、`adb shell` 等，#24 负责清理）、**设备进程**（手机或模拟器上的进程，如被调试的 App、logd）、**MCP 宿主**（Codex、Cursor、Claude Code 等启动 MCP 进程的 agent 工具，不叫“MCP 客户端”）。禁止单独使用“进程 / server / daemon / client / 服务”，必须加限定。代码标识符和对外输出暂不改名，上表列出了旧说法与新术语的对应关系。

时钟术语（#48）：**设备时间**（设备时钟，日志行时间戳及所有时间参数）、**主机时间**（运行进程的主机时钟，用于文件轮转、保留期等，不用于筛选日志行）、**设备当前时间**（主机当前时间 + 时钟偏移，分 observed / estimated / unknown）、**时钟偏移**、**时钟跳变**（偏移变化超过 60 秒）、**到达顺序**。禁止单独写“时间 / 现在”。
