# Changelog

All notable changes to android-log-mcp are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, breaking changes bump the minor version (see [AGENTS.md](AGENTS.md)). Entries marked **Breaking** need action or change behavior on upgrade; the [README](README.md#upgrading) has upgrade notes. Every user-facing change adds an entry under `[Unreleased]`; a release moves them under the new version, and the GitHub Release notes are taken from that section.

## [Unreleased]

### Changed

- **Breaking:** `logcat_search` answers are much smaller by default ([#23](https://github.com/s1crab/android-log-mcp/issues/23)). Upgrade note: pass `limit: 200` and `stack: "full"` for the previous output.
  - The default `limit` is 50 instead of 200. `logcat_search {}` on the API 29 fixture is 8.4k chars instead of 27.5k (about 2.1k tokens instead of 6.9k); the rest is one `before` cursor away, and a truncated page says that `view="summary"` (about 2k chars for any range) shows what the whole range contains.
  - Java stacks inside a line are short by default (new parameter `stack`, `"short"` or `"full"`). Each exception and each `Caused by` keeps its head and its first 3 frames; the rest becomes `↳ K more frames` (Java's `... N more` and the frames of a `Suppressed:` exception are counted there). A stack whose frames were already shown in the same response becomes `↳ same K frames as HH:MM:SS.mmm`. A line holding a stack is cut at 2,000 chars, and every `Caused by` head and its first frame are kept first. A `stacks: N shortened (M frames hidden) · stack="full" shows every frame` line says when anything was left out. Ten warnings with the same 30-frame stack and a `Caused by` now take 3.8k chars instead of 27.5k, and all ten fit (before, the 30k cap showed seven). `stack: "full"` shows the lines exactly as before.
  - The cut marker of a long line no longer suggests narrowing the query, which never showed more: `…[+N chars; lines are cut at 2,000 chars in every query, narrowing does not show more]`. A response that hits the ~30k-char cap now says so (`[output cap: only 7 of the 10 matches asked for fit in ~30k chars · …]`).
- `logcat_search` answers in milliseconds on large histories ([#20](https://github.com/s1crab/android-log-mcp/issues/20)). It used to keep counting matches for up to 1.5 s after the page was full (about 1.5 s per call on a one-day history). Now:
  - Without a filter or time window, the total is exact and comes from the stored record range, without scanning.
  - With a filter, counting stops 100 ms after the page is full. The total is then a lower bound, and the response says which span was counted and how to get an exact count: `matched ≥14,043 lines (counted the newest 21 min only, since 23:32:09; older history not counted — narrow with from/to, e.g. from="1h", for an exact count)`.
  - Every lines response has a machine-readable `count:` line: `total`, `count_exact` and, for a lower bound, `counted_from` / `counted_to`.
  - The top-tags line says when it covers only the counted matches, or only the page.
  - On a one-day history, `logcat_search {}` takes about 1 ms instead of 1.5 s, and filtered searches whose page fills quickly about 110 ms.
- `logcat_status` no longer repeats the lines that rarely change on every call ([#23](https://github.com/s1crab/android-log-mcp/issues/23)). `root:`, `linger:`, `lifetime:`, `service log:`, `adb:` and `store:` are shown on the first call of each MCP or CLI process's connection and again whenever one of them changes; otherwise they become one line, `unchanged since shown earlier in this session: root, linger, lifetime, service log, adb, store (logcat_status(verbose=true) shows them)`. This saves about 500 characters (about 35 %) on every later call. The `service:` line (pid, version, uptime, connected MCP and CLI processes), recording state, pins, devices, adb problems and the cursor are always shown. The new `verbose` parameter always shows every line. `android-log-mcp service status` is unchanged.

## [0.5.2] - 2026-10-11

### Fixed

- A device that disconnects and reconnects quickly no longer has two recording sessions running at once ([#30](https://github.com/s1crab/android-log-mcp/issues/30)). The new session starts only after the previous session's recorder stopped (at most 10 s) and its background work returned (at most 5 s, after which its adb children are killed together with their own child processes, never the adb server). A tool call right after the reconnect again waits for the new session's backfill: the previous session could end that wait early. If the previous session does not stop in time, the new session starts anyway, and the service log and `logcat_status` say so for that device ("recording session N started … before session N-1 had stopped: …").
- When a device starts recording again, the read-only view of its directory used while it was idle is closed at once instead of after one minute.

## [0.5.1] - 2026-10-11

### Fixed

- Stopping the recording of a device (idle stop, end of a pin, `record --stop`) or stopping the service no longer leaves adb processes behind ([#24](https://github.com/s1crab/android-log-mcp/issues/24)). It cancels the device's background work (process table, packages, buffer span, DropBox fetches, device tracking, identity probes) and waits for it, at most 5 s, before the store is closed. Work still running then is named in the service log, and its adb processes are killed with their child processes (never the adb server). A cancelled adb call also stops waiting for output pipes that a child process of adb still holds open. A DropBox fetch can now be cancelled, and a fetch interrupted by a stop runs in the next recording session.

## [0.5.0] - 2026-10-11

### Changed

- **Breaking:** recording starts on use ([#11](https://github.com/s1crab/android-log-mcp/issues/11)). An idle background service runs no adb at all. A device starts being recorded on the first log tool call that uses it (`logcat_status`, `logcat_search`, `logcat_crashes`, `logcat_app_launches`, also via `call`), on `report` and on `record --for`, and is first backfilled from its log buffer: calls wait up to 10 s for that and otherwise say "backfill in progress"; `logcat_status` then returns no cursor and asks to be called again. With several devices connected and no `device`, nothing is started and the devices are listed; exceeding `ANDROID_LOG_MCP_MAX_DEVICES` is an error. Upgrade notes: [README › Upgrading › To 0.5.0](README.md#to-050-breaking).
- **Breaking:** recording of a device stops after its own idle time since its last log tool call (`ANDROID_LOG_MCP_LINGER`, default `max(10m, 2 × the device's log buffer span)`, at most `1h`; `0` = after each call); a call that waits with `wait_ms` counts as use. Clients connecting or disconnecting no longer affect recording. The service no longer exits after the linger: it stays while clients are connected and exits 30 s after there are no clients, no pin and no recorded device.
- **Breaking:** `report` starts the background service (detached) when none runs, starts recording the requested devices and backfills them (up to 10 s; otherwise the manifest warns "backfill incomplete"), instead of building the report in its own process.
- `set-root` keeps the started devices, their idle timers and pins. Service protocol 2 (adds `record`); 0.4 clients keep working.
- Faster first backfill, mostly on emulators: the device's log buffer is streamed with `adb exec-out logcat` (`adb logcat` on Android < 5.0 or when exec-out fails); while backfilling no other adb command runs on the device (`ps`, `pm list packages`, `dumpsys dropbox`, buffer span), and afterwards each DropBox tag needed by crashes in the backfill is fetched once (together with the new-boot history; skipped if fetched in the last 5 minutes); lines from before recording started no longer trigger `ps`; the identity and device probes are one adb shell. On an API 29 emulator a full buffer (~28,000 lines) now catches up in 29–37 s instead of 54–70 s; reading the buffer alone takes ~27 s there.
- "backfill in progress" shows the estimated total, e.g. "(1,423 of ~27,000 lines; earliest …)", estimated from the entries logd holds (`logcat -S`); after a `-T` resume it shows "(N lines so far; …)".

### Added

- `android-log-mcp record [--device X]... --for DURATION`, `record --stop` and `record` (state; `--json`): keep a device recorded without tool calls or clients until a deadline (at most 24 h, never shortened), stop it now, or show each device's recording, backfill, idle deadline and pin. `--for` starts the service detached when none runs. Agents use `logcat_status(record_for="30m")` (`"0"` stops).
- Pins are stored in `<control dir>/run/record.json` and survive crashes, version handoffs, `set-root` and restarts. A connected client restarts a crashed service at once while a pin is active. A pin is suspended after 3 crashes within a minute (`crash-loop`) or more than 10 automatic restarts per service version (`crash-budget`), shown with the last error and cleared by a new `record --for`.
- `logcat_status`, `service status`, `root --json` and the client's stderr line show per device whether it is recorded or idle, the backfill progress, the idle deadline and pins.

### Deprecated

- `ANDROID_LOG_MCP_CALL_WAIT` is ignored with a note on stderr: calls wait for the backfill themselves.

### Fixed

- `logcat_status` / `service status`: the global `linger:` line no longer says "(not measured yet)"; it states the rule, each recorded device shows its own value.
- Recording resumes without losing or duplicating lines after the device clock went back (e.g. an emulator snapshot restore): the buffer is read without `-T` and already stored lines are skipped; `logcat_status` reports it.
- A log tool call (or `report`) that arrived while its device was being stopped for idleness could answer from the stored data alone: no new lines, no backfill note, and `logcat_status` returned a cursor from before the stop ([#15](https://github.com/s1crab/android-log-mcp/issues/15); seen on Windows CI). Such a call now waits (within the same 10 s) until the device is recorded again; otherwise it says "backfill in progress (recording is restarting …)" and `logcat_status` returns no cursor. An idle stop re-checks under its lock that no call used the device in the meantime and is skipped if one did. A device restarted within 30 s of its identity probe no longer reuses that probe's clock reading: each saved probe is used once, and its clock is advanced by its age.

## [0.4.1] - 2026-10-10

### Added

- `android-log-mcp install [--json]`: copies the binary to a fixed path, `<user cache dir>/android-log-mcp/exe/android-log-mcp[.exe]` (Windows `%LocalAppData%\android-log-mcp\exe\android-log-mcp.exe`), and prints it, so MCP clients can run the native binary directly instead of the `npx.cmd` → `cmd.exe` → `node` → binary chain. Re-running it updates the copy even while clients run it; a copy that is still running is renamed aside and removed by a later install. `root --json` shows it as `exe`. The README recommends this on Windows, with Codex, Cursor and Claude Code examples; `npx` remains supported.

### Changed

- README: "Install and configure" now has two paths, A. npx and B. the native binary (recommended on Windows; from `install` or a GitHub Release tarball), with Codex, Cursor and Claude Code examples for Windows and macOS/Linux, upgrade steps and a table of which path to use on which OS.
- The npm launcher states `shell: false` explicitly (it already started the binary directly with `windowsHide`); a test guards it.

### Fixed

- Windows: console windows kept popping up while the background service ran (for example with Codex or ChatGPT as the MCP client). The service was started with `DETACHED_PROCESS`, i.e. without any console, so every console child it started (`adb devices`, `track-devices`, `shell getprop`, `logcat`, `start-server`, …) got a new visible console window. The service now starts with its own hidden console (`CREATE_NO_WINDOW`, still in a new process group and outside the client's job), and every child process the program starts is created without a window. A source scan test makes sure no new `exec.Command` skips this.

## [0.4.0] - 2026-10-10

A breaking release: a new storage layout, one data root for all clients, and one background service per user that does all recording.

Upgrade notes: [README › Upgrading › To 0.4.0](https://github.com/s1crab/android-log-mcp/blob/main/README.md#to-040-breaking).

> 中文摘要：破坏性版本。新的存储结构 `v1/<设备ID>/`，旧数据不迁移、不读取；移除 `ANDROID_LOG_MCP_HOME` 和 `--home`，改用 `root` / `set-root` 统一管理数据根目录；每个用户一个后台服务负责全部录制，最后一个客户端关闭后继续录制一段时间（`ANDROID_LOG_MCP_LINGER`）；Windows 最低要求 Windows 10 1803 / Server 2019。

### Added

- **Background service** ([#3](https://github.com/s1crab/android-log-mcp/issues/3)): one long-lived process per user records every connected device and answers every tool call; MCP and `call` processes are thin stdio clients.
  - Started on demand and detached, from a copy of the binary under `<root>/bin/<version>-<hash>/` (the running and at most one other version are kept). If that is impossible, the client runs the service in its own process and serves the same socket for other clients.
  - Crash recovery: clients reconnect on their next call, one starts a new service and the call is retried; recording resumes with `logcat -T` and deduplication. A crash-loop guard stops respawning after 3 exits within a minute.
  - Linger: the service keeps recording for `max(10 min, 2 × device log buffer span)`, at most 1 h, after the last client disconnects; new variable `ANDROID_LOG_MCP_LINGER` (`30m`, `0`, `forever`). A new service waits at least 30 s for its first client.
  - Versions: a client replaces the service only if the service is older; a newer service is used as is. A separate protocol version lets compatible older clients use a newer service; incompatible clients get an error asking to upgrade.
  - Local only: AF_UNIX socket in a user-only directory (0700 / 0600; protected DACL for the user and SYSTEM on Windows) with a same-user peer check. No TCP.
  - Visibility: client stderr and `logcat_status` show the service pid, version, protocol, data root, linger and service log (`<root>/log/service.log`, 1 MiB plus one rotation, counted in the total cap; `crash.log` for runtime crashes).
  - `android-log-mcp service [status [--json] | stop]`; `android-log-mcp service` runs it in the foreground.
  - The adb server is started outside the service's process group and is never killed; the service's own adb children end with it.
- **`android-log-mcp set-root DIR --keep-old | --delete-old [--timeout] [--json]`** ([#3](https://github.com/s1crab/android-log-mcp/issues/3), [#10](https://github.com/s1crab/android-log-mcp/issues/10)): moves the data root for every client. The service stops recording, carries each device's state over (seq numbers, the `logcat -T` resume point and the boundary lines, so there is no gap and no duplicate), commits `config.json` atomically under `run/config.lock` and resumes; any failure before the commit is rolled back. `--delete-old` removes only the tool's own directories; leftovers are reported (exit 3) and retried at the next service start. Works without a running service (never starts one).
- **`android-log-mcp root [--json]`** prints the data root, control directory, config file, socket and running service ([#9](https://github.com/s1crab/android-log-mcp/issues/9), [#10](https://github.com/s1crab/android-log-mcp/issues/10)).
- **Stable device IDs** ([#5](https://github.com/s1crab/android-log-mcp/issues/5)): `ro.serialno` for physical devices (the same over USB and Wi-Fi), `avd-<name>` for emulators, a fingerprint hash as fallback. Two devices online at once with the same ID are told apart by boot id and filed as `<id>~<adb serial>` with a note in `logcat_status` ([#6](https://github.com/s1crab/android-log-mcp/issues/6)).
- `scripts/service-e2e.sh`: live check of the service on a device through the npm launcher.
- CI: the npx compatibility jobs also run the MCP handshake through the background service.

### Changed

- **Breaking:** new versioned storage layout `<root>/v1/<device-id>/` under one data root, by default `<user cache dir>/android-log-mcp` ([#5](https://github.com/s1crab/android-log-mcp/issues/5), [#7](https://github.com/s1crab/android-log-mcp/issues/7)). **Data recorded by 0.3 or earlier is neither migrated nor read**, and 0.3 report zips cannot be loaded with `--offline`; the old `<serial>/` directories are ignored, not deleted.
- **Breaking:** recording moved into the background service; it continues for the linger time after the last MCP client closes ([#3](https://github.com/s1crab/android-log-mcp/issues/3)).
- **Breaking:** Windows 10 1803 / Windows Server 2019 or later is required (AF_UNIX); older Windows gets a clear error.
- **Breaking:** `--offline <dir>` is the only read-only mode and takes the dataset directory (`--home DIR --offline` no longer works); the `report` flag that skips adb is now `--no-adb` ([#7](https://github.com/s1crab/android-log-mcp/issues/7)).
- `report` is built by the running service (or by the command itself when none runs); `call` goes through the service.
- Everything the tool creates is bounded: retention and the total size cap cover all format versions, `tmp/`, `run/`, `bin/` and `log/`; anything else under the root is ignored ([#5](https://github.com/s1crab/android-log-mcp/issues/5)).
- Windows file locks use `LockFileEx`, released by the OS when the process exits (no staleness heuristics); `flock` on Linux/macOS ([#9](https://github.com/s1crab/android-log-mcp/issues/9)).
- CI: Windows test failures now fail the build.
- README rewritten English-first; this changelog added.

### Removed

- **Breaking:** `ANDROID_LOG_MCP_HOME` and `--home`. If either is still set, the program exits with an error naming `set-root` ([#9](https://github.com/s1crab/android-log-mcp/issues/9), [#10](https://github.com/s1crab/android-log-mcp/issues/10)).
- Per-device owner locks (`owners/`), follower mode and takeover between MCP processes; a leftover `owners/` directory from development builds is removed automatically ([#3](https://github.com/s1crab/android-log-mcp/issues/3)).
- Migration of the pre-0.4 flat layout and the `legacy/` handling ([#7](https://github.com/s1crab/android-log-mcp/issues/7)).

### Fixed

- A crash of the recorder when the newest segment is empty after a seq jump (`Store.Scan` indexed past the in-memory window) ([#9](https://github.com/s1crab/android-log-mcp/issues/9), [#10](https://github.com/s1crab/android-log-mcp/issues/10)).
- Leaked `adb track-devices` processes after a recorder was killed: adb children now end with the service.

### Security

- The service socket is reachable only by the same user (directory 0700, socket 0600, peer credential check; Windows ACL for the user and SYSTEM) and there is no network listener.

## [0.3.0] - 2026-10-10

### Added

- Memory and disk limits ([#2](https://github.com/s1crab/android-log-mcp/issues/2)): new variables `ANDROID_LOG_MCP_MEM_RECORDS`, `ANDROID_LOG_MCP_MEM_MB`, `ANDROID_LOG_MCP_HISTORY_CACHE_MB`, `ANDROID_LOG_MCP_IDLE_RELEASE`, `ANDROID_LOG_MCP_MAX_DEVICE_MB`, `ANDROID_LOG_MCP_MAX_TOTAL_MB`, `ANDROID_LOG_MCP_MIN_FREE_MB`.
- Configurable retention with `ANDROID_LOG_MCP_MAX_AGE` (Go duration or days, e.g. `7d`); `logcat_status` shows the effective value.
- Total cap for the data directory (default 2 GiB) and a recording pause when free disk space drops below 512 MiB, resuming with `logcat -T`.

### Changed

- **Breaking:** the default retention is **1 day instead of 7**; on first start, older segments and older crash, DropBox and ended-process records are deleted. Set `ANDROID_LOG_MCP_MAX_AGE=7d` to keep the old behavior.
- In-memory window capped at 20,000 lines or about 16 MiB per device (was 100,000 lines, unbounded bytes); history is read from disk in about 512 KiB blocks into a 32 MiB LRU cache, released after 2 minutes without queries. Measured recorder RSS with 300k records: about 300 MiB → 65 MiB after queries, 19 MiB idle.
- Aux files (`procs.jsonl`, `crashes.jsonl`) are compacted periodically, dropping expired entries.

### Fixed

- Windows: a source file named `aux.go` (a reserved device name on Windows) broke checkouts.

## [0.2.1] - 2026-10-10

### Added

- `android-log-mcp compat [--serial S] [--json | --raw]` and `scripts/compat-probe.sh`: a read-only probe of a device's logcat / ps / pm / dumpsys features, evaluated with the recorder's own parsers.
- README compatibility matrix (verified vs designed-only) and evidence from an API 29 emulator.
- CI: npx compatibility jobs on Node 16 – current (Linux) and Node 22 on macOS and Windows; the release workflow creates a GitHub Release with the npm tarballs.

### Fixed

- uid names `llkd`, `lmkd`, `dhcp`, `mtp` and `dns` from API 29 logcat are mapped instead of left unparsed.

## [0.2.0] - 2026-10-10

### Added

- `android-log-mcp report`: packs recorded logs, device info, DropBox entries, an optional `am start -W` result and a note into a zip, with sensitive values (tokens, credentials, emails, phone numbers, ID and card numbers, device IDs, IPs, MACs, domains) redacted by default; the packed store loads back read-only ([#1](https://github.com/s1crab/android-log-mcp/issues/1)).
- Global `--offline` flag to browse a dataset without adb.

### Fixed

- Windows: stores closed in tests and safer file replace/delete (meta rename and segment pruning retried while another process has the file open).

## [0.1.0] - 2026-10-10

### Added

- First release: an MCP stdio server giving AI agents read-only access to Android logcat, with background recording of every connected device, the tools `logcat_status`, `logcat_search` (Android Studio filter syntax with strict errors), `logcat_crashes` and `logcat_app_launches`, and the `call` debug subcommand.
- npm packaging: a Node launcher plus per-platform binary packages (linux / darwin / win32 × x64 / arm64), CI and release workflows.

[Unreleased]: https://github.com/s1crab/android-log-mcp/compare/v0.5.2...HEAD
[0.5.2]: https://github.com/s1crab/android-log-mcp/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/s1crab/android-log-mcp/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/s1crab/android-log-mcp/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/s1crab/android-log-mcp/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/s1crab/android-log-mcp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/s1crab/android-log-mcp/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/s1crab/android-log-mcp/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/s1crab/android-log-mcp/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/s1crab/android-log-mcp/releases/tag/v0.1.0
