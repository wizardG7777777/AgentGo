# 代理参考手册（自 AGENTS.md 迁出）

本文件收纳自根目录 `AGENTS.md` 迁出的参考性材料（启动流程、包速览、核心机制详述、工具分组、配置细节、trace 子命令），供编码代理按需查阅；`AGENTS.md` 只保留行为约束与指针。各主题的更深权威源在每节末尾注明；若本文件与权威源冲突，以权威源为准。

## 启动流程（主干）

```
main.go（子命令 trace / config 分流；否则 -config 等 flags）
  └→ bootstrap.BootstrapWithOptions(...)
       ├─ config.LoadConfig + cfg.Validate()      // v4 schema 校验
       ├─ session.NewSessionManager               // .agentgo/sessions/；启动永远新建 Session（不读
       │                                          //   active-session 自动恢复），随后 SweepEmptySessions 清空会话
       ├─ trace.NewWriter + SetDefault            // 活跃 Session 时写入其 logs/
       ├─ store.NewMemoryTaskStore(eventCh, ...)  // 公告板 + cancelRegistry
       ├─ gate.NewRegistry()                      // 注册内置 Gates（经 internal/hook 适配器）
       ├─ roster.NewMemoryRoster()                // 文件级占用登记（花名册）
       ├─ mailbox.NewRegistry / memory.NewProcessStore / interaction.NewService
       ├─ reactor.NewRegistry()                   // 内置 Reactors + spawn.Manager + team.Manager
       │   └─ userdef.LoadFromFile(cfg.ReactorsFile)   // 用户 YAML Reactors
       ├─ trace.SetDefaultDispatcher(reactorReg)  // trace.Emit → Reactor.Dispatch
       ├─ scheduler.New(...)                      // Bundle{Agent, Activator, Modes}
       ├─ watchdog.New(store, cfg, eventCh, roster)
       └─ runtime_builder.buildAgentRuntime(kind, replicaIdx) → runner.New(rt, deps) × Σ(kind.Replicas)
  └→ sys.Start(ctx, cancel)   // Activator / Scheduler Agent / Watchdog / MailNotifier / Runners
  └→ sys.RunCLI(ctx)          // TUI 或 headless（web-only）
  └→ sys.Shutdown()
```

`Start` 时装配 SIGINT 哨兵（`sentinel.go`）：headless 下第一次信号优雅取消、3 秒内第二次强制 exit(130)、5 秒关闭期限后 exit(1)。

> 权威源：`Archtechture.md`「系统启动流程」节（含 Bootstrap 阶段逐步表格与启动顺序约束）。

## 包速览

