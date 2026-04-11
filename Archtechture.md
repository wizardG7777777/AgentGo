# 现状速览（2026-04-10）

> 本文档原本是设计稿，部分章节早于实现。本节为升级工作提供快速对齐入口，列出**与设计文档不一致的关键实现事实**。后续章节如有冲突，以本节和源代码为准。

**已实现的核心包**（`internal/` 下）：
`agent`（ReAct 循环 + 三层历史压缩 + FileStateCache）、`bootstrap`、`cli`、`config`、`explorer`（只读调查代理）、`hook` + `hook/builtin`（Tool Hook + Mailbox Hook 双框架，4 + 3 个内置 hook）、`llm`、`mailbox`（异步信箱 + Notifier + ring-buffer peek）、`model`、`pathutil`、`roster`（文件级 TryClaim/Release）、`scheduler`（**Phase 3：agent.Agent 实例 + Activator 事件桥 + SchedulerExecutor**，详见后文章节）、`shell`（命令审批/拦截）、`store`（公告板 + TaskCancelRegistry + ToolCallRecord 历史）、`tools`、`trace`（每任务一份 JSONL）、`watchdog`、`webtool`、`worker`（10 个工具的执行代理，可配置 N 个实例）。

**关键实现事实，与原设计文档的差异**：

- **执行代理 = `worker.Worker`**：原文档统称"执行代理"，实际是 `internal/worker` 包，可通过 `cfg.WorkerCount` 配置 N 个实例。每个 worker 拥有 10 个工具（read/write/edit/list/grep/glob/run_shell/publish_task/web_search/web_fetch）。
- **Scheduler = `agent.Agent` 一等代理实例**（2026-04-10 Phase 3 重构）：Scheduler 不再是独立写的 ReAct 循环，而是 `agent.NewAgent(EventType="__scheduler__")` 的实例，工具集 = Worker 全集 + SchedulerGroup（cancel_task + report_done）。它能直接 `read_file`/`grep_search`/`web_search`，自动获得 Tool Hook、3 层历史压缩、FileStateCache、Trace、per-task cancel ctx 等所有 worker 拥有的能力。`scheduler.New` 返回 `*Bundle{Agent, Activator, Mode}`。详见 §"Scheduler 一等代理重构"。
- **Roster 仅做文件级锁，不是团队花名册**：`Roster` 接口的 `TryClaim/Release/ReleaseAll/ListAllAgents` 全部围绕"防文件并发写"。它**不**承担团队成员注册或角色描述功能（设计文档中的"团队花名册"语义未实现，改由 mailbox `TeamSnapshot` 提供轻量替代）。
- **Mailbox 子系统**（设计文档完全未提及）：`internal/mailbox` 提供基于 Go channel 的异步信箱、`send_message` 工具、ack 自动回执、`TeamSnapshot` 团队感知。详见 §"邮箱与异步通讯"。
- **Hook 系统**（2026-04-09 落地）：Tool Hook + Mailbox Hook 两套并列的拦截框架。Tool Hook 在 `agent.NewLLMExecutor` 内部 dispatch 工具调用前后触发（4 个内置 hook：record-artifact / path-boundary / validate-expected-hash / require-read-before-write）；Mailbox Hook 在 `mailbox.Registry.Send` 和 `MailNotifier.scan` 触发（3 个内置 hook：chain-depth-limit / per-agent-dedup / wake-context-expand）。Phase 3 重构后 scheduler 也走 Tool Hook。详见 §"Hook System"。
- **MailNotifier 默认启用**（2026-04-09 Phase 2 完成后恢复）：邮件级联爆炸 P0 的 4 项根因全部修复后，`config.MailNotifierEnabled` 默认为 `true`，空闲 agent 会被自动唤醒读邮件。`ChainDepthLimitHook` (max=3) 在 BeforeSend 阶段截断超深邮件链，杜绝 cascade。
- **架构决策：无 git 依赖**（2026-04-09）：曾经的 `internal/isolation`（git worktree 隔离）整体删除。**AgentGo 代码本体不调用 git**。所有 Worker 共享 `ProjectRoot`。当前并发写文件**唯一防线**是 `Roster` 文件锁 + `expected_hash` TOCTOU 检查 + `pathutil.ValidatePath` 路径越界防护。删 git 后**故意暴露**的 4 项退化（并发写覆盖、半成品回滚、跨任务可见性、杀任务清理）正在等待"多代理协同重建"阶段按真实失败模式驱动设计。
- **任务数据流**（设计文档未提及，2026-04-08 落地 + Phase 3 扩展）：`Task.Artifacts`（实际写入文件清单，`write_file`/`edit_file` 自动追加）、`Task.ExpectedArtifacts`（发布者声明的硬合约，任务结束前由 `agent.checkExpectedArtifacts` 校验，缺失则触发重试）、`Task.LastResponse`（worker 最后一次 LLM 响应，无条件持久化用于失败诊断）、`Task.MailChainDepth`（邮件链跳数，Phase 2 引入）、`Task.SchedulerBatch`（scheduler 当前 reactLoop 跟踪的子任务 ID 列表，Phase 3 引入）。详见 §"产物契约与失败汇报"。
- **TaskCancelRegistry**：per-task cancel context，看门狗/调度器把任务转为 terminal 状态时自动取消正在执行的代理（通过 `ctx.Done()` 即时感知），不依赖广播。
- **崩溃汇报**：任务最终失败时 agent 自动调用 `sendCrashReport`，向 `task.EventSource` 发送 `priority=high` 邮件，附 expected vs actual artifacts、worker 最后响应原文。
- **三层历史压缩**：Layer 1 `snipOldToolResults`（无 LLM 开销，逐轮清理旧工具输出）；Layer 2 `compressHistory`（超过 `CompactTokenThreshold` 时摘要）；Layer 3 context overflow 时 `keepRecent=1` 激进压缩 + RetryRollback。
- **Trace 系统**：`internal/trace` 每任务一份 JSONL 文件，落盘到 `.agentgo/traces/`，保留最近 100 个任务。可通过 `AGENTGO_DUMP_PROMPTS=1` 环境变量额外启用 prompt dump。

**未启动 / 待设计**：
- 多代理协同重建（4 项退化的针对性修复）
- Scheduler 事件响应延迟 ~3 分钟根因排查（Phase 3 重构后旧根因路径已不存在，需要重新观察是否仍出现）
- Trace 多 goroutine 写入竞争（P1 复核）

详细缺陷与状态见 `docs/activate/KNOWN_ISSUES.md`（28/29 已修复）。

---

# Hook System（2026-04-09 阶段 1 + 阶段 2 完成）

Hook System 是工具调用生命周期的拦截层，为"软约束 → 硬约束"提供统一的注册、组合、测试、扩展面。原设计规范已归档至 `docs/archived/hookSystem.md`（阶段 1+2 已全部落地，文档不再更新）。

## 阶段 1 范围

只覆盖 **Tool Hook**（pre-call / post-call）；Mailbox Hook 留到阶段 2。

## 核心组件

| 包 | 职责 |
|---|---|
| `internal/hook` | `ToolHook` 接口 + `ToolHookRegistry`（值传递 ToolHookContext + nil 安全 + Args 浅拷贝隔离 + panic recover） |
| `internal/hook/builtin` | 4 个内置 hook 实现 |
| `internal/store/hookview.go` | `StoreHookView` 只读接口（hook 通过它查询任务历史，不能写入） |
| `internal/store` | `ToolCallRecord` + `AppendToolCall` / `QueryToolCalls` 二级索引 |

## 接入点

`agent.NewLLMExecutor` 在并行工具 goroutine 内按以下顺序：

```
preCtx := hook.ToolHookContext{Phase: PreCall, ...}
preDecision := hookReg.RunPre(preCtx)        // 可 Abort
if Abort:
    content = "[hook 拒绝] " + reason
    toolErr = error
else:
    result, toolErr = tools.Dispatch(ctx, c)

recordToolCall(taskID, ToolCallRecord{...}) // 写历史（独立闭包）

postCtx := hook.ToolHookContext{Phase: PostCall, Result, Err}
hookReg.RunPost(postCtx)                     // 纯观察，不短路
```

