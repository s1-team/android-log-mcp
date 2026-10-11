# AGENTS.md

Guidelines for any AI agent or human contributor working in this repository.

## 1. Start with the goals

Read [docs/GOALS.md](docs/GOALS.md) first, and check every change against its principles and must-nots. If a change needs to bend one, raise it in the issue before writing code.

## 2. Workflow

1. **Open an issue first.** Describe the problem, the proposed change and the acceptance criteria.
   - **Every issue gets a priority label and a milestone.** Priority labels:
     - `priority: P0`: silent data loss, wrong results or a crash; must ship in the current milestone.
     - `priority: P1`: important; planned for the next release.
     - `priority: P2`: nice to have; no commitment.
   - Agents set the priority label when it is clear and leave it unset when unsure.
   - Milestones and version planning are set by the release owner with the user, never by the implementing agent. When you open a new issue, set the priority label if it is clear, and never set a milestone.
2. **Implement with tests.** Add unit or regression tests for the behavior you change, and run:
   - `go vet ./...` and `go test ./...` in `server/`;
   - `-race` on the packages you touched;
   - `GOOS=windows go vet ./...`.
3. **Every user-facing change adds an entry under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md)** (Keep a Changelog groups: Added / Changed / Deprecated / Removed / Fixed / Security; mark breaking changes with **Breaking** and link the issue). Internal-only changes (tests, refactoring, CI) need no entry.
   **Bug fixes are described only in the CHANGELOG**, never in the README: the README describes the current behavior and options neutrally (no "before X.Y.Z this popped up / was broken" notes). Breaking changes still get an upgrade note in the README ("Upgrading").
4. **Work on a feature branch, never on `main`.** `main` is branch-protected: direct pushes are rejected (admins included), history must stay linear, and nobody force-pushes. Commit with a reference to the issue, for example `feat(store): … (closes #N)` (`refs #N` on intermediate commits).
5. **Open a pull request** from the branch with `Closes #N` in the PR body (and in the commit message), so the issue closes on merge. A squash merge uses the PR title and body as the commit message.
6. **Wait for CI to be fully green on the PR**: the required checks are `test` (ubuntu, macos, windows) and `npm-pack`, and the branch must be up to date with `main`. The npx-compat jobs and every other job must pass too. If anything fails, fix it on the branch. If `main` moved, rebase the branch on `origin/main` and push it again (`git push --force-with-lease` on your own feature branch is fine; never on `main`).
7. **Merge with rebase or squash** (`gh pr merge --rebase` or `--squash`; merge commits are disabled). No review approval is required, so you can merge your own PR once the checks are green. Then delete the branch.

## 3. Releases

Release only after **explicit sign-off from the user or maintainer**.

1. In `CHANGELOG.md`, move the `[Unreleased]` entries under a new `## [X.Y.Z] - YYYY-MM-DD` heading (leave `[Unreleased]` empty) and update the compare links at the bottom. Bump `npm/VERSION` and the default `Version` in `server/internal/mcpserver/server.go`, plus version examples in the READMEs. Commit `chore: release X.Y.Z` on a branch, open a PR, wait for CI to be green and merge it (§2).
2. Tag the merged, green commit on `main` `vX.Y.Z` (annotated) and push the tag. `.github/workflows/release.yml` then:
   - checks that the tag matches `npm/VERSION` and that `CHANGELOG.md` has a non-empty `X.Y.Z` section (`scripts/changelog-section.sh X.Y.Z`), before publishing anything;
   - publishes all npm packages;
   - creates the GitHub Release with the tarballs attached and **the `X.Y.Z` changelog section as its notes** (plus the compare link). Do not hand-edit release notes: fix the changelog on `main` (through a PR) and update the release with `gh release edit vX.Y.Z --notes-file <(scripts/changelog-section.sh X.Y.Z)` (a re-run of the workflow also updates the notes of an existing release, but only after the npm publish steps).
3. Breaking changes also need an upgrade note in the README ("Upgrading").
4. Verify that the npm `latest` tag points at the new version and that `npx -y android-log-mcp@X.Y.Z --version` works.

## 4. Versioning

- **Before 1.0:** breaking changes bump the **minor** version (0.x → 0.(x+1)), and they need a **Breaking** changelog entry and an upgrade note in the README.
- **After 1.0:** breaking changes bump the **major** version.
- Changing a default that deletes or hides user data counts as breaking. One example is retention.

## 5. Language

Repository artifacts (docs, issues, commit messages, release notes) are **English-first**, with a short Chinese section at the end where useful. Some older docs are still Chinese-first; convert them when you substantially edit them.

**Terminology:** issues, PRs, docs, code comments, logs and tool output use the terms in [docs/glossary.md](docs/glossary.md): MCP process, service process (short: service), CLI process, adb server, adb child, device process, MCP host. Never write a bare "process", "server", "daemon", "client" or "服务". Existing code identifiers and output strings that still use older words are listed there; do not add new ones.

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
  - `scripts/perf.sh` for the performance guard (CI job `perf`): output bytes, allocations, latency, memory against `perf/baseline.json` (pull requests: `--pr`, a small history against `perf/baseline-pr.json`). A change that intentionally moves a metric re-records both baselines (`scripts/perf.sh --update && scripts/perf.sh --update --pr`) in the same PR and says why ([docs/performance.md](docs/performance.md));
  - the evidence and procedures in [validation/README.md](validation/README.md).
