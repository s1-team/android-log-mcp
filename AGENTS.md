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
   **Bug fixes are described only in the CHANGELOG**, never in the README: the README describes the current behavior and options neutrally (no "before X.Y.Z this popped up / was broken" notes). **Upgrade notes and breaking changes are written only in the CHANGELOG** (a **Breaking** entry says what the user has to do); they are never repeated in the README.
4. **Work on a dev branch (`feature/…` or `fix/…`, see rule 9), never on `main`.** `main` is branch-protected: direct pushes are rejected (admins included), history must stay linear, and nobody force-pushes. Commit with a reference to the issue, for example `feat(store): … (closes #N)` (`refs #N` on intermediate commits).
5. **Open a pull request** from the branch with `Closes #N` in the PR body (and in the commit message), so the issue closes on merge. A squash merge uses the PR title and body as the commit message.
6. **Wait for CI to be fully green on the PR**: the required checks are `test` (ubuntu, macos, windows) and `npm-pack`, and the branch must be up to date with `main`. The npx-compat jobs and every other job must pass too. If anything fails, fix it on the branch. If `main` moved, rebase the branch on `origin/main` and push it again (`git push --force-with-lease` on your own feature branch is fine; never on `main`).
7. **Merge with rebase or squash** (`gh pr merge --rebase` or `--squash`; merge commits are disabled). No review approval is required, so you can merge your own PR once the checks are green. Then delete the branch.
8. **Scheduling: the implementing agent owns it.** The implementing agent schedules all code work; the release owner only sets priorities and milestones, reviews designs and decides releases.
   - Model the open work as a **dependency graph**: nodes are issues or tasks; edges are shared code areas (two changes to the same files), the single emulator, design approvals and release gates.
   - Run independent nodes in parallel, each in its own git worktree from the latest `main`.
   - **Never sit blocked.** While a node waits for a review, CI or the emulator, take the next unblocked node.
   - Design reviews still go to the release owner, but unrelated nodes continue in the meantime; only the nodes that depend on the design wait.
   - Keep the graph current in the pinned tracking issue [#67](https://github.com/s1crab/android-log-mcp/issues/67): update a node's status (and its PR) when it starts, blocks or merges.
9. **Branch rules.**
   - `0.7.x` and `0.8.x` are release branches; change them only via a PR.
   - Dev branches use an OkHttp-style prefix: `feature/` for features and docs (for example `feature/output-redesign`) and `fix/` for bug fixes (for example `fix/anr-window`). Version-number branches such as `0.7.x` are release branches (above), never dev branches.
   - Existing `feat-*` branches (the old rule) need not be renamed and stay valid until their PRs close; new branches follow the prefix rule.
   - PRs target `main` or the matching release branch.
   - When several PRs are ready, state the merge order and rebase the later ones so they are not BEHIND each other.

## 3. Releases

Release only after **explicit sign-off from the user or maintainer**.

**Pre-release verification** ([#106](https://github.com/s1crab/android-log-mcp/issues/106)). CI alone is not enough: before the release PR is merged, run the checklist below: device checks on the emulator (API 29 and API 35, software emulation only, never KVM, §6), race tests by the `race-gate` workflow and copy the results into the release PR body (the [release PR template](.github/PULL_REQUEST_TEMPLATE/release.md) carries it). **Raw evidence stays out of the repository** (logs, zips, screenshots, run directories, one-off run scripts): pack the local evidence into one archive (`android-log-mcp-X.Y.Z-evidence.tar.gz`, with a `MANIFEST.txt` and a sha256) and upload it as an asset of the GitHub Release after tagging; CI logs stay as Actions artifacts. The repository holds only `validation/X.Y.Z/README.md`, a short summary with the results and the links to the archive, the Actions runs and the release PR, plus small baselines, fixtures and reusable tools. Do not rewrite history to remove evidence committed earlier.

- [ ] CI green on all OSes for the release commit.
- [ ] `scripts/service-e2e.sh` on API 29 and API 35.
- [ ] `scripts/e2e-report.sh` on API 29 and API 35, including `--logs-only`.
- [ ] SIGSTOP the real service process: automatic replacement, `service stop --force`, no data loss (production timings).
- [ ] Upgrade: old-version service process + new MCP process or CLI process (version handoff, socket directory migration).
- [ ] `adb shell date -s` back-jump plus a real app crash and ANR: attribution still correct.
- [ ] `set-root`, then a whole-buffer re-read: no duplicates.
- [ ] Race gate: dispatch the `race-gate` workflow on the release commit (`gh workflow run race-gate.yml --ref <branch> -f count=20 -f pr=<release PR>`): `go test -race -count=20` per package (`store`, `ipc`, `analysis`, `device`, `mcpserver`, `service`, `cmd`) on Linux, Windows and macOS, results from Actions, its summary pasted into the release PR. Not run on the dev box (`service` alone took ~3.3 h there). The `perf` job is green and `scripts/perf.sh --check` passes locally.
- [ ] Windows / macOS: if not tested with a real or emulator-attached adb, mark them **unverified** in the release notes (CHANGELOG).

Testing never blocks merges to `main`; findings are fixed as a patch release.

1. In `CHANGELOG.md`, move the `[Unreleased]` entries under a new `## [X.Y.Z] - YYYY-MM-DD` heading (leave `[Unreleased]` empty) and update the compare links at the bottom. Bump `npm/VERSION` and the default `Version` in `server/internal/mcpserver/server.go`, plus version examples in the READMEs. Commit `chore: release X.Y.Z` on a branch, open a PR, wait for CI to be green and merge it (§2).
2. Tag the merged, green commit on `main` `vX.Y.Z` (annotated) and push the tag. `.github/workflows/release.yml` then:
   - checks that the tag matches `npm/VERSION` and that `CHANGELOG.md` has a non-empty `X.Y.Z` section (`scripts/changelog-section.sh X.Y.Z`), before publishing anything;
   - publishes all npm packages;
   - creates the GitHub Release with the tarballs attached and **the `X.Y.Z` changelog section as its notes** (plus the compare link). Do not hand-edit release notes: fix the changelog on `main` (through a PR) and update the release with `gh release edit vX.Y.Z --notes-file <(scripts/changelog-section.sh X.Y.Z)` (a re-run of the workflow also updates the notes of an existing release, but only after the npm publish steps).
3. Breaking changes need a **Breaking** CHANGELOG entry with the upgrade action (§2); there is no upgrade section in the README.
4. Verify that the npm `latest` tag points at the new version and that `npx -y android-log-mcp@X.Y.Z --version` works.

## 4. Versioning

- **Before 1.0:** breaking changes bump the **minor** version (0.x → 0.(x+1)), and they need a **Breaking** changelog entry that says what to do on upgrade (only there, §2).
- **After 1.0:** breaking changes bump the **major** version.
- Changing a default that deletes or hides user data counts as breaking. One example is retention.

## 5. Language

Repository artifacts (docs, issues, commit messages, release notes) are **English-first**, with a short Chinese section at the end where useful. Some older docs are still Chinese-first; convert them when you substantially edit them.

**Terminology:** issues, PRs, docs, code comments, logs and tool output use the terms in [docs/glossary.md](docs/glossary.md): MCP process, service process (short: service), CLI process, adb server, adb child, device process, MCP host. Never write a bare "process", "server", "daemon", "client" or "服务". Existing code identifiers and output strings that still use older words are listed there; do not add new ones.

## 6. Safety on the dev box

- **Never start KVM-accelerated emulators, and never touch `/dev/kvm`.** Nested KVM crashes the host. Use the device or emulator that is already running.
- Do not change device settings or app state beyond what a test explicitly requires.

## 7. Where things are

- **Layout:** the table below, and [server/README.md](server/README.md) for the Go packages.
- **User documentation:** [README.md](README.md) (install, short tools list, filter syntax, FAQ; at most about 200 lines), [docs/tools.md](docs/tools.md), [docs/reference.md](docs/reference.md), [docs/compatibility.md](docs/compatibility.md).
- **Design:** [docs/design-v3.md](docs/design-v3.md) (current) and [docs/research-v2.md](docs/research-v2.md) (background research).
- **Testing:**
  - `go test ./...`, including the API 29 fixture replay tests;
  - `scripts/e2e-report.sh` for the end-to-end redaction check on a device;
  - `scripts/compat-probe.sh` / `android-log-mcp compat` for the device compatibility probe;
  - `scripts/perf.sh` for the performance guard (CI job `perf`): output bytes, allocations, latency, memory against `perf/baseline.json` (pull requests: `--pr`, a small history against `perf/baseline-pr.json`). A change that intentionally moves a metric re-records both baselines (`scripts/perf.sh --update && scripts/perf.sh --update --pr`) in the same PR and says why ([docs/performance.md](docs/performance.md));
  - `scripts/check-doc-links.py` for relative links and anchors in the Markdown docs (run it after moving or renaming a heading or file);
  - the evidence and procedures in [validation/README.md](validation/README.md).
  - `.github/workflows/emulator.yml` (nightly and on demand, not a required check): `scripts/emulator-ci.sh` runs `compat`, `service-e2e`, `e2e-report`, `scripts/app-e2e.sh` (launches, crashes, ANR, markers) and `scripts/clock-e2e.sh` (backward-clock matrix #48/#52/#54/#65) on KVM emulators (API 29 and 35; 30 on demand) on GitHub-hosted runners. A failing nightly opens one rolling issue that closes itself on the next green run. It needs KVM, so never run it on the dev box (§6).
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

## 8. Design review criteria

Apply these to any design comment or PR that adds, splits, merges or restructures modules (based on [mattpocock/skills codebase-design](https://github.com/mattpocock/skills/tree/main/skills/engineering/codebase-design)):

1. **Deletion test.** Imagine deleting the module. If the complexity just vanishes, it was a pass-through: do not add it, or inline it. If the complexity reappears across N callers, it earns its keep.
2. **One adapter is a hypothetical seam, two is a real one.** Add an interface or injection point only when production and tests (or two real implementations) differ. Do not add one "for later".
3. **The interface is the test surface.** Tests assert observable behaviour through the public interface, so they survive internal refactors. Do not test private helpers or call order.

Design comments may use the vocabulary module, interface, seam, adapter and depth. Code, tool output and docs keep the [docs/glossary.md](docs/glossary.md) terms (device process, service process, device time, host time).

Large restructuring of the `device` package is evaluated with these criteria and goes through a design comment and review (§2), never a drive-by refactor inside an unrelated change.

### Repository layout

| Path | Content |
|---|---|
| `server/` | Go module `github.com/s1crab/android-log-mcp/server`: sources, unit and regression tests, fixtures (`internal/testdata/api29`); see [server/README.md](server/README.md) |
| `npm/` | npm launcher and package templates; `scripts/build-npm.sh [VERSION] [--pack]` cross-compiles and assembles all packages |
| `scripts/` | `build-npm.sh`, `e2e-report.sh` (report redaction on a device), `service-e2e.sh` (service process on a device through the npm launcher), `compat-probe.sh`, `perf.sh` (performance check / baseline), `emulator-ci.sh` / `app-e2e.sh` / `clock-e2e.sh` (KVM nightly, never on the dev box) |
| `perf/` | `baseline.json`: the performance baseline guarded in CI; see [docs/performance.md](docs/performance.md) |
| `validation/` | Findings and procedures from the API 29 emulator, trigger scripts, test app, small fixtures; per-release `validation/X.Y.Z/README.md` is a summary with links, raw run evidence goes to the release archive (§3) |
| `docs/` | Goals (`GOALS.md`), terminology (`glossary.md`), user documentation (`tools.md`, `reference.md`, `compatibility.md`), research and design (`design-v3.md`), performance metrics and baseline (`performance.md`) |
| `CHANGELOG.md` | Release history ([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)) |
| `AGENTS.md` | Workflow, release, versioning and safety rules for agents and contributors |
| `.github/workflows/` | CI (tests, npx compatibility, performance guard) and release (on `v*` tags) |

---

## 中文摘要

- 先读 `docs/GOALS.md`，每个改动都要对照其中的原则和禁止事项。
- 流程：
  1. 先开 issue；每个 issue 要有优先级标签和里程碑：`priority: P0`（静默丢数据、结果错误或崩溃，必须在当前里程碑发布）、`priority: P1`（重要，下个版本）、`priority: P2`（锦上添花，不承诺）。优先级明确时由 agent 设置，不确定就不设；里程碑和版本规划由发布负责人与用户确定，实现的 agent 不设。新开 issue 时，优先级明确就设标签，永远不设里程碑；
  2. 实现并补测试（`go vet`、`go test`、改动的包加 `-race`、Windows 交叉 vet）；
  3. 不直接提交到 main（main 有分支保护，禁止直接 push 和 force-push）：在功能分支上提交，开 PR，PR 正文写 `Closes #N`；
  4. 等 PR 的 CI 全绿（必需检查：三个系统的 `test` 和 `npm-pack`，分支要与 main 同步；npx-compat 等其他 job 也要通过），main 有更新就 rebase 后再推；
  5. 用 rebase 或 squash 合并（`gh pr merge --rebase` / `--squash`，不允许 merge commit），不需要审批。
  6. 排期由实现的 agent 负责：把未完成的工作建成依赖图（边 = 共用的代码区域、唯一的模拟器、设计审批、发版关口），互不依赖的节点在各自的 worktree 里并行；等审查、CI 或模拟器时不要干等，去做下一个不受阻的节点；依赖图在置顶的跟踪 issue #67 里保持最新。发布负责人只定优先级和里程碑、审设计、决定发版；设计审查仍交给发布负责人，但无关的节点照常推进。
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
- 夜间模拟器验证：`.github/workflows/emulator.yml` 在 GitHub 托管 runner 上用 KVM 模拟器（API 29 / 35，30 按需）运行 `scripts/emulator-ci.sh`（含 `app-e2e` 与 `clock-e2e`）；不是必需检查；夜间失败会自动开一个滚动 issue，下次通过时自动关闭。需要 KVM，禁止在开发机上运行。
- 绝不要杀 adb server。
- 设计评审标准（§8）：（1）删除测试：删掉该模块后复杂度直接消失，说明只是透传；若在 N 个调用方重新出现，才值得保留；（2）一个 adapter 只是假想的接缝，两个才是真的：仅当生产与测试（或两个真实实现）不同时才加接口/注入点；（3）接口就是测试面：测试只通过公开接口断言可观察行为。设计评论可以用 module / interface / seam / adapter / depth；代码、工具输出和文档仍用 `docs/glossary.md` 的术语。`device` 包的大规模重构必须先走设计评论和评审，不得顺手重构。
- 文档结构：README 只保留安装、精简工具表、过滤语法和 FAQ（约 200 行以内）；详细说明在 `docs/tools.md`、`docs/reference.md`、`docs/compatibility.md`；仓库结构表在 AGENTS.md §7。升级说明和破坏性变更只写在 CHANGELOG（Breaking 条目），README 不重复。
- 发版前验证清单（§3）：发版前在模拟器（API 29 和 35）上跑清单，结果写进发版 PR；测试不阻塞 main，问题以 patch 版本修复。