`hookReg / storeView / recordToolCall` 三个参数都允许 nil — nil 时整段 hook 路径退化为 noop。这是 V6 回归验证的核心：禁用所有 hook 时行为字节级一致。

## 4 个内置 Hook（注册顺序与优先级）

| Hook | Phase | Prio | Matches | 类型 | 决策 |
|---|---|---|---|---|---|
| `path-boundary` | Pre | 10 | read/write/edit/list/grep/glob_file系工具 | 迁移 | A1: 双重校验（工具内仍 ValidatePath 做标准化） |
| `validate-expected-hash` | Pre | 20 | write_file/edit_file | 迁移 | B1: 接受微秒级 TOCTOU 窗口 |
| `require-read-before-write` | Pre | 30 | write_file/edit_file | **新增** | 新文件豁免；list_dir 不算"已读"；失败 read 不计入 |
| `record-artifact` | Post | 950 | write_file/edit_file | 迁移 | 工具失败时不记录 |

## Scheduler 工具的豁免

Scheduler 的 `publish_task / cancel_task / report_done / send_message` 由 `internal/scheduler/scheduler.go` 内的 `dispatchTool` switch 直接处理，**不经过 `agent.NewLLMExecutor`**。Hook 系统对 Scheduler 工具完全不生效 — 这是有意的架构隔离。

## 与现有硬约束的关系

Hook System **不替换**现有的 8 处硬约束兜底（`checkExpectedArtifacts`、`Roster.TryClaim`、`pathutil.ValidatePath` 等），而是**为它们提供统一的注册、组合、测试、扩展面**。其中 3 处已经迁移到 hook 表达，5 处仍是 inline 实现（包括 Roster 锁，因为它是任务级配对操作不能简单拆成 pre/post hook）。

## 阶段 2：Mailbox Hook（2026-04-09 完成）

阶段 2 把 Hook 框架扩展到 mailbox 子系统，关闭 KNOWN_ISSUES.md 描述的"邮件级联爆炸" P0。与 Tool Hook **并列共存**、独立 registry、独立接口，不耦合。

### 核心组件

| 包 / 文件 | 职责 |
|---|---|
| `internal/hook/mailbox.go` | `MailboxHook` 接口 + 3 个 `MailboxHookPhase` 常量（`PhaseBeforeSend` / `PhaseBeforeDeliver` / `PhaseBeforeWake`）+ `MailboxHookContext` / `MailboxHookDecision`（值传递） |
| `internal/hook/mailbox_registry.go` | `MailboxHookRegistry` —— nil 安全 + panic recovery + Priority 升序，与 `ToolHookRegistry` 形态对称。`RunBeforeWake` 额外做 `WakeDescription` 累加 |
| `internal/hook/mailbox_runner_adapter.go` | `AsMailboxRunner(*MailboxHookRegistry) mailbox.MailboxHookRunner` 适配器，把 hook 包的 registry 包成 mailbox 包内部定义的最小接口 —— 用于打破 hook ↔ mailbox 循环导入 |
| `internal/mailbox/hookrunner.go` | `MailboxHookRunner` 接口（`BeforeSend` / `BeforeDeliver` / `BeforeWake`）—— 定义在 mailbox 包内部，让 mailbox 包**不依赖** hook 包 |
| `internal/mailbox/hookview.go` | `MailboxHookView` 接口（`HasPendingMail` / `GetRecentMessages`）—— hook 通过它读邮件不消费 channel |
| `internal/mailbox/mailbox.go` | `Mailbox.recent` 环形缓冲（容量 16，配合 `Snapshot` 实现 peek-without-consume）+ `Registry.AttachHookRunner` + `Registry.HookRunner` getter |

### 接入点

```
mailbox.Registry.Send:
  preCtx := MailboxHookContext{Phase: PhaseBeforeSend, Message}
  runner.BeforeSend(msg)        // 整条消息级，Abort 直接返回 error
  for each recipient:
      runner.BeforeDeliver(msg, deliverTo)
      // 广播：Abort 仅跳过该收件人；单点：Abort 返回 error
      mb.TrySend(msg)

mailbox.MailNotifier.scan:
  for each non-empty mailbox status:
      // 既有 inline EventType dedup（D4 双重防御内层）
      if pendingNotifyTypes[status.EventType]: continue
      runner.BeforeWake(agentID, eventType, unreadCount)
      // Abort 跳过本次发布；Continue 时累加的 wakeDescription 写入 wake task
      wakeTask := &Task{
          Description: wakeDescription or default,
          MailChainDepth: status.MaxChainDepth,  // 链深度继承
      }
      store.PublishTask(wakeTask)
```

`MailboxHookRunner` nil 时所有 hook 路径退化为 noop —— 这是 V9 回归验证的核心：禁用所有 hook 时既有 mailbox/notifier 测试**未修改通过**。

### 3 个内置 Mailbox Hook

| Hook | Phase | Prio | 类型 | 决策 |
|---|---|---|---|---|
| `chain-depth-limit` | BeforeSend | 10 | **新增** | 拦截 `ChainDepth > MaxDepth` 的消息（默认 max=3）。与 `MetaGroup.sendMessage` 内 inline `ChainDepth = parent.MailChainDepth+1` 写入构成 D3 双重防御 |
| `per-agent-dedup` | BeforeWake | 500 | **新增** | 通过 `StoreHookView.ScanPendingByEventSource("mail-notifier", eventType)` 检查是否已有 pending 唤醒任务。与 notifier inline 的 EventType dedup 构成 D4 双重防御 |
| `wake-context-expand` | BeforeWake | 800 | **新增** | 从 `MailboxHookView` 读最近 5 条邮件，构造人类可读的 wake task description（包含 from/type/summary）—— 这是 D2"有限 Replace 例外"：wake task 在 hook 调用时还不存在，hook 是协助构建而非修改 |

### 4 项根因关闭对照

| KNOWN_ISSUES 根因 | Phase 2 修复 |
|---|---|
| #1 mail-notifier 无去重 | `notifier.scan` inline EventType dedup（保留）+ `PerAgentDedupHook`（D4 镜像） |
| #2 邮件链无环路检测 / 跳数限制 | `Message.ChainDepth` + `Task.MailChainDepth` + `Config.MailChainMaxDepth` + `MetaGroup.sendMessage` 写入 + `ChainDepthLimitHook` 截断 |
| #3 唤醒任务不携带原始上下文 | `Mailbox.recent` 环形缓冲 + `MailboxHookView.GetRecentMessages` + `WakeContextExpandHook` 写 `WakeDescription` |
| #4 worker/explorer prompt 强制回复 | 早期已修复（`worker.go:47-50` + `explorer.go:33-37` 把"应回复"弱化为"可以忽略"+反例） |

### 关键回归测试

- `internal/hook/builtin/cascade_e2e_test.go::TestMailCascade_TerminatesAtMaxDepth` —— 模拟 5 步 worker A↔B 邮件循环，验证第 5 步 `chain_depth=4` 被精确截断
- `TestMailCascade_NoHook_DemonstratesCascadeWouldExplode` —— 反向证明 hook 是 cascade 的唯一防线
- V9 验证：注释 bootstrap 中所有 mailbox hook 注册后，全部 mailbox/notifier/bootstrap/worker/explorer/cli/agent/tools 包测试**未修改通过**

## 阶段 3+ 占位

按需扩展（Chathistory / Board / Session / Skill），触发标准是"≥ 2 个具体痛点"。

---

# Scheduler 一等代理重构（2026-04-10 完成）

Scheduler 在最初的设计里是一个**独立写的事件驱动 ReAct 循环**，不复用任何 `agent.Agent` 基础设施。这是早期遗留——之后所有"基础设施升级"（hook 系统 / 历史压缩 / FileStateCache / Trace / ToolGroup）都没有同步给它。结果协调者比被协调者还弱：scheduler 不能直接读文件、不能搜代码、不能查网页、send_message 不带 ChainDepth 是邮件级联爆炸的隐性源头。

