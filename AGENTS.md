# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Project

AgentGo is a Go multi-agent task orchestration system. The Scheduler (LLM-driven `agent.Agent` instance) decomposes user requests into tasks, publishes them to a bulletin board (`MemoryTaskStore`), where **Runner** agents (instances of `internal/runner`, declared per-kind in `setting.yaml`) poll and claim tasks. Each agent runs a ReAct loop with tool calling, 3-layer history compression, and per-agent file caching. A Watchdog monitors health. Agents coordinate file writes via a Roster (file-level claim registry) and may message each other through the Mailbox subsystem.

**v5 Reactive System** (2026-05-01) replaced the v4 three-Registry Hook system with two consolidated subsystems:
- **Gate** (`internal/gate`) — pre-action decision points (can Abort), unifying Tool / Mailbox / Shell-filter pre-call interception
- **Reactor** (`internal/reactor`) — post-state-change responders (cannot Abort), with developer-registered Go reactors *and* user-declarative YAML reactors

Language: Go 1.25 | Module: `agentgo` | Config: `setting.yaml` (YAML/JSON, nested v5 schema — see "Configuration" below) | LLM backend: OpenAI-compatible via `internal/llm` Provider Adapter (default: Aliyun DashScope qwen3.6-plus; built-in providers: `openai` / `deepseek-v4` / `deepseek-r1`)

## Build & Test Commands

```bash
go build              # Build (produces ./agentgo or .\agentgo.exe)
go test ./...         # Run all tests
go test ./internal/store/   # Run tests for a single package
go test -run TestName ./internal/agent/  # Run a single test
./agentgo trace list  # Inspect recent task traces (subcommand of the same binary)
./agentgo config doctor  # 校验配置 + prompt 承诺工具与实际 allowlist 对账（error/warning/info 分级）
```

No Makefile or linter configured — standard Go tooling only. Tests assume LF line endings (enforced via `.gitattributes`).

## Architecture

### System Startup Flow

```
main.go  (-config flag, default "setting.yaml"; -skip-startup-probe; subcommands: trace, config)
  └→ bootstrap.Bootstrap(configPath, explicit, skipStartupProbe)
       ├─ config.LoadConfig(path, explicit)         // YAML/JSON, v4 schema
       ├─ cfg.Validate()                            // model、Agent、UI 与运行时配置校验
       ├─ printStartupBanner + startupProbe (TCP)   // best-effort, configurable
       ├─ session.NewSessionManager(...)            // .agentgo/sessions/, history.jsonl
       ├─ trace.NewWriter + SetDefault              // logs to sess-<id>/logs/ when active
       ├─ store.NewMemoryTaskStore(eventCh, ...)    // 公告板 + cancelRegistry
       ├─ store.OpenArtifactLog + Replay            // artifacts persistence
       ├─ gate.NewRegistry()                        // unified Gate registry
       │   └─ register 6 Tool-domain + 4 Mailbox-domain Gates
       ├─ roster.NewMemoryRoster()                  // file-level claim tracking (花名册)
       ├─ mailbox.NewRegistry(...)                  // per-agent inboxes
       ├─ memory.NewProcessStore()                  // ScopeProcess Memory (team_snapshot/file_awareness)
       ├─ interaction.NewService(...)               // Plan / Shell / Agent question shared CAS state machine
       ├─ reactor.NewRegistry()                     // Reactor registry
       │   ├─ register 4 built-in reactors:
       │   │   record-artifact / task-end-callback / trace-history-event / read-set-write
       │   ├─ spawn.NewManager(...)  → register as Reactor (one_shot ad-hoc agents)
       │   └─ userdef.LoadFromFile(cfg.ReactorsFile, ...)  // user YAML reactors
       ├─ trace.SetDefaultDispatcher(reactorReg)    // trace.Emit → Reactor.Dispatch
       ├─ statusCh + outputCh                       // UI log/progress + typed agent output
       ├─ scheduler.New(store, schedLLM, eventCh, cfg, ...)  // Bundle{Agent, Activator, Modes}
       ├─ watchdog.New(store, cfg, eventCh, roster)
       ├─ runtime_builder.buildAgentRuntime(kind, replicaIdx) → AgentRuntimeConfig
       │   └─ runner.New(rt, deps) × Σ(kind.Replicas)         // unified execution agents
       └─ mailbox.NewMailNotifier(...)
  └→ sys.Start(ctx, cancel)
       ├─ scheduler.Activator.Run(ctx)   ← eventCh consumer
       ├─ scheduler.Agent.Run(ctx)        ← poll-based, EventType="__scheduler__"
       ├─ watchdog.Run(ctx)               ← ticker-driven, auto-restart on panic
       ├─ mailNotifier.Run(ctx)           ← scans non-empty inboxes (default: enabled)
       └─ runner[i].Run(ctx) × N          ← poll-based, per kind.EventType
  └→ sys.RunCLI(ctx)                      ← 若启用 tui 则运行 TUI；web-only 时等待关闭信号
  └→ sys.Shutdown()                       ← spawn.Shutdown / WG / trace / artifactLog / session
```

### Package Overview

