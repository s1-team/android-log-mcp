# Project goals

## Goal

Give any AI coding agent reliable, low-friction access to what an Android app is doing at runtime (logs, crashes, ANRs, lifecycle and timing), in a form the agent can query precisely and read correctly.

## Principles

1. **Zero-setup.** One command works on a developer's machine, with no extra runtimes or services to install.
2. **Observe, never interfere.** The tool must not change the device, the app, or the developer's existing tools.
3. **Bounded.** Every resource it uses (memory, disk, files, processes, connections) has a limit and a lifetime.
4. **Truthful.** It never silently guesses, drops data, or changes behavior. Gaps and errors are explicit and actionable.
5. **Private by default.** Nothing leaves the machine unless the user exports it, and exports are redacted.
6. **Agent-first.** The surface is small and stable; outputs are compact, structured, and unambiguous.
7. **Predictable evolution.** Breaking changes are deliberate, versioned, and documented with an upgrade path.

## Must not

- Modify device or app state, or require root or app-private access.
- Disrupt adb, Android Studio, or other tools sharing the device.
- Grow without bound, or leave unmanaged files or processes behind.
- Make network connections at runtime.
- Write anything except protocol messages to the protocol channel.

## Where the details live

This document is intentionally product-level. The concrete constraints that implement these principles live in `docs/design-*.md` and are enforced by CI where possible. Examples are resource limits, the single-recorder rule, and stdout discipline. When a design decision conflicts with a principle here, the principle wins, or this document is changed deliberately and explicitly.

---

## 中文摘要

**目标**：让任何 AI 编程 agent 都能可靠、低成本地知道 Android App 在运行时做了什么，包括日志、崩溃、ANR、生命周期和耗时。数据的形式要让 agent 能精确查询、正确理解。

**原则**
1. 零配置：一条命令就能用，不用另装运行时或服务。
2. 只观察、不干扰：不改变设备、App 和开发者现有的工具。
3. 有界：内存、磁盘、文件、进程、连接都有上限和生命周期。
4. 如实：不悄悄猜测、丢数据或改变行为；缺口和错误要明确，并且给出能照着做的处理办法。
5. 默认私密：用户不主动导出，数据就不离开本机；导出的内容要打码。
6. 面向 agent：接口小而稳定，输出紧凑、结构化、无歧义。
7. 可预期的演进：破坏性变更要有意为之、带版本号，并附升级说明。

**禁止**：
- 修改设备或 App 状态，或者需要 root 或 App 私有权限；
- 干扰 adb、Android Studio 或其他共用设备的工具；
- 无限增长，或者留下没人管理的文件、进程；
- 运行时发起网络连接；
- 往协议通道写协议消息以外的任何内容。

具体约束写在 `docs/design-*.md` 里，能用 CI 检查的都由 CI 检查。
