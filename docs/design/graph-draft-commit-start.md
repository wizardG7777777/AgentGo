# Graph Draft / Commit / Start 事务化构图架构

> 状态：Accepted Design，Implementation Landed / Single-task Architecture Verified / Multi-task Validation Open<br>
> 日期：2026-08-22<br>
> 归属：L5 Graph Engineering<br>
> 对应问题：SWE-012<br>
> 上位规范：[`五层工程架构规范`](five-layer-engineering-architecture.md)<br>
> 统一路线图：[`SWE 架构修复统一实施路线图`](swe-architecture-repair-roadmap.md)

## 0.1 2026-08-22 实施状态

第六轮后的跨层修订：Graph `InputBinding` 不再渲染进 Task Description。发布桥把
每个冻结 Result/Evidence 转成持久化 `Task.ContextInputs`，Session snapshot/Store
clone 保持该字段；L2 分别映射为 `upstream_result` / `upstream_evidence`，按各自
Fragment/section policy reference 或拒绝。这样 L5 原先的 96K-rune 自由文本注入
不再绕过 L2 的 64KiB `user_task` 边界，Task Description 只保存 objective 与控制
契约。

已进入生产主链：GraphDraft/GraphDefinition/GraphExecution 三对象、append-only
AuthoringStore、framework compiler/最小合法图/GraphContract/独立 Proposal
Acceptance、commit/start 分离、幂等 StartIntent、Definition digest、原生
create/patch/read/validate/commit/start 工具、GraphChangeProposal、Definition adoption
恢复、typed EndOutcome/GraphStatus、TaskOutcome→TerminalFact、delivery outbox、
subgraph typed outcome 传播、TUI/Web/read_graph 投影，以及 activation 冻结的
`NodeOutputContract`（Prompt 渲染 + outcome fsync 前 required path/type/summary
机械校验）。新 authoring controller 的 Lease 不再暴露 legacy `patch_graph`。

简单请求新增 framework-owned authoring 主链：空参 create 后，Scheduler 只声明
execution_class，framework 生成 work Agent、独立 acceptance、单赋值端口、typed
ends、policy refs、OutputContract 与 GraphContract bindings；随后三个 current
transaction 工具以空参数从 task/session durable cursor 解析 proposal/revision/
report/digests 并完成 validate/commit/start。通用 CAS patch 继续服务复杂拓扑，
不删除动态 Graph 能力。

当前已通过 focused unit/contract/recovery、full、race、vet、build、真实二进制与
provider 单题架构门。最新单题在5次 Scheduler 调用内完成 Draft→Acceptance→
Commit→Start，产生 immutable revision、Activation 和 typed blocked outcome；blocked
来自 Worker 零写入，不是 authoring/terminal 事故。三平台与8题证据仍开放。

两阶段 `TerminalIntent` 已落地：锁内 durable fence、锁外 action settlement 与
checkpoint Seal、TaskOutcome/outbox fsync、Task CAS，以及启动期 pending intent
恢复。新 ProgressContract Task 的 Graph terminal feed 只消费该正式结果；历史
`current_unsealed` 记录仍按原事实读取，不升级伪装为 sealed。

仍开放：legacy `submit_graph`/direct `patch_graph` 兼容入口的最终删除，以及
真实二进制和 Flask SWE 多 rollout。SWE-012 在外部证据完成前仍为
validation-open。

## 1. 决策摘要

AgentGo 的新图构造从“提交 JSON 即激活”改为事务化三对象模型：

```text
GraphDraft
    │ commit（框架验证、无执行副作用）
    ▼
GraphDefinition
    │ start（Scheduler 显式决定）
    ▼
GraphExecution
```

本设计固定以下架构决策：