| 包 | 职责 |
|---|---|
| `internal/agent` | Agent 结构、ReAct 循环、TaskExecutor、ToolRegistry、LLMExecutor、FileStateCache、3 层历史压缩、processTask 入口 Memory 注入 |
| `internal/bootstrap` | 系统装配与启动编排；`runtime_builder.go` 由 `AgentKind` 合成 `AgentRuntimeConfig`；SIGINT 哨兵；Graph 桥接（`graph_*.go`） |
| `internal/config` | YAML/JSON 配置加载，仅支持 v4 schema；v3 顶层字段静默忽略 |
| `internal/dashboard` | Web 前端：内嵌 SPA、`/api/snapshot`、`/api/events` SSE、Interaction 响应端点；Bearer/?token= 鉴权，非 loopback 强制 token |
| `internal/effect` | V6 Effect Journal：副作用 prepared/settled/unknown append-only 账本，ReplayPolicy 声明，启动 Recover 按策略裁决 |
| `internal/gate` | 统一 Gate 注册表。Phase 路由：`tool:preCall` / `tool:postCall` / `mailbox:beforeSend` / `mailbox:beforeDeliver` / `mailbox:beforeWake` |
| `internal/graph` | V6 JSON Graph：GraphDocument 即执行契约（校验/digest）、GraphStore 持久化（snapshot+journal/Recover）、Runtime 引擎（activation 模型、十类节点） |
| `internal/hook` | 遗留 Hook 接口与内置实现，现作为 Gate 的适配层（内置 Gate 实现存放于此） |
| `internal/interaction` | 结构化人机交互协议：两阶段 CAS 状态机；graph_approval / Shell / agent_question 共用同一 Service。详见 `docs/design/interaction.md` |
| `internal/llm` | LLM 客户端接口 + openai-go 实现；`Message.ExtraFields` 透传非标准字段；错误分型 Recoverable / Unrecoverable / BadResponse |
| `internal/mailbox` | 代理间直发消息（`send_message`）、MailNotifier、16 条环形缓冲、TeamSnapshot |
| `internal/memory` | v5 Memory：ScopeProcess / ScopeSession（`memory.jsonl`）/ ScopeProject（预留）；CM3：`Entry.State`（confirmed/inferred/stale/superseded）、`Supersede`/`MarkStale`/`Delete`、晋升规则 |
| `internal/model` | `Task` / `Event` / `Claim` 数据结构与状态机 |
| `internal/modes` | 两轴模式 Store：exec（`normal`/`strict`/`readonly`/`yolo`）、topo（`team`/`solo`），运行时 `/mode` 或 Shift+Tab 切换（gate 轴已于 V6 整体移除） |
| `internal/pathutil` | 路径穿越防护 + 敏感文件模式拦截 |
| `internal/probe` | 启动期能力探测（如 web_search / web_fetch 可达性） |
| `internal/prompt` | V6 §2 Prompt 有序编译：system prompt 与首条 user 消息编译为带身份的冻结 Build（现阶段只用于身份与观测） |
| `internal/reactor` | Reactor 注册表（Name/Subscribe/Run/IsSync/Priority），按 `trace.Event.Kind` 扇出；`userdef/` 为用户 YAML Reactor 加载器 |
| `internal/roster` | 文件级占用：原子 `TryClaim` / `Release` / `ReleaseAll` / `IsOccupied` |
| `internal/runner` | 统一执行代理外壳（取代已删除的 `internal/worker`、`internal/explorer`） |
| `internal/scheduler` | Scheduler 是一等 `agent.Agent`（`EventType="__scheduler__"`）；`Activator` 是 eventCh 唯一消费者 |
| `internal/session` | Session 生命周期：history.jsonl、公开 LLM 轮次 `turns.jsonl`、快照、回放、归档、保留策略 |
| `internal/shell` | Shell 命令过滤（黑/灰/白名单）+ `shell_command` 授权 Interaction；POSIX 走 `sh -c`，Windows 走 PowerShell |
| `internal/spawn` | Spawn Manager：从 `base_kind` 物化一次性 ad-hoc Runner，初始任务终结即拆除 |
| `internal/store` | `TaskStore` 接口、`MemoryTaskStore`、TaskCancelRegistry、ToolCallRecord、ArtifactLog 回放、ReadSet |
| `internal/suggest` | Did-You-Mean（`sahilm/fuzzy` 封装） |
| `internal/taskmem` | V6 Task Memory（CM2）：有界、版本化的滚动工作状态；`ApplyTurn` 只消费结构化 TurnFacts，按任务原子落盘 |
| `internal/team` | 按模板供给代理团队；Graph-first Team 绑定 `graph:<id>` 并监听 `graph_ended` 回收，legacy Team 才绑定 controller task |
| `internal/agenttemplate` | 代理模板目录（内嵌 builtin prompts） |
| `internal/tools` | 统一 ToolGroup 架构；`known_tools.go` 的 `AllToolNames` 是工具名权威清单 |
| `internal/trace` | 任务级 JSONL trace（Schema B fat struct）；CLI 查看器；`SetDefaultDispatcher` 扇出到 Reactors |
| `internal/tui` | Bubble Tea TUI；两层渲染（inline scrollback 主态 + Graph/详情/结果全屏层）；键位事实源在 `keymap.go` |
| `internal/ui` | TUI/Web 共享 Hub、安全快照与斜杠命令目录；轮次历史不受有界诊断 feed 淘汰 |
| `internal/output` | 类型化输出通道事件（文本 / 流式快照 / 不可变完成轮次 / 任务结果） |
| `internal/watchdog` | 周期健康检查、级联取消、roster 清理、超时告警（只告警不杀死）、panic 自动重启 |
| `internal/webtool` | Web 搜索 + URL 抓取，SSRF 防护，搜索后端可插拔 |
| `internal/workspace` | 按任务写时复制执行隔离：Manager / View（overlay）/ Swapper；`types.go` 为冻结契约 |

