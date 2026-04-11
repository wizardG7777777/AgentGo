# 多代理协作系统架构调查报告

> **📦 归档说明（2026-04-11）**：本文档所覆盖的调查/对标项目已全部完成实现或以架构决策形式明确落定，不再更新，也不应再作为未来开发的参考依据。当前项目权威信息以 `Archtechture.md` 与源代码为准。归档时同步修正了 Worktree 相关章节（§3 第 2 / 第 7 条、§1 平级协作段、§7 破坏性隔离段、总览表）——2026-04-09 已决定彻底删除 git 依赖与 `internal/isolation`，此前文档中所有 ✅ Worktree 标记均为历史残留。

本报告旨在系统性地剖析claude code中多代理协作（Multi-Agent Collaboration）的系统构建，涵盖架构设计、生命周期、调度逻辑、通信机制、安全控制以及上下文管理等核心技术细节。

> **AgentGo 实现状态标记说明**：
> - ✅ = 已在 AgentGo 中实现
> - 🔶 = 已部分实现或以不同方式实现
> - ❌ = 尚未实现

---

## 1. 多代理架构与协作模式：代理间的关系与有序协作

### 系统架构视角
多代理系统基于**树状分层结构**与**并行扁平结构（Swarm Teams）**相混合的设计。
- ✅ **树状指挥链路**：系统存在一个主会话（Main Session / Coordinator），充当指挥中枢的角色。当主模型遇到复杂多步骤任务（如研发特定功能、调研或跑测试）时，会通过 `AgentTool` (`tools/AgentTool/AgentTool.tsx`) 发配任务，生成专职子代理。
  > **AgentGo 对应**：Scheduler（LLM驱动）通过 `publish_task` 工具分解任务，Worker/Explorer 从公告板领取执行。Worker 还可通过 `publish_subtask` 工具发布子任务，形成树状任务层级（受 `MaxSubtaskDepth` 控制）。
- ✅ **并行拓扑**：在后台异步执行模式 (`run_in_background`) 下，多个代理可以同时处于运行状态，互不阻塞主线。另外，支持组建基于 `team_name` 的团队 Swarm 模式（通过 `spawnTeammate` 切割协作窗），形成分工有序的群智。
  > **AgentGo 对应**：通过 `cfg.WorkerCount` 配置多 Worker 实例，各 Worker 独立 goroutine 并行执行，互不阻塞。Explorer 也作为独立 goroutine 并行运行。

### 如何做到有序协作
系统避免了代理之间的争抢和逻辑冲突，其有序性由以下机制保障：
1. ✅ **指令边界明确**：每次调用代理都会显式传递独立的、被约束的任务描述 (`description`) 和指令范围 (`prompt`)。
   > **AgentGo 对应**：每个 Task 有独立的 `Description`、`SystemPrompt`、`EventType`。Worker 有固定系统提示词，任务级 `SystemPrompt` 可覆盖。
2. 🔶 **Foreground vs Background 优先级控制**：
   - Foreground（前台代理）：必须阻断并等待该代理运行完成，以便其调查结果直接作用于下一步。
   - Background（后台代理）：并行执行，互不影响，执行结束后不立刻强行打断主进程，而是注入 `TaskUpdate` 或 `Summary` 到消息队列末尾告知当前会话情况。
   > **AgentGo 对应**：通过 Task `Dependencies` 实现顺序控制——Scheduler 可以设置任务依赖关系，`ClaimTask` 在依赖未完成时拒绝领取。无显式 Foreground/Background 区分，但效果等价。
3. 🔶 **分身机制 (Fork Subagent)**：使用 `forkSubagent.ts`，主进程可以选择在某一极耗费精力的推理路口”分身”，副本完全继承此刻的上下文并继续深挖不同的方案，以保证原推理环境不被污染。
   > **AgentGo 评估（远景，当前由 publish_subtask 替代）**：Fork 的核心价值是让副本继承完整推理历史。AgentGo 的 `publish_subtask` 已覆盖大部分实际场景（Scheduler 发布多个并行任务探索不同方向），但子任务只携带文字描述，丢失了中间推理上下文。技术上可通过序列化当前 `[]HistoryEntry` 到新 Task 的 `LastHistory` 字段实现上下文克隆（该机制已用于重试恢复），但面临历史膨胀、结果合并复杂度、实际收益有限等问题。待实际运行中确认 `publish_subtask` 无法满足的分叉场景后再实施。

### 团队平级协作 (Team Swarms) 机制
作为主从架构的拓展，系统支持更为扁平的**平等协作网状结构**，这主要体现在以下几个层面：