1. Scheduler 负责提出、编辑 GraphDraft，并决定是否启动已提交的图。
2. 框架独占 commit 前验证；Scheduler 不能自我批准或绕过验证。
3. `commit` 与 `start` 分离；commit 不创建 Activation、Task 或 Effect。
4. 只有 `start_graph` 成功后，origin Scheduler Task 才进入 finalizing。
5. 新 Graph 必须满足统一的最小合法图规则，root 不得为 end。
6. Graph success path 必须包含实际产出 Result 的执行节点，不允许零步成功。
7. End 节点必须声明结构化 outcome，Graph 生命周期终态由 outcome 推导。
8. GraphDraft 必须绑定 GraphContract，框架机械检查交付物、Effect、验证与验收覆盖。
9. 自然语言请求与 GraphContract 的语义一致性由独立 Proposal Acceptance 核验，
   不能由原 Scheduler 自评。
10. 运行中修改统一使用 GraphChangeProposal；在途 Activation 继续使用冻结定义。
11. Graph 工具改用原生结构参数和小型 patch，不再把完整 Graph JSON 嵌入字符串。
12. 不采用 root=end 拦截之类的独立止血补丁；所有修复随完整事务架构落地。

## 2. 问题定义

当前 `submit_graph` 同时执行五件事：

1. 接收完整 Graph JSON 字符串；
2. 解析并验证；
3. durable 写入 GraphStore；
4. 立即激活 root；
5. 标记 origin Scheduler finalizing。

JSON 语法失败时，错误回执却建议 Scheduler“先提交 root+end 骨架，再通过
`patch_graph` 逐步扩展”。该建议与真实生命周期矛盾：提交成功已经激活 root
并结束 origin Scheduler；root=end 又会立即终结 Graph，因此不存在后续构图窗口。

第五轮 `secret-key-rotation` 实际提交了仅含 root=end 的 v2 Graph。Validator
认为其 schema/拓扑合法，Runtime 在约 25ms 内 durable 完成 submit、root
Activation、空 Result 与 Graph completed；没有发布任何执行 Task，也没有产生
文件、Effect、Artifact 或验收。这证明当前缺少的不是单条校验，而是独立的
Graph authoring transaction。

## 3. 目标与非目标

### 3.1 目标

1. 允许 Scheduler 通过多次小 patch 构造大图，构造期绝不启动半成品。
2. 让框架以结构化、确定性的方式判断 Graph 是否达到可提交条件。
3. 让 Scheduler 在 commit 后检查标准化 Definition，再显式决定 start。
4. 保证 start、恢复和重复调用幂等，不重复 root Activation 或副作用。
5. 保持 Graph 动态更新能力，但所有更新都经 Proposal、Validation、Commit。
6. 将“结构合法”“请求覆盖”“允许运行”“运行成功”拆成不同事实。

### 3.2 非目标

1. 不让 Scheduler 直接写 Graph runtime 状态。
2. 不让 Prompt 或节点标题代替 commit validation。
3. 不重新打开已激活节点的冻结定义。
4. 不恢复 Plan Runtime 或建立 GraphDraft→IR 的第二套编译语义。
5. 不把 Trace 当作 Draft、Definition 或 Execution 的权威状态。
6. 不要求一次改动重写现有 Graph Runtime 内核；迁移按切片完成。

## 4. 三个核心对象

### 4.1 GraphDraft

GraphDraft 是 Scheduler 可编辑、不可执行的构图工作区。

建议字段：

```text
proposal_id
graph_id
session_id
owner_task_id
draft_revision
status: editing | validating | rejected | committed | abandoned
request_ref / request_digest
contract: GraphContract
document: GraphDocument candidate
last_validation_report_ref
created_at / updated_at / expires_at
```

不变量：

- Draft 不在 Graph Runtime 活跃索引中；
- Draft 不创建 Activation、Task、Interaction、Effect 或 Graph Team；
- Draft patch 使用 `base_draft_revision` CAS；
- Draft 可 durable，进程恢复后仍为 parked authoring state；
- Draft 被 commit 后不可继续原地编辑；修改需 fork 新 Draft；
- Draft abandon 幂等，并保留有界审计记录。

### 4.2 GraphDefinition

GraphDefinition 是通过 commit validation 后的不可变执行定义。

