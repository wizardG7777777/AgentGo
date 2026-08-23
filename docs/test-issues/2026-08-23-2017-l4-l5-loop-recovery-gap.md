# SWE 第八轮 L4→L5 Loop Recovery 不可达事故

> 日期：2026-08-23 20:17 AWST<br>
> 问题：SWE-033<br>
> 归层：L4 Loop → L5 Graph，含 L3 ExecutionLease 授权投影<br>
> 状态：fixed(2026-08-23) / external-validation-complete

## 1. 事故证据

DeepSeek `reasoning_effort=low` 的同批 8 题结果为
`task_resolved=5/8 / architecture_ok=8/8`。三道未解题
`context-push-order`、`pass-context-dispatch`、`session-access-tracking` 均呈现：

- Worker 运行 18 个 no-progress Turn，经一次 Attempt rollover；
- `read/grep/run_shell` 事实完整，`edit_file/write_file/submit_task_result=0`；
- L4 durable 写入 `LoopInterventionRequested`，Task 以
  `blocked[loop_intervention_required]` 收口；
- framework-owned simple Graph 把 `work blocked` 直接路由到 blocked end；
- Graph terminal durable 后才物化 detached Scheduler wake，导致 recovery 面对
  已终态 Graph，`commit_graph_change` 结构性不可达；
- detached wake 暴露 `get_task_result`，但旧授权把它视作 legacy Scheduler，读取
  精确来源 Graph Task 时拒绝。

这不是 L4 no-progress 检测误杀：三题没有交付信号，18 Turn 介入符合
`progress:code-change/v3`。事故是 L5 把“当前 Activation 请求恢复”折叠为“整个
Graph 最终 blocked”，同时外部 architecture gate 没有检查 recovery Activation。

## 2. 漂移位置

1. **L4** 正确输出 typed intervention 和当前 Activation 的终态 TaskOutcome；
   L4 不应修改 Graph 或直接重开旧 Activation。
2. **L5 simple compiler** 曾生成 `work blocked → end_blocked`，违反“可恢复
   failed/blocked 默认进入 repair/recovery，只有裁决不可恢复才进入 end”的图约束。
3. **L5→L3 投影** 缺少冻结的 recovery controller 身份，无法为该节点授予精确
   GraphChange 控制工具；detached wake 的 Intervention scope 也未进入
   `get_task_result` 授权。
4. **测试缺口** 只验证 command/wake 字段、delivery 顺序和 ToolRouter 名单，没有
   执行 `intervention → Graph remains running → recovery decision → new Activation`。

## 3. 正式修复

### 3.1 L5 framework-owned recovery topology

新 simple Graph 固定加入 controller 节点 `recovery`：

```text
work completed → acceptance
work failed    → end_failed
work blocked   → recovery(failure_context)

recovery result.decision=retry   → work@new
recovery result.decision=blocked → end_blocked
recovery Runtime failed/blocked  → 独立 typed end
```

Acceptance 使用独立 `acceptance-recovery` Controller，避免多个失败来源汇入同一
普通节点。其 retry 边冻结 `replay_inputs=true`：Runtime 在选择边时把被恢复
Acceptance 上一 Activation 的 InputBinding 同条写进 TransitionRecord，新
Acceptance Activation 因而继续消费原 `work_result`/Evidence；Store 恢复会交叉
校验并重建这些绑定，不从文本或最新 Definition 猜测。

`recovery` 冻结 `controller_role=loop_recovery`、`ProgressCoordinationV1`、当前
Context policy、required `failure_context` 和 `$.decision` 输出契约。旧 `work@N`
保持冻结；retry 只创建新 Activation，GraphChange revision 也只影响未来 Activation。
framework 同时冻结 `recovery_max_retries=2`；超额 `decision=retry` 在提交期机械
拒绝并要求 `decision=blocked`，Controller 自身 intervention 被当前恢复边界吸收，
不递归生成无限 recovery 链。

### 3.2 L3 精确控制租约

Graph Task 持久化 `GraphControllerRole` 与 `RecoverySourceTaskID`。L3 对
`loop_recovery` controller 强制 `BusinessTools=[]`，只授予：

