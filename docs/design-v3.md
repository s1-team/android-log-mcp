# Android 日志 MCP server：设计 v3（面向 AI agent 的批判性修订）

- 日期：2026-10-10（Asia/Shanghai）。仍处于设计阶段，没有写代码。
- 前提：调用方**永远是 AI agent**（Claude Code/Desktop、Codex、Cursor 等），不是人。
- 承接 `research-v2.md`。v2 中 AS 语法、pid 映射、buffer 选择、daemon 存储的调研结论依然有效，本文只改“对 agent 暴露的接口”和相关行为。
- 标注约定：**[源]** 有出处；**[推断]** 是我的判断，没有实测；**[未知]** 待验证。

## 0. 依据的最佳实践（均已核对原文）

1. **Anthropic《Writing effective tools for agents》**：https://www.anthropic.com/engineering/writing-tools-for-agents
   - 工具“少而精”，围绕高价值工作流设计，避免 1:1 包装底层 API。原文举例：与其做 `read_logs`，不如做 `search_logs`，只返回相关行加周边上下文。
   - 用前缀做命名空间。
   - 返回高信号信息：用自然语言名字，避开难懂的 ID；可以提供 concise/detailed 两档。
   - 分页、过滤、截断要有合理默认值；截断时要告诉 agent 怎样拿到更多。
   - 错误信息要可执行，并给出正确格式的示例。
   - 参数名要无歧义（例如 `user_id` 而不是 `user`）。
   - 原文提到 Claude Code 默认把工具输出限制在 25,000 token。
2. **MCP 规范 2025-11-25 tools**：https://modelcontextprotocol.io/specification/2025-11-25/server/tools ，以及 schema.ts
   - 工具定义包含 `title`、`inputSchema`（无参数时推荐 `{"type":"object","additionalProperties":false}`）、可选的 `outputSchema`、`annotations`（readOnlyHint、destructiveHint、idempotentHint、openWorldHint）。
   - 返回 `structuredContent` 时，**SHOULD** 同时在 TextContent 中放序列化后的 JSON；声明了 outputSchema 就 **MUST** 遵守。
   - 业务错误用 `isError: true` 返回，“供模型自我纠正”。
   - `InitializeResult.instructions` 可以放服务器级说明。
3. **Claude Code MCP 文档**：https://code.claude.com/docs/en/mcp
   - 输出超过 10,000 token 时告警，默认上限 25,000 token（环境变量 `MAX_MCP_OUTPUT_TOKENS`）。
   - 文本结果超过 **50,000 字符**会被转存为文件。
   - 单次工具调用的超时由 `MCP_TOOL_TIMEOUT` 或 per-server `timeout` 控制，默认约 28 小时。
4. **Codex 配置参考**：https://developers.openai.com/codex/config-reference ，`mcp_servers.<id>.tool_timeout_sec` **默认 60 秒**。
5. **OpenAI function calling 最佳实践**：https://platform.openai.com/docs/guides/function-calling
   - 名字和参数要清晰；用 enum 和结构杜绝非法状态。
   - “Combine functions that are always called in sequence”。
   - 初始可用函数尽量少（软建议 < 20）。
   - 不要让模型填你已经知道的参数。

---

## 1. v3 相对 v2 改了什么、为什么

