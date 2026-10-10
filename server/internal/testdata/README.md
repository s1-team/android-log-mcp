# Test fixtures

`api29/` holds real, unedited output of an Android 10 / API 29 emulator
(`emulator-5556`, software emulation, timezone +08:00, test app
`com.example.logtest` uid 10146, source not included). Every `*.logcat.txt` /
`session*-raw.log` was captured with exactly the recorder's flags:

    adb logcat -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable -T 1

`dropbox-*.txt` are `dumpsys dropbox --print <tag>` outputs. `api29/README.md`
(Chinese) describes every scenario; trigger commands are in
`validation/api29/*.cmd.txt`. The regression tests in
`internal/mcpserver/replay_test.go` replay them through the real ingest
pipeline (`device.Pipeline`) and call the MCP tools.
