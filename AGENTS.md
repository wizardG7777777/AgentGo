# AGENTS.md

本文件为在本仓库中工作的编码代理提供项目说明与项目约束。

## 项目

AgentGo 是一个 Go 多智能体任务编排系统。Scheduler（由 LLM 驱动的 `agent.Agent` 实例）把用户请求分解为任务，发布到公告板（`MemoryTaskStore`）；**Runner** 执行代理（`internal/runner`，按 kind 在 `setting.yaml` 中声明）轮询并认领任务。每个代理运行带工具调用的 ReAct 循环，具备 3 层历史压缩与按代理的文件缓存。Watchdog 监控健康；代理间通过 Roster（文件级占用登记）协调写文件、通过 Mailbox 互发消息。

**v5 反应式体系**（2026-05 起）用两个子系统取代了 v4 Hook 体系：

- **Gate**（`internal/gate`）— 动作前决策点（可 Abort），统一拦截 Tool / Mailbox / Shell 调用。
- **Reactor**（`internal/reactor`）— 状态变更后响应器（不可 Abort），含开发者注册的 Go Reactor 与用户声明式 YAML Reactor。

语言：Go 1.25 ｜ 模块名：`agentgo` ｜ 配置：`setting.yaml`（YAML/JSON，v4 schema，见下文） ｜ LLM：OpenAI-compatible Chat Completions 统一请求路径，经 `internal/llm` 实现（V6 起不再按 provider 分支适配，`llm.provider` 字段已移除）。

## 构建、测试与调试命令

```bash
go build                          # 构建（产出 ./agentgo 或 .\agentgo.exe）
go test ./...                     # 全部测试
go test ./internal/store/         # 单包测试
go test -run TestName ./internal/agent/   # 单个测试
./agentgo -config setting.yaml    # 启动（另有 -skip-startup-probe、-resume <sessionID>）
./agentgo config doctor           # 校验配置 + prompt 承诺工具与实际 allowlist 对账（error/warning/info 分级）
./agentgo-eval preflight          # 行为评测凭证前置检查：env 变量注入 + LLM 密钥真实端点探测（eval/ 资产不入库；先 go build -o agentgo-eval ./cmd/agentgo-eval）
./agentgo-eval run [-smoke]       # 跑黄金任务套件并与基线对比（eval/suite.yaml，本地资产不入库；-binary 指定被测二进制，默认探测 ./agentgo）
./agentgo-eval record             # 跑整套件并录制基线候选（eval/baseline.candidate.json）
./agentgo-eval promote            # review 后把候选晋升为 accepted baseline（失败/不完整候选拒绝晋升）
./agentgo-eval offline            # 离线 fake-LLM E2E：脚本化假端点驱动真实主链，断言 trace 事实与禁止行为（eval/offline.yaml）
```

### trace 子命令（调试主入口）

任务执行过程以 JSONL trace 落盘，同一个二进制的 `trace` 子命令是排查任务行为的首选工具（不启动主系统）：

```bash
./agentgo trace list                    # 最近的任务（按发布时间倒序）
./agentgo trace show <task_id>          # 按时间序展示某任务全部事件；task_id 支持唯一前缀
./agentgo trace stats [task|agent]      # 聚合 LLM 调用与 token 消耗（默认按 task，按总 token 降序）
./agentgo trace graph [graph_id]        # 无参列出全部已知图；带参按时间序展示图生命周期事件；graph_id 支持唯一前缀
./agentgo trace node <graph_id>/<node_id>   # 单节点事件视图，按 activation 分组（回边重进一目了然）
```

- trace 文件位置：Session 活跃时在 `.agentgo/sessions/sess-<id>/logs/`，否则回退 `.agentgo/traces/`；子命令自动解析活跃 Session。
- 文件命名 `<时间戳>_<task_id前8位>.jsonl`；重试可能跨物理分片，CLI 会按完整 TaskID 重新归组。`.prompts.jsonl` 是 prompt dump，不算任务文件。
- Graph 生命周期事件落 `graph_<graph_id前8位>.jsonl` 分片（与任务分片同目录；分片名中 `/`、`:` 等路径敌对字符消毒为 `~`，writer 与 CLI 共用 `graphShardFileName` 对齐）；`graph_change_requested` 携 TaskID 落任务分片，`trace graph/node` 按事件 GraphID 跨分片精确归并。头部 revision/state_version/digest 取自 `.agentgo/state/graphs/<graph_id>/snapshot.json`（CLI 侧本地最小结构解码——trace 包不得 import internal/graph），缺失时由事件重建；覆盖度标记 complete / partial（分片缺失或有坏行）/ degraded（snapshot 不可读）。
- 实时跟踪最新任务：`tail -f .agentgo/sessions/sess-<id>/logs/<文件>.jsonl | jq`。
- V6 §7：事件身份——`session_id` 由 Writer 集中盖戳（`SetSessionID`，Emit 时为空才补），`invocation_id`（`<taskID前8>-<loop>-<seq>`）关联同一轮 `llm_call_start`/`llm_call_end`/`context_manifest_built`；默认脱敏（`internal/trace/redact.go`，工具事件 Args 与 shell 命令过 `RedactArgs`/`RedactShellCommand`——结构字段保留、自由内容 `<redacted len=N sha256=前12>`，`AGENTGO_TRACE_FULL_ARGS=1` 旁路）；写失败降级——Writer 首次连续写失败落 `trace_degraded.marker` + `OnDegraded` 回调（log + UI status），恢复自动清除，`trace list/show` 检测 marker 在 header 打 `trace_degraded` 提示。