| # | v2 | v3 | 理由 |
|---|---|---|---|
| 1 | 8–9 个工具：list_devices、query_logs、wait_logs、get_crashes、get_app_starts、list_processes、recording、clear_logs、mark | **4 个只读工具**：`logcat_status`、`logcat_search`、`logcat_crashes`、`logcat_app_launches` | 有重叠，也有一类“控制状态”的工具：recording、clear、mark 会让 agent 去操心隐藏状态。Anthropic 和 OpenAI 都建议少而精、合并总是连续调用的工具 |
| 2 | `query_logs` 和 `wait_logs` 分开 | 合并为 `logcat_search`，长轮询用 `after` + `wait_ms` | 两者的过滤和返回完全相同 |
| 3 | `list_devices` 和 `list_processes` 是两个工具 | 合并为 `logcat_status(package?)` | agent 的第一个问题通常是“设备在吗、在录吗、App 在跑吗”，一次回答 |
| 4 | `recording` 用来启停，只自动录一台设备 | **自动记录所有在线设备**（上限 4 台，可配置），没有启停工具 | 消除“当前在录哪台”这一隐藏状态；对 agent 来说零参数就能用 |
| 5 | `clear_logs` 可以执行 `logcat -c` | **删除**。改成“先拿 cursor 再复现”的模式（`logcat_status` 返回 `cursor`） | 清日志有破坏性，而且多个 agent 共用时会互相干扰；用游标就能做到“只看这次复现之后的日志” |
| 6 | `mark` 用 `log -t` 往设备打标记 | **删除**，同上用 cursor 替代 | 少一个工具，也少一次写设备 |
| 7 | 过滤串解析失败时，按 AS 行为静默退化为子串匹配，只给 warning | **严格模式**：解析错误返回 `isError:true`，附带列号、`^` 指示和改写建议。每次成功的响应都**回显规范化后的解释**（`filter parsed as: …`） | 静默退化会让 agent 拿到错误结果却毫无察觉。回显解释可以暴露 AS 里“同字段隐式 OR”这类和直觉相反的语义 |
| 8 | 只有 AS 过滤串 | AS `filter` 加上少量结构化参数：`package`、`tags[]`、`min_level`、`from`/`to`，**一律 AND** | 最常见的意图（某个 App、几个 tag、级别、时间窗）不必写语法，降低出错率；需要表达力时再用 filter |
| 9 | 输出可以是 JSON 或 text，默认 200 条 | **紧凑文本行为主**：表头写一次时区和日期，每行 `HH:MM:SS.mmm L pid/tid 包名 tag: msg`。另有字符硬预算 30,000，以及“如何拿更多”的明确指引 | JSON 每行要重复键名，token 成本约为文本的 2–3 倍 **[推断]**；Claude Code 超过 50,000 字符会转存文件 |
| 10 | 没有摘要 | `view:"summary"`：按级别、tag、包统计，再加 Top 重复消息模板。lines 模式截断时自动附一个迷你摘要 | 先看全貌再缩小范围，比一次倒出几千行省 token |
| 11 | 没有去重 | 连续完全相同的行折叠为 `↳ ×37 identical until 01:20:09.001`。多行消息（堆栈）整体保留，不拆开 | 省 token，也保证堆栈完整 |
| 12 | 只按 seq 或时间分页 | **不透明字符串 cursor**，含设备、boot、seq，跨 server 重启仍然有效。`before`/`after` 两个方向 | 无状态、可重放、幂等 |
| 13 | get_crashes 只看日志 | 加 **ANR**（events `am_anr` + ActivityManager `ANR in` 块），并从 **dropbox** 补充 traces 和历史崩溃 | 用户已确认。dropbox 还能补回“MCP 启动前”和“日志已经滚掉”的崩溃 |
| 14 | crash ID 没有定义 | 短 ID（`cr7`、`anr2`）。只有一个匹配时**自动返回详情**，包括同进程崩溃前 40 行日志 | Anthropic 指出语义化或短 ID 能减少幻觉；少一次调用 |
| 15 | get_app_starts 返回原始证据 | `logcat_app_launches` **直接给出算好的时长**：请求→进程启动→onCreate→onResume、TTID、TTFD、冷热判定，并附证据行时间 | 让 agent 不必自己算；冷启动场景只需 1 次调用 |
| 16 | 时间：`since` 接受 epoch、RFC3339 或相对时长 | `from`/`to` 接受**设备本地时间**（`2026-10-10 01:20:00` 或 `01:20:00`）、带偏移的 ISO、或相对时长（`30m`）。输出一律用设备本地时间，表头注明时区 | 设备日志本来就是设备本地时间。混用主机和设备时区是 agent 常见的错因 |
| 17 | 长等待没有上限 | `wait_ms` 最大 **25,000**；默认 0 | Codex 的工具超时默认 60 秒 |
| 18 | 所有工具都带 annotations（v2 没写） | 4 个工具都设 `readOnlyHint:true`、`openWorldHint:false` | 规范建议；客户端可以据此免确认（客户端行为 **[未知]**） |

**明确没有改的部分**：AS 语法本身照旧（键、运算符、优先级、同字段隐式 OR、大小写规则），只把出错时的行为改成报错。存储、daemon、设备发现、pid 映射沿用 v2。

---

## 2. 最终工具集（4 个，都只读）

**通用约定**
- 工具名统一用 `logcat_` 前缀，模型熟悉 logcat 这个词。服务器名建议为 `android-log`，在 Claude Code 中呈现为 `mcp__android-log__logcat_search`。
- 所有工具都有可选参数 `device`。只有一台有数据的设备时自动选中；有多台时如果没指定，返回可执行的错误并列出各设备的 serial。
- 返回方式：v1 **只返回 TextContent**（紧凑文本），不声明 outputSchema。原因见第 7 节第 2 个开放问题。
- annotations：`{"readOnlyHint": true, "openWorldHint": false}`，并设置 `title`。
- server instructions（`InitializeResult.instructions`）放一段 ≤ 120 词的工作流说明，见 2.5。各客户端是否把它注入模型上下文 **[未知]**，所以每个工具的 description 也要能独立读懂。
- 工具 description 用**英文**写：模型对英文工具描述最熟，社区工具也基本是英文。本文档其余部分用中文。

### 2.1 `logcat_status`

**description**
```
Check adb/device/recording state and (optionally) whether an app is running. Call this first if unsure which device is connected, or to get a `cursor` marking "now" before the user reproduces a bug (then pass it as `after` to logcat_search).
Logs from all connected devices are recorded automatically in the background (persisted across sessions); there is nothing to start or stop.
With `package`, also reports install state and live/recently-dead processes (pid, process name, start/end time), including multi-process names like com.foo:remote.
```