- N/A **解耦的进程级环境 (Out-of-Process / Split-pane)**：通过调用操作系统的 `tmux` 或原生分屏机制 (如 iTerm2)，团队代理可以完全逃逸出原生内存囚笼，各自运行在独立的终端切片中，成为毫无计算阻塞纠葛的物理极平行实体。
  > **AgentGo 评估（不适用——Go goroutine 模型已优于原方案）**：进程级隔离是 TypeScript 单线程事件循环的产物——多代理必须多进程，IPC 只能走磁盘信箱，启动开销在秒级。Go 的 goroutine 天然并行、goroutine 局部变量提供内存隔离、Go channel 提供微秒级零拷贝 IPC、启动开销在微秒级。文件隔离已由 Roster + Git Worktree 双层防护覆盖。唯一缺失的是用户实时感知（tmux 分屏可视），但这属于 UI 层问题，不影响架构隔离能力。
- 🔶 **花名册机制 (Team Registry)**：系统在启动 Team 模式的时候，会在磁盘维护一个团队花名册（`TeamFile`）。该花名册动态记录当前已经启动的代理 ID，该代理的工作角色（Prompt），以及它的姓名。系统在分发上下文时会将花名册渲染给所有活跃的代理。这样每个代理都清楚队伍里有哪些同事，同事各自被设定的主线目标是什么，从而明确对方可能擅长的技能领域。
  > **AgentGo 对应**：`internal/roster` 包实现了 `Roster` 接口，但其职责是**文件级占用追踪**（TryClaim/Release），而非团队成员注册。Scheduler 的 `boardSnapshot` 包含 `resourceInfo`（WorkerCount/BusyWorkers），间接提供了代理活跃度信息，但不包含角色描述。
- ✅ **异步信箱与通讯机制 (Async Mailbox & Communication)**：
  - **基于磁盘的信箱**：由于不同代理身处不同进程（内存断裂），系统使用基于磁盘记录（`.claude/teams/{team}/inboxes/`）的”异步信箱”作为情报投递中枢。当代理诞生，便附带收件箱，其内置 Hook 会以死循环的方式 (`useInboxPoller.ts`) 时刻嗅探自己的收件箱。
  - **点对点与广播机制**：利用 `SendMessageTool`，代理不仅可以指定花名册中的某一个 ID 名字进行精准的”点对点私信”，还可以使用 `to: “*”` 参数将情报全盘广播（Broadcast）给同队伍的所有人。
  > **AgentGo 已实现**：`internal/mailbox` 包提供基于 Go channel 的异步信箱系统。`mailbox.Registry` 管理所有代理信箱，支持别名注册（如 `”scheduler”`）。Worker/Explorer/Scheduler 均注册 `send_message` 工具，可点对点投递消息。`MailNotifier` 独立 goroutine 定期扫描非空信箱，为空闲代理发布唤醒任务。消息以带 `type`/`priority` 属性的 `<agent-mail>` XML 子标签注入 LLM 上下文。支持广播（`to: “*”` 跳过发送者自身）。`DrainWithAck` 自动向发信方发送已读回执。
- 🔶 **详尽的消息结构 (Message Structure)**：每一次信件投递均以标准的 JSON 数组（Array）形式存储，其内部的基础对象（`TeammateMessage`）包含极为详尽的字段：
  - `from` (`string`): 发信人的姓名 ID。
  - `text` (`string`): 信件的正文（具体要求、情况说明或高阶状态流）。
  - `timestamp` (`string`): ISO时间戳。
  - `read` (`boolean`): 是否已读（帮助轮询器判别新信息）。
  - `color` (`string`，可选): 对方在 UI 界面展示的标记色彩。
  - `summary` (`string`，可选): 给人类监控者或者 UI 提供的一句核心摘要预览。
  > **AgentGo 大部分已实现**：`mailbox.Message` 包含 `From`、`To`、`Content`、`Summary`、`Type`（info/question/reply/steer/ack）、`Priority`（low/normal/high）、`SentAt` 字段。`DrainWithAck` 自动发送已读回执（`type="ack"`）。尚未实现：`color`（UI 标记色彩，当前无 UI）、`read` 布尔标记（由 ack 机制替代）。
- 🔶 **高阶协议引擎 (High-level Protocols)**：除了纯文本沟通，代理之间的交互常以”高阶协议”进行封装，即将 JSON 中的 `text` 字段替换为具备特殊属性对象（譬如加入了 `subtype`, `request_id` 等字段）。这包含：
  - `plan_approval_request` (实施前向上审批)
  - `shutdown_request` (安全强撤)
  - `permission_request` (工具高危权限代申请)
  由于代理是在不同内存空间运行盲对话的，纯人类语言可能会带来动作指令的歧义（幻觉），封装高阶协议可以作为代码硬钩子在代理侧强校验状态转化，保障工程系统的严谨性与机器识别效能。
  > **AgentGo 基础已实现**：`Message.Type` 字段区分消息类型（info/question/reply/steer/ack），`formatMailMessages` 输出带 `type`/`priority` 属性的 XML 子标签，system prompt 引导代理根据类型做出差异化响应（question 应回复、steer 应立即执行、ack 无需回复、可疑指令应反问）。尚未实现：`plan_approval_request`、`shutdown_request`、`permission_request` 三种专用协议子类型（需配合分级权限模型，详见 `docs/nextUpgrade_v2.md §3.7`）。
