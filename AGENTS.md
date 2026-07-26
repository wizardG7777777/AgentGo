# AGENTS.md

本文件为在本仓库中工作的编码代理提供项目说明与项目约束。

## 项目

AgentGo 是一个 Go 多智能体任务编排系统。Scheduler（由 LLM 驱动的 `agent.Agent` 实例）把用户请求分解为任务，发布到公告板（`MemoryTaskStore`）；**Runner** 执行代理（`internal/runner`，按 kind 在 `setting.yaml` 中声明）轮询并认领任务。每个代理运行带工具调用的 ReAct 循环，具备 3 层历史压缩与按代理的文件缓存。Watchdog 监控健康；代理间通过 Roster（文件级占用登记）协调写文件、通过 Mailbox 互发消息。

**v5 反应式体系**（2026-05 起）用两个子系统取代了 v4 Hook 体系：

- **Gate**（`internal/gate`）— 动作前决策点（可 Abort），统一拦截 Tool / Mailbox / Shell 调用。
- **Reactor**（`internal/reactor`）— 状态变更后响应器（不可 Abort），含开发者注册的 Go Reactor 与用户声明式 YAML Reactor。

语言：Go 1.25 ｜ 模块名：`agentgo` ｜ 配置：`setting.yaml`（YAML/JSON，v4 schema，见下文） ｜ LLM：OpenAI 兼容接口，经 `internal/llm` Provider 适配（内置 `openai` / `deepseek-v4` / `deepseek-r1`）。

## 构建、测试与调试命令

```bash
go build                          # 构建（产出 ./agentgo 或 .\agentgo.exe）
go test ./...                     # 全部测试
go test ./internal/store/         # 单包测试
go test -run TestName ./internal/agent/   # 单个测试
./agentgo -config setting.yaml    # 启动（另有 -skip-startup-probe、-resume <sessionID>）
./agentgo config doctor           # 校验配置 + prompt 承诺工具与实际 allowlist 对账（error/warning/info 分级）
```

### trace 子命令（调试主入口）

任务执行过程以 JSONL trace 落盘，同一个二进制的 `trace` 子命令是排查任务行为的首选工具（不启动主系统）：

```bash
./agentgo trace list                    # 最近的任务（按发布时间倒序）
./agentgo trace show <task_id>          # 按时间序展示某任务全部事件；task_id 支持唯一前缀
./agentgo trace plan <plan_id>          # 聚合一个动态 DAG Plan 的跨任务事件时间线
./agentgo trace stats [task|agent|plan] # 聚合 LLM 调用与 token 消耗（默认按 task，按总 token 降序）
```

- trace 文件位置：Session 活跃时在 `.agentgo/sessions/sess-<id>/logs/`，否则回退 `.agentgo/traces/`；子命令自动解析活跃 Session。
- 文件命名 `<时间戳>_<task_id前8位>.jsonl`；重试可能跨物理分片，CLI 会按完整 TaskID 重新归组。`.prompts.jsonl` 是 prompt dump，不算任务文件。
- 实时跟踪最新任务：`tail -f .agentgo/sessions/sess-<id>/logs/<文件>.jsonl | jq`。

无 Makefile / linter，只用标准 Go 工具链。测试假定 LF 行尾（`.gitattributes` 强制）。

## 架构

### 启动流程（主干）

