# 0.12.0 pre-release verification (AGENTS.md §3, #106)

Build: `main` at `cfc0c7f` + the release commit `4878c48` (version **0.12.0**). Release PR: [#279](https://github.com/s1crab/android-log-mcp/pull/279). Changes since 0.11.0: https://github.com/s1crab/android-log-mcp/compare/v0.11.0...v0.12.0, details in [CHANGELOG.md](../../CHANGELOG.md#0120---2026-10-27). The model for this file is [../0.11.0/README.md](../0.11.0/README.md).

**Tested sha:** `4878c48` (release commit; `main` `cfc0c7f` + version/CHANGELOG bump). The final branch head differs from it only by this README.

## Contents of the release

- **Package-scoped crash/ANR counts** ([#269](https://github.com/s1crab/android-log-mcp/issues/269) / [#272](https://github.com/s1crab/android-log-mcp/pull/272) / omit-zeros [#273](https://github.com/s1crab/android-log-mcp/pull/273)): MCP `ok`/`empty` responses include non-zero `crashes=` / `anrs=` for the requested package (shared-UID aware); zero keys omitted.
- **`launches.jsonl`** ([#270](https://github.com/s1crab/android-log-mcp/issues/270) / [#274](https://github.com/s1crab/android-log-mcp/pull/274)): G3 aux index for `logcat_app_launches` (ingest + lazy backfill; warm query path).
- **Multi-package structured `package`** ([#276](https://github.com/s1crab/android-log-mcp/issues/276) / [#278](https://github.com/s1crab/android-log-mcp/pull/278)): JSON array / comma-separated ids (≤20); named per-package non-zero crash/ANR counts.
- **`ANDROID_LOG_MCP_SKIP_SEG_CRC64`** ([#271](https://github.com/s1crab/android-log-mcp/issues/271) / [#275](https://github.com/s1crab/android-log-mcp/pull/275)): opt-in skip of full-segment CRC64 on cold idx load; default remains verify.

No format-version bump: `launches.jsonl` is additive aux; older binaries ignore it.

## Verification summary

| item | result | evidence |
|---|---|---|
| CI (14/14, incl. perf) | PASS on `4878c48` (perf flaked once on variance; `--failed` rerun succeeded; tested sha unchanged) | [run 40527616969](https://github.com/s1crab/android-log-mcp/actions/runs/40527616969) |
| race-gate `count=20` on `4878c48` (AGENTS.md §3 full gate for minor) | PASS, all 7 packages × linux/windows/macos (21/21 race jobs). Slowest: macos `device` ~78 min (4697 s), windows `device` ~61 min (3675 s), ubuntu `device` ~59 min (3567 s), windows `cmd` ~59 min (3558 s; under 65m gotest timeout) | [run 40528402431](https://github.com/s1crab/android-log-mcp/actions/runs/40528402431) |
| emulator API 29 + 35 (workflow_dispatch) | PASS: compat, service-e2e, report-e2e, app-e2e, clock-e2e, crash-gate, crash-gate-control on both APIs | [run 40528406280](https://github.com/s1crab/android-log-mcp/actions/runs/40528406280), artifacts `emulator-api29`, `emulator-api35` |
| SIGSTOP / upgrade / `date -s` / `set-root` / local rows | covered where Actions covers them: `set-root` + whole-buffer via service-e2e; clock jumps / crash-ANR via clock-e2e + crash-gate. Local TCG SIGSTOP / upgrade / `wanted.json` / dedicated `date -s` rows **not re-run** for this cut (product changes are #269/#273/#270/#276/#271; gaps noted) | Actions emulator artifacts |
| Windows / macOS adb | **unverified** (CI only, as in CHANGELOG) | — |

## Evidence archive

Raw evidence (Actions CI / race-gate / emulator job lists, race-gate summary table, emulator artifacts): `/workspace/release-evidence/0.12.0-evidence.tar.gz` (upload as a Release asset after tagging). Nothing raw under `validation/0.12.0/` (this README only). sha256 `f38bee33ae7be6f8442e02d7d7478384b88a75dac8d92513cbbbb69863127378`.

Caveats: windows `cmd` at count=20 finished in ~3558 s against a 65m gotest timeout (comfortable vs 0.11.0's ~3760 s). Perf job needed one `--failed` rerun for export.ms variance; no product bug found in gates; no gate was masked.
