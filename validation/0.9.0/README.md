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

Local run of 2026-10-17 (Asia/Shanghai, 13:29-14:46) on **API 29** (Android 10, `google_apis/x86_64`), emulator-5556, software emulation (**TCG**, no KVM), started with `validation/scripts/emu-start.sh`; one emulator, strictly serial; `auto_time=1` restored afterwards. Binary: built from `main` at **`3c8b781`** (before #217 and before the release commit; the binary still printed `0.8.1`). Not re-run on the release commit; #217 (meta trim, `cursor_pruned` first=) is not covered.

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
| clock | #184 stamps and mtime 90 days old, `ANDROID_LOG_MCP_MAX_AGE=1h` | **F2: found, being fixed in 0.9.0 ([#218](https://github.com/s1crab/android-log-mcp/issues/218))** |

**F1 (test timing, not a product bug).** The `x1_root_m1d` straddle check of `clock-e2e.sh` schedules the clock jump 0.1 s after `am start`; on TCG `am start` needs seconds to reach `START`, so the jump lands before the launch and the launch is stamped outside the script's window. With the jump 4 s in, the product behaves correctly. The `#65` check of the script printed `dump=0` (its marker pattern does not match `-v epoch` output); recomputed by hand: 19 of 19 markers stored.

**F2 ([#218](https://github.com/s1crab/android-log-mcp/issues/218), P1, confirmed product bug; found, being fixed in 0.9.0 (#218); to be re-validated before release).** The retention sweep at service start (`layout.Sweep`) removes a segment of a device directory not yet owned by the service by **file mtime**, although its `seg_times` stamps would be re-anchored to now. Repro: copy a recorded root, set the segment's mtime and its `seg_times` to now - 90 days, `ANDROID_LOG_MCP_MAX_AGE=1h`, start the service with the device attached: `retention: removed 1 expired files`, lines on disk 11,241 -> 996. The CHANGELOG sentence "never an immediate mass delete after a clock fix" holds for stores the writer has opened, not for this sweep. Not fixed in this PR; the fix is being built on `fix/218-sweep-stamps` and ships in 0.9.0. **After the fix lands, the retention part of the clock matrix (`clock/retention`: future stamps, 90-day-old stamps and mtime, no `seg_times`) must be re-run on the new sha**, and this section updated.

**Not exercised.** `cap=time_budget` of `logcat_export` (needs about 300,000 redacted lines; unit tests only). The different-letter-case `--out` guard (#205) on a case-insensitive file system (the Linux file system is case-sensitive; unit test `TestProtectedOutDifferentCase` only). The host clock was not changed (retention after a host set-back was simulated by editing store state). API 35, Windows and macOS were not run.

## API 35 / Windows / macOS

*To fill.* Windows / macOS: CI only (**unverified** with a real or emulator-attached adb), same as 0.8.x.

## Where the raw evidence is

Raw logs, run outputs and the one-off scripts are **not in the repository** (AGENTS.md §3). The emulator part is in one archive, `0.9.0-evidence.tar.gz` (`MANIFEST.txt`, `SUMMARY.md`), sha256 `efa5bd706483f0fa14674bae50843b3ba6537c6cf6994f1b081ea01a53f78cc4`, to be attached to the GitHub Release `v0.9.0` after tagging. Until then it is on the release box at `/workspace/release-evidence/`. The other rows of the table above are not covered by that archive and are still to fill.