> v5 已删除（不要再找）：`internal/cli`、`internal/worker`、`internal/explorer`。V6 已删除：`internal/plan` 整包。
>
> 权威源：`Archtechture.md`「关键代码引用速查」节（按子系统列出文件与入口）。

## 核心机制（详述）

- **接口驱动**：`TaskStore` / `Roster` / `mailbox.Registry` / `memory.Store` / `gate.Registry` / `reactor.Registry` / `llm.Client` 均为接口 + 内存实现；无全局状态，全部经 `runner.RunnerDeps` 或 `scheduler.New` 注入。
- **事件驱动调度**：Store 以非阻塞发送（缓冲 64）发出任务事件；`Activator` 消费，`EventUserInput` 转为 `__scheduler__` 任务。
- **按任务取消**：`TaskCancelRegistry` 给每个任务挂可取消 context，任务进入终态即取消，执行中代理经 `ctx.Done()` 立即感知。
- **错误分型**：`llm.ErrRecoverable`（429/5xx）与 `ErrBadResponse`（length 截断）桥接为 `agent.ErrRecoverable` 触发重试回滚；`ErrUnrecoverable`（401/403）直接失败任务。
- **Kind 即配置**：`setting.yaml` 的 `agents[*]` 声明 kind + replicas + `profile`/`tools` + model + prompt；Bootstrap 按 kind×replica 建 Runner。运行时没有 `Agent.Kind` 枚举分支。
- **工具按 allowlist 剪枝**：`runner.resolveToolGroups()` 注册全部 ToolGroup，`ToolRegistry` 按 `AllowedTools` 在注册时剪掉未授权工具；任务级再经 `publish_task` 的 `tools` 参数二次裁剪——provision 时 kind 白名单是天花板，plan 时节点可声明更小的子集，认领后当次生效。新增工具必须同步 `internal/tools/known_tools.go`。
- **ExecutionLease（V6 §4 H1）**：任务首次被认领时按 `Lease = NodeRequirement ∩ RouteCeiling ∩ Policy` 冻结当次执行契约（挂 `Task.Lease`，随快照持久化）。`Capability.Tools` 未声明走**合成节点能力**规则（`Synthetic=true`，需求 = 认领方 Route 白名单全量，取代旧「隐式继承 kind 全集」语义）。只有 `GraphID==""` 的非图 scheduler 控制面保留 `BusinessTools=nil` 的记录型租约；Graph controller 是纯控制面——业务工具面恒为空集（无论认领方与 capability 声明，图校验期也拒绝 controller 声明 `capability.tools`），工具面只剩控制通道（2026-08-19 起，堵 scheduler 借 controller 节点越权执行）。Policy：exec=readonly 剔除写工具；exec=strict 记 `ApprovalRequired=true`；控制通道按持久化 `Task.GraphNodeKind` 派生（Graph controller/agent 为 `{submit_task_result, request_replan}`，acceptance 为 `{submit_task_result}`，非图任务为 `{submit_task_result}`，scheduler 为 `{report_done}`）。`GraphNodeKind` 随 TaskSpec 与 Session 快照持久化，恢复时非空 kind 与 activation 冻结定义不一致即 fail-closed；旧快照 kind 为空及未知未来类型一律按最小权限只授予 `submit_task_result`，不从可自定义 route 猜测。新计算、直接复用和并发冻结返回的 Lease 都做角色级复核：ControlTools 必须精确等于角色集合，acceptance/空 kind/未知 kind 的 `ToolUnion` 必须落在 read/list/grep/glob/web/submit 正向闭集；任何 Graph Task 即使 legacy Lease 的 `BusinessTools=nil` 也按 `ToolUnion` 换入，不能把 nil 解释为注册全集。生命周期：首认领冻结（`execution_lease_frozen`）→ 重认领复用（工具面不变）→ 终态撤销（此后工具 dispatch 一律拒绝，与 finalizing fence 互补）。per-node 工具换入由 Lease 驱动：registry 视图 = `BusinessTools ∪ ControlTools`（漏带控制工具也能收尾）；显式声明越界 fail-closed 走 `capability_violation` 路径。实现：`internal/agent/execution_lease.go` + store 可选接口 `FreezeTaskLease`/`RevokeTaskLease`。
- **公告板**：发布 → pending；`QueryAvailable(eventType)` 按优先级轮询；`ClaimTask` 原子转 processing（查依赖——统一须 completed——与并发上限，落锁前 capability checker 叠加检查节点工具子集）。终态 `Task.Results` 完整保留，board 快照只带预算内摘录；`Task.Artifacts`（实际产出，record-artifact Reactor 追加，shell 写产物经 `KindShellExecuted` 补登）≠ `Task.ExpectedArtifacts`（声明契约，Gate + 终止时校验，含磁盘兜底防重试换任务 ID 后账本失忆空转）；`Task.ReadSet` 由 read-set-write Reactor 维护。
- **Roster 花名册**：`write_file`/`edit_file` 先 `TryClaim`，被占用时返回含「占用」与占用者 ID 的错误；Watchdog 清理不再活跃代理的 claims；Roster 监听器写 `file_awareness` 到 Memory。
- **Gate**：`Decision.Action ∈ {Continue, Abort}`；同 Phase 内按 Priority 升序；panic 恢复为 Continue；nil Registry 一律 Continue。内置 11 个：tool:preCall 7 个（exec-mode-guard / path-boundary / validate-expected-hash / require-read-before-write / dependency-validator / enforce-expected-artifacts / validate-line-anchors）、beforeSend 1 个（chain-depth-limit）、beforeDeliver 1 个（per-agent-dedup）、beforeWake 2 个（wake-worthy-filter / wake-context-expand）。
- **Suggestions（V6 §4 H2a）**：Gate Abort 可携带结构化恢复提示——`Decision.ReasonCode`（码集中在 `internal/hook/builtin/reason_codes.go`）+ `Suggestions`（稳定 ID、retryable、有界 ≤3；类型在 `internal/hook/suggestion.go`，gate 包 alias 复导出），Harness 注入为结构化观察文本（建议不自动执行）。过滤纪律：不可重试 / finalizing 只给升级标记（user/blocked/replan/switch_mode）；同一建议 ID 任务内第 3 次熔断，改指引 blocked/replan。trace：`suggestions_returned` / `suggestion_disposition`。无结构化字段的拒绝仍走旧 `[hook 拒绝]` 文本路径。
- **Effect Journal（V6 §4 H2b）**：副作用统一账本（`internal/effect`，append-only JSONL，每 append flush+恰好一次 fsync）。埋点：`write_file`/`edit_file`（verify_first）、`run_shell`/`send_message`（manual_only）、workspace 合并（never_replay）；先 Prepare 再执行，完成后 Settle/失败标 unknown；完整参数/命令/正文不落账。启动 Recover：prepared 未 settled 一律标 unknown 再按策略裁决——verify_first 比对盘上 hash，一致转 settled；其余策略不自动执行任何动作（V6 红线：未知不得静默重跑）。nil 或落账失败只告警降级、绝不阻断副作用。trace：`effect_prepared` / `effect_settled` / `effect_unknown` / `effect_recovery_decided`。
- **Reactor**：同步 Reactor 在 `trace.Emit` 调用方 goroutine 串行执行；普通异步 Reactor 走有界、满载丢弃的观测 lane。`ReliableAsyncReactor` 供低频控制面（graph-terminal-feed / Team 生命周期）使用：脱离 Emit 调用栈，进入不丢弃的专用单 worker FIFO。内置：record-artifact / task-end-callback / trace-history-event / read-set-write / runtime-anomaly / graph-terminal-feed / session-memory-promotion，外加 spawn.Manager 与 team.Manager。用户 YAML Reactor 支持 `publish_task` / `invoke_llm` / `spawn_agent` / `call: send_message` 动作与 `when:` 条件；**永远异步**；Reactor 不得直接驱动状态迁移（无 `SetState`），只能发任务/消息让主循环自然迁移。
- **Graph（V6）**：JSON `GraphDocument` 经 `ParseAndValidate` 校验后即为执行契约；节点每次进入创建单调 activation（`<nodeID>@<n>`，回边重进 = 新 activation + 新任务），activation 事实/任务发布/边选择全部 durable（`.agentgo/state/graphs/` snapshot+journal），崩溃后 `Recover` + `ResumeGraph` 幂等补发（board 以 `(graph_id, activation_id)` 去重）。桥接在 `internal/bootstrap/graph_*.go`：`graphBoard` 发布带 `GraphID/NodeID/ActivationID` 的任务，`graph-terminal-feed` 把任务终态与结构化 `result` object 回填 `Runtime.OnTaskTerminal`。**数据流**：节点终态先把完整 Result/Evidence 写入 activation 级 durable Result Store；每条实际生效转移再持久化稳定 `ResultRef`、≤32KiB 内联值、目标输入端口与完整结构化 EvidenceEntry（`TransitionRecord.Input` → `Execution.Input`）。下游任务自动注入「## 上游输入」；发布时可按 ResultRef 展开大结果，但跨来源总注入仍有界，超大正文应走 artifact。EvidenceRef 按 CallID+调用内容或 artifact 身份稳定生成，展示序数不参与身份。acceptance 为发任务型节点（默认 `acceptance.verify`）：`required_inputs` 端口齐备后才发布；`required_evidence` 缺口会显式注入，verifier 应 blocked，若仍自报 pass，Runtime 强制 blocked。`cited_evidence` 只做输入谱系核验，越谱系即 `disputed`。合法跨任务回边不设 activation 上限；emergency fuse 只限制一次 Runtime 调用内 router/join 等不让出控制权的同步机械级联，触发时图 durable failed 并唤醒 Scheduler。approval、wait_event、只读 tool 节点及 Graph change 唤醒保持既有语义。
- **Graph 单赋值拓扑基线**：当前没有 flow generation/correlation token，所有非 join/acceptance 节点最多一条静态入边；join / acceptance 的每个 `target_input` 也只能有一条生产边。并行 AND 使用不同端口；条件分支必须各自保留后续与 `end`，不得共享端口或重新汇入同一普通节点。循环体可直接作为 root，由 root 的隐式初始 activation 加唯一回边重复进入；不要再额外添加 start → root 形成第二条静态入边。复杂 OR mux 与跨代汇流待 generation/correlation token 落地后再开放。join Result 以端口名归并，成功终态事件固定回落为 `completed`，所以 `join → summarize` 必须匹配 `completed`。
- **Acceptance 路由契约**：acceptance 必须有非空 `task.title` 与写明逐项验收标准的非空 `task.description`。verifier 的业务 verdict 只写 `submit_task_result.verdict`，枚举为 `pass` / `fixable` / `failed`；completed 结果必须省略 `event`，业务分支只允许 `{path:"$.verdict", operator:"eq", value:...}` 精确匹配。authoring 拒绝 acceptance 的无条件、`always`、`completed`、`pass`/`fixable` 事件出边；Runtime 自身的 `failed` / `blocked` 事件仅作兜底。证据或能力不足时 verifier 提交 `status=blocked`；`disputed` 是 Runtime 的谱系核验状态，不是 verifier 可提交的 verdict。自定义 path 字段必须由上游 `submit_task_result.result` 提交，不能藏在 summary/event。
- **Graph-scoped Team**：provision 前由 Scheduler 决定合法 `graph_id` 并显式传入；Team route owner 为 `graph:<id>`，`graph_ended` 才回收（省略 `graph_id` 的 legacy provision 才绑定 controller task）。Graph Task 持久化 `RouteScope`，`QueryAvailable`/`ClaimTask` 用 `CanRouteForPlan` fail-closed 校验。Graph controller 只能 read/patch 自身 Graph（禁止新 submit、脱图 `publish_task`、`report_done`）；虽路由为 `__scheduler__`，节点收尾仍必须用 `submit_task_result`，非图 Scheduler task 才用 `report_done`。终态 Graph 先 durable 取消 sibling 节点，再经 `GraphTaskTerminator` 取消公告板 Task，最后发 `graph_ended`。内联 subgraph 不继承父 Graph 私有 Team scope，只能用全局静态 route。
- **Interaction**：`pending → resolving → resolved` 两阶段，`BeginResolve` 以 `expected_version` 做 CAS；另有 cancelled/expired/failed/interrupted 终态。Interaction 只拥有「用户选择」事实，不拥有 Graph/Shell 执行事实；前端提交 `request_id + expected_version + option_id + text`。
- **Modes 两轴**：gate 轴（规划门控）已于 V6 整体移除——执行前审阅经 Graph approval 节点；`modes.gate` 设任何非空值（含 `immediate`）报迁移诊断。exec=readonly 由 exec-mode-guard Gate 硬拒写工具，strict 逐次审批写与 shell，yolo 灰名单自动放行（黑名单仍硬拒）；topo=solo 禁止 Scheduler `publish_task`，亲自执行。
- **Memory**：processTask 入口查询 `ScopeProcess/KindContext` 的 `team_snapshot`、`file_awareness` 注入；nil-safe。
- **Task Memory（CM2）**：processTask 入口加载或创建；settled Turn 收口从 ToolCallRecord 账本 + file_written hash + Artifacts 增量收集 `TurnFacts` 滚动更新（仅实质变化落盘/发 `task_memory_updated`）；L2 压缩前、Attempt 结束前、任务终态强制 checkpoint（终态置 Sealed）；注入经 ctx 载体插入 user 首条之后并登记 Manifest `task_memory` 段；nil 或落盘失败降级不阻断。
- **Session Memory（CM3）**：`session-memory-promotion` Reactor 订阅四种任务终态，从终态 Task Memory 经 `memory.BuildPromotionCandidates` 按终态规则筛选（inferred 一律丢弃），经 `SessionStore.Supersede` 写入（同 Key supersede 旧条目）；幂等标记 `TaskMemory.PromotedAt`（每 Task 终态最多一次）。召回在 processTask 入口（仅 RetryCount=0 注入）：过滤 stale/superseded、inferred 标注「未验证」，有界渲染（≤1200 runes）为 `<session-memory>` 块注入，Manifest 登记 `session_memory` 段。
- **3 层历史压缩**：L1 snip 旧工具输出为结构化墓碑（无 LLM 调用）；L2 按“自上次压缩以来”的 prompt token 累计周期检查，超过 `CompactTokenThreshold` 就摘要压缩并重置周期（上一轮摘要必须继续折叠进新摘要）；L3 context 溢出时激进压缩（keepRecent=1）。
- **公开轮次账本**：每次 Agent/Scheduler `TaskExecutor` 调用使用稳定轮次 ID；`KindStream` 只原位更新在途文本，返回后发布唯一 `KindTurn` 并 append+fsync 到当前 Session 的 `turns.jsonl`。账本只含公开 assistant 正文、工具名和终态错误；TUI/Web 从 `Snapshot.Turns` 恢复全部 Loop。
- **FileStateCache**：按 Runner 的 LRU（容量 50），Get 时 `os.Stat` 比对 mtime+size 再验证（跨代理写透明失效）；写工具经 Roster 路径时失效。
- **乐观并发**：`write_file`/`edit_file` 接受 `expected_hash`，SHA256 不符即返回「冲突」错误。
- **路径安全**：`pathutil.ValidatePath` 强制项目根边界并拦截敏感文件（.env、.ssh 等），工具内与 path-boundary Gate 双重执行。
- **SSRF 防护**：`webtool.validateURL` + `isPrivateOrLoopback` 拦截内网/回环地址。
- **Session 隔离（2026-08，取代 B3 连续语义；二期修订「不自动续跑」）**：session 是完整运行时隔离边界。`/new`、`/session [id]`（TUI 无参打开选择面板）、`/new force` 统一走「冻结 → 切换 → 解冻」（`internal/bootstrap/session_switch.go`，全程持 `snapshotMu`）：冻结 = 公告板静默（`store.EnterQuiesce`，11 个迁移入口 fail-closed）→ ctx 全取消 → team `SuspendAll`（spec 保持 ready）→ spawn 拆除 → Roster 清空 → 归档快照 → 中断 pending Interaction → 注销挂起 team 邮箱；解冻 = 快照非终态任务经 no-auto-run 守卫全部阻断为 `blocked` → `store.ReplaceSnapshot` 整体换板（零事件）→ 邮箱 `ClearAllMessages` + 快照重建（静态保留注册、team 走 recovered 认领）→ team 重新 `Start` 物化 → **Graph 保持停驻**（僵尸图无恢复入口）→ 观测面归零。终止变体（force）先 `TerminateAll` + `CancelAllNonTerminal` 再归档。**启动永远是全新 Session**（不读 `active-session` 自动恢复；`--resume` 与 `/session` 是进入历史会话的唯二入口，进入即按上述守卫阻断）；空会话（`TaskCount==0 && FirstUserInput==""`）在退出/切走/启动清扫时丢弃。Graph 按 `session_id` 归属（不进 digest，旧 JSON 单向兼容）；启动期全部历史图（含无归属图）停驻、恢复仅无 Session 模式全量执行、终态唤醒与审批补登记按会话过滤；UI 图列表经 `graphViewsForUI` 投影层按当前 session 过滤（一个 session 看不到另一个 session 的图）。冻结 session 的 workspace 经 Watchdog 豁免集免被孤儿清扫（启动时从非活跃快照重建；解冻阻断后清除）。可恢复窗口 = `session_retention_days`，逾期归档仅作历史审计。