**Phase 3 重构**把 scheduler 重写为**真正的 `agent.Agent` 实例**，同时保留它的事件驱动入口。

## 新架构数据流

```
用户输入 (CLI) ─→ EventUserInput ──┐
                                    ↓
                            scheduler.Activator goroutine
                                    │  PublishTask
                                    ▼
                            store: __scheduler__ task (pending)
                                    │  poll
                                    ▼
                            scheduler agent (agent.Agent 实例)
                            EventType = "__scheduler__"
                                    │  ClaimTask + processTask
                                    ▼
                            SchedulerExecutor (TaskExecutor wrapper)
                              ├─ 等待 task.SchedulerBatch 全部到达终态
                              ├─ 注入 board snapshot 到 history (IncomingMail)
                              └─ 调底层 NewLLMExecutor
                                    │
                                    ▼  LLM 工具调用
                            ToolRegistry
                              ├─ Worker 全集（read/write/edit/grep/glob/list/run_shell/web_*）
                              ├─ MetaGroup（send_message + publish_task with BatchTracker）
                              └─ SchedulerGroup（cancel_task + report_done）
```

## 核心组件

| 包 / 文件 | 职责 |
|---|---|
| `internal/scheduler/scheduler.go` | `Bundle` struct（Agent + Activator + ModeStore），`New(...)` 构造一等代理及其配套部件，`schedulerSystemPrompt`，`currentSchedulerTaskHolder`，`storeBatchTracker` |
| `internal/scheduler/executor.go` | `SchedulerExecutor` —— TaskExecutor wrapper，等 batch + 注入 snapshot |
| `internal/scheduler/snapshot.go` | `BuildBoardJSON` —— 从 store 读全局任务板，输出 JSON |
| `internal/scheduler/activator.go` | `Activator` goroutine，EventCh ↔ task 桥 |
| `internal/tools/scheduler.go` | `SchedulerGroup`：`cancel_task` + `report_done` |
| `internal/tools/meta.go` | `MetaGroup` 新增 `BatchTracker` 字段，scheduler 注入时 publish_task 追加到 `task.SchedulerBatch` |
| `internal/model/task.go` | `Task` 新增 `SchedulerBatch []string` 字段（仅 scheduler task 使用） |
| `internal/store/{iface,memory}.go` | `AppendSchedulerBatch` / `ClearSchedulerBatch` 方法 |

## 关键设计决策

| # | 决策 | 实现 |
|---|---|---|
| D1 | 等待 batch 完成的实现：**SchedulerExecutor 内部同步 select 阻塞** | `executor.go::waitForBatchTerminal` 在 `BatchUpdateCh` 与 30s 兜底之间循环。比 RetryRollback spin loop 干净（不堆 RetryCount，watchdog 友好），比 dependency 机制安全（不会被 worker failed 级联取消） |
| D2 | `task.SchedulerBatch` 是新字段，不复用 Dependencies | Dependencies 严格 completed 语义，scheduler 需要"终态"语义；新字段命名清晰 |
| D3 | `SchedulerGroup` 仅含 `cancel_task` + `report_done` | `publish_task` / `send_message` 复用 `MetaGroup`（共享 ChainDepth 继承等关键行为）；通过 `BatchTracker` 接口让 scheduler 上下文里的 publish_task 追加到 `task.SchedulerBatch` |
| D4 | `Activator` 是 EventCh ↔ scheduler agent 的桥 | `EventUserInput` → `PublishTask`，`EventTask{Completed,Failed,Cancelled,WatchdogAlert}` → `BatchUpdateCh` 信号 |
| D5 | board snapshot 通过 `IncomingMail` 注入 history | 复用既有的 `agent.HistoryEntry.IncomingMail` 字段，与 mailbox 注入对称 |
| D6 | scheduler task 的 `TimeoutSeconds=86400`（1 天） | `MemoryTaskStore.PublishTask` 把 0 替换为默认值，scheduler 必须显式设大值；24h 是工程兜底 |

## scheduler 重构后获得的新能力

| 能力 | 旧 scheduler | 新 scheduler |
|---|:---:|:---:|
| Tool Hook 系统（pre/post call 拦截） | ❌ 完全豁免 | ✅ 走 NewLLMExecutor，所有 hook 自动生效 |
| 3 层历史压缩 | ❌ | ✅ |
| FileStateCache (LRU 50) | ❌ | ✅ |
| Trace events（KindTaskClaimed/Submitted/Completed） | ❌ | ✅ |
| Per-task cancel context (`CancelRegistry`) | ❌ | ✅ |
| `read_file` / `grep_search` / `web_search` 等 worker 工具 | ❌ | ✅ |
| `send_message` 自动写 ChainDepth（共享 MetaGroup） | ❌ ChainDepth 永 0 | ✅ 与 worker 共享同一份 |
| `RecordArtifactHook` 在 scheduler 工具调用时生效 | ❌ | ✅（修复"P0 report_done 不基于 Artifacts"的根因之一） |

## scheduler 保留的独有特征

| 能力 | 实现 |
|---|---|
| 事件驱动入口 | `Activator` goroutine 监听 EventCh |
| 全局任务板视角 | `BuildBoardJSON` 注入到 history，LLM 看到所有 task 的状态 |
| `cancel_task` / `report_done` | `SchedulerGroup`，独占工具，worker 没有 |
| 系统级 mailbox 别名 `"scheduler"` | `New` 构造时 `mbRegistry.RegisterAlias("scheduler", schedID)` |
| 提前 `report_done` 硬拦截 | `SchedulerGroup.report_done` 内部扫描 `task.SchedulerBatch` 状态 |
| `task.Artifacts` 事实校对 | `SchedulerGroup.report_done` 调 `buildSchedulerArtifactsReport` |
| Plan / Immediate mode 切换 | `Bundle.Mode` (`*ModeStore`)，CLI `/mode` 命令通过 setter 切换 |

---

# 代理
代理是最为基础的运行单元，尽管我在后文会频繁提及调度器，但是调度器本身就是一个代理，它也可以回答用户的问题，并且操作有限度的工具。
## 代理工具
代理通过工具与外部世界交互。不同类型的代理拥有不同的工具集。
- **通用工具**（所有代理都具备）：
    - 公告板读写：领取任务、提交结果、读取任务状态与前置结果
    - LLM 调用：向配置的模型端点发送请求并解析响应
- **扩展工具**（由代理配置决定，按需分配）：
    - 文件操作：读取、写入、搜索项目文件
    - 代码执行：运行代码片段并获取输出
    - 网络请求：调用外部 API
    - 命令行操作：执行 shell 命令
- 工具集在代理创建时由配置确定，运行期间不可变更
- 调度器和看门狗等预制代理拥有额外的系统级工具（如发布任务、取消任务），普通执行代理不具备

## 代理操作
代理在运行期间与公告板之间的标准交互流程。
- **领取任务**：代理查询公告板上的可用任务（pending 且并发数未满），选择一个与自身事件类型匹配的任务，执行原子领取操作
- **执行任务**：代理根据任务描述中的 Prompt 调用 LLM，结合自身工具集完成任务。执行过程中代理可以多次调用 LLM（代理内部的 ReAct 循环）
- **提交结果**：执行完成后，代理向公告板提交部分结果，从执行列表中移除自身
- **读取前置结果**：当任务声明了前置依赖时，代理在执行前读取前置任务的输出作为上下文输入
- **请求协助**：代理在执行过程中发现任务超出自身能力时，可向公告板发布子任务请求其他代理协助
- **停止条件**：代理在以下任一条件满足时停止当前任务的执行：
    - **LLM 未调用工具（正常完成）**：LLM 返回的响应中没有任何工具调用，视为代理认为任务已完成。此时代理将完整的执行历史记录和最终结果提交到公告板。
    - **达到最大循环次数**：代理内部 ReAct 循环次数到达配置的上限，强制停止。阈值应设置得足够大，使 90%+ 的复杂调用不会触发。触发后走重试回退路径（processing→pending），重试次数加一，并将已有的部分结果和"因循环上限终止"的标注写入重试原因，使下一个接手的代理能获得充分的上下文提示，避免重蹈覆辙。
    - **超时**：单次任务执行的总时长超过超时阈值，强制停止。超时不走重试回退，而是由调度器介入：调度器将原任务标记为 failed，然后将其重新拆分为更细粒度的子任务重新发布。新的子任务继承原任务已消耗的重试次数（不重置），这样如果任务本身就无法完成，拆分后的子任务也会很快达到重试上限而终止，避免无限拆分。
    - **外部取消**：代理通过 Go context 或专用 channel 收到取消信号（来自看门狗或人类操作员），立即停止当前执行，清理资源，不提交结果。