**inputSchema**
```json
{
  "type": "object",
  "properties": {
    "device":  {"type": "string", "description": "adb serial. Omit when only one device is connected."},
    "package": {"type": "string", "description": "Exact application id, e.g. com.example.app. Optional."}
  },
  "additionalProperties": false
}
```

**输出示例**（示意，数据是虚构的）
```
adb: ok (server 1.0.41)
devices (1):
  emulator-5554  online  Pixel_8_API_35  android 15 (sdk 35)  tz Asia/Shanghai (+08:00)
    device time now: 2026-10-10 01:44:12   recording since 2026-10-09 18:02:31   history from 2026-10-09 17:58:10
    stored 1,204,331 lines; gaps: 1 (2026-10-09 22:10:04–22:10:19, device disconnected)
cursor (now): c1.ZW11LTU1NTQ.9f2a.1204331
package com.foo: installed (uid 10213)
  running: pid 23011 com.foo (since 01:40:55), pid 23102 com.foo:push (since 01:40:57)
  recently dead: pid 21877 com.foo (01:31:02–01:39:47, crashed: see logcat_crashes id=cr3)
```

### 2.2 `logcat_search`

**description**
```
Search recorded logcat lines (buffers main, system, crash, events) of one device. Returns matching lines oldest→newest in a compact format; by default the NEWEST `limit` matches.
Filtering = `filter` (Android Studio Logcat query syntax) AND the structured params (package, tags, min_level, from, to). All parts are ANDed.
Filter syntax (Android Studio): key:value contains (case-insensitive), key=:value exact, key~:regex (RE2, case-sensitive), -key:value negates. Keys: tag message package process line level age is pid buffer. level:error = error and above. age:5m. is:crash, is:stacktrace. Bare words search the whole line.
Combining: `&` AND, `|` OR (put spaces around them), parentheses group. Space-separated terms are ANDed, EXCEPT repeated positive terms on the same key, which are ORed: `tag:A tag:B` = A OR B; use `message:foo & message:bar` for AND. Quote values with spaces: message:"Start proc".
Examples:
  tag=:OkHttp tag=:MyRepo level:debug
  is:crash | tag~:"^am_(anr|crash)$"
  message~:"Displayed com\.foo" -tag:chatty
Invalid filters return an error with the position and a fix; nothing is guessed. The response echoes how the filter was interpreted — check it.
Paging: response gives `before`/`after` cursors. To wait for new lines (e.g. while the user reproduces), pass `after` plus `wait_ms` (≤25000).
Use view="summary" first for broad or unknown time ranges (counts by level/tag/package, top repeated messages).
```

**inputSchema**
```json
{
  "type": "object",
  "properties": {
    "device":    {"type": "string", "description": "adb serial. Omit when only one device has logs."},
    "filter":    {"type": "string", "description": "Android Studio Logcat query, e.g. `tag=:OkHttp level:warn -message:heartbeat`. Optional."},
    "package":   {"type": "string", "description": "Exact application id; matches lines from ALL its processes (com.foo, com.foo:remote), including processes that have since died."},
    "tags":      {"type": "array", "items": {"type": "string"}, "maxItems": 20, "description": "Exact tag names, ORed together, e.g. [\"OkHttp\",\"MyRepo\"]."},
    "min_level": {"type": "string", "enum": ["verbose", "debug", "info", "warn", "error", "fatal"], "description": "Minimum priority."},
    "from":      {"type": "string", "description": "Start time, inclusive. Device-local `2026-10-10 01:20:00` or `01:20:00` (= device's today), ISO-8601 with offset, or relative like `30m`, `2h` (= device now minus that)."},
    "to":        {"type": "string", "description": "End time, exclusive. Same formats as `from`. Default: now."},
    "view":      {"type": "string", "enum": ["lines", "summary"], "default": "lines"},
    "limit":     {"type": "integer", "minimum": 1, "maximum": 1000, "default": 200, "description": "Max matching lines to return (output is also capped at ~30k chars)."},
    "context":   {"type": "integer", "minimum": 0, "maximum": 20, "default": 0, "description": "Also show N lines before/after each match from the same process (like grep -C)."},
    "before":    {"type": "string", "description": "Cursor: return matches older than this (from a previous response)."},
    "after":     {"type": "string", "description": "Cursor: return matches newer than this (from a previous response or logcat_status)."},
    "wait_ms":   {"type": "integer", "minimum": 0, "maximum": 25000, "default": 0, "description": "With `after`: block up to this long until at least one new match arrives."}
  },
  "additionalProperties": false
}
```

**组合与优先级（写入实现规范）**
- 最终谓词 = `filter` ∧ `package` ∧ (`tags` 中任一) ∧ `min_level` ∧ `[from, to)` ∧ cursor 边界。
- 结构化参数**从不覆盖** filter，只做交集。例如 filter 里写了 `age:10m`，同时 `from=01:00`，结果取两者的交集。
- 能静态判定为必然为空的组合（例如 `from` ≥ `to`，或者 `before` 与 `after` 同时给出）直接报错说明，不返回“0 条”。
- `before` 和 `after` 不能同时给。给了 `after` 时，按从旧到新取前 `limit` 条（用于追新）；否则取最新的 `limit` 条。
- 结构化参数 `package` 是**精确匹配 applicationId**；AS 语法里的 `package:` 是 contains，两者区别要写在 description 里。

