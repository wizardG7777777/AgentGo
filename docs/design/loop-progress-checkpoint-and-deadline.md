# Loop Progress Contract / Checkpoint / Deadline 架构

> 状态：Accepted Design，SWE-016/017/023/026 implementation complete / single-task architecture verified<br>
> 日期：2026-08-22<br>
> 归属：L4 Loop Engineering<br>
> 对应问题：SWE-011<br>
> 上位规范：[`五层工程架构规范`](five-layer-engineering-architecture.md)<br>
> 关联设计：[`Graph Draft / Commit / Start`](graph-draft-commit-start.md)<br>
> 统一路线图：[`SWE 架构修复统一实施路线图`](swe-architecture-repair-roadmap.md)

## 0.1 2026-08-22 实施状态

### SWE-016 / SWE-017 边界修订

第六轮证明初版 enforcement 在两个边界漂移。现已完成正式修订：

- `Attempts` 只在创建/rollover 到新 Attempt 前检查；`used == limit` 表示当前
  最后一个合法 Attempt 已获得完整执行权，不再进入每 Turn exhaustion 谓词；
- 超额 Attempt 在写 LoopStore 前拒绝，不污染 durable Checkpoint；
- `runcontract.CompileDeadlines` 成为 Run/Graph/Activation/Attempt 唯一生成器，
  与 `ValidateChildDeadline` 同源校验；
- execution/recovery/finalization 分别消费 execution window、recovery reserve、
  finalization reserve；相邻 scope 保留 1 秒 durable settlement/cancel/checkpoint
  handoff，而不是用相等边界或纳秒补丁；
- Model/Tool action deadline 同样提前一个 handoff reserve；L4 reservation 的剩余
  completion tokens 作为本次 Invocation OutputBudget 下传。
- framework Run 总预算不再复用 `MaxNoProgressUsage`；`swe/v1` 冻结6个 Attempt，
  与5次 retry 对齐，仍由64 calls/400K completion/19分钟总预算约束；
- typed Invocation failure 形成 `ProgressInvocationFailure`：扣累计 Run usage 并
  触发 Attempt recovery，但暂停 no-progress turns/duration/usage，不能把 provider
  截断/畸形伪装成 Agent 空转；
- intervention wake 持久化独立 Graph/Node/Activation control scope；wake 自身
  GraphID 保持空，Scheduler 据 typed scope 区分新 Draft 与现有 Graph recovery。

已进入生产主链：版本化 Run/Attempt identity、ProgressContract catalog/compiler、
TurnSettlementDelta/ProgressEvaluator、append-only LoopStore、action reservation 与
settlement、ProgressCheckpoint 连续性、绝对 deadline 层级、no-progress
reminder/rollover/intervention facts、在 TaskOutcome/Graph settlement 后才物化
deterministic Scheduler coordination wake 的 reliable intervention bridge，以及只观察
checkpoint 的 Watchdog liveness backstop。bridge 不以 `PublishTask` 当作消费确认；
只有 wake 自身的 durable TaskOutcome delivery 完成后才 Ack 原 intervention。相关
focused、恢复和包测试已通过；全仓 compile-only 已通过。

两阶段 TerminalIntent 已进入生产主链：锁内 durable Prepare/fence 并取消当前
执行 context；锁外在 `min(5s, Attempt hard deadline)` 内等待 action settlement，
随后 Seal ProgressCheckpoint；仍未返回的 reservation 以同一 `terminal_seal`
journal record 标为 `ActionUnknown`，绝不重放 Effect；回锁后以原 status、Attempt、
agents、results 做强 CAS，从最新 ToolCall/Artifact ledger 重新装配 Evidence，再
fsync TaskOutcome/outbox 并提交 Task 终态。崩溃后在 dispatcher/Runner 启用前先
恢复 pending intent。`current_unsealed` 仅保留为历史/兼容 schema 状态，新
ProgressContract Task 的正式 coordinator 不再以它提交终态。

full/race/vet/build、真实二进制与最新 Flask 单题已重跑；最新 Run 的已知架构事故
为零，Graph task Outcome/ACK 完整。任务因 Worker 未产生写入而被 code-change
ProgressContract typed blocked，8题未运行，因此 SWE-011 仍按多题 closure 条件
保持 `implementation-landed / validation-open`。

## 1. 决策摘要

本设计为单个 Agent Activation 建立正式的进展、预算、checkpoint 和 deadline
控制面，使模型可以短暂试错，但不能在没有目标进展时无界消耗模型调用、工具、
时间或上下文。

本设计固定以下架构决策：

1. SWE-011 的主责层是 L4 Loop；L3 提供可信执行事实，L5 消费有类型介入请求，
   L1/L2 只负责解释与上下文呈现。
2. 不恢复固定 `MaxLoops`；循环是否继续由真实进展、预算、deadline 和终态共同
   决定，10000 轮 emergency fuse 继续只作为程序缺陷保险丝。
3. 进展契约采用“架构定义语言与判定机制、Scheduler 声明任务意图、框架编译
   校验并冻结”的权责模型。
