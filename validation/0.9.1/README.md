# 0.9.1 pre-release verification (AGENTS.md §3, #106)

Build: `main` at `v0.9.0` (`e0e7d19`) + the release commit (version **0.9.1**). Release PR: [#224](https://github.com/s1crab/android-log-mcp/pull/224). Changes since 0.9.0: https://github.com/s1crab/android-log-mcp/compare/v0.9.0...v0.9.1, details in [CHANGELOG.md](../../CHANGELOG.md#091---2026-10-17). The model for this file is [../0.9.0/README.md](../0.9.0/README.md).

## Contents of the release

Scope decision: only what was already under `[Unreleased]` on `main` after `v0.9.0`; `main` has no commit after the tag, so the release PR is the version bump, the CHANGELOG move and this file. [#177](https://github.com/s1crab/android-log-mcp/issues/177) is **not** in 0.9.1.

- **#173 G6:** `meta.json` is trimmed together with retention (`seg_times`, `clock_jumps` before the first retained seq, `sessions` that ended before it; the session covering the first retained seq is kept) and written compactly.
- **#173 G8:** the `cursor_pruned` note says which line "oldest" is (`first=<seq>`).
- **#173 docs:** the on-disk store (JSONL segments, `meta.json`, aux jsonl) is declared an internal, versioned format ([docs/reference.md](../../docs/reference.md): "On-disk store format is internal and versioned").
- No tool, CLI, protocol or segment-file change; older binaries read the compact `meta.json` as before.

Because the change is confined to store retention bookkeeping, one search note and docs, the emulator checks below are the standard §3 rows (device, service and handoff behaviour) plus a targeted #217 check; the long clock-e2e matrix, the export matrix and the F1/F2 recovery cases of 0.9.0 were not repeated (no code they exercise changed after the 0.9.0 validation).

## Verification summary

Device rows ran from a clean build of the release branch at **`51b15ae`** (version **0.9.1**; docs-only commits follow it), API 29 TCG emulator-5556 started by `validation/scripts/emu-start.sh` (software emulation, no KVM, one emulator, strictly serial), 2026-10-17 19:27-19:52 Asia/Shanghai. Evidence under `stage091/release-rows/` in the archive.

| item | result | evidence (in the archive) |
|---|---|---|
| CI (14/14) | PASS ([run 39030387902](https://github.com/s1crab/android-log-mcp/actions/runs/39030387902), 14/14 on `7a42cdf`; the commits after it only touch this file and the PR checks of the final head are the last word) | Actions on this PR |
| service-e2e API 29 | PASS (`RESULT: OK`, 29 PASS, 0 FAIL) | `release-rows/service-e2e-api29.txt` |
| service-e2e API 35 | **not run**: only one TCG emulator may run on this box; a second would perturb timing (AVD `api35` exists). Same as 0.8.x and 0.9.0 | — |
| e2e-report API 29 | PASS: 23 secrets of 16 kinds redacted, 29 LeakTest headers intact, offline load OK | `release-rows/e2e-report-api29.txt`, `release-rows/e2e-report-api29/` |
| e2e-report API 35 | **not run** (same reason) | — |
| `--logs-only` | PASS via service and `--offline` (small zips: diagnostics only) | `release-rows/logs-only.txt`, `logs-only.zip`, `logs-only-offline.zip` |
| app-e2e API 29 (incl. export / `--out`) | PASS (0 failed checks) | `release-rows/app-e2e-api29.txt`, `release-rows/app-e2e-api29/` |
| app-e2e API 35 | **not run** (same reason) | — |
| SIGSTOP the service | PASS: auto-replace after >10 s hang (client call 42.9 s), `service stop --force` 13.2 s; five markers each stored exactly once | `release-rows/hung091.sh`, `hung091-run.txt` |
| upgrade handoff | PASS: a 0.9.0 and a 0.8.1 service (with data) replaced by the 0.9.1 client; first status ~11.1 s; `caught_up=1` within seconds; marker stored once; replacement logged | `release-rows/upgrade091.sh`, `upgrade091-run.txt` |
| `date` back-jump + crash + ANR | PASS: clock set back 1 day then restored (`auto_time=1`); Java / native / ANR attributed to `com.example.logtest` at body/device times; 7 clock jumps split the marker search correctly | `release-rows/clockjump091.sh`, `clockjump091-run.txt`, `clockjump091/` |
| set-root + whole-buffer re-read | PASS (inside service-e2e): boundary line not re-stored; no marker stored twice after switch-back | `release-rows/service-e2e-api29.txt` |
| #217 meta trim + `cursor_pruned first=` | PASS: on a copy of the real emulator store (2 segments, 5 clock jumps) plus a dead session, dropping the oldest segment left `first=11693`, 0 clock jumps, only the covering session, only the retained `seg_times`, compact `meta.json` (1,912 → 788 bytes); an offline `logcat_search` with an old cursor answers `note code=cursor_pruned boot=c40308c3 first=11693 using=oldest`; store/mcpserver unit tests green | `release-rows/meta217/` (`probe.txt`, `cursor.txt`, `meta-before.json`, `meta-after.json`, `unittests.txt`) |
| JSONL internal-format docs | PASS: `docs/reference.md` declares the store format internal and versioned and points to `logcat_export` / `report`; `scripts/check-doc-links.py` clean | `release-rows/docs-check.txt` |
| race-gate | PASS, `count=10` ([run 39028688842](https://github.com/s1crab/android-log-mcp/actions/runs/39028688842) on `51b15ae`; `go test -race -count=10` per package, all packages × linux/windows/macos green; summary posted on the PR) | Actions artifact / run link |
| perf | EnsureIndex cold median **~1.53 s** at ~512 MiB retained (measured on `3c8b781`, see `release-rows/index-build-512mib.md`); the 0.9.1 changes do not touch the index path. CI `perf` job: green in the same run | `release-rows/index-build-512mib.md`, Actions |

## API 35 / Windows / macOS

- **API 35:** not run: a second emulator (AVD `api35`) would run beside the API 29 one and perturb timing; the API 29 rows are the release evidence. CI covers unit tests on all OSes.
- **Windows / macOS:** CI only (**unverified** with a real or emulator-attached adb), same as 0.8.x and 0.9.0; noted in CHANGELOG.

## Where the raw evidence is

Raw logs, run outputs and the one-off scripts are **not in the repository** (AGENTS.md §3). They are in one archive, `0.9.1-evidence.tar.gz` (`MANIFEST.txt`, `SUMMARY.md`), sha256 `782a995d1782dd2cec2accd0889d2dbaa9c554bedac4ecd9f908e9de9d72d820`, to be attached to the GitHub Release `v0.9.1` after tagging. Until then it is on the release box at `/workspace/release-evidence/` (`stage091/release-rows/`).