建议字段：

```text
graph_id
schema
revision
definition_digest
source_proposal_id
session_id
owner_task_id
contract_digest
validation_report_ref
status: pending | abandoned
root
nodes
committed_at
```

不变量：

- Definition 已提交但尚未运行；
- commit 后 node/task/capability/next 不原地变化；
- amendment 生成新 Draft 和新 Definition revision；
- `pending` Definition 不自动启动；
- Definition 可被读取、比较和放弃；
- Definition 不含 Activation、TaskID、NodeStatus 等运行事实。

### 4.3 GraphExecution

GraphExecution 是某一已提交 Definition 的运行实例。

建议字段：

```text
graph_id
definition_revision / definition_digest
state_version
status: starting | running | completed | failed | blocked | cancelled
root_activation_id
activations
transitions
results / evidence
started_at / ended_at
```

不变量：

- 只由 `start_graph` 创建；
- Root Activation 只创建一次；
- 每个 Activation 冻结创建时的 Definition revision；
- Definition 新 revision 只影响未来 Activation；
- GraphExecution 不反向修改 GraphDefinition；
- terminal outcome 来自 typed end，而不是 end 标题或 Scheduler 文本。

## 5. 生命周期状态机

### 5.1 Draft 生命周期

```text
editing
  ├─ patch(valid CAS) ───────────────→ editing(draft_revision+1)
  ├─ validate ───────────────────────→ editing + ValidationReport
  ├─ commit requested ───────────────→ validating
  │    ├─ validation rejected ───────→ rejected/editing
  │    └─ durable commit succeeded ──→ committed
  └─ abandon ────────────────────────→ abandoned
```

Validation rejected 时 Draft 内容不丢失，Scheduler 按结构化错误继续 patch。

### 5.2 Definition / Execution 生命周期

```text
Definition pending
  ├─ start_graph ──→ Execution starting
  │                    ├─ root durable ──→ running
  │                    └─ start failed ──→ pending / failed_start
  ├─ amend ────────→ new Draft
  └─ abandon ──────→ abandoned

Execution running
  ├─ success end ──→ completed
  ├─ failed end ───→ failed
  ├─ blocked end ──→ blocked
  ├─ cancellation ─→ cancelled
  └─ ChangeProposal commit ─→ new Definition revision（Execution 继续）
```

## 6. Scheduler 与框架的所有权

| 责任 | Scheduler | 框架 |
|---|:---:|:---:|
| 提出 GraphContract | 写候选 | 校验覆盖与一致性 |
| 创建/patch Draft | 是 | CAS、持久化、字段校验 |
| 判定 schema/拓扑合法 | 否 | 是 |
| 判定 route/capability 可执行 | 否 | 是 |
| 判定是否 commit | 发起请求 | 验证后接受/拒绝 |
| 判定是否 start | 是 | 检查前提并执行事务 |
| 创建 Activation/Task | 否 | 是 |
| 写 runtime status/Result/Evidence | 否 | 是 |
| 提出运行中变更 | 是 | 验证并 commit 新 revision |
| 自我批准语义覆盖 | 否 | 独立 Proposal Acceptance |

Scheduler 选择“不启动”时必须显式执行一种动作：继续 amend、请求用户、
abandon Definition，或把 origin 请求收口为 blocked。pending Definition 不允许
无限遗留，受 Session/TTL/owner Task 生命周期治理。

## 7. Graph authoring 工具面

### 7.1 Draft 工具

```text
create_graph_draft
read_graph_draft
patch_graph_draft
validate_graph_draft
commit_graph_draft
abandon_graph_draft
```

### 7.2 Runtime 工具

```text
read_graph
start_graph
propose_graph_change
read_graph_change
commit_graph_change
cancel_graph
```

### 7.3 参数原则