## 代理配置
代理创建时需要指定的参数，决定代理的行为特征。
- **LLM 模型**：指定使用的模型端点与模型名称（如 Haiku 用于调查代理，更强的模型用于复杂任务）
- **System Prompt 模板**：定义代理的角色、行为约束和输出格式要求
- **工具集声明**：该代理可使用的工具列表
- **事件类型过滤**：代理只领取匹配自身事件类型的任务
- **超时设置**：单次 LLM 调用的超时时间
- **重试上限**：代理内部 LLM 调用失败时的最大重试次数（区别于公告板任务级别的重试）

## 代理生命周期
每个代理对应一个 goroutine，由系统管理其创建到销毁的完整生命周期。
- **创建**：调度器或系统启动时创建代理，分配配置参数，启动 goroutine
- **空闲等待**：代理启动后进入空闲状态，轮询或监听公告板上的可用任务
- **执行中**：代理领取任务后进入执行状态，执行代理内部的 ReAct 循环直到任务完成或失败
- **提交后**：任务完成后，代理回到空闲等待状态，准备领取下一个任务。代理不会在每次任务完成后销毁，而是复用以减少 goroutine 创建开销
- **销毁**：以下情况代理会被销毁——系统关闭时统一回收、人类操作员主动终止、代理长时间空闲且系统代理数超过最低保留数量
- 预制代理（调度器、调查代理、看门狗）在系统启动时创建，生命周期与系统一致，不会因空闲而被回收

## 代理失败处理
代理执行任务失败时的标准处理流程，状态机中多处引用此方法。
- **错误捕获**：代理在 LLM 调用或工具执行过程中捕获错误，记录错误类型与详情
- **可恢复性判定**：根据错误类型判断是否可恢复
    - 可恢复：限流（429）、临时网络抖动、上游服务暂时不可用——触发公告板任务级重试回退（processing→pending）
    - 不可恢复：端点不存在、认证失败、权限不足、响应格式错误——提交为 failed（processing→failed）
- **失败信息写入**：将失败原因写入公告板的任务重试原因字段，供后续审计和调度器决策参考
- **资源清理**：代理失败后清理本次执行中占用的临时资源（如未完成的文件写入、未关闭的连接），然后回到空闲等待状态

# 公告板
公告板是一个信息存储桶，主公告板在程序启动的时候就存在，并且存储调度器和执行代理，以及更多后续启动的所有的Agent传递的消息。
## 为什么设立公告板
- 异步读写，调度器等高层级代理可以先发布任务，然后等执行代理拉取任务。
- 信息共享，所有的Agent都可以读取公告板上的信息，实现信息共享。
- 控制流与数据流拆分，而公告板负责数据流

## 公告板存储什么
- **任务描述**，这是最主要的部分，包含了调度器为这个任务撰写的Prompt内容
- 任务id，自动生成，用于在公告板中标识任务
- 任务优先级，暂时留空，但是这个对于控制流很有帮助，可以在相同的类型中区分哪些任务优先执行，哪些任务后续执行
- 任务依赖，前置依赖的任务 ID 列表，代理领取时公告板检查前置是否已完成，未完成则拒绝领取
- 任务状态，标识任务是否完成的重要参考，并且是看门狗连锁取消后续任务的重要依据
    - pending
    - processing
    - completed
    - cancelled
    - failed
- 任务结果，Agent执行完任务之后都应该返回一些文本内容作为执行结果，这个文本内容可以是Markdown形式，当然也可以是JSON，视任务而定
- 任务错误，如果任务执行失败，应该由负责失败处理的那一段程序去处理失败，这个失败错误一般都是HTTP错误码，当然也可以是其它的错误处理。
- 任务创建时间，用于审计的字段，记录任务被记录进公告板的时间
- 任务开始时间，用于审计的字段，记录任务被执行代理拉取并执行的时间戳
- 执行代理，所有负责该任务的代理都会被记录在这个字段内部。
- 该任务的最大并发数，这个字段有一个默认值，就是启动的时候设定的全局阈值，但是可以由调度器单独设置。
- 任务完成时间，用于审计的字段，记录任务由执行代理提交并完成的时间，但是请注意：执行失败也算是执行完成。而出现执行失败时，这个时间就是任务失败且错误堆栈被正确处理完毕的时间。
- 任务触发的规则，这个是为了更复杂的流程管理设计的，但是如果测试版被证明无用，则删除
- 任务触发的事件源，用于审计的字段，记录是谁提交的这个任务
- 任务触发的事件类型，用于标注事件类型的字段，而执行代理根据事件类型决定是否拉取
- 超时阈值，负载只是一个推测，但是却可以有效规避死锁和超长等待，目前决定使用一个数字代替（单位：秒），而这个数字将用于标记任务预估的事件是多久
- 任务重试的次数，由于LLM的不稳定性，确实需要允许执行失败的任务重试至少一次，而重试太多次的任务应当被判定为无法执行。
- 任务重试的原因，一个用于审计的字段，当触发重试的时候，由代理的失败处理方法进行处理，向公告板提交每一次失败的原因。
### 公告板架构
- 代理对公告板的操作
    - 原子操作（加锁）：
        - 领取任务：检查任务状态为 pending 且当前执行代理数 < 最大并发数 → 将代理加入执行列表，若为首个代理则状态转为 processing，记录任务开始时间
        - 提交结果：代理写回自己的部分结果，从执行列表中移除 → 若执行列表清空（所有代理均已提交），状态转为 completed，记录任务完成时间；若未清空，状态保持 processing
        - 状态转换：校验当前状态是否允许目标转换（参照状态机定义）→ 写入新状态，执行连带操作（如 failed/cancelled 时通知依赖此任务的后继任务）
        - 重试回退：代理提交失败且重试次数未达上限 → 重试次数加一，将失败原因追加至重试原因列表，将代理从执行列表移除，若执行列表清空则状态退回 pending
    - 非原子操作（读快照，无需加锁）：
        - 查询可用任务：代理查询状态为 pending 且执行代理数 < 最大并发数的任务，按优先级排序返回
        - 查看任务状态与结果：调度器、看门狗读取任务的当前状态、执行列表、部分结果等信息
        - 查看前置任务结果：代理读取其所依赖的前置任务的输出，作为自身执行的输入
        - 看门狗巡检：定期扫描所有任务，检查超时、前置失败、长期无人认领等异常情况