| Package | Responsibility |
|---------|---------------|
| `internal/agent` | Agent struct, ReAct loop, TaskExecutor, ToolRegistry, LLMExecutor, FileStateCache, 3-layer history compression, Memory injection at processTask entry |
| `internal/bootstrap` | System wiring, startup sequencing, kind×replica runner instantiation, validation; `runtime_builder.go` synthesises `AgentRuntimeConfig` from `AgentKind`. Arms a **SIGINT sentinel** (`sentinel.go`) at `Start`: headless 1st signal = graceful cancel, 2nd signal ≤3s (or post-cancel) = force exit(130), 5s shutdown deadline = force exit(1); `cancel_request.go` implements the request-tree cancel behind `Hub.CancelLatestRequest` |
| `internal/config` | YAML/JSON config loading. v4 schema only: `llm:` / `scheduler:` / `agents:` / `infra:` / `tool_profiles:` / `reactors_file:` / `modes:` blocks. v3 top-level fields (`worker_count`, `agent_max_loops`, `llm_base_url`, etc.) **were removed 2026-04-26** and are silently ignored |
| `internal/dashboard` | Web Dashboard（UI Hub 的 Web 前端）：内嵌 SPA + GET `/api/snapshot`、`/api/events` SSE、`/healthz`；POST 控制端点包含 `/api/interactions/{id}/response`，以 `expected_version` + 稳定 `option_id` 回答当前进程内任一 pending Interaction。`SessionID` 仅作创建审计归属，不用于过滤仍在运行的请求。Bearer/?token= 鉴权；非 loopback 监听强制 token |
| `internal/gate` | **Unified Gate Registry** (replaces v4 ToolHookRegistry / MailboxHookRegistry). `Phase` enum routes to `tool:preCall` / `tool:postCall` / `mailbox:beforeSend` / `mailbox:beforeDeliver` / `mailbox:beforeWake`. Interface-style Context (`ToolContext` / `MailboxContext`). 10 built-in Gates migrated from v4 |
| `internal/hook` | Legacy Hook interfaces still used internally by the LLMExecutor wiring as adapter surface to Gate. Tool/Mailbox/Agent Hook builtin folders remain; Agent Hook subsystem is empty (team-awareness deleted) |
| `internal/interaction` | **通用结构化人机交互协议**：`pending → resolving → resolved` 两阶段回答，CAS `Version`、稳定 Option ID、服务端私有 `ActionRef`，以及 cancel/expire/fail/interrupt 终态。Plan、Shell 与 `agent_question` 共用同一 Service；Interaction 只拥有“用户选择”事实，不直接拥有 Plan/Task/Shell 执行事实 |
| `internal/llm` | LLM client interface + openai-go SDK implementation, `Provider` adapter (built-in: `openai` / `deepseek-v4` / `deepseek-r1`), `Message.ExtraFields` passthrough for non-standard fields (e.g. `reasoning_content`); error types (Recoverable / Unrecoverable / BadResponse) |
| `internal/mailbox` | Direct agent-to-agent messaging (`send_message`), `MailNotifier` (default enabled), recent-message ring buffer (16) for hook peek, `TeamSnapshot` for team awareness |
| `internal/memory` | **Memory System** (v5). `Store` interface with `ScopeProcess` (implemented via `ProcessStore` — pure in-memory, RWMutex) / `ScopeSession` / `ScopeProject` (the latter two reserved for v5.x). Replaces v4 `team-awareness` Hooks; agent reads at `processTask` entry |
| `internal/model` | `Task`, `Event`, `Claim` data structures and state machine. `Task.SchedulerBatch`, `Task.Artifacts`, `Task.ExpectedArtifacts`, `Task.MailChainDepth`, `Task.LastResponse`, `Task.ReadSet` |
| `internal/modes` | **Three-axis mode store** (2026-07, NEW). `Store` (RWMutex) holds three orthogonal axes: gate (`immediate`/`plan` — plan-gate via `submit_plan_for_review` + `plan_review` Interaction), exec (`normal`/`strict`/`readonly`/`yolo` — readonly enforced by the `exec-mode-guard` Gate; strict = write_file/edit_file 逐次 `file_write` Interaction 审批 + run_shell 全量审批（白名单仍放行）; yolo = 灰名单 ask 自动放行+中文审计日志，两者黑名单都硬拒), topo (`team`/`solo` — solo forbids scheduler `publish_task`, direct execution + relaxed finalization). Injected into scheduler/bootstrap/UI Hub; axes compose freely (e.g. solo+plan+readonly) |
| `internal/pathutil` | Path traversal prevention + sensitive file pattern blocking |
| `internal/probe` | Directory/web probing primitives. `RunAll` performs startup capability probes (e.g. `web_search` / `web_fetch` reachability) |
| `internal/reactor` | **Reactor Registry** (v5, NEW). `Reactor` interface (Name/Subscribe/Run/IsSync/Priority); fans out `trace.Event` by `Kind`. 4 built-in reactors: `record-artifact` (KindFileWritten) / `task-end-callback` (KindTask{Completed,Failed,Cancelled,Retry}) / `trace-history-event` (compaction counts) / `read-set-write` (KindToolResult). User YAML loader at `reactor/userdef/` supports `publish_task` / `invoke_llm` / `spawn_agent` / `call: send_message` action verbs, `when:` conditions, per-kind filtering, and `via_translator:` |
| `internal/roster` | File-level claim tracking — atomic `TryClaim` / `Release` / `ReleaseAll` / `IsOccupied` / `ListByAgent` |
| `internal/runner` | **Unified execution-agent shell** (replaces v4 `internal/worker` and `internal/explorer`, both deleted). `runner.New(rt, deps)` instantiates one Agent per kind/replica based on the `AgentRuntimeConfig` produced by `runtime_builder.buildAgentRuntime`. The `Runner.Agent()` getter is used by Bootstrap to wire the scheduler kind specially |
| `internal/scheduler` | Scheduler is a first-class `agent.Agent` instance. `scheduler.New` returns `Bundle{Agent, Activator, Modes}`；`Modes` 是三轴 `*modes.Store`。**gate=plan** 时 Scheduler 必须调用 `submit_plan_for_review` 把计划正文写入 `Plan.Review` 并以 `plan_review` 挂起；运行时据 PlanStore 事实创建 Interaction，提供 `execute_plan` / `revise_plan`（需文本）/ `cancel_request`。用户回答后，受信任 handler 重新核对 Plan 版本、digest、pause reason 和三轴模式，再原子更新 PlanStore。Scheduler 可用 `request_user_input` 提普通 `agent_question`，但没有面向 LLM 的“替用户作答”或触发 Plan/Shell 特权 effect 的工具，也不能从自由文本猜测决定 |
| `internal/session` | Session lifecycle: manager, history.jsonl, snapshot, replay, archive; `SessionRetentionDays` / `SessionArchiveMax` drive retention |
| `internal/shell` | Shell command filtering + `shell_command` Interaction authorization. 黑名单始终硬拒绝；灰名单请求以 command/pattern/working directory 的摘要及 Agent/Task 身份绑定，选项为 `allow_once` / `deny` / `guidance`（需文本）/ `allow_session`。仅服务端保存 ActionRef；回答必须与原精确调用匹配。`allow_session` 只是稳定 Option ID，只能加入服务端捕获的原始 pattern；whitelist 在当前进程/本次运行内有效，切换 `/session` 不清空，退出后不持久化 |
| `internal/spawn` | **Spawn Manager** (v5, NEW). Implements `reactor.Reactor`; subscribes to task-terminal events. `Spawn(ctx, SpawnRequest)` materialises an ad-hoc Runner from a `base_kind` declaration with `RuntimeOverride`, publishes the initial task with a unique `EventType="adhoc:<spawnID>"`, and tears the runner down on initial task termination (one_shot lifecycle). `KindOf(agentID)` resolves base_kind for per-kind reactor routing. `ReactorSpawnMaxDepth=5` guards spawn-from-reactor cascades |
| `internal/store` | `TaskStore` interface, `MemoryTaskStore` (dependency-aware FIFO eviction), `TaskCancelRegistry`, `ToolCallRecord` history, `StoreHookView` read-only view, ArtifactLog with replay, ReadSet upsert, scheduler-batch helpers |
| `internal/suggest` | **Did-You-Mean** (v5, NEW). Single-function library wrapping `github.com/sahilm/fuzzy`; used by `tools/local_read.go` (empty results), agent tool dispatch ("tool not found"), and similar callsites |
| `internal/tools` | Unified ToolGroup architecture: LocalRead / LocalWrite / Web / Shell / Meta / Scheduler. Each `runner.New` registers all groups against a per-instance `ToolRegistry` whose **allowlist** (from `AgentKind.Profile` or `AgentKind.Tools`) prunes tools at registration time. `internal/tools/known_tools.go` (`AllToolNames`) is the canonical name registry. **No more `WorkerProfile` / `ExplorerProfile`** — every kind references `tool_profiles` symmetrically |
| `internal/trace` | Task-scoped JSONL trace; retries may span physical fragments, which the CLI regroups by full TaskID. `Event` carries `Kind` + nested payloads `Transition` / `ShellExec` / `ShellTimeout` / `Plan` / `Acceptance`. Subscribers via `SetDefaultDispatcher(reactorReg)` — `trace.Emit` fans events into Reactors. CLI viewer (`agentgo trace list/show/plan/stats`) auto-resolves the active session's `logs/`; `plan <plan_id>` aggregates a DAG across logical Tasks. |
| `internal/tui` | **Bubble Tea TUI** (v5, replaces deleted `internal/cli`). Pending Interaction 使用独立焦点：`↑/↓` 选择、`PgUp/PgDn` 翻长问题、`Enter` 提交，`RequiresText` 选项转入普通文本输入；不使用裸字母或裸数字作为动作键。Interaction 焦点中的 Esc 只返回输入框，不隐式拒绝。顶层 Esc 取消最新请求树；详情/结果视图 Esc 返回。Ctrl+C 两段式强退，Ctrl+L 清消息，输入框首/末行 `↑/↓` 调历史；键位事实源集中在 `keymap.go`。顶栏显示 session 级 token 总计（Hub 逐条累加 token_stats 事件，含已销毁 ad-hoc 团队的消耗；累加器未装配时回退为存活 agent 求和） |
| `internal/watchdog` | Periodic health checks, cascade cancellation, roster cleanup, timeout detection (110% threshold), crash report on cascade-cancel |
| `internal/webtool` | Web search + URL fetch primitives with SSRF protection (backend-pluggable via `SearchAPIProvider`) |