4. Scheduler 只能选择框架提供的 `policy_ref`、声明工作类别、交付物与验收目标；
   不能自定义无限阈值，也不能把模型文本声明为可信进展。
5. Scheduler 提交的是 `ProgressContractDraft`；框架在 Graph commit/Task 创建时
   生成 `CompiledProgressContract`，随 Activation 冻结。
6. 原工作名 `ObservationDelta` 正式定名为 `TurnSettlementDelta`：它只记录一次
   settled Turn 相对上个 checkpoint 新发生的结构化事实，不直接声称“有进展”。
7. L4 使用 `ProgressEvaluator` 将 `TurnSettlementDelta + CompiledProgressContract`
   映射成 `ProgressAssessment`。
8. L4 的控制状态持久化为独立 `ProgressCheckpoint`；它不属于 L2 Task Memory，
   retry、重新认领、重启或 Context 压缩不得把累计停滞状态清零。
9. 每次昂贵 action 前必须先预留预算；每个 settled Turn 后必须先 durable 写入
   Delta、Assessment 和 Checkpoint，成功后才能发起下一次 action。
10. ProgressCheckpoint 或预算预留的权威写入失败时 fail-closed，不允许仅打日志
    后继续消耗资源。
11. deadline 使用绝对时间并分层：operation < Attempt < Activation < Graph < Run；
    外部 harness deadline 必须作为 `RunContract` 进入系统。
12. `TimeoutSeconds` 不再同时表示预期时长、告警时间与硬 deadline；迁移后拆为
    `ExpectedDuration`、`InterventionAt`、`HardDeadlineAt` 和 reserve。
13. no-progress 使用分级状态机：提醒 → Attempt rollover → 有类型介入请求 →
    blocked；`cancelled` 保留给用户或系统主动撤销。
14. Watchdog 退回基础设施活性哨兵，负责缺失 heartbeat/checkpoint、执行者失联
    和拒绝取消，不再承担正常语义进展判断。
15. checkpoint + append-only Delta 支持恢复、重放、诊断分支和未来回溯；回退
    AgentGo 计算状态不得静默撤销或重放已经发生的 Effect。

## 2. 问题定义

### 2.1 第五轮事故

第五轮 Flask SWE 批测中：

- `automatic-options`：61 次 LLM 调用、零文件、零补丁，外部 timeout 时节点仍
  `processing`；其中 Scheduler 16 次，worker 45 次。
- `session-access-tracking`：72 次调用；worker 66 次，包含
  `read_file×23`、`run_shell×22`、`grep_search×18`，零 write/edit。
- `teardown-callbacks`：49 次调用；worker 46 次，零 write/edit/result。
- 多个 Attempt 发生纯文本退出、malformed/DSML 工具调用、请求 deadline 和
  RetryRollback，但没有形成有效代码修改。

这些行为不是单纯“模型回答质量差”。L3 Gate、路径边界、工具 schema 和结构化
终态检查正确拒绝了坏动作；真正的系统性问题是 L4 在拒绝之后只能“再给模型
一轮”，没有机械判断目标是否推进，也没有在外部 deadline 前完成有界介入。

### 2.2 当前 Loop 的实际出口

`processTask` 当前主要通过以下路径退出：

1. 结构化终态或自然完成；
2. 父 context 取消；
3. 执行错误、RetryRollback 或重试耗尽；
4. 空响应/纯文本退出的局部 streak；
5. 10000 轮 emergency loop fuse。

只要模型持续产生 syntactically valid 的 read/grep/shell 调用，就能避开空响应与
纯文本退出 streak。循环没有比较“调用前后，用户目标的可观测状态是否改变”。

### 2.3 deadline 倒置

第五轮实际时间配置为：

```text
LLM invocation timeout = 300 秒
shell timeout          = 300 秒
Graph Task timeout     = 默认 3600 秒
外部 SWE harness       = 1200 秒
Scheduler Task timeout = 86400 秒
```

Graph Task 未显式提供 `TimeoutSeconds`，公告板补成默认 3600 秒；Watchdog 超过
该值只告警、不取消。Agent 认领任务后也没有据此创建 Task deadline context。
因此内部控制面还未开始介入，外部 harness 已在 1200 秒直接终止进程。

### 2.4 现有状态不能直接充当进展

Task Memory 已具备结构化事实和重复读取去重，但它服务 L2 Context，不是 L4
控制状态：

- `automatic-options` 的 Task Memory version 达 20，但 `files=0`、`facts=0`；
- `session-access-tracking` version 达 40，但 `files=0`、`facts=0`；
- 不同目标的 read/grep、重复 Shell、失败尝试仍可能推进 Task Memory version；
- Task Memory update 没有驱动 Loop 收敛。

同理，settled Effect 只证明动作执行过。`session-access-tracking` 的 22 次
`run_shell` 形成 22 对 prepared/settled Effect，却没有产生代码交付。因此不能
把“新 Effect”“ToolCalled=true”“TaskMemory version 增长”直接定义成进展。

## 3. 目标与非目标

### 3.1 目标