**输出示例：lines**（示意）
```
device emulator-5554 · times are device-local Asia/Shanghai (+08:00) · date 2026-10-10
filter parsed as: package == com.foo AND (tag = "OkHttp" OR tag = "MyRepo") AND level >= DEBUG AND time in [01:20:00, 01:30:00)
matched 1,284 lines; showing newest 200 (01:27:41.002–01:29:58.731)
format: time level pid/tid process tag: message
01:27:41.002 D 23011/23090 com.foo OkHttp: --> GET https://api.foo.com/v1/feed
01:27:41.315 D 23011/23090 com.foo OkHttp: <-- 200 https://api.foo.com/v1/feed (312ms)
01:27:41.316 I 23011/23011 com.foo MyRepo: feed loaded items=20
  ↳ ×37 identical until 01:27:49.001
01:27:50.120 W 23102/23110 com.foo:push MyRepo: token refresh failed
    java.net.UnknownHostException: Unable to resolve host "push.foo.com"
        at java.net.Inet6AddressImpl.lookupHostByName(Inet6AddressImpl.java:156)
        ... (whole multi-line message kept)
…
[truncated: 1,084 older matches not shown]
more: before="c1.ZW11LTU1NTQ.9f2a.1188020"  ·  newer/wait: after="c1.ZW11LTU1NTQ.9f2a.1190311"
top tags in all 1,284 matches: OkHttp 911, MyRepo 373 · narrow with e.g. -message:heartbeat, min_level=info, or a smaller from/to
```

**行格式细则**
- 时间只显示 `HH:MM:SS.mmm`，日期放在表头；跨天时插入 `--- 2026-10-11 ---` 分隔行。
- process 列显示进程名（多进程 App 会显示 `com.foo:push`）；pid 未知时显示 `?`。
- 单条消息超过 2,000 字符时截断，标记为 `…[+N chars, use filter on this line's pid+time to get full]`。堆栈类消息上限放宽到 8,000 字符。
- `context` 带出来的周边行以两个空格缩进，匹配行不缩进。
- 折叠规则：同一 pid、tag、level，消息字节完全相同且相邻时才折叠，不做数字归一化，避免误伤。
- 时间来源一律是设备时间戳。

**输出示例：summary**（示意）
```
device emulator-5554 · Asia/Shanghai (+08:00) · 2026-10-10 01:20:00–01:30:00
filter parsed as: package == com.foo
total 18,402 lines · by level: V 0 D 12,110 I 5,201 W 902 E 187 F 2
top tags: OkHttp 6,021 · Choreographer 2,310 · MyRepo 1,904 · RenderThread 1,200 · …(10 shown)
top processes: com.foo 17,980 · com.foo:push 422
top repeated messages (digits normalized to #):
  4,010× OkHttp: <-- 200 https://api.foo.com/v1/feed (#ms)
  2,301× Choreographer: Skipped # frames!  The application may be doing too much work on its main thread.
crashes in range: 1 (cr3 01:24:10 java.lang.IllegalStateException) · ANRs: 0
latest errors (5): 01:24:10.511 E 21877 AndroidRuntime: FATAL EXCEPTION: main …
next: logcat_search with filter/tags narrowed, or logcat_crashes id="cr3"
```

### 2.3 `logcat_crashes`

**description**
```
List Java crashes, native crashes (tombstones) and ANRs, newest first, already parsed and grouped — use this instead of searching for "FATAL EXCEPTION" yourself.
Sources: crash/system/events log buffers (AndroidRuntime FATAL EXCEPTION, libc "Fatal signal" + DEBUG tombstone lines attributed to the crashed pid, am_anr + "ANR in" block) plus the device DropBox (data_app_crash / data_app_native_crash / data_app_anr), which also covers crashes from before recording started and ANR thread dumps.
Pass `id` (e.g. "cr3") for full detail: complete stack / Caused-by chain, the crashed process's last 40 log lines before the crash, and for ANRs the reason plus main-thread trace (or why traces are unavailable). If only one crash matches, detail is returned automatically.
```

**inputSchema**
```json
{
  "type": "object",
  "properties": {
    "device":  {"type": "string"},
    "package": {"type": "string", "description": "Exact application id. Omit for all apps."},
    "kinds":   {"type": "array", "items": {"type": "string", "enum": ["java", "native", "anr"]}, "description": "Default: all three."},
    "from":    {"type": "string", "description": "Same formats as logcat_search.from. Default: everything recorded."},
    "to":      {"type": "string"},
    "id":      {"type": "string", "description": "Crash id from a previous list (e.g. cr3, anr1) to get full detail."},
    "limit":   {"type": "integer", "minimum": 1, "maximum": 50, "default": 10}
  },
  "additionalProperties": false
}
```

