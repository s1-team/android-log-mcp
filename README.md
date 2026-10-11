# android-log-mcp

> Project goals: [docs/GOALS.md](docs/GOALS.md) · Changes: [CHANGELOG.md](CHANGELOG.md) · Contributor / agent guidelines: [AGENTS.md](AGENTS.md) · Terms: [docs/glossary.md](docs/glossary.md) · 中文说明见文末「中文摘要」

An MCP (Model Context Protocol) stdio server that gives AI coding agents (MCP hosts such as Claude Code, Codex CLI, Cursor, …) **read-only** access to Android logcat. It never clears logs, never starts or stops apps (except `report --am-start`, only when you ask) and never changes device settings.

- **Background recording that starts on use.** One service process per user records devices to disk, so history survives restarts and new sessions. A device is recorded from the first log tool call that uses it, backfilled from its log buffer, and until it has gone unused for the linger time; `android-log-mcp record --for 2h` keeps recording without calls.
- **Android Studio filter syntax** (`tag:` `message~:` `-package:` `level:` `age:` `is:crash` `|` `&`, parentheses) with strict errors: a parse error gives the column and a fix instead of silently degrading to a substring match, and every response first echoes how the filter was parsed.
- **Crashes and ANRs** (Java, native/tombstone, ANR) grouped by signature, with ANR traces and tombstones from `dumpsys dropbox` where available.
- **App launch timelines** (COLD / WARM / HOT, TTID / TTFD) from `START u0`, `am_proc_start`, `am_on_*_called` (API ≤ 29) / `wm_on_*_called` (API ≥ 30), `Displayed` and `Fully drawn`.
- **Diagnostic reports**: `android-log-mcp report` packs logs and diagnostics into a zip with sensitive data redacted by default.

Prebuilt binaries for Linux, macOS and Windows (x64, arm64). Requires `adb`.

## Contents