> **Deleted in v5** (do not look for these): `internal/cli`, `internal/worker`, `internal/explorer`. Their responsibilities were absorbed by `internal/tui`, `internal/runner`, and the Worker/Explorer toolset is now selected per-kind via `tool_profiles`.

### Key Design Patterns

- **Interface-driven**: `TaskStore`, `Roster`, `mailbox.Registry`, `memory.Store`, `gate.Registry`, `reactor.Registry`, `llm.Provider` are interfaces with in-memory implementations. No global state — everything is injected via `runner.RunnerDeps` or scheduler `New`.
- **Event-driven scheduling**: Store emits events (TaskCompleted, TaskFailed, etc.) via non-blocking channel send (buffer 64). Scheduler `Activator` consumes them; `EventUserInput` becomes a `__scheduler__` task; task-state events poke `BatchUpdateCh` to wake `SchedulerExecutor.waitForBatchTerminal`.
- **Per-task cancel context**: `TaskCancelRegistry` associates each task with a cancellable context. When watchdog/scheduler transitions a task to a terminal state, the context is cancelled and executing agents are notified immediately via `ctx.Done()`.
- **Error discrimination**: `llm.ErrRecoverable` (429/5xx) + `llm.ErrBadResponse` (FinishReason=length) bridge to `agent.ErrRecoverable` triggering retry rollback; `llm.ErrUnrecoverable` (401/403) marks task as failed. Uses `errors.As()` pattern.
- **Kind-based runtime**: `setting.yaml` declares one or more `agents[*]` entries (`kind` + `replicas` + `profile`/`tools` + `model` + `system_prompt_file` + behaviour params). Bootstrap calls `runtime_builder.buildAgentRuntime` per kind×replica, producing an `AgentRuntimeConfig`, then `runner.New(rt, deps)`. Kind is a configuration concept — there is no runtime `Agent.Kind` enum branching. The scheduler's `WorkerProfiles` / agent capability snapshot is rebuilt from `cfg.Agents` for board snapshots.
- **Tool selection by allowlist**: `runner.resolveToolGroups()` registers *every* ToolGroup; `agent.NewToolRegistryWithAllowlist(rt.AllowedTools)` filters out unauthorised tools before they reach the LLM. Adding a new kind requires no per-kind tool plumbing — only a profile entry or `tools:` list.
- **LLM Provider Adapter** (`internal/llm`): Two-layer mechanism. **Layer 1** — `Message.ExtraFields` / `Response.ExtraFields` (`map[string]json.RawMessage`) automatically round-trips unknown JSON fields via openai-go v3's `JSON.ExtraFields` + `param.metadata.SetExtraFields`. Covers DeepSeek V4 `reasoning_content`. **Layer 2** — `llm.Provider` interface (`PrepareMessages` / `RequestOptions`) handles transformation-style differences (DeepSeek R1 strips `reasoning_content` from history). Configure via `llm.provider:` per kind (defaults to `openai` no-op).
- **Scheduler as first-class agent** (Phase 3 refactor, 2026-04-10): The scheduler is an `agent.Agent` with `EventType="__scheduler__"`. It receives all the same infrastructure as runners (Gate, history compression, FileStateCache, Trace, per-task cancel). `scheduler.Activator` is the only event-channel consumer; it bridges `EventUserInput` → `PublishTask` and task-terminal events → `BatchUpdateCh` signal. Scheduler tools = MetaGroup (publish_task / send_message with `BatchTracker` injection) + SchedulerGroup (cancel_task / get_task_result / report_done) + the full worker toolset.
- **Watchdog**: Periodic ticker scans all tasks every tick (`Store.ScanAll`). Detects timeouts (110% threshold), unclaimed tasks, cascade-cancels dependents (both pending and processing), cleans stale roster claims, and sends crash-report mail to `task.EventSource` on cascade. Auto-restarts on panic via `recover()`.
- **3-layer history compression**: Layer 1 — `snipOldToolResults` replaces old high-output tool pages (including `get_task_result`) beyond `keepRecent` with structured tombstones (tool + path/command + original size + recall guidance; see `snipStub`), no LLM call; Layer 2 — `compressHistory` summarises when `totalPromptTokens > CompactTokenThreshold` (per-kind via `enforce_compact_token_threshold`); Layer 3 — aggressive compression on context overflow (`isContextOverflow` detects "length"/"截断"/"context"), `keepRecent=1`. Trace events `KindHistoryCompaction` / `KindHistoryTruncated` are recorded by the `trace-history-event` reactor.
- **FileStateCache**: Per-runner LRU cache (capacity 50) for file reads. Keyed by path, stores content + SHA256 hash + mtime+size for **stat-revalidation on Get** (cross-agent writes invalidate transparently — see Cross-platform constraints). Cleared on task boundaries; invalidated on write_file/edit_file via the Roster path.
- **Optimistic concurrency control**: `write_file` and `edit_file` accept optional `expected_hash`. If the file's current SHA256 differs, the operation is rejected with a "冲突" (conflict) error. Combined with Roster file locks, this prevents lost updates between agents.
- **Path security**: `pathutil.ValidatePath` enforces project-root boundary and blocks sensitive file patterns (.env, .ssh, credentials, etc.). Enforced both inside tools and as a `path-boundary` Gate (double-check).
- **SSRF protection**: `webtool.validateURL` + `isPrivateOrLoopback` blocks internal/loopback/link-local addresses before HTTP requests.