**ID 方案**
- 每台设备单调递增，`cr<N>` 表示 crash（java 和 native 共用一个序列），`anr<N>` 表示 ANR。ID 写入持久存储，跨会话稳定。
- 带 device 时 ID 在设备内唯一；有多台设备时，ID 前加 serial 的短前缀 **[待定]**。

**列表输出**（示意）
```
device emulator-5554 · Asia/Shanghai (+08:00) · package com.foo · 3 crash groups (5 events) in recorded history
cr5  2026-10-10 01:24:10  java   pid 21877 com.foo (thread main)
     java.lang.IllegalStateException: Fragment FeedFragment not attached to a context.
       at androidx.fragment.app.Fragment.requireContext(Fragment.java:972)
       at com.foo.feed.FeedFragment.onLoaded(FeedFragment.kt:88)
       at com.foo.feed.FeedViewModel$load$1.invokeSuspend(FeedViewModel.kt:41)
       … 23 more frames · same signature also at 01:12:03 (cr2), 00:58:40 (cr1)
anr2 2026-10-10 00:40:12  anr    pid 20111 com.foo  reason: Input dispatching timed out (… is not responding. Waited 5001ms for KeyEvent)
     main thread: Blocked on lock held by thread 23 "DiskIO" — at com.foo.db.Cache.get(Cache.kt:55)   [traces: dropbox data_app_anr]
cr4  2026-10-09 23:02:55  native pid 19876 com.foo  SIGSEGV (SEGV_MAPERR) fault addr 0x0 in libfoo.so (Java_com_foo_Native_decode+44)
more: logcat_crashes id="cr5" for full detail
```

**详情**（`id` 给定，或只有一个匹配）包含：
- 完整堆栈，Caused by 链保持完整。总长超过 20,000 字符时，每段只保留前 40 帧和最后 5 帧，并标注省略了多少。
- `before crash (pid 21877, last 40 lines)`：用 logcat_search 的行格式。
- native 崩溃：signal、abort message、backtrace（全部帧）、构建指纹。
- ANR：Subject 或 reason、CPU usage 摘要（截前 10 行）、主线程 trace 全文、它所等待的锁的持有线程的 trace。
  - 拿不到 traces 时明确写出原因，例如：`traces: unavailable — dropbox entry not found / rate-limited / dumpsys dropbox permission denied. /data/anr needs root. Fallback: run "adb bugreport" yourself.`

**ANR 与 dropbox 的事实**
- AMS 把 ANR 写入 dropbox，tag 为 `<processClass>_anr`，processClass 取 `system_server`、`system_app` 或 `data_app`。写入时传入 tracesFile 作为 dataFile，篇幅允许时**内联 traces 内容**。dropbox 有速率限制（DropboxRateLimiter）。
  - 来源 [源]：frameworks/base main 的 `ActivityManagerService.addErrorToDropBox`、`ProcessErrorStateRecord.java`（`addErrorToDropBox("anr", …, tracesFile, …)`）
- `dumpsys dropbox` 的参数为 `[-p|--print] [-f|--file] [--proto] [timestamp]`，其余非 `-` 开头的参数作为 tag 或时间的搜索条件。
  - 权限检查是 `checkDumpAndUsageStatsPermission`，**SHELL_UID 直接放行**。[源]：`DropBoxManagerService.java`、`DumpUtils.java`（main 分支）
- **[未知]**：老版本和各 OEM ROM 上 dropbox tag 是否开启、保留条数、traces 是否被截断，都需要实机验证。
- 实现方式：daemon 定时（例如 60 秒一次）或在看到 `am_anr`/`am_crash` 后触发 `dumpsys dropbox --print data_app_anr`（crash 同理），解析后缓存到本地。agent 调用时不需要等待。

### 2.4 `logcat_app_launches`

**description**
```
Report recent app launches with computed timings — use this for "how long does cold start take / time to first onResume".
For each launch: cold/warm/hot, the launched activity, and a timeline relative to the launch request: process start (am_proc_start), Application/Activity onCreate, onStart, onResume (from wm_on_*_called events in the app process), Displayed (time to initial display, TTID) and Fully drawn (TTFD, only if the app calls reportFullyDrawn()).
This tool only reads logs; launch the app yourself first (e.g. `adb shell am force-stop <pkg>` then `adb shell am start -W -n <pkg>/<activity>`), or ask the user to tap the icon, then call this.
```

**inputSchema**
```json
{
  "type": "object",
  "properties": {
    "device":  {"type": "string"},
    "package": {"type": "string", "description": "Exact application id. Omit for all apps."},
    "from":    {"type": "string", "description": "Default: last 30 minutes."},
    "to":      {"type": "string"},
    "limit":   {"type": "integer", "minimum": 1, "maximum": 20, "default": 5}
  },
  "additionalProperties": false
}
```

