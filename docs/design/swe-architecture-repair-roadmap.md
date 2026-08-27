# SWE 架构修复统一实施路线图

> 状态：SWE-015…045 已有外部 closure；SWE-046…053 implementation landed / external validation pending<br>
> 日期：2026-08-23<br>
> 适用范围：第五、六轮 Flask SWE 分层诊断及其残余问题<br>
> 上位规范：[`五层工程架构规范`](five-layer-engineering-architecture.md)<br>
> 问题总账：[`第五轮 SWE 分层诊断`](../test-issues/2026-08-21-2329-swe-round5-layered-diagnosis.md)；
> [`第六轮 0/8 分层诊断`](../test-issues/2026-08-22-1510-swe-round6-zero-of-eight-layered-diagnosis.md)

## 1. 路线图目的

第五轮 SWE 暴露的故障跨越 Model Invocation、L2 Context、L4 Loop 和 L5 Graph。
四份设计分别给出了正式目标，但其中共享 Attempt、Invocation、Snapshot、Failure、
Checkpoint、Outcome 和 Trace identity。若分别直接开工，极易产生重复类型、双重
Store、桥接字符串和不同 deadline 口径。

本路线图负责：

1. 给出所有 Accepted Design 的依赖顺序；
2. 固定跨层共享类型的唯一权威；
3. 将工作拆成可验证、可删除旧路径的小切片；
4. 允许独立工作流并行，但禁止越过尚未落地的契约；
5. 为每个 shadow/cutover/legacy adapter 设置明确退出条件；
6. 统一真实二进制、durable 产物和 SWE 多 rollout 验收。

本路线图不是新的运行时设计，也不覆盖各专文的详细不变量。

### 1.1 2026-08-22 实施账本

本表只记录实现切片是否已经进入仓库，不替代后文的 issue closure 条件。
“基础已落地”不等于生产 cutover，也不等于 SWE 问题已经关闭。

| 范围 | 当前状态 | 已进入仓库的权威 | 尚未满足的退出条件 |
|---|---|---|---|
| Wave 1 identity / run | 生产主链已接入 | `RunContract`、Run/Attempt/Turn/Action identity、durable TerminalIntent/TaskOutcome、OutcomeRef、terminal adapter 与 delivery outbox 已贯穿 Task/Session/Graph | 外部 E2E 与多 rollout 验证 |
| Invocation | Responses typed-item 主链已接入 | 显式 protocol；message/reasoning/function_call 信封；ContextBinding OutputBudget；required-nonce probe；partial no-dispatch | 外部 provider 多 rollout；Chat compatibility 退出 |
| L2 Context | Context v8/Replay v3 production default | v1–v7 digest 保留、Responses `assistant_response_items` RequiredExact carrier、32K completion、92K input、bounded runtime snapshot | 真实 tokenizer 与 provider matrix |
| L3 Harness | versioned SWE Test Runner 与 Scheduler capability contract 已闭合 | 仓库脚本、真实双层 probe、RunContract、typed terminal、安全 snapshot、完整 ToolResult ContentStore | 外部 provider 多样性 |
| L4 Loop | SWE-016/017/023/028/032/033 主链已接入并验证 | 6 Attempts、唯一 Deadline Compiler、Invocation failure 中性进展、typed intervention、exploration→required-action deliverable phase、code-change/v3 4/10/18/24 阈值 | 更多 provider/cohort 统计 |
| L5 Graph | simple/current transaction、typed Context data port 与 recovery controller 已验证 | framework-owned simple Graph、零参数 validate/commit/start、Task.ContextInputs、typed outcome/outbox、blocked→recovery→new Activation、Acceptance input replay | generation/correlation token 仍不开放；legacy 仍待退役 |
| Docs / issue ledger | 已建立，持续同步 | 五层规范、正式设计、路线图、第5/6轮及 Responses 单题总账 | 只按各问题 closure matrix 关闭 SWE-011～028 |

当前迁移采用“canonical contract → durable authority → production cutover → legacy
删除”的顺序。任何暂时并存的旧路径都必须保留在上表的“尚未满足”列中，不能
因为新包可编译就从总账消失。

### 1.2 统一遗留分类（2026-08-22）

| 归属 | 当前遗留 | 下一实施/验证门 |
|---|---|---|
| 基础层 Model Invocation | Responses typed items 已 cutover；Chat compatibility 未删 | provider 对照 + SWE-014/027；compat 调用归零 |
| L1 Prompt | 单体约 52.9KiB Prompt 已退出生产；core + phase task-control prompt，各阶段只见对应工具 | 外部 cohort 统计，不再是实现缺口 |
| L2 Context | v7 Optional/RequiredExact、Raw History projection 与动态 reserve 已落地 | 真实 tokenizer、更多 provider replay fixture |
| L3 Harness | repo SWE Test Runner、双层 tool probe、真实 Lease、phase Router、typed terminal 已落地 | 外部 provider fixture 扩展 |
| L4 Loop | final Attempt 权利、deadline、failure-neutral progress 与 intervention scope 已钉住 | recovery controller 外部多题 rollout |
| L5 Graph | simple/current transaction 与 framework recovery controller 已落地；legacy submit/patch 未删 | SWE-033 定向复跑、调用计数归零、migration tests、SWE-012 |
| Validation / Trace 横切 | full/race/vet/build、真实二进制、仓库 SWE Test Runner 与单题双门已完成 | 一批8题、三平台 CI |
| 横切关注点 | legacy adapter、权限最小可见面与跨平台恢复证据 | 调用计数、migration tests、outbox replay/ack、lease/prompt bytes 对账 |

