# 现状速览（2026-07-28）

> 本文档原本是设计稿，部分章节早于实现。本节为升级工作提供快速对齐入口，列出**当前实现事实**与**与原设计文档的关键差异**。后续章节如有冲突，以本节和源代码为准。

**已实现的主要核心包**（`internal/`）：

| 包 | 一句话职责 |
|---|---|
| `agent` | ReAct 循环 + Context v3 Raw History/replay 投影 + FileStateCache + Memory 注入入口 |
| `bootstrap` | 系统装配、启动顺序、kind×replica runner 实例化（含 `runtime_builder.go`） |
| `config` | YAML/JSON 配置加载，**v4 唯一格式**：`llm:` / `scheduler:` / `agents:` / `infra:` / `tool_profiles:` / `reactors_file:` 等顶层块 |
| `gate` | **统一 Gate 注册表**（v5 替代 v4 三套 HookRegistry）。Phase 路由：`tool:preCall/postCall` / `mailbox:beforeSend/Deliver/Wake`，10 个内置 Gate |
| `hook` | 旧 Hook 接口仍作为 LLMExecutor 与 Gate 之间的适配层保留；Agent Hook 子系统已空（team-awareness 删除）；Tool/Mailbox builtin 文件夹保留为兼容 surface |
| `interaction` | 通用结构化人机交互：稳定 Option ID、Version CAS、`pending → resolving → resolved` 两阶段协议，以及 Graph approval/Shell 受信任 effect 与普通 `agent_question` 路由 |
| `llm` | LLM 客户端（统一 OpenAI-compatible Chat Completions，V6 移除 Provider 适配层）+ `reasoning_effort` + SSE 聚合 + `Message.ExtraFields` 透传机制 |
| `mailbox` | 异步信箱、Notifier、recent ring-buffer（容量 16）、TeamSnapshot |
| `memory` | **Memory System**（v5）。`Store` 接口 + `ProcessStore` 内存实现（`ScopeProcess`），`ScopeSession` / `ScopeProject` v5.x 预留。替代 v4 team-awareness Hook |
| `model` | `Task` / `Event` / `Claim` 数据结构。`Task` 含 `Artifacts` / `ExpectedArtifacts` / `LastResponse` / `MailChainDepth` / `SchedulerBatch` / `ReadSet` |
| `modes` | exec / topo 两轴模式存储（gate 轴已于 V6 移除；执行前审阅改由 Graph approval 节点承担） |
| `pathutil` | 路径越界 + 敏感文件模式拦截 |
| `probe` | 启动期 TCP + 真实 function-call capability probe + 工具可用性探针 |
| `reactor` | **Reactor 注册表**（v5，新增）。订阅 `trace.Event` 的 `Kind`，4 个内置 reactor + `userdef/` 用户 YAML 加载器 |
| `roster` | 文件级 `TryClaim/Release/ReleaseAll/IsOccupied/ListByAgent` |
| `runner` | **统一执行代理外壳**（v5，**取代 v4 `internal/worker` + `internal/explorer`，两包已删**）。`runner.New(rt, deps)` 按 `AgentRuntimeConfig` 实例化 |
| `scheduler` | Scheduler 是 `agent.Agent` 一等代理（Phase 3 重构遗留），`scheduler.New` 返回 `Bundle{Agent, Activator, Modes}` |
| `session` | Session 管理、history.jsonl、公开 LLM 轮次 turns.jsonl、snapshot/replay/archive |
| `shell` | `CommandFilter`（黑/白/运行时白名单）+ 与原始调用精确绑定的 `shell_command` authorization Interaction |
| `spawn` | **Spawn Manager**（v5，新增）。实现 `reactor.Reactor` 接口，订阅任务终态事件销毁 ad-hoc runner（`one_shot` 生命周期）；`KindOf` 支持 per-kind reactor 路由；`ReactorSpawnMaxDepth=5` 防级联 |
| `store` | `MemoryTaskStore` + `TaskCancelRegistry` + `ToolCallRecord` + `StoreHookView` + `ArtifactLog`（带 replay）+ ReadSet upsert + scheduler-batch 辅助 |
| `suggest` | **Did-You-Mean**（v5，新增）。基于 `github.com/sahilm/fuzzy`，被 `tools/local_read.go` 空结果路径与工具未找到诊断使用 |
| `tools` | 6 个 ToolGroup：LocalRead / LocalWrite / Web / Shell / Meta / Scheduler；`AllToolNames` 是规范名称表 |
| `trace` | 每任务 JSONL，**Schema B**（嵌套子结构体 `Transition`/`ShellExec`/`ShellTimeout`），`SetDefaultDispatcher(reactorReg)` 使 `trace.Emit` 同时驱动 Reactor |
| `tui` | **Bubble Tea TUI**（v5，取代已删除的 `internal/cli`）。Agent 详情按 Loop 浏览全部公开轮次并支持 Home/End 最早/最新；Interaction 不注册裸字母或裸数字动作键；命令目录来自 `ui.CommandCatalog()` |
| `watchdog` | 周期巡检、级联取消、roster 兜底清理、超时崩溃汇报 |
| `webtool` | Web 检索/抓取 + SSRF 防护 |

> **v5 已删除的包**（不要去找它们）：`internal/cli`、`internal/worker`、`internal/explorer`。职责被 `tui`、`runner`、`tool_profiles` 共同承接。

**关键实现事实**（按"原设计 → v5 实现"对齐）：

- **执行代理 = `runner.Runner`**（不再是 worker / explorer）：原设计的"执行代理"和"调查代理"在 v5 统一为 `internal/runner` 包。所有差异通过 `setting.yaml` 的 `agents[*]` 块声明（kind / replicas / profile / event_type / system_prompt_file / model 等）。Bootstrap 调用 `runtime_builder.buildAgentRuntime(kind, replicaIdx)` 合成 `AgentRuntimeConfig`，然后 `runner.New(rt, deps)`。**没有运行时 Kind 枚举分支** —— Kind 仅是配置字段。
- **Scheduler = `agent.Agent` 一等代理实例**（2026-04-10 Phase 3 重构后保持至 v5）：`agent.NewAgent(EventType="__scheduler__")` 的实例，工具集 = Worker 全集 + SchedulerGroup（cancel_task + get_task_result + report_done + probe_directory）+ MetaGroup（publish_task / send_message，scheduler 上下文里 publish_task 通过 `BatchTracker` 追加到 `task.SchedulerBatch`）。直接 `read_file`/`grep_search`/`web_search`，自动获得 Gate、3 层历史压缩、FileStateCache、Trace、per-task cancel ctx。`scheduler.New` 返回 `*Bundle{Agent, Activator, Modes}`。详见 §"Scheduler 一等代理重构"。
- **Roster 仅做文件级锁**：`TryClaim/Release/ReleaseAll/IsOccupied/ListByAgent` 全部围绕"防文件并发写"。团队成员感知改由 mailbox `TeamSnapshot` 与 Memory System 的 `KindContext / file_awareness` 项共同承担。
- **Mailbox 子系统**：`internal/mailbox` 提供基于 Go channel 的异步信箱、`send_message` 工具、ack 自动回执、recent 16 条 ring buffer（供 Gate peek-without-consume）、`TeamSnapshot` 团队感知。
- **ReactiveSystem（v5 重构，替代 v4 三套 Hook 系统）**：详见 §"ReactiveSystem：Gate + Reactor + Memory"。要点：
  - **Gate**（事前决策门，可 Abort）：单一 `gate.Registry`，按 `Phase` 路由 Tool / Mailbox 子域。10 个内置 Gate（6 Tool + 4 Mailbox）。
  - **Reactor**（事后状态响应，不可 Abort）：`reactor.Registry` 订阅 `trace.Event` 的 `Kind`。4 个内置 reactor（`record-artifact` / `task-end-callback` / `trace-history-event` / `read-set-write`）+ `spawn.Manager` 自身注册为 reactor。**用户可在 `cfg.ReactorsFile` (默认空) 中通过 YAML 声明 reactor**，支持动作 verb：`publish_task` / `invoke_llm` / `spawn_agent` / `call: send_message`，以及 `when:` / `kind:` / `via_translator:`。
  - **Memory**（取代 team-awareness）：`internal/memory` 的 `ProcessStore` 在 `Agent.processTask` 入口被读取（`team_snapshot` / `file_awareness`），由 scheduler / runners / Roster 监听器写入。`GoalAnchor` 直接删除（`task.Description` 已承载目标）。
  - **Spawn**：`spawn.Manager` 让 reactor 可以"创建 ad-hoc agent"。从 `base_kind` 模板 + `RuntimeOverride` 派生新 runtime，发布 initial task（`EventType="adhoc:<spawnID>"`），任务终态时 manager 作为 reactor 自动销毁 runner（one_shot）。`ReactorSpawnMaxDepth=5` 防级联。