无 Makefile / linter，只用标准 Go 工具链。测试假定 LF 行尾（`.gitattributes` 强制）。

## 架构

### 启动流程（主干）

```
main.go（子命令 trace / config 分流；否则 -config 等 flags）
  └→ bootstrap.BootstrapWithOptions(...)
       ├─ config.LoadConfig + cfg.Validate()      // v4 schema 校验
       ├─ session.NewSessionManager               // .agentgo/sessions/, history.jsonl + turns.jsonl
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

### 包速览

| 包 | 职责 |
|---|---|
| `internal/agent` | Agent 结构、ReAct 循环、TaskExecutor、ToolRegistry、LLMExecutor、FileStateCache、3 层历史压缩、processTask 入口 Memory 注入 |
| `internal/bootstrap` | 系统装配与启动编排；`runtime_builder.go` 由 `AgentKind` 合成 `AgentRuntimeConfig`；SIGINT 哨兵；请求树取消 |
| `internal/config` | YAML/JSON 配置加载，仅支持 v4 schema（`llm:`/`scheduler:`/`agents:`/`infra:`/`tool_profiles:`/`modes:`/`reactors_file:` 等块）；v3 顶层字段已于 2026-04-26 移除，静默忽略 |
| `internal/dashboard` | Web 前端：内嵌 SPA、`/api/snapshot`、`/api/events` SSE、`/healthz`、`/api/interactions/{id}/response`；Bearer/?token= 鉴权，非 loopback 监听强制 token |
| `internal/effect` | V6 §4 H2b Effect Journal：副作用执行前记 prepared、执行后记 settled/unknown 的 append-only 账本（`.agentgo/state/effects.jsonl`，每 append flush+一次 fsync），声明 ReplayPolicy（safe_replay/verify_first/manual_only/never_replay）；启动 Recover 对 prepared 未 settled 的账目按策略裁决（verify_first 经 FileHashVerifier 比对盘上 hash，其余不自动动作），未知不得静默重跑 |
| `internal/eval` | 行为评测体系（独立开发工具 `agentgo-eval`，`cmd/agentgo-eval`，V6 起不入 Release 二进制）：preflight 凭证检查、黄金任务进程外黑盒驱动（临时 project_root + `/api/input` 注入 + snapshot 终态轮询）、确定性判据、Run Fingerprint 基线对比（record→candidate→promote）、`fakellm` 离线 E2E 假端点；套件/模板/基线在 gitignored `eval/` 下 |
| `internal/gate` | 统一 Gate 注册表。Phase 路由：`tool:preCall` / `tool:postCall` / `mailbox:beforeSend` / `mailbox:beforeDeliver` / `mailbox:beforeWake` |
| `internal/graph` | V6 JSON Graph：GraphDocument 即执行契约（校验/digest）、GraphStore 持久化（snapshot+journal/Recover）、Runtime 引擎（activation 模型、十类节点执行、graph_ 分片 trace） |
| `internal/hook` | 遗留 Hook 接口与内置实现，现作为 Gate 的适配层（内置 Gate 实现存放于此） |
| `internal/interaction` | 结构化人机交互协议：`pending → resolving → resolved` 两阶段 CAS；`graph_approval` / Shell / `agent_question` 共用同一 Service；`SetOnResolved` 为终态回调挂点。详见 `docs/design/interaction.md` |
| `internal/llm` | LLM 客户端接口 + openai-go 实现；`Message.ExtraFields` 透传非标准字段；错误分型 Recoverable / Unrecoverable / BadResponse |
| `internal/mailbox` | 代理间直发消息（`send_message`）、MailNotifier（默认启用）、16 条环形缓冲、TeamSnapshot |
| `internal/memory` | v5 Memory：`ScopeProcess`（纯内存）/ `ScopeSession`（`memory.jsonl` 持久化）/ `ScopeProject`（预留）；processTask 入口注入 `team_snapshot` / `file_awareness`；V6 §3 CM3：`Entry.State`（confirmed/inferred/stale/superseded，空=confirmed）+ `Evidence`/`SupersededBy` 生命周期字段、`SessionStore.Supersede`（同 Key 取代，旧条目置 superseded 留审计）/`MarkStale`/`Delete`(forget)、`promotion.go` 四终态晋升规则（纯函数）、召回过滤 helper `Entry.Recalled` |
| `internal/model` | `Task` / `Event` / `Claim` 数据结构与状态机 |
| `internal/modes` | 三轴模式 Store：gate（`immediate`；`plan` 已于 V6 移除，显式设置报迁移诊断，轴实体保留至 C6c）、exec（`normal`/`strict`/`readonly`/`yolo`）、topo（`team`/`solo`），正交可组合，运行时 `/mode` 或 Shift+Tab 切换 |
| `internal/pathutil` | 路径穿越防护 + 敏感文件模式拦截 |
| `internal/probe` | 启动期能力探测（如 web_search / web_fetch 可达性） |
| `internal/reactor` | Reactor 注册表（Name/Subscribe/Run/IsSync/Priority），按 `trace.Event.Kind` 扇出；`userdef/` 为用户 YAML Reactor 加载器 |
| `internal/roster` | 文件级占用：原子 `TryClaim` / `Release` / `ReleaseAll` / `IsOccupied` |
| `internal/runner` | 统一执行代理外壳（取代已删除的 `internal/worker`、`internal/explorer`） |
| `internal/scheduler` | Scheduler 是一等 `agent.Agent`（`EventType="__scheduler__"`）；`Activator` 是 eventCh 唯一消费者 |
| `internal/session` | Session 生命周期：history.jsonl、公开 LLM 轮次 `turns.jsonl`、快照、回放、归档、保留策略 |
| `internal/shell` | Shell 命令过滤（黑/灰/白名单）+ `shell_command` 授权 Interaction；POSIX 走 `sh -c`，Windows 走 PowerShell |
| `internal/spawn` | Spawn Manager（实现 `reactor.Reactor`）：从 `base_kind` 物化一次性 ad-hoc Runner，初始任务终结即拆除 |
| `internal/store` | `TaskStore` 接口、`MemoryTaskStore`、TaskCancelRegistry、ToolCallRecord、ArtifactLog 回放、ReadSet |
| `internal/suggest` | Did-You-Mean（`sahilm/fuzzy` 封装） |
| `internal/taskmem` | V6 §3 Task Memory（CM2）：有界、版本化、可恢复的滚动工作状态；`ApplyTurn` 只消费结构化 TurnFacts（不调 LLM），各段硬预算 + 有界渲染，按任务 JSON 原子落盘（`.agentgo/state/taskmem/`）；`PromotedAt` 是 CM3 晋升幂等标记 |
| `internal/team` | 按模板供给代理团队（`list_agent_templates` / `provision_agent_team`），自身注册为 Reactor |
| `internal/agenttemplate` | 代理模板目录（内嵌 builtin prompts） |
| `internal/tools` | 统一 ToolGroup 架构；`known_tools.go` 的 `AllToolNames` 是工具名权威清单 |
| `internal/trace` | 任务级 JSONL trace（Schema B fat struct）；CLI 查看器；`SetDefaultDispatcher` 扇出到 Reactors |
| `internal/tui` | Bubble Tea TUI；键位事实源在 `keymap.go`；Agent 详情按 Loop 浏览完整轮次，Home/End 控制最早/最新 |
| `internal/ui` | TUI/Web 共享 Hub、安全快照与斜杠命令目录；轮次历史不受有界诊断 feed 淘汰 |
| `internal/output` | 类型化输出通道事件（文本 / 流式快照 / 不可变完成轮次 / 任务结果） |
| `internal/watchdog` | 周期健康检查、级联取消、roster 清理、超时检测（110% 阈值）、panic 自动重启 |
| `internal/webtool` | Web 搜索 + URL 抓取，SSRF 防护，搜索后端可插拔 |
| `internal/workspace` | 按任务写时复制执行隔离：Manager（物化/合并/清理/孤儿扫描）、View（overlay 读穿透/写落副本）、Swapper（per-Runner 换入）；`types.go` 为冻结契约 |

> v5 已删除（不要再找）：`internal/cli`、`internal/worker`、`internal/explorer`。

## 核心机制（速查）

- **接口驱动**：`TaskStore` / `Roster` / `mailbox.Registry` / `memory.Store` / `gate.Registry` / `reactor.Registry` / `llm.Client` 均为接口 + 内存实现；无全局状态，全部经 `runner.RunnerDeps` 或 `scheduler.New` 注入。
- **事件驱动调度**：Store 以非阻塞发送（缓冲 64）发出任务事件；`Activator` 消费，`EventUserInput` 转为 `__scheduler__` 任务。
- **按任务取消**：`TaskCancelRegistry` 给每个任务挂可取消 context，任务进入终态即取消，执行中代理经 `ctx.Done()` 立即感知。
- **错误分型**：`llm.ErrRecoverable`（429/5xx）与 `ErrBadResponse`（length 截断）桥接为 `agent.ErrRecoverable` 触发重试回滚；`ErrUnrecoverable`（401/403）直接失败任务。
- **Kind 即配置**：`setting.yaml` 的 `agents[*]` 声明 kind + replicas + `profile`/`tools` + model + prompt；Bootstrap 按 kind×replica 建 Runner。运行时没有 `Agent.Kind` 枚举分支。
- **工具按 allowlist 剪枝**：`runner.resolveToolGroups()` 注册全部 ToolGroup，`ToolRegistry` 按 `AllowedTools` 在注册时剪掉未授权工具；任务级再经 `publish_task` 的 `tools` 参数二次裁剪——provision 时 kind 白名单是天花板，plan 时节点可声明更小的子集，认领后当次生效。新增工具必须同步 `internal/tools/known_tools.go`。节点能力容器 `model.NodeCapability` 另挂 `Isolation` 字段（写时复制执行隔离，见下条）。
- **ExecutionLease（V6 §4 H1 冻结执行租约）**：任务首次被认领时按 `Lease = NodeRequirement ∩ RouteCeiling ∩ Policy` 冻结当次执行契约（`model.ExecutionLease`，挂 `Task.Lease`，随快照持久化）。NodeRequirement：`Capability.Tools` 显式声明即用；未声明走**合成节点能力**规则（`Synthetic=true`）——需求合成为认领方 Route 白名单全量，这是文档化的合成授予，取代旧「隐式继承 kind 全集」语义（Graph 节点未声明同走合成）。Policy：exec=readonly 从 BusinessTools 剔除 `write_file`/`edit_file`/`run_shell`；exec=strict 保留并记 `ApprovalRequired=true`；控制通道按角色派生（Graph 节点 `{submit_task_result, request_replan}`，非图执行任务 `{submit_task_result}`，scheduler 控制面 `{report_done}`——scheduler 工具装配不变，仅生成 Lease 记录）。生命周期：首认领冻结（`execution_lease_frozen`，Digest=sha256 前 12 覆盖执行语义字段）→ RetryRollback 后重认领复用（`execution_lease_reused`，Digest 与工具面不变，快照恢复同路径；旧快照无 Lease 字段则认领时即时冻结）→ 任务终态（含 finalizing 被接受）撤销（`execution_lease_revoked`，`Revoked=true`，此后任何工具 dispatch 拒绝——runner 派发活性守卫重读检查，与 L1 finalizing fence 互补）。per-node 工具换入是 Lease 驱动的：registry 视图 = `BusinessTools ∪ ControlTools`（显式声明漏带控制工具也能收尾；并集覆盖注册全集时跳过换入保持零开销）；显式声明越界 fail-closed 走既有 `capability_violation` 路径（`execution_lease_rejected` 事件含缺失清单）。实现：`internal/agent/execution_lease.go`（计算/acquire/撤销辅助）、store 可选接口 `FreezeTaskLease`/`RevokeTaskLease`（终态方法自动撤销）。
- **公告板**：发布 → pending；`QueryAvailable(eventType)` 按优先级排序轮询；`ClaimTask` 原子转 processing（认领时查依赖与并发上限——依赖统一须 completed；落锁前经 capability checker 叠加检查节点工具子集）。终态 `Task.Results` 完整保留，board 快照只带预算内摘录（`result_refs`）；`Task.Artifacts`（实际产出，record-artifact Reactor 追加——写工具经 `KindFileWritten`，shell 写产物经 `KindShellExecuted` 成功后对 ExpectedArtifacts 盘后补登）≠ `Task.ExpectedArtifacts`（声明契约，Gate + 终止时校验——校验含磁盘兜底：账本缺失的预期项 stat 物理路径，防重试换任务 ID 后的账本失忆空转）；`Task.ReadSet` 由 read-set-write Reactor 维护。
- **Roster 花名册**：`write_file`/`edit_file` 先 `TryClaim`，被占用时返回含「占用」与占用者 ID 的错误；Watchdog 清理不再活跃代理的 claims；Roster 监听器写 `file_awareness` 到 Memory。
- **Gate**：`Decision.Action ∈ {Continue, Abort}`；同 Phase 内按 Priority 升序；panic 恢复为 Continue；nil Registry 一律 Continue。内置 11 个：tool:preCall 7 个（exec-mode-guard / path-boundary / validate-expected-hash / require-read-before-write / dependency-validator / enforce-expected-artifacts / validate-line-anchors）、beforeSend 1 个（chain-depth-limit）、beforeDeliver 1 个（per-agent-dedup）、beforeWake 2 个（wake-worthy-filter / wake-context-expand）。
- **Suggestions（V6 §4 H2a）**：Gate Abort 可携带结构化恢复提示——`Decision.ReasonCode`（稳定原因码，内置 Gate 的码集中在 `internal/hook/builtin/reason_codes.go`）+ `Suggestions`（稳定 ID=gate+原因码+目标 digest 前 8、retryable、有界 ≤3 的候选动作或升级标记）。类型权威定义在 `internal/hook/suggestion.go`，`internal/gate/suggestion.go` 以 type alias 复导出（gate 适配层已 import hook，反向会成环）。Harness（`internal/agent/suggestions.go`）把 Abort 注入为结构化观察文本：`[拒绝] 原因码=... retryable=... 说明=...` + 建议清单 + 「建议不自动执行，采纳需重新经过全部校验」。过滤纪律：不可重试 / 任务 finalizing（租约撤销）不给 tool_call 建议只给升级标记（user/blocked/replan/switch_mode）；同一建议 ID 在任务内第 3 次触发熔断，改指引 blocked/replan（per-task 计数，任务切换即弃）。trace 事件 `suggestions_returned`（计数与标识，无正文）与 `suggestion_disposition`（下一轮调用按 ID + Tool/Args 结构匹配判 adopted/abandoned/repeated，不做自然语言猜测）。无结构化字段的 Gate 拒绝仍走旧 `[hook 拒绝]` 文本路径（逐步迁移兼容）。
- **Effect Journal（V6 §4 H2b）**：副作用统一账本（`internal/effect`，`.agentgo/state/effects.jsonl`，append-only JSONL，每 append flush+恰好一次 fsync，刻意不做 group-commit——prepared→settled 间隙正是崩溃窗口）。埋点：`write_file`/`edit_file`（verify_first，ArgsDigest=落盘内容 sha256 前 12）、`run_shell`（manual_only，Target 只载命令 digest）、`send_message`（manual_only）、workspace 合并（never_replay）；先 Prepare（ID=`<taskID>-<seq>` per-task 单调、重启按账本最大值续号）再执行，完成后 Settle/失败标 unknown。脱敏纪律：完整参数/命令/消息正文不落账。启动 Recover：prepared 未 settled 一律标 unknown 再按策略裁决——verify_first 经 `FileHashVerifier` 比对盘上 hash，一致转 settled「已核验」、不一致/不可核验保持 unknown；manual_only/never_replay/safe_replay 均不自动执行任何动作（V6 红线：未知不得静默重跑），unknown 清单经日志 + 控制台 + trace 可见。账本注入经 `RunnerDeps.EffectJournal` 与 `scheduler.New` 末参，nil 或落账失败只告警降级、绝不阻断副作用（与 trace 同一纪律）。trace 事件 `effect_prepared` / `effect_settled` / `effect_unknown` / `effect_recovery_decided`（Effect 子载荷只载标识与摘要）。
- **Reactor**：同步 Reactor 在 `trace.Emit` 调用方 goroutine 串行执行，异步各起 goroutine 且 panic 隔离。内置：record-artifact / task-end-callback / trace-history-event / read-set-write / runtime-anomaly / graph-terminal-feed / session-memory-promotion，外加 spawn.Manager 与 team.Manager。用户 YAML Reactor 支持 `publish_task` / `invoke_llm` / `spawn_agent` / `call: send_message` 动作与 `when:` 条件；**永远异步**；Reactor 不得直接驱动状态迁移（无 `SetState`），只能发任务/消息让主循环自然迁移。
- **Graph（V6）**：JSON `GraphDocument` 经 `ParseAndValidate` 校验后即为执行契约（无第二条 IR 转换链）；节点每次进入创建单调 activation（`<nodeID>@<n>`，回边重进 = 新 activation + 新任务），activation 事实/任务发布/边选择全部 durable（`.agentgo/state/graphs/` 的 snapshot+journal），崩溃后 `Recover` + `ResumeGraph` 按 durable 事实幂等补发（TaskBoard 以 `(graph_id, activation_id)` 去重）。C5a 桥接（`internal/bootstrap/graph_runtime.go`）：`graphBoard` 把节点任务发布到公告板（任务带 `GraphID/NodeID/ActivationID`，capability 沿用 per-node 机制），`graph-terminal-feed` Reactor（Async，100 档）把四种任务终态回填 `Runtime.OnTaskTerminal`（cancelled 按 failed 喂，原状态留 `Result["status"]`）；`Results` 全量并入 `Result`，`Results["event"]` 驱动事件形态转移条件。C5b 落地 Scheduler 图工具（`submit_graph`/`patch_graph`）、acceptance 默认路由 `acceptance.verify` 与 `submit_task_result` 的 `event` 通道。C5c 补齐剩余节点语义：acceptance 为发任务型节点（与 agent 同路径经 board 发布验收任务，路由可被 `metadata["route"]` 覆盖，验收语义由验收 agent 的 prompt 契约承担——`submit_task_result` 的 `verdict`（写 `Results["verdict"]`，供 `$.verdict` 边条件）/`event` 字段；Plan 时代的熔断未随 C6b 迁移）；**G1b 起 acceptance 节点 completed 终态结算经注入的 `AcceptanceVerifier` 服务端核验**（`internal/graph/acceptance.go` 契约 + `internal/bootstrap/graph_acceptance.go` 实现：验收 agent 经 `submit_task_result` 的 `evidence_items` 参数上报 JSON 证据到 `Results["evidence"]`——command 对照 Effect Journal 该任务 shell 账逐字 digest 比对 + exit code、file_hash 边界内重算 sha256、task_status 词表；`valid` 按 verdict 正常转移，`disputed`/`unverifiable` 不采信 verdict——节点 failed（自报 verdict/event 不进路由输入）+ graph change 唤醒；未注入核验器保持 C5c 契约自报行为；审计事件 `acceptance_completed`）；approval 桥（`internal/bootstrap/graph_approval.go`）把 approval 节点落成 `purpose=graph_approval` 的授权型 Interaction（批准/拒绝两选项，requestID 由 `(graph_id, activation_id)` 确定性派生，重启后 rearm 幂等补登记），决议经 `interaction.Service.SetOnResolved` 终态回调异步回填 `Runtime.OnApprovalDecided`（cancelled/expired/failed/interrupted 映射为 rejected）；tool 桥（`internal/bootstrap/graph_tool.go`）只放开只读四工具（`read_file`/`list_dir`/`grep_search`/`glob_search`，复用 LocalReadGroup handler，pathutil 边界照常），其余工具名中文错误拒绝。C5d 把**图任务**的 `request_replan` 重定向到 Graph change 流：emit `graph_change_requested` + 发布 `__scheduler__` 唤醒任务（描述含 `[graph-change-request: <graph_id>/<activation_id>/change]` 幂等标记，同一 activation 未处理重复请求抑制；唤醒任务刻意不带图身份，避免被 graph-terminal-feed 当作节点终态回填），Scheduler 认领后用 `patch_graph` 裁决；patch 成功 emit `graph_revision_committed`（归 graph 分片）。**C6b 起非图任务**的 `request_replan` 走同形态通用 replan 唤醒任务（幂等标记 `[replan-request: <taskID>/replan]`，`EventSource="replan-request"`，不带图身份；`submit_task_result` 的 `blocked_reason`/`request_replan=true` 对非图任务附带同款唤醒；workspace 合并冲突与 runtime loop fuse 亦经此通道），Scheduler 认领后自行裁决后续编排。
- **Interaction**：`pending → resolving → resolved` 两阶段，`BeginResolve` 以 `expected_version` 做 CAS；另有 cancelled/expired/failed/interrupted 终态。Interaction 只拥有「用户选择」事实，不拥有 Graph/Shell 执行事实；`ActionRef`/Resolution 只在服务端。前端提交 `request_id + expected_version + option_id + text`；动作不得绑定裸字母/数字键。
- **Modes 三轴**：gate 轴 `plan` 值已于 V6 移除（执行前审阅经 Graph approval 节点；配置或 `/mode gate plan` 显式设置报迁移诊断）；exec=readonly 由 exec-mode-guard Gate 硬拒写工具，strict 逐次审批写与 shell，yolo 灰名单自动放行（黑名单仍硬拒）；topo=solo 禁止 Scheduler `publish_task`，亲自执行。
- **Memory**：processTask 入口查询 `ScopeProcess/KindContext` 的 `team_snapshot`、`file_awareness` 注入；nil-safe，无 Memory 时退化为不注入。
- **Task Memory（CM2）**：processTask 入口加载或创建（`Agent.TaskMemStore`，runner 装配注入）；settled Turn 收口处从 ToolCallRecord 账本增量 + file_written hash 重算 + Artifacts 增量收集 `TurnFacts` 滚动更新（仅实质变化调版本/落盘/发 `task_memory_updated`）；L2 压缩前、Attempt 结束前、任务终态强制 checkpoint（终态置 Sealed）；注入经 ctx 载体在 executor 内插入 user 首条之后并登记 Manifest `task_memory` 段（Source=task-memory，informational）；`Agent.TaskMemStore` 为 nil 或落盘失败时降级不阻断（Manifest 记 `dropped:<原因>`）。
- **Session Memory（CM3）**：`session-memory-promotion` Reactor（Async，100 档，`internal/bootstrap/session_promotion.go`）订阅四种任务终态事件，从终态 Task Memory 经 `memory.BuildPromotionCandidates` 按终态规则筛选（completed=已验证结果/产物/用户决定/约束；blocked=阻塞/已尝试/证据/恢复条件且不宣称完成；failed=仅可复现失败证据；cancelled=仅权威 Effect+明确用户决定；inferred 一律丢弃），经 `SessionStore.Supersede` 写入（同 Key 自动 supersede 旧条目）；幂等标记 `TaskMemory.PromotedAt`（每 Task 终态最多一次，重复事件/重启不重复晋升）；SessionStore 后端惰性解析（resume 挂接，未挂接跳过且不置标记）。召回在 processTask 任务入口（RetryCount=0 才注入）：按 Kind+recency 范围查询、过滤 stale/superseded、inferred 标注「未验证」，有界渲染（≤1200 runes）为 `<session-memory>` 块经 IncomingMail 以 user 角色注入，Manifest 登记 `session_memory` 段（Source=session-memory，informational，Freshness 按条目最新 UpdatedAt）；trace 事件 `session_memory_promotion_proposed/decided` / `memory_recalled` / `memory_entry_state_changed`。
- **3 层历史压缩**：L1 snip 旧工具输出为结构化墓碑（无 LLM 调用）；L2 超 `CompactTokenThreshold` 时摘要压缩；L3 context 溢出时激进压缩（keepRecent=1）。
- **公开轮次账本**：每次 Agent/Scheduler `TaskExecutor` 调用使用稳定轮次 ID；`KindStream` 只原位更新在途文本，返回后发布唯一 `KindTurn` 并 append+fsync 到当前 Session 的 `turns.jsonl`。账本只含公开 assistant 正文、工具名和终态错误，不复制 reasoning、工具参数或结果；TUI/Web 从 `Snapshot.Turns` 恢复全部 Loop。
- **FileStateCache**：按 Runner 的 LRU（容量 50），Get 时 `os.Stat` 比对 mtime+size 再验证（跨代理写透明失效）；写工具经 Roster 路径时失效。
- **乐观并发**：`write_file`/`edit_file` 接受 `expected_hash`，SHA256 不符即返回「冲突」错误。
- **路径安全**：`pathutil.ValidatePath` 强制项目根边界并拦截敏感文件（.env、.ssh 等），工具内与 path-boundary Gate 双重执行。
- **SSRF 防护**：`webtool.validateURL` + `isPrivateOrLoopback` 拦截内网/回环地址。

### 任务状态机

```
pending → processing → completed / failed / cancelled / blocked
                     ↘ pending（重试回滚）
