# SWE-051～053：Run 预算作用域、Control Invocation 与 Recovery Permit

> 日期：2026-08-24
> 状态：closed / repository-validation-complete / external-validation-complete

## 2026-08-25 外部回归补充：Acceptance Control Lane 不可达

新的 8 题批次在第 5 题 `pass-context-dispatch` 后按架构门停止。该题业务 Judge
已 resolved，但 Acceptance 的 `progress:verification/v2` 在第 8 个知识 turn
触发 Observation 后，冻结 Lease 只含只读业务工具与 `submit_task_result`；原
executor 又只保留换入后的任务视图，导致 exact `record_observation_delta` 在
provider dispatch 前不可达。L4 两次修正后恢复业务，随后每个新知识 turn 重复
同一失败，共产生 20 个本地 preflight 失败。

同时，L4 旧结算把每个已预留 model action 固定记为 `model_calls=1`，即使请求
从未到达 provider。因而 Ledger 为 116、trace 为 96，而两边 prompt/completion
token 完全一致。SWE gate 正确停批，但把根因显示成
`run_budget_usage_mismatch`；原 `control_checkpoint_unavailable` 只读取终态
TaskOutcome，看不到周期性 checkpoint 恢复路径。

正式补充修复：

- `LLMExecutor` 保留启动期 framework tool authority；ExecutionLease 仍只裁剪
  普通业务视图。Observation phase 从 framework authority exact 取唯一工具，
  Acceptance 普通轮仍看不到 `record_observation_delta`。
- `ExecuteResult.ProviderCallStarted` 冻结 provider dispatch 事实；L4/RunBudget
  只有该事实为真才结算 `model_calls=1`。本地 preflight 失败结算 0，并释放
  reservation。
- 新增 `observation_checkpoint_failed` trace；周期性非终态失败也可按
  `control_invocation_preflight_failed` 被 SWE gate 识别，不再只靠终态 Outcome。
- 本地真实二进制 fake-provider 现同时让 Worker 读取 12 次、Acceptance 读取
  9 次；两者均成功经过 exact Observation，Graph/Check/final-report 完整收口，
  Ledger model calls 与 trace model calls 相等。

## 事故事实

最新 8 题 SWE cohort 为 `task_resolved=5/8 / architecture_ok=8/8`。三个失败题中：

- `ipv6-session-txn` 在 Observation checkpoint 连续两次返回当前 ToolRouter 未授权
  的 read/shell 工具，L3 正确拒绝，但 L4 以
  `observation_checkpoint_failed` 终结业务 Activation；
- `pass-context-dispatch` 在 execution deadline 前未完成有效修改，新的时间可行性
  门正确拒绝了无法启动的 retry；
- `session-access-tracking` 的 `work@1` 恰好累计 64 model calls 后报告
  “Run model_calls 预算耗尽”，Recovery 却创建 `work@2` 并继续执行 14 calls；整个
  Run 最终记录 88 calls。

代码审计证明：旧实现把 framework 所称的 Run 总预算复制进每个
`loopProgressRuntime`，再从 `LoopStore.LoadCheckpoint(task.ID)` 的
`ProgressCheckpoint.CumulativeUsage` 计算余额。新 Activation 使用新 TaskID，因而
自动重置所谓 Run 预算。`RunBudgetRef` 只是固定策略字符串，没有对应的 mutable
Run authority。

## 五层归属

| 编号 | 主责层 | 根因 | 后果 |
|---|---|---|---|
| SWE-051 | L4（L5/Bootstrap 装配） | Run 级可累加额度由 Task-local Checkpoint 执法；额度轴没有与 Attempt/no-progress 轴分离 | 新 Activation 重置调用/token/cost 授权；Trace 与错误文案把 Activation 限制误称为 Run 限制 |
| SWE-052 | L3 Harness / Invocation | 非终态 Observation 为保持业务 thinking 使用 auto-singleton，provider 仍可生成未声明工具 | 安全门正确拒绝，但 framework checkpoint 可用性依赖模型遵循阶段工具 |
| SWE-053 | L5 Graph / 外部观测 | Recovery retry 只检查 execution 时间窗，没有持有 Run execution grant；SWE gate 不核对 Run ledger | 检查与发布间可竞争，`architecture_ok` 无法发现预算重置或控制 checkpoint 不可用 |

## 正式修复

### RunBudgetLedger 与不收紧原则

- 新增 append-only `internal/runbudget`，按 RunID 保存 contract digest、显式业务
  limit、phase、reservation、claim、settlement 和累计 usage；每次 append
  flush+fsync，恢复校验 per-Run sequence/digest。