**输出示例**（示意）
```
device emulator-5554 · Asia/Shanghai (+08:00) · package com.foo · 2 launches
#1 2026-10-10 01:40:55.102  COLD  com.foo/.ui.MainActivity  (pid 23011)
   +0 ms     launch request      START u0 {act=MAIN cat=[LAUNCHER] cmp=com.foo/.ui.MainActivity} from uid 10132
   +38 ms    process started     am_proc_start pid 23011
   +412 ms   MainActivity.onCreate returned   (onCreate took 186 ms)
   +431 ms   onStart returned    (took 9 ms)
   +440 ms   onResume returned   (took 6 ms)   ← request→first onResume: 440 ms
   +812 ms   Displayed (TTID)    system reports +812ms
   —         Fully drawn         not reported (app does not call reportFullyDrawn)
#2 2026-10-10 01:35:12.880  WARM  …
caveats: onX "returned" times are when the callback finished (event logged after return); TTID is measured by the system from the launch request.
```

**算法要点**
- 锚点：ActivityTaskManager 的 `START u0 {… cmp=<pkg>/…}`。
- 冷启动：锚点之后、首个 onResume 之前，出现了该 package 的 `am_proc_start`。
- 温启动：进程已存在，但出现了 `wm_on_create_called`。
- 热启动：只有 `wm_on_resume_called`，没有 onCreate。
- `wm_on_*_called` 必须出自该包的 pid，并且组件名匹配。
- `Displayed` 同时兼容 ActivityTaskManager 和 ActivityManager 两种 tag、是否带 ` for user N` 两种写法。
- 证据缺失时逐项写 `not found`，并给出原因猜测，例如：events buffer 没录到；启动早于 history 起点。
- 上述冷/温/热判定规则是 **[推断]**，需要用 `am start -W` 的 LaunchState 交叉验证。

### 2.5 server instructions（草案）

```
Android logcat recorder. Logs from every connected device are captured continuously in the background and kept across sessions; nothing to start/stop/clear.
Typical flows: crashes/ANRs → logcat_crashes. Startup time → logcat_app_launches. Anything else → logcat_search (use view="summary" first for wide ranges).
To capture a reproduction: logcat_status → take `cursor` → user reproduces → logcat_search(after=cursor, wait_ms=20000).
All times are device-local; responses state the device timezone.
```

---

## 3. 暴露给 agent 的过滤语言（AS 语法 + 少量扩展 + 严格报错）

### 3.1 语法速查

| 写法 | 含义 |
|---|---|
| `tag:x` `message:x` `package:x` `process:x` `line:x` | contains，忽略大小写 |
| `key=:x` | 完全相等，区分大小写 |
| `key~:re` | 正则部分匹配，区分大小写；可用 `(?i)` 关闭大小写区分；引擎是 RE2 |
| `-key:x` `-key=:x` `-key~:re` | 取反 |
| `level:<verbose\|debug\|info\|warn\|error\|assert>` | 不低于该级别 |
| `age:<N>[smhd]` | 最近 N 秒、分、时、天（以设备时钟为准，见 3.4） |
| `is:crash` `is:stacktrace` | 与 AS 定义一致 |
| 裸词或 `"带 空格"` | 在整行中 contains |
| `a & b`、`a \| b`、`( … )` | AND、OR（`&` 优先）、分组；运算符两边**必须有空格** |
| 空格并列 | AND；**例外**：同一字段的正向项之间是 OR；多个 `level:` 之间、多个 `age:` 之间、`is:crash` 与 `is:stacktrace` 之间也是 OR |

**扩展（非 AS，在 description 和回显中标注 `[ext]`）**
- `pid:<n>`、`tid:<n>`：精确匹配。
- `buffer:<main|system|crash|events>`：可取反。
- `level:` 额外接受单字母 `V/D/I/W/E/A/F`，以及 `fatal`（等同 assert）。agent 常沿用 logcat 的 `*:E` 习惯。
- `name:` 和 `package:mine`：**不支持**，因为 MCP 没有“当前工程”的概念。遇到时报错，并建议改用 `package` 参数。

### 3.2 解释回显

每次成功的响应第一行写出规范化的谓词树，例如：

`filter parsed as: (tag = "OkHttp" OR tag = "MyRepo") AND level >= DEBUG AND NOT message contains "heartbeat"`

agent 可以借此发现自己写的 `message:foo message:bar` 实际被解释成了 OR。

### 3.3 错误行为（替代 AS 的静默退化）

所有错误都以 `isError: true` 返回，文本由 3 部分组成：问题、位置、改法。

| 情况 | 返回文本示例 |
|---|---|
| 未知键 | `Filter error at col 12: unknown key "lvl". Keys: tag message package process line level age is pid tid buffer.`<br>`  tag:OkHttp lvl:error`<br>`             ^`<br>`Did you mean: level:error` |
| 键名大写 | `Filter error at col 1: keys are lowercase — use tag: instead of Tag:` |
| 非法 level | `level:"err" is not a level. Use one of verbose debug info warn error assert (or V D I W E A F).` |
| 非法 age | `age:"10min" — use <number><s|m|h|d>, e.g. age:10m` |
| 引号未闭合或括号不配对 | 给出列号，并附一个配对后的建议写法 |
| RE2 不支持的正则 | `tag~:"(?<!Ok)Http" uses look-behind, which is not supported (regex engine: Go RE2 — no look-around, no backreferences). Rewrite, e.g.: tag~:Http -tag~:OkHttp` |
| 括号内空格并列 `(a b)` | `Inside parentheses terms must be joined with & or \|, e.g. (tag:a & level:error)` |
| `name:` 或 `package:mine` | `package:mine is not supported here (no IDE project). Use the package parameter: package="com.foo"` |