特别说明：正式两阶段 TerminalIntent 已保证新 ProgressContract Task 先 Seal 再
提交 Outcome/Task；`current_unsealed` 仍保留在 schema 中用于历史/legacy 事实，
读取时不得升级伪装为 sealed。到期未返回的 L4 reservation 只会标为
`ActionUnknown`；L3 Effect Journal 的 unknown 不会被重放或自动补偿。

### 1.3 本次证据口径

已执行并通过：

- Invocation/Context/Effect/Loop/Graph 各切片的 focused unit/contract/recovery tests；
- `go test ./internal/graph ./internal/store ./internal/outcome ./internal/outcomestore ./internal/terminaladapter -count=1`；
- Graph authoring/feed/outcome/output-contract/recovery 的 bootstrap focused tests；
- TerminalIntent/OutcomeStore/LoopStore 的两阶段、external-cancel、崩溃恢复与
  focused race tests；Mailbox Run/Session 分区、定向 wake/claim/drain/ACK、steer
  与 Trace Session correlation tests；
- `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build -o agentgo .`；
- 仓库 SWE Test Runner 33 项 Python 单测、`git diff --check`；
- 真实二进制 startup function-call probe、RunContract 注入、Graph
  Draft/Definition/StartIntent/typed outcome 与 TaskOutcome delivery ACK。

第六轮 DeepSeek 8题为0/8；Responses cutover 及后续五层修复后，
最终冻结二进制的真实 `deepseek-v4-flash` thinking 完整 cohort
达到 `architecture_ok=8/8`、`task_resolved=6/8`。两道普通模型
业务失败随后定向复跑均 resolved；原 cohort 统计不被改写。
SWE-041～045 的 exploration/Context/fulfillment/recovery/Observation 事故形状均为零，
所有 TaskOutcome 全 ACK。证据见
[`Responses 主链与单题成功`](../test-issues/2026-08-23-0109-responses-mainline-and-single-task-success.md)
、[`DeepSeek Responses 精确重放事故`](../test-issues/2026-08-23-0500-deepseek-responses-exact-replay.md)、
[`五层限制重组`](../test-issues/2026-08-24-0715-swe-layer-limit-recomposition.md)
与 [`Observation thinking replay`](../test-issues/2026-08-24-0735-observation-thinking-replay.md)。

后续单题再次暴露 Observation 状态退化、语义重复刷新 progress、过期 retry 与
TerminalSummary 失真；SWE-046～050 已完成 repository implementation，等待指定
监控任务重新编译并执行 8 题外部验证。记录见
[`Observation 状态与 Recovery 可启动性`](../test-issues/2026-08-24-1100-observation-state-and-recovery-feasibility.md)。

后续 cohort 证明 framework “Run 总预算”实际按 Task 重置、Observation
auto-singleton 仍可能生成错阶段工具、Recovery 缺少可转交的 execution grant；
SWE-051～053 已切换到 RunID durable Ledger、`swe/v3` 非收紧默认、独立 exact
Observation Control Invocation 与 RecoveryStartPermit。再后续的 provider 402
与 incomplete summary 暴露 SWE-054～056，现由 typed quota、unknown intervention
和 batch finally transaction 收口。provider 恢复后的最终当前批次为完整 8/8：
任务、架构、基础设施全部通过。记录见
[`Run 预算与 Control lane`](../test-issues/2026-08-24-2205-run-budget-and-control-lane.md)
与 [`Provider 额度与批次收口`](../test-issues/2026-08-25-1100-provider-quota-and-batch-finalization.md)。

## 2. 输入设计与问题映射

