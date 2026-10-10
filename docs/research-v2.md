# Android 日志 MCP stdio server：调研 v2（只调研，不写代码）

- 调研日期：2026-10-10（Asia/Shanghai）。AS 源码取自 AOSP `platform/tools/adt/idea` 的 **studio-main** 分支，抓取当天的快照（没拿到单个文件的 commit 日期）。
- 标注约定：**[源码]** 表示读过源码确认；**[测试]** 表示有 AS 单元测试印证；**[文档]** 出自官方文档；**[推断]** 从语法或代码推出，但没有测试或实机验证；**[未知]** 表示没查到。
- 常用链接前缀：
  - `AS` = https://android.googlesource.com/platform/tools/adt/idea/+/refs/heads/studio-main/logcat/src/com/android/tools/idea/logcat/
  - `AST` = https://android.googlesource.com/platform/tools/adt/idea/+/refs/heads/studio-main/logcat/testSrc/com/android/tools/idea/logcat/filters/LogcatFilterParserTest.kt
  - `PM` = https://android.googlesource.com/platform/tools/base/+/refs/heads/studio-main/process-monitor/
  - `LOGGING` = https://android.googlesource.com/platform/system/logging/+/refs/heads/main/
  - 官方文档：https://developer.android.com/studio/debug/logcat 和 https://developer.android.com/tools/logcat

---

## 1. Android Studio Logcat 过滤语言：完整规格

源文件：
- 词法：`AS/filters/parser/LogcatFilter.flex`
- 语法：`AS/filters/parser/LogcatFilter.bnf`
- 构建与隐式组合：`AS/filters/LogcatFilterParser.kt`
- 匹配语义：`AS/filters/LogcatFilter.kt`
- 级别：`AS/message/LogLevel.kt`
- 测试：`AST`

### 1.1 文法（与 AS 等价的 EBNF）

```
root            ::= expression*                 // 顶层按空白并列，见 1.4 的隐式组合
expression      ::= orExpr
orExpr          ::= andExpr ( "|" andExpr )*    // & 比 | 绑得紧 [测试 parse_operatorPrecedence]
andExpr         ::= primary ( "&" primary )*
primary         ::= literal | "(" expression? ")"   // 空括号 () 得到 EmptyFilter [源码]
literal         ::= TEXT_KEY_TOKEN value         // [-]tag: [-]tag=: [-]tag~: …
                  | REGULAR_KEY_TOKEN \S+        // age: level: is: name:
                  | value                        // 自由文本
TEXT_KEY_TOKEN  ::= "-"? ("line"|"message"|"package"|"process"|"tag") (":" | "=:" | "~:")
REGULAR_KEY     ::= ("age"|"level"|"is"|"name") ":"          // 不能加 "-"、"=" 或 "~"
value           ::= '\'' ( [^'\\] | "\\'" | … )* '\''
                  | '"'  ( [^"\\] | "\\\"" | … )* '"'
                  | [^'"\s()] ( [^\s():] | "\\ " )*          // UNQUOTED_VALUE
```

- BNF 原文是 `root ::= expression*`，`expression ::= orExpression | andExpression | literalExpression | parenExpression`。`OR` 和 `AND` 的优先级在 Grammar-Kit 里声明。
- 括号里只能是一个 expression。所以 `(a b)` 这种括号内空格并列会解析失败，然后按 1.5 回退成纯文本。**[推断]**

### 1.2 键（key）

| 键 | 匹配字段 | 支持的写法 | 默认语义 | 来源 |
|---|---|---|---|---|
| `tag` | header.tag | `:` `=:` `~:`，可加 `-` 前缀 | contains，忽略大小写 | [源码][测试] |
| `message` | 消息正文（可能多行） | 同上 | 同上 | [源码] |
| `package` | header.applicationId | 同上 | 同上 | [源码] |
| `process` | header.processName | 同上 | 同上 | [源码] |
| `line` | 整行规范串 `"<yyyy-MM-dd HH:mm:ss.SSS> <pid>-<tid> <tag> <applicationId> <级别字母>: <message>"`，时间用本地时区 | 同上 | 同上 | [源码 LogcatMessageWrapper.kt] |
| 自由文本（不带 key） | 同 `line` | 不能加 `-`；引号可选 | contains，忽略大小写 | [源码][测试] |
| `level` | 级别 ≥ 给定值 | 只有 `:` | 只接受 verbose/debug/info/warn/error/assert，大小写不限；**不接受** `V`/`E` 单字母，**不接受** `fatal` | [源码 LogLevel.getByString] |
| `age` | 主机当前时钟 − 条目时间 ≤ N | 只有 `:` | 正则 `\d+[smhd]`，例如 `age:30s`、`5m`、`2h`、`1d` | [源码][文档] |
| `is` | `crash` / `stacktrace` / `firebase` | 只有 `:` | crash：(E 级 且 tag=AndroidRuntime 且 message 以 "FATAL EXCEPTION" 开头) 或 (A 级 且 tag 为 DEBUG 或 libc)。stacktrace：message 匹配 `\n\s*at .+\(.+\)\n`。firebase 没写进文档，是一组固定 tag 的正则。整个 `is:` 受 StudioFlags.LOGCAT_IS_FILTER 开关控制 | [源码] |
| `name` | 不参与匹配（恒为真） | 只有 `:` | 给过滤器起名，取最后一个出现的值 | [源码][文档] |
| `package:mine` | 当前工程的包集合 | — | 只有 Android 工程里才生效。匹配条件：appId（没有则用 processName）在包集合中，**或者** 级别 ≥ ERROR 且 message 含任一工程包名。非 Android 工程里它就是普通的 `package:` contains "mine" | [源码] |