- 新工具使用原生 object/array 参数，不使用 `graph: "{...}"` 双重编码；
- 大图通过 `upsert_nodes/remove_nodes/root/contract` 等小 patch 构造；
- 每个 mutation 带 base revision，冲突返回当前 revision；
- 错误返回稳定 code、path、retryable、message 和安全建议；
- Suggestion 只给候选操作，不自动 patch/commit/start；
- `submit_graph` 对新图逐步退役，legacy 图保留只读兼容入口。

示例 patch：

```json
{
  "proposal_id": "gp-123",
  "base_draft_revision": 3,
  "upsert_nodes": [
    {
      "id": "implement",
      "kind": "agent",
      "task": {
        "title": "实施修复",
        "description": "..."
      },
      "next": []
    }
  ]
}
```

## 8. 最小合法图架构

最小合法图分为通用硬规则与 GraphContract 条件规则。只有两类均满足才能 commit。

### 8.1 通用硬规则

1. Graph 至少包含一个非 end 节点；
2. Graph 至少包含一个 end；
3. root 存在且 `root.kind != end`；
4. root 无需未提供的上游输入即可合法激活；
5. 所有节点从 root 可达；
6. 所有节点在结构上均能到达某个 end；
7. 每个强连通循环区域至少存在一条通往 end 的结构出口；
8. 不存在 root→success end 的零步成功路径；
9. 每个 Task-producing 节点具有非空 title/description 和 typed OutputContract；
10. 每个 Task-producing 节点覆盖 completed、failed、blocked 的合法出口；
11. acceptance 覆盖 pass/fixable/failed 与 runtime failed/blocked；
12. 每个 success end 必须消费至少一个上游 ResultRef；
13. Graph 至少有一个真正产生 Result 的 activation-capable 节点；
14. 单赋值、端口、join、acceptance 和 outlet 规则继续满足 Graph v2；
15. 未知节点、条件、extension 或 capability fail-closed。

### 8.2 Root 类型

默认允许：

- `controller`
- `agent`
- 参数完整的确定性 `tool`
- `approval`
- `wait_event`
- `subgraph`

条件允许：

- `router`：GraphContract 提供完整 initial input，且条件只引用这些输入。

禁止：

- `end`
- `join`
- `acceptance`

### 8.3 最小图形态

最小 answer/read/work 图：

```text
work
  ├─ completed → end_success(outcome=success)
  ├─ failed    → end_failed(outcome=failed)
  └─ blocked   → end_blocked(outcome=blocked)
```

即使是简单回答，也由 controller/agent 生成 Result 后进入 end；end 本身不生成答案。

### 8.4 EndOutcome

所有新 end 必须声明：

```text
success | failed | blocked | cancelled
```

Runtime 推导：

| EndOutcome | GraphExecution status |
|---|---|
| success | completed |
| failed | failed |
| blocked | blocked |
| cancelled | cancelled |

禁止根据 end ID/title 中的 `success`、`failed` 等单词推断 outcome。

## 9. GraphContract

结构合法不等于覆盖用户请求。GraphDraft 必须绑定 GraphContract：

```text
request_ref / request_digest
execution_class: answer | read_only | mutating | interactive | waiting
deliverables[]
constraints[]
required_effects[]
required_artifacts[]
required_checks[]
requires_acceptance
success_evidence[]
```

示例：

```json
{
  "request_ref": "user-request-123",
  "request_digest": "...",
  "execution_class": "mutating",
  "deliverables": [
    {
      "id": "source_change",
      "kind": "artifact",
      "description": "修改 src/flask 下实现"
    }
  ],
  "required_effects": ["file_write"],
  "required_checks": ["full_test_suite"],
  "requires_acceptance": true
}
```

框架机械检查：

- mutating contract 存在满足写能力的节点；
- success path 产生要求的 Effect/Artifact；
- required check 映射到 checker/evidence；
- requires_acceptance 时所有 success path 经过 acceptance；
- deliverable 映射到 OutputContract；
- success end 消费 required Result/Evidence；
- readonly/answer contract 不携带越权写能力。

## 10. 独立 Proposal Acceptance

自然语言请求无法完全由确定性代码判断，因此验证分两层：