| 问题 | 主责 | 正式设计/处置 |
|---|---|---|
| SWE-014 | Model Invocation + L4 | [`Invocation Failure / Loop Recovery`](invocation-failure-and-loop-recovery.md) |
| SWE-013 | L2 Context | [`Context Snapshot / Item Budget`](context-snapshot-item-budget.md) |
| SWE-011 | L4 Loop | [`Loop Progress / Checkpoint / Deadline`](loop-progress-checkpoint-and-deadline.md) |
| SWE-012 | L5 Graph | [`Graph Draft / Commit / Start`](graph-draft-commit-start.md) |
| SWE-003 Graph JSON | L5 | 并入 SWE-012 原生 Draft patch |
| SWE-003 流式/输出 | Invocation/L2 | 并入 SWE-013 |
| SWE-003 无界恢复 | L4 | 并入 SWE-011/014 |
| SWE-003 structured output | provider adapter | 独立 capability 实验，非正确性前提 |
| SWE-004 Graph Outcome | L5 | 并入 SWE-012 typed EndOutcome |
| SWE-015 | L2 Context（Invocation 协同） | Response→Replay representability、Optional/RequiredExact disposition |
| SWE-016 | L4 Loop | Attempt budget 在 rollover/start 边界执法，保留最后 Attempt 执行权 |
| SWE-017 | L4 Loop | 唯一 Deadline Compiler 与合法 Recovery wake Checkpoint |
| SWE-018 | L1 Prompt | Scheduler phase Prompt/行动收敛，先做 cohort 因果对照 |
| SWE-019 | L3 Harness | Provider tool capability、真实 Scheduler Lease、阶段化 ToolRouter 与 batch cap |
| SWE-020 | 外部 SWE Test Runner / L3 观测 | 仓库内 versioned SWE Test Runner、RunContract、typed terminal 与双指标 |
| SWE-021 | L2 Context | typed/bounded RuntimeSnapshot 与 optional drop |
| SWE-022 | L2/Invocation | Context v4–v7 tokenizer/replay/completion/reasoning 预算版本化 |
| SWE-023 | L4 Loop | Run 总预算、6 Attempts、Invocation-failure 中性进展 |
| SWE-024 | L5 Graph | framework simple Graph 与 current-transaction cursor |
| SWE-025 | L5 Proposal Acceptance | singleton typed verdict tool 与三标量 wire contract |
| SWE-026 | L4→L5 | intervention Graph/Node/Activation typed control scope |
| SWE-027 | Model Invocation/L2 | Responses typed-item/SSE、required-argument probe、typed replay、正文不提升为工具 |
| SWE-028 | L4/L3 | verification exploration 超额进入 auto-singleton + L3 required-action submit phase，不直接 blocked |
| SWE-029 | Model Invocation/L2 | DeepSeek Responses required 空字段 raw exact replay |
| SWE-030/031 | Model Invocation/L3 | auto + singleton + L3 required-action 替代 exact/required choice；provider fan-out 只 dispatch 首个并为余下 call_id 保留 skipped result |
| SWE-032 | L4 Loop | rollover 用尽不再提前 intervention；新 code-change Task 冻结 v3，v1/v2 保留供历史恢复 |
| SWE-033 | L4→L5/L3 | recoverable blocked 进入 framework recovery Controller；Outcome ACK 后按最新 Definition 创建新 Activation，缺失 recovery 进入架构事故门 |
| SWE-045 | L3/L4/Invocation | 非终态 Observation 保持 thinking + auto-singleton，L3 强制 exact action；Invocation 失败进入两次有界 checkpoint 计数 |
| SWE-046 | L2 Context | Observation v2 predecessor/candidate 状态与最新 TaskMemory 投影 |
| SWE-047 | L4 Loop | Observation semantic advance 与连续 stale state intervention |
| SWE-048 | L5 Graph | recovery retry 的 execution phase 可启动性预检 |
| SWE-049 | L3/Invocation | phase action-contract rejection 与 malformed response 分离 |
| SWE-050 | L2/L5 finalization | TerminalSummary v2 的 task-published/workspace/settlement 事实 |
| SWE-051 | L4 / Run authority | RunID durable budget ledger、Activation-local progress 与显式 execution limit 分离 |
| SWE-052 | L3/Invocation | Observation 独立 reasoning=none + exact Control Invocation，不混入业务 replay |
| SWE-053 | L5/外部 SWE Test Runner | RecoveryStartPermit 与 Run ledger/trace/terminal reservation 架构门 |
| SWE-054 | L3/Invocation | provider quota/billing typed failure，与 429 rate limit 分离 |
| SWE-055 | L4 Loop | RecoveryRequestIntervene/Cancel 穷尽执行，unknown durable 交 L5 |
| SWE-056 | 外部 SWE Test Runner | startup typed infra error、batch finally summary 与 current-run 状态行 |

## 3. 实施总原则

1. **契约先于行为**：先定义 canonical DTO/identity/schema，再改变生产控制流。
2. **单一权威**：同一稳定事实只有一个 Store/类型；shadow 只对账，不成为第二
   权威。
3. **切换必须删除旧入口**：任何 adapter/feature gate 必须有 owner、退出条件和
   最晚删除 wave。
4. **fail-closed 只用于权威边界**：契约/预算/terminal/Effect 写失败停止；Trace/UI
   投影失败显示 degraded，但不抢写业务状态。
5. **冻结语义不破坏**：在途 Activation 保留冻结定义；Context/Lease/Policy 核心
   变化产生新 Attempt/Activation/revision。
6. **无固定 MaxLoops**：进展、预算、deadline、cancel 和终态共同控制 Loop。
7. **Graph 始终是请求控制面**：不恢复 direct-answer 生命周期，不让 Scheduler
   主动 author subgraph。
8. **副作用不重放**：prepared 未 settled 仍为 unknown；checkpoint/replay 不绕过
   Effect Journal。
9. **Session 不自动续跑**：历史会话非终态工作仍 blocked，迁移不借恢复路径偷偷
   重启旧任务。
10. **完成必须 E2E**：单测/编译成功不等于跨包装配完成。

## 4. 目标运行链路