pending → cancelled / failed / blocked
```

终态：completed、cancelled、failed、blocked。合法迁移见 `internal/model/task.go`。

**agent 自报 blocked 与 finalizing fence（V6 §5）**：`submit_task_result` 接受可选 `status`（缺省 `completed`；`blocked` 需同填 `blocked_reason`，failed/cancelled 不接受自报）。提交被接受即进入 finalizing——同一 LLM 响应中排在其后的工具调用被 fence 跳过（不 dispatch、无副作用，逐个 emit `tool_call_skipped`），同一任务重复提交返回中文错误（唯一终态提交者）。`status=blocked` 的收尾事务在 agent finalization 分支内先落 blocked 终态（store `BlockProcessingTaskBySystem`，cause=`agent_reported_blocked`；结果摘要保留在 `Results[agent]`，刻意不写 event/verdict 键以免图路由把 blocked 误判为事件命中；blocked 永不满足依赖认领闸），再为**非图任务**发布通用 replan 唤醒任务（图任务由 graph-terminal-feed 路由）；唤醒失败保留终态并 emit error 事件。trace 新增 `task_finalizing` / `tool_call_skipped` / `task_result_committed` 三事件。

### 工具分组（权威清单见 `internal/tools/known_tools.go`）

| 组 | 工具 |
|---|---|
| LocalRead | `read_file` `list_dir` `grep_search` `glob_search` |
| LocalWrite | `write_file` `edit_file` |
| Web | `web_search` `web_fetch` |
| Shell | `run_shell` |
| Meta | `publish_task` `send_message` `request_user_input` |
| PlanControl | `submit_task_result` `request_replan` |
| Scheduler 专属 | `cancel_task` `get_task_result` `report_done` `report_progress` `probe_directory` |
| AgentTemplate（Scheduler 专属） | `list_agent_templates` `provision_agent_team` |

普通 Runner 的工具来自 `tool_profiles` 或 `agents[].tools`；Scheduler 专属组不走 profile。`publish_task` 另接受可选 `tools`（逗号分隔工具名子集）、`model`（模型名）与 `isolation`（执行隔离，唯一合法值 `"workspace"`）参数（仅 Scheduler 计划控制面可设置——MetaGroup 装配 `AllowNodeCapability`），为单个 DAG 节点声明能力覆盖（`model.NodeCapability`）：认领 Runner 当次换入过滤后的工具注册表视图并临时替换模型，任务结束恢复；声明 `isolation:"workspace"` 的节点在写时复制 overlay 中执行（读穿透主根、写落 `.agentgo/workspaces/<taskID>/`），成功终态控制面自动合并回主根，冲突 → 任务 failed + 经通用 replan 唤醒任务交 Scheduler 裁决（详见 `docs/design/workspace-isolation.md`）。节点工具集必须 ⊆ 某条现存路由的白名单——`QueryAvailable(eventType, agentID)` 按认领方过滤 + `ClaimTask` 落锁前 capability checker 叠加检查（双保险，fail-closed），子集越界的任务对所有 Runner 不可见，滞留至 Watchdog 发出 `claim_starvation` 告警后由 Scheduler 修复；无 override 的任务行为不变。详见 `docs/design/per-node-capability.md`。

## 配置（v4 schema，唯一支持的格式）

`setting.yaml`（或 `.json`）顶层块：`llm:`（base_url/api_key/default_model/timeout_sec/reasoning_effort/stream）、`scheduler:`（仅 model 可覆盖）、`agents:`（kind/replicas/profile 或 tools/model/system_prompt_file/task_max_retries/enforce_compact_token_threshold/event_type/description）、`infra:`（watchdog/mail_notifier/store/roster）、`modes:`（gate/exec/topo 三轴；gate 仅 `immediate`，显式 `plan` 报 V6 迁移诊断）、`ui:`（frontends: [tui, web]，web.listen/token）、`tool_profiles:`、`reactors_file:`，以及杂项（`project_root`、`max_subtask_depth`、`shell_timeout_sec`、`shell_blacklist`、`shell_greylist`、`session_retention_days` 等）。完整示例见 `config.example.yaml`。

> V6 移除：`agent_max_loops`（agents[] 与 scheduler 两处）已删除——ReAct Loop 不再因到达固定轮数终止，由结构化终态、取消、deadline、预算与不可配置的 emergency fuse（10000，触发即 blocked + replan，不重跑）共同约束；旧配置显式设置该字段时 Validate 报迁移诊断。`context_limit`（固定上下文硬截断）同步移除——上下文适配由历史压缩（`enforce_compact_token_threshold`）与溢出重试承担，显式设置同样报迁移诊断。

> V6 C6b 移除：`internal/plan` 整包（动态 DAG Plan 控制面：Coordinator/PlanStore/验收 spec/run/熔断）与 `model.Plan` 全部类型已删除；验收四工具（`define_acceptance_spec`/`ensure_acceptance_run`/`submit_acceptance_result`/`get_acceptance_evidence）同步删除——验收语义由 Graph acceptance 节点 + `submit_task_result` 的 `verdict`/`event` 契约承担。`Task` 的 Plan 系字段（`PlanID`/`NodeRole`/`CreatedRevision`/`RetiredRevision`/`Supersedes`/`AcceptanceRunID`/`PlanMutationSource`）一并删除；Agent Team 归属从 Plan 改为发起 provision 的 controller 任务（生命周期挂 controller 任务终态）；`request_replan` 非图路径改为发布通用 replan 唤醒任务（幂等标记 `[replan-request: <taskID>/replan]`）。trace 的 `KindPlan*`/`PlanTraceContext` 常量与 UI 残留展示在 C6c 清理。