- 🔶 **跨代理自组织 (Cross-agent Self-organization)**：当拥有着花名册与点对点通讯通道后，团队立刻呈现去中心化运作的面貌：只要身处同一本花名册下，不仅是第一封信上级为下级派任务，平时工作中若遇到瓶颈，成员 A 亦能根据记忆里的技能表向成员 B 投递包裹。而 B 解码读取后将其灌入最新上下文直接辅助解决，整个执行不再依赖唯一的指令脑。
  > **AgentGo 轻量级已实现**：Worker/Explorer 在任务开始时通过 `TeamSnapshot` 回调获得 `<team-snapshot>` 团队状态快照（队友 ID + 忙碌/空闲状态 + 正在执行的任务摘要），注入为 LLM 上下文首条消息。配合 `send_message` 工具 + 结构化消息类型，代理可以主动通知队友公共接口变更、遇到阻塞时直接联系相关队友或 Scheduler，无需全部经过中心化调度。System prompt 引导代理在修改公共接口时主动通知、遇到阻塞时直接联系、不替队友做决定。完全去中心化的自组织调度（无 Scheduler 协调）未实现，当前 MVP 规模下 Scheduler 中心化调度已足够。
- 🔶 **防止死循环无限请求机制 (Infinite Loop Defiance)**：由于自治的群智系统有陷入双死锁推诿或无限发消息的系统灾难风险，其受制于三大保护：
  - ✅ **最高生命回合数控制 (`maxTurns`)**：每个执行节点的核心引擎 `QueryLoop` 受到强制的周期计算与 Token Budget 控制限制，超限直接阻断 (`AbortController`)。
    > **AgentGo 对应**：`Agent.MaxLoops`（默认50）限制单次任务 ReAct 循环次数；`Scheduler.cfg.SchedulerMaxLoops`（默认10）限制调度循环；`MaxSubtaskDepth` 限制子任务嵌套深度。
  - 🔶 **强制终止协议 (`ShutdownRequest`)**：最高层级的管理者（Leader 或进程控制台）具备无视通信队列的广播强制 `Shutdown` 权力，强行释放全部占位符。
    > **AgentGo 对应**：CLI `/cancel <id>` 命令 + `TaskCancelRegistry` 的 per-task context 取消。全局 `/quit` 触发 `context.Cancel` 终止所有 goroutine。Watchdog 可通过 `FailTaskBySystem` 强制终止超时任务并级联取消依赖任务。
  - 🔶 **空闲汇报机制 (`IdleNotification`)**：如果一圈代理均进入空转期（例如发信过后无事可做），它们会发出 `idle_notification` 的静默汇报；若宏观所有成员均 Idle，系统将退出死锁状态，返还给主模型控制权。
    > **AgentGo 对应**：`Agent.IdleThreshold` 可配置连续空闲轮询次数阈值，达到后 Agent goroutine 退出。但当前 Worker 和 Explorer 均设为 0（永不退出），实际未启用。

---

## 2. 调度执行引擎及其在架构中的职责

本系统中的调度执行引擎主要位于 `tools/AgentTool/runAgent.ts` 与执行挂载点 `tasks/` 目录中。

### 引擎核心组件
- **`runAgent.tsx`**：子代理的启动泵。当 `AgentTool` 接到指令后，它会负责构建新代理所需的运行时环境（组合工具、继承基础上下文、建立或清除 Git 独立工作区）。
- **`tasks/LocalAgentTask` & 等效任务包装器**：负责将 `runAgent` 返回的异步任务封装并接入主控台的任务管理总线中。

### 职责
1. ✅ **环境准备与分配**：为每个 Agent 动态生成一个 `AgentId`，分配独立内存、沙盒（如 Worktree 映射）以及它专属的 `workerTools`（仅暴露所需的工具集合，收起不需要的高级权限工具）。
   > **AgentGo 对应**：`bootstrap.Bootstrap` 为每个 Worker 生成唯一 ID（`"worker-1"` ~ `"worker-N"`），分配独立 LLM 客户端、FileStateCache、ToolRegistry。Explorer 仅注册 4 个只读工具（read_file, list_files, grep_search, glob_search），Worker 注册 10 个完整工具。