```text
User/API/SWE Test Runner
  │ RunContract
  ▼
GraphDraft ── commit ──> GraphDefinition ── start ──> GraphExecution
                                                     │
                                                     ▼
                                                Activation
                                      冻结 NodeDefinition / refs
                                                     │
                                                     ▼
                                             L4 LoopController
                  ┌──────────────────────────────────┼──────────────────┐
                  │                                  │                  │
          ActionReservation                  ContextProvider       Cancel/Deadline
                  │                                  │                  │
                  │                          ContextSnapshot            │
                  │                                  │                  │
                  │                           Model Invocation          │
                  │                        typed result/failure          │
                  │                                  │                  │
                  │                            L3 Tool/Effect           │
                  │                                  │                  │
                  └────────────── TurnSettlementDelta ──────────────────┘
                                                     │
                                             ProgressAssessment
                                                     │
                                             ProgressCheckpoint
                                                     │
                                               TaskOutcome
                                                     │
                                      TaskOutcome → TerminalFact
                                                     │
                                        EndOutcome → GraphStatus
```

## 5. Canonical identity 链

所有新契约使用同一身份链：

```text
SessionID
  → RunID
    → GraphID
      → GraphRevision
        → ActivationID
          → TaskID
            → AttemptID
              → TurnID
                → InvocationID / ActionID
                  → DeltaID / AssessmentID / EffectID
```

### 5.1 生成与所有权

| Identity | 生成者 | 是否 durable | 是否可复用 |
|---|---|---:|---|
| RunID | request ingress | 是 | 同一用户请求内 |
| GraphID/Revision | L5 Draft/Definition | 是 | revision CAS |
| ActivationID | Graph Runtime | 是 | 绝不复用旧 activation |
| TaskID | Graph Board/Task Store | 是 | 一 activation 一确定性 Task |
| AttemptID | L4 Loop | 是 | 每次核心执行契约变化新建 |
| TurnID | L4 Loop | 是 | settled Turn 唯一 |
| InvocationID | Model Invoker | 是 | 每次请求唯一 |
| ActionID | Harness | 是 | 每次 tool/action 唯一 |
| Delta/AssessmentID | settlement/progress | 是 | 单调 sequence |

### 5.2 兼容纪律

旧 Trace 缺 AttemptID/RunID 时只显示 legacy/degraded，不从时间戳或文本伪造稳定
身份。新 Session/新任务必须完整写入；旧记录不可变。

## 6. 共享类型唯一权威

### 6.1 类型归属表

| 类型 | 唯一权威建议 | 消费方 |
|---|---|---|
| RunContract/DeadlineBudget | `internal/runcontract` | L4、L5、SWE Test Runner |
| InvocationContract/Failure/Usage | `internal/invocation` | llm adapter、L2、L4 |
| ContextFragment/Snapshot/Policy | `internal/contextcontract` | compiler、agent、trace |
| ProgressContract/Delta/Checkpoint | `internal/loopcontract` 或拆分小包 | L4、L3 adapters、L5 ref |
| TaskOutcome | neutral outcome package | agent/store/Graph adapter |
| TerminalFact | Graph terminal adapter | L5 Runtime |
| EndOutcome/GraphStatus | `internal/graph` | Graph/UI/SWE Test Runner |
| ContentRef | `internal/contentstore` | L2/L3/Graph adapters |

包名可在 Wave 1 评审时调整，但一个稳定概念不得同时在 `llm`、`agent`、`model`、
`bootstrap` 各定义一份近似 struct。

### 6.2 禁止依赖

- `internal/graph` 不 import `agent`、具体 LLM SDK 或具体 Tool Registry；
- `internal/invocation` 不 import L4/Graph；
- `internal/contextcontract` 不执行 Store/Tool；
- `ProgressEvaluator` 不读文件、不调模型、不改 Graph；
- Trace types 不成为 Domain DTO 的唯一运行输入；
- bootstrap 只组装 adapter，不重新定义业务语义。

## 7. 依赖图与并行边界

```text
Wave 0 事故 fixture / 文档总账
  │
  ▼
Wave 1 Canonical Identity + RunContract + InvocationFailure + TaskOutcome DTO
  │
  ├───────────────┬────────────────┐
  ▼               ▼                ▼
Track A          Track B           Track C
Invocation/L2    L4 Loop           L5 Graph
  │               │                │
  │ Failure/      │ Run/Attempt    │ Draft/Definition
  │ Snapshot      │ contracts      │ domain/validator
  └───────┬───────┘                │
          ▼                        │
 TurnSettlementDelta/Checkpoint    │
          │                        │
          └──────────┬─────────────┘
                     ▼
       TaskOutcome → TerminalFact → EndOutcome
                     │
                     ▼
       SWE Test Runner/UI/legacy cutover
```

Track A/B/C 可以在 Wave 1 后并行，但只能通过 canonical DTO/ref 协作。不得让某个
Track 为赶进度在本包复制对方尚未完成的类型。

## 8. Wave 0：基线与总账

### 8.1 交付

- 固定第五轮 8 题 summary、关键 Trace 和 TaskMemory fixture；
- 为 automatic-options/session-access-tracking/teardown-callbacks 建立 no-progress
  回归形状；
- 为 ipv6-server-name/automatic-options/pass-context-dispatch 固定 request timeout
  误分类；