1. 对没有目标进展的资源消耗给出可证明的时间、token、action 和 Attempt 上界。
2. 允许复杂任务持续运行，只要它仍产生契约认可的真实进展。
3. 让 retry、重启、重新认领和历史压缩无法逃逸累计停滞预算。
4. 在外部 Run deadline 前留出介入、Graph settlement 和最终汇报窗口。
5. 让每次进展判断可由 durable 事实重放和解释，不依赖模型自述或日志字符串。
6. 将局部 Loop 收敛与 Graph 级 replan/change 分离，通过有类型命令桥接。
7. 为未来 checkpoint replay、诊断分支和执行回溯提供稳定事件基础。

### 3.2 非目标

1. 不保证模型第一次编辑一定正确；正确性仍由验证和 acceptance 判断。
2. 不以 no-progress 机制替代 L3 Gate、ExecutionLease、Effect Journal 或路径边界。
3. 不自动撤销已经发生的外部副作用；撤销需要显式 compensation。
4. 不把 Trace 变成权威 Progress Store。
5. 不让 Scheduler 在 Prompt 中随意定义判定代码、表达式或无限预算。
6. 不把所有任务强制成代码修改任务；调查、验证和协调有独立 WorkClass。
7. 不通过降低 emergency fuse 或恢复固定 `MaxLoops` 完成本设计。

## 4. 分层责任

| 边界 | 拥有 | 不拥有 |
|---|---|---|
| Model Invocation 基础层 | 单次模型请求 deadline、typed failure、usage | Loop 是否继续 |
| L1 Prompt | 解释进展缺口和下一动作纪律 | 进展权威、预算和停止 |
| L2 Context | 渲染有界 Progress 摘要和恢复提示 | Progress 判定、Checkpoint |
| L3 Harness | Tool/Effect/Artifact/File/Evaluator 的 settled 事实和 Store | 是否构成目标进展 |
| **L4 Loop** | Contract 执行、ProgressEvaluator、预算、deadline、干预、Checkpoint | Graph topology |
| L5 Graph | 工作意图、Graph/Node 输出契约、介入后的 replan/change/路由 | 逐 Turn 进展判断 |
| Validation/Trace | 合约校验与脱敏投影 | 权威控制状态 |

一个关键边界是：L3 说“文件 hash 从 A 变成 B”，L4 才能结合“这是 code_change
节点且交付物范围是 `src/flask/**`”判断它是否构成 deliverable progress。

## 5. Progress Contract 权责模型

### 5.1 ProgressContractDraft

Scheduler 解释用户目标和 Graph 节点职责后提交草案。建议字段：

```text
work_class
deliverables[]
verification_targets[]
milestones[]
policy_ref
extensions
```

示例：

```json
{
  "work_class": "code_change",
  "deliverables": [
    {"kind": "file_delta", "scope": "src/flask/**"}
  ],
  "verification_targets": [
    {"kind": "evaluation", "id": "focused_tests"},
    {"kind": "evaluation", "id": "full_suite"}
  ],
  "policy_ref": "bounded_code_change/v1"
}
```

Scheduler 负责声明“要完成什么”，但不能声明：

- 任意模型文本等于进展；
- 任意 Effect settled 等于进展；
- 不受 RunContract 限制的 timeout/token/action 数；
- 关闭 checkpoint、deadline 或 intervention；
- 自定义执行代码或自由文本判定表达式。

### 5.2 框架封闭词表

首版 `WorkClass`：

```text
code_change
investigation
verification
coordination
external_effect
```

首版 `ProgressSignalKind`：

```text
file_version_changed
artifact_registered
artifact_version_changed
evaluation_changed
evaluation_passed
novel_evidence
confirmed_fact_added
blocker_cleared
input_revision_advanced
result_field_set
external_effect_settled
```

词表由框架版本化。未知 WorkClass、signal 或 policy 必须在 commit/publish 阶段
fail-closed，不能降级为“有任何工具调用就算进展”。

### 5.3 CompiledProgressContract

框架将 Draft、GraphContract、节点 OutputContract、Capability 和 RunContract
编译成执行契约：

```go
type CompiledProgressContract struct {
    ContractID          string
    ContractDigest      string
    WorkClass           WorkClass
    Deliverables        []DeliverableRule
    VerificationTargets []VerificationRule
    AcceptedSignals     []ProgressSignalRule
    Policy              ProgressPolicy
    RunBudgetRef        string
}
```

编译器必须：

1. 校验 deliverable/verification 可由当前 Harness 观测；
2. 将声明范围规范化并写入 digest；
3. 选择框架安全范围内的 policy；
4. 拒绝 change 节点只有 read-only 信号、却没有任何交付或验证规则；
5. 拒绝 verification target 没有对应 evaluator/命令身份；
6. 将最终 Contract 随 Activation 定义冻结。

Graph revision 只影响未来 Activation。在途 Activation 不接受 Scheduler 动态
修改 ProgressContract；需要改变时走 GraphChangeProposal 或结束当前 Attempt。

## 6. TurnSettlementDelta

### 6.1 定义