- 任务状态机
    - pending->processing: 一个代理领取了任务，应当默认它正在尽全力执行。
    - pending->cancelled: 任务被取消，由人或者看门狗主动操作取消，人可以通过命令行或者控制面板取消代理，但是看门狗的限制则严格地多：
        - 一个任务重试次数超过了全局阈值设定
        - 看门狗会定期扫描部分任务，一旦被发现一个任务的前置条件已经失败或者取消，则连带取消这个任务
    - pending->failed: 任务被判定为失败，这个操作只能由看门狗进行，出现以下场景后判定为失败：
        - 在一个任务提交之后，很长的时间内没有任何代理接取，则由看门狗判定为失败。
    - processing->cancelled: 任务被取消，这个操作只能由人类操作员，看门狗，这两个实体进行：
        - 人类操作员可以在控制面板，或者命令行下达命令，立刻停止一个代理的工作
        - 看门狗可以在确定一个代理超时且消耗了太多的重试次数的前提下，取消它
    - processing->completed: 一个代理完成了任务并且提交结果显示其正确完成
    - processing->failed: 一个任务执行失败，并且是以下几种错误情况，由代理的失败处理方法处理失败，然后在提交的时候提交为失败：
        - 端点不存在，不仅是用户的端点配置错误，而且也有可能是API端点因为不可抗力无法访问
        - 认证错误与权限不足
        - 上游服务发生了内部错误
        - 网络中断
        - 响应式错误，不是OpenAI compatible或者是 genai 的格式
    - processing->pending: 一个任务失败了，但是其并没有触发到重试次数上限，并且不是无法重试的情况，此时返回重试一次。重试的时候，重试次数加一，并且在附加信息中写明失败的原因。
- 公告板等共享区域的底层实现
    - 公告板和花名册在单进程多 goroutine 场景下，使用内存数据结构实现（sync.RWMutex + map/slice），不依赖 Redis 等外部存储
    - 定义抽象接口（interface），上层逻辑只依赖接口而不依赖具体实现，未来如需分布式部署或持久化，可新增 Redis 等实现替换
    - 具体接口定义和数据结构详见 InterfaceDesign.md
- 通知机制
    - 公告板在完成原子写操作后，向事件 channel 发送状态变更信号
    - 调度器通过 Go select 监听该 channel，实现事件驱动的唤醒（详见"事件驱动"章节）
- 任务的结构
    - 任务是公告板中的核心数据单元，包含描述、状态、依赖、执行代理列表、结果、审计时间戳等字段
    - 协作模式下，结果字段为 map 结构（agentID → 部分结果），可追溯每个代理的贡献
    - 完整字段定义详见 InterfaceDesign.md
## 已完成任务的保留策略
- 已完成（completed / failed / cancelled）的任务不立刻删除，保留在公告板中供调度器和调查代理读取分析
- 设立数量上限（全局可配置），超出上限时执行 FIFO 淘汰，最早完成的任务最先被移除
- 历史任务仅作为**参考上下文**，不作为可信缓存——项目文件可能随时间变化导致历史结论过时
- 当调度器需要基于历史任务做决策时，可发布调查任务交由调查代理验证历史结论是否仍然成立

## 什么时候使用公告板
### 公告板写入
- 调度器接受了用户的输入，发布任务
- 执行代理请求更多的协助
- 执行代理完成任务，写回结果
### 公告板读取
- 执行代理拉取任务
- 调度器查看任务
- 看门狗定时查看任务，排除那些已经陷入停滞，长时间阻塞且没有恢复希望的任务

# 预制代理集合
系统启动时内置的特殊代理，各自承担不同的架构职责。

## 调度器（Scheduler）
**Phase 3 重构后**：调度器是一个**真正的一等代理**（`agent.Agent` 实例，`EventType="__scheduler__"`），与 Worker / Explorer 共享同一套底层框架，只是工具集和触发方式不同。详见 §"Scheduler 一等代理重构"。
### 调度器的核心职责
- **接收用户输入**：通过 `Activator` goroutine 把 `EventUserInput` 翻译成 `EventType="__scheduler__"` 任务发布到公告板，scheduler agent 在下次 poll 时认领
- **动态任务拆分**：调度器不需要一次性规划出所有任务，而是根据当前进展逐步拆分。前一批任务完成后，`SchedulerExecutor.waitForBatchTerminal` 唤醒 LLM 进入下一轮决策
- **设置任务依赖**：在发布任务时声明前置依赖（任务 ID 列表），公告板在代理领取时检查前置是否已完成，但不做全局建图或拓扑排序
- **设置任务并发数**：可以为单个任务覆盖全局并发阈值
- **结果汇总**：通过 `SchedulerGroup.report_done` 工具向用户汇报；该工具内部硬性校验 `task.SchedulerBatch` 全部到终态、自动附加 `task.Artifacts` 事实校对块
- **直接执行简单查询**：拥有 worker 全部工具，简单的"读个文件"、"搜下代码"可以自己做，无需 publish 子任务
### 调度器何时直接回答，何时发布任务
- **直接回答**：用户的问题属于系统状态查询、闲聊、意图澄清；或者只需 1-2 次 read_file/grep_search/web_search 就能解决的简单查询
- **发布任务**：信息量超出单次 LLM 调用能处理的范围、或涉及多个独立子问题适合并行调查、或需要持续多步骤的写文件 / 跑命令任务
### 调度器不负责什么
- 不负责长任务的具体执行——通常交给执行代理（Worker），保留自己的上下文容量给规划决策
- 不负责异常检测与任务回收——交给看门狗
- 不负责维护全局任务图——任务之间仅通过依赖字段表达先后关系，无全局 DAG

## 看门狗（Watchdog）
看门狗是系统的健康监控代理，负责巡检公告板和花名册，发现并处置异常任务。
### 看门狗的核心职责
- **超时检测**：发现 processing 状态的任务执行时长超过其超时阈值的 110%，判定为超时
- **无人认领检测**：发现 pending 状态的任务长时间无任何代理领取，判定为 failed，这个长时间是全局变量设置的
- **连锁取消**：发现某任务的前置依赖已 failed 或 cancelled，连带取消该任务
- **重试耗尽处置**：发现任务重试次数超过全局配置的重试上限，取消该任务
- **花名册兜底清理**：作为 defer 机制的最后一道防线，清理因极端情况（如进程级崩溃）残留的花名册声明
### 巡检机制
- 使用 ticker 驱动定期巡检，每次随机抽样扫描公告板中一半的任务
- 超时判定阈值为任务自身记录的超时阈值的 110%，留出余量避免误判
- 重试上限读取全局配置
### 操作权限边界
- **能做的**：取消公告板上的任务（pending→cancelled、processing→cancelled）、判定任务为 failed（pending→failed）、清理花名册残留声明
- **不能做的**：不能发布新任务、不能修改任务内容、不能直接与代理通信——这些是调度器的职责
### 看门狗自身的容错
- 看门狗由 main goroutine 负责拉起和监控
- main goroutine 监控看门狗的存活状态，若看门狗 goroutine 异常退出（panic 或其他原因），立即通过 for 循环 + recover 重启
- 看门狗是无状态的（所有状态都在公告板和花名册中），因此重启后可以立即恢复巡检，不会丢失信息

## 调查代理（Explorer）
调查代理是一个轻量级的只读代理，默认使用快速低成本的 LLM（如 Haiku 级别），专门用于验证和检索。
### 调查代理的核心职责
- **验证历史结论**：调度器基于公告板中的历史任务做决策前，发布调查任务让调查代理确认历史结论是否仍然成立。在以下几个场景中，调度器应当倾向于发布调查任务去总结内容：
    - 有一个或多个目标文件的调查结果完全缺失，调度器无从得知必须文件的内容。
    - 发现存在冲突或者更改，比如：一个文件的调查记录之后存在更改记录，等等情况下，就有必要进行更改。但是这并非程序强制，而是在提示词中进行限制。因为有的时候更改幅度确实不大，可以不启动调查。
- **快速信息检索**：对项目文件、代码、配置等进行只读检索，返回当前状态的快照
- **对比变更**：将历史任务的结论与当前项目状态进行比对，标注哪些结论已过时
### 调查代理的特点
- 只读操作，不修改任何文件或状态
- 默认用轻量级 LLM，降低时间和成本开销
- 任务结果简短明确：结论仍然成立 / 结论已过时（附当前状态摘要）