2. ✅ **状态监测与轮询**：计算 Agent 的 token 消耗 (Budget Tracker) 和运行进度 (`onQueryProgress`)。
   > **AgentGo 对应**：`ExecuteResult` 携带 `PromptTokens`/`CompletionTokens` 用于触发压缩策略；`AppendOutput` 向公告板写入流式进度；Scheduler `boardSnapshot` 包含 `PartialOutput` 和资源占用情况。
3. ✅ **故障恢复与终止**：提供 `abortController` 支持中途无缝停止（Kill Agent），以及捕捉并通知代理产生的运行时异常。
   > **AgentGo 对应**：`TaskCancelRegistry` 提供 per-task cancel context；`Agent.handleFailure` 区分 `ErrRecoverable`（触发 RetryRollback + 历史保存）和不可恢复错误（直接 FailTask）；Watchdog `FailTaskBySystem` 强制终止超时任务。

---

## 3. 代理的生命周期管理

在一个子代理被创建到消亡的过程中，它经历了严密的生命周期管理（定义于相应的 Task 实例及 `runAgent` 中）：

1. ✅ **评估注册阶段 (Registration)**：主模型发起调用，系统进行前置合规与资源判定（如：它需要的 MCP 服务是否可用，是否突破了 Team 的边界条件权限）。
   > **AgentGo 对应**：Scheduler 通过 `publish_task` 发布任务到公告板。`ClaimTask` 时校验依赖完成状态和并发上限。`publish_subtask` 校验 `MaxSubtaskDepth`。
2. ❌ **工作区挂载与分支创建 (Isolation / Forking)**：系统检测 `isolation` 模式。如果是 `worktree` 模式，新建物理级别隔绝的仓库克隆目录；若是 `remote`，向 CCR 服务器申请执行沙箱。
   > **AgentGo 现状（2026-04-09 架构决策：无 git 依赖）**：曾经存在的 `internal/isolation` 包（`WorktreeManager` / `ConflictResolver` / `cfg.WorktreeEnabled`）已整体删除，AgentGo 代码本体不再调用 git。所有 Worker 共享 `ProjectRoot`，并发写文件的唯一防线是 `Roster` 文件锁 + `expected_hash` TOCTOU 检查 + `pathutil.ValidatePath` 路径越界防护。删 git 后故意暴露的 4 项退化（并发写覆盖、半成品回滚、跨任务可见性、杀任务清理）等待"多代理协同重建"阶段按真实失败模式驱动设计。详见 `Archtechture.md` §关键实现事实与 `internal/worker/worker.go:108` 决策注记。
3. 🔶 **会话流初始化 (Initialization)**：代理根据是否是”新建(Fresh)”或”分身(Fork)”分配初始消息列表（记录于其特定的存储空间，如 `subagents/<agent_id>` 内侧链，不混入主链）。
   > **AgentGo 对应**：每个 Agent 在 `processTask` 中构建独立的 `history []HistoryEntry`。重试时从 `task.LastHistory`（JSON）反序列化恢复历史上下文。各 Agent 历史天然隔离（不同 goroutine 的局部变量）。
4. ✅ **主轮询运转 (Execution)**：进入大模型驱动反馈循环 (`query.ts` 内的 `queryLoop`)。不断地产生思考、执行工具、读取反馈。
   > **AgentGo 对应**：`Agent.processTask` 中的 ReAct 循环，最多 `MaxLoops` 次迭代。每轮调用 `Execute(ctx, task, depResults, history)` → LLM 返回 tool_calls → 并行执行工具 → 追加历史 → 压缩检查 → 循环。
5. 🔶 **休眠与重唤醒 (Suspend/Resume)**：长时任务遭遇等待时可休眠，被依赖任务解锁时通过 `SendMessageTool` 被重新激活恢复工作。
   > **AgentGo 部分实现**：Agent 通过 500ms 轮询 `QueryAvailable` 检测可领取任务，依赖未满足时跳过。`MailNotifier` 可为有未读邮件的空闲代理发布唤醒任务，间接实现了基于消息的重激活。但无显式 suspend/resume 机制（当前 MVP 规模下轮询开销可忽略，详见 `docs/nextUpgrade_v2.md §3.5`）。
6. ✅ **终止与总结汇报 (Termination & Summary)**：Agent 达到目标或到达最大轮次 (`maxTurns`)。它触发总结组件 `agentSummary.ts`，将所有零散日志高度概括为一份摘要，推给它的”上级”。
   > **AgentGo 对应**：当 LLM 返回 `ToolCalled=false` 时，Agent 调用 `SubmitResult` 将输出写入 Task.Results。Scheduler 通过 `boardSnapshot` 的 `results` 字段读取结果，决定下一步动作。循环耗尽时保存历史并 RetryRollback 或 FailTask。