- 为 secret-key-rotation 固定 end-only Graph；
- 固定 241K reasoning/275K assistant 的 oversized fixture；
- 固定 failed end → Graph completed 的 Outcome fixture；
- 建立包依赖和 canonical type inventory。

### 8.2 原始证据纪律

临时 `/tmp/agentgo-swe` 可能被覆盖。进入实现前，将必要的脱敏、最小事故形状转成
仓库 test fixtures；不得复制 API key、完整 reasoning、受版权保护仓库正文或巨大
日志。原始外部测试目录只作一次性取证，不成为测试依赖。

### 8.3 Gate

- fixture 能在未修代码上稳定复现预期红态；
- fixed 历史问题（路径边界、工具名清洗、Graph v2 outlet）保持绿；
- 文档状态无重复“open”和“subsumed”冲突。

## 9. Wave 1：契约内核

### 9.1 Run/Attempt/Turn identity

- 引入 RunID、AttemptID、TurnID；
- 贯穿 Task/Session checkpoint、Trace 和 UI snapshot；
- 不改变现有业务决策；
- 缺失身份显示 degraded。

### 9.2 Invocation contract

- 定义 FailureKind/Phase/Scope/UsageState；
- SDK adapter shadow 分类；
- 旧 ErrRecoverable 路径继续运行但做差异对账；
- 新 Trace 字段不驱动 Reactor。

### 9.3 RunContract/Deadline DTO

- request ingress 接受或生成 RunContract；
- 使用绝对 deadline；
- Graph/Activation/Attempt 先只计算 shadow budget；
- SWE Test Runner 能传入 Run deadline，但暂不依赖其终态。

### 9.4 TaskOutcome/TerminalFact DTO

- 固定 completed/failed/blocked/cancelled 和 structured Result/Evidence refs；
- 当前 terminal bridge 旁路生成 shadow TerminalFact；
- 对账现有 Task/Graph 状态，不抢写。

### 9.5 Gate

- canonical DTO 无 import cycle；
- snapshot/export/import property tests 更新；
- 新身份和旧任务兼容；
- shadow 事实与当前状态可解释对账。

## 10. Track A：Model Invocation + L2 Context

### A1：absolute response ceiling

- SSE content/reasoning/ExtraFields/tool name/tool args/total byte counters；
- 不允许配置为无限；
- partial tool call no-dispatch；
- typed `output_limit_exceeded`；
- 不将 partial 写入 History。

这是正式 InvocationContract 的第一条安全边界，不是临时关键词止血。

### A2：typed failure cutover

- context/cancel cause 分层；
- provider code/HTTP/finish reason/transport/decode 分类；
- L4 shadow RecoveryDecision；
- 删除字符串 `isContextOverflow` 控制流；
- transport retry 同 Snapshot，context overflow 才 rebuild。

### A3：Content Store / Context contract

- FragmentKind/Disposition/AtomicGroup/Policy；
- ContentRef/scope/retention；
- ToolResult/Result 大正文外置；
- policy catalog 和 config doctor；
- provider replay policy fixtures。

### A4：ContextCompiler shadow

- 从现有 buildMessages 输入生成 shadow Snapshot；
- actual request digest 对账；
- stable ordering；
- ToolRouter visible/runtime snapshot identity；
- 记录所有超限但不改变正常范围内 payload。

### A5：ContextCompiler cutover

- Messages/ToolSpecs/Manifest 只从 WireItem 生成；
- 每项/group/section/total/completion reserve enforcement；
- 退役独立 buildMessages/buildContextManifest 权威；
- History 改为 settled source/ref + view，不保存 partial。

### A6：退出条件

- 生产调用不再直接拼 message；
- 旧 Manifest 影子 builder 无调用方；
- 旧字符串错误分类无调用方；
- provider required replay fixtures 全绿；
- oversized fixture 在进入 History 前被阻止。

## 11. Track B：L4 Loop

### B1：ProgressContract compiler

- WorkClass/signal/policy catalog；
- Scheduler 只声明 Draft/policy_ref；
- framework 编译 CompiledProgressContract；
- Graph/Task 先保存 opaque ContractRef/digest；
- change 任务不能只有无限 read progress。

### B2：TurnSettlementDelta shadow

- 投影 Invocation、ToolCallRecord、Effect、Artifact、file version、evaluation、input；
- stable cursor/sequence/digest；
- ProgressEvaluator 纯函数 shadow assessment；
- 重复、振荡和 Shell 同结果 fixture。

### B3：ProgressCheckpoint

- Checkpoint CAS；
- action reservation ledger；
- retry/reclaim/restart 连续；
- absolute deadline 恢复；
- 权威写失败 fail-closed。

### B4：分级干预

- Reminder；
- AttemptRollover；
- typed LoopInterventionRequested；
- blocked(no_progress_budget_exhausted)；
- caller cancellation authority 优先；
- 与 L5 Graph adapter 对接。

### B5：Watchdog 收敛

- heartbeat/checkpoint lease；
- HardDeadlineAt；
- Runner/Agent 失联和拒绝取消；
- 旧 TimeoutSeconds 迁移为 ExpectedDuration；
- 旧文本 watchdog wake 设置退出条件。