**警告**（不算错误，写在表头 `note:` 行）
- 可疑写法 `tag:foo|bar`：`note: value "foo|bar" contains "|" — if you meant OR write tag:foo | tag:bar`
- 同字段多项被合并成 OR 时：`note: tag:A tag:B are ORed (same key)`
- 结果为 0 条时给出最可能的原因：package 在该时间范围内没有进程；tag 大小写不匹配（`=:` 区分大小写）；时间窗在 history 起点之前；附近相似的 tag 名有哪些。

### 3.4 与 AS 的有意差异（全部写进 description 或文档）

1. 出错时报错，不退化。
2. 扩展键 pid、tid、buffer，以及 level 单字母。
3. 不支持 `name:` 和 `package:mine`。
4. `age:` 以**设备时钟的估计值**为准，AS 用的是主机时钟。理由是日志时间戳来自设备。设备“现在”= 主机现在 + 连接时测得的偏移，偏移用 `adb shell date +%s` 测量，精度 1 秒，**[推断]** 够用。
5. 正则引擎是 RE2，不是 java.util.regex。
6. `foo bar` 按 AS **代码**解释为 AND；AS 文档说是短语，两者冲突，见 v2。

---

## 4. 错误与边界情况（可执行的文本）

| 情况 | 返回（isError） |
|---|---|
| adb 不在 PATH 中 | `adb not found. Install Android platform-tools or set ANDROID_LOG_MCP_ADB=/path/to/adb (also checked: $ANDROID_HOME/platform-tools, ~/Library/Android/sdk/platform-tools).` |
| adb server 起不来 | 附 `adb start-server` 的错误输出的前 3 行 |
| 无设备 | `No device connected. Connect a device with USB debugging enabled or start an emulator; logcat_status shows the state. Previously recorded devices: emulator-5554 (last seen 01:10:22) — pass device="emulator-5554" to query its stored logs.` |
| 设备 unauthorized | `Device R5CT… is unauthorized: accept the "Allow USB debugging" prompt on the phone.` |
| 多台设备且未指定 | `2 devices have logs; pass device=one of: emulator-5554 (Pixel_8_API_35, online), R5CT12345 (SM-S918B, online).` |
| 指定的设备没有记录 | 列出已有记录的设备 |
| package 从未见过 | `No logs from package "com.fo" in range. Did you mean com.foo? (installed: yes). Known packages with logs in range: com.foo, com.android.systemui, …`（不算错误，返回 0 条并附提示） |
| App 当前没在运行 | 不算错误：`com.foo has no running process (last pid 21877 died 01:39:47).` |
| 时间无法解析 | `from="yesterday 3pm" not understood. Use "2026-10-10 15:00:00", "15:00:00", ISO-8601, or a duration like "30m".` |
| 时间窗早于 history 起点 | 正常返回，并在表头注明：`note: history starts 2026-10-09 17:58:10; earlier logs were not recorded (device buffer had rolled over).` |
| cursor 失效（设备重启或数据被清理） | `cursor refers to boot 9f2a which has been pruned/replaced; showing from oldest available instead.`（这算 note；只有 cursor 格式错误时才是 error） |
| `wait_ms` 超时 | 不算错误：`no new matches within 20000 ms; call again with the same after cursor to keep waiting.` |
| 输出预算耗尽 | 见 2.2 的截断尾部：剩余条数、cursor、缩小范围的建议 |
| 录制有缺口 | 在相关时间段的表头注明 `gap: 22:10:04–22:10:19 (device disconnected)` |

---

## 5. 场景走查（最少调用次数）

**A. 冷启动到首个 onResume 的耗时**（1–2 次调用）
1. agent 用自带的 shell 执行 `adb shell am force-stop com.foo && adb shell am start -W -n com.foo/.ui.MainActivity`；没有 shell 能力时，请用户手动点开 App。
2. `logcat_app_launches(package="com.foo")`，直接得到 `request→first onResume: 440 ms` 和各阶段耗时。
3. 想看启动期间的 App 日志时：`logcat_search(package="com.foo", from="01:40:55", to="01:40:57")`，时间取自上一步结果中的绝对时间。

**B. 崩溃或 ANR 分析**（1–2 次调用）
1. `logcat_crashes(package="com.foo")`。只有一条时直接返回详情：完整堆栈加崩溃前 40 行。多条时，列表里已经有异常、首个 App 帧和重复次数。
2. 需要时调用 `logcat_crashes(id="cr5")`，或者看更大范围的上下文：`logcat_search(package="com.foo", from="01:23:40", to="01:24:11", min_level="debug")`。
3. ANR 走同样的路径。详情里有 reason 和主线程 trace；traces 不可用时会写明原因和替代办法（bugreport）。

