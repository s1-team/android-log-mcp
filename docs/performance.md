# Performance baseline and regression guard

Issue #13. This document explains which numbers we track, why they matter to AI agents and to the people who run the tool, how to measure them, how CI guards them, and how to update the baseline when a change is intended. The baseline is [`perf/baseline.json`](../perf/baseline.json).

## Quick start

```bash
scripts/perf.sh            # measure and compare with perf/baseline.json (exit 1 on a regression)
scripts/perf.sh --run      # measure only and print every metric as a table
scripts/perf.sh --update   # measure and record a new perf/baseline.json
scripts/perf.sh --pr …     # the same with the small pull-request history against perf/baseline-pr.json
cd server && go test -run '^$' -bench . ./internal/perf    # ad-hoc micro-benchmarks
```

A full run takes about 2–3 minutes. Everything runs in temporary directories with the fake adb (`internal/fakeadb`) and an isolated per-user cache dir. It never touches the real data root, control dir, service process, adb server or a device. `go test ./...` is not slowed down: the measurements skip unless `ALM_PERF_OUT` is set, which `scripts/perf.sh` does.

## What is measured, and why

The metric names in the baseline file start with the group names below. Kinds decide the comparison rule (next section).

| Group | Metrics | Kind | Why agents / users care |
|---|---|---|---|
| **Agent cost** `out.*` | Bytes of `initialize`, `tools/list` and the default output of every tool in 14 standard scenarios on the real API 29 fixture (`session-raw.log` + DropBox). Tokens ≈ bytes / 4. | exact | Every byte is context the model pays for, on every call. `tools/list` + `initialize` are paid once per session by every agent, whether it uses the tools or not. Claude Code warns above 10k tokens and spills results above 50,000 chars to a file (design-v3 §0). |
| **Startup** `startup.*` | MCP process start → `initialize` + `tools/list` answered: cold (spawns the background service: binary copy to `<root>/bin`, start, socket) and warm (service running). | time | The agent's MCP client waits for this before any tool exists. Slow starts show up as "server failed to start" in clients with short timeouts. |
| **First call** `first_call.*` | First `logcat_search` against a fake device with an 8 MiB buffer (~53k real lines, ≈ 2 MiB per buffer): probe, logcat start, backfill into the store, then the answer. | time, rate | Since #11 recording starts on use, so the first call pays the backfill. The call waits at most 10 s, then answers with "backfill in progress". |
| **Query latency** `query.*` | Every tool on a synthetic one-day history (80 back-to-back retimed copies of `session-raw.log`: 604k lines, 516k records), warm (median of 5) and, for history-bound queries, cold (history cache dropped): search default / package / tag / level / regex / negation / `age:` / old `from`–`to` window / full scan with no match, summary, crashes list and detail, launches (30 min and 24 h), status. | time | The design target is about 1 s per call (design-v3 §6). Codex's default tool timeout is 60 s. Agents call these tools in loops, so latency multiplies. |
| **End to end** `e2e.*` | `logcat_search {}` through MCP client → socket → service and back. | time | The IPC overhead of the 0.4 service architecture. |
| **Ingest** `ingest.*`, `scan.*` | Lines/s through parse + assemble + process tracking + crash detection + JSONL append, over the whole synthetic history. Raw `Store.Scan` records/s over all history, cold, warm and with a filter. | rate | Ingest bounds how quickly a backlog (first call, reconnect) is caught up. The scan rate bounds how much history a search can cover within its 2.5 s scan budget. |
| **Disk** `disk.*` | Segment bytes per input line and per stored record. | exact | With the default 512 MiB per device and 1-day retention, bytes/line decides how much history a user keeps. |
| **Memory** `mem.*` | Service RSS idle, while recording (after the 8 MiB backfill), and after heavy queries; thin-client RSS; in-process heap and history cache after all scale queries and after the idle release; binary size. | mem | GOALS "Bounded": the service runs all day next to Android Studio and an emulator. The history cache must stay within its 32 MiB limit, which is a hard budget here. |
| **Allocations** `allocs.*` | allocs/op and B/op of `logcat.ParseLine`, the ingest path per line (fresh store, and averaged over the one-day history), filter parsing and evaluation, and each tool call on the fixture. | allocs | These are nearly deterministic, so they catch hot-path regressions that timing noise would hide. Allocation also drives GC CPU and RSS. |
| **Calibration** `calib.cpu_ms` | A fixed stdlib-only CPU workload: SHA-256, sort, JSON, regexp. | info | Used to scale timing baselines between machines. |