### B6：退出条件

- `processTask` 不再包含主要 Context/Store/Graph 决策；
- retry 不重置 ProgressCheckpoint；
- 外部 Run deadline 前完成内部 intervention/terminal；
- emergency fuse 仍只作程序缺陷保险；
- 40+ 调用零写入 fixture 在预算内 blocked。

## 12. Track C：L5 Graph

### C1：GraphDraft domain

- DraftStore/GraphContract/ValidationReport；
- create/read/native patch；
- commit 前零 Activation/Task/Effect；
- Draft identity 与 Session scope。

### C2：Compiler/Validator

- 当前 Graph v2 validation 迁入 Definition compiler；
- 最小合法图；
- root 非 end、zero-work/zero-result 拒绝；
- EndOutcome；
- GraphContract coverage；
- Proposal Acceptance port/fake；
- ProgressContractRef/ContextPolicyRef 可编译性检查。

### C3：Commit/Start

- commit durable Definition，零激活；
- Scheduler read/决定 start；
- start 幂等创建 root Activation；
- start success 后 origin finalizing；
- crash recovery 不激活半构造图。

### C4：Authoring/ChangeProposal

- Scheduler 使用小型原生结构工具；
- 删除完整 Graph JSON-in-JSON 新图路径；
- 运行中修改经 ChangeProposal/CAS；
- 只影响未来 Activation；
- Scheduler 暂不 author subgraph。

### C5：Outcome/Terminal

- TaskOutcome→TerminalFact 唯一 adapter；
- transition 到 end 与 EndOutcome settlement 同一 durable 事务；
- GraphStatus 推导 success/failed/blocked/cancelled；
- graph-ended/UI/SWE Test Runner 使用 typed outcome；
- `graph_done` 不再暗示业务成功。

### C6：退出条件

- 新 v2 Graph 无 submit即activate 入口；
- 新 authoring 无完整 Graph JSON 字符串；
- end ID/title 不参与 outcome 推断；
- failed/blocked Graph 不显示 completed；
- v1 仅兼容跑完，不产生新 authoring；
- 无生产调用方后删除旧 submit/patch 路径。

## 13. Track D：Provider capability 实验

本 Track 对应 SWE-003 唯一未并入核心设计的残余，不阻塞 A/B/C 正确性。

### 13.1 Capability contract

```text
supports_strict_function_schema
supports_response_format_json_schema
supports_streaming_structured_output
supported_json_schema_subset
supports_structured_refusal
preserves_tool_call_streaming
```

能力按 provider/model/version/protocol fixture 建立，不能从“OpenAI-compatible”或
provider 名称推断。

### 13.2 实验矩阵

- strict function schema：小/中/边界参数；
- response_format JSON Schema：结构化文本；
- tools + streaming；
- refusal/content filter；
- schema subset 和 unsupported keyword；
- malformed rate、valid rate、latency、tokens；
- provider 忽略 strict 的检测；
- fallback 到 framework validator/hard cap。

### 13.3 启用原则

- GraphDraft 原生工具优先使用验证过的 strict function schema；
- response_format 不全局应用到 ReAct tool-calling 请求；
- 不支持/忽略 strict 时 fail capability probe，但核心系统继续用 validator；
- capability 变化产生新 InvocationContract/Attempt；
- 实验失败不重新打开完整长 Graph JSON 路径。

## 14. 跨 Track 集成门

### Gate I：Invocation ↔ Context

- Failure 绑定 SnapshotID/PolicyID；
- context-window/output-limit/request-timeout 分类不同；
- partial 不入 History；
- provider replay 原子组完整。

### Gate II：Context ↔ Loop

- Loop 只经 ContextProvider 获取 Snapshot；
- Context rebuild 有 parent ref/reason；
- compile/invocation 结果进入 TurnSettlementDelta；
- retry/no-progress budget 正确 charge。

### Gate III：Loop ↔ Graph

- Activation 冻结 ProgressContractRef/ContextPolicyRef；
- L4 只产生 TaskOutcome/InterventionRequest；
- L5 不读取 History 猜终态；
- blocked/failed 路由与 Result/Evidence 同源。

### Gate IV：Graph ↔ SWE Test Runner/UI

- lifecycle terminal 与 business outcome 分离；
- UI 展示 Draft/Definition/Execution/Outcome；
- SWE Test Runner 可在 typed terminal 后提前 judge；
- final user report 与 Graph outcome 一致。

## 15. Shadow、Cutover 与删除纪律

### 15.1 Shadow 规则

Shadow 只允许：

- 读取同一权威输入；
- 计算新 digest/assessment/DTO；
- 写独立测试/对账记录；
- 不改变状态、权限、请求字节或路由。

Shadow 不得成为 Reactor 输入，不得在恢复时抢写旧权威。

### 15.2 Cutover 规则

每个 cutover PR 必须列：

```text
old_authority
new_authority
adapter
comparison evidence
rollback condition
deletion issue
latest deletion wave
```

### 15.3 禁止永久双轨

