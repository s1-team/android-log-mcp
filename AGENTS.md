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
3. **Every user-facing change adds an entry under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md)** (Keep a Changelog groups: Added / Changed / Deprecated / Removed / Fixed / Security; mark breaking changes with **Breaking** and link the issue). Internal-only changes (tests, refactoring, CI) need no entry.
4. **Commit with a reference to the issue so it closes**, for example `feat(store): … (closes #N)`.
5. **Push to `main`** (never force-push).
6. **Wait for CI to be fully green**, including the Windows tests and every npx-compat job. If it fails, fix forward.

## 3. Releases

Release only after **explicit sign-off from the user or maintainer**.

1. In `CHANGELOG.md`, move the `[Unreleased]` entries under a new `## [X.Y.Z] - YYYY-MM-DD` heading (leave `[Unreleased]` empty) and update the compare links at the bottom. Bump `npm/VERSION` and the default `Version` in `server/internal/mcpserver/server.go`, plus version examples in the READMEs. Commit `chore: release X.Y.Z`, push, and wait for CI to be green.
2. Tag the green commit `vX.Y.Z` (annotated) and push the tag. `.github/workflows/release.yml` then:
   - checks that the tag matches `npm/VERSION` and that `CHANGELOG.md` has a non-empty `X.Y.Z` section (`scripts/changelog-section.sh X.Y.Z`), before publishing anything;
   - publishes all npm packages;
   - creates the GitHub Release with the tarballs attached and **the `X.Y.Z` changelog section as its notes** (plus the compare link). Do not hand-edit release notes: fix the changelog on `main` and update the release with `gh release edit vX.Y.Z --notes-file <(scripts/changelog-section.sh X.Y.Z)` (a re-run of the workflow also updates the notes of an existing release, but only after the npm publish steps).
3. Breaking changes also need an upgrade note in the README ("Upgrading").
4. Verify that the npm `latest` tag points at the new version and that `npx -y android-log-mcp@X.Y.Z --version` works.

## 4. Versioning

- **Before 1.0:** breaking changes bump the **minor** version (0.x → 0.(x+1)), and they need a **Breaking** changelog entry and an upgrade note in the README.
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
- **Isolating tests:** the background service, its control directory (`os.UserCacheDir()/android-log-mcp`: `run/service.lock`, the socket, `config.json`) and the default data root are fixed per user. Tests must never touch the real user's control dir, config or service (a test that stops, replaces or `set-root`s the real service would break the user's recording).
  - Go: every package whose tests create a `device.Manager`, start a service or call `layout.DefaultDir` has `func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }` (`internal/testhome` points `XDG_CACHE_HOME`, `HOME`, `LocalAppData` at a temp dir). Multi-process tests in `cmd/android-log-mcp` give every child its own cache dir with `isoEnv` (and a nonexistent `ANDROID_LOG_MCP_ADB`, or the fake adb of `fakeADBEnv`); the spawned service inherits it.
  - Shell / manual runs: redirect the whole per-user cache dir, which moves the control dir, the service and the default root: Linux `XDG_CACHE_HOME=/tmp/x`, macOS `HOME=/tmp/x`, Windows `LocalAppData=C:\tmp\x`. Stop such a test service with the same environment (`… android-log-mcp service stop`).
  - Read-only checks of a dataset: `--offline <dir>` (in-process; never talks to the service, never records, prunes or creates files).
  - Live checks against the real service on the dev box: `scripts/service-e2e.sh` and `scripts/e2e-report.sh` (they use the real control dir on purpose; `service-e2e.sh` switches the root and back).
- **Start every child process as `procattr.Hide(exec.Command…(…))`** (`server/internal/procattr`). On Windows anything else opens a console window for each adb call; `TestEveryChildIsHidden` fails the build otherwise.
- **Never kill the adb server** (`adb kill-server`, or killing its process) in code, tests or scripts: other tools and the user depend on it. The service starts it outside its own process group and only its own `adb` children end with it.
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
- 测试隔离：后台服务、控制目录（`os.UserCacheDir()/android-log-mcp`：`run/`、socket、`config.json`）和默认数据根目录都是每用户固定的，测试绝不能碰真实用户的这些东西。Go 测试用 `testhome.Run` 的 TestMain，多进程测试用 `isoEnv`；手动运行用 `XDG_CACHE_HOME`（Linux）/ `HOME`（macOS）/ `LocalAppData`（Windows）把整个缓存目录指向临时目录。只读查看数据集用 `--offline <目录>`。
- 绝不要杀 adb server。