### Interaction System

`internal/interaction` 是 TUI、Web 与执行控制面共享的结构化人机交互协议，不是某个前端专用的命令通道。请求包含创建时的 Session 审计归属、Kind/Purpose、稳定 Option ID、`RequiresText` 和递增 `Version`；`ActionRef`、Resolution 与执行元数据只保留在受信任服务端，前端不得读取或回传它们。

- **领域状态机**：正常回答采用 `pending → resolving → resolved`。`BeginResolve` 以 `expected_version` 做 CAS，只有首个回答者能进入 `resolving`；同一回答可幂等重试，竞争回答返回冲突。请求也可进入 `cancelled` / `expired` / `failed` / `interrupted` 终态。
- **两阶段 effect**：CAS 只锁定用户选择；受信任 handler 随后应用领域 effect。成功后 `Complete`，可恢复错误用 `Release` 回到 `pending`，不可恢复或陈旧绑定用 `Fail`。不要先把 Interaction 标成 resolved 再执行领域写入。
- **事实所有权**：Interaction 是“用户选择”的事实源，不是执行事实源。Plan 的图、版本、pause reason、模式和 `ExecutionOverride` 仍以 PlanStore 为准；Shell 是否执行仍由原始被拦截调用及精确命令绑定决定。
- **Agent 提问适配器**：`request_user_input` 属于 MetaGroup，固定创建 `Kind=choice` / `Purpose=agent_question`。输入只有 `prompt` 与含 2–8 个 `{id,label,description?,requires_text?}` 项的 `options_json`；未知字段（尤其 ActionRef/Resolution/Metadata）拒绝。它只向 Agent 返回 `request_id`、稳定 `option_id` 与 `text`，不替代 Plan/Shell 控制面。
- **工具可见性**：Scheduler 使用无 allowlist registry，Service 可用时自动获得 `request_user_input`；普通 runner 仍需在 `tool_profiles` 或 `agents[].tools` 中显式授权。
- **前端契约**：TUI 和 Web 都提交 `request_id + expected_version + option_id + text`，并显示当前进程内全部 pending 请求；`SessionID` 只作创建审计归属，因为任务可跨 `/session` 切换继续运行。Web 使用 `POST /api/interactions/{id}/response`；TUI 使用 `↑/↓` 选择、`PgUp/PgDn` 翻问题正文与 Enter 提交，不为动作注册裸字母或数字键。Agent 等待期间显示 `waiting_interaction`。
- **恢复**：Plan Interaction 可由持久化 PlanStore 事实重新物化；进程内 Interaction Service 不可替代持久化恢复。