- **MailNotifier 默认启用**：邮件级联爆炸 P0 的 4 项根因全部由 Phase 2（v4） + Mailbox Gate（v5）守住。`chain-depth-limit` (max=`MailChainMaxDepth`，默认 3) 在 BeforeSend 截断；`per-agent-dedup` 在 BeforeDeliver 去重；`wake-worthy-filter` 在 BeforeWake 过滤；`wake-context-expand` 在 BeforeWake 累加 wake task description。
- **通用 Interaction 前端**：TUI 与 Web 都从 UI Hub 接收当前进程内完整 pending Interaction 列表，并以 `request_id + expected_version + option_id + text` 回答；`SessionID` 仅标记创建审计归属，任务跨 `/session` 继续时不会被过滤。TUI 用 Tab 切换 Interaction 焦点、`↑/↓` 选择、`PgUp/PgDn` 翻长问题、Enter 提交；`RequiresText` 转入普通文本输入。Esc 在 Interaction 焦点只返回输入框。旧 `1/2/3/4` 单键方案仅存在于 `docs/archived/interface-design-tui-2026-05.md`，已经废弃。
- **Agent 公开轮次历史**：Scheduler 与所有 Runner 共用 `output.KindStream` / `output.KindTurn` 协议。同轮流式文本以稳定 ID 原位更新；每次 LLM 调用返回后冻结唯一完成事实并追加到 Session 的 `turns.jsonl`。UI Hub 的 `Snapshot.Turns` 不受 200 条实时 feed 上限影响，TUI/Web 重连或切换 Session 后可恢复全部 Loop；账本只保存公开正文、工具名、状态和错误。
- **Graph approval、Shell 与 Agent question 共用协议**：GraphStore 是 Graph 执行事实源；`graph_approval` 的决议经 `SetOnResolved` 终态回调驱动 `Runtime.OnApprovalDecided` 写回节点。Shell 灰名单请求绑定 command/pattern/working directory/Agent/Task；`allow_session` 只把服务端捕获的 pattern 加入当前进程、本次运行的 whitelist，切换 `/session` 不清空，退出后不持久化。MetaGroup 的 `request_user_input` 仅创建 `Purpose=agent_question`，返回稳定 `option_id`/`text`，不能携带 ActionRef 或触发前两类特权 effect。详见 `docs/design/interaction.md`。
- **架构决策：无 git 依赖**（2026-04-09 起保持）：`internal/isolation`（git worktree 隔离）整体删除——git 锁模型为单用户串行设计、整树 checkout 摧毁 mtime 观测层，隔离要的是命名空间而非版本控制。所有 runner 共享 `ProjectRoot`。并发写文件防线 = `Roster` 文件锁 + `expected_hash` TOCTOU 检查 + `pathutil.ValidatePath` + `path-boundary` Gate（双重）+ `require-read-before-write` Gate + `enforce-expected-artifacts` Gate + `validate-line-anchors` Gate。2026-07-26 起新增**按任务写时复制隔离**（`internal/workspace`，仍无 git）：DAG 节点声明 `isolation:"workspace"` 后，认领 Runner 在 overlay 中执行（读穿透主根、写落 `.agentgo/workspaces/<taskID>/`），成功终态控制面经 Roster 锁逐文件合并回主根（fast-forward / 行级三路自动合并），冲突 → 任务 failed + 自动高优 replan 交 Scheduler 裁决。详见 `docs/design/workspace-isolation.md`。
- **任务数据流**：`Task.Artifacts`（`record-artifact` reactor 在 `KindFileWritten` 上自动追加，路径相对项目根）、`Task.ExpectedArtifacts`（发布者硬合约，由 `enforce-expected-artifacts` Gate 与 `agent.checkExpectedArtifacts` 双重把守）、`Task.LastResponse`（无条件持久化用于失败诊断）、`Task.MailChainDepth`（邮件链跳数）、`Task.SchedulerBatch`（scheduler 当前 reactLoop 跟踪的子任务 ID 列表）、`Task.ReadSet`（v5 Phase 6 新增，由 `read-set-write` reactor 在 `KindToolResult{tool=read_file}` 上写入；`require-read-before-write` Gate 改读 ReadSet 而不再反查 ToolCallHistory）。
- **TaskCancelRegistry**：per-task cancel context，看门狗/调度器把任务转为 terminal 状态时自动取消正在执行的代理（通过 `ctx.Done()` 即时感知）。
- **崩溃汇报**：任务最终失败时 agent 自动调用 `sendCrashReport`，向 `task.EventSource` 发送 `priority=high` 邮件，附 expected vs actual artifacts、最后一次 LLM 响应原文。
- **Context v3 replay 投影**：Raw History 不可变；L2 按当前 Snapshot section 压力生成有界 replay，Optional reasoning 可 dropped，RequiredExact 在工具 dispatch 前完成 representability gate；context overflow 只请求 aggressive projection + 新 Attempt。Shell/Web 大结果先写 Task-scope ContentStore，再以预览 + ContentRef 回放。
- **Trace 系统 Schema B**：`internal/trace.Event` 是 fat struct，包含 `Transition` / `ShellExec` / `ShellTimeout` / `Lease` / `Suggestion` / `Effect` / `Acceptance` 可选指针载荷与 V6 Graph Runtime 顶层字段，旧字段保留不动。EventKind 覆盖任务/Agent 状态机、Graph 生命周期、验收核验、执行租约与副作用账目等审计事件。`SetDefaultDispatcher(reactorReg)` 让 `trace.Emit` 同时驱动 Reactor 链路。物理 JSONL 在 Session 活跃时落盘到 `.agentgo/sessions/sess-<id>/logs/`，否则写入 `.agentgo/traces/`，默认保留 100 个文件；Task 重试可能跨多个分片。`agentgo trace list/show/graph/node/stats` 会按完整 `task_id` 重组逻辑任务并自动定向到 active session 的 `logs/`，其中 `graph <graph_id>` 聚合单个 Graph 的生命周期时间线。可通过 `AGENTGO_DUMP_PROMPTS=1` 启用 prompt dump。
- **LLM 请求策略与流式调用**：`internal/llm` 在统一工厂层应用 `reasoning_effort` 与 `stream`，因此 Scheduler、静态/动态 Agent、spawn Agent 和 Reactor LLM 共享同一请求策略（V6 起统一 OpenAI-compatible Chat Completions，无 provider 分支）。SSE 路径先聚合完整文本、工具调用参数、usage 与未知扩展字段，再返回普通 `Response`；UI 收到带稳定 `stream_id` 的累积快照。未知扩展字段经 ExtraFields 透传往返（如 `reasoning_content`）。详见 §"LLM 请求路径与 ExtraFields 透传"。

**未启动 / 待设计**：
- ScopeSession / ScopeProject 持久化记忆（v5.x 排期）
- 引用幻觉验证 Gate / E2E 幻觉测试基线（详见 `docs/archived/hallucination-acceptance-audit-2026-05.md`，P0 改进尚未落地）
- Shell 工具改造的 T6（YAML 持久化）（详见 `docs/activate/ToolUpgradePlan.md`）
- AgentHook 子系统是否在 v5 内合并入 Reactor（方案 C，详见 `docs/activate/MemoryManageSystem.md` §5）

当前限制见 `docs/activate/KNOWN_ISSUES.md`；已完成的 UI Hub 修复总账见 `docs/archived/ui-hub-remediation-2026-07-18.md`。

---

# ReactiveSystem：Gate + Reactor + Memory（v5 重构，2026-05-01 落地）

v4 时代的三套 Hook 系统（`ToolHookRegistry` / `MailboxHookRegistry` / `AgentHookRegistry`）在 v5 经过命名空间清理后，重新归类为两类**核心角色** + 一个独立配套子系统：

- **Gate**（事前决策门，可否决动作）—— 统一到单一 `gate.Registry`，按 `Phase` 路由 Tool / Mailbox 子域。
- **Reactor**（事后状态响应，不可否决，**用户可配**）—— 全新引入的 `reactor.Registry` 子系统，订阅 `trace.Event` 的 `Kind`。
- **Memory**（上下文供给）—— 取代 v4 的 `team-awareness-*` Hook。详见 §"Memory System" 章节。

设计源头与详细决议：`docs/activate/ReactiveSystem.md` + `docs/activate/MemoryManageSystem.md`。

## 核心原则（不可妥协）

1. **状态转换的驱动权归 AgentGo 内核**：用户 reactor 永远不允许直接调用 `agent.SetState(...)` 或 `store.TransitionState(...)`，YAML 动作语言层面就排除这种能力。Reactor 想让 agent 进入下一状态必须通过 `publish_task` / `send_message` / 调工具，让主流程在合适时机自然驱动。
2. **Reactor 分两类，失败语义不同**：内置 reactor（开发者 Go 注册）允许声明 `IsSync: true` 同步执行，失败需被 trace 显眼记录；用户 reactor（YAML 声明）**永远异步**，panic-isolated，失败仅记日志。
3. **trace 是事实标准**：所有状态变更必须遵循三步序列：**主流程 SetState → emit `KindAgentStateChanged`（或对应 EventKind）→ Reactor 订阅者响应**。trace 事件不仅是观察手段，更是 Reactor 系统的**唯一事件源**。
4. **Reactor 不允许直接驱动新状态转换**（承接原则 1 的延伸，包括内置 reactor）—— 避免 Reactor → Reactor 级联导致事件循环复杂度爆炸；保留单一驱动入口便于 Replay 与调试。
5. **Reactor 自带的独立 LLM 调用必须是上下文隔离的纯文本生成器**：`invoke_llm` / `via_translator` 路径**无工具、无 history 累积、无运行时上下文**，纯文本输入输出。这阻断了"无监督的影子 agent"攻击面。

## Gate 子系统

### 核心组件

| 包 / 文件 | 职责 |
|---|---|
| `internal/gate/gate.go` | `Phase` 枚举 + `Context` 接口 + `Decision` 返回值 + `Gate` 接口 |
| `internal/gate/registry.go` | `Registry`：`map[Phase][]Gate`，按 Phase 分桶，同 Phase 内 `Priority` 升序；nil 安全 + panic recover |
| `internal/gate/tool_context.go` | `ToolContext` 具体实现（携带 ToolName / Args / TaskID / AgentID / FilePath / 等域专属字段） |
| `internal/gate/mailbox_context.go` | `MailboxContext` 具体实现（携带 Message / Recipient / 等） |
| `internal/gate/adapter.go` | `AsMailboxRunner(*Registry) mailbox.MailboxHookRunner` 适配器，把 gate 包的 registry 包成 mailbox 包内部定义的最小接口（保留 v4 的解耦边界） |
| `internal/hook/builtin/*.go` | 10 个 Gate 实现（v4 命名沿用，注册到新 `gate.Registry`） |

### Phase 枚举与内置 Gate

| Phase | 触发点 | 内置 Gate（按 Priority 升序） |
|---|---|---|
| `tool:preCall` | `agent.NewLLMExecutor` 在 dispatch 工具前 | `path-boundary`(10) / `validate-expected-hash`(20) / `require-read-before-write`(30) / `dependency-validator`(40) / `enforce-expected-artifacts`(50) / `validate-line-anchors`(60) |
| `tool:postCall` | dispatch 工具后 | （观察占位；`record-artifact` 已迁为 reactor） |
| `mailbox:beforeSend` | `mailbox.Registry.Send` 入口 | `chain-depth-limit`(20) |
| `mailbox:beforeDeliver` | 每收件人投递前 | `per-agent-dedup`(30) |
| `mailbox:beforeWake` | `MailNotifier.scan` 唤醒任务发布前 | `wake-worthy-filter`(40) / `wake-context-expand`(900) |

**Decision 语义**：`Action ∈ {Continue, Abort}` + 可选 `AbortReason` + （BeforeWake 专用）`WakeDescription` 累加。Gate.Matches 返回 false 时跳过；任一 Abort 立即短路。所有 Gate 继续时 BeforeWake 阶段聚合 `WakeDescription` 后由 reactor 写入 wake task。`panic` 被 recover 视作 Continue；nil Registry 返回 Continue —— **禁用所有 Gate 时行为与 v4 字节级一致**（回归测试基线）。

### 接入点（Tool 域）

`agent.NewLLMExecutor` 在并行工具 goroutine 内：
```
preCtx := gate.NewToolContext(Phase=tool:preCall, Tool, Args, TaskID, AgentID, ...)
decision := gateReg.Dispatch(preCtx)
if decision.Action == Abort:
    content = "[gate 拒绝] " + decision.AbortReason
    toolErr = error
else:
    result, toolErr = tools.Dispatch(ctx, call)

recordToolCall(taskID, ToolCallRecord{...})

postCtx := gate.NewToolContext(Phase=tool:postCall, ..., Result, Err)
gateReg.Dispatch(postCtx)  // 观察类，不短路（无 reactor 依赖此）
```

### 接入点（Mailbox 域）

```
mailbox.Registry.Send:
  ctx := MailboxContext{Phase: beforeSend, Message}
  if gateReg.Dispatch(ctx).Aborted: return error
  for each recipient:
      ctx2 := MailboxContext{Phase: beforeDeliver, Message, Recipient}
      if gateReg.Dispatch(ctx2).Aborted: skip recipient
      mb.TrySend(msg)

mailbox.MailNotifier.scan:
  for each non-empty mailbox status:
      ctx3 := MailboxContext{Phase: beforeWake, AgentID, EventType, UnreadCount}
      decision := gateReg.Dispatch(ctx3)
      if decision.Aborted: continue
      wakeTask := &Task{
          Description: decision.WakeDescription or default,
          MailChainDepth: status.MaxChainDepth,
      }
      store.PublishTask(wakeTask)
```

### 与 Scheduler 的关系

**Phase 3 重构以来 scheduler 走 `agent.NewLLMExecutor`，所有 Tool / Mailbox Gate 对 scheduler 工具调用与发邮件正常生效**，不存在"豁免通道"。这是修复历史 P0「report_done 不基于 `task.Artifacts`」的架构前提。

## Reactor 子系统（v5 真正新增的核心能力）

Reactor 让"状态变化之后的副作用"被显式表达 + **可由用户 YAML 声明**。`trace.Emit(ev)` 通过 `SetDefaultDispatcher(reactorReg)` 同时驱动 trace 写入与 Reactor 链路。

### 核心组件