7. 🔶 **遗迹清理 (Cleanup)**：合并带有改动的 Worktree 分支（或丢弃脏状态），卸载专属 MCP Client，销毁本地相关数据。
   > **AgentGo 现状**：Worktree 相关清理路径（commit+merge / `WorktreeManager.CleanupAll`）已随 git 依赖一并删除，见本节第 2 条。当前的清理机制仅剩：`Agent.Run` 退出时 defer 调用 `Roster.ReleaseAll(agentID)` 释放所有文件锁；`processTask` 开始时调用 `FileCache.Clear()` 清除文件缓存；任务完成/失败/取消进入 FIFO 淘汰队列时 `ToolCallRecord` 一并淘汰。

---

## 4. 主事件循环与代理通信机制

### 主事件循环 (`QueryLoop`)
系统的脉搏是 `query.ts` 中的 `queryLoop`。它是一个状态机与流式异步发生器 (AsyncGenerator)。
在每一次循环中：
- 接收现存 `messages` 并检测是否因为过长需要自动浓缩 (`autoCompact`) 或切片剔除 (`snipCompact`)。
- 请求 LLM 获得流式响应被组装。
- 探测并剥离工具调用请求，交由执行器 (`StreamingToolExecutor`) 进行异步执行，结果重新打包成 `tool_result` 放回循环流。如此反复，直至触发最终回复 (`Terminal`) 退出循环。

> ✅ **AgentGo 对应**：`Agent.processTask` 实现了等价的 ReAct 循环。每轮：
> 1. 调用 `Execute(ctx, task, depResults, history)` → `buildMessages` 构建完整消息链 → LLM 调用
> 2. 如果 `ToolCalled=false`（终态回复）→ `SubmitResult` 退出循环
> 3. 如果 `ToolCalled=true` → 并行执行所有 tool_calls → 追加 HistoryEntry → Layer 1 snip → Layer 2 compress 检查 → 继续循环
> 4. 错误处理：`ErrRecoverable` + context overflow → Layer 3 aggressive compress → RetryRollback

### 子代理通讯与分工交换 (Inter-Agent Communication)
- ✅ **发信唤醒 (`SendMessageTool`)**：如果 Agent A 在等 Agent B 计算核心数据，A 不用一直挂起，而是可被置于等待；当外部数据可用时，Coordinator 或别的 Agent 使用 `SendMessageTool({ to: subagent_name, message: ... })` 直接将情报灌入目标上下文。
  > **AgentGo 已实现**：Worker/Explorer/Scheduler 均注册 `send_message` 工具（参数: `to`, `content`）。消息通过 `mailbox.Registry.Send()` 投递到目标信箱，Agent 在 ReAct 循环每轮开头调用 `Mailbox.Drain()` 读取并以 `<agent-mail>` XML 标签注入 LLM 上下文（作为 `{Role: "user"}` 消息）。`MailNotifier` 为有未读邮件的空闲代理发布唤醒任务。
- 🔶 **任务更新挂钩 (`TaskUpdateTool`)**：对于面向用户的长时任务，子代理可以使用任务更新工具来向 UI 回传其目前最新的逻辑卡点、进度和心跳状态。
  > **AgentGo 对应**：`AppendOutput(agentID, taskID, chunk)` 写入 `Task.PartialOutput`，通过 CLI `/status` 或 Scheduler `boardSnapshot` 可查看。非工具调用，而是 Agent 框架自动在每轮 ToolCalled 后追加。
- ✅ **摘要返还协议**：协作的最基本面在结束点交换 —— 并行出去的测试代理在收集并验证一堆 Bug 之后，只把结果提炼成一个 `<summary>` 丢给主环境。
  > **AgentGo 对应**：`SubmitResult` 将最终输出写入 `Task.Results[agentID]`。Scheduler 通过 `boardSnapshot.tasks[].results` 读取所有子任务结果，由 LLM 综合判断后调用 `report_done` 向用户汇报。

---

## 5. 探寻 KimiSoul 的概念本质与状态持久化机制

结合上下文与对同类系统演化的回溯，**”KimiSoul” 是对本系统中核心执行内核与认知轮询模块的高阶抽象代名词**，在现今项目的代码物理实体上对应：`QueryEngine.ts` 联合 `query.ts` 的认知执行体系。

- ✅ **它是系统的心智枢纽 (The Core Engine)**：它承载大模型思考（Thinking Config）的控制流。每一个 Agent 的心智运作不是孤立存在的，它们都依赖运行这套 `QueryEngine` 的实例化副本来产生自己的意识流。
  > **AgentGo 对应**：`Agent` 结构体 + `TaskExecutor` 闭包 + `LLMExecutor`。每个 Worker/Explorer 实例化各自的 Agent，持有独立的 LLM 客户端、ToolRegistry、FileStateCache。`processTask` 中的 ReAct 循环即为”心智运转”。