### 1.3 运算符与修饰

| 写法 | 语义 | 大小写 | 来源 |
|---|---|---|---|
| `key:v` | contains | 忽略 | [源码] |
| `-key:v` | 不 contains | 忽略 | [源码] |
| `key=:v` | 完全相等 | **区分** | [源码] |
| `-key=:v` | 不相等 | 区分 | [源码 flex 允许 `-`+`=:`] |
| `key~:re` | 正则部分匹配（Kotlin `Regex.containsMatchIn`，即 java.util.regex） | **区分**（除非写 `(?i)`） | [源码] |
| `-key~:re` | 正则不匹配 | 区分 | [源码] |
| `a & b` | AND | — | [源码][测试] |
| `a \| b` | OR，优先级低于 `&` | — | [测试] |
| `( … )` | 分组 | — | [源码] |
| 没有 `=~` 或 `~=` 组合写法 | — | — | [源码] |
| 键名只能小写（flex 没开 `%caseless`） | `Tag:` 会被当成自由文本 | — | [源码][推断] |

**引号和转义**（PsiExtensions.kt 中的 toText）[源码]
- 单引号或双引号包裹的值：去掉外层引号，并把 `\'` 或 `\"` 还原。
- 不加引号的值：做 `\\([ :'"\\])` → `$1` 的反转义，所以可以写 `tag:foo\ bar`、`message:a\:b`。
- 不加引号时，值的首字符不能是引号、空白或括号；从第二个字符起不能出现空白、`:` 或括号。
- 冒号后允许有空格，`tag: foo` 合法。

**词法上的坑** [推断：根据 flex 最长匹配规则推出，没有专门测试]
- `tag:foo|bar` 会被当成一个值 `foo|bar`。所以 `|` 和 `&` 两边必须留空格，或者把值加引号。
- 值里要放 `:`、空格或括号时，必须加引号或转义。

### 1.4 隐式组合（顶层空白并列时）

生产环境的构造方式在 LogcatMainPanel 里：`LogcatFilterParser(project, packageNamesProvider, androidProjectDetector)`。参数取默认值，`joinConsecutiveTopLevelValue=false`，`topLevelSameKeyTreatment=OR`。[源码]

顶层各项先按 `getFieldForImplicitOr` 分组。**组内做 OR，组与组之间做 AND。**

| 分组规则 | 例子 | 结果 | 来源 |
|---|---|---|---|
| 同一字段的正向 `:`、`=:`、`~:` 归为一组（三种写法可以混用） | `tag:a tag~:b tag=:c` | 三者 OR | [源码][测试] |
| 所有 `level:` 归为一组 | `level:warn level:error` | OR（实际效果等于取较低的那个） | [源码] |
| 所有 `age:` 归为一组 | — | OR | [源码] |
| `is:crash` 与 `is:stacktrace` 归为一组 | `is:crash is:stacktrace` | OR | [源码] |
| 否定项、自由文本、`package:mine`、`name:` 各自单独成组 | `-tag:a -tag:b` 或 `foo bar` | AND | [源码] |
| 不同字段之间 | `tag:a package:x level:info` | AND | [源码][测试] |

- 顶层空白并列的优先级最低，比 `|` 还低。例如 `tag:a level:error | tag:b` 的含义是 `tag:a AND (level:error | tag:b)`。**[推断]**：依据是 root 由 expression* 并列构成。
- **文档与代码不一致**：官方文档写 `foo bar` 等同于 `'foo bar'`（拼成一个短语）。但生产代码里 `join=false`，实际得到 `line contains foo AND line contains bar`。只有在 join=true 的测试里才是拼接。我们的实现建议跟随代码（取 AND），并在文档中注明这一差异。