`TurnSettlementDelta` 是一次 Turn/action 完成结算后，相对前一个 durable cursor
新发生的中性运行事实。它不是 Prompt 内容，也不是进展结论。

```go
type TurnSettlementDelta struct {
    DeltaID       string
    Sequence      int64
    TaskID        string
    ActivationID  string
    AttemptID     string
    TurnID        string
    PreviousRef   string
    ContractDigest string

    FileChanges       []FileChange
    ArtifactChanges   []ArtifactChange
    EffectSettlements []EffectSettlement
    EvaluationChanges []EvaluationChange
    EvidenceChanges   []EvidenceChange
    BlockerChanges    []BlockerChange
    InputChanges      []InputChange

    UsageDelta    UsageDelta
    Failure       *TypedFailure
    SettledAt     time.Time
}
```

字段只保存有界元数据、稳定 Ref 和 digest；大正文留在对应权威 Store，通过 Ref
解引用。Delta 不复制 ToolResult、Artifact 或 Effect Journal 的第二份全文。

### 6.2 来源

L3 在 Turn settlement 边界从以下权威事实投影 Delta：

- ToolCallRecord 新游标；
- Effect Journal prepared/settled/unknown；
- Artifact ledger 增量；
- 文件版本与内容 hash；
- evaluator/test result 的结构化签名；
- blocker 状态变更；
- 上游 Result/Input revision；
- Model Invocation usage 和 typed failure。

Delta projector 只陈述“发生了什么”，不得写 `progress=true`。

### 6.3 示例

重复读取：

```json
{
  "evidence_changes": [{
    "kind": "file_read",
    "ref": "src/flask/app.py",
    "digest": "sha256:aaa",
    "novel": false
  }]
}
```

真实编辑：

```json
{
  "file_changes": [{
    "path": "src/flask/sessions.py",
    "before_hash": "sha256:aaa",
    "after_hash": "sha256:bbb"
  }]
}
```

重复测试：

```json
{
  "evaluation_changes": [{
    "evaluation_id": "focused_tests",
    "before_digest": "failed:3:xyz",
    "after_digest": "failed:3:xyz",
    "changed": false
  }]
}
```

测试改善：

```json
{
  "evaluation_changes": [{
    "evaluation_id": "focused_tests",
    "before_digest": "failed:3:xyz",
    "after_digest": "failed:1:abc",
    "changed": true
  }]
}
```

### 6.4 与 Effect 的关系

Effect settled 不自动构成进展。L3 必须同时提供 effect class、幂等身份、目标与
outcome digest。L4 的规则至少区分：

- read-only/test shell：最多形成 knowledge/verification observation；
- 状态改变型 Effect：只有新语义身份且属于 deliverable scope 时才可能是进展；
- 同 Effect 的幂等 replay：不是新进展；
- unknown Effect：不是进展，并触发安全处置；
- compensation：单独记录，不能冒充原交付进展。

## 7. ProgressEvaluator 与 ProgressAssessment

### 7.1 评估输出

```go
type ProgressAssessment struct {
    AssessmentID           string
    DeltaID                string
    ContractDigest         string
    Class                  ProgressClass
    AcceptedSignals        []AcceptedSignal
    RejectedSignals        []RejectedSignal
    ResetAnyProgressClock  bool
    ResetDeliverableClock  bool
    BudgetCharge           BudgetUsage
    ReasonCode             string
}
```

`ProgressClass`：

```text
deliverable_progress
verification_progress
knowledge_progress
coordination_progress
no_progress
regression
oscillation
unsafe_unknown
```

### 7.2 判定原则

1. 模型正文、reasoning、计划和自报完成不产生进展。
2. Tool 调用成功只说明 action settled，不说明目标推进。
3. 相同 `(kind, identity, digest)` 重复出现为 `no_progress`。
4. 文件同 hash 重写为 `no_progress`。
5. 文件版本 `A→B→A` 或在近期 digest 集中形成环为 `oscillation`。
6. code_change 的新读取只能算 knowledge progress，且不能无限刷新 deliverable
   clock；探索额度由 policy 限制。
7. verification 的相同失败集合不算进展；失败集合减少或目标 verdict 前进才算。
8. investigation 可以接受 novel evidence，但同内容换路径/换查询措辞必须按内容
   digest 去重。
9. blocker 只有从 active→cleared 的状态迁移才算 coordination progress。
10. unknown Effect 或无法证明 settlement 的 action 不得算进展。

### 7.3 防止“制造活动逃逸”

不能只维护一个 `lastProgressAt`。至少维护两条时钟：

- `LastAnyProgressAt`：任意契约认可的新知识或协调变化；
- `LastDeliverableProgressAt`：交付物、验证或结构化 Result 的推进。

code_change 节点即使不断读取新文件，也只能在有界 exploration quota 内延后提醒，
不能无限刷新 deliverable deadline。

## 8. ProgressCheckpoint

### 8.1 类型