# 任务依赖管理
本项目不使用有向无环图（DAG）进行全局任务编排。原因：DAG 要求在任务发布前确定完整的任务拓扑，但 LLM 驱动的任务天然是动态展开的，调度器无法在接收用户输入时就规划出完美的任务图。
## 替代方案：轻量级前置依赖
- 每个任务可以声明一个前置依赖列表（零到多个任务 ID）
- 代理领取任务时，公告板检查所有前置任务是否已 completed；若未完成，则拒绝领取
- 前置任务 failed 或 cancelled 时，看门狗巡检发现后连锁取消依赖它的后继任务
- 不做环检测——由调度器在发布任务时自行保证不产生循环依赖，这是调度器作为 LLM 代理的责任
## 工作模式
系统支持两种工作模式，默认启动时为即时模式。CLI 通过 `/mode` 命令切换（**Phase 3 改动**：旧设计提到的 `Shift+Tab` 快捷键未实现，实际是 `/mode` slash command；切换通过 `Bundle.Mode` (`*scheduler.ModeStore`)，scheduler agent 在每次 reactLoop 注入 board snapshot 时实时读取最新 mode）。
### 即时模式（默认）
- 调度器不预先规划完整的任务链，而是作为**"下一步决策者"**被反复唤醒
- 每次唤醒时，调度器只读取公告板的当前状态，然后决定生成 0 个或多个**立即可执行**的下一步任务
- 一个阶段的任务全部完成（或出现失败但至少有 1 个完成）后，触发调度器进入下一阶段的规划
- 系统整体形成一个 ReAct 循环：观察（读公告板）→ 思考（调度器推理）→ 行动（发布任务）→ 观察...
- 任务不使用 Dependencies 字段，先后顺序由调度器的 ReAct 循环自然保证
### 计划模式
- 调度器接收到用户输入后，不立即发布执行任务，而是先发布一系列调查任务，通过调查代理收集项目信息
- 调度器根据调查结果规划出完整的实现路径，一次性发布带 Dependencies 的任务链
- 任务之间的先后顺序由 Dependencies 字段显式声明，公告板在代理领取时检查前置是否已完成
- 适用于大规模重构、多文件联动修改等需要全局视角的复杂任务
### 模式切换
- 系统启动时默认进入即时模式
- 用户在 CLI 输入 `/mode` 切换模式，切换后终端打印当前模式提示
- 模式切换仅影响调度器的规划策略（通过 system prompt 表达），不影响公告板、花名册、看门狗等基础设施的行为
- 切换模式时，scheduler 当前正在 reactLoop 内的决策不受影响（mode 字段在下次 board snapshot 注入时生效）；已发布的子任务也不受影响
## 与 DAG 的区别
- 无全局拓扑排序，无建图开销
- 依赖关系可以随任务动态追加，不需要预先确定
- 代价是失去了全局死锁检测能力，依赖看门狗的超时机制兜底

# 事件驱动
系统以事件驱动为主、poll 兜底为辅的方式运作。**Phase 3 重构后**，事件 → scheduler 的路径由 `Activator` 桥承担，scheduler agent 本身是 poll-based。
## 事件类型
- **任务状态变更**：任务从一个状态转换到另一个状态时触发（如 processing→completed、processing→failed）
- **用户输入**：用户通过命令行或控制面板提交新请求
- **看门狗告警**：看门狗巡检发现异常（超时、前置失败等）
## 事件如何驱动调度器（Phase 3 新架构）
事件 channel 由 `scheduler.Activator` goroutine 消费，转换为对 store / scheduler agent 的副作用：
- **`EventUserInput`** → `Activator` 调 `store.PublishTask({EventType: "__scheduler__", Description: 用户文本})`，scheduler agent 在下次 poll（默认 1 秒间隔）认领该任务
- **`EventTaskCompleted` / `Failed` / `Cancelled` / `WatchdogAlert`** → `Activator` 向 `BatchUpdateCh`（容量 1，select default 防阻塞）发送一个信号；任何正在 `SchedulerExecutor.waitForBatchTerminal` 中阻塞等待 batch 完成的 scheduler 实例会被唤醒重新检查
- **其他事件类型**（如 `EventTaskRetry`、`EventTickerWakeup`）：Activator 忽略
### 事件与调度器决策映射
事件 → Activator → store/Channel 之后，scheduler agent 读到的是 `BuildBoardJSON` 注入的全局任务板快照（含所有 task 状态、Artifacts、依赖、resources），LLM 据此做决策：
- **任务 completed/failed/cancelled**：通过 board snapshot 看到，结合 SchedulerBatch 决定是否进入下一阶段
- **用户新输入**：scheduler task 的 `Description` 字段就是用户文本
- **看门狗告警**：board snapshot 显示该 task 状态变更（通常进入 failed），scheduler 据此决策
### 调度器 ReAct 循环（Phase 3 新架构）
scheduler agent 与 worker / explorer 共享同一套 `agent.Agent.processTask` 实现，区别仅在于 `TaskExecutor` 是 `SchedulerExecutor`（包装 `NewLLMExecutor`）：
1. **认领**：`agent.Agent.Run` poll 到 `__scheduler__` 任务，`ClaimTask` + `processTask`
2. **等待 batch**：`SchedulerExecutor.Execute` 进入前先 `waitForBatchTerminal`——如果 `task.SchedulerBatch` 中还有非终态任务，select 在 `BatchUpdateCh` / 30s 兜底 / `ctx.Done()` 之间循环等待
3. **观察**：调用 `BuildBoardJSON` 生成全局任务板快照，注入到 history 末尾（`IncomingMail` 类型，与 mailbox 注入对称）
4. **思考**：调底层 `NewLLMExecutor` 实际调用 LLM
5. **行动**：LLM 调用 `publish_task`（追加到 `task.SchedulerBatch`）/ `cancel_task` / `report_done`（清空 batch + 打印事实校对块）/ `send_message` / `read_file` / `grep_search` / 等等
6. **循环**：LLM 还有 tool call 则下一轮 reactLoop（`agent.Agent.processTask` 内部 for 循环）；LLM 给文本响应（无 tool call）则任务完成
- `cfg.SchedulerMaxLoops` 控制单次 reactLoop 上限
## 实现机制
- 使用 Go channel 作为事件通道，公告板写操作完成后向 channel 发送事件
- `scheduler.Activator` goroutine 以 select 监听事件 channel，转换为 store/BatchUpdateCh 副作用
- scheduler agent 是普通 poll-based agent，不直接读 eventCh
- 事件 channel 应设置合理的缓冲区大小（默认 64），防止公告板写操作因 channel 满而阻塞

# 子代理交互
执行代理之间的协调通过三个共享状态组件中介：**公告板**负责任务级协调，**花名册**负责文件级资源协调，**邮箱**负责异步消息传递（点对点 + 广播）。代理之间不需要知道对方的存在，也不需要直接连接，天然解耦。

> 注：原设计文档把"花名册"描述为团队成员注册表（含角色描述）。当前实现的 `Roster` 仅做文件级 TryClaim/Release，团队成员感知由 mailbox 的 `TeamSnapshot` 提供（详见 §"邮箱与异步通讯"）。

## 公告板协调
公告板是任务级协调的核心，代理通过它感知整体进度：
- 代理在领取任务前可以看到哪些任务已完成、哪些正在执行、哪些在等待
- 当任务声明了前置依赖时，代理可以读取前置任务的输出作为自己的上下文输入
- 调度器根据公告板的全局状态决定下一步发布什么任务，隐式地协调了代理之间的工作顺序
- 搜索范围的划分由调度器在发布任务时通过任务描述完成，不在运行时动态协调

## 花名册
花名册是独立的资源级协调组件，与看门狗地位等价，背后可以由传统算法或 LLM 驱动。它的职责是管理代理对文件资源的写声明，防止多个代理同时修改同一文件产生竞态和冲突。

### 声明机制
- **声明粒度**：文件路径级别，代理声明"我正在修改 `/path/to/file`"
- **原子操作**：查询与声明是一个原子操作，防止并发声明产生竞态——两个代理同时查询时只有一个能成功声明
- **声明内容**：代理 ID、目标文件路径、声明时间戳、预期完成时间（LLM不能做出准确判断，暂时不选）

