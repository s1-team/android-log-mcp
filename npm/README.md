# npm 打包

采用 esbuild / biome 的做法：

- `android-log-mcp/`：主包。只有一个 Node 启动器 `bin/android-log-mcp.js`，在 `optionalDependencies` 里列出 6 个平台包。npm 会根据平台包 `package.json` 里的 `os` / `cpu` 字段，只安装与当前机器匹配的那一个。
- `platform.template.json`：平台包的模板，包名为 `android-log-mcp-{linux,darwin,win32}-{x64,arm64}`，包里只有 `bin/android-log-mcp[.exe]`。
- 启动器：
  - 用 `spawn(bin, args, {stdio: "inherit"})` 直接把 stdio 交给 Go 二进制，自己不往 stdout 写任何东西（stdout 是 MCP 的通道）。
  - 会转发 SIGINT、SIGTERM、SIGHUP 等信号，并原样返回子进程的退出码。
  - 找不到平台包时，往 stderr 打印提示并以 1 退出。
  - 设置了 `ANDROID_LOG_MCP_BINARY` 时，改用该路径下的二进制。

```bash
scripts/build-npm.sh            # 版本号取自 npm/VERSION，交叉编译到 npm/dist/
scripts/build-npm.sh 0.1.0 --pack   # 另外把所有包 npm pack 到 npm/dist/tarballs/
python3 npm/test/mcp_smoke.py npx -y android-log-mcp   # 通过 stdio 依次发送 initialize、tools/list、logcat_status
```

`npm/test/verify-0.1.0.txt` 是本地验证记录：用本机 verdaccio 作为 registry，跑通了 `npx -y android-log-mcp`。
