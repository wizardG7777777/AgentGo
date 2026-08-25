# SWE-046～050：Observation 状态、收敛与 Recovery 可启动性

> 日期：2026-08-24
> 状态：closed / repository-validation-complete / external-validation-complete

## 事故事实

`session-access-tracking` 单题 Run
`run-swe-session-access-tracking-df7d829a-f833-4e18-8209-1c0f967a6758`
最终为 `architecture_ok=true / task_resolved=false`。Judge 为 489 passed / 1 failed，
Worker 在 68 次模型调用中累计约 5.89M prompt tokens，产生 30 次 grep、21 次
shell、18 次读取、4 次 Observation、2 次编辑，却没有形成通过的 typed check。

关键时间线：execution phase 于 02:23:09 截止；Worker 的最后 Invocation 在该时刻
以 `attempt_deadline` 结束。Recovery Controller 随后在 02:23:29 合法提交 retry，
L5 才在发布 `work@2` 时发现 execution window 已关闭，并把 Graph 置 failed。
final-report 又错误声称没有源码修改，尽管运行中存在 2 次编辑与 artifact。

## 五层归属

| 编号 | 主责层 | 根因 | 后果 |
|---|---|---|---|
| SWE-046 | L2 Context | Observation 是无 predecessor 的文本追加；TaskMemory 不替换旧 Observation facts，也没有候选关闭证明 | 旧计划、重复事实和已过时的调查方向持续进入 Context |
| SWE-047 | L4 Loop | 任意新 digest 都重置进展时钟；周期 Observation 只计数，不判断状态是否前进 | 模型可用语义重复但字节不同的读/grep 一直刷新 knowledge progress |
| SWE-048 | L5 Graph（消费 L4 deadline） | Recovery retry commit 前未校验下一 execution Activation 是否仍可启动 | `submit_recovery_decision` 成功后必然在 `work@2` 发布失败 |
| SWE-049 | L3 Harness / Invocation | phase ToolRouter 拒绝合法 response 中的错工具，被记成 provider `malformed_response` | provider 协议故障与 L3 action-contract 拒绝混淆 |
| SWE-050 | L2/L5 finalization | TerminalSummary 不含 Task 是否发布、settlement reason code 和累计 workspace/artifact 事实 | final-report 把未发布的 work@2 说成执行过，并把已有修改说成零修改 |

## 正式修复

### L2：`agentgo.observation-delta/v2`

- v1 不再读取或生成；v2 必须形成 `previous_ref` 链。
- phase 使用封闭枚举 `investigate|implement|verify|finalize|blocked`。
- 下一步由 framework 生成稳定 `candidate:sha256:*`；后续只能用 predecessor
  receipt 给出的 ref 关闭候选。
- 关闭候选必须引用 predecessor 创建之后的新 settled evidence；旧证据不能
  被重复使用来伪造状态前进。
- workspace revision、latest CheckRef 与 `semantic_advance` 均由 framework
  绑定/计算，模型无权自述。
- TaskMemory 只投影最新 Observation 的当前 facts/next candidates；immutable
  Observation 文件继续 append-only 保留审计链。
- 删除 TaskMemory 固定 1500-rune 预截断；完整的结构化有界状态交给当前模型的
  L2 Context policy 统一做 inline/reference。渲染优先级改为当前
  blockers/facts/next candidates 先于历史 actions，避免动作日志挤掉恢复状态。

### L4：语义收敛而非固定交卷

- `TurnSettlementDelta` 增加 typed `ObservationChange`。
- phase 前进、关闭 predecessor candidate、workspace revision 前进或 CheckRef
  前进才产生 `observation_state_advanced`。
- 只改事实/候选措辞的 checkpoint 不刷新语义进展；连续两份无语义前进的
  Observation 产生 `observation_state_stalled` intervention，交 L5 换策略，
  不强制 completed，也不重新引入 business exploration 固定交卷。

### L5：Recovery feasibility gate

- `submit_recovery_decision(retry)` 在绑定 RecoveryDelta 前调用
  `ValidateRecoveryRetryStart`。
- execution phase 已关闭时返回稳定
  `reason_code=recovery_retry_unstartable; allowed_decisions=[blocked]`，Controller
  必须改交 blocked；Recovery reserve 不得重新解释为业务执行时间。
- Runtime outlet/settlement 再做同一校验，封闭工具旁路与时钟竞态。
- SWE runner 新增 `recovery_retry_activation_unstartable` 事故门；若旧事故形状
  仍进入 Graph settlement，`architecture_ok=false`。

### L3 与 finalization

- 新增 Invocation failure kind `action_contract_rejected`；缺必需调用、超过阶段
  数量或调用未授权工具不再计为 `malformed_response`。
- `agentgo.graph-terminal-summary/v2` 增加 `task_published`、
  `settlement_reason_code`、`workspace_changed`、workspace task/artifact 计数。
- final-report Prompt 禁止在 `task_published=false` 时声称 Activation 已执行，
  也禁止在 `workspace_changed=true` 时声称零修改。

## 本地验证

- Python SWE runner：34 tests passed；
- `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build -o agentgo .`
  与 `git diff --check` passed；
- Observation predecessor、候选证据时序、最新状态投影、语义 stagnation、
  recovery phase preflight、action-contract 分类与 TerminalSummary v2 均有 focused
  contract tests。
- 真实二进制 local fake-provider smoke：单顶层 Graph success；12 个 read
  跨过 Observation checkpoint 后继续，随后 recovery、workspace write、typed
  CheckRecord、Acceptance、final-report 全部 durable 完成；Observation 文件均为 v2。

外部关闭证据（2026-08-25）：指定监控任务重新编译后完成一批 8 题，
`task_resolved=8/8`、`architecture_ok=8/8`、`infrastructure_ok=8/8`；
`recovery_retry_activation_unstartable`、Observation control、final-report scope、
TerminalSummary 与所有 known incident 门均为 0，Graph/final-report/Outcome ACK
完整。SWE-046～050 关闭。