### 1.5 出错时的行为 [源码][测试]

下列任一情况都会让整条查询回退为一个过滤器：`line` 忽略大小写 contains 原始查询串。
- 解析失败：括号不配对、引号没闭合
- 非法 level
- 非法 age
- 未知的 `is:` 值
- 非法正则

### 1.6 其他行为

- LogcatMasterFilter 会额外丢掉被忽略的 tag，以及 `studio.` 开头的 tag。[源码] 我们不需要照做。
- 正则兼容性：Go RE2 不支持后向引用和 lookaround（`(?=`、`(?<=`、`\1`）。遇到时按 AS 的思路回退为纯文本，并在返回结果中给出 warning；也可以引入 `github.com/dlclark/regexp2`（.NET 风格引擎）以求完整兼容，代价是失去线性时间保证。
- 我们自己加的扩展必须明确标为“非 AS”，并且不能和 AS 语义冲突。建议的扩展：
  - `pid:` `tid:` `uid:`（AS 没有这些键。注意按 AS 的词法，`pid:123` 根本解析不出来，所以加上它不会和 AS 冲突）
  - `buffer:events|crash|…`
  - `package:mine` 读取 MCP 配置里的包名列表
- 是否接受单字母 level（`level:e`）：AS 遇到它会回退成纯文本，我们若接受就会和 AS 行为不同。建议默认严格按 AS，单字母作为可选的宽松模式。

---

## 2. pid → 包名/进程名

### 2.1 AS 的做法 [源码]

**读日志的方式**
- 文本模式：`logcat -v long -v epoch`，加 `-t`/`-T` 取历史。没有传 `-b`，所以用默认 buffer `main,system,crash(,kernel)`，**不读 events**。
  - 来源：`AS/service/LogcatServiceImpl.kt`、`LOGGING/logcat/logcat.cpp`
- API ≥ 36 且 StudioFlags.LOGCAT_PROTOBUF_ENABLED 打开时，改用 `logcat --proto`。
  - 每条记录前有 uint64 LE 长度前缀，字段为 time_sec/nsec、priority、uid、pid、tid、tag、message、process_name。
  - process_name 由设备端 ProcessNames 在**打印时**解析：读 `/proc/PID/cmdline` 的 basename，读不到再用 comm，并有 100 条的 LRU 缓存。
  - 因此历史中已经死掉的 pid 名字会为空，甚至错误（pid 被复用时）。
  - 来源：`LOGGING/logcat/logcat.proto`、`LOGGING/logcat/process_names.cpp`

**header 解析**（`AS/message/LogcatHeaderParser.kt`）
- 调用 ProcessNameMonitor.getProcessNames(serial, pid)。
- pid 0 记为 "kernel"；查不到名字时记为 "pid-N"。
- applicationId 取 names.applicationId，没有则用 processName。

**ProcessNameMonitor**（`PM/src/main/com/android/processmonitor/…`）
- 每台设备一个 PerDeviceMonitor，合并两类 tracker：
  1. **JdwpProcessTracker**：基于 adblib 的 `track-jdwp`，设备支持时用 `track-app`。只覆盖可调试或可 profile 的进程，名字从 JDWP 的属性里取。
  2. **AgentProcessTracker**（API ≥ 21，且在 sdk < 36 或 proto 关闭时启用）：
     - 推送 `process-tracker` 原生二进制到 `/data/local/tmp/.studio/`，每隔 interval 扫一次 `/proc`，输出 `+ pid name` 或 `- pid`。
     - 名字取 cmdline 的 basename，读不到再用 comm；跳过 zygote*、`<pre-initialized>` 和空名字。
     - 来源：`PM/process-tracker-agent/process-tracker.cc`
- 用 RetainingMap 保留已经消失的 pid。保留上限 = StudioFlags.PROCESS_NAME_MONITOR_MAX_RETENTION，**数值[未知]**。agent 的 interval 数值同样**[未知]**。
- package 未知时，取 processName 第一个 `:` 之前的部分，所以 `com.foo:remote` 对应 `com.foo`。

### 2.2 同类工具的做法（参考）

- **JakeWharton/pidcat**（4955★，最后 push 2024-05-10）：
  - 启动时执行 `adb shell ps` 得到种子映射。
  - 之后用正则跟踪生命周期：
    - 进程启动：`Start proc <pid>:<proc>/<uid> for …`（5.1+）以及更老的格式
    - 进程结束：`Killing <pid>:<proc>/…`、`No longer want <proc> (pid N)`、`Process <proc> (pid N) has died`
  - 源码：https://github.com/JakeWharton/pidcat/blob/master/pidcat.py