### 10.1 Deterministic Validation

由框架执行：schema、拓扑、数据流、状态、能力、route、GraphContract 映射、
outlet、end outcome 和持久化前提。它不调用 Scheduler，也不相信自由文本自述。

### 10.2 Independent Planning Acceptance

独立 verifier 对比：

```text
原始用户请求
GraphContract
规范化 GraphDefinition candidate
系统策略
```

检查 Scheduler 是否遗漏交付物、否定约束、验收、失败路径或权限边界。verifier
不是原 Scheduler，不拥有 commit 权限，只产生：

```text
pass | fixable | blocked | failed
```

commit gate 机械消费 verdict。证据/能力不足必须 blocked；需要强保证的任务可
升级为用户审批。后续如果用户输入已经结构化，可由确定性 Contract Validator
取代对应语义检查。

## 11. Commit validation 管线

```text
Draft Snapshot
  → input/size/depth/duplicate-key
  → strict schema decode
  → canonical normalization
  → root/reference/reachability
  → SCC/termination reachability
  → single-assignment/dataflow/ports
  → task/output/outlet/end outcome
  → capability/route/policy
  → GraphContract coverage
  → independent Proposal Acceptance
  → persistence preflight
  → atomic commit
```

ValidationReport：

```json
{
  "accepted": false,
  "proposal_id": "gp-123",
  "draft_revision": 4,
  "normalized_digest": null,
  "errors": [
    {
      "code": "ROOT_CANNOT_BE_END",
      "path": "$.root",
      "retryable": true,
      "message": "新图 root 必须是可执行节点"
    }
  ],
  "warnings": []
}
```

验证失败不修改 Draft，不生成 GraphDefinition，也不产生执行副作用。

## 12. Commit 事务

`commit_graph_draft(proposal_id, expected_draft_revision)`：

1. 读取指定 Draft snapshot；
2. 锁定或 CAS draft revision；
3. clone、normalize、validate；
4. 获取 Proposal Acceptance；
5. 生成 canonical GraphDefinition；
6. 计算 definition/contract digest；
7. append+fsync commit record；
8. 原子插入 DefinitionStore，状态 pending；
9. 标记 Draft committed；
10. 返回 revision、digest 和 ValidationReportRef。

Commit 不激活 root、不发布 Task、不创建 Effect、不发 graph_ended、不 finalizing
origin Scheduler。

## 13. Start 事务

`start_graph(graph_id, expected_revision, expected_digest)`：

1. 校验 GraphDefinition pending；
2. 校验 owner Task、Session 和调用权限；
3. 校验 revision/digest 未变化；
4. 重新检查 route、环境和外部能力可用性；
5. 预留唯一 root ActivationID；
6. durable 写 starting intent；
7. durable 创建 root Activation；
8. Task-producing root 幂等发布 Task，写回稳定 TaskID；
9. durable GraphExecution running；
10. 返回 root ActivationID。

只有步骤 9 成功后才标记 origin Scheduler finalizing。Start 重复调用必须返回
同一 root Activation，不得创建第二个执行实例。Start 失败时 origin Scheduler
继续运行，可 amend、重试、请求用户或 abandon。

## 14. 运行中动态图：GraphChangeProposal

运行中的 Definition 修改不再由 `patch_graph` 直接写入，而是：

```text
create GraphChangeProposal(base_revision, patch, reason)
  → clone current Definition
  → apply patch
  → deterministic validation
  → active-reference/ownership validation
  → optional independent acceptance
  → atomic commit new revision
```

不变量：

- 在途 Activation 保持旧冻结定义；
- 不能删除被 Activation/Transition/Result 引用的节点；
- 不重新解释旧 Result/Evidence；
- 不重复激活 root；
- 新 revision 只供未来 Activation 使用；
- revision 冲突必须重读后重新提案；
- ChangeProposal commit 不改变当前 GraphExecution 的 lifecycle identity。

## 15. 持久化与恢复

### 15.1 Draft 恢复

