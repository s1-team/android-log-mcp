# 0.7.0 supplemental testing progress (v0.7.0 = 52ad69c, API29 emulator-5556 TCG)
Raw evidence: /workspace/ev/*, copied to worktree /workspace/alm-wt/ev-070/validation/0.7.0-supplemental/ (branch test-0.7.0-supplemental-evidence, no PR yet)
(Resumed 23:15 CST by second worker; items 1-5 done by first worker.)

| # | item | result | numbers |
|---|---|---|---|
| 1 | service-e2e.sh | FAIL 4 of 29 -> issue #115 | 25 PASS / 4 FAIL, 4m25s |
| 1 | e2e-report.sh (+--logs-only) | PASS | 23 secrets/16 kinds redacted, 29 headers intact, offline load OK, 57s |
| 2 | SIGSTOP real service | PASS w/ bug #109 | auto-replace after 10s hang, call 41.9s, old pid dead, no orphan adb; `stop --force` 13.4s; markers each counted once (6 HUNG lines final) |
| 3 | 0.6.0 svc + 0.7.0 client | PASS handoff; socket move FAIL -> #108 | handoff ok, 0.6 client vs 0.7 service compatible; long path still /tmp/alm-1000/<hash>.sock; first status after handoff not recording #109 |
| 4 | clock back-jump + crash/ANR | PASS w/ bug #111 | 3 runs (clockjump, 2, 3); ANR split in two (33s dump > 30s window); see ev/clockjump3 |
| 5 | set-root then re-read | PASS | old root 25862 lines distinct, new 20; 0 dups, 0 keys in both roots |
| 6 | go test -race -count=20 | RUNNING (ev/race-progress.txt) | packages store ipc service analysis device mcpserver |
