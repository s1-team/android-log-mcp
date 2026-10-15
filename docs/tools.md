# Tools and time semantics

The README has the short list; this page has the full descriptions. Terms are defined in [glossary.md](glossary.md).

| Tool | Purpose | Main parameters |
|---|---|---|
| `logcat_status` | Starts recording the device (or keeps it recording until now + `record_for`, at most `24h`; `"0"` stops). Devices, recording and backfill state, history range and gaps, the service process (pid, version, root, linger, pins), and a "now" cursor; with `package`, install state and running device processes. The lines that rarely change (`root`, `linger`, `lifetime`, `service log`, `adb`, `store`) are shown on the first call of a session and again when they change; `verbose=true` always shows them | `device` `package` `record_for` `verbose` |
| `logcat_search` | Search logs. `view="lines"` (default) returns the newest `limit` lines (default 50); `view="summary"` returns counts by level, tag and device process, repeated messages, crash count and latest errors. Java stacks inside a line are short by default (`stack="short"`): each exception and `Caused by` keeps its first 3 frames, the rest is `↳ K more frames`, a stack already shown in the response is `↳ same K frames as <time>`, and a line holding a stack is at most 2,000 chars; `stack="full"` shows every frame | `filter` `package` `tags` `min_level` `from` `to` `limit` `context` `before` `after` `wait_ms` `stack` |
| `logcat_crashes` | Crashes and ANRs grouped by signature. With a single match or an `id`: full details (stack or tombstone, the pid's last 40 lines before the crash; for ANRs the main-thread trace and lock holders) | `package` `kinds` (java/native/anr) `from` `to` `id` `limit` |
| `logcat_app_launches` | Launch timeline (COLD/WARM/HOT/UNKNOWN), last 30 min by default. Each `+N ms` is when a line was logged relative to the launch request; TTID / TTFD on their own line with their source (`Displayed` or `*_activity_launch_time`). A step logged after a device clock jump that followed the request shows `?` and `duration: unreliable (device clock jumped)` with its device time instead of a number; steps are paired within the clock segment of the request and across the jump in arrival order | `package` `from` `to` `limit` |

- **Device time:** `from` / `to` / `age:` are device time (the Android device's clock, as printed in lines): relative (`30m`, `2h` = device now minus that; such a window ends at device now + 2 s), device-local (`14:05`, `2026-10-10 14:05:00`), ISO 8601 and epoch. Output is in the **device's time zone**; each response shows device now, host now and the clock offset. Lines stay in arrival order; when the device clock is changed (auto_time, a manual set, expiry testing) the output marks the clock jump with matches on each side.
- **Reproducing a bug:** call `logcat_status` for a `cursor`, let the user reproduce, then call `logcat_search(after=cursor, wait_ms=20000)`. If `logcat_status` says "backfill in progress", call it again: it returns no cursor until the device's log buffer has been read.
- **`package` parameter:** an exact application id. It matches the package's device processes: a device process belongs to the installed package that its name equals or extends at a `.` or `:`. When packages share a uid, a device process of that uid whose name matches none of them (for example `android.process.acore` for `com.android.providers.contacts` and `com.android.contacts`) can run code of any of them: its lines, crashes and lifetimes are attributed to no package, and `package=` includes them for each package of the uid, with a `note:` naming the packages. The `package:` filter term keeps its Android Studio meaning (device process name prefix).
- **Several devices:** a call without `device` uses the single connected device; with several connected, it starts none and lists them.

Filter syntax:

```
tag:OkHttp                 contains (case-insensitive, Android Studio semantics)
tag=:MainActivity          exact match
message~:"timeout \d+ms"   regex (RE2: no backreferences or lookaround; errors say so)
-tag:chatty                exclude
level:warn  age:5m  is:crash  is:stacktrace
pid:1234  tid:1234  buffer:crash          (extensions of this project)
tag:A | tag:B   (tag:A | level:error) & package:com.foo
```

The same key repeated is OR (`tag:A tag:B` = A or B); different keys are AND. Unknown keys, `&&` / `||`, unbalanced parentheses and unquoted `foo:bar` are errors.

## 中文摘要

- **工具**：`logcat_status`、`logcat_search`（Android Studio 过滤语法，严格报错）、`logcat_crashes`（Java / native / ANR 按签名分组）、`logcat_app_launches`（冷 / 温 / 热启动时间线；设备时钟跳变后，跨跳变记录的步骤只给设备时间，标注 `duration: unreliable (device clock jumped)`，不给时长）。所有时间都是设备本地时区。
- **`package` 参数**：精确的应用 id。共享 uid 的设备进程若名称不对应其中任何包（如 `android.process.acore`），其日志、崩溃和进程不归属任何包；对该 uid 的每个包，`package=` 都会包含它们，并在 `note:` 中列出这些包。过滤语法中的 `package:` 仍是 Android Studio 语义（进程名前缀）。