> 权威源：`Archtechture.md`（主架构与各子系统）、`docs/activate/ReactiveSystem.md`（Gate/Reactor）、`docs/activate/MemoryManageSystem.md`（Memory）、`docs/design/interaction.md`（Interaction）、`docs/design/session-isolation.md`（Session 隔离）、`docs/design/workspace-isolation.md`（workspace）、`docs/design/per-node-capability.md`（per-node 能力）、`docs/nextUpgrade-V6.md`（V6 机制）。

## agent 自报 blocked 与 finalizing fence（V6 §5）

`submit_task_result` 接受可选 `status`（缺省 `completed`；`blocked` 需同填 `blocked_reason`，failed/cancelled 不接受自报）与可选 `result` JSON object。`result` 字段类型保真展开到 Graph Result 顶层供 `$.path` 条件与下游消费；`event`/`verdict`/`cited_evidence` 仍走专用参数，但 acceptance completed 只使用 `verdict` 且必须省略 `event`。提交被接受即进入 finalizing——后续工具调用被 fence；自定义 carrier、专用路由字段、正文与 completed/blocked 终态在同一 Store 临界区原子落盘，失败时任务 fail-closed。trace：`task_finalizing` / `task_result_committed`。

## 工具分组

权威清单见 `internal/tools/known_tools.go`。

