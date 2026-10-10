# `report` 端到端验证（emulator-5556，Android 10 / API 29，TCG，2026-10-10 19:33 +08:00，issue #7 后重跑）

命令：`scripts/e2e-report.sh emulator-5556 validation/report-e2e`（run id `LT1791631955`）。

流程：用 `adb shell log -t LeakTest '…'` 注入 23 条带假密钥的日志（16 种类型，含 URL 主机和裸主机名两条 `domain`，见 `injected-secret-types.txt`；原文在脚本里）和 6 条结构反例（pid/tid、epoch 时间戳、十六进制地址和寄存器列、native 堆栈帧、版本号和日期、Java 堆栈帧 / 包名 / 类名、intent action、系统属性名、`base.apk` `libc.so` `base.odex` `main.xml` `app.json` 文件名）；等后台录制进程（0.3.0-dev+issue7，pid 445829，数据目录 `~/.cache/android-log-mcp/v1/avd-api29`）落盘后执行 `android-log-mcp report --serial emulator-5556 --since 20m --note …`，解压后检查。

结果（`verify.txt`）：**PASS**

- 23 个假密钥在 zip 的任何文件里都找不到，manifest 里 16 种类型都有命中计数。
- 域名只替换主机：`--> POST https://[REDACTED:domain#5]:8443/v1/login?page=2 (64-byte body)`、`Unable to resolve host [REDACTED:domain#6]: No address …`；其他行里的 URL（`https://cdn…/a.png?…`、`postgres://bob:…@db…:5432/app`）也只换主机。窗口内录制到的其他日志中，域名规则只命中测试 App 的 OkHttp 请求主机和 `Unable to resolve host "…"`，没有误判。
- 29 条 LeakTest 记录在报告里的时间、uid、pid、tid、级别、tag、buffer 和原始录制完全一致；6 条结构反例的正文一字未改。
- note 里的邮箱被打码，并且和日志里同一个邮箱用的是同一个编号。
- 报告的 `store/`（`store/v1/<设备ID>/`）用 `android-log-mcp --offline …/store call logcat_search` 只读加载，29 条都能查到（`offline-search.txt`）。

| 文件 | 内容 |
|---|---|
| `verify.txt` | 检查脚本输出：报告中的 LeakTest 记录（已打码）、manifest 打码计数、zip 文件列表、结论 |
| `report-logcat-LeakTest.txt` | 报告内 `devices/emulator-5556/logcat/ccb368e7.txt` 中本次的 LeakTest 行 |
| `offline-search.txt` | 离线加载报告后 `logcat_search` 的输出 |
| `manifest.json` | 本次报告的 manifest（窗口 20 分钟内也包含之前几次试跑注入的 LeakTest 行，所以计数大于 23） |
| `report.stderr.txt` | `report` 子命令打印的摘要 |
| `injected-secret-types.txt` | 注入的类型和条数 |