> v3 顶层字段（`worker_count`、顶层 `llm_base_url` 等）已移除并**静默忽略**：解析成功但无运行时效果。

## 约定

- 日志与代码注释**全用中文**；测试断言也用中文错误串（如「未找到」「占用」「冲突」「截断」）。
- YAML 配置键用 `snake_case`。
- 依赖：`google/uuid`、`gopkg.in/yaml.v3`、`openai/openai-go/v3`、`charmbracelet/bubbletea`、`charmbracelet/bubbles`、`charmbracelet/lipgloss`、`sahilm/fuzzy`。不要假设常用库可用——先查 go.mod 与邻近代码。
- agent 与 store 测试使用 property-based 测试（`testing/quick`）。
- 修改了本文件提及的结构、约定或工作流时，同步更新本文件。

## 跨平台硬约束

AgentGo 同等支持 Windows / macOS / Linux。以下每一条都曾在生产坏过一次，视为硬性要求：

- **测试中文件句柄必须先关闭再让 `TempDir` 清理**。Windows 的 `os.OpenFile` 不给 `FILE_SHARE_DELETE`；凡打开长生命周期 writer（history、snapshot、artifact log、trace writer）的测试必须 `t.Cleanup(func() { _ = x.Close() })`。
- **按代理的缓存命中时必须再验证新鲜度，不能信自己的 Invalidate**。跨代理写无法失效别人的缓存；参考 `FileStateCache`：Put 记 mtime+size，Get 时 `os.Stat` 比对。
- **路径只用 `filepath.Join` / `filepath.Clean`**，禁止 `/` 或 `"\\"` 拼接；`pathutil.ValidatePath` 是唯一权威边界检查。
- **Shell 一律走 `internal/shell`**（POSIX `sh -c`，Windows `powershell -NoProfile -NonInteractive -Command`，刻意不用 cmd）；不要从工具或 hook 直接 `exec.Command("sh", ...)`。
- **行尾 LF，`.gitattributes` 强制**。不要对字面 `"\r\n"` 做比较；解析可能带 CRLF 的输入时在边界处 `strings.ReplaceAll(s, "\r\n", "\n")` 归一。
- **终端输入无跨 shell 的统一「提交」语义**。TUI 用 Bubble Tea `textarea`（Enter 提交，Ctrl+J/Alt+Enter 换行）；Windows ConPTY 不能保证透传 bracketed paste，退化出的高速 `KeyRunes + Enter` 必须经过 `internal/tui/paste_burst.go` 状态机重组，禁止再用固定 Enter 防抖代替。任何新输入通路（Interaction、session 选择等）必须建在 Bubble Tea MVU 模型内，不用裸模式。Interaction 动作不得绑裸字母/数字键。
- **Windows NTFS 上 fsync 频率更敏感**。append 密集的 JSONL 日志保持「每次 append  flush+sync」，但绝不在已经过一次 fsync 的路径里加第二次。
- **新增 CI 时应同时跑 `ubuntu-latest` 与 `windows-latest`**——上述故障在 POSIX 上几乎全是静默的。