- **thiagodmont/keynobi**：
  - 种子：`adb shell ps -A -o PID,NAME`
  - 增量："Start proc" 行，以及 "has died" 或 "Killing" 行
  - 包名：取进程名 `:` 之前的部分
  - 源码：`src-tauri/src/services/logcat.rs`、`log_pipeline.rs`，https://github.com/thiagodmont/keynobi

### 2.3 我们的推荐方案（鲁棒、不需要往设备推送二进制）

1. **每条记录在入库时就解析出名字并写进记录**，不在查询时现查。这样进程死后、pid 被复用后，查询结果依然正确。
2. **键用 (boot_id, pid, start_ts)**：
   - boot_id 取 `/proc/sys/kernel/random/boot_id`；设备重启后它会变，pid 表随之整体换代。
   - 同一个 pid 被再次 Start proc 时，关闭旧条目（写入 end_ts），再开一条新的。
3. **数据来源按可靠性从高到低**：
   - a. **events buffer 的 `am_proc_start`**（tag 30014）：给出 User、PID、UID、进程名、类型、组件，结构化最好。[源码 frameworks/base am/EventLogTags.logtags]
   - b. system buffer 中 ActivityManager 的 `Start proc <pid>:<proc>/<uid> for <type> <component>` 行，作为 a 的兜底。
   - c. 进程结束的信号：`am_proc_died`(30011)、`am_kill`(30023)，以及 "Process X (pid N) has died"、"Killing N:X/…" 行。收到后写 end_ts，但条目继续保留。
   - d. **启动时的种子**：`adb shell ps -A -o PID,UID,NAME`（toybox ps）。用来覆盖 MCP 启动前就已存在的进程。
     - 旧设备上 `-A` 和 `-o` 是否可用是 **[未知/需实机]**。兜底是解析不带参数的 `ps` 输出（pidcat 就是这么做的），再兜底是 `ls /proc` 加 `cat /proc/N/cmdline`。
   - e. **周期性补漏**：每 5–10 秒跑一次 `ps -A -o PID,UID,NAME`，或者只在遇到未知 pid 时按需触发一次（加去抖）。
     - 补漏的必要性：用户版设备上普通 App 进程一定会有 Start proc 或 am_proc_start；但 native 守护进程、shell 拉起的进程没有。
   - f. API ≥ 36 时可以直接用 `--proto` 自带的 uid 和 process_name 作为补充，但不能把它当唯一来源（理由见 2.1）。
4. **包名的取法**：
   - 优先用 uid → package：`adb shell cmd package list packages -U`，输出形如 `package:com.foo uid:10123`。该命令格式**[需实机确认]**。共享 uid 时会对应多个包。
   - 退而求其次，用进程名中 `:` 之前的部分（与 AS 一致）。
   - 多进程 App：`android:process` 写成 `:remote` 时，进程名为 `com.foo:remote`，两种方法都能得到 `com.foo`。写成完全自定义的名字时，只能靠 uid 映射。
   - 文本格式下 `-v uid` 输出的 uid 规则（`LOGGING/liblog/logprint.cpp`）[源码]：用户名不超过 5 个字符时输出 `root:`、`shell:` 这样的名字，否则输出数字加冒号。App uid（≥10000）一定是数字形式。权限说明 "If permitted by access controls"，即 adb shell 能否拿到 uid **[需实机]**。
5. **dead 条目的保留**：保留时长与日志存储的保留期一致（见 4.3），pid 表与日志同步做 GC。

---

## 3. 场景、buffer 与输出格式

### 3.1 buffer 和 `-v` 的选择

**命令**：`adb -s <serial> logcat -b main,system,crash,events -v threadtime -v epoch -v uid -v usec -v printable [-T <last_epoch>]`

- **必须加 events**。AS 不读 events，但冷启动用到的 `am_proc_start`、`wm_on_*_called`、`wm_activity_launch_time`，以及 ANR/crash 相关的 `am_anr`、`am_crash` 都只在 events 里。
- radio 默认关闭。kernel 只有 userdebug/eng 构建上才能读，默认也关闭。
- `-v epoch`：时间输出为 `秒.微秒`（配合 usec），没有时区和年份歧义，`-T` 续传时也直接用这个格式。logcat 帮助中列出的 TIME 格式包含 `'sssss.mmm'`。[源码 logcat.cpp 帮助]
- `-v uid` 按 2.3 第 4 点解析。`-v printable` 用于转义不可打印字节。
- 不选 `-v long`：AS 用它是为了多行消息。但 logcat 输出时会把多行消息拆成多条同头记录，threadtime 也一样，入库时按 (ts, pid, tid, tag, level) 合并相邻的同头行即可。**[推断]**
- 可选：API ≥ 36 时改用 `--proto`。省掉文本解析，并带上 uid 和 process_name。建议放到 v2。

