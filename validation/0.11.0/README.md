# 0.11.0 pre-release verification (AGENTS.md §3, #106)

Build: `main` at `d686441` + the release commit `aad0933` (version **0.11.0**). Release PR: [#266](https://github.com/s1crab/android-log-mcp/pull/266). Changes since 0.10.1: https://github.com/s1crab/android-log-mcp/compare/v0.10.1...v0.11.0, details in [CHANGELOG.md](../../CHANGELOG.md#0110---2026-10-23). The model for this file is [../0.10.1/README.md](../0.10.1/README.md).

**Tested sha:** `aad0933` (release commit; `main` `d686441` + version/CHANGELOG bump). The final branch head differs from it only by this README.

## Contents of the release

- **G1** ([#260](https://github.com/s1crab/android-log-mcp/issues/260) / [#263](https://github.com/s1crab/android-log-mcp/pull/263)): cold index load for closed segments via `seg-*.idx` sidecars (`ALMI` v1, CRC64; open segment stays memory-only).
- **R1** ([#258](https://github.com/s1crab/android-log-mcp/issues/258) / [#261](https://github.com/s1crab/android-log-mcp/pull/261) / [#264](https://github.com/s1crab/android-log-mcp/pull/264)): unified `EventClock` for jump-normalized time across detector, DropBox, launches and process table.
- **R5** ([#259](https://github.com/s1crab/android-log-mcp/issues/259) / [#262](https://github.com/s1crab/android-log-mcp/pull/262) / [#265](https://github.com/s1crab/android-log-mcp/pull/265)): `ResumeRing` (boot+seq hash ring) replaces the device-time-bounded compare set for resume/dedup.

No format-version bump: `.idx` is a derived cache; `resume_dedup.bin` / `Meta.ResumeDedup` layout unchanged.

## Verification summary

| item | result | evidence |
|---|---|---|
| CI (14/14, incl. perf) | PASS on `aad0933` | [run 39910068825](https://github.com/s1crab/android-log-mcp/actions/runs/39910068825) |
| race-gate `count=20` on `aad0933` (AGENTS.md §3 full gate for minor) | PASS, all 7 packages × linux/windows/macos (21/21 race jobs). Slowest: macos `device` ~78 min, windows `device` ~64 min, windows `cmd` ~63 min (under 65m gotest timeout) | [run 39910074459](https://github.com/s1crab/android-log-mcp/actions/runs/39910074459) |
| emulator API 29 + 35 (workflow_dispatch) | PASS: compat, service-e2e, report-e2e, app-e2e, clock-e2e, crash-gate, crash-gate-control on both APIs (~19 min each job) | [run 39910077256](https://github.com/s1crab/android-log-mcp/actions/runs/39910077256), artifacts `emulator-api29`, `emulator-api35` |
| SIGSTOP / upgrade / `date -s` / `set-root` / local rows | covered where Actions covers them: `set-root` + whole-buffer via service-e2e; clock jumps / crash-ANR via clock-e2e + crash-gate. Local TCG SIGSTOP / upgrade / `wanted.json` / dedicated `date -s` rows **not re-run** for this cut (product changes are G1/R1/R5; gaps noted) | Actions emulator artifacts |
| Windows / macOS adb | **unverified** (CI only, as in CHANGELOG) | — |

## Evidence archive

Raw evidence (Actions CI / race-gate / emulator job lists, race-gate summary table, emulator artifacts): `/workspace/release-evidence/0.11.0-evidence.tar.gz` (upload as a Release asset after tagging). Nothing raw under `validation/0.11.0/` (this README only). sha256 `1ff5fb58299f5d74fe766431647df5b53b6797a09440a80fd5126f0ea072ba03`.

Caveats: windows `cmd` at count=20 finished in ~3760 s against a 65m gotest timeout (tight but green). No product bug found in gates; no gate was masked.