```go
type ProgressCheckpoint struct {
    CheckpointID              string
    Version                   int64
    TaskID                    string
    GraphID                   string
    ActivationID              string
    AttemptID                 string
    ContractDigest            string
    LastDeltaSequence         int64

    LastAnyProgressAt         time.Time
    LastDeliverableProgressAt time.Time
    RecentFingerprints        []ProgressFingerprint
    NoProgressTurns           int
    NoProgressDuration        time.Duration
    NoProgressUsage           BudgetUsage
    CumulativeUsage           BudgetUsage

    InterventionStage         InterventionStage
    LastInterventionAt        time.Time
    InterventionCount         int

    RunDeadlineAt             time.Time
    GraphDeadlineAt           time.Time
    ActivationDeadlineAt      time.Time
    AttemptDeadlineAt         time.Time
    FinalizationReserve       time.Duration
    RecoveryReserve           time.Duration

    UpdatedAt                 time.Time
    Sealed                    bool
}
```

`RecentFingerprints` 必须有界，只保留检测重复与短周期振荡所需窗口。完整事实由
append-only Delta/原始 Store 保存，Checkpoint 不无限增长。

### 8.2 与 Task Memory 的边界

| 状态 | 对象 |
|---|---|
| 任务目标、约束、工作事实、文件摘要、blocker Context | L2 Task Memory |
| no-progress 计数、预算、deadline、干预阶段 | L4 ProgressCheckpoint |
| Tool/Effect/Artifact 原始事实 | L3 Store/Journal |
| Graph revision、Activation、路由 | L5 Graph Store |

Task Memory 可以在 Context 中渲染 ProgressCheckpoint 的有界摘要，但不能成为其
权威 Store，也不能通过 Context 压缩改变控制状态。

### 8.3 Retry/重启纪律

以下动作不得清零累计预算和干预阶段：

- RetryRollback；
- 同 Task 重新认领；
- Runner 重启；
- 进程重启；
- Context 压缩或 ContextSnapshot 变化；
- 相同契约下仅更换 InvocationID。

只有以下结构化事实允许重置或降低干预级别：

- ContractDigest 改变且已通过 Graph revision/commit；
- 新上游 Input revision 到达；
- 新用户决定到达；
- route/model/tools 通过新 Attempt/Activation 明确改变；
- 产生契约认可的 deliverable/verification progress。

相同 Graph 节点以未变化定义反复创建新 Activation 时，L5 还需携带逻辑工作
lineage 的累计 spend，避免用回边无限刷新预算。定义或输入真正变化时才创建新
ContractDigest 和预算窗口。

## 9. Durable 结算顺序

### 9.1 action 前预算预留

每次 Model Invocation 或 Tool action 前：

```text
读取 ProgressCheckpoint
  → 检查 Run/Graph/Activation/Attempt 剩余时间
  → 检查 no-progress/intervention 状态
  → 预留本次 action 最大时间/token/cost
  → durable commit reservation
  → 才允许 dispatch
```

预留失败或无法为 finalization/recovery 保留窗口时，不得发起 action。

### 9.2 action 后结算

```text
Harness action settled/unknown
  → 原始 Tool/Effect/Artifact 事实先落权威 Store
  → 写 TurnSettlementDelta
  → 运行纯函数 ProgressEvaluator
  → 写 ProgressAssessment
  → 原子更新 ProgressCheckpoint + settle reservation
  → 发 Domain Event/Trace projection
  → 进入下一 Loop 决策
```

如果进程死在原始 Effect settled 之后、Checkpoint 之前，恢复逻辑凭 Delta cursor
和 Effect/Tool ledger 重建缺失 Delta，不重放 action。

### 9.3 fail-closed

以下任一权威写入失败时停止下一昂贵 action：

- budget reservation；
- Delta append；
- Assessment append；
- ProgressCheckpoint CAS；
- terminal outcome commit。

Trace、UI 投影或非权威日志失败不改变业务判断，但必须暴露 degraded 状态。

## 10. Deadline 与 RunContract

### 10.1 RunContract

每个用户请求/评测运行建立：

```go
type RunContract struct {
    RunID               string
    DeadlineAt          time.Time
    FinalizationReserve time.Duration
    RecoveryReserve     time.Duration
    BudgetProfile       string
}
```

TUI/交互会话未指定 deadline 时由产品默认 profile 提供；SWE harness、API 客户端
或上层系统有明确时限时必须把绝对时间传入，不能只在外部 kill。

### 10.2 层级

```text
operation deadline
      < Attempt deadline
      < Activation deadline
      < Graph deadline
      < Run deadline
```

并满足：

```text
InterventionAt
  <= ActivationDeadlineAt - RecoveryReserve

GraphDeadlineAt
  <= RunDeadlineAt - FinalizationReserve
```

所有绝对 deadline 随 Checkpoint durable；重启恢复不能从“当前时间 + 原 duration”
重新计算，否则每次重启都会延长生命。

### 10.3 operation deadline

单次 Invocation/Tool timeout 必须取：

```text
min(
  operation policy 上限,
  Attempt 剩余预算,
  Activation 剩余预算 - RecoveryReserve,
  Run 剩余预算 - FinalizationReserve
)
```

当剩余预算不足时直接进入 intervention/finalization，不得再启动一个可能跨过
deadline 的 300 秒调用。