### 3.2 场景 (a)：冷启动，从启动到首个 Activity 的 onResume

**相关信号**（frameworks/base main 源码）

| 信号 | buffer | 进程 | 含义 |
|---|---|---|---|
| ActivityTaskManager `START u0 {…cmp=com.foo/.Main} … from uid N` | system | system_server | 启动请求，T0 |
| `am_proc_start [0,<pid>,<uid>,com.foo,activity,…]` 或 ActivityManager `Start proc <pid>:com.foo/u0aNN for …` | events 或 system | system_server | fork 完成；从这里拿到新 pid |
| `wm_on_create_called [token,com.foo.Main,performCreate,<ms>]` | events | **App pid** | onCreate 返回后写入；最后一列是回调耗时 ms |
| `wm_on_start_called` / `wm_on_resume_called`(30022) | events | App pid | **onResume 返回的时刻**，即本场景的终点 |
| `Displayed com.foo/.Main for user 0: +812ms` | main | system_server | TTID（首帧）。新版 tag 为 ActivityTaskManager；老版本是 ActivityManager，并且没有 " for user" |
| `wm_activity_launch_time` | events | system_server | Displayed 的 events 版本 |
| `Fully drawn com.foo/.Main: +1s20ms` | main | system_server | 只有 App 调用 reportFullyDrawn() 才会有 |