### 感知时机
代理不需要全程订阅花名册变更，只在**决策节点**（准备对某个文件采取写操作之前）主动查询一次最新状态：
- 查询成功（无人占用）：写入声明，继续执行
- 查询失败（文件已被占用）：调整计划，转向该任务的其他方向，或等待后重试

### 锁的释放
- **正常释放**：代理完成对文件的修改后，主动清除自己的声明
- **释放机制**：使用 Go 的 defer 机制，代理 goroutine 启动时立即注册 defer 清理函数，无论正常完成、panic 还是 context 取消，都会自动释放该代理持有的所有花名册声明

### 协调示例
以多个代理协作修改 authentication 组件为例：
1. 代理 A 准备修改 `auth.py`，查询花名册，发现无人占用，原子写入声明
2. 代理 B 稍后也需要修改 `auth.py`，查询花名册，发现代理 A 已声明，于是转向修改 `auth_utils.py` 或 `auth_middleware.py` 等其他相关文件
3. 代理 A 完成修改，释放 `auth.py` 的声明
4. 若代理 B 仍需修改 `auth.py`，此时可重新尝试声明

> **当前局限**：Roster 只防"同一时刻两 agent 同时打开同一文件写"，**不防**"agent A 读 → agent B 写 → agent A 写覆盖 B"序列竞争。对后者由 `expected_hash` TOCTOU 检查兜底（`read_file` 返回 SHA256，`write_file`/`edit_file` 可携带 `expected_hash`，写入前校验，不一致则返回"冲突"错误）。

## 邮箱与异步通讯
**`internal/mailbox`** 提供基于 Go channel 的异步信箱系统，支持点对点投递与广播。原设计文档未提及，是 2026-04 实现的能力。

### 组件
- **`mailbox.Registry`**：所有代理信箱的注册中心。每个代理通过 `Register(agentID, eventType, aliases...)` 申请信箱，可注册别名（如 `"scheduler"`、`"explorer-1"`）。
- **`mailbox.Mailbox`**：单个代理的收件箱，内部是带缓冲的 Go channel（容量 = `cfg.MailboxBufferSize`）+ 容量 16 的 ring buffer（Phase 2 引入，用于 hook 系统的 peek-without-consume）。
- **`mailbox.MailNotifier`**：独立 goroutine，定期扫描非空信箱，为有未读邮件的空闲代理发布"唤醒任务"。**默认启用**（`cfg.MailNotifierEnabled=true`，2026-04-09 Phase 2 完成后恢复）。Phase 2 的 `ChainDepthLimitHook` (max=3) + `PerAgentDedupHook` + `WakeContextExpandHook` 三层防御彻底关闭了邮件级联爆炸 P0。

### 消息结构（`mailbox.Message`）
- `From` / `To` / `Content` / `Summary` / `SentAt`
- `Type`：`info` / `question` / `reply` / `steer` / `ack`
- `Priority`：`low` / `normal` / `high`
- `ChainDepth`：邮件链跳数（Phase 2 引入），由 `MetaGroup.sendMessage` 自动写入 `parent.MailChainDepth + 1`，超过 `cfg.MailChainMaxDepth`（默认 3）时被 `ChainDepthLimitHook` 在 `BeforeSend` 阶段拒绝

`DrainWithAck` 在代理消费消息时自动向发送方回送 `type=ack` 已读回执。

### 工具与代理集成
- `send_message` 工具注册在 worker / explorer / scheduler 三类代理上（**Phase 3 后**：scheduler 也通过共享的 `MetaGroup.sendMessage` 注册，消除了之前的双写实现），支持 `to=<agentID>` 点对点或 `to=*` 广播（自动跳过自己）。
- 代理任务开始时，从 `Registry` 拉取 `TeamSnapshot`，把队友 ID + 忙碌/空闲状态 + 当前任务摘要注入为首条 `<team-snapshot>` 系统消息，让 LLM 知道"此刻谁在做什么"。
- 邮件以 `<agent-mail type=... priority=...>` XML 子标签形式注入 LLM 上下文，prompt 引导代理根据 type 做差异化响应。

### Scheduler 接收邮件
Scheduler agent (Phase 3 后) 与 worker / explorer 共享同一套 `Mailbox.DrainWithAck` 机制——`agent.Agent.processTask` 在每轮 reactLoop 开始时自动 drain 收件箱，把消息以 `IncomingMail` history entry 形式注入 LLM 上下文。`/steer` 命令投递的用户消息以及其他代理通过 `to="scheduler"` 别名发来的消息都走这条路径。

## 产物契约与失败汇报
2026-04-08 落地的硬约束机制，用于解决 worker 凭空捏造任务结果 / 任务无文件产出两个 P0 缺陷。

### `Task.Artifacts`（实际产出清单）
- `write_file` / `edit_file` 成功后自动调用 `Store.AppendArtifact(taskID, path)`，路径经 `normalizeArtifactPath` 标准化为相对项目根的相对路径。
- 下游任务通过 `Store.GetDependencyArtifacts(taskID)` 获取所有上游任务的实际产出清单，由 `agent.processTask` 注入到 user prompt 的"前置任务结果"段，文案明确告知"必须 read_file 这些文件，不要凭空生成"。

### `Task.ExpectedArtifacts`（发布者声明的硬合约）
- Scheduler 通过 `publish_task` 工具的 `expected_artifacts` 参数声明任务必须产出哪些文件。
- 任务结束前 `agent.checkExpectedArtifacts` 扫描 `task.Artifacts`，缺失任何 expected 文件则触发 `handleFailure` 重试，错误消息明确告知"缺失 X，已写入 Y"。
- 路径精确匹配失败时按 `filepath.Base` 兜底命中并记 `Drifted` warning，避免硬卡。
- Explorer 是只读代理，scheduler 和 meta 工具双端硬拒绝 `event_type=explore && expected_artifacts != nil`。

### `Task.LastResponse`（失败诊断锚点）
- Worker 每次 non-tool LLM 响应都通过 `Store.RecordLastResponse(taskID, content)` 无条件持久化，无论后续校验成败。
- 任务最终崩溃时 `sendCrashReport` 把 LastResponse 原文附在邮件正文里发给 `task.EventSource`，scheduler 不再只看到一个干瘪的"重试次数耗尽"。

### 校验反馈进入历史
`appendValidationFeedback` 把 ExpectedArtifacts 校验失败的诊断（缺失文件、实际写入文件、纠正策略）作为 `<validation-feedback>` 段以 `IncomingMail` 形式注入历史，重试时 LLM 能直接看见自己为何被打回，避免"重试还是同样输出"的死循环。

### 终态崩溃汇报
`agent.terminateTask` 在 RetryCount >= MaxRetry 时调用 `sendCrashReport`，向 `task.EventSource` 发送 `priority=high` 邮件，正文格式："代理 X 在执行任务 Y 时崩溃，原因 Z；任务描述、重试次数、expected vs actual artifacts、worker 最后一次响应原文"。

# 系统启动流程
系统由 `main.go` → `bootstrap.Bootstrap(configPath, explicit)` 完成初始化，再由 `System.Start(ctx, cancel)` 拉起所有 goroutine，最后 `System.RunCLI(ctx, stdin, stdout)` 阻塞主线程。