- buildMessages 与 ContextCompiler 不能永久同时发送；
- ErrRecoverable 与 InvocationFailure 不能永久同时决策；
- submit_graph 与 Draft/Commit/Start 不能同时作为新图入口；
- Graph title-based outcome 与 EndOutcome 不能并存；
- TaskMemory 与 ProgressCheckpoint 不能互相抢写控制状态。

## 16. 持久化与事务顺序

### 16.1 单 Turn

```text
reserve action budget
  → invoke/execute
  → settle raw Tool/Effect/Artifact facts
  → append TurnSettlementDelta
  → compute ProgressAssessment
  → CAS ProgressCheckpoint + settle reservation
  → decide continue/recovery/terminal
```

### 16.2 Task terminal

```text
final Harness/Effect/Artifact settlement
  → sealed ProgressCheckpoint
  → durable TaskOutcome
  → Task terminal state
  → terminal outbox
  → Graph TerminalFact adapter
```

Artifact flush 失败、TaskOutcome 结构化提交失败或 terminal outbox 写失败必须
fail-closed，不得标 completed。

### 16.3 Graph terminal

```text
activation TerminalFact
  → evaluate selected edge
  → transition/end settlement
  → EndOutcome
  → GraphStatus
  → graph-ended outbox
```

transition/end/outcome 必须同一 durable 事务或具备幂等恢复记录，不能先显示
completed 再靠 Scheduler 文本更正。

## 17. Legacy 与数据迁移

### 17.1 Graph

- v1 存量图按 v1 语义跑完；
- 新图只走新 v2 Draft authoring；
- 旧 v2 已运行图保持冻结 activation；
- patch 只影响未来 Activation 的不变量保留；
- legacy authoring 设置可观测调用计数，归零后删除。

### 17.2 Task/Session

- 旧 Task 缺 Run/Attempt/Progress identity 显示 legacy/degraded；
- 不自动恢复旧非终态 Task；
- 新用户输入可建立新 Run/Graph；
- Session snapshot schema 迁移需 property tests 和 Windows close-before-cleanup。

### 17.3 Trace

- 历史 Trace 不重写；
- CLI 对缺字段明确显示 unknown/legacy；
- 新 Trace 只从新 Domain facts 投影；
- rollout 对比必须使用新 Session。

## 18. 测试分层

| 层 | 必需测试 |
|---|---|
| Invocation | httptest/SSE、cause、provider code、output cap、usage state |
| Context | Fragment fixture、atomic group、budget、digest、replay、ContentRef |
| Harness | Lease/ToolRouter、Effect、Artifact、Content Store recovery |
| Loop | retry/cancel/deadline/progress/checkpoint/intervention/terminal |
| Graph | Draft/CAS/commit/start/activation/change/outcome/recovery |
| Adapter | TaskOutcome→TerminalFact、Graph→SWE Test Runner/UI、outbox |
| Cross-platform | Windows file handle/path/shell/compile，Linux/macOS runtime |

所有纯函数 evaluator/compiler 测试不调用真实模型。provider capability 单独标记为
有成本的 integration suite，不混入普通 `go test ./...`。

## 19. 每个切片的交付门

任何非平凡切片报告完成前：

1. focused unit/contract tests；
2. 相关 recovery/idempotency tests；
3. `go test ./... -count=1`；
4. 必要时 `go test -race`；
5. `go vet ./...`；
6. `go build ./...`；
7. Windows compile/CI；
8. 真实二进制启动；
9. 走一遍新跨包路径；
10. 断言预期 durable 产物/事件/文件；
11. `git diff --check`；
12. bug 切片同步更新 `docs/activate/KNOWN_ISSUES.md`。

## 20. SWE Test Runner 改造

### 20.1 RunContract

注入请求时传入：

```text
run_id
deadline_at
finalization_reserve
recovery_reserve
budget_profile
```

外部 hard kill 晚于内部 Run deadline，只作为进程失活兜底。

### 20.2 终态口径

结果分开记录：

```text
process_terminal
graph_lifecycle_terminal
graph_outcome
task_outcomes
judge_verdict
```

`graph_done` 不再同时承担“图停止”和“业务成功”。

### 20.3 成本与收敛指标

每题至少记录：

```text
wall time
LLM calls
prompt/completion tokens
Invocation failures by kind
Context transforms/drops/rejections
no-progress assessments/interventions
tool/effect/artifact/file deltas
Attempt count
Graph revisions/activations
patch lines
judge delta vs baseline
external hard kill
```

## 21. SWE 回归门槛

### 21.1 必须消失的事故形状

- 40+ 调用、零写入、仍 processing；
- 241K/275K item 原样进入下一 Context；
- partial/malformed tool call 被 dispatch；
- request timeout 被当 context-window overflow；
- end-only/zero-work Graph completed；
- failed/blocked end 显示 Graph completed；
- 外部 1200 秒 kill 是第一个正常终止者；
- retry/重启重置 no-progress budget；
- 完整长 Graph JSON 多次损坏仍继续同路径。

### 21.2 必须保持的能力

- 合法复杂任务可持续推进，不受固定轮数截断；
- Graph back-edge/new Activation 语义保持；
- ResultRef/EvidenceRef/Artifact lineage 可解引用；
- Gate/Lease/path/read-before-write/Effect fail-closed 不退化；
- v1 存量 Graph 可按兼容策略跑完；
- Session 不自动续跑；
- Windows/macOS/Linux 支持保持。