- `read_graph` / `get_task_result` / `read_content_ref`；
- `propose_graph_change` / `read_graph_change` /
  `validate_graph_change` / `commit_graph_change`；
- `submit_task_result`。

禁止 `run_shell`、文件写、legacy `patch_graph`、`report_done` 和跨 Graph 目标。
每个 Invocation 使用 `scheduler:graph-recovery` 单动作 phase。

### 3.3 Intervention delivery/ACK

Graph feed 结算 source Task 后若已发布匹配
`RecoverySourceTaskID=<source task>` 的 recovery controller，intervention bridge
不再重复创建 detached wake。只有 recovery controller 的 durable TaskOutcome
delivery 完成后才 ACK 原 intervention；重启时 source terminal 事件重放也能用已终态
recovery Outcome 补 ACK。旧 Graph 没有 recovery controller 时保留 detached wake
兼容路径，其 `InterventionGraph/Node/Activation` exact scope 现可读取来源结果。

### 3.4 外部架构门

SWE runner 新增 `loop_intervention_without_recovery`：Graph TaskOutcome 出现
`reason_code=loop_intervention_required`，却没有同 Graph `node_id=recovery` 的
TaskOutcome 时，`known_incidents_absent=false`。旧批次的三道 blocked 不再能被
`architecture_ok=8/8` 掩盖。

## 4. 回归覆盖

- L4 生产 Loop：intervention 阈值产生 terminal Activation TaskOutcome 与 typed
  command，lineage/reason 精确。
- L5 Runtime：`work@1 blocked → recovery@1 → retry → work@2 → success`，期间
  Graph 始终 running，旧 Activation 不重开。
- Bootstrap 跨层：真实 TaskStore + Graph Runtime + intervention bridge；recovery
  Outcome ACK source command，不创建 detached duplicate wake。
- L3：recovery controller 冻结 role/source、租约正向闭集、专用 phase、跨 Graph
  GraphChange 拒绝。
- Session：role/source 经 snapshot 往返保真。
- SWE runner：逐 source Task 核验 recovery outcome；缺少恢复的事故形状进入
  architecture incident，recovery Controller 自身的 bounded intervention 不递归计错。

## 5. 关闭条件

1. focused/full/race/vet/build 与真实二进制启动通过；
2. 新单题故意制造一次 no-progress intervention，观察 Graph 不终态、recovery
   controller 被认领、旧 command 在 recovery Outcome 后 ACK；
3. 三道历史失败题至少定向复跑，`loop_intervention_without_recovery=0`；
4. 若 recovery 决定 retry，证据中必须出现 `work@2`（或更高）且没有复用旧 Task；
5. architecture gate 不再把缺失 recovery 的 blocked 图判为正常架构运行。

## 6. 完成证据

- `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build -o agentgo .`
  与 `git diff --check` 全部通过；SWE runner 25 项 Python 测试通过。
- 离线跨层测试真实覆盖：L4 typed command、`work@1 blocked → recovery@1 →
  work@2`、recovery Outcome ACK、Acceptance 冻结输入 replay、Store Close/Recover
  后 replay binding 保真、retry budget 机械拒绝。
- 真实 DeepSeek `context-push-order` 首次复跑复现旧 Worker 18 轮空转，并观察到
  Graph 保持 running、`recovery@1` 两轮裁决、原 command 以 recovery OutcomeRef
  ACK、`work@2` 产出修复。该次又发现 Acceptance recovery 缺口，runner 的逐
  source gate 正确将其重判为架构失败；补齐后再次复跑为 Graph success、Judge
  487 passed/2 skipped、patch 18 行、双门通过。
- `pass-context-dispatch` 定向复跑两次进入 Worker recovery；模型最终仍未产出
  补丁，但每个 source intervention 都有 recovery Controller，当前 runner 回放为
  `loop_recovery_missing_sources=[] / architecture_ok=true`，因此归为业务失败。
- 最新二进制 `session-access-tracking` 定向复跑为 Graph success、Judge
  490 passed、patch 69 行、`architecture_ok=true / task_resolved=true`。
- 对第八轮旧三题原始产物重放新 gate，均能识别旧实现缺少 recovery；新产物不再
  触发 `loop_intervention_without_recovery`。