| 包 / 文件 | 职责 |
|---|---|
| `internal/reactor/reactor.go` | `Reactor` 接口（`Name() / Subscribe() []EventKind / Run(ctx, ev) error / IsSync() bool / Priority() int`）|
| `internal/reactor/registry.go` | `Registry`：`map[trace.EventKind][]Reactor`，每桶按 Priority 升序；Sync 在 emit 调用方串行；Async 起独立 goroutine + recover |
| `internal/reactor/builtin/*.go` | 4 个内置 reactor（详见下表） |
| `internal/reactor/userdef/loader.go` | `LoadFromFile(path, projectRoot, deps)` 解析 `reactors.yaml`，构造用户 reactor |
| `internal/reactor/userdef/*.go` | 4 类动作动词的实现（publish_task / invoke_llm / spawn_agent / call:send_message）|

### 4 个内置 Reactor

| Reactor | IsSync | Priority | 订阅 EventKind | 功能 |
|---|---|---|---|---|
| `record-artifact` | Async | 950 | `KindFileWritten` | `write_file`/`edit_file` 成功时写 `Store.AppendArtifact(taskID, path)`；失败不记录。**v5 从 ToolHook PostCall 迁移而来**，是"角色错位治理"的典型示范 |
| `task-end-callback` | Sync | 500 | `KindTask{Completed,Failed,Cancelled,Retry}` | 调度已注册的任务结束回调（清理 `CurrentTaskHolder`、释放 per-task 资源等）。从 v4 的 `OnTaskEnd` 闭包形态迁移而来 |
| `trace-history-event` | Async | 950 | `KindHistory{Compaction,Truncated}` | 原子计数历史压缩 / 截断次数，便于性能诊断 |
| `read-set-write` | Async | 950 | `KindToolResult`（filter `tool=read_file`）| 写 `Task.ReadSet`，供 `require-read-before-write` Gate 替代旧时代反查 ToolCallHistory 的实现 |

此外，`spawn.Manager` 自身实现 `reactor.Reactor`，订阅任务终态事件销毁 ad-hoc runner（详见 §"Spawn 子系统"）。

### 用户 YAML Reactor

加载入口：`cfg.ReactorsFile`（顶层配置项，例如 `reactors.yaml`）。空值跳过加载。

**4 类动作动词（Phase 5 已落地）**：

| Verb | 用途 | 关键字段 |
|---|---|---|
| `publish_task` | 发布新任务 | `description` / `event_type` / `dependencies` / `expected_artifacts` 等 |
| `invoke_llm` | **一次性纯文本 LLM 调用**（无工具、无 history） | `prompt` / `model` / `output: { sink, ... }`（写文件 / 发邮件 / 进 reactor 自身上下文）|
| `spawn_agent` | 经 `spawn.Manager` 创建 ad-hoc agent | `base_kind` / `override` / `initial_task: { description \| via_translator }` / `lifecycle: one_shot` |
| `call: send_message` | 调用受白名单约束的内置工具（v1 仅 send_message）| `args` 模板化 |

**附加修饰符**：

- `when:` —— 条件表达式过滤（基于事件 payload 字段）
- `kind:` —— per-base-kind 过滤；ad-hoc agent 通过 `spawn.Manager.KindOf` 查得 base_kind 后参与匹配
- `via_translator:` —— 在 `spawn_agent` 之前先用 `invoke_llm` 把用户 prompt 加工成 `initial_task.description`；不允许嵌套

**仍 fail-fast 的占位字段**（schema 接受、运行期报错）：

- `lifecycle: persistent`（spawn_agent 长期形态尚未实现）
- `prompt.url` / `prompt.inline`（PromptSpec 占位字段）
- `call:` 其他工具（read_file / web_search 等需要 agent 上下文，按需逐个加白名单）

### Reactor 与 Trace 的耦合

`trace.SetDefaultDispatcher(reactorReg)` 在 bootstrap 末段调用。此后：

- `trace.Emit(ev)` 先写 trace.jsonl，再调 `reactorReg.Dispatch(ev)`
- Sync reactor 串行执行；任一失败 emit `KindError` 但继续其他 reactor
- Async reactor 立即 fork goroutine，panic-recovered，仅记日志
- `nil` Registry 是 noop —— 禁用所有 reactor 时 trace 行为不变（回归测试基线）

### 内置 vs 用户的失败语义对照

| 维度 | 内置 reactor | 用户 reactor |
|---|---|---|
| 注册者 | 开发者 Go 代码（bootstrap.go） | YAML 声明 |
| 同步性 | 允许 `IsSync: true` | **强制异步** |
| 失败影响 | panic-isolated，但失败应被 trace 显眼记录（系统不变量被打破） | panic-isolated，仅记日志 |
| 可订阅 EventKind 范围 | 全部 31 个内置系统事件 | `internal/reactor/userdef/loader.go` 白名单中的 27 个；4 个重规划控制事件仅内置可订阅 |

### Reactor 的判别速查（写新代码时）