```
main.go（子命令 trace / config 分流；否则 -config 等 flags）
  └→ bootstrap.BootstrapWithOptions(...)
       ├─ config.LoadConfig + cfg.Validate()      // v4 schema 校验
       ├─ session.NewSessionManager               // .agentgo/sessions/, history.jsonl
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
| `internal/gate` | 统一 Gate 注册表。Phase 路由：`tool:preCall` / `tool:postCall` / `mailbox:beforeSend` / `mailbox:beforeDeliver` / `mailbox:beforeWake` |
| `internal/hook` | 遗留 Hook 接口与内置实现，现作为 Gate 的适配层（内置 Gate 实现存放于此） |
| `internal/interaction` | 结构化人机交互协议：`pending → resolving → resolved` 两阶段 CAS；Plan / Shell / `agent_question` 共用同一 Service。详见 `docs/design/interaction.md` |
| `internal/llm` | LLM 客户端接口 + openai-go 实现；`Message.ExtraFields` 透传非标准字段；错误分型 Recoverable / Unrecoverable / BadResponse |
| `internal/mailbox` | 代理间直发消息（`send_message`）、MailNotifier（默认启用）、16 条环形缓冲、TeamSnapshot |
| `internal/memory` | v5 Memory：`ScopeProcess`（纯内存）/ `ScopeSession`（`memory.jsonl` 持久化）/ `ScopeProject`（预留）；processTask 入口注入 `team_snapshot` / `file_awareness` |
| `internal/model` | `Task` / `Event` / `Claim` 数据结构与状态机 |
| `internal/modes` | 三轴模式 Store：gate（`immediate`/`plan`）、exec（`normal`/`strict`/`readonly`/`yolo`）、topo（`team`/`solo`），正交可组合，运行时 `/mode` 或 Shift+Tab 切换 |
| `internal/pathutil` | 路径穿越防护 + 敏感文件模式拦截 |
| `internal/plan` | 动态 DAG Plan：Coordinator、PlanStore、验收（acceptance spec/run、熔断） |
| `internal/probe` | 启动期能力探测（如 web_search / web_fetch 可达性） |
| `internal/reactor` | Reactor 注册表（Name/Subscribe/Run/IsSync/Priority），按 `trace.Event.Kind` 扇出；`userdef/` 为用户 YAML Reactor 加载器 |
| `internal/roster` | 文件级占用：原子 `TryClaim` / `Release` / `ReleaseAll` / `IsOccupied` |
| `internal/runner` | 统一执行代理外壳（取代已删除的 `internal/worker`、`internal/explorer`） |
| `internal/scheduler` | Scheduler 是一等 `agent.Agent`（`EventType="__scheduler__"`）；`Activator` 是 eventCh 唯一消费者 |
| `internal/session` | Session 生命周期：history.jsonl、快照、回放、归档、保留策略 |
| `internal/shell` | Shell 命令过滤（黑/灰/白名单）+ `shell_command` 授权 Interaction；POSIX 走 `sh -c`，Windows 走 PowerShell |
| `internal/spawn` | Spawn Manager（实现 `reactor.Reactor`）：从 `base_kind` 物化一次性 ad-hoc Runner，初始任务终结即拆除 |
| `internal/store` | `TaskStore` 接口、`MemoryTaskStore`、TaskCancelRegistry、ToolCallRecord、ArtifactLog 回放、ReadSet |
| `internal/suggest` | Did-You-Mean（`sahilm/fuzzy` 封装） |
| `internal/team` | 按模板供给代理团队（`list_agent_templates` / `provision_agent_team`），自身注册为 Reactor |
| `internal/agenttemplate` | 代理模板目录（内嵌 builtin prompts） |
| `internal/tools` | 统一 ToolGroup 架构；`known_tools.go` 的 `AllToolNames` 是工具名权威清单 |
| `internal/trace` | 任务级 JSONL trace（Schema B fat struct）；CLI 查看器；`SetDefaultDispatcher` 扇出到 Reactors |
| `internal/tui` | Bubble Tea TUI；键位事实源在 `keymap.go` |
| `internal/ui` | 斜杠命令目录（TUI/Web 共享的单一数据源） |
| `internal/output` | 类型化输出通道事件（文本 / 任务结果，不用魔法字符串分类） |
| `internal/watchdog` | 周期健康检查、级联取消、roster 清理、超时检测（110% 阈值）、panic 自动重启 |
| `internal/webtool` | Web 搜索 + URL 抓取，SSRF 防护，搜索后端可插拔 |

> v5 已删除（不要再找）：`internal/cli`、`internal/worker`、`internal/explorer`。

## 核心机制（速查）

- **接口驱动**：`TaskStore` / `Roster` / `mailbox.Registry` / `memory.Store` / `gate.Registry` / `reactor.Registry` / `llm.Provider` 均为接口 + 内存实现；无全局状态，全部经 `runner.RunnerDeps` 或 `scheduler.New` 注入。
- **事件驱动调度**：Store 以非阻塞发送（缓冲 64）发出任务事件；`Activator` 消费，`EventUserInput` 转为 `__scheduler__` 任务。
- **按任务取消**：`TaskCancelRegistry` 给每个任务挂可取消 context，任务进入终态即取消，执行中代理经 `ctx.Done()` 立即感知。
- **错误分型**：`llm.ErrRecoverable`（429/5xx）与 `ErrBadResponse`（length 截断）桥接为 `agent.ErrRecoverable` 触发重试回滚；`ErrUnrecoverable`（401/403）直接失败任务。
- **Kind 即配置**：`setting.yaml` 的 `agents[*]` 声明 kind + replicas + `profile`/`tools` + model + prompt；Bootstrap 按 kind×replica 建 Runner。运行时没有 `Agent.Kind` 枚举分支。
- **工具按 allowlist 剪枝**：`runner.resolveToolGroups()` 注册全部 ToolGroup，`ToolRegistry` 按 `AllowedTools` 在注册时剪掉未授权工具；任务级再经 `publish_task` 的 `tools` 参数二次裁剪——provision 时 kind 白名单是天花板，plan 时节点可声明更小的子集，认领后当次生效。新增工具必须同步 `internal/tools/known_tools.go`。
- **公告板**：发布 → pending；`QueryAvailable(eventType)` 按优先级排序轮询；`ClaimTask` 原子转 processing（认领时查依赖与并发上限）。终态 `Task.Results` 完整保留，board 快照只带预算内摘录（`result_refs`）；`Task.Artifacts`（实际产出，record-artifact Reactor 追加）≠ `Task.ExpectedArtifacts`（声明契约，Gate + 终止时校验）；`Task.ReadSet` 由 read-set-write Reactor 维护。
- **Roster 花名册**：`write_file`/`edit_file` 先 `TryClaim`，被占用时返回含「占用」与占用者 ID 的错误；Watchdog 清理不再活跃代理的 claims；Roster 监听器写 `file_awareness` 到 Memory。
- **Gate**：`Decision.Action ∈ {Continue, Abort}`；同 Phase 内按 Priority 升序；panic 恢复为 Continue；nil Registry 一律 Continue。内置 11 个：tool:preCall 7 个（exec-mode-guard / path-boundary / validate-expected-hash / require-read-before-write / dependency-validator / enforce-expected-artifacts / validate-line-anchors）、beforeSend 1 个（chain-depth-limit）、beforeDeliver 1 个（per-agent-dedup）、beforeWake 2 个（wake-worthy-filter / wake-context-expand）。
- **Reactor**：同步 Reactor 在 `trace.Emit` 调用方 goroutine 串行执行，异步各起 goroutine 且 panic 隔离。内置：record-artifact / task-end-callback / trace-history-event / read-set-write / runtime-anomaly，外加 spawn.Manager 与 team.Manager。用户 YAML Reactor 支持 `publish_task` / `invoke_llm` / `spawn_agent` / `call: send_message` 动作与 `when:` 条件；**永远异步**；Reactor 不得直接驱动状态迁移（无 `SetState`），只能发任务/消息让主循环自然迁移。
- **Interaction**：`pending → resolving → resolved` 两阶段，`BeginResolve` 以 `expected_version` 做 CAS；另有 cancelled/expired/failed/interrupted 终态。Interaction 只拥有「用户选择」事实，不拥有 Plan/Shell 执行事实；`ActionRef`/Resolution 只在服务端。前端提交 `request_id + expected_version + option_id + text`；动作不得绑定裸字母/数字键。
- **Modes 三轴**：gate=plan 时 Scheduler 须 `submit_plan_for_review` 挂起等用户裁决；exec=readonly 由 exec-mode-guard Gate 硬拒写工具，strict 逐次审批写与 shell，yolo 灰名单自动放行（黑名单仍硬拒）；topo=solo 禁止 Scheduler `publish_task`，亲自执行。
- **Memory**：processTask 入口查询 `ScopeProcess/KindContext` 的 `team_snapshot`、`file_awareness` 注入；nil-safe，无 Memory 时退化为不注入。
- **3 层历史压缩**：L1 snip 旧工具输出为结构化墓碑（无 LLM 调用）；L2 超 `CompactTokenThreshold` 时摘要压缩；L3 context 溢出时激进压缩（keepRecent=1）。
- **FileStateCache**：按 Runner 的 LRU（容量 50），Get 时 `os.Stat` 比对 mtime+size 再验证（跨代理写透明失效）；写工具经 Roster 路径时失效。
- **乐观并发**：`write_file`/`edit_file` 接受 `expected_hash`，SHA256 不符即返回「冲突」错误。
- **路径安全**：`pathutil.ValidatePath` 强制项目根边界并拦截敏感文件（.env、.ssh 等），工具内与 path-boundary Gate 双重执行。
- **SSRF 防护**：`webtool.validateURL` + `isPrivateOrLoopback` 拦截内网/回环地址。

### 任务状态机

```
pending → processing → completed / failed / cancelled
                     ↘ pending（重试回滚）