| 组 | 工具 |
|---|---|
| LocalRead | `read_file` `list_dir` `grep_search` `glob_search` |
| LocalWrite | `write_file` `edit_file` |
| Web | `web_search` `web_fetch` |
| Shell | `run_shell` |
| Meta | `publish_task` `send_message` `request_user_input` |
| PlanControl | `submit_task_result` `request_replan` |
| Scheduler 专属 | `cancel_task` `get_task_result` `report_done` `report_progress` `probe_directory` |
| AgentTemplate（Scheduler 专属；2026-08-20 起 `agent_templates.enabled` 缺省 `false`，默认搁置不注册） | `list_agent_templates` `provision_agent_team` |

普通 Runner 的工具来自 `tool_profiles` 或 `agents[].tools`；Scheduler 专属组不走 profile。`publish_task` 另接受可选 `tools`（逗号分隔子集）、`model` 与 `isolation`（唯一合法值 `"workspace"`）参数（仅 Scheduler 计划控制面可设置），为单个 DAG 节点声明能力覆盖（`model.NodeCapability`）：认领 Runner 当次换入过滤后的工具视图并临时替换模型，任务结束恢复；`isolation:"workspace"` 节点在写时复制 overlay 中执行（写落 `.agentgo/workspaces/<taskID>/`），成功终态自动合并回主根，冲突 → failed + 通用 replan 唤醒（`docs/design/workspace-isolation.md`）。节点工具集必须 ⊆ 某条现存路由的白名单——`QueryAvailable` 按认领方过滤 + `ClaimTask` 落锁前 capability checker 叠加检查（fail-closed），越界任务对所有 Runner 不可见，滞留至 Watchdog `claim_starvation` 告警后由 Scheduler 修复。