- editing/rejected Draft 恢复为 parked；
- 不自动 commit/start；
- owner Session 不活跃时保持归档；
- TTL 到期可 abandon，但保留审计摘要。

### 15.2 Definition 恢复

- pending Definition 恢复后仍 pending；
- 不因进程启动自动 start；
- `--resume`/Session 规则继续约束 owner Task；
- amend/abandon 使用 revision CAS。

### 15.3 Start 崩溃窗口

- starting intent、root ActivationID 和 Task 发布使用稳定幂等身份；
- 恢复时对账 GraphStore、TaskStore、Effect Journal；
- Task 已发布但 task_id 未回填时只补账，不重复发布；
- Effect unknown 继续 fail-closed，不以重新 start 绕过。

### 15.4 运行中恢复

沿用当前 Activation/Transition/Result journal 和冻结定义恢复语义；新 authoring
层不得降低现有 durable Graph Runtime 保证。

## 16. Trace 与领域事件

新增稳定事实：

```text
graph_draft_created
graph_draft_patched
graph_draft_validation_requested/completed
graph_draft_commit_rejected/committed
graph_definition_abandoned
graph_start_requested/started/failed
graph_change_proposed/rejected/committed
graph_outcome_committed
```

DraftStore、DefinitionStore、Graph journal 是权威；Trace 只保存 ID、revision、
digest、错误 code、authority ref 和脱敏摘要。Reactor 应消费 commit 后的领域事件，
不能通过 Trace 反向构造 Graph 状态。

## 17. 包边界建议

```text
internal/graph/authoring
  draft.go
  draft_store.go
  draft_patch.go
  contract.go

internal/graph/compiler
  normalize.go
  structural_validate.go
  dataflow_validate.go
  contract_validate.go
  validation_report.go

internal/graph/definition
  definition.go
  definition_store.go

internal/graph/runtime
  execution.go
  activation.go
  transition.go
  result.go

internal/graph/change
  proposal.go
  validate.go
  commit.go
```

实际迁移可先在现有 `internal/graph` 内以文件/私有类型落地，再决定拆子包；
`internal/bootstrap` 只装配端口，不承载 authoring/validation 业务规则。

## 18. 迁移策略

### Slice 0：特征化

- 固定当前 submit→activate→finalize 行为测试；
- 新增 SWE-012 end-only 事故 fixture；
- 固定现有 Graph v1/v2 recovery 与 patch-future 行为。

### Slice 1：Draft Domain

- 新增 GraphDraft/GraphContract/ValidationReport/DraftStore；
- 只提供内部 API 和单测，不接生产 Scheduler。

### Slice 2：Compiler/Validator

- 迁移当前 ParseAndValidate；
- 加最小合法图、EndOutcome、GraphContract coverage；
- 加独立 Proposal Acceptance 端口与 fake。

### Slice 3：Commit/Start 分离

- 将当前 `Runtime.SubmitGraph` 拆为 commit definition 和 start execution；
- 保留 adapter 供旧测试，生产新路径不再调用一体化 submit；
- finalization 移到 start success。

### Slice 4：Authoring Tools

- 上线 Draft 工具和原生结构 patch；
- Scheduler Prompt 改为 create→patch→commit→read→start；
- 删除“root+end 骨架后 patch”建议。

### Slice 5：ChangeProposal

- 运行中 patch 迁移到 proposal/validation/commit；
- 保持只影响未来 Activation；
- 退役新图的直接 `patch_graph` 写入口。

### Slice 6：Outcome / Recovery / UI

- EndOutcome 推导 GraphStatus；
- UI 展示 Draft/Definition/Execution 和 success/failed/blocked；
- 补全 restart、Session、Effect unknown 对账。

### Slice 7：E2E 与 legacy 退出

- 跑全仓测试、race、Windows 编译、二进制真实启动；
- 重跑 8 题 Flask SWE 多 rollout；
- v1 只读兼容，新 v2 authoring 只走事务路径；
- 确认无生产调用方后删除一体化 `submit_graph`。

