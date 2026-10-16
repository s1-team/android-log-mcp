# 0.8.0 pre-release verification (AGENTS.md §3, #106)

Build: `main` 3b9038f + the release commit (version 0.8.0), binary `0.8.0`. Local runs: API 29 emulator-5556 (software emulation / TCG, no KVM), scratch `XDG_CACHE_HOME` per run, 2026-10-16. API 35: run by the CI emulator jobs on GitHub KVM runners (not run locally: a second TCG emulator next to the first would perturb the timing checks). Windows / macOS: CI only, **unverified** with a real adb.

| item | result | evidence |
|---|---|---|
| service-e2e API 29 | 30 PASS, RESULT OK (rerun). First run: 28 PASS / 1 FAIL (`service status --json` had no `clients`; not reproduced on the rerun) | service-e2e-api29.txt, service-e2e-api29-first-run-1fail.txt |
| e2e-report API 29 | PASS: 23 secrets of 16 kinds redacted, 29 LeakTest headers intact, offline load OK | e2e-report-api29.txt, e2e-report-api29/ |
| `report --logs-only` | built by the service and with `--offline`; diagnostics/*, README, manifest | logs-only.zip, logs-only-offline.zip |
| app-e2e API 29 | 14/14 PASS | app-e2e-api29.txt |
| SIGSTOP the service | auto replacement after the 10 s hang (client call 43.0 s), `service stop --force` 13.4 s, five markers each stored exactly once | hung080.sh, hung080-run.txt |
| upgrade handoff | 0.6.0 and 0.7.0 service with stored data replaced by the 0.8.0 client; first status 11.1 s, `caught_up=1` after 10 s; marker once | upgrade080.sh, upgrade080-run.txt |
| `date` back-jump + crash + ANR | jumps listed, crashes and the real ANR attributed; see the finding below | clockjump080.sh, clockjump080-run.txt, clockjump080/ |
| set-root + whole-buffer re-read | no duplicates (service-e2e, with a stored clock jump in the device buffer) | service-e2e-api29.txt |
| perf | `scripts/perf.sh --check` and `--check --pr`: 136 metrics, 0 regressions | perf-check.txt, perf-check-pr.txt |

Finding (not a regression proof, see the release PR): after the back-jump the device DropBox re-dated 14 older ANR entries to the same time; the store lists two extra ANRs (`anr22`, `anr23`) from them, one with the pid of an ANR that was already listed (`anr10`).