- **Isolating tests:** the service process, its control directory (`os.UserCacheDir()/android-log-mcp`: `run/service.lock`, the socket, `config.json`) and the default data root are fixed per user. Tests must never touch the real user's control dir, config or service (a test that stops, replaces or `set-root`s the real service would break the user's recording).
  - Go: every package whose tests create a `device.Manager`, start a service or call `layout.DefaultDir` has `func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }` (`internal/testhome` points `XDG_CACHE_HOME`, `HOME`, `LocalAppData` at a temp dir). Multi-process tests in `cmd/android-log-mcp` give every child its own cache dir with `isoEnv` (and a nonexistent `ANDROID_LOG_MCP_ADB`, or the fake adb of `fakeADBEnv`); the spawned service inherits it.
  - Shell / manual runs: redirect the whole per-user cache dir, which moves the control dir, the service and the default root: Linux `XDG_CACHE_HOME=/tmp/x`, macOS `HOME=/tmp/x`, Windows `LocalAppData=C:\tmp\x`. Stop such a test service with the same environment (`… android-log-mcp service stop`).
  - Read-only checks of a dataset: `--offline <dir>` (in-process; never talks to the service, never records, prunes or creates files).
  - Live checks on the dev box: `scripts/service-e2e.sh` and `scripts/e2e-report.sh` use the per-user control dir of their environment (`service-e2e.sh` kills the service and switches the root and back), so run them with `XDG_CACHE_HOME` / `HOME` pointing at a scratch directory unless you mean to test the real service.
  - Fake adb: `internal/fakeadb` is a scriptable adb (devices, log buffers, `date`, per-line delay, an invocation log) for unit and multi-process tests; use it to assert which adb commands run.
- **Start every child process as `procattr.Hide(exec.Command…(…))`** (`server/internal/procattr`). On Windows anything else opens a console window for each adb call; `TestEveryChildIsHidden` fails the build otherwise.
- **An idle service runs no adb.** Recording starts on use (issue #11): any new code path that runs adb in the service must go through a device that a log tool call, `report` or a pin started (`Manager.Want` / `mcpserver` `use`), and must stop with it. Tests in `device/lazy_test.go` and `cmd/android-log-mcp/lazy_test.go` assert that an idle service makes no adb call.
- **Never kill the adb server** (`adb kill-server`, or killing its process) in code, tests or scripts: other tools and the user depend on it. The service starts it outside its own process group and only its own `adb` children end with it.
- **Packaging:** run `scripts/build-npm.sh [VERSION] [--pack]`. CI runs the npx smoke tests on every Node version in the matrix.

---

## 中文摘要

- 先读 `docs/GOALS.md`，每个改动都要对照其中的原则和禁止事项。
- 流程：
  1. 先开 issue；每个 issue 要有优先级标签和里程碑：`priority: P0`（静默丢数据、结果错误或崩溃，必须在当前里程碑发布）、`priority: P1`（重要，下个版本）、`priority: P2`（锦上添花，不承诺）。优先级明确时由 agent 设置，不确定就不设；里程碑和版本规划由发布负责人与用户确定，实现的 agent 不设。新开 issue 时，优先级明确就设标签，永远不设里程碑；
  2. 实现并补测试（`go vet`、`go test`、改动的包加 `-race`、Windows 交叉 vet）；
  3. 不直接提交到 main（main 有分支保护，禁止直接 push 和 force-push）：在功能分支上提交，开 PR，PR 正文写 `Closes #N`；
  4. 等 PR 的 CI 全绿（必需检查：三个系统的 `test` 和 `npm-pack`，分支要与 main 同步；npx-compat 等其他 job 也要通过），main 有更新就 rebase 后再推；
  5. 用 rebase 或 squash 合并（`gh pr merge --rebase` / `--squash`，不允许 merge commit），不需要审批。
- 发版：必须先得到用户明确同意。
  1. 改 `npm/VERSION` 和 `server.go` 里的默认版本号，同样走 PR 合并；
  2. CI 绿了以后打 `vX.Y.Z` tag，`release.yml` 会发布 npm 包并创建 GitHub Release；
  3. 有面向用户的变化时，把自动生成的说明换成手写的说明（英文为主）。
- 版本规则：1.0 之前，破坏性变更升 minor 版本，并附升级说明；1.0 之后升 major 版本。
- 语言：仓库产出物英文为主，中文为辅。
- 术语：issue、PR、文档、代码注释、日志和工具输出统一使用 [docs/glossary.md](docs/glossary.md) 中的术语（MCP 进程、后台服务进程、CLI 进程、adb server、adb 子进程、设备进程、MCP 宿主），禁止单独使用“进程 / server / daemon / client / 服务”。
- 安全：开发机上禁止启动 KVM 模拟器，也不要碰 `/dev/kvm`。
- 测试隔离：后台服务进程、控制目录（`os.UserCacheDir()/android-log-mcp`：`run/`、socket、`config.json`）和默认数据根目录都是每用户固定的，测试绝不能碰真实用户的这些东西。Go 测试用 `testhome.Run` 的 TestMain，多进程测试用 `isoEnv`；手动运行用 `XDG_CACHE_HOME`（Linux）/ `HOME`（macOS）/ `LocalAppData`（Windows）把整个缓存目录指向临时目录。只读查看数据集用 `--offline <目录>`。`scripts/service-e2e.sh` / `e2e-report.sh` 也要这样隔离，除非就是要测真实服务。`internal/fakeadb` 是可编程的假 adb，用来断言运行了哪些 adb 命令。
- 空闲的服务不运行任何 adb：录制按需开始（#11），服务里新增的 adb 调用必须属于已被工具调用、`report` 或 pin 启动的设备，并随它停止。
- 绝不要杀 adb server。