### 10.4 `TimeoutSeconds` 迁移

当前字段语义混杂，正式迁移为：

```text
ExpectedDuration     # SLO、UI、容量估计
InterventionAt       # 应进入收敛/上报的绝对时间
HardDeadlineAt       # L4 必须停止的绝对时间
FinalizationReserve  # 终态提交保留
RecoveryReserve      # steer/replan/change 保留
```

兼容期可读取旧 `TimeoutSeconds` 作为 `ExpectedDuration`，不得继续暗示它是硬
deadline。`config.example.yaml`、yaml guide、schema 和 doctor 必须同步迁移诊断。

## 11. 分级干预状态机

### 11.1 状态

```text
Running
  ├─ meaningful progress ───────────────┐
  └─ no progress                       │
       ▼                               │
    Reminder ── progress ──────────────┤
       │ still stalled                 │
       ▼                               │
    AttemptRollover ─ progress ────────┤
       │ still stalled                 │
       ▼                               │
    InterventionRequired ─ new input ──┘
       │ no viable change / budget exhausted
       ▼
    Blocked(no_progress_budget_exhausted)
```

### 11.2 Reminder

L4 将结构化、确定性的缺口摘要交给 L2 渲染：

```text
reason_code
重复的 signal/fingerprint
缺失的 deliverable/milestone
剩余 exploration/attempt budget
下一步允许的收口动作
```

Reminder 不扩大权限，不改变 Lease，不由 LLM 决定是否已解除。

### 11.3 AttemptRollover

当前 Attempt 以 `stalled` 原因 checkpoint 后结束。若 policy 允许恢复，可创建新
Attempt，并明确改变至少一项恢复输入：

- 更窄 Context；
- 新的结构化 steer；
- 新 model；
- 新 route；
- 新 Lease/tool 子集；
- 新上游输入。

ExecutionLease 已冻结时不能原地删工具；改变能力必须产生新 Attempt/Activation。
没有任何新信息或契约变化时，不得机械重开相同 Attempt。

### 11.4 InterventionRequired

L4 发布 durable、有类型的控制命令：

```go
type LoopInterventionRequested struct {
    CommandID          string
    GraphID            string
    ActivationID       string
    TaskID             string
    AttemptID          string
    ContractDigest     string
    ReasonCode         string
    MissingMilestones  []string
    RepeatedSignals    []ProgressFingerprint
    BudgetUsed         BudgetUsage
    BudgetRemaining    BudgetUsage
    CheckpointRef      string
}
```

L5/Scheduler 可以据此：

1. 提供有新信息的 steer；
2. 通过 GraphChangeProposal 修改未来节点定义；
3. 更换 route/model/capability 后创建新 Activation；
4. 拆分任务；
5. 诚实走 blocked/failed 路径。

不得要求 Scheduler 从自由文本 `[watchdog-alert: ...]`、原始日志或几十轮 history
自行重建进展状态。

### 11.5 Blocked

预算耗尽且无新的恢复输入时，L4 提交：

```text
status=blocked
reason_code=no_progress_budget_exhausted
blocked_reason=<结构化缺口摘要>
checkpoint_ref=<最后权威 checkpoint>
```

`cancelled` 仅表示用户、系统或上级控制面主动撤销；能力不足或停滞使用 blocked。

## 12. Watchdog 的新职责

Watchdog 不再判断语义进展。正常 Turn 已由 L4 在 settlement 边界机械评估。

Watchdog 只处理：

- L4 超过 heartbeat/checkpoint lease 未更新；
- Runner/Agent 失联；
- operation 超过已冻结 deadline 且不响应取消；
- Store 中任务超过 HardDeadlineAt 仍非终态；
- pending route starvation、无 route、依赖异常和孤儿 workspace。

Watchdog 发出的应是有类型 liveness fault，而不是要求 Scheduler读自然语言后猜测
是否有进展。SWE-010 的 wake 接线可作为兼容后备，最终由 typed intervention/
liveness command 取代普通 `__scheduler__` 文本任务。

## 13. 回放、恢复与未来回溯

### 13.1 Checkpoint + Delta

```text
Checkpoint N
  + Delta N+1
  + Delta N+2
  + Delta N+3
  = Checkpoint N+3
```

这一模型支持：

- 崩溃后从最后 settled Turn 恢复；
- 事故回放；
- 比较两个 Attempt 的决策差异；
- 使用新版 ProgressEvaluator 重放旧 Delta；
- 从旧 checkpoint 创建只读诊断分支；
- 未来在明确权限下创建新的执行分支。

### 13.2 回溯红线

回放只重建 AgentGo 的计算/控制状态，不撤销真实世界：

- settled Effect 复用 Result，不重跑；
- prepared 未 settled 仍为 unknown；
- 需要撤销时执行显式 compensation；
- 文件或外部状态的恢复必须形成新 Effect 和新 Delta；
- 诊断分支默认 read-only，不获得旧 Lease 的副作用权限。

Checkpoint 不是时间机器，也不能绕过 Effect Journal。

### 13.3 身份与 CAS