## 运行时文件访问边界（早期阶段，有意为之）

所有运行时文件/Shell 工具（`read_file`/`write_file`/`edit_file`/`list_dir`/`grep_search`/`glob_search`/`run_shell`）被限制在 `ProjectRoot` 内，工具内与 path-boundary Gate 双重执行，对所有 agent kind 一视同仁。这是**有意且暂时**的：在按代理能力声明体系成熟前，**不要**给单个工具加逃生口或「就这一次」例外。注意不对称性：YAML 配置层路径（如 `agents[*].system_prompt_file`）**允许**绝对路径——它以用户完整权限在启动期解析，与运行时工具权限是两个信任域。

## 交付约定（shipping conventions）

以下规则因其缺失反复浪费过工程时间，对任何非平凡改动视为硬性要求：

- **「完成」= 单测通过 + 二进制真实启动过一次 + 断言到预期产物**。本项目的重复故障模式是「装配漏接」：各包单测都绿，但跨包握手（bootstrap 装配、Gate/Reactor 注册、跨子系统状态注入）从未被端到端执行过。凡触及子系统边界的特性，汇报完成前：跑起二进制、端到端走一遍新路径、断言预期产物（文件落盘 / 事件发出 / 日志行出现）。5 行冒烟测试能抓到 100 行单测抓不到的东西。
- **修 bug 的提交必须同提交更新 `docs/activate/KNOWN_ISSUES.md`**。只记录当前可复现、未解决的问题；已解决的条目在同一变更中移除或归档其修复证据。

## 文档索引

- `Archtechture.md` — 详细系统设计与组件职责
- `docs/activate/ReactiveSystem.md` — v5 Gate + Reactor 架构
- `docs/activate/MemoryManageSystem.md` — v5 Memory 设计与迁移
- `docs/design/interaction.md` — Interaction 状态机、Graph approval/Shell effect 边界、TUI/Web 响应契约
- `docs/design/per-node-capability.md` — per-node 能力覆盖（publish_task tools/model/isolation）设计
- `docs/design/workspace-isolation.md` — 按任务写时复制执行隔离（overlay / 合并协议 / 触点表）设计
- `docs/activate/KNOWN_ISSUES.md` — 当前限制、验证缺口与可复现的开放问题
- `docs/tool-profiles.md` — 工具 profile / agent 声明 schema
- `docs/archived/` — 历史 RFC 与已完成的升级计划