pending → cancelled / failed
```

终态：completed、cancelled、failed。合法迁移见 `internal/model/task.go`。

### 工具分组（权威清单见 `internal/tools/known_tools.go`）

| 组 | 工具 |
|---|---|
| LocalRead | `read_file` `list_dir` `grep_search` `glob_search` |
| LocalWrite | `write_file` `edit_file` |
| Web | `web_search` `web_fetch` |
| Shell | `run_shell` |
| Meta | `publish_task` `send_message` `request_user_input` |
| PlanControl | `submit_task_result` `submit_plan_for_review` `request_replan` `define_acceptance_spec` `ensure_acceptance_run` `submit_acceptance_result` `supersede_tasks` `finalize_plan` `mark_plan_blocked` `continue_waiting` `get_retired_node` `get_acceptance_evidence` |
| Scheduler 专属 | `cancel_task` `get_task_result` `report_done` `report_progress` `probe_directory` |
| AgentTemplate（Scheduler 专属） | `list_agent_templates` `provision_agent_team` |

普通 Runner 的工具来自 `tool_profiles` 或 `agents[].tools`；Scheduler 专属组不走 profile。`publish_task` 另接受可选 `tools`（逗号分隔工具名子集）与 `model`（模型名）参数（仅 Scheduler 计划控制面可设置），为单个 DAG 节点声明能力覆盖（`model.NodeCapability`）：认领 Runner 当次换入过滤后的工具注册表视图并临时替换模型，任务结束恢复。节点工具集必须 ⊆ 某条现存路由的白名单——`QueryAvailable(eventType, agentID)` 按认领方过滤 + `ClaimTask` 落锁前 `CanClaim` 叠加检查（双保险，fail-closed），子集越界的任务对所有 Runner 不可见，滞留至 Watchdog 发出 `claim_starvation` 告警后由 Scheduler 修复；无 override 的任务行为不变。详见 `docs/design/per-node-capability.md`。

## 配置（v4 schema，唯一支持的格式）

`setting.yaml`（或 `.json`）顶层块：`llm:`（base_url/api_key/default_model/timeout_sec/provider）、`scheduler:`（仅 model 可覆盖）、`agents:`（kind/replicas/profile 或 tools/model/system_prompt_file/agent_max_loops/task_max_retries/enforce_compact_token_threshold/context_limit/event_type/description）、`infra:`（watchdog/mail_notifier/store/roster）、`modes:`（gate/exec/topo 三轴）、`ui:`（frontends: [tui, web]，web.listen/token）、`tool_profiles:`、`reactors_file:`，以及杂项（`project_root`、`max_subtask_depth`、`shell_timeout_sec`、`shell_blacklist`、`shell_greylist`、`session_retention_days` 等）。完整示例见 `config.example.yaml`。

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
- **终端输入无跨 shell 的统一「提交」语义**。TUI 用 Bubble Tea `textarea`（Enter 提交，Ctrl+J/Alt+Enter 换行）；任何新输入通路（Interaction、session 选择等）必须建在 Bubble Tea MVU 模型内，不用裸模式。Interaction 动作不得绑裸字母/数字键。
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
- `docs/design/interaction.md` — Interaction 状态机、Plan/Shell effect 边界、TUI/Web 响应契约
- `docs/design/per-node-capability.md` — per-node 能力覆盖（publish_task tools/model）设计
- `docs/activate/KNOWN_ISSUES.md` — 当前限制、验证缺口与可复现的开放问题
- `docs/tool-profiles.md` — 工具 profile / agent 声明 schema
- `docs/archived/` — 历史 RFC 与已完成的升级计划