建议稳定身份链：

```text
RunID
  → GraphID
    → ActivationID
      → TaskID
        → AttemptID
          → TurnID
            → DeltaID / AssessmentID
```

Checkpoint 以 `(TaskID, Version)` CAS 更新；Delta sequence 单调递增。恢复时若
发现 cursor 缺口、重复 sequence、ContractDigest 不匹配或 checkpoint 指向不存在
的 Delta，必须 fail-closed 并请求恢复裁决。

## 14. 建议包边界与接口

包名不是最终强制项，但依赖方向应满足：

```text
internal/loopcontract
  ProgressContractDraft / CompiledProgressContract / policy types

internal/loopprogress
  TurnSettlementDelta / ProgressEvaluator / Assessment / Checkpoint state machine

internal/loopstore
  Delta append / Checkpoint CAS / reservation ledger

internal/agent
  Loop Controller adapter；Agent 只保留身份、认领与生命周期外壳

internal/bootstrap
  Harness fact projector、Graph intervention adapter、RunContract wiring

internal/graph
  保存冻结 ProgressContractRef/ExecutionPolicyRef，不 import agent 实现
```

建议端口：

```go
type SettlementProjector interface {
    Project(ctx context.Context, cursor SettlementCursor) (TurnSettlementDelta, error)
}

type ProgressEvaluator interface {
    Evaluate(contract CompiledProgressContract, checkpoint ProgressCheckpoint,
        delta TurnSettlementDelta) (ProgressAssessment, error)
}

type ProgressRepository interface {
    ReserveAction(ctx context.Context, req ActionReservation) (Reservation, error)
    AppendSettlement(ctx context.Context, delta TurnSettlementDelta,
        assessment ProgressAssessment, next ProgressCheckpoint) error
    LoadCheckpoint(ctx context.Context, taskID string) (ProgressCheckpoint, error)
}

type InterventionPublisher interface {
    PublishLoopIntervention(ctx context.Context, command LoopInterventionRequested) error
}
```

Evaluator 必须是纯函数：不读文件、不调模型、不执行工具、不改 Graph。

## 15. 与其他 SWE 问题的关系

### 15.1 SWE-014

SWE-014 的 typed Invocation failure 是本设计的前置输入：request timeout、caller
cancel、context-window exceeded 和 malformed response 必须对 no-progress、retry
和 deadline 产生不同 charge/决策。Invocation timeout 消耗时间预算，但不是一个
settled business progress Turn。

### 15.2 SWE-013

Progress checkpoint 不替代 Context item hard cap。SWE-013 仍需限制单项
reasoning/content/tool result；本设计只阻止这些异常 Context 驱动无界 Loop。

### 15.3 SWE-012

GraphDefinition 应保存节点的 `ProgressContractRef/ExecutionPolicyRef`；GraphDraft
commit validator 编译并验证，start 后随 Activation 冻结。运行中修改经
GraphChangeProposal，只影响未来 Activation。

### 15.4 SWE-003 / SWE-009

Model Invocation 仍需流式输出上限和 malformed/DSML 早夭。本设计确保即使模型
持续产生坏格式，失败也会进入有界恢复和 blocked，而不是多 Attempt 无界空转。

## 16. 迁移切片

### Slice 0：特征化与契约冻结

- 固定三道 SWE 事故的调用、写入、重试、deadline 和终态行为 fixture；
- 建立当前 Task timeout 只告警、不形成 context deadline 的测试；
- 固定 Task Memory 与 Progress state 的责任边界；
- 增加禁止重新引入固定 MaxLoops 的架构测试。

### Slice 1：RunContract 与绝对 deadline

- 定义 Run/Graph/Activation/Attempt deadline DTO；
- 引入 action reservation；
- operation timeout 从剩余预算派生；
- SWE harness 在输入时传入 RunContract；
- 保留外部 hard kill 作为失活保险。

### Slice 2：Progress Contract compiler

- 定义 WorkClass、signal 和 framework policy catalog；
- Scheduler/Graph schema 接入 ProgressContractDraft；
- commit/publish 阶段编译并校验；
- Activation 冻结 ContractDigest。

### Slice 3：TurnSettlementDelta

- 从 ToolCallRecord、Effect、Artifact、文件版本、evaluator 和 input revision 投影；
- 建立稳定 DeltaID、sequence、cursor 和有界 digest；
- 不改变当前 Loop 决策，仅做 shadow 记录和离线对照。

### Slice 4：ProgressEvaluator shadow mode

- 对历史/新运行输出 Assessment，但不干预；
- 用三道 SWE fixture 校准 duplicate、oscillation、knowledge 和 deliverable 分类；
- 验证复杂成功题不会被误判。

### Slice 5：ProgressCheckpoint 与 durable reservation

- 实现 Checkpoint CAS、预算预留和原子 settlement；
- retry/重启/重新认领恢复；
- 权威写失败 fail-closed；
- Trace 仅消费投影。

### Slice 6：分级干预

- Reminder；
- AttemptRollover；
- typed LoopInterventionRequested；
- blocked(no_progress_budget_exhausted)；
- Graph adapter/Result feed 接入。