- 冷启动到 onResume 的耗时 = `ts(wm_on_resume_called, pid=新pid)` − `ts(START u0 … com.foo)`，也可以用 `am_proc_start` 作为起点。各 buffer 的时间戳来自同一个 logd 时钟，可以直接相减。**[推断]**
- 另一条路：执行 `adb shell am start -W -n com.foo/.Main`，直接读出 TotalTime 和 WaitTime。[文档 https://developer.android.com/topic/performance/vitals/launch-time]

**查询示例**（AS 语法）：
```
tag~:"^Activity(Task)?Manager$" message~:"^(START u0|Displayed|Fully drawn|Start proc)" message:com.foo
```
```
tag~:"^(am_proc_start|wm_on_(create|start|resume)_called|wm_activity_launch_time)$" message:com.foo
```
- 第一条：tag 组与两个 message 条件做 AND；两个 message 条件同属一个字段，按 1.4 会被合并成 OR。**这一点要特别注意**，所以更稳妥的写法是：
```
tag~:"^Activity(Task)?Manager$" & message~:"^(START u0|Displayed|Fully drawn|Start proc).*com\.foo"
```
- 想看 App 进程自己打的日志，再加一条：`package:com.foo level:debug`。

### 3.3 场景 (b)：崩溃分析

| 类型 | 关键行 | buffer | 备注 |
|---|---|---|---|
| Java 崩溃 | `E AndroidRuntime: FATAL EXCEPTION: main`，接着是 `Process: com.foo, PID: 123` 和堆栈 | crash（RuntimeInit 通过 Clog_e 写 LOG_ID_CRASH） | 另有 events `am_crash`(30039)，以及反复崩溃时的 `am_process_crashed_too_much` |
| Native 崩溃 | `F libc: Fatal signal 11 (SIGSEGV) … pid 123 (com.foo)` | crash | 由**崩溃进程本身**打出 |
| tombstone | `F DEBUG: *** *** ***`、`pid: 123, tid: …, name: … >>> com.foo <<<`、backtrace | crash | 由 crash_dump 进程打出，**pid 不是 App 的 pid**，所以 `package:com.foo` 过滤不到。这也是 AS 的 `is:crash` 要按 tag 判断的原因 |
| ANR | `E ActivityManager: ANR in com.foo (…)` 以及原因和 CPU 信息；events 中有 `am_anr [0,123,com.foo,…,reason]`(30008) | system 或 events | 完整 traces 在 `/data/anr/` 下，adb shell 通常读不到；可以借助 `bugreport` 或 `dumpsys activity anr`，权限**[需实机]** |

**查询示例**：
- 全部崩溃：`is:crash`
- 某个 App 的崩溃加 ANR：
  ```
  is:crash | (tag=:ActivityManager & message:"ANR in com.foo") | tag~:"^am_(anr|crash)$"
  ```
- 某个 App 的 E 级以上日志（类似 AS 的 package:mine 逻辑）：
  ```
  package:com.foo level:error | message:com.foo & level:error
  ```
- 建议另做一个 `crashes` 工具，把 FATAL EXCEPTION 块和 tombstone 块（以 `*** *** ***` 开头的整段，pid 来自 `>>> pkg <<<` 那一行）**归并成结构化记录**，并把 tombstone 归到真正崩溃的 pid 上。

### 3.4 场景 (c)：同时过滤多个 tag

- `tag=:OkHttp tag=:MyRepo tag=:Billing package:com.foo level:debug`：三个 tag 做 OR，再与 package 和 level 做 AND。[测试：同 key 隐式 OR]
- 正则写法：`tag~:"^(OkHttp|MyRepo|Billing)$" package:com.foo`
- 排除噪音：`package:com.foo -tag:Choreographer -tag:OpenGLRenderer level:info`。多个否定之间是 AND。
- 限定时间：在任一查询后加 `age:10m`。
- 显式写法：`(tag=:OkHttp | tag=:MyRepo) & package:com.foo`。`|` 两边**必须有空格**。

---

## 4. 运行架构

### 4.1 设备发现

- **推荐 `adb track-devices`**，对应 adb server 协议中的 `host:track-devices`：连接保持不断开，每次设备变化推送一次完整列表，格式是 4 位十六进制长度加上 `serial\tstate` 行。[源码 packages/modules/adb docs/dev/services.md]
  - 实现可以二选一：直接连 `localhost:5037` 讲 adb 协议，或者执行 `adb track-devices` 子进程。
  - 断线后（例如 adb server 重启）退避重连；在此期间每 2 秒用 `adb devices` 轮询兜底。
- 只有 `device` 状态的设备才开始记录；`unauthorized`、`offline` 只做上报。
- 默认自动记录：只有 1 台设备时就记录它；多台设备时，记录环境变量 `ANDROID_SERIAL` 或配置中指定的那台，否则谁都不记录，并在 `devices` 工具里提示。多设备并行记录放到 v2（架构上已按 serial 隔离）。

### 4.2 断开与重连

- `adb logcat` 进程退出，或者 track-devices 报告设备消失，都标记为一个 gap 事件。
- 设备回到 `device` 状态后：
  - 先读 boot_id。没变就用 `-T <last_epoch>` 续传，并在交界处按 (ts, pid, tid, tag, msg hash) 去重。
  - boot_id 变了说明设备重启过：开启一个新 session，pid 表整体换代。重启前的 RAM 日志已经丢失；`logcat -L` 可读 pstore，但通常只有内核日志，**[需实机]**。
- 启动时也一样：如果缓存里有该 serial 的上一次记录，就用 `-T last_epoch` 续传，否则用 `-T 1`（只取新日志）或 `-d` 先把整个缓冲区导入一遍，后者为可配置项。

### 4.3 持久化

- **路径**：`os.UserCacheDir()/android-log-mcp/<serial>/`。三大平台分别对应：
  - Linux：`$XDG_CACHE_HOME`，或 `~/.cache`
  - macOS：`~/Library/Caches`
  - Windows：`%LocalAppData%`
  - 绝不写到当前工作目录。允许用环境变量或参数覆盖。
- **格式**：只追加的分段文件，例如每段 16 MB，内容是压缩 JSONL 或长度前缀的二进制记录。每条记录包含 seq、ts、uid、pid、tid、level、tag、msg、buffer、process、package 和 boot_id。
- **内存**：最近 N 条（例如 20 万条）的 ring 加 seq 索引，热查询走内存，冷查询顺序扫描分段文件。
- 这样不需要 SQLite（Go 纯 Go 的 sqlite 体积较大），保持单二进制并且简单。v2 可以按需加 tag/pid 倒排索引。
- **上限**：默认每台设备 512 MB 或 7 天，取先到者，超出后整段删除。全局另设一个上限。
- `clear` 工具可以清本地存储，也可以选择同时执行 `logcat -c`。后者需要用户在调用时显式传参。

### 4.4 logd 溢出与 chatty

- 当前 logd 默认使用 "serialized" 缓冲区，大小 256 KB（low_ram 设备上 64 KB），可以用 `persist.logd.size(.<buffer>)` 覆盖。[源码 `LOGGING/logd/README.property`]
- 读取端跟不上时日志会丢失，而 logd **没有可靠的丢失通知**。**[推断/未见相关接口]**
- 对策：
  1. 读取线程只负责读和入队，解析和落盘放在其他 goroutine，保证尽量不阻塞读取。
  2. 可选 `logcat -G 16M` 调大设备缓冲区。这会修改设备设置，必须显式开启，默认关闭。
  3. 重连时用 `-T` 回补。
- chatty：当前 main 分支的 logd 里已经没有 ChattyLogBuffer。老设备上仍会出现 `chatty … identical N lines` 或 `expire N lines` 这类行，保留它们但打上标记。具体是从哪个 Android 版本移除的 **[未知]**。

### 4.5 stdio 生命周期：要不要独立的常驻 daemon

- MCP 规范规定，stdio 方式下客户端关闭时会关闭 stdin，然后发 SIGTERM，最后发 SIGKILL。[文档 https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle]
- 因此 server 的生命周期等于客户端会话的生命周期：Claude Code 和 Codex 每次会话都会新起一个进程，多个客户端同时运行时就会有多个进程。

| 方案 | 优点 | 缺点 |
|---|---|---|
| A. 进程内记录（stdio 进程自己跑 logcat） | 最简单，没有 IPC，也没有孤儿进程 | 只在会话期间记录，**会话开始前的崩溃抓不到**。启动时可以用 `-d` 导出 logd 的环形缓冲区，但那里只有几百 KB 到几 MB。多个客户端会同时起多个 logcat、重复落盘、互相争抢文件锁 |
| B. 共享 daemon（同一个二进制的 `daemon` 子命令），stdio 进程变成薄代理 | 持续记录，跨会话保留历史；多客户端共用一份数据；pid 表持续维护，质量更高 | 需要 IPC（Unix socket 或 Windows named pipe）和锁文件；代理与 daemon 版本可能不一致；daemon 必须 setsid 或 detach，才能不被客户端的信号连带杀掉（客户端会不会杀整个进程组 **[未知]**）；要处理僵尸、端口和权限问题 |

**推荐 B 的轻量形态**（keynobi 的 "attached/standalone" 是先例）：
1. 同一个二进制，`android-log-mcp`（默认就是 serve）启动后尝试连接 `<cache>/android-log-mcp/daemon.sock`。
2. 连不上时，拿 flock 锁文件，然后以 detached 方式拉起 `android-log-mcp daemon`。如果拉起失败，**退回方案 A**，进程内记录。
3. 握手时校验协议版本。版本不一致时，若 daemon 当前无客户端则让它退出并重启，否则用旧版本继续并给出 warning。
4. daemon 在没有客户端并且超过 idle 时间（默认 30 分钟，可以配置，设为 0 表示常驻）后退出。也可以选择用 launchd 或 systemd 安装成常驻服务。
5. Windows 用 named pipe，或者 127.0.0.1 加随机端口，并把 token 写进缓存目录里的文件。

---

## 5. 更新后的 MCP 工具列表（v1）

所有工具都有可选参数 `device`（serial）；省略时使用当前正在记录的设备。时间参数一律接受 epoch 秒、RFC3339 或相对时长（`10m`）。

| 工具 | 参数 | 返回 |
|---|---|---|
| `list_devices` | — | `[{serial, state, model, sdk, boot_id, recording:bool, buffers, oldest_ts, newest_ts, records, gaps}]` |
| `query_logs` | `query`（AS 语法字符串，可为空）、`since?`、`until?`、`after_seq?`（游标）、`limit=200`（最大 2000）、`order="newest"\|"oldest"`、`format="text"\|"json"`、`max_message_chars=2000` | `{records:[{seq, ts, level, tag, pid, tid, uid, process, package, buffer, message}] 或 text, next_seq, more:bool, gaps_in_range:[…], query_warning?}`。query_warning 例如“非法正则，已按 AS 规则回退为纯文本” |
| `wait_logs` | `query`、`after_seq`（必填）、`timeout_ms=10000`（最大 30000）、`limit=200` | 结构同 query_logs。超时没有新结果时返回空数组和原 `next_seq`。用于长轮询，代替 stream |
| `get_crashes` | `package?`、`since?`、`kinds=["java","native","anr"]`、`limit=20` | `[{kind, ts, pid, process, package, thread?, signal?, reason?, headline, stack:[…], first_seq, last_seq}]`。tombstone 已归到真实崩溃的 pid 上 |
| `get_app_starts` | `package`（必填）、`since?`、`limit=10` | `[{pid, start_type:"cold"\|"warm?", t_request, t_proc_start, t_on_create, t_on_start, t_on_resume, request_to_resume_ms, displayed_ms?, fully_drawn_ms?, activity, evidence_seqs:[…]}]`。warm/hot 的判定规则需要实现时再定 **[推断]** |
| `list_processes` | `package?`、`include_dead=true`、`since?` | `[{pid, uid, process, package, start_ts?, end_ts?, alive, source:"am_proc_start"\|"ps"\|…}]` |
| `recording` | `action:"status"\|"start"\|"stop"`、`buffers?` | `{device, recording, buffers, store_path, store_bytes, records, dropped_or_gaps, daemon:{pid, mode:"daemon"\|"in-process", uptime}}` |
| `clear_logs` | `scope:"store"\|"device"\|"both"`（device 表示执行 `logcat -c`） | `{cleared_records, device_cleared:bool}` |
| `mark` （可选） | `text` | 通过 `adb shell log -t LOGMCP_MARK <text>` 在设备日志里打一个时间锚点，便于“复现前打标记、复现后从标记开始查”。设备上 `log` 命令是否可用 **[需实机确认]** |

工具描述（tool description）里应内嵌一份精简版语法速查：键、运算符、空格规则、`|` 两边要留空格，以及 3 个示例。

---

## 6. 现成的 AS 风格过滤实现（第二轮搜索）

| 项目 | 语言/许可 | 与 AS 的一致程度 | 可复用性 |
|---|---|---|---|
| **AS 本身**（`LogcatFilter.flex/.bnf`、`LogcatFilterParser.kt`、`LogcatFilter.kt`、`LogcatFilterParserTest.kt`） | Kotlin / Apache-2.0 | 标准本身 | **最有价值**：照着 flex 和 bnf 手写 Go 递归下降解析器只需几百行；**把 AST 测试用例移植成 Go 的表驱动测试**，当作一致性基准 |
| gitofleonardo/vscLogFilter（VS Code 扩展，pushed 2026-10-09，0★） | TS / Apache-2.0 | 部分一致：支持 `& \| ()`、`=:`、`-`；**不支持 `~:` 正则**；键名不区分大小写（与 AS 不同）；另加了 pid/after/before | 它的 `reference/as-logcat/` 里存了 AS 的 bnf、kt 和测试副本，`docs/SYNTAX.md` 列了与 AS 的差异，可以参考；代码是 TS，不能直接用。https://github.com/gitofleonardo/vscLogFilter |
| thiagodmont/keynobi（202★，Tauri，macOS） | TS 前端 + Rust / MIT | "AS-like" 方言：支持 `\|` 分组；level 可以写 fatal；自由文本会搜 tag、message 和 package。解析器在前端 `src/lib/logcat-query.ts`，MCP 侧的 `get_logcat_entries` 只接受 tag/text/package/min_level 这些简单参数 | MCP 工具设计和 pid 跟踪思路可以借鉴，语法不可直接搬。https://github.com/thiagodmont/keynobi |
| HBLADEH/CatScope（Go + Vue / MIT） | 自家语法：`tag:`、`pid:`、`level:`、排除。解析在前端 `frontend/src/stores/logs.ts`，Go 侧没有解析器；pid 通过 `pidof` 获得 | 不可复用 |
| carlosmuvi/acli（Go TUI） | 代码里出现了 `package:mine`，没有深入看 | **[未细查]** |
| parfenovvs/lazylogcat（Go TUI，67★） | 用 JSON 配置的过滤器，不是 AS 语法 | 不可复用 |
| flxo/rogcat、google/purr、Sherlog 等 | 各有自家语法 | 不可复用 |

**结论**：没有现成的 Go 版 AS 语法实现，最好的“现成资产”是 AS 自己的 grammar 和测试。建议自己写 Go 的 lexer 和递归下降 parser，再加上隐式 OR 分组和回退成纯文本的逻辑，用移植过来的 AS 测试保证一致。

---

## 7. 未知或待实机验证的清单

1. AS 中 `PROCESS_NAME_MONITOR_MAX_RETENTION` 和 `PROCESS_NAME_TRACKER_AGENT_INTERVAL_MS` 的默认值。
2. 官方文档说 `foo bar` 等于 `'foo bar'`，代码却是 AND；要等 AS 官方确认，或在真机 AS 上实测。
3. 括号内空格并列（`(a b)`）的行为，以及 `tag:foo|bar` 的词法结果。这两条是推断，最好在 AS 里实测。
4. chatty 从哪个 Android 版本开始移除。
5. 旧设备（< Android 8）上 `ps -A -o` 的支持情况；`cmd package list packages -U` 的可用性与输出格式；`-v uid` 在 adb shell 权限下是否输出真实 uid。
6. `logcat --uid` 最早从哪个版本开始支持（也不打算依赖它）。
7. MCP 客户端结束时会不会杀整个进程组，这关系到 daemon 是否必须 setsid。
8. `/data/anr` traces 和 `logcat -L` 在用户版设备上能否读到。
9. 设备上 `log` 命令的可用性（`mark` 工具依赖它）。