完整协议、选项和失败语义见 `docs/design/interaction.md`。

### Gate System (v5, replaces v4 Hook system)

`internal/gate` is the unified pre-action decision layer. Phase enum routes Gates to a single registry:

| Phase | When | Domain | Built-in Gates |
|---|---|---|---|
| `tool:preCall` | before each tool dispatch | Tool | `exec-mode-guard` (5), `path-boundary` (10), `validate-expected-hash` (20), `require-read-before-write` (30), `dependency-validator` (40), `enforce-expected-artifacts` (50), `validate-line-anchors` (60) |
| `tool:postCall` | after tool dispatch | Tool | (currently observation-only path; record-artifact migrated to a Reactor on `KindFileWritten`) |
| `mailbox:beforeSend` | mailbox.Send entry | Mailbox | `chain-depth-limit` (20) |
| `mailbox:beforeDeliver` | per-recipient delivery | Mailbox | `per-agent-dedup` (30) |
| `mailbox:beforeWake` | MailNotifier wake decision | Mailbox | `wake-worthy-filter` (40), `wake-context-expand` (900) |

Gate behaviour: `Decision.Action ∈ {Continue, Abort}` + optional `AbortReason` + (BeforeWake) `WakeDescription` accumulation. Gates within a Phase run by `Priority` ascending. `panic` is recovered as `Continue`. `nil` Registry returns `Continue` — disabling all Gates yields byte-identical behaviour (regression-tested).

### Reactor System (v5, NEW core capability)

`internal/reactor` introduces post-state-change response logic, **including user-configurable YAML reactors** — this is the v5 capability that did not exist in v4. `trace.Emit(ev)` fans out to subscribed Reactors via `SetDefaultDispatcher(reactorReg)`.

**Subscribable EventKinds** (from `internal/trace`): `KindTaskPublished/Claimed/Submitted/Completed/Failed/Cancelled/Retry`, `KindLLMCallStart/End`, `KindToolCall/ToolResult`, `KindHistoryCompaction/Truncated`, `KindFileWritten/WriteQueued`, `KindShellExecuted/TimeoutPending/Resolved`, `KindTextOnlySubmission`, `KindTokenStats`, `KindProgressNotify`, `KindError`, `KindAgentStateChanged`, `KindReactorSpawnDepthExceeded`, plus dynamic-DAG audit events `KindReplanRequested/Coalesced/Decided`, `KindAcceptanceCompleted`, `KindPlanRevisionChanged/Paused/Terminal` (user YAML exposure remains restricted by `reactor/userdef/loader.go`).

**Built-in reactors** (Go, registered in bootstrap.go):
- `record-artifact` — Async, Prio 950, on `KindFileWritten` → `Store.AppendArtifact`
- `task-end-callback` — Sync, Prio 500, on `KindTask{Completed,Failed,Cancelled,Retry}` → invokes registered task-end callbacks (e.g. clear `CurrentTaskHolder`)
- `trace-history-event` — Async, Prio 950, on `KindHistory{Compaction,Truncated}` → atomic counters
- `read-set-write` — Async, Prio 950, on `KindToolResult` (filters tool=read_file) → `Store.UpsertReadSet`
- `spawn.Manager` itself is registered as a Reactor and listens to terminal task events to tear down `one_shot` ad-hoc agents

**User YAML reactors** (`cfg.ReactorsFile`, loader at `reactor/userdef/`):
- Action verbs: `publish_task` (publish a new Task) / `invoke_llm` (one-shot pure-text LLM, no tools/history) / `spawn_agent` (materialise an ad-hoc agent via `spawn.Manager`) / `call: send_message` (only `send_message` in the v1 whitelist)
- Conditions: `when:` expression filtering, `kind:` per-base-kind filtering (uses `spawn.Manager.KindOf` for ad-hoc agents)
- `via_translator:` runs an `invoke_llm` to rewrite `initial_task.description` before `spawn_agent`
- **Sync vs Async**: built-in reactors may set `IsSync=true`; user reactors are **always async**, panic-isolated, failures only logged
- Reactor Principle 4: Reactors **may not directly drive new state transitions** (no `SetState` / `TransitionState`) — they must use `publish_task` / `send_message` / tool calls and let the main loop transition naturally
- `ReactorSpawnMaxDepth=5` caps spawn-from-reactor cascades

### Memory System (v5, replaces v4 team-awareness Hook)

`internal/memory` provides scope-based long/short-term memory:

```go
type Scope int  // ScopeProcess (implemented) | ScopeSession (v5.x) | ScopeProject (v5.x)
type Kind  string // KindConstraint | KindLearning | KindPattern | KindContext | KindAgentState
```

`ProcessStore` is the only v5 implementation — pure in-memory (`map[ID]*Entry` + `(scope,kind,key)→ID` index, single RWMutex). Agents read at `processTask` entry via `injectMemoryContext` (`Memory.Query(ScopeProcess, KindContext, "team_snapshot"|"file_awareness", 1)`); writers (scheduler / runners on team-state change, Roster on file-claim change) call `Memory.Put` directly. `GoalAnchor` was simply deleted (the task description already carries the goal). `Memory` is `nil`-safe — agents without one degrade to v4-without-team-awareness behaviour.

### Bulletin Board (公告板) Mechanism

The **MemoryTaskStore** is the central coordination point between scheduler, runners, watchdog, and reactors:

- **Publishing**: Scheduler (or `spawn.Manager`, or `Activator` for user input) calls `PublishTask()` with description, priority, EventType, dependencies, `ExpectedArtifacts`, optional `SystemPrompt`. Tasks start `pending`.
- **Claiming**: Agents call `QueryAvailable(eventType)` to poll for pending tasks (sorted by priority desc), then `ClaimTask(agentID, taskID)` to atomically transition to `processing`. Dependency checks and concurrency limits are enforced at claim time.
- **Event emission**: State transitions emit events (`EventTaskCompleted`, `EventTaskFailed`, `EventTaskCancelled`, `EventTaskRetry`) via a non-blocking channel send. Scheduler `Activator` consumes them.
- **Resource awareness**: Scheduler builds a board snapshot JSON including agent capabilities, busy/free agents, and per-task progress — enabling the LLM to gauge parallelism capacity when planning. Managed Plans expose only the active graph; legacy roots expose only their PlanID/SchedulerBatch/ParentTaskID request tree.
- **Bounded hot results**: Terminal `Task.Results` remain complete in Store/session persistence, while board `tasks[].result_refs` carries stable agent/byte/rune/SHA256 metadata and a globally budgeted excerpt. Scheduler uses `get_task_result` only when that excerpt is insufficient.
- **Streaming progress**: Agents call `AppendOutput()` to write partial output; board `tasks[].progress` retains only a bounded UTF-8 tail plus original size metadata.
- **Artifacts vs ExpectedArtifacts**: `Task.Artifacts` is the *actual* file list (auto-appended by `record-artifact` Reactor); `Task.ExpectedArtifacts` is the *declared contract* checked at task termination by `agent.checkExpectedArtifacts` and the `enforce-expected-artifacts` Gate.
- **ReadSet**: `Task.ReadSet` is the set of files an agent has read in this task, populated by the `read-set-write` Reactor on `KindToolResult{tool=read_file}`. The `require-read-before-write` Gate consults this set instead of replaying the tool-call history.

### Roster (花名册) Mechanism

The **MemoryRoster** prevents file write conflicts between concurrent agents:

- **TryClaim(agentID, filePath)**: Atomic file-level lock. Returns `(false, nil)` if another agent already holds the claim — no error, no blocking.
- **Release / ReleaseAll**: Agents release file claims after write operations. `ReleaseAll(agentID)` is called when a runner exits (defer in `Agent.Run`).
- **Watchdog cleanup**: `cleanupStaleClaims` collects active agent IDs from processing tasks, then calls `ReleaseAll` for any agent in the roster that is no longer active.
- **Integration with tools**: `write_file` and `edit_file` call `TryClaim` before any write. If another agent holds the file, the tool returns an error containing "占用" (occupied) and the occupying agent's ID.
- **FileAwareness sourcing**: A Roster listener writes `KindContext / file_awareness` entries to Memory so agents can see live file occupancy in their next `processTask` entry.

### Runner Tools (per-kind ToolGroups)

`runner.resolveToolGroups()` (in `internal/runner/dependency_map.go`) registers all six groups; `AllowedTools` from `tool_profiles` or `agents[*].tools` prunes them in the `ToolRegistry`:

| Tool | Group | Description | Security |
|------|-------|-------------|----------|
| `read_file` | LocalRead | Read file + SHA256 hash, LRU+stat cached；缓存命中且文件未变时返回摘要 stub，`force_full=true` 取全文 | `pathutil.ValidatePath` + `path-boundary` Gate |
| `list_dir` | LocalRead | List directory entries | `pathutil.ValidatePath` + `path-boundary` Gate |
| `grep_search` | LocalRead | Literal text search, max 50 lines, skips dotfiles and >1MB | `pathutil.ValidatePath` + `path-boundary` Gate |
| `glob_search` | LocalRead | Recursive glob with `**` support, max 200 results | `pathutil.ValidatePath` + `path-boundary` Gate |
| `write_file` | LocalWrite | Create/overwrite, optional `expected_hash` | Roster lock + path Gate + `validate-expected-hash` Gate + `require-read-before-write` Gate + `enforce-expected-artifacts` Gate |
| `edit_file` | LocalWrite | Precise old→new replacement (exactly 1 match) | Same as write_file + line-anchor validation |
| `run_shell` | Shell | Shell command (`sh -c` / PowerShell `-NoProfile -Command`), output capped 10000 chars | `shell.CommandFilter`; blacklist hard deny, 写文件重定向（`>`/`>>`/`Out-File`/`tee`）硬拒绝并指引 write_file/edit_file, greylist → exact-call-bound `shell_command` authorization Interaction |
| `publish_task` | Meta | Publish child task | `MaxSubtaskDepth` + `dependency-validator` Gate + `BatchTracker` (scheduler-only) |
| `send_message` | Meta | Direct mailbox message | `MailChainMaxDepth` + Mailbox Gates |
| `request_user_input` | Meta | Ask a 2–8 option `agent_question` and await `option_id`/`text` | No client-supplied ActionRef or privileged Plan/Shell effect; ordinary runners require allowlist |
| `submit_task_result` | PlanControl | 普通执行节点的结构化完成提交（summary/checks_performed/evidence/remaining_risks/blocked_reason/request_replan）；调用即显式完成意图，经 FinalizationHolder 短路 ReAct 循环 | Runner-only（scheduler 用 `report_done`，acceptance 节点用 `submit_acceptance_result`）；提交前强制 ExpectedArtifacts 校验；blocked/replan 自动持久化 ReplanRequest |
| `web_search` | Web | Pluggable backend (DuckDuckGo HTML by default), max 10 results | SSRF protection |
| `web_fetch` | Web | Fetch URL, extract text, max 1MB / 10000 chars | SSRF protection |
| `cancel_task` | Scheduler | Cancel another task | Scheduler-only (declare in profile) |
| `get_task_result` | Scheduler | Page a visible terminal result by Unicode rune offset (default 4000, max 8000) | Active Plan controller/current node or legacy request-tree scope only; full-body SHA256 returned |
| `report_done` | Scheduler | Report user-facing completion + Artifacts fact-check | Scheduler-only |
| `probe_directory` | Scheduler | Recursive tree+stats for task planning | Scheduler-only |