### Slice 7：Watchdog 收敛与兼容退出

- Watchdog 使用 heartbeat/HardDeadlineAt；
- 旧 `TimeoutSeconds` 迁移为 ExpectedDuration；
- 修正文档/schema/config doctor；
- 旧文本 watchdog wake 降级为兼容路径并设退出条件。

### Slice 8：真实批测验收

- 真实二进制启动；
- 单题 no-progress 对照；
- 8 题 Flask SWE 回归；
- 断言 durable Delta/Checkpoint/Intervention/TaskOutcome/Graph outcome；
- 对比 token、wall time、误杀和 resolved rate。

## 17. 验证矩阵

### 17.1 ProgressEvaluator

| 场景 | 预期 |
|---|---|
| 相同文件、相同 digest 重读 100 次 | 首次最多 knowledge，后续 no_progress |
| 不同查询返回相同内容 digest | no_progress |
| code_change 连续读取不同文件 | 有界 knowledge，不无限刷新 deliverable clock |
| 文件同 hash 重写 | no_progress |
| 文件 A→B→A→B | oscillation |
| 同一测试、相同失败集合 | no_progress |
| 失败集合 3→1 | verification_progress |
| 新 Artifact 落在 deliverable scope | deliverable_progress |
| run_shell settled 但无状态/验证变化 | no_progress 或有界 knowledge |
| unknown Effect | unsafe_unknown，不算进展 |

### 17.2 Checkpoint/恢复

- RetryRollback 后 no-progress usage 不清零；
- 重新认领同 Task 保留 InterventionStage；
- 重启从绝对 deadline 恢复，不延长时间；
- Delta 已写、Checkpoint 未写时恢复重放投影，不重跑 Effect；
- Checkpoint CAS 冲突 fail-closed；
- ContractDigest 变化只有 Graph revision/new Activation 可接受；
- sealed checkpoint 后拒绝追加下一 action。

### 17.3 Deadline

- operation timeout 不超过 Attempt 剩余时间；
- 预算不足 finalization reserve 时拒绝新 Invocation；
- 1200 秒 RunContract 在 hard kill 前产生内部介入和 durable outcome；
- Scheduler/Graph 有足够 recovery reserve 消费 intervention；
- caller cancellation 优先于 no-progress blocked；
- Invocation timeout 与 context-window overflow 使用不同 charge/恢复策略。

### 17.4 Intervention

- Reminder 本身不扩大 Lease；
- 未变化契约不能机械 Attempt rollover 无限续命；
- typed intervention 幂等发布一次；
- 新输入到达后可以恢复且保留累计 spend；
- 无恢复输入时终态为 blocked，不是 cancelled；
- Graph 正确消费 blocked/failed 并留下 Activation lineage。

### 17.5 真实事故

- `automatic-options` 同类节点在 45 个 worker 调用之前介入；
- `session-access-tracking` 重复 git/show/test 不被视为交付进展；
- `teardown-callbacks` malformed/纯文本 retry 不重置累计停滞；
- 不再出现外部 timeout 时节点仍 processing 且无 durable terminal；
- 持续产生真实文件/验证进展的成功题不因固定轮数被截断。

## 18. Trace 与可观测性

新增投影事件建议：

```text
turn_settlement_delta_appended
progress_assessed
progress_checkpoint_committed
action_budget_reserved
action_budget_settled
no_progress_reminder
attempt_stalled
loop_intervention_requested
no_progress_budget_exhausted
loop_deadline_reached
```

每条事件至少关联：

```text
run_id
graph_id
activation_id
task_id
attempt_id
turn_id/delta_id
contract_digest
checkpoint_ref
reason_code
```

Trace 不保存完整敏感正文；fingerprint/digest 不替代权威 Store 的内容引用。查询
必须能区分 progress、no-progress、regression、oscillation、intervention 和
terminal decision。

## 19. 完成定义

只有以下全部满足，SWE-011 才能标记 fixed：

1. 每个新执行节点都有经框架编译的 `CompiledProgressContract`；
2. Scheduler 不能绕过 policy catalog 或声明无限预算；
3. L3 每个 settled Turn 产生可恢复的 `TurnSettlementDelta`；
4. L4 只依据 Delta + Contract 产生可重放 Assessment；
5. `ProgressCheckpoint` 在 retry、重启、重新认领后保持连续；
6. action 前预算预留、action 后 settlement/checkpoint 顺序 fail-closed；
7. deadline 层级与 RunContract 生效，外部 harness 不再是首个正常终止者；
8. 分级干预能在 no-progress budget 内进入 Reminder/rollover/intervention/blocked；
9. Watchdog 回归 liveness backstop，不再承担语义进展判断；
10. Effect recovery 与 checkpoint replay 不发生副作用静默重放；
11. 目标单测、恢复测试、race、全量测试、真实二进制启动和预期 durable 产物通过；
12. 新一轮 Flask SWE 证明不再有 40+ 调用、零写入、仍 processing 的同类事故，
    同时持续有真实进展的长任务不被误杀。
