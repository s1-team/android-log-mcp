# 0.8.1 pre-release verification (AGENTS.md §3, #106)

Build: `0.8.x` + release commit (version **0.8.1**), binary `0.8.1`. Local runs: API 29 emulator-5556 (software emulation / TCG, no KVM), scratch `XDG_CACHE_HOME` per run, 2026-10-17 Asia/Shanghai. **API 35:** not run locally (only one TCG emulator; a second would perturb timing checks). Windows / macOS: CI only, **unverified** with a real adb.

Release PR: https://github.com/s1crab/android-log-mcp/pull/169

| item | result | evidence (in the archive) |
|---|---|---|
| CI (14/14) | green on `release/0.8.1` | Actions on PR #169 |
| service-e2e API 29 | RESULT: OK (all PASS, incl. set-root / whole-buffer no dups) | service-e2e-api29.txt |
| e2e-report API 29 | PASS: 23 secrets of 16 kinds redacted, 29 LeakTest headers intact, offline load OK | e2e-report-api29.txt, e2e-report-api29/ |
| `report --logs-only` | built by the service and with `--offline DIR`; small zips | logs-only.zip, logs-only-offline.zip, logs-only.txt |
| SIGSTOP the service | auto replacement after the 10 s hang; `service stop --force` 13.4 s; five markers each stored exactly once | hung081.sh, hung081-run.txt |
| upgrade handoff | 0.6.0 and 0.7.0 service with stored data replaced by the 0.8.1 client; first status ~11.1 s; `caught_up=1`; marker once | upgrade081.sh, upgrade081-run.txt |
| `date` back-jump + crash + ANR | jumps applied/restored (`auto_time=1`); java/native crash + ANR attributed to `com.example.logtest` at body/device times | clockjump081.sh, clockjump081-run.txt, clockjump081/ |
| set-root + whole-buffer re-read | no duplicates (service-e2e, boundary line not re-stored) | service-e2e-api29.txt |
| perf | `scripts/perf.sh --check` and `--check --pr`: 0 regressions | perf-check.txt, perf-check-pr.txt |
| race-gate `count=20` | **21/21 pass**, run [38960806063](https://github.com/s1crab/android-log-mcp/actions/runs/38960806063) on `release/0.8.1` | Actions artifacts `race-*-*` |

## API 35 / Windows / macOS

- **API 35:** not exercised on this box (single TCG API 29 emulator). CI covers unit tests on all OSes; emulator e2e for API 35 was not repeated for this patch.
- **Windows / macOS:** CI only (**unverified** with a real or emulator-attached adb), same as 0.8.0.

## Where the raw evidence is

Raw logs, run outputs, `logs-only*.zip`, and the one-off run scripts are **not in the repository** (AGENTS.md §3). They are in one archive, `android-log-mcp-0.8.1-evidence.tar.gz` (with `MANIFEST.txt` and a sha256), to be attached to the GitHub Release `v0.8.1` after tagging. Until then it is kept on the release box at `/workspace/release-evidence/`. CI logs stay as Actions artifacts.