### 21.3 对照纪律

候选与基线保持：

- 相同 8 题和 test patch；
- 相同 provider/model；
- 相同 Prompt cohort；
- 相同权限、工具、网络、时间和 token budget；
- 相同 judge/baseline；
- 多 rollout，不以单次随机通过宣告完成。

## 22. Issue closure 矩阵

| Issue | 可关闭条件 |
|---|---|
| SWE-014 | InvocationFailure 全链路 + 字符串分类退出 + E2E |
| SWE-013 | ContextCompiler 唯一权威 + item/output cap + replay/Ref E2E |
| SWE-011 | ProgressCheckpoint/enforcement + deadline/RunContract + no-progress SWE |
| SWE-012 | Draft/Commit/Start + native authoring + EndOutcome + recovery E2E |
| SWE-003 | 被吸收切片关闭，structured-output 实验结论落档；不要求 provider 必须支持 |
| SWE-004 | SWE-012 typed EndOutcome/SWE Test Runner/UI 落地后关闭 residual |
| SWE-015 | Response commit + Replay v2 Optional/RequiredExact + Raw History 不变 |
| SWE-016 | Attempt 只在 start/rollover 边界执法，最后 Attempt 保留完整 Turn 权利 |
| SWE-017 | 唯一 Deadline Compiler，Recovery/Finalization 使用合法阶段窗口 |
| SWE-018 | Scheduler core/phase Prompt 与 phase ToolRouter；外部 cohort 作为效果证据 |
| SWE-019 | 真实 Lease、function-call probe、batch cap、advertise/dispatch 同源 phase Router |
| SWE-027 | Responses typed item + required nonce probe + DSML message no-dispatch + 真实 provider E2E |
| SWE-028 | novel evidence 不 blocked + exact deliverable ToolRouter + acceptance success E2E |

任何 issue 的设计完成、shadow 通过或包单测通过都不足以关闭。

## 23. 后续实施顺序

canonical contract、durable authority 和主要 production cutover 已完成，后续不再
按旧“首批 shadow PR”排序。统一剩余顺序为：

1. **legacy 退出**：用调用计数和 migration tests 删除旧 Graph submit/direct
   patch、legacy error/recovery 与重复 Context 权威。
2. **provider/cross-platform fixtures**：structured error/replay/strict schema、
   Windows/Linux/macOS、race/vet/build。
3. **真实二进制 E2E**：启动新路径，断言 TerminalIntent/Outcome/Checkpoint/
   Graph/Context/Effect/Mailbox
   durable 产物与 outbox ack。
4. **Flask SWE 多 rollout**：同模型、同 Prompt cohort、同预算运行 8 题并按
   §20/§21 口径决定 issue closure。

## 24. 风险与防护

| 风险 | 防护 |
|---|---|
| shadow 变永久双轨 | 每项指定删除 wave 和调用计数 |
| 过严 Context cap 误伤成功题 | shadow 分布、类型 policy、Ref/atomic group fixture |
| ProgressEvaluator 被活动欺骗 | digest 去重、振荡检测、deliverable 双时钟 |
| deadline 误杀长任务 | 绝对层级、reserve、真实进展可续但受 Run authority |
| Checkpoint 写放大 | 有界状态、CAS、每 settled Turn 一次、无重复 fsync |
| Graph 改造重复激活 | commit/start 事务、幂等 key、recovery fixture |
| provider strict 被忽略 | capability probe、request/response fixture、validator 兜底 |
| adapter 形成新 God object | 小端口、包依赖测试、单一类型权威 |
| 回放重放副作用 | Effect Journal、只重建计算状态、显式 compensation |

## 25. 路线图完成定义

本路线图只有在以下条件全部满足时才能标记 Implemented：

1. 四份 Accepted Design 的完成定义均满足；
2. SWE-003/004 residual 按 closure 矩阵关闭；
3. canonical identity/type 无重复权威；
4. 旧 message/error/Loop/Graph authoring/outcome 路径达到删除条件并退出；
5. 所有持久化 schema、恢复、Session 和 legacy 测试通过；
6. 全仓/race/vet/build/Windows/真实二进制验证通过；
7. SWE Test Runner 使用 RunContract 和 typed Graph outcome；
8. 多 rollout 8 题回归达到约定正确性和成本门槛；
9. 不再出现第五轮同型事故；
10. `docs/activate/KNOWN_ISSUES.md` 只保留仍可复现的真实开放问题；
11. `AGENTS.md` 仅在运行时契约实际落地后按切片同步，不提前宣称目标设计已实现。

当前状态为“Responses/五层主链实现、仓库验证与外部架构门完成，
legacy 退出仍开放”。已有 full/race/vet/build/真实二进制、DeepSeek 8 题
`architecture_ok=8/8 / task_resolved=6/8` 以及两道失败题定向 resolved 证据，
不再使用 compile-only 作为完成证据。由于 legacy Graph/Chat 退出、
三平台 CI 与部分早期 issue closure 仍未全部满足，路线图整体仍不标为
Implemented；SWE-041～045 子集已关闭。