### Task State Machine

```
pending → processing → completed
                     → failed
                     → cancelled
                     → pending (retry rollback)
pending → cancelled / failed
```

Valid transitions defined in `internal/model/task.go`. Terminal states: completed, cancelled, failed. State transitions emit `Transition` substruct via `trace.Event` (Schema B).

### Concurrency

- `sync.RWMutex` on store, roster, mailbox registry, memory ProcessStore; `sync.Mutex` on FileStateCache and scheduler internals
- `context.Context` for cancellation propagation (global + per-task)
- Scheduler: two goroutines — `Activator` (event-channel select loop) + `Agent.Run` (poll-based, EventType=`__scheduler__`)
- Watchdog goroutine (ticker-driven with auto-restart on panic)
- Mail Notifier goroutine (default enabled)
- Runner goroutines × Σ(kind.Replicas) (poll-based, per-kind EventType)
- TUI blocks the main goroutine (`sys.RunCLI` → `tui.Run`); `/quit` or stdin close triggers global cancel
- Tool calls within a single LLM response are executed in parallel (`sync.WaitGroup`)
- Reactor dispatch: Sync reactors run serially in `trace.Emit` caller's goroutine; Async reactors fork a goroutine each, panic-recovered

### Configuration (v4 schema, the only supported format)

`setting.yaml` (or `.json`) top-level blocks:

```yaml
llm:                              # Global LLM defaults
  base_url: "..."
  api_key: "..."
  default_model: "qwen3.6-plus"
  timeout_sec: 60
  provider: "deepseek-v4"         # openai / deepseek-v4 / deepseek-r1

scheduler:                        # Scheduler is a hard-coded kind; only model is overridable
  model: "qwen3-max"

agents:                           # List of AgentKind declarations
  - kind: worker
    replicas: 2                   # Two worker-1, worker-2 instances
    profile: "full-access"        # OR `tools: [...]` (mutually exclusive)
    model: "qwen3.6-plus"         # Optional per-kind override
    system_prompt_file: "prompts/worker.md"
    agent_max_loops: 50
    task_max_retries: 3
    enforce_compact_token_threshold: 80000
    context_limit: 128000
    description: "通用任务执行代理，能读写文件、跑命令、发邮件"
  - kind: explorer
    replicas: 1
    event_type: "explore"         # Only claim tasks with this EventType
    profile: "read-only"
    system_prompt_file: "prompts/explorer.md"

infra:
  watchdog:     { interval_sec: 30 }
  mail_notifier: { enabled: true, interval_sec: 5 }
  store:        { event_channel_buffer: 64, fifo_limit: 100, default_concurrency: 1, default_timeout_sec: 300 }
  roster:       { wait_timeout_sec: 30 }

modes:                            # 三轴模式（正交可组合）；运行时可用 /mode 或 Shift+Tab 切换
  gate: immediate                 # immediate / plan（plan=规划后提交 plan_review Interaction，用户选择后才执行）
  exec: normal                    # normal / strict / readonly / yolo（readonly=Gate 硬拒写；strict=写工具 file_write 审批+shell 全量审批；yolo=灰名单自动放行）
  topo: team                      # team / solo（solo=scheduler 亲自执行，禁止 publish_task）

# Top-level miscellaneous
project_root: "."
max_subtask_depth: 1
shell_timeout_sec: 30
transfer_note_max_tokens: 3000
progress_notify_enabled: false
agent_idle_threshold: 0
search_api_provider: "duckduckgo_html"
shell_blacklist: []
shell_greylist: []
tool_profiles:
  read-only:    [read_file, list_dir, grep_search, glob_search, web_search, web_fetch, send_message, request_user_input]
  full-access:  [read_file, write_file, edit_file, list_dir, grep_search, glob_search, run_shell, publish_task, send_message, request_user_input, web_search, web_fetch]
reactors_file: "reactors.yaml"    # Optional v5 user reactor declarations
session_retention_days: 14
session_archive_max: 50
session_resume_max_idle_sec: 3600
session_snapshot_interval_sec: 30

ui:                               # UI Hub 前端；frontends 缺省 [tui]，仅 [web] 即 headless（无 TUI）
  frontends: [tui, web]
  web:
    listen: "127.0.0.1:8399"      # 非 loopback 监听必须设置 token
    token: ""
```

> **v3 fields removed (silently ignored)**: `worker_count`, `agent_max_loops` (top-level), `llm_base_url`, `llm_api_key`, `llm_model`, `worker_profile`, `explorer_profile`, `scheduler_max_loops`, `mailbox_buffer_size` (top-level), `mail_chain_max_depth` (top-level), and similar v3 top-level keys. Behaviour: parsing succeeds, fields produce no runtime effect. Migrate by moving values into the v4 blocks.

### Trace System (Schema B)

`internal/trace.Event` is a fat struct with five optional pointer payloads: `Transition` / `ShellExec` / `ShellTimeout` plus dynamic-DAG `Plan` / `Acceptance`. Existing top-level fields are preserved; `omitempty` keeps old jsonl readable. The 31 built-in system EventKinds include task/agent/tool/shell events and seven dynamic-DAG audit events. The CLI viewer (`agentgo trace list/show/plan/stats`) auto-resolves the active session's `logs/` via `.agentgo/sessions/active-session`, regroups retry fragments by full TaskID, and uses `trace plan <plan_id>` to reconstruct a stable cross-Task Plan timeline.

Subscriber model: `trace.SetDefaultDispatcher(reactorReg)` — `trace.Emit` fans events to Reactors. `nil` dispatcher is a no-op (no behaviour change vs. trace-only mode).

## Conventions