- `RunContract.Budget` 的 model/tool/token/cost 非零维度只在 Ledger 中跨 Task
  执法；wall time 继续由绝对 deadline 执法，Attempts 保持 Activation-local。
- 新 profile `interactive/v3` / `swe/v3` 不把旧的 64-per-Task 偷换成
  64-per-Run：默认 model/tool/token/cost 继续完整记账但不作为正常终止条件，
  deadline、no-progress、Observation、Attempt 与 emergency fuse 保持硬边界。
- coordination/recovery/finalization 使用独立 phase entitlement，不消费显式业务
  execution limit；它们仍受各自冻结 ProgressContract、Lease 与时间 reserve 约束，
  不能借控制额度执行文件编辑、Shell 或业务验证。

### L4 reservation/settlement

- 每个 model/tool action 同时写 task-local LoopStore 和 RunBudgetLedger；全局
  reservation 失败时不 dispatch，局部写失败时取消全局 reservation。
- model/tool 返回后以同 action identity 结算；并发 Activation 由 Ledger 原子
  reservation 防止超发。
- 本地 framework profile 改称 Activation 护栏，错误码/文案不再把本地 64 calls
  伪装成 Run 总量。

### L5 RecoveryStartPermit

- Recovery 在绑定 framework-owned RecoveryDelta 时，从 RunBudgetLedger 预留下一
  execution Activation 的首个 model-call slot，生成稳定
  `run-permit:sha256:*`。
- PermitRef 随 RecoveryDelta/recovery_directive、TaskSpec、Task 与 Session snapshot
  冻结；目标 Task 首轮把 permit 原子 claim 到真实 action_id，随后 settlement。
- Runtime outlet/settlement 同时验证 execution deadline 与 permit；没有 grant 只能
  blocked。Recovery 控制额度不能被解释成新的业务 grant。

### L3 Observation Control Invocation

- Observation checkpoint 改为独立 control lane：只消费冻结 TaskMemory、phase
  contract、dynamic evidence enum 和 singleton tool schema。
- wire 使用 `reasoning=none + exact record_observation_delta`；正常业务调用仍保持
  thinking。
- Observation control ToolCall/ToolResult 不进入后续业务 Responses replay；durable
  ObservationRef 通过 TaskMemory 注入，原始业务历史不改写。

### SWE 事故门

- 新增 Run budget ledger 投影与 phase usage；检测 ledger 缺失、trace/ledger
  model-call 不一致、终态 active reservation、execution usage 越过显式 limit。
- `observation_checkpoint_failed` 现在形成 `control_checkpoint_unavailable`，不再被
  `architecture_ok=true` 掩盖。

## 验证与关闭条件

初始实现完成 35 项 Python runner、`go test ./...`、`go test -race ./...`、
`go vet ./...`、`go build -o agentgo .`、`git diff --check` 与本地
fake-provider 真实二进制：单顶层 Graph
success，12 次读取跨 Observation exact control lane 后继续；blocked work 经
RecoveryStartPermit 创建 `work@2`，随后写入、typed check、Acceptance、final-report
全部完成；RunBudget 112 条 durable record 的 reservation 全部 settlement。

2026-08-25 补充修复的本地验收为 36 项 Python runner、相关 Go 包、构建与上述
Acceptance 跨阈值真实二进制 smoke。

外部关闭条件：指定 SWE 监控任务重新构建后执行 8 题，必须满足：

- `run_budget_ledger_missing/run_budget_usage_mismatch/run_budget_reservation_leak=0`；
- `run_budget_scope_reset/control_checkpoint_unavailable=0`；
- Observation checkpoint 不再产生错阶段 business tool；
- retry 必须携带并消费 RecoveryStartPermit；
- architecture incident 为零，业务 Judge 结果与架构门继续分开统计。

### 外部关闭证据

指定监控任务在 provider 恢复后执行当前批次（`batch_start=2026-08-25
11:19:31 +0800`）：8/8 行均为当前 `completed`，`task_resolved=8/8`、
`architecture_ok=8/8`、`infrastructure_ok=8/8`。逐题 RunBudget settled model
calls 与 trace 完全一致（21/21、63/63、18/18、34/34、113/113、24/24、
59/59、41/41），active reservation 全为 0；known incidents、hard kill、pending
Outcome delivery 均为 0，Graph、final-report 与 ACK 全部成功。SWE-051～053 关闭。
