# 0.7.1 device verification (emulator-5556, API 29, TCG)

Build: origin/0.7.x with #124, #125, #126 (tree identical to local branch 0.7.x+PR125 head d00bb78 used for the runs; binary reports `0.7.0`). Scratch XDG_CACHE_HOME per run, nice'd, no KVM.
`old-build-9c4c25d/` = first runs on an earlier combined build (before #125 was force-pushed); kept for reference only.

| item | result | evidence |
|---|---|---|
| (a) #109 SIGSTOP/auto-replace | PASS: first `logcat_status` after replace = `recording: 1 device(s)`, `avd-api29: recording; backfill in progress` (not idle); client call took 43.0 s (10 s hang detection + 10 s forced kill + restart); 2nd replace via `service stop --force` 13.4 s, recording again; every marker stored exactly once | hung071c-run.txt |
| (a) #109 0.6.0 service -> new client handoff | PASS: first status after handoff = `recording; backfill in progress (0 lines so far)`, took 11.1 s; +3 s same; +10 s `caught up` (0.6.0 service itself answered `idle` first until identity was read, as before) | upgrade071b-run.txt |
| (b) #111 real ANR, dump > 30 s | PASS (2 runs): am_anr 03:43:52 -> DropBox 03:44:24 (32 s) and am_anr 03:45:54 -> DropBox 03:46:23 (29 s): ONE entry each (anr8, anr9). Caveat: earlier run with 65 s gap split in two (old-build), see adopt-gap-probe.txt: `AdoptDropbox` still uses a 60 s window (new issue) | anr071b-run.txt, anr-live-crashes.txt, adopt-gap-probe.txt, old-build-9c4c25d/ |
| (c) #108 long cache path (84-char dir, primary socket would be 117 bytes) | PASS: socket at /tmp/alm-1000/dc9c9c389b4006b0.sock for both 0.6.0 and new client, matches CHANGELOG | upgrade-long071-run.txt |
| (d) service-e2e | 27 PASS / 2 FAIL (0.7.0 baseline 25/4). Now passing: "boundary line of the new root not stored again after switching back", "default root keeps its history ...". Still failing: "service records in the default root again", "marker logged during the pin was recorded" (both #115 backfill-timing items) | service-e2e.txt |
