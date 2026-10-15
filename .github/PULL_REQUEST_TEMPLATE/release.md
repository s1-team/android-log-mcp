<!-- Release PR template. Use: ?template=release.md in the PR URL. See AGENTS.md §3. -->
## Release X.Y.Z

Closes #

- [ ] `CHANGELOG.md` section moved, compare links updated
- [ ] `npm/VERSION`, default `Version` in `server/internal/mcpserver/server.go` and README version examples bumped

## Pre-release verification (AGENTS.md §3, #106)

Emulator API 29 and API 35, software emulation only (never KVM). Link or paste the evidence (committed under `validation/`) for each item.

- [ ] CI green on all OSes for the release commit
- [ ] `scripts/service-e2e.sh` on API 29 and API 35:
- [ ] `scripts/e2e-report.sh` on API 29 and API 35, including `--logs-only`:
- [ ] SIGSTOP the real service process: automatic replacement, `service stop --force`, no data loss (production timings):
- [ ] Upgrade: old-version service process + new MCP process or CLI process (version handoff, socket directory migration):
- [ ] `adb shell date -s` back-jump plus a real app crash and ANR: attribution still correct:
- [ ] `set-root`, then a whole-buffer re-read: no duplicates:
- [ ] `go test -race -count=20` on `service`, `store`, `device` and every package touched; `perf` job green:
- [ ] Windows / macOS tested with a real or emulator-attached adb, or marked **unverified** in the release notes:

Testing never blocks `main`; findings are fixed on a branch from the tag and released as a patch.