`time.*` (ns/op of the micro-benchmarks) and `*.bytes` at scale are recorded as `info`. They give context and are never checked.

## Regression rules

`server/internal/perf/compare.go` (unit-tested in `compare_test.go`). A metric fails when it is worse than the baseline by more than **both** its relative and its absolute tolerance:

| Kind | Fails when | Notes |
|---|---|---|
| exact (output bytes, disk bytes per line) | > +2 % **and** > +16 bytes | Strict. Output size changes are product changes, so re-record the baseline deliberately. |
| allocs (allocs/op) | > +10 % **and** > +2 | |
| allocs (B/op) | > +15 % **and** > +512 B | |
| time (latency) | > 2.5 × (baseline × machine factor) **and** > +5 ms | Machine factor = `calib.cpu_ms` now / in the baseline, so a slower CI runner is not a regression. |
| rate (throughput) | < (baseline / machine factor) / 2.5 | |
| mem (RSS, heap) | > +50 % **and** > +16 MiB | |
| any metric with a `budget` | over the budget (rate: under it), on any machine | Product limits, not relative to the baseline: tools/list warm < 1 s, cold < 3 s; first call < 10 s (the catch-up wait); warm queries < 3.5 s and cold < 4 s (the 2.5 s scan budget plus rendering); ingest ≥ 20k lines/s; history cache ≤ 32 MiB; service RSS idle ≤ 64 MiB, recording ≤ 128 MiB, after queries ≤ 192 MiB. |
| missing metric | always fails | A removed benchmark must be removed from the baseline on purpose. |

Clear improvements are listed as `improved`. A metric not in the baseline is listed as `new` and is not guarded until the baseline is re-recorded. When the check fails, `scripts/perf.sh` measures once more (`--retries 1`) and fails only if the regression reproduces.

## CI