- ✅ **无限工作循环 (Agentic Loop)**：之所以被称为 “Soul”（灵魂），它不是机械的”一问一答”，它是具备流式拦截与自纠错韧性 (Self-Healing Loop) 的架构。使得原本没有状态的大模型变成了能够面对混沌的终端环境，持续跟踪问题、出错了自动重试直至完成的智能生命体。
  > **AgentGo 对应**：`Agent.processTask` 循环 + `handleFailure` 错误区分 + `RetryRollback` 自动重试 + `LastHistory` 跨重试上下文恢复。Agent 可在失败后回滚到 pending 状态，保留完整历史上下文重新执行。

由于 LLM API 本质是无状态的单次调用，`KimiSoul` 为保障大模型能够记住”十分钟前做过的事”而不会内存爆栈，在 `QueryEngine.ts` 的底层设计中引入了两项至关重要的记忆维系机制：

### ✅ 内存截断与自动浓缩策略 (`autoCompact` & `snipCompact`)
一条普通的测试或环境编译命令极易产生几万 Token 的过程日志，如果全部灌入上下文，LLM API 资费将瞬间被拉爆，并严重稀释模型的注意力。
在每次引擎抛出下一轮 `yield` 之前，系统会时刻审视承载大模型底层对话记忆池的 `mutableMessages` 数组。当整体字数逼近警戒水位时，它会唤醒类似于大脑海马体的记忆过滤组件（对应代码中的 `snipCompact` 与 `autoCompact` 模块）。
过滤组件会在底层大刀阔斧地将过程期的冗长系统回显干掉，只利用 `compact_boundary` 等在原地留下一句诸如 `<snipped>` 的短文本锚点。该机制能够用极小代价强制抑制记忆灾难，确保代理体系即便连翻处理问题数十回合，也依旧能够清醒记得主线目标。

> **AgentGo 已实现 3 层压缩策略：**

#### ✅ 第一层压缩策略（Layer 1: snipOldToolResults）
~~滑动窗口压缩，至少MIN轮次的对话，最多MAX token数量的文字。~~
**AgentGo 实现**：每轮 tool_call 后自动执行。遍历 `history`，对 `snipTargetTools`（run_shell, read_file, grep_search, glob_search）中超出 `keepRecent` 范围的旧条目，将 `ToolResult.Content` 替换为 `”[已清空，内容过长]”`，保留 `ToolCallID` 以维持 tool_call/tool_result 配对。无 LLM 调用开销。

#### ✅ 第二层压缩策略（Layer 2: compressHistory）
~~轻量级LLM集中压缩，当历史记录已经超过了设定的阈值，比如说180,000 token的时候，就会触发第二层压缩策略。~~
**AgentGo 实现**：当 `totalPromptTokens > CompactTokenThreshold`（默认 80000）时触发（每任务仅一次）。调用 `buildHistorySummary` 将旧历史生成文本摘要（”=== 历史摘要 === 步骤N: [toolName] content...”，AssistantContent 截断至200字符），然后 `compressHistory` 返回 `[摘要条目] + 最近 keepRecent 条历史`。注意：当前实现不调用 LLM 生成摘要，而是使用规则化文本拼接。

#### ✅ 第三层压缩策略（Layer 3: 上下文溢出兜底）
**AgentGo 实现**：`handleFailure` 中，当错误为 `ErrRecoverable` 且 `isContextOverflow(err)` 返回 true（检测 “length”/”截断”/”context” 关键词）时，以 `keepRecent=1` 激进执行 Layer 1 + Layer 2 压缩，保存到 `task.LastHistory`，然后 `RetryRollback`。Agent 在下次领取该任务时，从压缩后的历史恢复上下文继续执行。

### ✅ 文件态感知闭环 (`FileStateCache`)
人类在修补代码时极少在脑中反复回放同一个文件的所有字母，但困于上下文沙盒里的模型一旦缺乏标记很容易疯狂发起重复”阅读”请求耗费性能。
基于此，`QueryEngine` 在外部包裹了基于当前引擎存活周期的**文件态感知系统 (`this.readFileState`)**。
大模型除了拥有一个文字形式的事件履历表（`messages`），还挂靠在了一个独立的读写映射池上。代理不用再浪费力气靠猜测文件内容，只要它之前看过的东西就会被缓存挂载。甚至当物理界面的文件遭到其他 Swarm 队友的篡改时，只要触发隔离校验，它依旧知晓那份基于当前时空的精确工作快照，完美规避了错乱的文件记忆幻觉。