- Logs and comments use **Chinese** throughout the codebase
- YAML config uses `snake_case` keys
- Dependencies: `google/uuid`, `gopkg.in/yaml.v3`, `openai/openai-go/v3`, `charmbracelet/bubbletea`, `charmbracelet/bubbles`, `charmbracelet/lipgloss`, `sahilm/fuzzy`
- Property-based tests (`testing/quick`) used in agent and store tests
- Tests use Chinese error messages for assertion (e.g., "未找到", "占用", "冲突", "截断")

## Cross-platform constraints

AgentGo targets Windows / macOS / Linux equally. The following rules exist because each of them was broken once and re-broke something in production — treat them as hard project requirements, not suggestions.

- **File handles in tests must be closed before `TempDir` cleanup**. Windows' Go `os.OpenFile` does not grant `FILE_SHARE_DELETE`, so any test that opens a long-lived writer (history log, snapshot writer, artifact log, trace writer) must register `t.Cleanup(func() { _ = x.Close() })`. POSIX-only developers won't see this locally; it will only manifest on Windows CI as `unlinkat ... being used by another process`.
- **Per-agent caches must validate freshness on hit, not trust their own Invalidate**. Cross-agent writes cannot invalidate another agent's cache. The `FileStateCache` pattern — record `mtime+size` on `Put`, `os.Stat`+compare on `Get` — is the reference fix.
- **Path handling uses `filepath.Join` / `filepath.Clean` only**. Never concatenate with `/` or `"\\"`. `pathutil.ValidatePath` is the one authoritative boundary checker.
- **Shell commands route through `internal/shell`**. It dispatches `sh -c` on POSIX and `powershell -NoProfile -NonInteractive -Command` on Windows (cmd is intentionally not used — its quoting rules clash with Unix-command priors; Windows without PowerShell is unsupported), plus applies blacklist/whitelist/runtime-whitelist covering both Unix and Windows/PowerShell dangerous-command forms. Do not call `exec.Command("sh", ...)` directly from tools or hooks.
- **Line endings are LF, enforced by `.gitattributes`**. Never compare against literal `"\r\n"`; when parsing user/file input that might carry CRLF, normalise via `strings.ReplaceAll(s, "\r\n", "\n")` at the boundary.
- **Terminal input has no universal "submit" semantics across shells**. The TUI uses Bubble Tea `textarea`（Enter 提交，Ctrl+J/Alt+Enter 换行）. Any new input pathway (Interaction、session picker、steer) must be built within Bubble Tea's MVU model, not raw mode. Interaction 动作不得绑定裸字母或裸数字，以免截获正常文本输入。
- **fsync frequency matters more on Windows NTFS**. For append-heavy JSONL logs (history, trace, artifacts), keep the "flush+sync per append" pattern, but never add a second fsync inside a code path that already goes through one.
- **CI should run both `ubuntu-latest` and `windows-latest`** when added, because the failure modes above are almost all silent on POSIX.

## Runtime file access scope (early phase)

All runtime file/shell tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `grep_search`, `glob_search`, `run_shell`) are restricted to paths within `ProjectRoot`, enforced both inside the tools and as a `path-boundary` Gate. This applies uniformly across all agent kinds and is **intentional + temporary**.

- The boundary exists because per-tool capability checks would otherwise have to reason about external filesystem layout per agent kind, which is the job of the permission system, not the tool itself.
- Once the per-agent capability declaration system matures, this restriction should be relaxed so agents with declared external-access capabilities can read/write outside `ProjectRoot`.
- Until that lands, do **not** add per-tool escape hatches or "just this one" exceptions.
- **Asymmetry with config-layer paths**: YAML config fields (e.g. `system_prompt_file` in `agents[*]`) **do** accept absolute paths because they are resolved at startup under the user's full authority, not under agent runtime permissions. Treat config-layer file access and runtime tool access as two distinct trust domains.

## Shipping conventions

The following rules exist because their absence has repeatedly cost engineering time on this project. Treat them as hard requirements for any non-trivial change.

- **"Done" = unit tests pass + binary started once + artifact asserted**. AgentGo's repeat failure mode is "装配漏接" (assembly miswiring): each package's unit tests pass, but the cross-package handshake (bootstrap wiring, gate registration, reactor registration, cross-subsystem state injection) was never exercised end-to-end. For any feature touching a subsystem boundary, before reporting done: run the binary, exercise the new path end-to-end, and assert the expected artifact (file written, event emitted, log line present). 5 lines of smoke test catch what 100 lines of unit test cannot.
- **Bug fix PRs must update `docs/activate/KNOWN_ISSUES.md` in the same commit**. Record only currently reproducible, unresolved issues; remove a resolved entry or archive its remediation evidence in the same change.

## Documentation

- `Archtechture.md` — detailed system design and component responsibilities
- `docs/archived/interface-design-tui-2026-05.md` — historical pre-Web UI TUI interface record
- `docs/activate/ReactiveSystem.md` — v5 Gate + Reactor architecture, full design + landed phases
- `docs/activate/MemoryManageSystem.md` — v5 Memory System design + migration path
- `docs/archived/trace-upgrade-design-2026-05.md` — historical Schema B trace upgrade design; current behavior is TraceGuide + code
- `docs/design/interaction.md` — current Interaction state machine, Plan/Shell effect boundaries, TUI/Web response contract
- `docs/activate/ToolUpgradePlan.md` — historical Shell tool configuration plan; its legacy prompt protocol is not the current Interaction contract
- `docs/archived/hallucination-acceptance-audit-2026-05.md` — historical hallucination audit; current gaps are in KNOWN_ISSUES
- `docs/activate/KNOWN_ISSUES.md` — current limitations, verification gaps, and reproducible open issues
- `docs/tool-profiles.md` — tool profile / agent declaration schema
- `docs/archived/` — historical RFCs and completed upgrade plans (`nextUpgrade_v4.md`, `nextUpgrade_v5.md`, `hookSystem.md`, etc.)