**C. 用几个 tag 定位 bug**（1–3 次调用）
1. 范围不明时，先 `logcat_search(package="com.foo", from="15m", view="summary")`，看 top tags 和重复消息。
2. `logcat_search(package="com.foo", tags=["OkHttp","MyRepo","Billing"], min_level="debug", from="15m")`。
3. 需要排除时：`filter="-message:heartbeat -tag:Choreographer"`。需要 AND 两个 message 条件时：`filter="message:order & message~:\"id=\\d+\""`。

**D. 指定时间窗**（1 次调用）
- `logcat_search(from="2026-10-10 01:20:00", to="2026-10-10 01:25:00", filter="level:warn")`，或者用相对时间 `from="30m"`，也可以加 `filter="... age:10m"`，两者取交集。

**E. 现场复现**（2 次调用）
1. `logcat_status()`，拿到 `cursor`。
2. 请用户复现。
3. `logcat_search(after=cursor, package="com.foo", wait_ms=20000)`。没等到新日志就用同一个 cursor 再调。
4. 也可以复现后直接 `logcat_crashes(package="com.foo", from=<上一步的时间>)`。

---

## 6. 其他与 agent 友好性相关的决策

- **无隐藏会话状态**：所有工具都是纯查询，同样的参数加同样的 cursor 得到同样的结果（`wait_ms` 和“最新 N 条”除外，它们随时间变化）。cursor 是不透明字符串 `c1.<base64 serial>.<boot>.<seq>`，可以跨 server 重启、跨 agent 共享。
- **零参数调用都有用**：
  - `logcat_status()`：设备、录制状态、cursor。
  - `logcat_search()`：最新 200 行，附 top tags 摘要。
  - `logcat_crashes()`：全部 App 的近期崩溃。
  - `logcat_app_launches()`：最近 30 分钟内的启动。
- **大小控制**：
  - 每次响应的硬预算是 30,000 字符，留出 Claude Code 50,000 字符转存阈值以下的余量，大约 8–10k token。
  - limit 默认 200。超出时按 2.2 的截断格式输出。
  - 可以考虑给 Claude Code 声明 `anthropic/maxResultSizeChars`，**[未知]** 是否值得做。
- **超时**：只有 `wait_ms` 会阻塞，上限 25 秒。其他查询目标是 1 秒内返回（内存索引加分段扫描）。冷查询扫描大段历史时，返回已扫描的部分，并给出 `continue: before=<cursor>`，不做长时间阻塞。
- **时区**：
  - 所有输出用设备本地时间，表头写 `tz <IANA> (+hh:mm)`。时区来源是 `getprop persist.sys.timezone`，具体命令 **[需实机]**。
  - 输入的绝对时间也按设备本地时间解释；带偏移的 ISO 时间按其偏移换算。
  - 主机与设备时区不同时，`logcat_status` 会提示：`note: host timezone differs (UTC-07:00)`。
- **pid 内联名字**：每行都带进程名，名字在入库时解析（v2 的方案），已死进程也能正确显示。
- **安全**：工具不执行任意 shell，不写设备。只调用固定的 `logcat`、`ps`、`dumpsys dropbox`、`getprop`、`pm list packages -U`、`date`。

---

## 7. 开放问题（需要用户决定或实测）

1. **要不要提供一个带副作用的 `logcat_measure_launch(package, activity?, mode=cold|warm, runs=1..5)`**：它会 force-stop App 再 `am start -W`，然后返回汇总。
   - 好处：没有 shell 的客户端（如 Claude Desktop）也能一键测冷启动；多次运行可以给出中位数。
   - 坏处：会杀掉 App（destructiveHint），并超出“只看日志”的边界。
   - 建议：v1 不做，或者用启动参数 `--enable-actions` 打开，默认关闭。
2. **structuredContent 和 outputSchema**：各客户端把 text 还是 structuredContent 交给模型，**[未知]**，需要按客户端实测。规范要求返回 structuredContent 时 SHOULD 同时给出 JSON 文本，可能导致 token 翻倍。
   - 建议：v1 只返回文本。v1.1 在 status、crashes、launches 这几个小结果上评测加 structuredContent 的效果。
3. **自动记录所有设备**还是只记录一台。前者对 agent 最简单，但资源占用随设备数线性增长；上限 4 台是拍脑袋定的。
4. **严格报错是否需要一个“AS 兼容的宽松模式”开关**。agent 场景下建议不提供。
5. **默认 limit**（200 条、30k 字符）和**是否默认折叠重复行**，应通过评测确定。Anthropic 建议做 eval：拿真实任务比较调用次数、token 和成功率。
6. **dropbox 在旧版本和 OEM ROM 上的可用性**，以及 ANR traces 是否被截断。
7. **冷/温/热判定规则**和 `wm_on_*_called` 在旧版本上是否存在（这些事件在 main 分支中存在，最早出现的版本 **[未知]**）。
8. **工具 description 用英文**是否可以接受；如有需要可以加中文别名说明。
9. 多设备时 crash ID 的格式。