The `perf` job in `.github/workflows/ci.yml` (ubuntu-latest) runs `scripts/perf.sh --check` on pushes to `main` and nightly, and `scripts/perf.sh --check --pr` on pull requests (issue #25), and uploads the result files as the `perf-results` artifact (`merged.json`, `table.md`). Only Linux is guarded, because RSS comes from `/proc`. The deterministic metrics would be the same elsewhere. The test files still compile, and skip, on all three OSes.

`--pr` builds a smaller synthetic history (`ALM_PERF_COPIES=20`, about 151k lines instead of about 604k) so the job takes about a minute. Query latency and the history-averaged ingest metrics depend on the history size, so it has its own baseline, [`perf/baseline-pr.json`](../perf/baseline-pr.json). The fixture-based metrics (allocations, output bytes per call) do not depend on the size and are guarded the same way in both.

## Updating the baseline

Re-record the baseline when a change intentionally moves a metric: a richer output, a new metric, a deliberate trade-off, or an optimisation you want to lock in.

1. Run `scripts/perf.sh --update && scripts/perf.sh --update --pr` on a quiet machine: always re-record both baselines together. For timing-heavy changes, run it twice and keep the second run. Or download `merged.json` from the CI `perf-results` artifact and copy it over `perf/baseline.json` (a pull request's artifact is the small size: copy it over `perf/baseline-pr.json`).
2. Review `git diff perf/`. Every changed exact / allocs value should be explained by the change.
3. Commit it in the same PR, with the reason in the commit message or PR body. Update the numbers below if a core metric moved.

## Current numbers

Recorded 2026-10-11 on the dev box: linux/amd64, Intel Xeon, 8 vCPUs shared with a running TCG Android emulator, go1.24.4, commit 733eb0b (0.5.0). Timings on GitHub runners differ; the machine factor accounts for that. Full list: `perf/baseline.json`, or `scripts/perf.sh --run`.

**Agent cost** (fixture, deterministic)

| Scenario | Bytes | ≈ tokens |
|---|---:|---:|
| `initialize` (instructions) | 1,068 | 267 |
| `tools/list` (4 tools) | 8,729 | 2,182 |
| `logcat_status {}` | 567 | 142 |
| `logcat_status {package}` | 1,155 | 289 |
| `logcat_search {}` (newest 200) | 27,471 | 6,868 |
| `logcat_search {package}` | 28,011 | 7,003 |
| `logcat_search {min_level: error}` | 18,737 | 4,684 |
| `logcat_search {filter: tag=:ActivityManager, limit: 20}` | 5,307 | 1,327 |
| `logcat_search {view: summary}` | 2,119 | 530 |
| `logcat_search` no match (hints) | 409 | 102 |
| `logcat_crashes {}` (7 groups) | 2,923 | 731 |
| `logcat_crashes {id}` (Java crash detail) | 6,326 | 1,582 |
| `logcat_app_launches {package}` (5 launches) | 4,277 | 1,069 |

**Startup, first call, memory**

| Metric | Value |
|---|---:|
| MCP start → tools/list, service running | 5.6 ms |
| MCP start → tools/list, spawning the service | 68 ms (spawn ≈ 63 ms) |
| First call, backfill of 8 MiB / 52,808 lines | 1.27 s (≈ 42k lines/s) |
| `logcat_search {}` end to end through the service | 6.0 ms |
| Service RSS: idle / recording / after queries | 10.6 / 57.0 / 58.1 MiB |
| MCP client RSS | 10.9 MiB |
| Heap after scale queries / after idle release; history cache | 26.9 / 2.4 MiB; 31 MiB (limit 32) |
| Binary | 10.5 MiB |

**Scale** (604k lines / 516k records ≈ one day of history; warm median)

| Query | ms |
|---|---:|
| `logcat_status {}` / `{package}` | 0.1 / 33 |
| `logcat_search {}`, `{package}`, `tag=:`, `min_level`, regex, negation | ≈ 1,515 each |
| `logcat_search {filter: "age:5m level:warn"}` | 1,661 |
| `logcat_search` 5-minute window at the oldest end (cold / warm) | 1,819 / 1,714 |
| `logcat_search` full scan, no match (cold / warm) | 2,879 / 2,836 (completes, at the edge of the 2.5 s scan budget) |
| `logcat_search {view: summary, from: 1h}` | 69 |
| `logcat_crashes {}` / detail of the oldest crash (cold) | 17 / 105 |
| `logcat_app_launches {package}` 30 min / 24 h | 0.8 / 1,797 |

| Throughput / disk | Value |
|---|---:|
| Ingest over the one-day history | 47k lines/s |
| Ingest per line, fresh store (micro-benchmark) | 3.0 µs, 6.2 allocs, 869 B |
| Ingest per line, averaged over the one-day history | 29 allocs, 8,113 B |
| `Store.Scan` cold / warm / cold + filter | 211k–261k / 285k / 266k records/s (noisy box) |
| Disk | 222 bytes per input line (raw logcat text: 168), 260 per record |

## Findings worth optimising

1. **Searches cost about 1.5 s once the history is large.** On a one-day history, every search without `from`/`to` takes about 1.5 s, even when the 200 newest matches are found within a few ms (3 ms on a 75k-line history). After collecting the matches, the scan keeps going to count all matches ("matched N lines") until the 1.5 s count budget runs out, and then reports a lower bound (`matched ≥430,079 lines`). A cheaper count (an estimate, or stopping at e.g. 10× limit) would make the common calls several hundred times faster.
2. **`age:` and old windows do not bound the scan.** `age:5m` is evaluated per record, but it does not stop the newest-first scan the way `from` does (1.7 s). A 5-minute window at the old end scans the whole newer history first (1.8 s). A time → seq lookup over the block index would make both proportional to the window.
3. **Full-history scans are at the edge of the 2.5 s scan budget.** JSON decoding limits the scan to about 210k–260k records/s, so one call can cover at most about 600k records. A no-match search over the synthetic day still completes, in about 2.8–3.0 s: about 2.1 s of scanning, and the zero-result hints take the rest. On a busier day, or a slower machine, the same search returns "scan stopped … incomplete".
4. **Ingest slows down as crashes accumulate.** `Pipeline.EmitCrash` calls `LoadCrashes`, which re-reads and JSON-decodes the whole `crashes.jsonl`, for every crash. That is O(crashes²). On the synthetic day (several hundred crashes and ANRs) it takes 80 % of the ingest CPU: 47k lines/s and 8.1 KB allocated per line, against about 330k lines/s (3.0 µs per line) and 0.9 KB per line on a fresh store. An in-memory crash index would fix it. `logcat_crashes` itself allocates 1.5 MB per call on the fixture for the same reason.
5. **The default `logcat_search` output is about 6.9k tokens.** The 30,000-char budget is nearly used up by the newest 200 lines. That is within limits, but agents calling `logcat_search {}` repeatedly pay about 7k tokens each time. A smaller default limit, or summary-first guidance, is worth an eval (design-v3 §7 Q5).
6. **The summary view allocates about 3.9 MB / 41k allocations per call on 6.4k records** (template normalisation per record). It is fine at this size, but it scales linearly with the range.

---

## 中文摘要

- **目的**：给性能建立基准，防止改动悄悄变慢、变占内存，或者让输出变大（agent 的 token 成本）；也为以后的优化提供参照。基线文件是 `perf/baseline.json`。
- **用法**：
  - `scripts/perf.sh`：测量并和基线比较，有回归时退出码为 1；
  - `--run`：只测量，打印表格；
  - `--update`：重新记录基线。
  测量全部在临时目录里进行，用假 adb，不碰真实服务、数据和设备。`go test ./...` 不受影响：没有 `ALM_PERF_OUT` 时这些测量会跳过。
- **指标**：
  - agent 成本：各工具默认输出和 `tools/list` 的字节数，token ≈ 字节/4，结果确定；
  - 启动时间：冷启动 / 热启动到 `tools/list` 返回；
  - 首次调用：回填 8 MiB buffer 的耗时；
  - 一天量级历史（60 万行）上各工具的查询耗时；
  - 写入吞吐、每行占用的磁盘字节；
  - 服务 RSS 和历史缓存上限；
  - 热路径的 allocs/op。
- **规则**：
  - 确定性指标严格比较：输出字节 +2%、allocs +10%、B/op +15%，各带很小的绝对容差；
  - 耗时先按 CPU 校准换算机器快慢，再按 2.5 倍宽松比较；
  - 内存 +50% 且 +16 MiB 才算回归；
  - 另有与机器无关的绝对预算（如查询 < 3.5 s、历史缓存 ≤ 32 MiB）；
  - 失败时会自动重测一次，以排除噪声。
- **CI**：`perf` job（ubuntu）在 main 推送和每晚运行 `scripts/perf.sh --check`（完整规模），在 PR 上运行 `scripts/perf.sh --check --pr`（20 份数据，对比 `perf/baseline-pr.json`），结果作为 artifact 上传。
- **更新基线**：有意改变指标时运行 `scripts/perf.sh --update && scripts/perf.sh --update --pr`（两个基线一起更新）（或用 CI artifact 里的 `merged.json`），在同一个 PR 里提交并说明原因。
- **发现的问题**：
  1. 历史一大，不带时间范围的搜索都要约 1.5 s：为了统计匹配总数，会一直扫到计数预算用完；
  2. `age:` 和旧时间窗不会让扫描提前结束；
  3. 全量扫描约每秒 25 万条，扫完一天的历史已接近 2.5 s 的扫描预算；
  4. 每次发现崩溃都会重读整个 `crashes.jsonl`，复杂度 O(n²)，崩溃一多，写入速度降到约七分之一；
  5. 默认搜索输出约 6.9k token。