> **AgentGo 实现**：`internal/agent/filecache.go` — `FileStateCache` 结构体：
> - **LRU 缓存**：默认容量 50，存储 `{content, SHA256 hash}`，按访问顺序淘汰最久未用的条目
> - **Per-agent 隔离**：每个 Worker/Explorer 拥有独立 FileStateCache 实例，不共享
> - **任务边界清除**：`processTask` 开始时调用 `FileCache.Clear()`，防止跨任务脏读
> - **写操作失效**：`write_file` 和 `edit_file` 执行后调用 `cache.Invalidate(path)`，确保下次读取获得最新内容
> - **hash 一致性**：`read_file` 返回 `content + “\ncontent_hash: “ + SHA256`，供 `write_file`/`edit_file` 的 `expected_hash` 参数做乐观并发控制

---

## 6. 上下文隔离与其必要性

### 什么是上下文隔离？
是指不同代理在运转时，从硬盘读写视野到上下文记录（历史消息、Prompt Cache），都与主干及其它代理”隐身级”剥离。
实现手段：
- 🔶 **记忆与历史侧链 (`Sidechain Transcript`)**：在 `.gemini/.../subagents/<id>` 写独立日志，而不是都保存在主会话。
  > **AgentGo 对应**：每个 Agent goroutine 维护各自的 `history []HistoryEntry` 局部变量，天然内存隔离。重试时通过 `task.LastHistory`（JSON 序列化到 Task 结构体）跨生命周期持久化。无磁盘侧链日志。
- ✅ **独立的 FileStateCache**：主系统与子系统各自拥有已读文件缓冲区副本。
  > **AgentGo 对应**：每个 Worker 和 Explorer 在构造时创建独立的 `FileStateCache(50)` 实例，互不共享。
- ❌ **物理系统隔离 (`Git Worktree` / `Remote Task`)**：不同工作目录和远端运行进程。
  > **AgentGo 现状（2026-04-09 架构决策：无 git 依赖）**：`internal/isolation` 包已整体删除，Worker/Explorer 一律共享 `ProjectRoot`，不再存在 Per-Task Git Worktree。Remote Task 沙箱未实现。详见 `Archtechture.md` §关键实现事实。

### 为什么要做上下文隔离？
1. ✅ **防止系统性幻觉与污染**：如果研发代理和排障代理看同一个上下文，海量的 `git log` 和失败报错栈会严重污染主进程记忆，导致模型注意力机制越距，丢失当前宏观目的。
   > **AgentGo 实现**：Agent 历史天然隔离（goroutine 局部变量）；3 层压缩策略防止单个 Agent 内部的上下文膨胀；Scheduler 只通过 `boardSnapshot` 读取结构化的任务状态，不接触 Agent 的原始历史。
2. ✅ **极大的成本优化 (Token Economy)**：使用分支会话可避免将动辄过十万字的情报强行塞进下一回合，能通过摘要形式降低海量 Token 的支出并提升响应潜伏期。
   > **AgentGo 实现**：Layer 1 无 LLM 调用开销清空旧工具输出；Layer 2 生成摘要后丢弃旧历史；`SubmitResult` 只传递最终输出而非完整执行过程。
3. 🔶 **保持 Prompt Cache 的高命中率**：上下文如果被旁路工作严重污染，树结构的公共头部将无法被哈希命中，使得大模型 API 出现性能降级。
   > **AgentGo 现状**：每次 LLM 调用通过 `buildMessages` 完整重建消息列表，系统提示词固定在头部（有利于 cache hit）。但未显式优化 Prompt Cache 命中率。

---

## 7. 安全机制的工作内容与原理

将执行权交给多代理意味着更高的安全风险，该项目通过多层次安全防御遏制”越权与破坏”：

1. ❌ **分级权限模型 (PermissionMode Schema)**：
   代理启动时继承或被覆盖自身 `permissionMode` 状态（如 `acceptEdits`, `plan` 等）。主系统可能完全信任自动操作，但对未知风险子代理（比如互联网搜索抓取代理），会将其置于受控模式中，任何非预期外的文件写入必须向上抛出申请拦截（`awaitAutomatedChecksBeforeDialog`）。
   > **AgentGo 现状**：无权限分级模型。所有 Worker 拥有相同的完整工具集（10个），所有 Explorer 拥有相同的只读工具集（4个）。无运行时权限提升/降级机制。
2. ✅ **细粒度的工具池裁剪 (Worker Tool Extraction)**：
   在 `runAgent.ts` 构建子系统时：`workerTools = assembleToolPool(...)`。引擎会依据被拉起代理自身的性质（由 `tools` 及 `disallowedTools` 前端约定），剔除不安全的高危命令能力（如直接允许高权限终端读写或删除根文件）。代理只掌握自身完成目的所需的”极小特权”。
   > **AgentGo 对应**：Explorer 通过 `registerReadOnlyTools` 仅注册 4 个只读工具（read_file, list_files, grep_search, glob_search），无法执行 write_file、edit_file、run_shell、publish_subtask、web_search、web_fetch。Worker 通过 `registerWorkerTools` 注册完整 10 个工具。工具注册在构造时完成，运行时只读。