> 权威源：`internal/tools/known_tools.go`、`docs/tool-profiles.md`、`docs/design/per-node-capability.md`。

## 配置细节（v4 schema，唯一支持的格式）

`setting.yaml`（或 `.json`）顶层块：`llm:`（base_url/api_key/default_model/timeout_sec/reasoning_effort/stream）、`scheduler:`（仅 model 可覆盖）、`agents:`（kind/replicas/profile 或 tools/model/system_prompt_file/task_max_retries/enforce_compact_token_threshold/event_type/description）、`infra:`（watchdog/mail_notifier/store/roster）、`modes:`（exec/topo 两轴；`gate` 键已移除，设任何非空值报迁移诊断）、`ui:`（frontends: [tui, web]，web.listen/token）、`tool_profiles:`、`reactors_file:`，以及杂项（`project_root`、`max_subtask_depth`、`shell_timeout_sec`、`shell_blacklist`、`shell_greylist`、`session_retention_days` 等）。完整示例见 `config.example.yaml`。

> V6 移除的配置字段：`agent_max_loops`（Loop 不再按固定轮数终止，由结构化终态、取消、deadline、预算与不可配置的 emergency fuse（10000，触发即 blocked + replan）共同约束）与 `context_limit`（上下文适配由历史压缩与溢出重试承担）已删除，显式设置报迁移诊断。

