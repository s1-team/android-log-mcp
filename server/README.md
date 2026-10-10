# server/（Go 模块）

`github.com/s1crab/android-log-mcp/server`：android-log-mcp 的全部 Go 源码。面向用户的说明见仓库根目录的 [README.md](../README.md)。

```bash
go vet ./... && go test ./...                          # 单元测试，以及 API 29 fixture 回放回归测试
go build -o android-log-mcp ./cmd/android-log-mcp      # 本机构建
go build -ldflags "-X main.version=0.3.0" ./cmd/android-log-mcp   # 写入版本号（release 构建就是这样做的）
REPLAY_DUMP=1 go test ./internal/mcpserver -run ReplayExplore -v  # 打印 fixture 回放后各工具的完整输出
../scripts/e2e-report.sh emulator-5556 [证据目录]          # 真机 / 模拟器上的 report 打码端到端检查（需要 adb）
```

| 包 | 职责 |
|---|---|
| `cmd/android-log-mcp` | 入口：MCP stdio 服务器，`call` 调试子命令，`report` 诊断包子命令，`--offline <dir>`（只读打开数据集：不用 adb，不录制、不清理、不创建文件） |
| `internal/device` | 设备管理（adb track-devices；按稳定设备 ID 管理，`identity.go`；所有权、旧目录迁移）、录制、ingest pipeline（`Pipeline`，fixture 回放也走这里）、DropBox |
| `internal/logcat` | logcat 行解析、多行合并、`-v printable` 反转义 |
| `internal/procs` | pid 到进程的生命周期表（am_proc_*、ps 快照、force-stop、重启） |
| `internal/analysis` | 崩溃、ANR、tombstone 识别和 DropBox 合并；启动时间线 |
| `internal/filter` | Android Studio Logcat 过滤语法的解析器（移植，含 AS 测试） |
| `internal/layout` | Data root: `v1/<device-id>/` layout, owner locks in the fixed per-user `owners/` dir, and the sweep (retention + total cap over every `v<N>/` format, `tmp/`, `run/`; stale sockets / owner locks). Anything else under the root (pre-0.4 dirs) is ignored |
| `internal/testhome` | `TestMain` helper that redirects the per-user cache dir so tests never touch the real owners dir |
| `internal/store` | JSONL 分段存储；单写者多读者（flock）；最近窗口和按块的历史缓存（条数 / 字节上限，空闲释放）；保留期（`ANDROID_LOG_MCP_MAX_AGE`）、目录总量上限和低剩余空间暂停 |
| `internal/mcpserver` | 4 个工具的实现、渲染，以及回归测试（含 replay-from-report） |
| `internal/compat` | `compat` 子命令：只读探测设备的 logcat / ps / pm / dumpsys 功能，并用录制时的解析器评估 |
| `internal/redact` | 敏感信息打码规则（`[REDACTED:<类型>#n]`，同值同号，HMAC 随机密钥） |
| `internal/report` | `report` 子命令：按窗口 / 包名 / 大小裁剪、打码、生成 zip 和 manifest |
| `internal/testdata/api29` | 真实设备 fixture，见其中的 README |
| `third_party_ref` | AOSP / Android Studio 参考源码（Apache-2.0，不参与编译） |
