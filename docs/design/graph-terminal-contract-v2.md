# Graph 终态契约 v2：封闭终态 + 数据流路由

> 2026-08-20 定稿。问题来源：`docs/test-issues/2026-08-20-1508-swe-round3-open-issues.md`
> 的 SWE-004（图设计死胡同与终态语义）；本文是用户三项决策的权威设计记录，
> 实施完成后在 test-issues 中以新文档闭环。

## 1. 背景与问题

第三轮 Flask SWE 评测暴露：agent/controller 节点经 `submit_task_result` 提交的
`event` 参数是**任意字符串**——worker 交 `ready` 而契约要求 `completed`，
无匹配出路即全图 fail-closed（ipv6-session-txn）。同族问题：失败路径无返工
回边、「到达任意 end = completed」成败不可分。

现状分裂：边条件事件名是封闭枚举，acceptance verdict 是封闭三值，runtime
自产事件封闭，**唯独 agent/controller 的业务终态事件自由填写**。acceptance
节点已证明高级形态可行：不报事件、只交数据（verdict 字段）、边用
`{$.verdict eq ...}` 数据条件路由。v2 把该形态推广到全部图节点。

## 2. 设计原则（用户定调）

- **D1 无匹配出路两击协议**：第一次告知节点 Agent 错误、由其重新撰写提交
  （自愈）；再次出现错误判定不可自愈，Scheduler 介入判决。
- **D2 event 收窄为少量可控字段，graph schema v2 直接迁移**（非渐进共存）。
- **D3 legacy `publish_task` 路径 strict 渐进**：图外 legacy 语义暂不动。
- **路由判定保持机械**：边条件对数据求值（eq/ne/in/exists），零成本、确定；
  LLM 不判路由，Scheduler 只判疑难（无匹配/契约违例升级）。
- **自由填写不得搬家**：只说「节点交数据、边判数据」会把自由填写从 event
  名字搬到字段名字——必须配输出契约的机械派生与钉入（§5），否则本设计
  不成立。

## 3. 终态契约 v2 核心

1. **节点终态 status 封闭三值**：`submit_task_result.status ∈
   {completed, failed, blocked}`（blocked 语义不变：唤醒 Scheduler 重规划）。
2. **图任务 event 参数废弃**：属于 v2 图的任务提交不得携带 `event`；
   acceptance 的 `verdict` 通道不变（本就是数据字段）。v2 图任务的提交
   带 event → 提交期拒绝（不计入两击，属参数级错误）。
3. **业务路由全部走 result 数据字段 + path 条件**；result 的系统保留键
   收窄为 `status/verdict/cited_evidence`（删除 `event`）。
4. **系统事件词表**（runtime 自产，封闭）：`completed / failed / blocked /
   timeout / always`——节点 status 的镜像加 runtime 事件，用于不需要
   业务语义的边（如失败兜底边）。

## 4. schema v2 的边条件规则

| 节点 kind | v2 出边允许 |
|---|---|
| agent / controller | path 条件（`$.field eq/ne/in/exists`）；系统事件 `completed/failed/blocked/always`。**禁止** `ready/pass/fixable/approved/rejected` 等业务事件名——建图校验拒绝 |
| acceptance | 不变：`$.verdict` 精确分支 + runtime `failed/blocked` 兜底 |
| router / join / tool / wait_event / approval | 不变（系统自产封闭事件） |

建图校验同时要求：agent/controller 出边引用的 path 字段，必须在该节点
`task.description` 的输出契约段显式声明（与 acceptance 判据同款显式纪律，
拒绝「边引用了生产者没承诺的字段」）。

## 5. 输出契约机械派生与钉入

- 建图校验通过时，从每个 agent/controller 节点的出边 path 条件**机械反推**
  必需字段与值域（`$.coverage eq "gap"` + `in ["ok","gap"]` →
  `coverage ∈ {gap, ok}`）；
- 节点任务发布时，该契约注入任务描述「输出契约」段，并钉入
  `TaskMemory.Constraints`（复用 SWE-001 契约钉住通道）：「你的 result
  必须包含字段 coverage ∈ {gap, ok}；不得提交 event」；