1. 它是状态转换的"主动作"吗？ → 留在 `agent.go` 主流程，**不进 ReactiveSystem**
2. 它是状态变成 B 之后的副作用吗？ → Reactor（开发者 Go = 内置；用户 YAML = 用户）
3. 它在事件之前需要决策放行/否决吗？ → Gate
4. 它是邮件多对一聚合吗？ → 留在 `internal/mailbox/` 内部，**不立顶层抽象**（这是 ReactiveSystem v5 设计决议的下放点 —— 原 `Aggregator` 角色被废）
5. 它需要"在 ReactLoop 节奏点为 LLM 提供上下文"吗？ → 进 [Memory System](#memory-system取代-team-awareness-的-v5-子系统)，**不进 ReactiveSystem**（原 `Provider` 角色被废）

## Memory System（取代 team-awareness 的 v5 子系统）

### 模块命题

v4 的 `team-awareness-*` Hook 是 v2 时代的临时修复 —— 把团队感知信息硬编码注入 LLM history。它的实质问题不是"叫 Hook 不合适"，而是**整个机制的形态都错了**：所有"注入内容"都是临时文本，系统重启后项目知识全部丢失；agent 被多个 hook 反复"塞"东西，而不是主动从知识源拉取；信息复用、跨会话保留、向量检索等持久化记忆能力完全缺失。

v5 用 `internal/memory` 取代之，遵循 4 条设计哲学：**记忆是一等公民** / **Agent 主动拉取（非被动注入）** / **作用域分层（Process/Session/Project）** / **写入与读取解耦**。

### 接口与作用域

```go
type Scope int  // ScopeProcess (0) | ScopeSession (1, v5.x) | ScopeProject (2, v5.x)
type Kind  string // KindConstraint / KindLearning / KindPattern / KindContext / KindAgentState

type Store interface {
    Put(ctx, entry Entry) error
    Query(ctx, scope Scope, kind Kind, query string, limit int) ([]Entry, error)
    Delete(ctx, id string) error
    Clear(ctx, scope Scope) error
}

type Entry struct {
    ID, Key, Content, Source string
    Scope Scope; Kind Kind
    Tags []string
    CreatedAt, UpdatedAt time.Time
}
```

### v5 实现进度

| Scope | 状态 | 存储 | 说明 |
|---|---|---|---|
| `ScopeProcess` | ✅ v5 完成 | 纯内存（`map[ID]*Entry` + `(scope,kind,key)→ID` 双索引，单 RWMutex） | team_snapshot / file_awareness / 实时状态 |
| `ScopeSession` | 📅 v5.x | 落盘 `.agentgo/sessions/sess-<id>/memory.jsonl` | session 结束时清空 |
| `ScopeProject` | 📅 v5.x | 持久化 `.agentgo/memory/`（JSONL 或 SQLite） | 跨会话学习积累 |

### Query 语义（Phase 1 最小集）

- query 非空 → 精确 key 匹配（快速路径，用于 team_snapshot/file_awareness）
- query 为空 → 范围检索该 scope+kind 下全部条目（按 UpdatedAt 倒序，limit 截断）

### Agent 侧的读取入口

```go
// internal/agent/agent.go: processTask 入口
func (a *Agent) processTask(ctx context.Context, taskID string) {
    if a.Memory != nil {
        // 替代旧的 runAgentInject(PhaseTaskStart)
        if entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext, "team_snapshot", 1); len(entries) > 0 {
            history = append(history, HistoryEntry{IncomingMail: entries[0].Content})
        }
        if entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext, "file_awareness", 1); len(entries) > 0 {
            history = append(history, HistoryEntry{IncomingMail: entries[0].Content})
        }
    }
    // 进入 ReAct 循环
}
```

`Agent.Memory` 字段 `nil` 安全 —— 不持有 store 时退化为 v4-without-team-awareness 行为。

### 三 section 的迁移路径

| Section | v4 位置 | v5 去向 |
|---|---|---|
| `TeamSnapshot`（队友状态） | `PhaseTaskStart` / `PhaseLoopPre` 注入 | **Process Memory**：scheduler / runners 在团队状态变化时调 `Memory.Put`；Agent `processTask` 入口 `Memory.Query` |
| `FileAwareness`（文件占用） | 同上 | **Process Memory**：Roster 监听器 → `Memory.Put`；Agent 按需读取，不再每轮调 `ListClaims` |
| `GoalAnchor`（目标锚定） | 同上 | **直接删除，不迁移** —— `task.Description` 本身就是目标 |

### AgentHook 子系统命运

team-awareness 三个 hook 删除后 `AgentHookRegistry` 在事实上为空（v4 仅有的 2 个 AgentHook 都是 team-awareness）。v5 阶段保留空壳子系统作为未来"在 ReactLoop 节奏点做副作用"的扩展位；倾向方案是后续合并入 Reactor（订阅新增的 `KindReactLoopIterationEnd` / `KindTaskEnd`），按 Phase 1 启动具体安排再拍板。详见 `docs/activate/MemoryManageSystem.md` §5。

## Spawn 子系统（v5 新增）

`spawn.Manager` 让 reactor 可以"创建 ad-hoc agent"。本质是把"动态拉起 runner、跑完一个任务、自动销毁"这条路径包进一个 reactor.Reactor 实现里。

### 核心组件

| 组件 | 职责 |
|---|---|
| `spawn.Manager` | 同时实现 `reactor.Reactor`，订阅 `KindTask{Completed,Failed,Cancelled}` |
| `SpawnRequest` | `BaseKind` / `Override (RuntimeOverride)` / `InitialTaskDescription` / `Lifecycle` / `SourceTaskID` / `Depth` |
| `Manager.Spawn(ctx, req)` | (1) 解析 base_kind 模板 + 合并 Override → `AgentRuntimeConfig`；(2) 生成唯一 spawnID + `EventType="adhoc:<spawnID>"`；(3) 发布 initial_task；(4) 起 runner goroutine（ctx 派生自 `m.parentCtx`）；(5) 登记 activeSpawn 映射 |
| `Manager.Run(ctx, ev)`（reactor 实现）| Initial task 进入终态 → cancel runner ctx，清理映射（one_shot 销毁） |
| `Manager.KindOf(agentID)` | 返回 ad-hoc agent 的 base_kind，供 §6.2.4 per-kind reactor 路由使用 |

### 防护栏

- `ReactorSpawnMaxDepth = 5`：spawn_agent reactor 级联硬上限。超过时触发 `KindReactorSpawnDepthExceeded` 事件
- `lifecycle: persistent` schema 接受但运行期报错（v5 仅支持 one_shot）
- `via_translator` 不允许嵌套（一次 LLM 转译已是上下文隔离的极限）

### Wiring 锚点

```
bootstrap.go:
  spawnMgr := spawn.NewManager(cfg, deps, llmFactoryForSpawn, taskStore)
  reactorReg.Register(spawnMgr)         // 同时是 reactor

  userdefDeps.SpawnHost = spawnMgr      // 用户 reactor 的 spawn_agent 通过它创建 ad-hoc

  // KindOf 合并：静态 agent 从 staticKindOf；ad-hoc 从 spawnMgr.KindOf
  combinedKindOf := func(agentID) string {
      if k := staticKindOf(agentID); k != "" { return k }
      return spawnMgr.KindOf(agentID)
  }
```

---

# LLM 请求路径与 ExtraFields 透传（2026-04-25 落地；V6 起统一 OpenAI-compatible Chat Completions，Provider 层已移除）

## 背景

`internal/llm/client.go` 用 openai-go v3 官方 SDK 作为 HTTP/认证/重试基座。openai-go 是 OpenAI 官方的**强类型** SDK——响应 struct 的字段 = OpenAI 当前 schema；第三方 provider 对 OpenAI 协议的**非兼容扩展**会被默默吞掉：

- **DeepSeek V4 thinking 模式**：响应 `message` 里多一个 `reasoning_content` 字段，**并要求下一轮请求把它原样送回**，否则返回 400 `The "reasoning_content" in the thinking mode must be passed back`。openai-go 的 `ChatCompletionMessage` 没有这个字段 → 字段在接收时丢失；`ChatCompletionAssistantMessageParam` 也没有 → 回写时更不可能带上。结果第一轮成功，第二轮必崩（2026-04-24 日志）。
- **DeepSeek R1 (deepseek-reasoner)**：要求**相反**——下一轮请求必须删除历史 assistant 消息里的 `reasoning_content`，否则同样 400。
- **Qwen QwQ / Kimi / Claude 兼容网关**：各有各的自定义字段、`<think>` 标签、tool_use 格式差异。

"每遇到一个模型加一个 if/else" 走不通。本架构用 ExtraFields 通用透传机制把「保留即可」类差异收敛到可维护的边界上，**保留 openai-go 基座不变**。

## 层 1：ExtraFields 通用透传（零预知）

覆盖"保留即可"类扩展——不需要 AgentGo 理解任何字段语义。

- `llm.Message` 与 `llm.Response` 新增 `ExtraFields map[string]json.RawMessage`。
- 响应侧：`SDKClient.Chat` 遍历 openai-go 聚合好的 `choice.Message.JSON.ExtraFields`（openai-go v3 把所有未声明字段自动塞进这个 map），每个 `respjson.Field.Raw()` 的原始 JSON 字节抄进 `Response.ExtraFields`。
- 请求侧：`convertMessage` 的 assistant 分支在构造完 `ChatCompletionAssistantMessageParam` 后，用 openai-go 的 `param.metadata.SetExtraFields(map[string]any)` 把 `Message.ExtraFields` 挂回去——openai-go 序列化时会与已声明字段合并输出。
- Agent 侧：`ExecuteResult` 和 `HistoryEntry` 新增 `ExtraFields` 字段，`buildMessages` 重建对话时把它恢复到 `llm.Message`。通过 `json.Marshal(history)` 的持久化路径自动透传到 `task.LastHistory`。

**覆盖范围**：DeepSeek V4 的 `reasoning_content`、provider 自定义元数据等所有"只要原样回传就行"的扩展。无需编写任何模型专属代码。

## 层 2：Provider 插件（**已于 V6 移除**）——原 `Provider` 接口（`PrepareMessages` / `RequestOptions`）、注册表与 openai / openrouter / deepseek-v4 / deepseek-r1 四个内置实现已整体删除；V6 起只保留统一 OpenAI-compatible Chat Completions 请求路径，`llm.provider` 字段在 Validate 返回迁移诊断错误。层 1 ExtraFields 透传保留。

## 配置（v4 schema）

```yaml
llm:
  default_model: ...
  base_url: ...
  api_key: ...
  timeout_sec: ...
```

`bootstrap.go` 经 `runtime_builder.buildKindLLMClient` 统一构造 `llm.Client` 注入所有调用点（V6 起不再传 provider）。per-kind 模型区分由 `model` 名（同一 endpoint 下选不同模型）承担。

## 为什么不直接丢弃 openai-go

考虑过激进方案：改用 `net/http + encoding/json`，所有消息走 map。litellm / LangChain 走的就是这条路。但 openai-go 提供的重试、类型补全、参数校验、tool schema 组装仍然值得保留；层 1 ExtraFields 透传已覆盖「保留即可」类典型非兼容扩展，无须下这一刀。如果未来出现透传无法优雅处理的 endpoint（例如完全不同的 tool 协议），再考虑把底层 HTTP 层接口化。

## 关键文件

- `internal/llm/client.go` —— Message/Response 结构 + SDKClient + extras 抽取/回写
- `internal/llm/client_test.go` —— 单元测试 + httptest 集成测试（含 ExtraFields 往返断言）
- `internal/agent/agent.go` / `internal/agent/llm_executor.go` —— HistoryEntry.ExtraFields 透传
- `internal/config/config.go` —— `LLMConfig.ReasoningEffort` / `Stream` 字段（顶层 `llm:` 块）；`Provider` 仅保留解析位，非空即被 Validate 拒绝（V6 迁移诊断）
- `internal/bootstrap/bootstrap.go` —— 所有 `llm.NewSDKClient(...)` 调用点串联

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
                              └─ SchedulerGroup（cancel_task + get_task_result + report_done）
```

## 核心组件

| 包 / 文件 | 职责 |
|---|---|
| `internal/scheduler/scheduler.go` | `Bundle` struct（Agent + Activator + Modes），`New(...)` 构造一等代理及其配套部件，`schedulerSystemPrompt`，`currentSchedulerTaskHolder`，`storeBatchTracker` |
| `internal/scheduler/executor.go` | `SchedulerExecutor` —— TaskExecutor wrapper，等 batch + 注入 snapshot |
| `internal/scheduler/snapshot.go` | `BuildBoardJSON` —— 输出当前 Graph/请求树可见任务；终态正文投影为有界 `result_refs`，执行中输出投影为有界 `progress` |
| `internal/scheduler/activator.go` | `Activator` goroutine，EventCh ↔ task 桥 |
| `internal/tools/scheduler.go` | `SchedulerGroup`：`cancel_task` + `get_task_result` + `report_done` + `report_progress` |
| `internal/tools/meta.go` | `MetaGroup` 新增 `BatchTracker` 字段，scheduler 注入时 publish_task 追加到 `task.SchedulerBatch` |
| `internal/model/task.go` | `Task` 新增 `SchedulerBatch []string` 字段（仅 scheduler task 使用） |
| `internal/store/{iface,memory}.go` | `AppendSchedulerBatch` / `ClearSchedulerBatch` 方法 |

## 关键设计决策

| # | 决策 | 实现 |
|---|---|---|
| D1 | 等待 batch 完成的实现：**SchedulerExecutor 内部同步 select 阻塞** | `executor.go::waitForBatchTerminal` 在 `BatchUpdateCh` 与 30s 兜底之间循环。比 RetryRollback spin loop 干净（不堆 RetryCount，watchdog 友好），比 dependency 机制安全（不会被 worker failed 级联取消） |
| D2 | `task.SchedulerBatch` 是新字段，不复用 Dependencies | Dependencies 严格 completed 语义，scheduler 需要"终态"语义；新字段命名清晰 |
| D3 | `SchedulerGroup` 只放 Scheduler 专属控制/结果工具 | `get_task_result` 仅分页读取当前可见终态结果；`publish_task` / `send_message` 仍复用 `MetaGroup`，并通过 `BatchTracker` 追加到 `task.SchedulerBatch` |
| D4 | `Activator` 是 EventCh ↔ scheduler agent 的桥 | `EventUserInput` → `PublishTask`，`EventTask{Completed,Failed,Cancelled,WatchdogAlert}` → `BatchUpdateCh` 信号 |
| D5 | board snapshot 通过 `IncomingMail` 注入 history | 复用既有的 `agent.HistoryEntry.IncomingMail` 字段，与 mailbox 注入对称 |
| D6 | scheduler task 的 `TimeoutSeconds=86400`（1 天） | `MemoryTaskStore.PublishTask` 把 0 替换为默认值，scheduler 必须显式设大值；24h 是工程兜底 |

## scheduler 重构后获得的新能力

| 能力 | 旧 scheduler | 新 scheduler |
|---|:---:|:---:|
| Tool Hook 系统（pre/post call 拦截） | ❌ 完全豁免 | ✅ 走 NewLLMExecutor，所有 hook 自动生效 |
| 3 层历史压缩 | ❌ | ✅ |
| FileStateCache (per-agent LRU 50，Get 时 stat mtime+size 校验跨 agent 新鲜度) | ❌ | ✅ |
| Trace events（KindTaskClaimed/Submitted/Completed） | ❌ | ✅ |
| Per-task cancel context (`CancelRegistry`) | ❌ | ✅ |
| `read_file` / `grep_search` / `web_search` 等 worker 工具 | ❌ | ✅ |
| `send_message` 自动写 ChainDepth（共享 MetaGroup） | ❌ ChainDepth 永 0 | ✅ 与 worker 共享同一份 |
| `RecordArtifactHook` 在 scheduler 工具调用时生效 | ❌ | ✅（修复"P0 report_done 不基于 Artifacts"的根因之一） |

## scheduler 保留的独有特征

| 能力 | 实现 |
|---|---|
| 事件驱动入口 | `Activator` goroutine 监听 EventCh |
| 当前控制域任务板 | `BuildBoardJSON` 注入当前 Graph 有效图或 legacy 请求树；结果 excerpt 全板共享上限 |
| `cancel_task` / `get_task_result` / `report_done` | `SchedulerGroup`，独占工具，worker 没有；完整结果只在 excerpt 不足时按 rune 偏移读取 |
| **探针工具 `probe_directory`** | Scheduler 专属目录探测，用于任务规划前了解工作区全貌 |
| 系统级 mailbox 别名 `"scheduler"` | `New` 构造时 `mbRegistry.RegisterAlias("scheduler", schedID)` |
| 提前 `report_done` 硬拦截 | `SchedulerGroup.report_done` 内部扫描 `task.SchedulerBatch` 状态 |
| `task.Artifacts` 事实校对 | `SchedulerGroup.report_done` 调 `buildSchedulerArtifactsReport` |
| exec / topo 两轴模式切换 | `Bundle.Modes`（`internal/modes`），CLI `/mode` 命令通过 setter 切换 |

### Scheduler 探针工具（`probe_directory`）

**位置**: `internal/tools/scheduler_probe.go`

Phase 3 重构后 scheduler 获得直接读取文件的能力，探针工具在此基础上提供更高效的任务规划前侦查：

**功能**：
- 递归遍历目录树（可配置深度，默认 3 层，最大 10 层）
- 统计文件夹数、文件数、总大小
- 文件类型分布（按扩展名分类，Top 5 + 其他）
- 格式化树状输出（最多 500 个条目，防止输出过大）

**使用场景**：
```
用户: "请分析这个项目"
Scheduler: probe_directory(path=".") 
→ 获得项目结构概览，决定如何拆分任务
```

**输出示例**：
```
[综述] 根目录: /AgentGo | 文件夹: 42 | 文件: 187 | 总大小: 2.4 MB
[类型分布] .go: 89 (48%) | .md: 23 (12%) | .yaml: 12 (6%) | .json: 8 (4%) | .mod: 3 (2%) | 其他: 52 (28%)

internal/
  agent/
    agent.go                                    12.5 KB
    llm_executor.go                              8.3 KB
    ...
  scheduler/
    scheduler.go                                 6.7 KB
```

**与其他工具的对比**：
| 工具 | 用途 | 输出特点 |
|------|------|---------|
| `list_dir` | 查看单层目录 | 简单列表 |
| `glob_search` | 按模式找文件 | 文件路径列表 |
| `probe_directory` | 任务规划前侦查 | 统计 + 树状结构 + 类型分布 |

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
    - **可恢复故障重试**：LLM 瞬时错误（429/5xx 等）触发重试回退路径（processing→pending），重试次数加一，并将已有的部分结果写入重试原因，使下一个接手的代理能获得充分的上下文提示，避免重蹈覆辙；重试预算耗尽则任务失败。（V6 起不再有固定循环轮数上限；程序性死循环由不可配置的 emergency fuse 兜底——触发后任务进 blocked 并登记 replan，不自动重跑。）
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

## Context v3 Raw History / Replay Projection

**位置**：`internal/agent/history_projection.go`、`internal/contextadapter`、`internal/contentstore`。

系统不再按累计完整 Prompt Token 修改 History，也不再使用 `snipOldToolResults` / `compressHistory` / `keepRecent=1` 三层有损压缩：

1. settled Turn 作为 Raw History 保持不可变；
2. 每次 Invocation 前，L2 按当前 `conversation_history` 与 `tool_results` section 使用率重新派生 replay 视图；
3. 压力不足时全部保留；接近预算时只在视图中以有界、带 digest/TurnID 的索引替代较旧轮次，索引每次从 Raw History 生成，不递归压缩旧摘要；
4. Optional `reasoning` 超限记录 `DispositionDropped`，不终止 Agent；RequiredExact provider 字段在 Response commit、任何工具执行之前证明可重放；
5. 大 ToolResult 先持久化到 Task-scope ContentStore，History 只保存 `ref_id`、原始尺寸/digest 与首尾预览，可用 `read_content_ref` 分页；
6. provider `context_window_exceeded` 创建新 Attempt 并请求 aggressive replay projection，不改写 Raw History。

Context v3 同时冻结 model window、completion reserve、protocol overhead 与 Invocation OutputBudget；SDK 实际上限取 L2 reserve、L4 剩余预算与模型安全上限的最小值。旧 `enforce_compact_token_threshold` 配置已删除，显式设置返回迁移诊断。

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
- 不负责长任务的具体执行——通常通过 `publish_task` 派发给非 scheduler 的 runner（典型如 worker / explorer kind），保留自己的上下文容量给规划决策
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

## 调查代理（Explorer kind）

> **v5 实现现状**：v4 之前 Explorer 是独立的 `internal/explorer` 包；**v5 已删除该包**，"调查代理"在实现上就是 `agents:` 列表中一个声明 `event_type: explore` + `profile: read-only` 的 `runner.Runner` 实例。本节保留以阐述其**配置语义**与**调度器何时应当用它**，而不是描述独立子系统。

### 在 setting.yaml 里如何声明

```yaml
agents:
  - kind: explorer
    replicas: 1
    event_type: "explore"               # scheduler 通过此 EventType 派发调查任务
    profile: "read-only"                # 工具集只含 read/list/grep/glob/web_*，无写入工具
    model: "qwen3.6-flash"              # 通常用更便宜更快的模型
    system_prompt_file: "prompts/explorer.md"
    description: "广度优先的只读调查代理，不写文件，仅返回 Markdown 文字回复"
```

### 调度器何时应当 publish 调查任务

- **验证历史结论**：调度器基于公告板中的已完成任务做决策前，先发布 `event_type=explore` 的调查任务确认历史结论是否仍然成立。倾向于这样做的场景：
    - 有一个或多个目标文件的调查结果完全缺失，调度器无从得知必须文件的内容
    - 发现存在冲突或更改（一个文件的调查记录之后存在更改记录等）—— 但这并非程序强制，而是在 system prompt 中给出指引；幅度小的更改可以不启动调查
- **快速信息检索**：对项目文件、代码、配置等进行只读检索，返回当前状态的快照
- **对比变更**：将历史任务的结论与当前项目状态进行比对，标注哪些结论已过时

### 配套的硬约束

- **只读纪律**：scheduler 与 `MetaGroup.publishTask` 双端硬拒绝 `event_type=explore && expected_artifacts != nil` —— 防止 scheduler 误把"必须写文件"的任务派给 explorer
- **降级模型**：用 `model:` 字段切到便宜模型（如 `qwen3.6-flash`、`gpt-4o-mini`），降低验证成本
- **结果简短**：通过 system prompt 引导 explorer 用"结论仍然成立 / 结论已过时（附当前状态摘要）"的简短回复格式

# 任务依赖管理
本项目不使用有向无环图（DAG）进行全局任务编排。原因：DAG 要求在任务发布前确定完整的任务拓扑，但 LLM 驱动的任务天然是动态展开的，调度器无法在接收用户输入时就规划出完美的任务图。
## 替代方案：轻量级前置依赖
- 每个任务可以声明一个前置依赖列表（零到多个任务 ID）
- 代理领取任务时，公告板检查所有前置任务是否已 completed；若未完成，则拒绝领取
- 前置任务 failed 或 cancelled 时，看门狗巡检发现后连锁取消依赖它的后继任务
- 不做环检测——由调度器在发布任务时自行保证不产生循环依赖，这是调度器作为 LLM 代理的责任
## 工作模式
系统支持两种工作模式，默认启动时为即时模式。TUI 通过 `/mode` 斜杠命令切换（**Phase 3 改动**：旧设计提到的 `Shift+Tab` 快捷键未实现，实际是 `/mode`；切换通过 `Bundle.Mode` (`*scheduler.ModeStore`)，scheduler agent 在每次 reactLoop 注入 board snapshot 时实时读取最新 mode）。
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
事件 → Activator → store/Channel 之后，scheduler agent 读到的是 `BuildBoardJSON` 注入的当前控制域快照（含可见 task 状态、Artifacts、依赖、resources 和有界结果引用），LLM 据此做决策：
- **任务 completed/failed/cancelled**：通过 board snapshot 看到，结合 SchedulerBatch 决定是否进入下一阶段
- **用户新输入**：scheduler task 的 `Description` 字段就是用户文本
- **看门狗告警**：board snapshot 显示该 task 状态变更（通常进入 failed），scheduler 据此决策
### 调度器 ReAct 循环（Phase 3 新架构）
scheduler agent 与 worker / explorer 共享同一套 `agent.Agent.processTask` 实现，区别仅在于 `TaskExecutor` 是 `SchedulerExecutor`（包装 `NewLLMExecutor`）：
1. **认领**：`agent.Agent.Run` poll 到 `__scheduler__` 任务，`ClaimTask` + `processTask`
2. **等待 batch**：`SchedulerExecutor.Execute` 进入前先 `waitForBatchTerminal`——如果 `task.SchedulerBatch` 中还有非终态任务，select 在 `BatchUpdateCh` / 30s 兜底 / `ctx.Done()` 之间循环等待
3. **观察**：调用 `BuildBoardJSON` 生成当前 Graph/请求树快照，注入到 history 末尾（`IncomingMail` 类型，与 mailbox 注入对称）
4. **思考**：调底层 `NewLLMExecutor` 实际调用 LLM
5. **行动**：LLM 调用 `publish_task`（追加到 `task.SchedulerBatch`）/ `cancel_task` / `get_task_result`（按需分页）/ `report_done` / `send_message` / `read_file` / `grep_search` / 等等
6. **循环**：LLM 还有 tool call 则下一轮 reactLoop（`agent.Agent.processTask` 内部 for 循环，无固定轮数上限）；LLM 给文本响应（无 tool call）则任务完成
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
- **`mailbox.Mailbox`**：单个代理的收件箱，内部是带缓冲的 Go channel + 容量 16 的 ring buffer（用于 Gate 的 peek-without-consume）。二者语义不同：channel 是未读队列，ring buffer 是包含已读历史的观察窗口；v4 Session 快照只持久化未读队列。
- **`mailbox.MailNotifier`**：独立 goroutine，定期扫描非空信箱，为有未读邮件的空闲代理发布"唤醒任务"。**默认启用**（`cfg.MailNotifierEnabled=true`，2026-04-09 Phase 2 完成后恢复）。Phase 2 的 `ChainDepthLimitHook` (max=3) + `PerAgentDedupHook` + `WakeContextExpandHook` 三层防御彻底关闭了邮件级联爆炸 P0。

### 消息结构（`mailbox.Message`）
- `From` / `To` / `Content` / `Summary` / `SentAt`
- `Type`：`info` / `question` / `reply` / `steer` / `ack`
- `Priority`：`low` / `normal` / `high`
- `ChainDepth`：邮件链跳数（Phase 2 引入），由 `MetaGroup.sendMessage` 自动写入 `parent.MailChainDepth + 1`，超过 `cfg.MailChainMaxDepth`（默认 3）时被 `ChainDepthLimitHook` 在 `BeforeSend` 阶段拒绝

`DrainWithAck` 在代理消费消息时自动向发送方回送 `type=ack` 已读回执。

### 工具与代理集成
- `send_message` 工具属于 `MetaGroup`，被所有 runner（worker / explorer / 任何用户声明的 kind）以及 scheduler 共享注册（**Phase 3 后**：scheduler 通过同一份 `MetaGroup.sendMessage` 注册，消除了之前的双写实现），支持 `to=<agentID>` 点对点或 `to=*` 广播（自动跳过自己）。tool_profile 没把 `send_message` 列进 allowlist 的 kind 在工具注册时被剪枝。
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
- 只读 kind（典型如 explorer，但泛化为所有 `event_type=explore` 的 kind）：scheduler 与 `MetaGroup.publishTask` 双端硬拒绝 `event_type=explore && expected_artifacts != nil`，防止派发"必须写文件"的任务给只读代理。

### `Task.LastResponse`（失败诊断锚点）
- Runner 每次 non-tool LLM 响应都通过 `Store.RecordLastResponse(taskID, content)` 无条件持久化，无论后续校验成败。
- 任务最终崩溃时 `sendCrashReport` 把 LastResponse 原文附在邮件正文里发给 `task.EventSource`，scheduler 不再只看到一个干瘪的"重试次数耗尽"。

### 校验反馈进入历史
`appendValidationFeedback` 把 ExpectedArtifacts 校验失败的诊断（缺失文件、实际写入文件、纠正策略）作为 `<validation-feedback>` 段以 `IncomingMail` 形式注入历史，重试时 LLM 能直接看见自己为何被打回，避免"重试还是同样输出"的死循环。

### 终态崩溃汇报
`agent.terminateTask` 在 `RetryCount >= a.MaxRetries` 时调用 `sendCrashReport`，向 `task.EventSource` 发送 `priority=high` 邮件，正文格式："代理 X 在执行任务 Y 时崩溃，原因 Z；任务描述、重试次数、expected vs actual artifacts、worker 最后一次响应原文"。`MaxRetries` 由各壳包（worker/explorer/scheduler）的角色常量设置，不由 yaml 配置。

# 系统启动流程

系统由 `main.go` → `bootstrap.Bootstrap(configPath, explicit, skipStartupProbe)` 完成初始化，再由 `System.Start(ctx, cancel)` 拉起所有 goroutine，最后 `System.RunCLI(ctx)`（内部调 `tui.Run`）阻塞主线程。

`main.go` 入口除 `-config` / `-skip-startup-probe` 外还支持 `trace` 子命令：`./agentgo trace list/show/graph/node/stats ...` 不进 bootstrap，直接进入 `internal/trace/cli.go`。Trace CLI 自动通过 `.agentgo/sessions/active-session` 解析当前 session 的 `logs/` 目录，回退到 `.agentgo/traces/`；`graph <graph_id>` 会按时间聚合单个 Graph 的生命周期事件。

## Bootstrap 阶段（构造对象图，v5 顺序）

| Step | 子系统 | 关键调用 |
|---|---|---|
| 1 | 配置加载 | `config.LoadConfig(path, explicit)` + `cfg.Validate()`（v4 §11.5.3 12 条规则） |
| 1.1 | 启动 banner | `printStartupBanner(stdout, configPath, cfg)` —— 逐 kind 摘要 + 脱敏 api_key |
| 1.2 | 启动期 provider probe | `startupProbe(stdout, cfg)`：`tool` 执行 TCP + 真实 function call schema/arguments；`tcp` 仅连通；`off`/`-skip-startup-probe` 跳过 |
| 1.3 | Session 管理器 | `session.NewSessionManager(...)` + `history.jsonl` 溯源 |
| 1.5 | Trace 系统 | `trace.NewWriter(traceDir, 100)` + `trace.SetDefault()`；Session 活跃时 `traceDir = sessMgr.LogDir()` |
| 1.6 | Prompt Dumper | 条件启用（`AGENTGO_DUMP_PROMPTS=1`） |
| 2 | 公告板 | `store.NewMemoryTaskStore(eventCh, ...)` + `store.NewTaskCancelRegistry()` |
| 2.3 | Artifact 日志 | `store.OpenArtifactLog()` + `Replay()` + `RestoreArtifacts()` |
| 2.5 | **Gate Registry** | `gate.NewRegistry()` + 注册 6 个 Tool 域 Gate（path-boundary 等） |
| 3 | 花名册 | `roster.NewMemoryRoster()` |
| 3.5 | 邮箱注册表 | `mailbox.NewRegistry(...)` |
| 3.5.1 | Session History 注入 | `SetHistoryEmitter()` 串联 store / roster / mailbox |
| 3.6 | Mailbox 域 Gate | 注册 4 个 Mailbox 域 Gate（chain-depth-limit / per-agent-dedup / wake-worthy-filter / wake-context-expand）+ `mbRegistry.AttachHookRunner(gate.AsMailboxRunner(gateReg))` |
| 3.8 | **Memory System** | `memory.NewProcessStore()` —— team_snapshot / file_awareness 共享存储 |
| 3.9 | **Reactor Registry** | `reactor.NewRegistry()` + 注册 4 个内置 reactor（record-artifact / task-end-callback / trace-history-event / read-set-write） |
| 5 | Scheduler | `scheduler.New(...)` 返回 `*Bundle{Agent, Activator, Modes}`；scheduler 是 `EventType="__scheduler__"` 的一等 agent |
| 6 | 看门狗 | `watchdog.New(store, cfg, eventCh, roster)` |
| 6.8 | 工具可用性探针 | `probe.RunAll()` —— 检测 `web_search` / `web_fetch` 实际可用性 |
| 7.4 | 通用 Interaction Service | `interaction.NewService(...)`；Graph approval、Shell、Agent question、TUI 与 Web 共享 Version CAS 状态机 |
| 4.5 | UI 输出通道 | `statusCh` + `outputCh`，由 UI Hub 统一消费并向多前端投影 |
| 8 | **Runner 实例化** | 按 `cfg.Agents` 循环，每 kind × `replicas` 调用：(1) `runtime_builder.buildAgentRuntime(kind, replicaIdx)` 合成 `AgentRuntimeConfig`（含 `InstanceID="<kind>-<replicaIdx>"`、`AllowedTools` 由 `profile` 或 `tools` 决定）；(2) `runner.New(rt, deps)` 构造 Runner（含 ToolRegistry allowlist 剪枝、TaskEndCallbackReactor 注册、Mailbox 注册）；(3) 打印 `Runner <id> 已启动 [kind=..., model=...]` |
| 8.5 | **Spawn Manager** | `spawn.NewManager(cfg, deps, llmFactoryForSpawn, taskStore)` + `reactorReg.Register(spawnMgr)`（manager 自身就是 reactor） |
| 8.6 | 用户 YAML Reactor | `cfg.ReactorsFile` 非空时 `userdef.LoadFromFile(...)` 解析并注册到 `reactorReg` |
| 9 | Reactor Dispatcher | `trace.SetDefaultDispatcher(reactorReg)` —— 使 `trace.Emit` 同时驱动 trace.jsonl 写入与 Reactor 链路 |
| 10 | 邮差通知器 | `mailbox.NewMailNotifier(...)` 对象 |

## Start 阶段（拉起 goroutine）

| 顺序 | 组件 | 关键调用 |
|---|---|---|
| 0 | Spawn Manager parent ctx | `spawnMgr.SetParentContext(ctx)` —— 后续 ad-hoc runner 的 ctx 都派生自此 |
| 1 | Scheduler Activator | `scheduler.Activator.Run(ctx)`（事件桥）—— **必须先就绪**，否则 `EventUserInput` 可能丢失 |
| 2 | Scheduler Agent | `scheduler.Agent.Run(ctx)`（poll-based） |
| 3 | 看门狗 | `runWatchdogWithRecover(ctx)` —— for 循环 + recover，panic 后延迟重启 |
| 4 | 邮差通知器 | `MailNotifier.Run(ctx)`，仅当 `cfg.Infra.MailNotifier.Enabled=true` 时启动（**默认启用**） |
| 5+ | Runner（all kind × replica） | `runner[i].Run(ctx)` 并行 goroutine |

最后打印 `[启动] 系统就绪，等待用户输入`。

## RunCLI 阶段

`sys.RunCLI(ctx)` 内部调 `tui.Run(ctx, deps)`，基于 Bubble Tea；是否使用 alt-screen 由运行环境决定。

- **斜杠命令**：以 `internal/ui.CommandCatalog()` 为事实源；包含 `/help /status /cancel /mode /steer /new /session /doctor` 共享命令和 TUI 视图/退出命令
- **自由文本**（非 `/` 开头）→ `EventUserInput` 写入 `eventCh`，同步调 `SessionManager.RecordFirstInput` + `IncrementTaskCount`；`scheduler.Activator` 翻译为 `EventType="__scheduler__"` 任务，scheduler agent 在下次 poll 时认领
- **Interaction 面板**：面板与普通输入区同时存在；显式切到 Interaction 焦点后用 `↑/↓` + Enter，需文本选项回到 textarea。可打印字符在普通输入焦点始终是文本
- **并发回答与队列**：Hub 下发完整 pending 列表；回答携带 Version CAS，first-writer-wins。当前条目完成后显示下一项，竞争或陈旧回答不会覆盖已接受选择
- **没有 `"""` 多行块**（v1 范围决议；v4 之前的 `bufio.Scanner` 多行聚合机制已不存在）

## Shutdown 阶段

`sys.Shutdown()` 顺序：
1. `cancel()` 传播到所有 ctx
2. `SpawnManager.Shutdown()` —— 终止所有 ad-hoc runner
3. `s.wg.Wait()` 等待所有静态 goroutine
4. `trace.Default().Close()`
5. `trace.DefaultDumper().Close()`（若启用）
6. `ArtifactLog.Close()`
7. `SessionMgr.Close()`

## 启动顺序约束

- 公告板、花名册、Gate、Memory、Reactor 是基础设施，必须先于所有 agent 初始化
- Scheduler 先于其他 agent goroutine 启动（消费者先就绪，避免事件丢失）
- Activator 先于 Scheduler Agent 启动（否则 `EventUserInput` 在 Agent 未启动时到达可能丢失）
- 看门狗先于 runner 启动，确保第一批任务就处于监控之下
- `trace.SetDefaultDispatcher(reactorReg)` 必须在所有 reactor 注册**之后**调用，否则早期事件无法触发 reactor
- 任一步骤失败时返回 error 终止启动，不进入半初始化状态

# Trace 系统

**位置**: `internal/trace/`

Trace 系统为每个任务记录完整的执行轨迹，便于问题诊断、性能分析和行为审计。

## Trace 文件结构

**存储位置**:
- Session 活跃（生产默认）：`.agentgo/sessions/sess-<id>/logs/`
- 无 Session：回退 `.agentgo/traces/`

`main.go` 的 `trace` 子命令会先读 `.agentgo/sessions/active-session` 并定向到对应 `logs/`；读不到时回退到 `.agentgo/traces/`。

**文件命名**: `<UTC时间戳>_<taskID前8位>.jsonl`
- 示例：`2026-04-08T04-17-06_321b561d.jsonl`
- 重试会关闭并重新打开 writer，同一 Task 可能对应多个物理文件；同秒且前 8 位碰撞时，一个物理文件也可能含多个 Task。CLI 始终按事件中的完整 `task_id` 聚合。

**格式**: JSON Lines（每行一个 JSON 对象）

## 事件类型（Schema B，2026-07-18 当前实现）

`Event` 顶层字段保持向后兼容，使用可选指针子结构体：`Transition` / `ShellExec` / `ShellTimeout` 承载状态和 Shell 信息，`Lease` / `Suggestion` / `Effect` / `Acceptance` 承载执行租约、结构化建议、副作用账目与验收核验事实；V6 Graph Runtime 事件字段平铺在顶层。`omitempty` 让旧 jsonl 仍可被当前 viewer 读取。

### 任务/Agent 状态变更类

| EventKind | 说明 | 关键字段 |
|-----------|------|----------|
| `task_published` | 任务发布 | `task_id`, `parent_task_id`, `batch_id`, `description`, `dependencies`, `event_type`, `priority`, `depth`, `published_by` |
| `task_claimed` | 任务被认领 | `task_id`, `agent_id`, `Transition{prev_status, new_status, cause}` |
| `task_submitted` | SubmitResult 成功，准备进入 completed | `task_id`, `agent_id`, `output_len`, `loops_used` |
| `task_completed` / `task_failed` / `task_blocked` / `task_cancelled` / `task_retry` | 任务终态 / 重试 | `task_id`, `agent_id`, `Transition{...}`（含 cancel_source / retry_count） |
| `agent_state_changed` | **v5 新增** —— Agent 4 状态枚举切换 | `agent_id`, `Transition{prev_state, new_state, cause}`；**开放给用户 reactor** |
| `error` | 非终态错误（ExpectedArtifacts 校验失败、SubmitResult 异常等） | `task_id`, `agent_id`, `error` |

### 工具/LLM 类

| EventKind | 说明 | 关键字段 |
|-----------|------|----------|
| `llm_call_start` / `llm_call_end` | LLM 调用 | `history_entries`, `tool_calls_count`, `prompt_tokens`, `completion_tokens`, `finish_reason`, `duration_ms` |
| `tool_call` | 工具调用开始 | `tool`, `call_id`, `args` |
| `tool_result` | 工具调用完成 | `tool`, `call_id`, `args`, `duration_ms`, `result_len`；触发 `read-set-write` reactor |
| `text_only_submission` | LLM 仅文本响应（无 tool call，自然完成） | `task_id`, `output_len` |

### 文件 / Shell 类

| EventKind | 说明 | 关键字段 |
|-----------|------|----------|
| `file_written` | `write_file` / `edit_file` 成功 | 触发 `record-artifact` reactor → `Store.AppendArtifact` |
| `file_write_queued` | 文件写入进入队列或完成等待 | `path`, `description`, `wait_ms`；`queue_len` 当前为兼容预留字段 |
| `shell_executed` | **v5 新增** —— shell 命令完成 | `ShellExec{command, exit_code, duration_ms, outcome, stdout/stderr_excerpt}` |
| `shell_timeout_pending` | **v5 新增** —— shell 命令超时等待决策 | `ShellTimeout{command, elapsed_sec, previous_waits, ...}` |
| `shell_timeout_resolved` | **v5 新增** —— shell 超时决策落地 | `ShellTimeout{decision, extra_seconds, ...}` |

### 历史压缩 / 通知 / Reactor

| EventKind | 说明 | 关键字段 |
|-----------|------|----------|
| `history_compaction` | 历史压缩 | `prompt_tokens_before`, `prompt_tokens_after`, `strategy`, `kept_entries`；触发 `trace-history-event` reactor |
| `progress_notify` | 进度通知（`progress_notify_enabled=true` 时） | `task_id`, `agent_id`, `loop`, `notify_type` |
| `reactor_spawn_depth_exceeded` | spawn_agent reactor 级联超限（`ReactorSpawnMaxDepth=5`） | `task_id`, `depth`, `reason` |

### Graph 控制面（V6）

Plan 控制面（replan_* / plan_* / acceptance_completed）已于 V6 整体删除。Graph 生命周期事件携带 `graph_id` / `node_id` / `activation_id` 并写入独立 `graph_<id>.jsonl` 分片：

| EventKind | 说明 |
|-----------|------|
| `graph_submitted` / `graph_submission_rejected` | JSON Graph 提交激活 / 校验失败 |
| `node_activation_created` | 节点新 activation（回边新建，绝不重开旧 task） |
| `graph_transition_selected` | 边选择生效（按 source activation 幂等） |
| `graph_wait_started` / `graph_wait_resumed` | wait_event / approval 挂起与恢复 |
| `graph_join_resolved` | join 汇聚裁决 |
| `graph_approval_decided` | 人工审批决议 |
| `graph_revision_committed` | patch_graph 定义变更提交 |
| `graph_change_requested` | 节点请求 graph change（唤醒 Scheduler） |
| `graph_ended` | 图到达终态（completed/failed） |

> **`task_submitted` / `task_completed` 对称性**：`agent.processTask` 的两条完成路径（自然完成 / Finalized 跨轮短路）都会 emit 这两个事件。2026-04-19 修复前，短路路径只 `SubmitResult` 未 emit，导致 `trace list` 把 scheduler 任务错标为 `running/loops=0`。

> **Schema B 兼容性**：新字段 `omitempty`，旧 jsonl 仍可被新 viewer 读懂；旧 viewer 读新 jsonl 时看不到新增子结构细节，但不会因未知 JSON 字段崩溃。

### Reactor 订阅约定

| 开放级别 | EventKind |
|---|---|
| **开放给用户 YAML reactor** | `internal/reactor/userdef/loader.go` 中 `knownEventKinds` 白名单（任务、LLM、工具、历史、文件、Shell、错误、Graph 等事件） |
| **仅内置 reactor 可订阅** | 白名单外的运行时事件（详见 loader.go 注释） |

## 使用示例

### 查看最近任务的 trace
```bash
./agentgo trace list                        # 推荐：自动定向 active session
ls -lt .agentgo/sessions/sess-*/logs/ | head -10   # 或直接看当前 session
```

### 分析特定任务的执行流程
```bash
# 读取 trace 文件（以 active session 为例）
cat .agentgo/sessions/sess-<id>/logs/2026-04-08T04-17-06_321b561d.jsonl | jq '.'

# 统计工具调用次数
cat .agentgo/sessions/sess-<id>/logs/*.jsonl | jq 'select(.kind=="tool_call") | .tool' | sort | uniq -c

# 查看 LLM token 消耗
cat .agentgo/sessions/sess-<id>/logs/*.jsonl | jq 'select(.kind=="llm_call_end") | {prompt: .prompt_tokens, completion: .completion_tokens}'
```

### Prompt Dump（调试模式）
设置环境变量启用详细 prompt 记录：
```bash
AGENTGO_DUMP_PROMPTS=1 ./agentgo
```

## 并发安全

- 每 Writer 实例一把互斥锁（`sync.Mutex`）
- `Emit()` 调用串行化，保证 JSON 行不交错
- 文件名只使用 TaskID 前 8 位；罕见碰撞由 CLI 读取完整 `task_id` 拆分，不依赖文件名推断逻辑任务

## GC 策略

- 默认保留最近 100 个物理 trace 文件（重试分片分别计数）
- 超出限制时按修改时间删除最旧文件
- 正在写入的文件不会被删除

# 全局配置（v4 块状 schema，2026-04-26 起唯一支持的格式）

系统运行所需的全局参数，从 `setting.yaml` 或 `setting.json` 读取，文件不存在时使用内置默认值。当前仅支持 `-config <path>` 命令行参数指定文件路径，**单字段命令行覆盖未实现**。配置定义在 `internal/config/config.go`。

> **v3 顶层字段已整体删除**（2026-04-26）：`worker_count` / `agent_max_loops`（顶层） / `llm_base_url` / `llm_api_key` / `llm_model` / `llm_timeout_sec` / `worker_profile` / `explorer_profile` / `scheduler_max_loops` / `mailbox_buffer_size`（顶层） / `mail_chain_max_depth`（顶层） / `compact_token_threshold` / `compact_keep_recent` / `default_concurrency`（顶层） / `fifo_limit`（顶层） / `event_channel_buffer`（顶层） / `default_timeout_sec`（顶层） / `watchdog_interval_sec`（顶层） / `mail_notifier_*`（顶层） / `explorer_*`（除 `agents:` 中的 explorer kind） 等。**旧 setting.yaml 仍可解析**（不报错，未知字段被静默忽略），但这些字段不再产生运行时效果。用户必须改写为下述 v4 块状结构。

## 顶层块（与 `Config` 结构体一一对应）

```yaml
# ==============================================================================
# v4 块（必填或常用）
# ==============================================================================

llm:                                # 全局 LLM 默认值
  base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
  api_key: "sk-..."
  default_model: "qwen3.6-plus"
  timeout_sec: 60

scheduler:                          # Scheduler 是内置单例；模型与运行预算可覆盖
  model: "qwen3-max"

agents:                             # AgentKind 列表 —— 取代 v3 的 worker_count + explorer 二分
  - kind: worker
    replicas: 2                     # 实例化为 worker-1 / worker-2
    profile: "full-access"          # 或 tools: [...]（互斥）
    model: "qwen3.6-plus"           # 可选，per-kind 覆盖 llm.default_model
    system_prompt_file: "prompts/worker.md"
    task_max_retries: 3
    description: "通用任务执行代理，能读写文件、跑命令、发邮件"  # 给 scheduler 看的语义提示
  - kind: explorer
    replicas: 1
    event_type: "explore"           # 只领取此 event_type 的任务
    profile: "read-only"
    model: "qwen3.6-flash"
    system_prompt_file: "prompts/explorer.md"

infra:
  watchdog:
    interval_sec: 30
    pending_alert_grace_sec: 300  # 有合法 route 但 pending 过久：只告警
    unroutable_grace_sec: 300     # 持续无兼容 route：宽限期后 blocked
  mail_notifier: { enabled: true, interval_sec: 5 }
  store:
    event_channel_buffer: 64
    fifo_limit: 100
    default_concurrency: 2
    default_timeout_sec: 300        # 单次 processing 执行的默认超时
  roster:        { wait_timeout_sec: 0 }

# ==============================================================================
# 顶层杂项字段
# ==============================================================================

project_root: "."                   # 路径越界检查基准
max_subtask_depth: 1                # publish_task 子任务最大深度
shell_timeout_sec: 30
progress_notify_enabled: false      # 进度通知（写文件 / 发布子任务 / 任务过半）开关
agent_idle_threshold: 0             # runner 连续空轮询退出阈值；0 = 永不退出
hashline_enabled: null              # 行哈希锚点开关（null = 默认启用）

# Web 检索
search_api_provider: "duckduckgo_html"
search_api_url: ""
search_api_key: ""

# Shell 命令拦截（追加到默认规则）
shell_blacklist: []
shell_greylist:  []

# Tool Profile（命名工具集）
tool_profiles:
  read-only:
    [read_file, list_dir, grep_search, glob_search, web_search, web_fetch, send_message]
  full-access:
    [read_file, write_file, edit_file, list_dir, grep_search, glob_search,
     run_shell, publish_task, send_message, web_search, web_fetch]

# v5 用户 Reactor 配置
reactors_file: "reactors.yaml"      # 空值跳过加载

# Session
session_retention_days: 14
session_archive_max:    50
session_resume_max_idle_sec: 3600   # 已废弃（启动永远新会话、不再自动恢复）；保留仅为配置兼容，设置无效
session_snapshot_interval_sec: 30   # 运行期快照心跳；显式 0 仅保留切换/关闭快照

# 启动期 provider capability probe
startup_probe: "tool"               # tool / tcp / off
startup_probe_timeout_sec: 5
startup_probe_failure_action: "warn" # "warn"（默认）或 "exit"
```

Session 恢复遵守以下安全边界（2026-08 二期「不自动续跑」）：**进程启动永远是全新 Session**——`initSession` 不读 `active-session` 自动恢复，旧请求绝不随启动重跑，`active-session` 指针只服务 trace CLI；进入历史会话只有 `--resume <id>` 与运行时 `/session` 切换两个显式入口，且进入时快照中的非终态任务一律经 no-auto-run 守卫阻断为 `blocked`（清 Agents/PendingSince、撤销 lease；Effect Journal unknown 裁决的任务以更具体的 quarantine 原因优先阻断），该 session 的 Graph 保持停驻（启动期会话模式下全部历史图含无归属图一次性停驻，无恢复入口），续跑由用户提交新提示词驱动。空会话（`TaskCount==0 && FirstUserInput==""`，且快照无任务的双保险）在 Shutdown（`DiscardCurrentIfEmpty`）、切换成功（`DiscardSessionIfEmpty`）与下次启动（`SweepEmptySessions`，崩溃遗留兜底）时删除目录。其余边界：snapshot v4 只重放真实未读邮件，v1-v3 中无法区分已读状态的 mailbox 消息会被丢弃；旧进程的 Roster 文件租约不恢复。动态 Team runner 以稳定 ID 事务性认领快照预建邮箱：route/start 失败会撤销认领并保留原 FIFO 未读队列，恢复完成后仍无人认领的 terminal/stopped/未知 Team 邮箱会在 MailNotifier 启动前丢弃；普通重复注册仍视为装配错误。正常关机先停止 Team runner 使邮箱静止，但把邮箱保留到最终 Session 快照写完后才注销，确保关机—重启不丢未读邮件；最终快照失败会有限重试、保留邮箱并由 `Shutdown` 返回错误。完整关闭流程由系统级 once 串行化，并发调用只等待同一结果。整个恢复/Graph 对账阶段都不安装 Reactor dispatcher，恢复审计只写 Trace，避免用户 Reactor 重新发布任务。`/new` 与真正发生的 `/session` 切换会立即把连续运行时保存到新的 current Session，避免 active-session 指针已切换而快照仍为空或陈旧的崩溃窗口；重复选择当前 Session 是无副作用 no-op。若旧/新快照保存或 Graph/Team 持久化重绑失败，System 会回滚到旧 Session、重新刷新旧快照（失败原地恢复不走 no-auto-run 守卫——会话未真正切走，工作照常继续），并以结果 generation CAS 避免旧结果覆盖切换窗口内的新结果。

## AgentKind 关键字段说明

| 字段 | 说明 |
|---|---|
| `kind` | 唯一标识，scheduler 在 board snapshot 中用以选择派发对象 |
| `replicas` | 该 kind 的实例数；InstanceID 形如 `<kind>-<replicaIdx>`（1-based） |
| `event_type` | 只领取此 EventType 的任务；为空时领取默认队列（worker 通常如此） |
| `profile` / `tools` | 工具集来源，**互斥**。`profile` 引用 `tool_profiles` 中的命名集合；`tools` 直接列举工具名 |
| `model` | per-kind 模型覆盖（空则用 `llm.default_model`） |
| `system_prompt_file` | 必填，提示词文件路径；resolves 相对当前 cwd 或绝对路径 |
| `task_max_retries` | 任务级重试上限（v3 时代的 worker/explorer/scheduler hardcoded constant 已被此字段取代） |
| `enforce_compact_token_threshold` | 已删除；显式设置报迁移诊断，Context v3 按 Snapshot pressure 投影 |
| `description` | 给 scheduler 看的一句话角色描述（拼入 board snapshot 的 `agent_capabilities` 段） |

`internal/config/config.go` 在 v4 之外还保留一个**仅内部使用**的 `AgentRuntimeConfig` 结构，由 `bootstrap.runtime_builder.buildAgentRuntime(kind, replicaIdx)` 合成并注入 `runner.New(rt, deps)`。`AgentRuntimeConfig` 不出现在 YAML 中。

## 配置加载顺序

1. 通过 `-config <path>` 命令行参数获取配置文件路径（默认 `setting.yaml`）
2. `LoadConfig` 按文件后缀（`.yaml`/`.yml`/`.json`）选择解析器
3. 文件不存在时：
   - 显式指定（`-config explicit`）→ 报错终止
   - 默认路径 → 打印 warning 后使用内置默认配置
4. 解析后字段以文件值为准，未指定字段保持默认值
5. `cfg.Validate()` 强制 12 条规则（agents 非空、kind 唯一、profile/tools 互斥、system_prompt_file 必填且可读、replicas ≥ 1、event_type 不重复 explorer 队列、startup_probe_failure_action 合法值等）
6. **单字段命令行覆盖暂未实现**

---

# 关键代码引用速查（v5）

以下是各核心功能的关键代码位置，供开发和调试时快速定位。**行号是参考点，可能随提交漂移；以包名 + 文件名 + 类型 / 函数名为准**。

## Agent 核心

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Agent 结构体 | `internal/agent/agent.go` | `type Agent struct` |
| ReAct 主循环 | `internal/agent/agent.go` | `processTask()` |
| Memory 注入入口（v5） | `internal/agent/memory_context.go` | `injectMemoryContext()` |
| Context replay 投影 | `internal/agent/history_projection.go` + `internal/contextadapter` | Raw History → Snapshot-pressure replay / ContentRef |
| LLMExecutor | `internal/agent/llm_executor.go` | `NewLLMExecutor()` / `Execute()` |
| ToolRegistry | `internal/agent/registry.go` | `type ToolRegistry struct` / `NewToolRegistryWithAllowlist()` |

## Runner（v5 取代 v4 worker / explorer）

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Runner 外壳 | `internal/runner/runner.go` | `type Runner struct` / `New(rt, deps)` / `Run(ctx)` / `Agent()` |
| 依赖注入聚合 | `internal/runner/runner.go` | `type RunnerDeps struct` |
| 工具组装 | `internal/runner/dependency_map.go` | `resolveToolGroups()` |
| CurrentTaskHolder | `internal/runner/holder.go` | `type CurrentTaskHolder struct`（实现 `tools.TaskHolder`） |
| Runtime 合成 | `internal/bootstrap/runtime_builder.go` | `buildAgentRuntime(kind, replicaIdx)` |

## Scheduler

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Bundle 构造 | `internal/scheduler/scheduler.go` | `New()` / `type Bundle struct{ Agent, Activator, Mode }` |
| Activator（事件桥） | `internal/scheduler/activator.go` | `Activator.Run()` —— 消费 `eventCh`，`EventUserInput` → `PublishTask`，task 终态事件 → `BatchUpdateCh` |
| Executor | `internal/scheduler/executor.go` | `SchedulerExecutor.Execute()` / `waitForBatchTerminal()` |
| Board Snapshot | `internal/scheduler/snapshot.go` | `BuildBoardJSON()` / `result_refs` / `progress` |
| Mode 切换 | `internal/scheduler/scheduler.go` | `type ModeStore struct` / `Bundle.Mode` |
| 探针工具 | `internal/tools/scheduler_probe.go` | `probeDirectory()` |

## Tools

| 工具组 | 文件 | 关键类型 | 说明 |
|--------|------|----------|------|
| ToolGroup 接口 | `internal/tools/group.go` | `type ToolGroup interface` | 工具组通用接口，定义 `Register()` 方法 |
| 名称表 | `internal/tools/known_tools.go` | `AllToolNames` | 唯一规范工具名集合 |
| LocalReadGroup | `internal/tools/local_read.go` | `type LocalReadGroup struct` | 只读文件工具：read_file / list_dir / grep_search / glob_search |
| LocalWriteGroup | `internal/tools/local_write.go` | `type LocalWriteGroup struct` | 写入文件工具：write_file / edit_file |
| WebGroup | `internal/tools/web.go` | `type WebGroup struct` | web_search / web_fetch |
| ShellGroup | `internal/tools/shell.go` | `type ShellGroup struct` | run_shell（黑名单硬拒绝；灰名单经 `shell_command` Interaction） |
| MetaGroup | `internal/tools/meta.go` | `type MetaGroup struct` | publish_task / send_message（含 `BatchTracker` 接口供 scheduler 注入） |
| SchedulerGroup | `internal/tools/scheduler.go` | `type SchedulerGroup struct` | Scheduler 专属：cancel_task / get_task_result / report_done / report_progress / probe_directory |

## Gate（v5 统一拦截）

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Phase / Gate 接口 | `internal/gate/gate.go` | `type Gate interface` / `type Phase string` |
| Registry | `internal/gate/registry.go` | `type Registry struct` / `Dispatch(ctx Context)` |
| ToolContext | `internal/gate/tool_context.go` | `type ToolContext struct` |
| MailboxContext | `internal/gate/mailbox_context.go` | `type MailboxContext struct` |
| Mailbox 适配器 | `internal/gate/adapter.go` | `AsMailboxRunner(*Registry) mailbox.MailboxHookRunner` |
| 内置 Gate | `internal/hook/builtin/*.go` | path-boundary / validate-expected-hash / require-read-before-write / dependency-validator / enforce-expected-artifacts / validate-line-anchors / chain-depth-limit / per-agent-dedup / wake-worthy-filter / wake-context-expand |

## Reactor（v5 状态响应）

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Reactor 接口 | `internal/reactor/reactor.go` | `type Reactor interface` |
| Registry | `internal/reactor/registry.go` | `type Registry struct` / `Dispatch(ev trace.Event)` |
| record-artifact | `internal/reactor/builtin/record_artifact.go` | 订阅 `KindFileWritten` |
| task-end-callback | `internal/reactor/builtin/task_end_callback.go` | 订阅 `KindTask{Completed,Failed,Cancelled,Retry}` |
| trace-history-event | `internal/reactor/builtin/trace_history_event.go` | 订阅 `KindHistory{Compaction,Truncated}` |
| read-set-write | `internal/reactor/builtin/read_set_write.go` | 订阅 `KindToolResult`（filter `tool=read_file`） |
| 用户 YAML 加载 | `internal/reactor/userdef/loader.go` | `LoadFromFile(path, projectRoot, deps)` |
| 用户 reactor 实现 | `internal/reactor/userdef/{publish_task,invoke_llm,spawn_agent,call_send_message}.go` | 4 类动作动词 |

## Memory（v5）

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Store 接口 | `internal/memory/memory.go` | `type Store interface` / `type Entry struct` / `Scope` / `Kind` |
| ProcessStore | `internal/memory/process_store.go` | `NewProcessStore()` / 单 RWMutex / 双索引 |

## Spawn（v5 ad-hoc agent）

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Manager（兼 reactor） | `internal/spawn/manager.go` | `type Manager struct` / `NewManager()` / `Spawn(ctx, req)` / `Run(ctx, ev)` / `KindOf(agentID)` / `Shutdown()` |
| 请求结构 | `internal/spawn/types.go` | `type SpawnRequest struct` / `type RuntimeOverride struct` |
| 运行时合成 | `internal/spawn/runtime.go` | base_kind 模板 + override 合成 `AgentRuntimeConfig` |

## Mailbox

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Message 结构 | `internal/mailbox/mailbox.go` | `type Message struct` |
| Mailbox / Registry | `internal/mailbox/mailbox.go` | `type Mailbox struct` / `type Registry struct` |
| MailNotifier | `internal/mailbox/notifier.go` | `type MailNotifier struct` / `Run(ctx)` |
| MailboxHookRunner（最小接口） | `internal/mailbox/hookrunner.go` | `type MailboxHookRunner interface` |
| MailboxHookView | `internal/mailbox/hookview.go` | `HasPendingMail / GetRecentMessages` |

## Store & Model

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Task 结构体 | `internal/model/task.go` | `type Task struct` |
| Event 结构体 | `internal/model/event.go` | `type Event struct` / `EventType` 常量 |
| TaskStore 接口 | `internal/store/iface.go` | `type TaskStore interface` |
| MemoryTaskStore | `internal/store/memory.go` | `type MemoryTaskStore struct` / 依赖感知 FIFO 淘汰 |
| TaskCancelRegistry | `internal/store/cancel.go` | `type TaskCancelRegistry struct` |
| StoreHookView | `internal/store/hookview.go` | 只读视图（reactor / hook 可见） |
| ArtifactLog | `internal/store/artifact_log.go` | 持久化 + replay |
| ReadSet upsert | `internal/store/memory.go` | `UpsertReadSet(taskID, files)` |

## Bootstrap & TUI

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Bootstrap 主入口 | `internal/bootstrap/bootstrap.go` | `Bootstrap(configPath, explicit, skipStartupProbe)` / `type System struct` |
| Start | `internal/bootstrap/bootstrap.go` | `System.Start(ctx, cancel)` |
| Shutdown | `internal/bootstrap/bootstrap.go` | `System.Shutdown()` |
| 启动 banner / probe | `internal/bootstrap/banner.go` / `probe.go` | `printStartupBanner` / `startupProbe` |
| Config | `internal/config/config.go` | `type Config struct` / `LoadConfig()` / `Validate()` |
| TUI 入口 | `internal/tui/app.go` | `Run(ctx, deps)` / `type AppModel struct` / `type Deps struct` |
| TUI 命令分发 | `internal/tui/commands.go` + `internal/ui/commands.go` | `/mode` 等命令执行 + `CommandCatalog()` 单一目录 |
| TUI Interaction 面板 | `internal/tui/interaction.go` / `keymap.go` | `↑/↓` 选择、Enter 提交、RequiresText 文本输入；无裸字母/数字动作键 |

## Trace

| 功能 | 文件 | 关键类型/函数 |
|------|------|---------------|
| Writer | `internal/trace/writer.go` | `type Writer struct` / `Emit(ev Event)` |
| Event 结构（Schema B） | `internal/trace/event.go` | `type Event struct` / `Transition` / `ShellExec` / `ShellTimeout` 子结构 / `Kind*` 常量 |
| Dispatcher | `internal/trace/writer.go` | `SetDefaultDispatcher(reactorReg)` / `DefaultDispatcher()` |
| CLI viewer | `internal/trace/cli.go` | `CLI(args, traceDir, stdout)` —— 启发式异常检测 |
| Prompt Dumper | `internal/trace/prompt_dumper.go` | `AGENTGO_DUMP_PROMPTS=1` 启用