## 19. 验证矩阵

### 19.1 Draft/Commit

- 多次小 patch 构造大图；
- revision conflict 不丢 Draft；
- validation failure 无 Graph/Task/Effect；
- commit crash 重放恰好一次；
- commit 后 Definition immutable。

### 19.2 最小合法图

- root=end 拒绝；
- 无非 end 节点拒绝；
- 无 typed end 拒绝；
- 零步 success 拒绝；
- 无法到达 end 的 SCC 拒绝；
- success end 无 ResultRef 拒绝；
- mutating contract 无 write/effect path 拒绝；
- required acceptance 被旁路时拒绝。

### 19.3 Start

- commit 不发布 Task；
- start 后才创建 root Activation；
- start success 后 origin finalizing；
- start failure 时 origin 保持可操作；
- 重复 start 不重复 root/Task；
- Task publish 崩溃窗口恢复不重复 Effect。

### 19.4 Dynamic Change

- old Activation 使用旧定义；
- future Activation 使用新 revision；
- 删除被引用节点拒绝；
- revision conflict fail-closed；
- recovery 不重复 transition/root。

### 19.5 真实行为

- 长 Graph 通过多 patch 构造，不生成 JSON-in-JSON 长字符串；
- `secret-key-rotation` 同类题不能以 end-only Graph 完成；
- Scheduler 能在 ValidationReport 后 amend 或选择 start；
- graph completed 必须对应 success outcome；
- failed/blocked end 不再显示 completed。

## 20. 完成定义

SWE-012 只有在以下条件全部满足时才可标记 fixed：

1. 新图生产路径不存在“提交即激活”的一体化入口；
2. GraphDraft 可分批构造且 commit 前零执行副作用；
3. commit 由框架完整验证，Scheduler 不能绕过；
4. Scheduler 显式 start，且 start success 后才 finalizing；
5. 最小合法图与 GraphContract coverage 均机械执行；
6. root=end/zero-work/zero-result success 图被拒绝；
7. EndOutcome 正确推导 completed/failed/blocked/cancelled；
8. 运行中修改只影响未来 Activation；
9. crash/Session/Effect unknown 路径均通过恢复测试；
10. 二进制真实启动和外部 Flask SWE 多 rollout 未再出现同型事故。

## 21. 与其它文档的关系

- [`五层工程架构规范`](five-layer-engineering-architecture.md)：定义 L5 与其它层的
  总体边界；本文细化 L5 authoring transaction。
- [`Graph 终态契约 v2`](graph-terminal-contract-v2.md)：定义节点结构化终态和
  outlet 规则；本文不削弱其约束，并补充 Definition/Execution 生命周期。
- [`第五轮 SWE 分层诊断`](../test-issues/2026-08-21-2329-swe-round5-layered-diagnosis.md)：
  SWE-012 事故证据与问题状态。
- [`docs/nextUpgrade-V6.md`](../nextUpgrade-V6.md)：历史 Graph Runtime 设计母本；
  本文取代其中“submit_graph 立即执行”作为新图 authoring 的长期方向。

## 22. 已接受、已实现与待退出

已接受：

- 三对象模型；
- commit/start 分离；
- Scheduler 决定 start；
- 框架独占 commit validation；
- 最小合法图与 GraphContract；
- 独立 Proposal Acceptance；
- typed EndOutcome；
- 运行中 ChangeProposal；
- 事务、恢复和验证矩阵。

已实现：上述三对象 authoring transaction、生产工具/cutover、ChangeProposal、
typed outcome/TaskOutcome/outbox、恢复与 UI/Prompt 投影均已进入仓库并通过 focused
验证。

待退出/待验收：legacy submit/patch、三平台 CI 与 Flask SWE 8题 rollout。两阶段
checkpoint seal、full/race/vet/build、真实二进制和单题架构门已经完成；实现落地
仍不等于多题 closure，SWE-012 保持 `implementation-landed / validation-open`。