- 提交期（终态落盘前）校验：缺必需字段 / 字段值无任何出边匹配 → 进入
  两击协议（§6）。

## 6. 两击升级协议（D1 机制化）

判定时机统一为 **submit_task_result 终态落盘前**（runtime 用提交数据预
求值全部出边）：

1. **第一击（自愈）**：无匹配 → 拒绝提交（**不进入 finalizing**，同一任务
   可再次提交），工具回执携带精确错误：缺哪个字段、当前值、合法值域、
   以及「result 示例形态」。Agent 在 ReAct 循环内修正重交。
2. **第二击（升级）**：同一 activation 第 2 次仍无匹配 → 提交被拒，
   节点标 `failed`（原因 `contract_no_outlet`，含两次提交摘要），立即
   发布 graph-change-request 唤醒任务（幂等标记
   `[graph-change-request: <graph_id>/<activation_id>/no-outlet]`）请
   Scheduler 裁决：patch_graph 补边/改道/宣布图失败。
3. 违例计数按 activation **持久化**（崩溃恢复不丢）；activation 以
   `activation:"new"` 重进时归零。
4. 参数级错误（v2 图任务携带 event、status 越界、verdict 用于非
   acceptance）不属于两击：首次即拒绝并附修正说明，不计数。

Scheduler 侧纪律（提示词）：收到 no-outlet 唤醒后必须先 `read_graph` 再
裁决；禁止机械地把当前值塞进新边了事——要判断是生产者漏字段（返工）、
边写错值（修边）还是需求变化（改道）。

## 7. 版本与迁移（D2 + D3）

- `schema: "agentgo.graph/v2"` 为新提交唯一接受值；`SchemaV1` 常量为
  `"agentgo.graph/v1"`。**v1 存量图（快照/恢复）按 v1 语义继续跑完**——
  runtime 的边求值按图的 schema 版本分流；v1 图不迁移、不混用。
- `patch_graph` 不得跨版本（v1 图只能按 v1 规则 patch）。
- legacy `publish_task`（图外）保留 event 与旧词表，strict 渐进：行为
  不变，标记为过渡路径，随 scheduler 提示词重构逐步退场。
- Scheduler 提示词：只产出 v2 图；契约钉入与工具描述同步更新（
  `submit_task_result` 的 event 参数对图任务标记废弃）。

## 8. 影响面与实施切片

| 切片 | 内容 | 主要位置 |
|---|---|---|
| 1 | schema v2 常量和解析、边条件规则校验（§4）、输出契约声明校验 | `internal/graph/validate.go` |
| 2 | 提交期出路匹配检查 + 两击协议 + 违例计数持久化 + no-outlet 唤醒 | `internal/graph/runtime*.go`、`internal/tools/`（submit_task_result）、`internal/bootstrap/graph_runtime.go` |
| 3 | 输出契约派生 → 任务描述注入 + TaskMemory.Constraints 钉入 | `internal/graph/`（发布路径）、`internal/agent/task_memory.go` |
| 4 | legacy strict 渐进标注、scheduler 提示词 v2 化、工具描述更新 | `internal/scheduler/scheduler.go`、`internal/tools/plan_control.go` |
| 5 | 测试（专职 subagent）：v2 校验矩阵、两击协议、契约钉入、v1 兼容、恢复 | 各包 `_test.go` |

明确不在本期：end 节点 outcome 落账（SWE-004 子问题 3，另列）；失败路径
返工回边 doctrine（子问题 1，属 scheduler 提示词重构专场）；v1 存量图的
迁移工具（v1 跑完即自然消亡）。

## 9. 与既有机制的关系

- SWE-001 契约钉住：本设计的 §5 是其第二期（同一 TaskMemory.Constraints
  通道）；
- SWE-002 回填容错：两击协议第二击的「节点 failed + 唤醒」复用
  graph-terminal-feed 的终态回填链路，回填失败处置（重试/显式 fail）与
  SWE-002 修复共享同一处改动；
- SWE-003 工具名清洗：无交集，独立进行。