- [Install and configure](#install-and-configure) ([A. npx](#a-using-npx) · [B. native binary](#b-without-npx-the-native-binary-recommended-on-windows)) · [Tools](#tools) · [Command line](#command-line)
- [Service process](#service-process) · [Data root and `set-root`](#data-root-and-set-root) · [Data layout and limits](#data-layout-and-limits) · [Environment variables](#environment-variables)
- [Diagnostic reports and privacy](#diagnostic-reports-and-privacy) · [Upgrading](#upgrading) · [Known limitations](#known-limitations) · [Compatibility](#compatibility)
- [What has been verified](#what-has-been-verified) · [Repository layout](#repository-layout) · [Releasing](#releasing-maintainers) · [中文摘要](#中文摘要)

## Install and configure

Prerequisites: adb (Android SDK platform-tools). Windows needs **Windows 10 1803 / Windows Server 2019 or later**. Go is not needed.

There are two ways to run it. Both use the same service process and data.

| Your OS | Recommended | Why |
|---|---|---|
| macOS, Linux | **A. npx** (B works too) | One line, no install step; `npx` starts `node` and then the binary, which is cheap here |
| Windows | **B. native binary** | Avoids `npx.cmd` → `cmd.exe` → `node` → binary for every MCP session; A still works |
| No Node.js | **B**, from a GitHub Release asset | No npm at all |

adb is looked up in this order: `ANDROID_LOG_MCP_ADB` → `PATH` → `platform-tools` under `$ANDROID_HOME` / `$ANDROID_SDK_ROOT` → the usual SDK locations (macOS `~/Library/Android/sdk`, Linux `~/Android/Sdk`, Windows `%LOCALAPPDATA%\Android\Sdk`). Set `ANDROID_LOG_MCP_ADB` in the MCP host's `env` for android-log-mcp if adb is elsewhere.

Whichever way the MCP host starts the MCP process, the service process always runs from its own copy under `<root>/bin/`, started directly (never through node, npx or cmd.exe) and without a console window.

### A. Using npx

Needs Node.js ≥ 16 (only to download and launch). The npm package is a tiny Node launcher; the program is a prebuilt Go binary in the optional dependency `android-log-mcp-<os>-<cpu>` (linux/darwin/win32 × x64/arm64), which the launcher starts directly (no shell) with stdio inherited and `windowsHide`. If the platform package is missing (e.g. installed with `--omit=optional`) it says so; `ANDROID_LOG_MCP_BINARY` can point at a binary you built yourself.

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

**Windows**

The same configuration works, with `npx` resolving to `npx.cmd`. If the MCP host cannot start `npx` (some MCP hosts do not run `.cmd` files), go through `cmd`:

```toml
[mcp_servers.android-log]
command = "cmd"
args = ["/c", "npx", "-y", "android-log-mcp"]
```

```json
{ "mcpServers": { "android-log": { "command": "cmd", "args": ["/c", "npx", "-y", "android-log-mcp"] } } }
```

Trade-offs on Windows: every MCP session runs a chain of `npx.cmd` → `cmd.exe` → `node` → the binary, i.e. three extra processes between the MCP host and the MCP process and a slower start (npx resolves the package first). Whether `cmd.exe` gets a console window depends on how the MCP host starts it. Path B starts the binary directly.

**Upgrading:** `npx -y android-log-mcp` uses the newest version npx has cached and may not check for a new one every time. To upgrade, pin the version in `args` (`"android-log-mcp@0.5.2"`) and change it on upgrades, or use `android-log-mcp@latest`; then restart the MCP host. The new MCP process replaces an older running service process by itself.

### B. Without npx: the native binary (recommended on Windows)

**Get the binary.** If you have Node.js, let android-log-mcp copy itself to a fixed path:

```bash
npx -y android-log-mcp@latest install
```

It prints the path: `<user cache dir>/android-log-mcp/exe/android-log-mcp[.exe]`, i.e.

| OS | Path printed by `install` |
|---|---|
| Windows | `C:\Users\<you>\AppData\Local\android-log-mcp\exe\android-log-mcp.exe` (`%LOCALAPPDATA%\android-log-mcp\exe\android-log-mcp.exe`) |
| macOS | `/Users/<you>/Library/Caches/android-log-mcp/exe/android-log-mcp` |
| Linux | `/home/<you>/.cache/android-log-mcp/exe/android-log-mcp` (`$XDG_CACHE_HOME` if set) |

`android-log-mcp root --json` also shows it as `exe`. Without Node.js, download the platform tarball from the [GitHub Releases](https://github.com/s1crab/android-log-mcp/releases) (`android-log-mcp-win32-x64-<version>.tgz`, `-darwin-arm64-`, `-linux-x64-`, …) and extract `package/bin/android-log-mcp[.exe]` (`tar -xzf <file>.tgz`, also built into Windows 10 1803+); run `<extracted binary> install` to put it at the fixed path, or keep it wherever you like. Building from source (Go 1.24+): `cd server && go build -o android-log-mcp ./cmd/android-log-mcp`.

**Configure the MCP host** with the absolute path and no args.

Windows, Codex (`%USERPROFILE%\.codex\config.toml`; a TOML literal string `'…'` needs no escaping):

```toml
[mcp_servers.android-log]
command = 'C:\Users\<you>\AppData\Local\android-log-mcp\exe\android-log-mcp.exe'
args = []
```

Windows, Cursor (`.cursor\mcp.json`) and Claude Code (`.mcp.json`); JSON needs doubled backslashes:

```json
{
  "mcpServers": {
    "android-log": { "command": "C:\\Users\\<you>\\AppData\\Local\\android-log-mcp\\exe\\android-log-mcp.exe", "args": [] }
  }
}
```

or, in cmd.exe, `claude mcp add android-log -- "%LOCALAPPDATA%\android-log-mcp\exe\android-log-mcp.exe"`.

macOS / Linux, Codex:

```toml
[mcp_servers.android-log]
command = "/Users/<you>/Library/Caches/android-log-mcp/exe/android-log-mcp"   # Linux: /home/<you>/.cache/android-log-mcp/exe/android-log-mcp
args = []
```

macOS / Linux, Cursor and Claude Code:

```json
{
  "mcpServers": {
    "android-log": { "command": "/Users/<you>/Library/Caches/android-log-mcp/exe/android-log-mcp", "args": [] }
  }
}
```

or `claude mcp add android-log -- ~/Library/Caches/android-log-mcp/exe/android-log-mcp` (Linux: `~/.cache/…`).

**Upgrading:** run `npx -y android-log-mcp@latest install` again (or `<new binary> install`), then restart the MCP host. The fixed path stays the same, so the configuration does not change. A copy that MCP processes are running is replaced safely (on Windows the running file is renamed aside and removed by a later `install` once it has stopped), and the new MCP process replaces the older running service process. Check with `android-log-mcp --version` and `android-log-mcp service status`.

The `exe/` directory holds one binary (about 11 MB), plus renamed copies only while an MCP process still runs them. It lives in the fixed control directory, so `set-root` never moves or deletes it, and it is not counted in the data cap. Delete it to uninstall.

## Tools

| Tool | Purpose | Main parameters |
|---|---|---|
| `logcat_status` | Starts recording the device (or keeps it recording until now + `record_for`, at most `24h`; `"0"` stops). Devices, recording and backfill state, history range and gaps, the service process (pid, version, root, linger, pins), and a "now" cursor; with `package`, install state and running device processes. The lines that rarely change (`root`, `linger`, `lifetime`, `service log`, `adb`, `store`) are shown on the first call of a session and again when they change; `verbose=true` always shows them | `device` `package` `record_for` `verbose` |
| `logcat_search` | Search logs. `view="lines"` (default) returns the newest `limit` lines (default 50); `view="summary"` returns counts by level, tag and device process, repeated messages, crash count and latest errors. Java stacks inside a line are short by default (`stack="short"`): each exception and `Caused by` keeps its first 3 frames, the rest is `↳ K more frames`, a stack already shown in the response is `↳ same K frames as <time>`, and a line holding a stack is at most 2,000 chars; `stack="full"` shows every frame | `filter` `package` `tags` `min_level` `from` `to` `limit` `context` `before` `after` `wait_ms` `stack` |
| `logcat_crashes` | Crashes and ANRs grouped by signature. With a single match or an `id`: full details (stack or tombstone, the pid's last 40 lines before the crash; for ANRs the main-thread trace and lock holders) | `package` `kinds` (java/native/anr) `from` `to` `id` `limit` |
| `logcat_app_launches` | Launch timeline (COLD/WARM/HOT/UNKNOWN), last 30 min by default. Each `+N ms` is when a line was logged relative to the launch request; TTID / TTFD on their own line with their source (`Displayed` or `*_activity_launch_time`) | `package` `from` `to` `limit` |

- **Device time:** `from` / `to` / `age:` are device time (the Android device's clock, as printed in lines): relative (`30m`, `2h` = device now minus that; such a window ends at device now + 2 s), device-local (`14:05`, `2026-10-10 14:05:00`), ISO 8601 and epoch. Output is in the **device's time zone**; each response shows device now, host now and the clock offset. Lines stay in arrival order; when the device clock is changed (auto_time, a manual set, expiry testing) the output marks the clock jump with matches on each side.
- **Reproducing a bug:** call `logcat_status` for a `cursor`, let the user reproduce, then call `logcat_search(after=cursor, wait_ms=20000)`. If `logcat_status` says "backfill in progress", call it again: it returns no cursor until the device's log buffer has been read.
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

## Command line

| Command | What it does |
|---|---|
| `android-log-mcp` | The MCP process (MCP stdio server; records nothing and forwards every call to the service process, which it starts when needed) |
| `android-log-mcp call TOOL [JSON]` | Call one tool through the service and print the result (like an MCP call, it starts recording the device and waits up to 10 s for the backfill) |
| `android-log-mcp record [--device X]... --for 2h` / `--stop` / (no flag) | Keep recording until now + the duration without tool calls or connected MCP / CLI processes (a pin), stop now, or show what is recorded (see [below](#recording-pins-record-and-record_for)); `--json` for scripts |
| `android-log-mcp report [flags]` | Diagnostic zip (see [below](#diagnostic-reports-and-privacy)); `report -h` lists the flags |
| `android-log-mcp service [status [--json] \| stop]` | Show or stop the service process; without arguments, run it in the foreground |
| `android-log-mcp install [--json]` | Copy this binary to a fixed path (`<user cache dir>/android-log-mcp/exe/`) and print it, for MCP hosts that run the binary directly (recommended on Windows) |
| `android-log-mcp root [--json]` | Print the data root (and the control dir, config file, socket and running service) |
| `android-log-mcp set-root DIR --keep-old\|--delete-old` | Move the data root for every MCP and CLI process (see [below](#data-root-and-set-root)) |
| `android-log-mcp compat [--serial S] [--json\|--raw]` | Read-only probe of a device for the logcat / ps / pm / dumpsys features this tool needs, evaluated with the recorder's own parsers. Exit 0 = supported (possibly degraded), 1 = a required feature is missing, 2 = usage or adb error |
| `android-log-mcp --offline DIR [call …]` | Open a dataset read-only, e.g. an unzipped report's `store/`: no adb, no service, never records, prunes or creates files |

## Service process

```
MCP host ─stdio─ android-log-mcp (MCP process) ─┐
MCP host ─stdio─ android-log-mcp (MCP process) ─┼─ AF_UNIX socket ─ android-log-mcp service (one per user)
android-log-mcp call / report / record / set-root ┘                    records devices in use, answers every tool call
```

- **One service process per user.** It holds `run/service.lock` in the control directory `<user cache dir>/android-log-mcp`, records devices into the data root and answers all tool calls. MCP and CLI processes never open data files (except `--offline`), so there is one cache, one writer and one sweep however many MCP processes are running.
- **Started on demand.** An MCP or CLI process that finds no service process copies its own binary to `<root>/bin/<version>-<hash>/` (so the npx cache can change while the service runs; the running and at most one other version are kept) and starts it detached. If that is impossible, the MCP process runs the service **in its own process** and serves the same socket for the other MCP and CLI processes; when it exits, they start a new one. After 3 service process exits within a minute an MCP process stops respawning and runs it in-process.
- **Recording starts on use.** An idle service runs no adb at all, not even `adb track-devices`. A device starts being recorded on the first log tool call that uses it (`logcat_status`, `logcat_search`, `logcat_crashes`, `logcat_app_launches`, also through `call`), on `report` and on `record --for`. A call without `device` uses the single connected device; with several connected it starts none and lists them; more than `ANDROID_LOG_MCP_MAX_DEVICES` recorded devices is an error naming the variable.
- **Backfill.** A newly started device is first read from its log buffer: the whole buffer when nothing of this boot is stored, else from the last stored line (`logcat -T`) with deduplication. Before starting, the service reads the buffer's last line (`logcat -d -t 50`); the backfill has caught up once that line is stored. Every call waits for that at most 10 s; if it has not caught up, the answer starts with "backfill in progress (1,423 of ~27,000 lines; earliest …)" (the total is estimated from the number of entries logd holds, `logcat -S`; after `-T` only the lines so far are shown) and `logcat_status` returns no cursor and asks to call again. While backfilling, the service runs no other adb command on that device (`ps`, `pm`, `dumpsys dropbox`); crash details from DropBox are fetched once it has caught up. If the device clock went back since the last stored line (e.g. an emulator snapshot), or a clock jump lies among the stored lines that may still be in the device buffer, the whole buffer is read without `-T`, already stored lines are skipped (compared with counts, so a line logged twice is kept twice), and `logcat_status` says so.
- **Per-device idle stop (linger).** A device stops being recorded when it has had no log tool call for `max(10 min, 2 × the device's log buffer span)`, at most 1 h (10 min until the span is measured). A call that waits (`wait_ms`) counts as use while it waits. Starting or stopping MCP processes plays no role. `ANDROID_LOG_MCP_LINGER` overrides it (`30m`; `0` = stop after each call; `forever`). The service process reads it when it starts, so set it in the MCP host's `env` for android-log-mcp. When the device is used again, recording resumes with `logcat -T` and deduplication; anything the buffer dropped meanwhile is marked as a gap.
- **Service lifetime.** The service process never exits while MCP or CLI processes are connected (MCP processes and running `call` / `report` / `record --for` CLI processes; idle, it runs no adb). It exits 30 s after none is connected, there is no active pin and no recorded device; short queries such as `service status`, `root` or `record` (state) do not keep it running. A call that finds no device to record stops `adb track-devices` again 30 s later.
- **Crash recovery.** If the service process dies, every MCP and CLI process reconnects on its next call, one of them starts a new service process and the call is retried (all tools are read-only). With an active pin, a connected MCP process restarts it immediately. Devices started by tool calls are not carried over a crash or a version handoff (the next call starts them again); pinned devices are. Recording resumes with `logcat -T` and deduplication; anything the device buffer dropped meanwhile is marked as a gap.
- **Versions.** An MCP or CLI process replaces the service process only if the service process is **older**: it asks it to hand off, waits for it to stop and starts its own version. A newer service process is used as is. Both sides also carry a protocol version: an older MCP or CLI process that the newer service process still supports works normally (`logcat_status` says so); an incompatible one fails with an error asking you to upgrade and never replaces the newer service process.
- **Visibility.** The MCP process's stderr names the service process it uses (pid, version, protocol, root, recording). `logcat_status` starts with `service: … pid N, version V, protocol P` followed by `root:`, `linger:`, pin and `service log:` lines (on later calls of the same MCP process, unchanged `root:`, `linger:`, `lifetime:`, `service log:`, `adb:` and `store:` lines are replaced by one `unchanged since shown earlier in this session: …` line; `verbose=true` shows them); `service status` and `record` show each device as recording or idle, its backfill progress, idle deadline and pin.
- **Local only.** The socket lives in `<control dir>/run/` (0700) and is 0600 on Linux/macOS; the service also checks that the peer runs as the same user (`SO_PEERCRED` / `LOCAL_PEERCRED`). On Windows the run directory and socket get a protected ACL for the current user and SYSTEM only. No TCP and no network listener. Very long socket paths fall back to `/tmp/alm-<uid>/` (`%TEMP%\alm-<hash>\` on Windows).
- **Service log:** `<root>/log/service.log` (1 MiB plus one rotated copy) and `log/crash.log` for Go runtime crashes; both count towards the total size cap.
- **adb:** the service process runs `adb start-server` outside its own process group and never kills the adb server; its adb children (track-devices, logcat, shell) end with it, and with the last recorded device.

### Recording pins: `record` and `record_for`

To keep a device recorded without tool calls (e.g. overnight, or while reproducing a bug by hand), pin it:

```bash
android-log-mcp record --for 2h              # the single connected device; or --device X (repeatable)
android-log-mcp record                       # what is recorded: recording/idle, backfill, idle deadline, pin
android-log-mcp record --stop                # stop now and remove the pin
```

Agents do the same with `logcat_status(record_for="30m")` (`"0"` stops).

- The deadline is `max(current deadline, now + duration)`: a shorter `--for` never shortens it. Durations need a unit (`30m`, `2h`, `1h30m`); at most `24h`, longer is an error. The usual size and disk limits apply.
- `record --for` starts the service process detached when none runs (never inside the CLI process) and returns without waiting for the backfill. `--stop` stops recording at once.
- Pins are stored in `<control dir>/run/record.json` (format `"v": 1`, written atomically), so they survive a service process crash, a version handoff, `set-root` and a machine restart, and they keep the service process running until the deadline (it is started again by the next MCP process, CLI process or `record`). Deadlines more than 24 h ahead are cut to 24 h with a warning; a file with an unknown format version is ignored, never overwritten, and `record --for` refuses until it is removed.
- **Crash protection.** If the service crashes 3 times within a minute while a pin is active, the pin is suspended (`suspended: crash-loop`). Independently, a pin allows at most 10 automatic restarts per service version (`suspended: crash-budget`). Both are stored, shown with the last error by `record`, `service status` and `logcat_status`, and cleared by a new `record --for`; a newer version taking over gets a fresh budget.

## Data root and `set-root`

The data root is `<user cache dir>/android-log-mcp` by default (Linux `~/.cache/android-log-mcp`, macOS `~/Library/Caches/android-log-mcp`, Windows `%LocalAppData%\android-log-mcp`). There is no per-MCP-host setting: every MCP process, CLI process and the service process use the same root.

```bash
android-log-mcp root [--json]                     # where the data is
android-log-mcp set-root /data/alm --keep-old     # move; leave the old root as it is
android-log-mcp set-root /data/alm --delete-old   # move; remove our directories from the old root
```

- Exactly one of `--keep-old` / `--delete-old` is required (no prompts). `DIR` must be absolute, writable, new or empty (or an earlier android-log-mcp root), and must not be the current root, inside it, or a parent of it.
- The running service takes `run/config.lock`, waits for running tool calls, stops recording, carries each device's state into the new root (seq numbers and the `logcat -T` resume point, so recording continues without a gap or duplicates), writes `<control dir>/config.json` atomically and resumes recording there. **Any failure before that commit is rolled back** and the old root stays in use. With no service running, the command does the switch itself (it does not start a service).
- `--delete-old` removes only `v<N>/`, `tmp/`, `bin/`, `log/` (and `run/` outside the control directory), then the old root if it is empty. What cannot be removed (for example an open file on Windows, which always includes the running service's own binary under the old root's `bin/`) is reported with exit status 3 and retried when the service next starts.
- **History recorded before a switch stays in the old root and is not queried.** Open it with `--offline <old root>` if you kept it.
- The control directory (socket, locks, `config.json`) never moves.

## Data layout and limits

```
<control dir> = <user cache dir>/android-log-mcp   (fixed; also the default data root)
  run/                     0700: service.lock, service.json, service.sock, spawn.lock, config.lock
  config.json              {"root": "<dir>"}, written by set-root (absent = the default root)
  exe/android-log-mcp[.exe]  written only by `android-log-mcp install` (fixed path for MCP hosts)
<root>/v1/<device-id>/     device data, format 1: seg-*.jsonl (16 MiB segments), meta.json, crashes.jsonl, procs.jsonl, packages.json
<root>/tmp/                scratch files, removed after 1 hour
<root>/bin/<version>-<hash>/android-log-mcp   service binaries: the running one and at most one other
<root>/log/                service.log (1 MiB + one rotated copy), crash.log
```

- **Device IDs are stable:** `ro.serialno` for a physical device (the same over USB and Wi-Fi), `avd-<name>` for an emulator. Two devices online at once that resolve to the same ID are filed separately (`<id>~<adb serial>`) and `logcat_status` explains it.
- Anything else under the root (pre-0.4 `<serial>/` directories, `legacy/`, …) is ignored: not read, not counted, not deleted. A leftover `owners/` directory from 0.4 development builds is removed.
- **Retention:** 1 day (`ANDROID_LOG_MCP_MAX_AGE`) and 512 MiB per device (`ANDROID_LOG_MCP_MAX_DEVICE_MB`); the oldest segments go first. A segment is deleted once its newest line is past retention, and an active segment spans at most a quarter of the retention, so logs are kept for at most about 1.25 × the retention. Crash, DropBox and ended device-process records follow the same retention. DropBox entries dated more than the retention before device now are not imported.
- **Total cap:** 2 GiB for everything the tool manages under the root (all devices and format versions, `tmp/`, `run/`, `bin/`, `log/`; `ANDROID_LOG_MCP_MAX_TOTAL_MB`). The oldest files go first; each current-format device keeps its newest segment.
- **Sweep** (in the service, at start and every minute; never with `--offline`): device directories that are not being recorded lose expired segments and are removed when nothing in them is newer; then the total cap applies. Directories whose `writer.lock` is held by another android-log-mcp process (e.g. an older version) are left alone. Only known file names are deleted.
- **Low disk space:** below 512 MiB free (`ANDROID_LOG_MCP_MIN_FREE_MB`) recording pauses and a gap is recorded; it resumes with `logcat -T` once twice that is free. `logcat_status` shows why.
- **Memory:** per device, the newest 20,000 lines (at most about 16 MiB) stay in memory; older history is read from disk in blocks into a 32 MiB LRU cache, released after 2 minutes without queries. `logcat_status` shows the current usage.
- Aux files (`procs.jsonl`, `crashes.jsonl`) are compacted about 30 s after start and then every 10 minutes.

## Environment variables

Set them in the MCP host's `env` for android-log-mcp; recording settings are read by the service process when it starts.

| Variable | Meaning |
|---|---|
| `ANDROID_LOG_MCP_ADB` | Path to adb |
| `ANDROID_LOG_MCP_BINARY` | npm launcher only: use this binary instead of the platform package |
| `ANDROID_LOG_MCP_LINGER` | How long a device keeps being recorded after its last log tool call: a duration (`30m`; a bare number is seconds), `0` (stop after each call) or `forever`. Default `max(10m, 2 × the device's log buffer span)`, at most `1h` |
| `ANDROID_LOG_MCP_CALL_WAIT` | Deprecated and ignored (a note on stderr): calls wait for the backfill themselves, up to 10 s |
| `ANDROID_LOG_MCP_MAX_DEVICES` | Devices recorded at once (default 4); starting one more is an error |
| `ANDROID_LOG_MCP_MAX_AGE` | Retention, default `1d`: a Go duration (`72h`, `90m`) or days (`1d`, `7d`, `1.5d`). Invalid or non-positive values log a warning and use the default. `logcat_status` shows the effective value |
| `ANDROID_LOG_MCP_MAX_DEVICE_MB` | Per-device cap in MiB (default 512) |
| `ANDROID_LOG_MCP_MAX_TOTAL_MB` | Cap for everything under the root in MiB (default 2048; `0` = no cap) |
| `ANDROID_LOG_MCP_MIN_FREE_MB` | Pause recording below this much free disk space in MiB (default 512; `0` = off) |
| `ANDROID_LOG_MCP_MEM_RECORDS` | Newest lines kept in memory per device (default 20000) |
| `ANDROID_LOG_MCP_MEM_MB` | Byte cap of that in-memory window in MiB (estimated; default 16) |
| `ANDROID_LOG_MCP_HISTORY_CACHE_MB` | Per-device cache of decoded history blocks in MiB (default 32, LRU over ~512 KiB blocks) |
| `ANDROID_LOG_MCP_IDLE_RELEASE` | Release the history cache after this long without queries (default `2m`; `0` = never) |

`ANDROID_LOG_MCP_HOME` and `--home` were removed in 0.4.0; if either is set, the program exits with an error saying so.

## Diagnostic reports and privacy

To send a problem to a maintainer, pack the diagnostics into a zip. **Sensitive values are redacted by default.**

```bash
npx -y android-log-mcp report                                   # every recorded device, last 2 hours
npx -y android-log-mcp report --serial emulator-5556 --package com.foo \
    --since 30m --note "crashes after tapping Login" -o foo-crash.zip
npx -y android-log-mcp report --from "2026-10-10 14:00" --to "2026-10-10 14:30" --note @repro.txt
npx -y android-log-mcp report --package com.foo --am-start com.foo/.MainActivity   # LAUNCHES the app, see below
```

| Flag | Meaning |
|---|---|
| `--serial S` | Only this device (repeatable); default every device with recorded data |
| `--since 2h` / `--from` / `--to` | Window in device-local time (same syntax as the tools). Default last 2 hours; no `--to` = up to the newest line |
| `--package P` | Only this app's lines (by package / device process name), system lines that name it (`START u0`, `am_proc_start`, `Displayed`, …), and its crashes, device processes and DropBox entries |
| `--max-size 50MB` | Cap on uncompressed log data; the oldest lines are dropped first (counted in `manifest.json` and marked as a "report size cap" gap) |
| `--note TEXT` / `--note @FILE` | Repro steps or description, stored as `note.txt` (also redacted) |
| `--am-start PKG/Activity` | Run `am start -W -n` and record its output. **This launches the app**, so it only runs when you ask; it then waits up to 8 s for the launch logs to be recorded |
| `--redact-extra REGEX` | Additional redaction rule (RE2, repeatable; group 1 if present, else the whole match), type `custom` |
| `--no-redact` | No redaction (the manifest warns) |
| `--keep-private-ip` | Keep private addresses (10/8, 172.16/12, 192.168/16, link-local, ULA) |
| `--keep-domain D` | Keep domain `D` and its subdomains (repeatable), e.g. your own API host; not written to the manifest |
| `--no-adb` | Do not use adb: no live device info or DropBox |
| `-o FILE` | Output (default `./android-log-mcp-report-<time>.zip`; the path goes to stdout, a summary to stderr) |

The service process builds the report (it has the data and adb); with no service process running, the CLI process starts it detached. The service process starts recording the requested devices (with `--serial`, else the single connected one) and backfills their buffers first, waiting at most 10 s; if that is not enough, the manifest warns "backfill incomplete". `--no-adb` and `--offline DIR` use only what is stored. It only reads the data and, apart from `--am-start`, only runs `getprop`, `date`, `cat /proc/sys/kernel/random/boot_id` and `dumpsys dropbox --print` on the device.

| Path in the zip | Content |
|---|---|
| `manifest.json` | Version, time, parameters (values of `--note` / `--redact-extra` / `--keep-domain` omitted), per-device window and line count, file sizes, trimming, hits and distinct values per redaction rule, warnings |
| `note.txt` | The `--note` text |
| `store/v1/<device-id>/` | The recorded store, trimmed to the window/package and renumbered 1..N; load it with `android-log-mcp --offline <unzipped>/store` |
| `devices/<serial>/device.json` | Fingerprint, model, manufacturer, Android version, SDK, time zone and offset, boot_id, device–host clock difference (from the recording if the device is offline, with the source noted) |
| `devices/<serial>/logcat/<boot>.txt` | The same lines as `logcat -v threadtime -v epoch -v uid -v usec -v printable` text, one file per boot |
| `devices/<serial>/dropbox/<tag>.txt` | DropBox entries in the window (`data_app_crash`, `data_app_native_crash`, `data_app_anr`, `SYSTEM_TOMBSTONE`, `system_app_*`) |
| `devices/<serial>/am-start.txt` | `--am-start` output |

```bash
unzip foo-crash.zip -d r
android-log-mcp --offline r/store call logcat_crashes '{"package":"com.foo"}'
```

### What is redacted

Values are replaced by `[REDACTED:<type>#n]`. The same original value gets the same number across the whole zip (logs, crashes, DropBox, note), so lines can still be correlated; numbers come from a random HMAC key that is used once and discarded, so originals cannot be recovered.

| Type | Rule |
|---|---|
| `token` | `Authorization:` / `Proxy-Authorization:` headers, values after `Bearer` / `Basic` |
| `jwt` | `eyJ….eyJ….…` JWTs |
| `secret` | Common key prefixes: `ghp_` `gho_` `ghu_` `ghs_` `ghr_` `github_pat_` `npm_` `sk-` (incl. `sk-proj-`) `AKIA` / `ASIA` `AIza` `xox?-` `glpat-`, PEM private-key headers |
| `credential` | `key=value`, `key: value` and JSON fields whose key contains api_key / secret / token / password / passwd / access_key / private_key / credential, and URL query parameters with those names |
| `url_param` | URL query `sig` `signature` `code` `key` `auth` `ticket` `sid` `session` `otp` `nonce` and AWS / GCS signature parameters |
| `password` | `scheme://user:password@host` |
| `cookie` | `Cookie:` / `Set-Cookie:` values |
| `email` | Email addresses |
| `phone` | Mainland China mobile numbers (11 digits, incl. `+86`) and international numbers with `+` |
| `id_card` | 18-digit PRC resident ID numbers (birth date and GB 11643 check digit) |
| `bank_card` | 13–19 digit card numbers (Luhn + issuer ranges; bare digit runs must be 15/16/19 digits and not look like counters) |
| `device_id` | IMEI, IMSI, ICCID, MEID, serial, Android ID, advertising ID (`key=value`) |
| `ipv4` / `ipv6` | IP addresses (not 127.0.0.1, ::1, 0.0.0.0); `--keep-private-ip` keeps private ones |
| `mac` | MAC addresses (not all-zero / broadcast) |
| `domain` | Host names, see below; `--keep-domain` keeps listed domains |

`domain`:
- **Hosts in URLs** of any network scheme (`https://`, `wss://`, `ftp://`, `postgres://`, …): only the host is replaced; scheme, `user:`, port, path and query stay (`GET https://api.foo.com:8443/v1/login?page=2` → `GET https://[REDACTED:domain#1]:8443/v1/login?page=2`). IP literals count as `ipv4`/`ipv6`. Non-network schemes (`content://`, `file://`, `android.resource://`, `intent:`) and package-name authorities (`app://com.foo.app:10108`) are left alone.
- **Bare host names** outside URLs (`Unable to resolve host "cdn.foo.com"`): only lower-case names with at least two labels whose last label is in a curated list of public TLDs (com, net, org, io, co, ai, app, dev, cn, uk, jp, de, …) and whose first label is not a reverse-DNS / property prefix (`com`, `org`, `android`, `java`, `ro`, `persist`, …). Package and class names, stack frames, file names (`Foo.java`, `libc.so`, `base.apk`), versions, properties and intent actions are therefore not touched. TLDs that are also common file extensions or words (`.so` `.md` `.py` `.sh` `.id` `.in` `.it` `.to` …) are not in the list, so such hosts are only redacted inside URLs.

**Not redacted:** timestamps, pid, tid, uid, level, tag, package and device process names, device properties, and package / class / file / property names in messages. Rules exclude epoch timestamps and ns counters, hex values and addresses, register and memory dumps, native and Java stack frames, version numbers, HIDL library names, `schedstat`-style number lists, and numbers whose key looks like time / size / bytes / count / duration. All rules have unit tests and are regression-tested over real API 29 logs.

**Redaction is pattern-based and cannot be complete**: custom token formats, names, postal addresses or random strings without a key are not recognized. Review the zip before sharing it, add rules with `--redact-extra` if needed, and send `--no-redact` reports only to people you trust.

## Upgrading

Full history: [CHANGELOG.md](CHANGELOG.md).

### Unreleased (breaking)

- **`logcat_search` returns 50 lines by default** instead of 200, and **Java stacks are short by default**: each exception and `Caused by` keeps its first 3 frames, and a stack already shown in the same response becomes `↳ same K frames as <time>` ([#23](https://github.com/s1crab/android-log-mcp/issues/23)). To get the previous output, pass `limit: 200` and `stack: "full"`. `logcat_crashes(id=…)` still shows a crash's whole stack.

### To 0.5.0 (breaking)

- **Recording starts on use.** A device is no longer recorded just because an MCP process is running: it starts with the first log tool call that uses it (backfilled from the device's log buffer) and stops after the linger time without calls. Lines that dropped out of the device's buffer while it was not recorded are not in the history; use `android-log-mcp record --for DURATION` to keep recording without calls.
- **The service process lifetime no longer depends on the linger.** It stays while MCP or CLI processes are connected (running no adb when idle) and exits 30 s after none is connected and there is no pin and no recorded device. `ANDROID_LOG_MCP_LINGER` now applies per device, counted from its last log tool call; `0` means stop after each call.
- **`report` starts the service** when none runs, instead of building the report itself.
- **`ANDROID_LOG_MCP_CALL_WAIT` is deprecated** and ignored with a note: remove it.
- A 0.5 MCP process takes over an older service process as before; a 0.4 MCP process keeps working with a 0.5 service process.

### To 0.4.0 (breaking)

- **Old data is neither migrated nor read.** Data lives in `<user cache dir>/android-log-mcp/v1/<device-id>/`; logs recorded by 0.3 or earlier are not visible, and 0.3 report zips cannot be loaded with `--offline`. The old `~/.cache/android-log-mcp/<serial>/` directories (e.g. `emulator-5554/`) are ignored, not deleted: remove them, and any manual backups, when you no longer need them.
- **`ANDROID_LOG_MCP_HOME` and `--home` are removed.** If either is still in an MCP host's `env` / `args` for android-log-mcp, the MCP process exits with an error saying so: remove it. Move the data root with `android-log-mcp set-root` instead.
- **One service process per user now records everything.** Recording **continues for a while after the last MCP process exits** (the linger). Nothing changes in the MCP host configuration. A still-running 0.3 MCP process uses its own old directories and does not conflict, but keeps its own recording: restart old MCP host sessions after upgrading.
- **Windows 10 1803 / Windows Server 2019 or later** is required (AF_UNIX sockets); older Windows gets a clear error.
- `--offline <dir>` is the only read-only mode (`--home DIR --offline` no longer works); the `report` flag that skips adb is `--no-adb`.
- The `owners/` directory of 0.4 development builds is no longer used and is removed automatically.

### To 0.3.0

> **Default retention changed from 7 days to 1 day.** On first start, older log segments and older crash, DropBox and ended device-process records are **deleted**. Keep the old behavior with `ANDROID_LOG_MCP_MAX_AGE=7d` in the MCP host's `env` for android-log-mcp.

## Known limitations

- **Recording starts on use and stops after the linger time.** A device is recorded from the first log tool call and until it has gone unused for the linger time (or until a pin's deadline). The next call resumes with `logcat -T <last time>` and deduplicates; if the device's ring buffer was overwritten in between, the lost part is a gap shown in `logcat_status` and in search results. Use `record --for` when you need recording without calls.
- **Slow backfill on emulators.** A TCG emulator can take tens of seconds to dump a full log buffer, so the first call there often answers "backfill in progress"; call `logcat_status` again.
- **No supervisor for pins without MCP processes.** A pinned service process that crashes while no MCP process is connected stays down until the next MCP process or a `call`, `report` or `record` CLI process starts it (the pin is kept).
- **`set-root` does not move history** (see above).
- **Small device ring buffers.** Emulator buffers default to 2 MiB each (`logcat -g`) and can wrap within minutes when busy; what existed before recording started depends on them. Older crashes can only come from DropBox (`logcat_crashes` says so).
- **No device-side filtering.** API 29 logcat has no `--uid` and accepts one `--pid`, so all buffers (main, system, crash, events) are recorded and package / pid / uid filtering happens on the host.
- **Read latency.** Lines are flushed per batch, plus a ~800 ms delay queue for pid → device process mapping, so new lines are usually queryable within about a second. `logcat_status` distinguishes a stalled recorder, a device-side backlog (e.g. TCG emulators dumping the whole buffer slowly on first `-D`) and a quiet device.
- **Device clock.** The device clock can differ from the host clock by any amount (TCG emulators run a minute or more behind; auto_time off; expiry testing). Device now is host now plus the clock offset, taken from one `date +%s.%N` read at connect and then from live lines (device time minus host arrival time); relative windows count back from it. A change of the clock offset by more than 60 s is a clock jump, marked where it falls in the output. Log timestamps themselves come from the device.
- **Time zone.** The device's `date +%z` offset is authoritative; `persist.sys.timezone` is only used as the display name when its offset agrees. All output states the offset.
- **`-v printable`** escapes non-ASCII bytes; they are restored to UTF-8 before storage (invalid sequences keep the escape). C escapes such as `\b` stay as they are.
- **Multi-line messages** are merged when consecutive lines have identical headers (time to the microsecond, pid, tid, level, tag, buffer), so two distinct messages with identical headers are merged too.
- **logcat output is not strictly time-ordered** across buffers. Search results follow recording order (seq); launch analysis sorts by time first.
- **Launch timelines.** API 29 has `am_on_*_called` without durations, API 30+ `wm_on_*_called` with durations; both are handled. COLD/WARM/HOT is inferred (`am_proc_start` → COLD; onCreate without a new device process → WARM; only onRestart/onStart/onResume → HOT, which has no `Displayed`). An intent delivered to the foreground instance (onNewIntent) is UNKNOWN, matching `am start -W`. `Displayed` can come more than 10 s after onResume on slow devices, so the search looks up to 180 s ahead.
- **Crashes.** Both `am_crash` field orders (`[pid,user,…]` on API 29, `[user,pid,…]` later) are parsed. A native crash's `am_crash("Native crash")` carries the system_server pid and is merged into the native entry. Tombstone lines come from crash_dump64 (same uid as the app) and are attributed to the app with device process `crash_dump`. The crash location is the first app frame (a library under `/data/`), else `#00`. After "crashed too many times" the system drops background broadcasts to the app until a foreground launch; this tool just shows that.
- **ANR traces and full tombstones** depend on `dumpsys dropbox`, which may be rate-limited or not readable by the shell; then the result says so and suggests `adb bugreport`. `/data/anr` and `/data/tombstones` need root and are not read.
- **Device process table** comes from `am_proc_start`, `am_proc_died`, `am_kill`, `has died`, zygote signal exits, force-stop and `ps -A -o PID,UID,NAME` every 30 s. Freshly forked `<pre-initialized>` device processes wait for their real name; short-lived debuggerd children are not treated as app processes; entries missing from a complete snapshot are closed as `gone (not in ps)` unless the stream is replaying a backlog; a new boot closes processes of the previous one. Lines from `log -t` and similar shell commands show as `?(uid shell)`.
- **Storage** is uncompressed JSONL.
- **Windows and macOS** have not yet recorded real devices with the service process (CI runs the multi-process tests there with a fake adb). Windows older than 10 1803 / Server 2019 is not supported; named pipes may be added later.

## Compatibility

Two layers: **verified** combinations were actually run; **designed, not yet verified** are handled following AOSP behavior (mostly with unit tests) but not run in that environment.

### Verified

| Area | Verified | How |
|---|---|---|
| Node / npx | Node 16 – current on Linux, Node 22 on macOS and Windows; locally Node 20 and 16.20 | CI `npx-compat`: install the packed tarballs, `npx android-log-mcp --version`, MCP stdio handshake (initialize → tools/list → logcat_status) with `--offline <empty dir>` and through the service process (`npm/test/mcp_smoke.js`) |
| Host OS | **Minimum: Windows 10 1803 / Windows Server 2019** (AF_UNIX; older Windows gets a clear error); any supported macOS / Linux. Linux x64: full use (recording, end-to-end checks). Linux x64, macOS arm64, Windows x64: `go test ./...` incl. multi-process service process tests, and the npx smoke tests | `.github/workflows/ci.yml` |
| Android | API 29 (Android 10, x86_64 emulator, TCG): end-to-end checks; `compat`: SUPPORTED (11 ok, 2 info) | `validation/mcp-v2/`, `validation/report-e2e/`, `validation/service-e2e/`, `validation/compat/api29-emulator-5556/` |
| adb | platform-tools 37.0.1 (adb 1.0.41), `track-devices` | same |

### Designed, not yet verified

| Area | Designed for | Notes |
|---|---|---|
| Node | `engines: node >=16` (the launcher only uses `child_process.spawn` and `require.resolve`) | Node 14 and older are not supported |
| Host OS / arch | npm platform packages linux / darwin / win32 × x64 / arm64 | darwin x64, linux arm64 and win32 arm64 binaries are cross-compiled and have never run; macOS and Windows have never recorded a real device |
| Android API | **Minimum API 24 (Android 7.0)**: recording needs logcat `-v uid` / `-v epoch` / `-v usec` / `-v printable` and the `crash` buffer | Only API 29 has been run. Without `ps -A -o` (API < 26) the old `ps` is used; without `pm list packages -U`, `cmd package list packages -U`, else attribution by process name only. These branches have unit tests only |
| API 30 – 35 | `wm_on_*_called` (with durations), `am_crash` as `[user,pid,…]`, `logcat --uid` | Unit tests cover both event formats and field orders (`server/internal/analysis/apilevels_test.go`, `server/internal/compat`); no API 30+ emulator could be run here (no KVM) |
| adb | platform-tools with `track-devices` | Without it, `adb devices` is polled every 2 s (not tested) |

API differences and how they are handled:

| Difference | API 29 | API 30+ | Handling |
|---|---|---|---|
| `logcat --uid` | no | newer versions | not used: all filtering on the host |
| several `--pid` | only one | — | not used |
| Activity lifecycle events | `am_on_*_called [user,class,reason]`, no duration | `wm_on_*_called [token,class,reason,duration]` | both parsed; missing durations are noted |
| `am_crash` field order | actually `[pid,user,…]` (event-log-tags says `[user,pid,…]`) | `[user,pid,…]` | pid chosen by value, both orders tested |
| `Displayed` tag | `ActivityTaskManager` | `ActivityTaskManager` | the old `ActivityManager` is accepted too |

To check another device or API level, run the read-only probe (`getprop`, `logcat -d/-g`, `ps`, `pm list`, `dumpsys dropbox` and help output; it never clears logs or starts apps):

```bash
android-log-mcp compat --serial <SERIAL>
scripts/compat-probe.sh <SERIAL> out/            # build from source and write the results to out/
```

It prints `SUPPORTED`, `SUPPORTED (degraded)` or `NOT SUPPORTED`. Results from other devices are welcome; see `validation/README.md`.

## What has been verified

- `go test ./...` on Linux, macOS and Windows, including the ported Android Studio filter parser tests, and replay tests that feed real API 29 logcat and DropBox output (`server/internal/testdata/api29/`) through the live ingest pipeline and assert the tool output (`server/internal/mcpserver/replay_test.go`).
- **Service process:** multi-process tests in CI on all three OSes with a fake adb (single instance, crash/restart, in-process fallback, version handoff both ways and the incompatible-protocol error, linger, `set-root` keep/delete/rollback/lock timeout/concurrency, the adb server survives the service process, socket/dir permissions, bounded `bin/` and service log). Live on emulator-5556 through the npm launcher (`scripts/service-e2e.sh`, `validation/service-e2e/`): two MCP processes share one service process; after `kill -9` of the service process both recover with one new service process and a line logged meanwhile is recorded; `set-root` away (`--keep-old`) and back (`--delete-old`) with `log -t` markers, each recorded exactly once across the roots; linger exit 5 s after the last MCP process exits.
- **`report`:** unit tests for every redaction rule (positive and negative), regression over all real API 29 logs (time, pid, tid, level and tag unchanged), replay of report zips; on emulator-5556 `scripts/e2e-report.sh` injects 23 fake secrets of 16 kinds plus structural negatives: all redacted, headers intact, offline load OK (`validation/report-e2e/`).
- End-to-end on emulator-5556 (Android 10 / API 29, TCG): cold and hot launches, onNewIntent, Java crash, SIGSEGV, SIGABRT, a `:remote` app process and its crash, broadcast ANR, `log -t` markers, non-ASCII text (`validation/mcp-v2/`).
- **Not verified:** the Android 11+ `wm_*` format on a device (unit tests only); a WARM launch (could not be produced); polling without `track-devices`; several devices at once; recording on Windows and macOS.

## Repository layout

| Path | Content |
|---|---|
| `server/` | Go module `github.com/s1crab/android-log-mcp/server`: sources, unit and regression tests, fixtures (`internal/testdata/api29`); see [server/README.md](server/README.md) |
| `npm/` | npm launcher and package templates; `scripts/build-npm.sh [VERSION] [--pack]` cross-compiles and assembles all packages |
| `scripts/` | `build-npm.sh`, `e2e-report.sh` (report redaction on a device), `service-e2e.sh` (service process on a device through the npm launcher), `compat-probe.sh`, `perf.sh` (performance check / baseline) |
| `perf/` | `baseline.json`: the performance baseline guarded in CI; see [docs/performance.md](docs/performance.md) |
| `validation/` | Evidence from the API 29 emulator: findings, trigger scripts, test app, actual tool output |
| `docs/` | Goals (`GOALS.md`), terminology (`glossary.md`), research and design (`design-v3.md`), performance metrics and baseline (`performance.md`) |
| `CHANGELOG.md` | Release history ([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)) |
| `AGENTS.md` | Workflow, release, versioning and safety rules for agents and contributors |
| `.github/workflows/` | CI (tests, npx compatibility, performance guard) and release (on `v*` tags) |

## Releasing (maintainers)

See [AGENTS.md](AGENTS.md) §3. In short: move the `Unreleased` entries in `CHANGELOG.md` under the new version, bump `npm/VERSION` and the default `Version` in `server/internal/mcpserver/server.go`, commit `chore: release X.Y.Z`, wait for green CI, then tag and push: `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. `release.yml` tests, cross-compiles, publishes the six platform packages and then the main package (`npm publish --provenance`, secret `NPM_TOKEN`) and creates the GitHub Release with the notes taken from the version's `CHANGELOG.md` section. Local dry run: `scripts/build-npm.sh X.Y.Z --pack` (tarballs in `npm/dist/tarballs/`).

## Third-party reference

`server/third_party_ref/` holds AOSP and Android Studio source excerpts (filter syntax, logcat format, DropBox) under Apache-2.0, used only as a reference for porting; they are not compiled.

## License

MIT © s1crab. `server/third_party_ref/` is Apache-2.0 reference source and is not compiled.

---

## 中文摘要

android-log-mcp 是一个 MCP stdio server，让 AI agent（MCP 宿主，如 Claude Code / Codex / Cursor 等）**只读**地访问 Android logcat：不清空日志、不启动或停止应用（`report --am-start` 除外，且只在你明确要求时）、不改设备设置。

- **安装与配置（两种方式）**：A. **npx**（macOS / Linux 推荐）：MCP 宿主的 command 设为 `npx`，args 为 `["-y", "android-log-mcp"]`；Windows 上每个会话会经过 `npx.cmd` → `cmd.exe` → `node` 三层额外进程（位于 MCP 宿主和 MCP 进程之间），启动较慢；升级时修改 `@版本` 或用 `@latest` 并重启 MCP 宿主。B. **直接运行原生二进制**（Windows 推荐）：运行 `npx -y android-log-mcp@latest install`，把 MCP 宿主的 command 设为它打印的绝对路径（Windows：`%LOCALAPPDATA%\android-log-mcp\exe\android-log-mcp.exe`），args 留空；没有 Node.js 时可从 GitHub Release 下载对应平台的 tgz 并解压出 `package/bin/` 下的二进制；升级时再运行一次 `install` 并重启 MCP 宿主。Windows 需要 **Windows 10 1803 / Server 2019 及以上**。
- **工具**：`logcat_status`、`logcat_search`（Android Studio 过滤语法，严格报错）、`logcat_crashes`（Java / native / ANR 按签名分组）、`logcat_app_launches`（冷 / 温 / 热启动时间线）。所有时间都是设备本地时区。
- **后台服务进程（0.4 起）**：每个用户只有一个后台服务进程负责录制设备并回答所有工具调用；MCP 宿主启动的 MCP 进程不录制，只把调用转发给后台服务进程，需要时自动启动它。后台服务进程崩溃后 MCP 进程和 CLI 进程自动重连并重启它；新版 MCP 进程会替换旧版后台服务进程，旧版 MCP 进程在协议兼容时直接使用新版后台服务进程。`android-log-mcp service status|stop` 查看或停止。只使用本机 AF_UNIX socket（仅当前用户可访问），没有网络监听。
- **按需录制（0.5 起）**：后台服务进程空闲时不运行任何 adb；某台设备第一次被日志工具调用（含 `call`、`report`、`record`）时才开始录制，并先从设备缓冲区回填（最多等 10 秒；没追上时回答以 "backfill in progress" 开头，`logcat_status` 不返回游标，请再调用一次）。每台设备在最后一次调用之后 `max(10 分钟, 2 × 该设备日志缓冲区跨度)`（最多 1 小时，`ANDROID_LOG_MCP_LINGER` 可调）停止录制；MCP 进程启动或退出不影响录制。有 MCP 进程或 CLI 进程连接时后台服务进程不退出；没有连接、没有保持录制、没有正在录制的设备时 30 秒后退出。连接多台设备且没有指定 `device` 时不启动任何设备。
- **保持录制**：`android-log-mcp record --for 2h`（不需要工具调用也持续录制，单次最多 24 小时，截止时间只会延后）、`record --stop`（立即停止）、`record`（查看状态）；agent 用 `logcat_status(record_for="30m")`。保存在 `run/record.json`，崩溃、版本切换、`set-root` 和重启后仍然有效；1 分钟内崩溃 3 次或同一版本自动重启超过 10 次会暂停（显示原因和最后的错误），新的 `record --for` 清除暂停。
- **数据根目录**：默认 `<用户缓存目录>/android-log-mcp`（`android-log-mcp root` 查看）；用 `android-log-mcp set-root <绝对路径> --keep-old|--delete-old` 为所有 MCP 进程和 CLI 进程统一切换，失败会回滚；切换前的历史留在旧目录里，不再被查询。
- **上限**：每台设备保留 1 天、最多 512 MiB；整个根目录最多 2 GiB；剩余磁盘低于 512 MiB 时暂停录制。
- **诊断包**：`android-log-mcp report` 生成 zip（没有后台服务进程时会以 detached 方式启动它并先回填），默认打码 token、密码、邮箱、手机号、身份证、银行卡、设备 ID、IP、MAC、域名等；打码基于模式，分享前请检查。
- **升级到 0.5.0（破坏性变更）**：录制改为按需开始、空闲后停止；未录制期间被设备缓冲区覆盖的日志不会出现在历史里，需要时用 `record --for`。`ANDROID_LOG_MCP_LINGER` 改为按设备、从最后一次日志工具调用开始计时；`report` 会启动后台服务进程；`ANDROID_LOG_MCP_CALL_WAIT` 已弃用并被忽略。
- **升级到 0.4.0（破坏性变更）**：旧数据不迁移、不读取（旧 `<serial>/` 目录被忽略，请手动删除）；移除 `ANDROID_LOG_MCP_HOME` 和 `--home`（仍设置时会报错退出）；开发版遗留的 `owners/` 目录会被自动删除。升级到 0.3.0 时默认保留期从 7 天改为 1 天。
- 变更历史见 [CHANGELOG.md](CHANGELOG.md)。
