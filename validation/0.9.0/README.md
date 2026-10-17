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

Local run of 2026-10-17 (Asia/Shanghai, 13:29-14:46) on **API 29** (Android 10, `google_apis/x86_64`), emulator-5556, software emulation (**TCG**, no KVM), started with `validation/scripts/emu-start.sh`; one emulator, strictly serial; `auto_time=1` restored afterwards. Binary: built from `main` at **`3c8b781`** (before #217, #219 and the release commit; the binary still printed `0.8.1`); the sweep/retention parts were re-run on `bd656e7` (see F2). Not re-run on the release commit; #217 (meta trim, `cursor_pruned` first=) is not covered.

| part | case | result |
|---|---|---|
| export | MCP and CLI default (unredacted: `# WARNING`, `redacted=0`, `note code=unredacted`); `redact=true` / `--redact` (hashed device id in the file name, e-mail and bearer masked); file in `<root>/export/` mode 0600; `logcat_status` export line; temp file in `<root>/export/`, none in `tmp/` | PASS (52 checks, 0 FAIL) |
| export | truncation `max_mb=1` with `note code=truncated`, `cap=max_mb`, `next: after=`; continuation parts concatenate to one full export; CLI `--max-mb` | PASS |
| export | `--out` guard (own entries `v1/ run/ tmp/ bin/ log/ export/ config.json`, subdirectory, symlinks refused; relative path and missing directory refused; `<root>/notes/x.txt` allowed); `--force`; `--offline` needs `--out` | PASS |
| recovery | #180: follower promoted after the writer was SIGKILLed with a torn last line injected: no duplicate seqs, gaps or joined lines; lines the follower had not seen are taken over | PASS |
| recovery | #182/#191: real `ENOSPC` on a tmpfs data root, then space freed: the recorder recovers by itself; all 900 test lines present once in memory and on disk, no burnt seqs, no partial lines (note: `last_error` stays in `logcat_status` after recovery) | PASS |
| clock | `scripts/clock-e2e.sh` set-back matrix (-10 min, -3 h, -2 d, 1970, +1 d, +1 y, back; #48, #52, #54, #65) | 110 PASS, 2 FAIL (F1: test timing, no product bug) |
| clock | #54 launch straddling a set-back (jump 4 s into the launch): launch kept, steps/TTID `unreliable=clock_jump` | PASS |
| clock | #183 follower reloads `meta.json` after a device clock set-back (jumps 18 -> 20 seen without restart) | PASS |
| clock | #189/#184 store state after a **simulated** host-clock set-back (stamps and mtime 3 days ahead; no `seg_times` plus future mtime): clamped to now, data kept | PASS |
| clock | #184/#218 stamps and mtime 90 days old, `ANDROID_LOG_MCP_MAX_AGE=1h` | F2 on `3c8b781`: segment deleted. **Fixed by #219; re-run on `bd656e7`: PASS** (segment and lines kept, stamps re-anchored) |
| clock (re-run on `bd656e7`) | sweep of unowned device dirs: expired dir with `seg_times` removed; old store without `seg_times` and old mtimes removed (mtime fallback); fresh dir kept; clock-corrected dir (fresh stamps, old mtimes) kept with all 200 lines | PASS (5 checks) |
| clock (re-run on `bd656e7`) | retention/meta cases again: future stamps, 90-day-old stamps, no `seg_times` + future mtime; #183 follower meta reload (jumps 22 -> 24) | PASS (7 + 3 checks) |

**F1 (test timing, not a product bug).** The `x1_root_m1d` straddle check of `clock-e2e.sh` schedules the clock jump 0.1 s after `am start`; on TCG `am start` needs seconds to reach `START`, so the jump lands before the launch and the launch is stamped outside the script's window. With the jump 4 s in, the product behaves correctly. The `#65` check of the script printed `dump=0` (its marker pattern does not match `-v epoch` output); recomputed by hand: 19 of 19 markers stored.

**F2 ([#218](https://github.com/s1crab/android-log-mcp/issues/218), P1: fixed by [#219](https://github.com/s1crab/android-log-mcp/pull/219), re-validated).** On `3c8b781` the retention sweep at service start (`layout.Sweep`) removed a segment of a device directory not yet owned by the service by **file mtime**, although its `seg_times` stamps would be re-anchored to now (repro `stale-repro.sh`: segment and `seg_times` 90 days old, `ANDROID_LOG_MCP_MAX_AGE=1h`: `retention: removed 1 expired files`, lines on disk 11,241 -> 996). #219 (main at **`bd656e7`**) makes the sweep expire by the store's stamps. **Re-run on `bd656e7`** (2026-10-17 14:55-15:03; same emulator, same API 29 TCG; only the sweep/retention/meta parts were repeated, the other parts keep the validated sha `3c8b781`): `stale-repro.sh` now keeps the segment (11,241 -> 12,397 lines, nothing lost), and the cases above pass. A directory that is genuinely expired is still removed.

**Known harmless note (#219 review).** Removing a whole device directory still checks the mtime of its non-segment files (`meta.json`, aux files). After a clock correction a rewritten `meta.json` can therefore keep an all-expired, empty directory for one more sweep. No data is lost and the code is not changed (observed: the directory with expired stamps and a recent `meta.json` stays after its segment is removed).

**Not exercised.** `cap=time_budget` of `logcat_export` (needs about 300,000 redacted lines; unit tests only). The different-letter-case `--out` guard (#205) on a case-insensitive file system (the Linux file system is case-sensitive; unit test `TestProtectedOutDifferentCase` only). The host clock was not changed (retention after a host set-back was simulated by editing store state). API 35, Windows and macOS were not run.

## API 35 / Windows / macOS

*To fill.* Windows / macOS: CI only (**unverified** with a real or emulator-attached adb), same as 0.8.x.

## Where the raw evidence is

Raw logs, run outputs and the one-off scripts are **not in the repository** (AGENTS.md §3). The emulator part is in one archive, `0.9.0-evidence.tar.gz` (`MANIFEST.txt`, `SUMMARY.md`), sha256 `82c36a6cd9ea447468f03ce6bd08c51860b7fe14f4fc12dcaeb8b3aff7a1d1cb`, to be attached to the GitHub Release `v0.9.0` after tagging. Until then it is on the release box at `/workspace/release-evidence/`. The other rows of the table above are not covered by that archive and are still to fill.