## Bootstrap 阶段（构造对象图）
1. **加载配置**：`config.LoadConfig`，YAML/JSON 自动判别，文件不存在时回退默认值。打印：`[启动] 全局配置加载完成`
2. **初始化 Trace 系统**：`trace.NewWriter(.agentgo/traces, 100)` + 可选 `PromptDumper`（`AGENTGO_DUMP_PROMPTS=1` 启用）。失败仅 warning，不中断主流程。打印：`[启动] Trace 系统已启动` 或 warning
3. **初始化公告板**：`store.NewMemoryTaskStore` + `store.NewTaskCancelRegistry`，把 cancelRegistry 注入 store（terminal 状态转换时自动取消正在执行的代理）。打印：`[启动] 公告板初始化完成`
4. **初始化 Tool Hook 系统**：`hook.NewToolHookRegistry()` + 注册 4 个内置 hook（record-artifact / path-boundary / validate-expected-hash / require-read-before-write）。打印：`[启动] Hook 系统初始化完成（已注册：...）`
5. **初始化花名册**：`roster.NewMemoryRoster`。打印：`[启动] 花名册初始化完成`
6. **初始化邮箱注册表**：`mailbox.NewRegistry(cfg.MailboxBufferSize)`。打印：`[启动] 邮箱注册表初始化完成`
7. **初始化 Mailbox Hook 系统**：`hook.NewMailboxHookRegistry()` + 注册 3 个内置 hook（chain-depth-limit / per-agent-dedup / wake-context-expand）+ `mbRegistry.AttachHookRunner(hook.AsMailboxRunner(mailboxHookReg))`。打印：`[启动] Mailbox Hook 系统初始化完成（已注册：...）`
8. **创建 LLM 客户端**：scheduler / explorer / worker × N 各自创建 `llm.NewSDKClient`（OpenAI 兼容 SDK）
9. **创建看门狗**：`watchdog.New(store, cfg, eventCh, roster)`
10. **创建调查代理**：`explorer.New(store, roster, llm, cfg, cancelRegistry, mbRegistry, hookReg, storeView, recordToolCall, searchProvider)`
11. **创建命令审批通道**：`approvalCh := make(chan shell.ApprovalRequest, 8)`，Worker→CLI 通道
12. **创建调度器**（Phase 3 重构）：`scheduler.New(store, roster, schedulerLLM, eventCh, cfg, cancelRegistry, mbRegistry, approvalCh, hookReg, storeView, recordToolCall)` 返回 `*scheduler.Bundle{Agent, Activator, Mode}`。**注意**：scheduler 现在需要 roster + approvalCh + hook 三件套，与 worker 一致；构造在 explorer / worker 之后，因为它依赖 approvalCh
13. **创建执行代理**：`worker.NewWithID("worker-N", ...)` × `cfg.WorkerCount`，每个 worker 持有独立 LLM client
14. **创建邮差通知器**：`mailbox.NewMailNotifier(...)` 对象

## Start 阶段（拉起 goroutine）
- **Scheduler 双 goroutine**（Phase 3）：先启 `Scheduler.Activator.Run(ctx)`（事件桥），再启 `Scheduler.Agent.Run(ctx)`（poll-based agent）。Activator 必须先就绪，否则 `EventUserInput` 在 Agent 未启动时到达可能丢失。打印：`[启动] 调度器已启动 (agent + activator)`
- **看门狗**：`runWatchdogWithRecover(ctx)` — for 循环 + recover，panic 后 1 秒延迟重启。打印：`[启动] 看门狗已启动`
- **邮差通知器**：`MailNotifier.Run(ctx)`，仅当 `cfg.MailNotifierEnabled=true` 时启动（**默认启用**）。打印：`[启动] 邮差通知器已启动`
- **调查代理**：`Explorer.Run(ctx)`。打印：`[启动] 调查代理已启动`
- **执行代理**：`Worker[1..N].Run(ctx)` × `cfg.WorkerCount` 并行 goroutine。打印：`[启动] 执行代理已启动 (N 个)`
- 最后打印：`[启动] 系统就绪，等待用户输入`

## RunCLI 阶段
- `cli.New(...).Run(ctx)` 阻塞主线程，处理用户输入与命令（`/quit` `/mode` `/status` `/cancel` `/help` `/steer`）
- 用户输入由 CLI 通过 `eventCh` 发送 `EventUserInput`，`scheduler.Activator` 翻译为 `EventType="__scheduler__"` 任务，scheduler agent 在下次 poll 时认领并 reactLoop（详见 §"Scheduler 一等代理重构"）

## 启动顺序约束
- 公告板和花名册是基础设施，必须先于所有代理初始化
- Scheduler 先于其他代理 goroutine 启动（消费者先就绪，避免事件丢失）
- 看门狗先于 explorer/worker 启动，确保第一批任务就处于监控之下
- 任一步骤失败时返回 error 终止启动，不进入半初始化状态

# 全局配置
系统运行所需的全局参数，从 `setting.yaml` 或 `setting.json` 读取，文件不存在时使用内置默认值。当前仅支持 `-config <path>` 命令行参数指定文件路径，**单字段命令行覆盖未实现**。配置定义在 `internal/config/config.go`。

## 配置项（与 `Config` 结构体一一对应）
| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| **任务调度与并发** | | |
| max_retry | 任务级重试上限，超过则由看门狗/agent 取消 | 3 |
| default_concurrency | 单个任务的默认最大执行代理数 | 2 |
| fifo_limit | 公告板保留已完成任务的数量上限（依赖感知淘汰） | 100 |
| event_channel_buffer | 事件 channel 缓冲区大小 | 64 |
| default_timeout_sec | 任务默认超时阈值（秒） | 300 |
| worker_count | 启动的 Worker 实例数 | 1 |
| **代理 ReAct 循环** | | |
| agent_max_loops | 执行代理内部 ReAct 最大循环次数 | 50 |
| agent_idle_threshold | agent 连续空轮询次数达到阈值后退出 goroutine（0 = 禁用） | 0 |
| compact_token_threshold | Layer 2 历史压缩触发阈值（prompt tokens） | 80000 |
| compact_keep_recent | 历史压缩时保留最近 N 条消息 | 3 |
| max_subtask_depth | worker 通过 `publish_task` 发布的子任务允许的最大深度（scheduler 模式无此限制） | 1 |
| **调度器** | | |
| scheduler_ticker_sec | _（Phase 3 后已不使用，仅保留向后兼容）_ 旧 scheduler 事件循环 ticker 兜底间隔 | 10 |
| scheduler_max_loops | scheduler agent 单次 `processTask` 内 ReAct 循环次数上限（与 `agent_max_loops` 平行） | 10 |
| **看门狗** | | |
| watchdog_interval_sec | 看门狗巡检间隔（秒） | 30 |
| **LLM 后端** | | |
| llm_base_url | OpenAI 兼容 API 端点 | （无） |
| llm_api_key | API 密钥 | （无） |
| llm_model | 主模型名称（用于 scheduler 和 worker） | gpt-4o |
| llm_timeout_sec | 单次 LLM 调用超时（秒） | 60 |
| explorer_model | 调查代理使用的轻量模型 | gpt-4o-mini |
| explorer_event_type | 调查代理监听的事件类型 | explore |
| **Shell 与文件** | | |
| shell_timeout_sec | `run_shell` 命令默认超时（秒） | 30 |
| project_root | 项目根目录（路径越界检查基准） | （无，由 main 设置） |
| **邮箱与代理通讯** | | |
| mailbox_buffer_size | 单个代理信箱的 channel 缓冲容量 | 32 |
| mail_notifier_interval_sec | 邮差扫描间隔（秒） | 5 |
| mail_notifier_enabled | 邮差是否启动（Phase 2 完成后默认启用，cascade P0 已被 hook 系统关闭） | true |
| mail_chain_max_depth | 邮件链跳数上限（Phase 2 引入）。`MetaGroup.sendMessage` 写入 `parent.MailChainDepth+1`，超过此阈值的邮件被 `ChainDepthLimitHook` 在 BeforeSend 拒绝 | 3 |
| **Web 检索** | | |
| search_api_provider | 搜索 provider 名称 | duckduckgo_html |
| search_api_url | 搜索 API URL（如 provider 需要） | （无） |
| search_api_key | 搜索 API 密钥（如 provider 需要） | （无） |

## 配置加载顺序
1. 通过 `-config <path>` 命令行参数获取配置文件路径（默认 `setting.yaml`）
2. `LoadConfig` 按文件后缀（`.yaml`/`.yml`/`.json`）选择解析器
3. 文件不存在时：
   - 显式指定（`-config explicit`）→ 报错终止
   - 默认路径 → 打印 warning 后使用内置默认配置
4. 解析后字段以文件值为准，未指定字段保持 `DefaultConfig()` 默认值
5. **单字段命令行覆盖（如 `-worker_count=3`）暂未实现**

