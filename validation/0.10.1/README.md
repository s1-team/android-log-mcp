# 0.10.1 pre-release verification (AGENTS.md §3, #106)

Build: `main` at `7058140` (includes #257) + the release commit `e82ee98` (version **0.10.1**). Release PR: [#255](https://github.com/s1crab/android-log-mcp/pull/255). Changes since 0.10.0: https://github.com/s1crab/android-log-mcp/compare/v0.10.0...v0.10.1, details in [CHANGELOG.md](../../CHANGELOG.md#0101---2026-10-20). The model for this file is [../0.10.0/README.md](../0.10.0/README.md).

**Tested sha:** `e82ee98` (release commit; `main` `7058140` + version/CHANGELOG bump). The final branch head differs from it only by this README. (The first round of gates ran on the earlier release commit `f9b708e` on `cdc855c`, before #257; it is kept below for reference.)

## Contents of the release

CI and test reliability only. `git diff v0.10.0..e82ee98` touches only: `.github/workflows/emulator.yml`, `.github/workflows/race-gate.yml`, `scripts/emulator-ci.sh`, nine `server/cmd/android-log-mcp/*_test.go` files, `server/internal/fakeadb/fakeadb.go` (test fake adb, imported only by tests), `server/internal/fakeadb/devices_test.go`, `server/internal/mcpserver/lazy_test.go`, and the release bump (`CHANGELOG.md`, `docs/reference.md`, `npm/VERSION`, the `Version` default in `server/internal/mcpserver/server.go`). **No product code changed** (no non-test Go file in the binary other than the version string), so the device checks of the 0.10.0 table were not re-run locally; the Actions emulator runs cover the product on both APIs. No emulator workflow or script changed since emulator run 39202398595 (`git diff cdc855c..7058140` touches only the three fakeadb/mcpserver test files), so the emulator was not re-dispatched.

- **#251** (#252): Windows `cmd` package race-gate no longer hits the 50 min timeout.
- **#242** (#253): API 29 emulator job no longer hangs at shutdown (bound-kill, `adb kill-server`, bracketed `pkill`).
- **#204** (#254): first emulator validation attempt timeout 32 → 25 min.
- **#256** (#257): nightly race-gate `TestStatusShowsLateHandover` (Windows) failure fixed in the test fake adb: a read racing `SetDevices`' rename reported an empty device list. Test/fake only, no product code.

## Verification summary

| item | result | evidence |
|---|---|---|
| CI (14/14, incl. perf) | PASS on `e82ee98` | [run 39786030434](https://github.com/s1crab/android-log-mcp/actions/runs/39786030434) |
| race-gate `count=10` on `e82ee98` | PASS, all 7 packages × linux/windows/macos (21/21 race jobs). Windows `cmd` ~33 min, Windows `mcpserver` ~12 min. (User rule; AGENTS.md says 20.) | [run 39786025466](https://github.com/s1crab/android-log-mcp/actions/runs/39786025466) |
| race-gate `count=10` on `f9b708e` (earlier release commit, before #257) | PASS 21/21 | [run 39202380058](https://github.com/s1crab/android-log-mcp/actions/runs/39202380058) |
| race-gate `count=10` on the #257 fix branch | PASS | [run 39780002442](https://github.com/s1crab/android-log-mcp/actions/runs/39780002442) |
| nightly race-gate 2026-10-20 (`cdc855c`) | PASS (Windows `cmd` green) | [run 39343915216](https://github.com/s1crab/android-log-mcp/actions/runs/39343915216) |
| nightly race-gate 2026-10-21 (`cdc855c`) | FAIL: `mcpserver` windows `TestStatusShowsLateHandover` → issue #256, fixed by #257 (Windows `cmd` green) | [run 39513754781](https://github.com/s1crab/android-log-mcp/actions/runs/39513754781) |
| nightly race-gate 2026-10-22 (`cdc855c`) | PASS (Windows `cmd` green) | [run 39693651917](https://github.com/s1crab/android-log-mcp/actions/runs/39693651917) |
| emulator API 29 + 35 (workflow_dispatch, `f9b708e`) | PASS: all steps on both (compat, service-e2e, report-e2e, app-e2e, clock-e2e, crash-gate, crash-gate-control) | [run 39202398595](https://github.com/s1crab/android-log-mcp/actions/runs/39202398595), artifacts `emulator-api29`, `emulator-api35` |
| API 29 job duration (#242 / #204) | dispatch 18 min 29 s; nightlies 18 min 43 s / 18 min 46 s / 18 min 50 s; `emulator boot retried: false` on the dispatch | same run; nightlies [39344577347](https://github.com/s1crab/android-log-mcp/actions/runs/39344577347), [39514804287](https://github.com/s1crab/android-log-mcp/actions/runs/39514804287), [39694628964](https://github.com/s1crab/android-log-mcp/actions/runs/39694628964) (all PASS) |
| API 35 job duration | 18 min 44 s (dispatch) | same run |
| local emulator rows (SIGSTOP, upgrade, `date -s`, `set-root`, `wanted.json`) | **not re-run**: no product code changed since 0.10.0 | — |
| Windows / macOS adb | **unverified** (CI only, as in 0.10.0) | — |

Nightly confirmation: race-gate Windows `cmd` was green in all three nightlies (2026-10-20/21/22), and the emulator nightlies were green on both APIs with API 29 at ~18–19 min. The one nightly failure (2026-10-21, #256) was in the test fake adb and is fixed in this release by #257. The nightlies ran on `cdc855c` (before #257); the post-fix race-gate is run 39786025466 above.

## Evidence archive

Raw evidence (Actions emulator artifacts, job lists of the CI/race-gate/emulator runs incl. the nightlies and #257, the failed job log of 39513754781, changed-file list): `/workspace/release-evidence/0.10.1-evidence.tar.gz` (upload as a Release asset after tagging). Nothing raw under `validation/0.10.1/` (this README only). sha256 `714b3f1f3217a6d2d434ec3b4ccf15a1886363d6dff6aa7009da3ab460ac32c0`.

Caveats: the first emulator dispatch (API 29 only, run 39202383982) was cancelled seconds after start and replaced by the default 29,35 run. One test-harness bug found pre-release (#256, fake adb on Windows), fixed in this release by #257; no product bug; no gate was masked.
