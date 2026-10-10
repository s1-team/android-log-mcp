# AGENTS.md

Guidelines for any AI agent or human contributor working in this repository.

## 1. Start with the goals

Read [docs/GOALS.md](docs/GOALS.md) first, and check every change against its principles and must-nots. If a change needs to bend one, raise it in the issue before writing code.

## 2. Workflow

1. **Open an issue first.** Describe the problem, the proposed change and the acceptance criteria.
2. **Implement with tests.** Add unit or regression tests for the behavior you change, and run:
   - `go vet ./...` and `go test ./...` in `server/`;
   - `-race` on the packages you touched;
   - `GOOS=windows go vet ./...`.
3. **Commit with a reference to the issue so it closes**, for example `feat(store): … (closes #N)`.
4. **Push to `main`** (never force-push).
5. **Wait for CI to be fully green**, including the Windows tests and every npx-compat job. If it fails, fix forward.

## 3. Releases

Release only after **explicit sign-off from the user or maintainer**.

1. Bump `npm/VERSION` and the default `Version` in `server/internal/mcpserver/server.go`, plus version examples in the READMEs. Commit `chore: release X.Y.Z`, push, and wait for CI to be green.
2. Tag the green commit `vX.Y.Z` (annotated) and push the tag. `.github/workflows/release.yml` then:
   - publishes all npm packages;
   - creates the GitHub Release with the tarballs attached.
3. If there are user-facing changes, replace the auto-generated release notes with written, English-first notes. Keep the "Full Changelog" line at the end.
4. Verify that the npm `latest` tag points at the new version and that `npx -y android-log-mcp@X.Y.Z --version` works.

## 4. Versioning

- **Before 1.0:** breaking changes bump the **minor** version (0.x → 0.(x+1)), and they need an upgrade note in the README and the release notes.
- **After 1.0:** breaking changes bump the **major** version.
- Changing a default that deletes or hides user data counts as breaking. One example is retention.

## 5. Language

Repository artifacts (docs, issues, commit messages, release notes) are **English-first**, with a short Chinese section at the end where useful. Some older docs are still Chinese-first; convert them when you substantially edit them.

## 6. Safety on the dev box

- **Never start KVM-accelerated emulators, and never touch `/dev/kvm`.** Nested KVM crashes the host. Use the device or emulator that is already running.
- Do not change device settings or app state beyond what a test explicitly requires.

## 7. Where things are

- **Layout:** the "仓库结构" (repository layout) section of [README.md](README.md), and [server/README.md](server/README.md) for the Go packages.
- **Design:** [docs/design-v3.md](docs/design-v3.md) (current) and [docs/research-v2.md](docs/research-v2.md) (background research).
- **Testing:**
  - `go test ./...`, including the API 29 fixture replay tests;
  - `scripts/e2e-report.sh` for the end-to-end redaction check on a device;
  - `scripts/compat-probe.sh` / `android-log-mcp compat` for the device compatibility probe;
  - the evidence and procedures in [validation/README.md](validation/README.md).
- **Isolating tests:** owner locks live in a fixed per-user directory, `os.UserCacheDir()/android-log-mcp/owners`, whatever the data root. Tests must never touch the real user's owners dir (a test that takes a device's lock there would stop the user's recorder from recording it).
  - The data root of every process comes from the per-user config file `os.UserConfigDir()/android-log-mcp/config.json` (written by `set-root`). Tests must never touch the real one either: a test that writes it would move the user's running recorders.
  - Go: every package whose tests create a `device.Manager` or call `layout.DefaultDir` / `DefaultOwnersDir` / `ConfigPath` / `ConfiguredRoot` / `WriteConfig` / `SetRoot` has `func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }`. `internal/testhome` points `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`, `HOME`, `LocalAppData`, `AppData` at a temp dir, and refuses to run unless both the cache dir and the config dir resolve inside it. In tests, set `Manager.Root = layout.Root{Dir: base, OwnersDir: <temp>}` (see `internal/device/ownership_test.go`). Leave `Manager.FollowConfig` off unless the test is about the configured root.
  - Shell / manual runs: redirect both per-user dirs, which moves the owners dir and the config too:
    - Linux: `XDG_CACHE_HOME=/tmp/x/cache XDG_CONFIG_HOME=/tmp/x/config`;
    - macOS: `HOME=/tmp/x`;
    - Windows: `LocalAppData=C:\tmp\x\local AppData=C:\tmp\x\roaming`.
    Never run `set-root` against the real config except on purpose: it moves every running recorder of the user.
  - Read-only checks of a dataset: `--offline <dir>` (never records, prunes or creates files).
- **Packaging:** run `scripts/build-npm.sh [VERSION] [--pack]`. CI runs the npx smoke tests on every Node version in the matrix.

---

## 中文摘要

- 先读 `docs/GOALS.md`，每个改动都要对照其中的原则和禁止事项。
- 流程：
  1. 先开 issue；
  2. 实现并补测试（`go vet`、`go test`、改动的包加 `-race`、Windows 交叉 vet）；
  3. 提交信息引用 issue，让它自动关闭；
  4. push 到 main，不要 force-push；
  5. 等 CI 全绿（包括 Windows 和 npx-compat）。
- 发版：必须先得到用户明确同意。
  1. 改 `npm/VERSION` 和 `server.go` 里的默认版本号；
  2. CI 绿了以后打 `vX.Y.Z` tag，`release.yml` 会发布 npm 包并创建 GitHub Release；
  3. 有面向用户的变化时，把自动生成的说明换成手写的说明（英文为主）。
- 版本规则：1.0 之前，破坏性变更升 minor 版本，并附升级说明；1.0 之后升 major 版本。
- 语言：仓库产出物英文为主，中文为辅。
- 安全：开发机上禁止启动 KVM 模拟器，也不要碰 `/dev/kvm`。
- 测试隔离：owner 锁固定在 `os.UserCacheDir()/android-log-mcp/owners`，与数据根目录无关，测试绝不能碰真实用户的这个目录。数据根目录来自每用户配置文件 `os.UserConfigDir()/android-log-mcp/config.json`（由 `set-root` 写入），测试同样不能碰它。Go 测试用 `testhome.Run` 的 TestMain（同时重定向缓存目录和配置目录）；手动运行时把缓存目录和配置目录都指向临时目录：Linux 用 `XDG_CACHE_HOME` + `XDG_CONFIG_HOME`，macOS 用 `HOME`，Windows 用 `LocalAppData` + `AppData`。`ANDROID_LOG_MCP_HOME` 已移除；只读查看数据集用 `--offline <目录>`。
