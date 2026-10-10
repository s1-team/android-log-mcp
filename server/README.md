# server/（Go 模块）

`github.com/s1crab/android-log-mcp/server`：android-log-mcp 的全部 Go 源码。面向用户的说明见仓库根目录的 [README.md](../README.md)。

```bash
go vet ./... && go test ./...                          # 单元测试，以及 API 29 fixture 回放回归测试
go build -o android-log-mcp ./cmd/android-log-mcp      # 本机构建
go build -ldflags "-X main.version=0.1.0" ./cmd/android-log-mcp   # 写入版本号（release 构建就是这样做的）
REPLAY_DUMP=1 go test ./internal/mcpserver -run ReplayExplore -v  # 打印 fixture 回放后各工具的完整输出
```

| 包 | 职责 |
|---|---|
| `cmd/android-log-mcp` | 入口：MCP stdio 服务器，以及 `call` 调试子命令 |
| `internal/device` | 设备管理（adb track-devices）、录制、ingest pipeline（`Pipeline`，fixture 回放也走这里）、DropBox |
| `internal/logcat` | logcat 行解析、多行合并、`-v printable` 反转义 |
| `internal/procs` | pid 到进程的生命周期表（am_proc_*、ps 快照、force-stop、重启） |
| `internal/analysis` | 崩溃、ANR、tombstone 识别和 DropBox 合并；启动时间线 |
| `internal/filter` | Android Studio Logcat 过滤语法的解析器（移植，含 AS 测试） |
| `internal/store` | JSONL 分段存储；单写者多读者（flock） |
| `internal/mcpserver` | 4 个工具的实现、渲染，以及回归测试 |
| `internal/testdata/api29` | 真实设备 fixture，见其中的 README |
| `third_party_ref` | AOSP / Android Studio 参考源码（Apache-2.0，不参与编译） |