> V6 移除的代码：`internal/plan` 整包与 `model.Plan` 全部类型、验收四工具（`define_acceptance_spec` 等）已删除——验收语义由 Graph acceptance 节点 + `submit_task_result.verdict` 契约承担；`Task` 的 Plan 系字段、trace 的 `KindPlan*` 常量与 UI 残留展示均已清理。

> v3 顶层字段（`worker_count`、顶层 `llm_base_url` 等）已移除并**静默忽略**：解析成功但无运行时效果。

> 权威源：`config.example.yaml`（全注释示例）、`docs/yaml-config-guide.md`、`docs/config-consolidated-reference.md`。

## trace 子命令（调试主入口）

任务执行过程以 JSONL trace 落盘，同一个二进制的 `trace` 子命令是排查任务行为的首选工具（不启动主系统）：

```bash
./agentgo trace list                    # 最近的任务（按发布时间倒序）
./agentgo trace show <task_id>          # 按时间序展示某任务全部事件；task_id 支持唯一前缀
./agentgo trace stats [task|agent]      # 聚合 LLM 调用与 token 消耗（默认按 task，按总 token 降序）
./agentgo trace graph [graph_id]        # 无参列出全部已知图；带参展示图生命周期事件；graph_id 支持唯一前缀
./agentgo trace node <graph_id>/<node_id>   # 单节点事件视图，按 activation 分组（回边重进一目了然）
```

- trace 文件位置：Session 活跃时在 `.agentgo/sessions/sess-<id>/logs/`，否则回退 `.agentgo/traces/`；子命令自动解析活跃 Session。实时跟踪：`tail -f .../logs/<文件>.jsonl | jq`。
- 文件命名 `<时间戳>_<task_id前8位>.jsonl`；重试可能跨物理分片，CLI 按完整 TaskID 重新归组。`.prompts.jsonl` 是 prompt dump。Graph 事件落 `graph_<graph_id前8位>.jsonl` 分片，`trace graph/node` 跨分片归并，覆盖度标记 complete / partial / degraded。
- 事件身份：`session_id` 由 Writer 盖戳，`invocation_id` 关联同一轮 LLM 调用事件；默认脱敏（自由内容 `<redacted>`，`AGENTGO_TRACE_FULL_ARGS=1` 旁路）；写失败降级落 `trace_degraded.marker`，恢复自动清除。

> 权威源：`TraceGuide.md`（命令、事件字段、异常检测与排错场景完整参考）。
