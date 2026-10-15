# Compatibility and verification

What has been verified on which Android versions, known limitations, and the evidence. The evidence lives in [../validation/README.md](../validation/README.md).

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