3. ❌ **管理员信赖标记 (`SourceAdminTrusted`)**：
   在注入 MCP 或执行前端命令中验证 Agent `source`，如用户自写恶意代理若没有 `isSourceAdminTrusted` 为 true 的断言，不仅无法调用钩子甚至拿不到 MCP 危险服务的通讯句柄。
4. 🔶 **破坏性隔离 (Sandboxing)**：
   借助于 `isolation: “worktree”` 与 `Remote Task`，即使代理”疯狂乱作”，它的脏环境也不会立刻进入主开发主干，保障工程的核心逻辑随时可以被人类安全评估后终止并扬弃。
   > **AgentGo 现状**：Git Worktree 隔离已于 2026-04-09 随 git 依赖整体删除（见 §3 第 2 条）。当前仍保留的"破坏性隔离"手段只有 Shell 命令拦截（`internal/shell/intercept.go`）：黑名单硬拒绝 + 灰名单 CLI 审批。无容器/chroot 级硬沙箱，Remote Task 未实现。其他安全机制：
   > - ✅ **路径安全**：`pathutil.ValidatePath` 限制所有文件操作在 `ProjectRoot` 内，阻止 `../` 遍历和敏感文件访问
   > - ✅ **SSRF 防护**：`webtool.validateURL` + `isPrivateOrLoopback` 阻止内网/环回/链路本地地址访问
   > - ✅ **文件写冲突防护**：Roster 文件锁 + `expected_hash` 乐观并发控制
   > - ✅ **Shell 命令拦截**：黑名单（rm -rf /、mkfs、shutdown 等）硬拒绝 + 灰名单（git push、chmod、curl|sh 等）CLI 审批
   > - ✅ **Shell 超时**：`run_shell` 有可配置超时（默认30s），防止恶意或死循环命令
   > - ✅ **子任务深度限制**：`MaxSubtaskDepth` 防止无限嵌套任务创建

---

## AgentGo 实现进度总览

| 能力维度 | 状态 | 说明 |
|---------|------|------|
| 树状任务分解 | ✅ | Scheduler → Worker/Explorer，publish_subtask 支持子任务 |
| 多代理并行执行 | ✅ | WorkerCount 可配置，goroutine 并行 |
| 任务依赖管理 | ✅ | Dependencies + ClaimTask 校验 + GetDependencyResults |
| ReAct 工具调用循环 | ✅ | Agent.processTask + 并行 tool_call 执行 |
| 3 层历史压缩 | ✅ | snip → compress → aggressive overflow recovery |
| 文件缓存 (FileStateCache) | ✅ | Per-agent LRU，任务边界清除，写操作失效 |
| 乐观并发控制 | ✅ | expected_hash + Roster 文件锁 |
| 路径安全 + SSRF 防护 | ✅ | pathutil + webtool URL 校验 |
| 工具池裁剪 | ✅ | Explorer 只读4工具 vs Worker 完整10工具 |
| 资源感知调度 | ✅ | boardSnapshot 含 WorkerCount/BusyWorkers |
| 错误区分 + 自动重试 | ✅ | Recoverable/Unrecoverable + RetryRollback |
| 任务级 SystemPrompt | ✅ | task.SystemPrompt 覆盖默认 prompt |
| 依赖感知驱逐 | ✅ | evictSafe + isDependedUpon |
| 代理间直接通信 | ✅ | mailbox.Registry + send_message 工具 + MailNotifier 唤醒 |
| 分身机制 (Fork) | 🔶 | 远景。publish_subtask 覆盖大部分场景，完整上下文克隆技术可行但待实际需求驱动 |
| 物理隔离 (Worktree) | ❌ | 2026-04-09 架构决策：无 git 依赖，`internal/isolation` 已整体删除；所有 Worker 共享 ProjectRoot |
| Shell 命令拦截 | ✅ | 黑名单硬拒绝 + 灰名单 CLI 审批（internal/shell/intercept.go） |
| 跨代理自组织 | 🔶 | TeamSnapshot 团队感知 + send_message 主动通信 + prompt 协作引导。完全去中心化调度未实现 |
| 权限分级模型 | ❌ | 无运行时权限控制（已移至 nextUpgrade_v2.md §3.7） |
| 高阶通信协议 | 🔶 | Message.Type 区分 info/question/reply/steer/ack + XML 子标签 + prompt 引导。专用协议子类型待实现 |
| 用户中途干预 (Steer) | ✅ | /steer CLI 命令 + mailbox From:"user" 投递 |
