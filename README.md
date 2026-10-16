# android-log-mcp

> Project goals: [docs/GOALS.md](docs/GOALS.md) · Changes and upgrade notes: [CHANGELOG.md](CHANGELOG.md) · Contributor / agent guidelines: [AGENTS.md](AGENTS.md) · Terms: [docs/glossary.md](docs/glossary.md)

An MCP (Model Context Protocol) stdio server that gives AI coding agents (MCP hosts such as Claude Code, Codex CLI, Cursor, …) **read-only** access to Android logcat. It never clears logs, never starts or stops apps (except `report --am-start`, only when you ask) and never changes device settings.

- **Background recording that starts on use.** One service process per user records devices to disk, so history survives restarts and new sessions. A device is recorded from the first log tool call that uses it, backfilled from its log buffer, and until it has gone unused for the linger time; `android-log-mcp record --for 2h` keeps recording without calls.
- **Android Studio filter syntax** (`tag:` `message~:` `-package:` `level:` `age:` `is:crash` `|` `&`, parentheses) with strict errors: a parse error gives the column and a fix instead of silently degrading to a substring match, and every response first echoes how the filter was parsed.
- **Crashes and ANRs** (Java, native/tombstone, ANR) grouped by signature, with ANR traces and tombstones from `dumpsys dropbox` where available.
- **App launch timelines** (COLD / WARM / HOT, TTID / TTFD) from `START u0`, `am_proc_start`, `am_on_*_called` (API ≤ 29) / `wm_on_*_called` (API ≥ 30), `Displayed` and `Fully drawn`.
- **Diagnostic reports**: `android-log-mcp report` packs logs and diagnostics into a zip with sensitive data redacted by default.

Prebuilt binaries for Linux, macOS and Windows (x64, arm64). Requires `adb`.

**中文摘要：** android-log-mcp 是一个 MCP stdio server，让 AI agent（MCP 宿主，如 Claude Code / Codex / Cursor 等）**只读**地访问 Android logcat：不清空日志、不启动或停止应用（`report --am-start` 除外，且只在你明确要求时）、不改设备设置。
安装：`npx -y android-log-mcp`（Windows 推荐原生二进制）。工具说明见 [docs/tools.md](docs/tools.md)，服务进程、数据与环境变量见 [docs/reference.md](docs/reference.md)，兼容性见 [docs/compatibility.md](docs/compatibility.md)，升级说明只在 [CHANGELOG.md](CHANGELOG.md)。

## Install and configure

Prerequisites: adb (Android SDK platform-tools); Node.js ≥ 16 for npx (not needed for the native binary). Windows needs **Windows 10 1803 / Windows Server 2019 or later**. Go is not needed. adb is found via `ANDROID_LOG_MCP_ADB`, then `PATH`, then the usual SDK locations. Details, both install paths and the upgrade procedure: [docs/reference.md](docs/reference.md#install-and-configure-full).

### A. npx (recommended on macOS and Linux)

```bash
npx -y android-log-mcp --version                 # check that it runs
npx -y android-log-mcp call logcat_status '{}'   # debugging: call one tool and print the result
```

**macOS / Linux**

Codex (`~/.codex/config.toml`; the Codex CLI and IDE extension share it):

```toml
[mcp_servers.android-log]
command = "npx"
args = ["-y", "android-log-mcp"]
# env = { ANDROID_LOG_MCP_ADB = "/path/to/adb" }
```

Cursor (`~/.cursor/mcp.json` or the project's `.cursor/mcp.json`) and Claude Code (the project's `.mcp.json`):

```json
{
  "mcpServers": {
    "android-log": { "command": "npx", "args": ["-y", "android-log-mcp"] }
  }
}
```

or `claude mcp add android-log -- npx -y android-log-mcp`.

**Windows:** the same configuration works (`npx` resolves to `npx.cmd`); if the host cannot start it, use `"command": "cmd", "args": ["/c", "npx", "-y", "android-log-mcp"]`, or use B.

### B. Native binary (recommended on Windows)

```bash
npx -y android-log-mcp@latest install    # copies the binary to a fixed path and prints it
```

Without Node.js, download the tarball from [GitHub Releases](https://github.com/s1crab/android-log-mcp/releases). Then point the MCP host at the absolute path with no args, for example Cursor / Claude Code on Windows:

```json
{
  "mcpServers": {
    "android-log": { "command": "C:\\Users\\<you>\\AppData\\Local\\android-log-mcp\\exe\\android-log-mcp.exe", "args": [] }
  }
}
```

Paths for every OS, Codex and `claude mcp add` variants, and why B is better on Windows: [docs/reference.md](docs/reference.md#b-without-npx-the-native-binary-recommended-on-windows).

## Tools

| Tool | Purpose |
|---|---|
| `logcat_status` | Start recording a device; devices, history range, gaps, service process, a "now" cursor; with `package`, install state and device processes |
| `logcat_search` | Search logs with the filter syntax below: lines or a summary; `after`/`before` cursors, `wait_ms` |
| `logcat_crashes` | Java, native and ANR crashes grouped by signature; full detail with `id` |
| `logcat_app_launches` | Launch timeline (COLD / WARM / HOT) with TTID / TTFD |

All times in `from` / `to` / `age:` are **device time**; output is in the device's time zone. Parameters, time semantics, the `package` parameter and several devices: [docs/tools.md](docs/tools.md). Reproducing a bug: call `logcat_status` for a `cursor`, let the user reproduce, then `logcat_search(after=cursor, wait_ms=20000)`. `cursor` is the newest stored line at the time of the call; `after=cursor` is exclusive (only lines stored after it) and `before=cursor` pages older (only lines before it).

### Filter syntax

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

Precedence: `|` binds tighter than the space (implicit AND), so `tag:A level:error | tag:B` is `tag:A & (level:error | tag:B)`; write `(tag:A & level:error) | tag:B` to OR whole groups. `OR` / `AND` are not operators: a bare `OR` is a message keyword (use `|` and `&`). A value with spaces needs quotes: `message:'rate limit'` (single or double); unquoted, `message:rate limit` is `message:rate` AND a line containing `limit`.

## FAQ

- **Where is the data, and how long is it kept?** In `<user cache dir>/android-log-mcp` (`android-log-mcp root`); 1 day and 512 MiB per device by default. See [docs/reference.md](docs/reference.md#data-layout-and-limits).
- **Nothing is recorded.** Recording starts with the first log tool call that uses a device; to keep recording without calls, run `android-log-mcp record --for 2h`. See [docs/reference.md](docs/reference.md#recording-pins-record-and-record_for).
- **The MCP host cannot start `npx` on Windows.** Use `cmd /c npx …` or the native binary (B).
- **Send diagnostics to a maintainer.** `android-log-mcp report` writes a zip with sensitive data redacted: [docs/reference.md](docs/reference.md#diagnostic-reports-and-privacy).
- **What is verified, and known limitations?** [docs/compatibility.md](docs/compatibility.md).
- **Upgrading.** Upgrade notes are only in the [CHANGELOG](CHANGELOG.md); look for the **Breaking** entries.

## License

MIT © s1crab. `server/third_party_ref/` is Apache-2.0 reference source and is not compiled.
