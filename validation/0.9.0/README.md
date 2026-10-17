# 0.9.0 pre-release verification (AGENTS.md §3, #106)

Build: `main` + release commit (version **0.9.0**). Release PR: *(this PR)*. Changes since 0.8.1: https://github.com/s1crab/android-log-mcp/compare/v0.8.1...main, details in [CHANGELOG.md](../../CHANGELOG.md#090---2026-10-17).

## Contents of the release

- **Feature:** `logcat_export` tool and `android-log-mcp export` CLI, `export/` directory, `logcat_status` export line ([#172](https://github.com/s1crab/android-log-mcp/issues/172); PRs #174, #179, #203).
- **Store:** all-or-nothing `Append` (#182, #191, PR #201); follower promotion takeover and torn tail (#180, PR #190); `AppendAux` fresh line (#181, PR #198); search reads an index snapshot (#197, PR #202); meta reload on any change and failed save stays dirty (#183, #187, PR #208); retention from persisted segment stamps, not mtimes (#184, #186, #189, PR #210); `after=MaxUint64` and nil class ids (#188, PR #211); `RewriteAux` writer check (#196, PR #214).
- **Device / analysis / logcat:** split multi-line entry on resume (#192, PR #199); `setState` one critical section (#193, PR #200); per-iteration `recordOnce` context (#194, PR #212); times beyond int64 ns rejected (#195, PR #213); one-to-one DropBox ANR match (#176, PR #178); idempotent same-seq ANR repeat (#175, PR #215).
- **Export hardening:** `--out` guard by file identity / case (#205, PR #207); `--out` temp file in `<root>/export/` (#206, PR #209).
- No open release-blocking PRs at the time of writing; every PR above is merged to `main`.

## Verification summary

| item | result | evidence (in the archive) |
|---|---|---|
| CI (14/14) | *to fill: Actions on the release PR* | |
| service-e2e API 29 / API 35 | *to fill* | |
| e2e-report API 29 / API 35, `--logs-only` | *to fill* | |
| SIGSTOP the service | *to fill* | |
| upgrade handoff | *to fill* | |
| `date` back-jump + crash + ANR | *to fill* | |
| set-root + whole-buffer re-read | *to fill* | |
| race-gate `count=20` | *to fill: run id* | |
| perf | *to fill* | |

## Emulator validation

> **PENDING.** To be filled from `/workspace/release-evidence/summary.md` once the emulator validation run has finished. Until then the PR stays a draft.

## API 35 / Windows / macOS

*To fill.* Windows / macOS: CI only (**unverified** with a real or emulator-attached adb), same as 0.8.x.

## Where the raw evidence is

Raw logs and run outputs are **not in the repository** (AGENTS.md §3). They go into one archive, `android-log-mcp-0.9.0-evidence.tar.gz` (with `MANIFEST.txt` and a sha256), to be attached to the GitHub Release `v0.9.0` after tagging. Until then it is kept on the release box at `/workspace/release-evidence/`.
