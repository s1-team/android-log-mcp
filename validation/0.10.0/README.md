# 0.10.0 pre-release verification (AGENTS.md §3, #106)

Build: `main` at `eee9361` + the release commit (version **0.10.0**). Release PR: [#250](https://github.com/s1crab/android-log-mcp/pull/250). Changes since 0.9.1: https://github.com/s1crab/android-log-mcp/compare/v0.9.1...v0.10.0, details in [CHANGELOG.md](../../CHANGELOG.md#0100---2026-10-18). The model for this file is [../0.9.1/README.md](../0.9.1/README.md).

**Tested sha:** `eee9361` (`origin/main` when gating started; the release branch head `3846e6a` differs from it only by version/CHANGELOG/validation README). #249 (e2e adb-wait hardening) was already merged into that head.

## Contents of the release

Scope (user decision): everything on `main` after `v0.9.1`, including #247 wait-for-exit and #234 `wanted.json` resume.

- **#234 / #226:** recording resumes for previously wanted devices after the service is killed/replaced (`run/wanted.json`); opt out with `ANDROID_LOG_MCP_LINGER=0`.
- **#247 / #226:** `StopHolder` waits for the old holder process to exit (identity pid + start time; Windows; Unix wait deferred), `ExitWait` bounded to 5 s.
- **R2 crash/ANR fact identity (#225):** #229/#235/#238/#245 — one crash/ANR stays one row across restarts; ANR sources paired by ordinal; DropBox incarnation veto. Additive fields (`inc`, `incw`, `ev`, `n`, `run/wanted.json`) do not bump the format version.
- **#177:** redaction ~30× faster, output byte-identical.
- Test-only: #249/#248 e2e adb-wait hardening, race/lifecycle flakes.

## Verification summary

Actions emulator (API 29 and API 35, KVM runners) and local API 29 TCG (`validation/scripts/emu-start.sh`, one emulator, serial, `auto_time=1`) for the rows Actions does not cover. Windows / macOS: CI only, **unverified** with a real or emulator-attached adb.

| item | result | evidence |
|---|---|---|
| CI (14/14, incl. perf) | PASS ([run 39089415238](https://github.com/s1crab/android-log-mcp/actions/runs/39089415238)) on `3846e6a` | Actions on this PR |
| race-gate `count=10` | PASS, all packages × linux/windows/macos ([run 39089420363](https://github.com/s1crab/android-log-mcp/actions/runs/39089420363)); summary posted on the PR. (User rule for this release; AGENTS.md says 20.) | Actions |
| perf job | PASS (same CI run) | Actions |
| service-e2e API 29 | PASS (`RESULT: OK`, Actions) | [emulator run 39089422383](https://github.com/s1crab/android-log-mcp/actions/runs/39089422383) artifact `emulator-api29` |
| service-e2e API 35 | PASS (`RESULT: OK`, Actions) | same run, `emulator-api35` |
| e2e-report API 29 | PASS (23 secrets / 16 kinds redacted, 29 LeakTest headers intact) | Actions `emulator-api29` |
| e2e-report API 35 | PASS (same pattern) | Actions `emulator-api35` |
| `--logs-only` | PASS via Actions report-e2e (service path) | Actions |
| app-e2e API 29 (incl. export / `--out`) | PASS (0 failed checks) | Actions |
| app-e2e API 35 | PASS (0 failed checks) | Actions |
| clock-e2e API 29 / 35 | PASS | Actions |
| crash-gate + crash-gate-control (R2 E1–E3, uninterrupted native one row) API 29 / 35 | PASS (E1 java+clock, E2 anr+clock, E3 native/abort + restart; control one native row) | Actions |
| SIGSTOP the service (local API 29) | PASS: auto-replace after hang (client call 42.9 s), `service stop --force` 13.4 s; five markers each stored exactly once | `release-rows/hung0100-run.txt` |
| upgrade handoff (local API 29) | PASS: 0.9.1 and 0.9.0 services replaced by the 0.10.0 client; first status 11.1 s / 4.6 s; marker stored once; replacement logged | `release-rows/upgrade0100-run.txt` |
| `date -s` back-jump + real crash/ANR (local API 29) | PASS: markers on both sides of the jump; cr14/cr15 java, cr35 native (one row), anr11 (one row, `n=1`), cr81 after restore; each with logcat+events(+dropbox) sources | `release-rows/clockjump0100/` |
| `set-root` + whole-buffer re-read | PASS (inside Actions service-e2e, both APIs) | Actions service-e2e logs |
| `wanted.json` real-restart (local API 29) | PASS: kill -9 while wanted → successor resumes (`resumed after the replaced or crashed service`); with `ANDROID_LOG_MCP_LINGER=0` it does not (`not resuming … LINGER=0`); only the pre-kill marker stored in linger0 mode | `release-rows/wanted0100-run.txt` |
| Windows / macOS adb | **unverified** (CI unit tests with fake adb only); marked in CHANGELOG | — |

## Evidence archive

Raw evidence (local run logs + Actions result/log excerpts): `/workspace/release-evidence/0.10.0-evidence.tar.gz` (upload as a Release asset after tagging). Nothing raw under `validation/0.10.0/` (this README only). sha256 `e00dbfd45d6dc911cd455f14387ce8a258f736fc9f95612b1d55a5d41b73e300`.

Caveats: the first local upgrade/clockjump attempt aborted when the TCG emulator process died after the hung row (~10:24 Asia/Shanghai); emulator was restarted with `emu-start.sh` and those two rows were re-run successfully (10:40–10:55). API 29 Actions job used the cold-boot retry path (`emulator boot retried: true`); API 35 did not. No real bugs found; no gate was masked.
