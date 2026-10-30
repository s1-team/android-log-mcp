# 0.12.1 pre-release verification (AGENTS.md §3, #106)

Build: `main` at `5c5cc4e` + release commit `1e3b933` + Listen harden `d8caecf` (version **0.12.1**). Release PR: [#287](https://github.com/s1-team/android-log-mcp/pull/287). Changes since 0.12.0: https://github.com/s1-team/android-log-mcp/compare/v0.12.0...v0.12.1, details in [CHANGELOG.md](../../CHANGELOG.md#0121---2026-10-30). The model for this file is [../0.12.0/README.md](../0.12.0/README.md).

**Tested sha:** `d8caecf` (`main` `5c5cc4e` + `chore: release 0.12.1` `1e3b933` + Listen sock-name race harden). The final branch head differs from it only by this README.

## Contents of the release

- **Windows leftover `service.sock` / `WSAEINVAL` recovery** ([#285](https://github.com/s1-team/android-log-mcp/issues/285) / [#286](https://github.com/s1-team/android-log-mcp/pull/286) @ `5c5cc4e`): `Listen` clears a stale AF_UNIX sock and retries bind once so force-killed hung-service restarts recover without a manual delete.
- **Listen sock-name race harden** (`d8caecf`, seen in #287 CI): keep `UnlinkOnClose` false until restrict succeeds; refuse a listener when the name vanished between bind and chmod; retry once. Fixes flaky `TestWantedGarbageAndUnknownKeys` chmod ENOENT on ubuntu-latest.

No format-version bump; no adb / ingest path change.

## Verification summary

| item | result | evidence |
|---|---|---|
| CI (14/14, incl. perf) | PASS on `d8caecf` (ubuntu was red on earlier `1e3b933` via `TestWantedGarbageAndUnknownKeys` chmod ENOENT — fixed in `d8caecf`; perf flaked once on `export.ms` variance, `--failed` rerun succeeded; tested sha unchanged) | [run 41013654129](https://github.com/s1-team/android-log-mcp/actions/runs/41013654129) |
| race-gate `count=20` on `d8caecf` | PASS, all 7 packages × linux/windows/macos (21/21 race jobs). Slowest: macos `device` ~79 min (4736 s), windows `device` ~64 min (3815 s), ubuntu `device` ~60 min (3589 s), windows `cmd` ~57 min (3444 s; under 65m gotest timeout) | [run 41017224049](https://github.com/s1-team/android-log-mcp/actions/runs/41017224049) |
| emulator API 29 / 35 | **skipped** — product change is Windows AF_UNIX leftover sock recovery (+ Listen name-race harden); Linux emulator does not prove AF_UNIX leftover recovery; covered by CI `windows-latest` `TestListenRecoversLeftoverSocket` + new Listen race regressions; no adb / ingest path change | — |
| SIGSTOP / upgrade / `date -s` / `set-root` / local rows | **not re-run** (Windows-only IPC fix + Listen harden; no adb path change) | — |
| Windows / macOS real/emulator-attached adb | **unverified** (CHANGELOG); Windows AF_UNIX recovery covered by unit test on `windows-latest` | — |

### Race gate (`count=20` at `d8caecf`)

| package | os | result | seconds |
|---|---|---|---|
| store | ubuntu-latest | pass | 237 |
| ipc | ubuntu-latest | pass | 35 |
| analysis | ubuntu-latest | pass | 36 |
| device | ubuntu-latest | pass | 3589 |
| mcpserver | ubuntu-latest | pass | 859 |
| service | ubuntu-latest | pass | 1477 |
| cmd | ubuntu-latest | pass | 1042 |
| store | windows-latest | pass | 334 |
| ipc | windows-latest | pass | 91 |
| analysis | windows-latest | pass | 87 |
| device | windows-latest | pass | 3815 |
| mcpserver | windows-latest | pass | 1000 |
| service | windows-latest | pass | 1616 |
| cmd | windows-latest | pass | 3444 |
| store | macos-latest | pass | 377 |
| ipc | macos-latest | pass | 51 |
| analysis | macos-latest | pass | 61 |
| device | macos-latest | pass | 4736 |
| mcpserver | macos-latest | pass | 813 |
| service | macos-latest | pass | 1986 |
| cmd | macos-latest | pass | 1435 |

## Evidence archive

Raw evidence (Actions CI / race-gate job lists, race-gate summary table, head snapshot): `/workspace/release-evidence/0.12.1-evidence.tar.gz` (upload as a Release asset after tagging). Nothing raw under `validation/0.12.1/` (this README only). sha256 `8f9e82d0da849e4730181f57a34aa237aabe7e42e9c121d535701319c608db5d`.

Caveats: emulator skipped by design for this Windows AF_UNIX / Listen harden patch (unit coverage on `windows-latest`). Win/mac real adb unverified. Perf needed one `--failed` rerun for export.ms variance; no product bug found in gates; no gate was masked.
