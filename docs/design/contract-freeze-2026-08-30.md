# AgentGo 运行契约冻结基线（2026-08-30）

> 状态：Frozen baseline
> 目的：停止跨层语义原地漂移；后续实现只能修复本文件列出的契约接缝，
> 改变 wire、持久化或状态机语义必须发布新版本并提供显式恢复策略。

## 冻结版本

| 责任域 | 冻结契约 | 当前实现权威 |
|---|---|---|
| L1 Prompt | Scheduler `embedded:v10.11-recovery-evidence-v4`；SWE Worker/Verifier 当前受控文件 | `internal/scheduler/scheduler.go`、`prompts/swe/` |
| L2 Context | `context:default/v10`、`provider-replay:openai-compatible/v4`、新写入 `agentgo.observation-delta/v3`（v2 历史恢复） | `internal/policycatalog`、`internal/contextadapter`、`internal/tools/observation.go` |
| L3 Harness | ExecutionLease digest、RunContract `agentgo.run-contract/v2`、code-change RecoveryDelta `agentgo.recovery-delta/v4`、ChangeDecision `agentgo.change-decision/v1` | `internal/agent/execution_lease.go`、`internal/runcontract`、`internal/graph/recovery_delta.go` |
| L4 Loop | `progress:code-change/v6`、四阶段 Run deadline、TaskOutcome `agentgo.task-outcome/v3` | `internal/agent/loop_progress.go`、`internal/loopcontract`、`internal/outcome` |
| L5 Graph | `agentgo.graph/v3`、Delivery `agentgo.delivery/v1`、GraphChangeProposal 当前事务 | `internal/graph`、`internal/delivery` |
| 外部评测 | SWE Test Runner result v2、pytest phase report v1 | `scripts/swe_test_runner/` |

历史快照继续按自身版本恢复；不得把 v1/v2 数据静默补字段后当成当前版本。

## 本次允许的接缝修复

- L1：Prompt 只解释角色、动作顺序和输出契约；工具可见性以本轮
  ToolRouter/ExecutionLease 为准。Prompt 不写经验轮数、token 数、等待秒数或
  候选条数，机械阈值只存在于 policy/schema。
- L2：展示字段必须保留权威语义名称。`no_progress_turns`、
  `no_progress_model_calls` 不得缩写成容易被误读为累计总量的 `turns`、
  `model_calls`。
- L2：Observation v2 保留历史 confirmed 投影；新写入 v3 把模型自然语言 facts
  标成 inferred。settled evidence 只证明引用归属，不机械证明 claim 与正文之间的
  语义蕴含，inferred 只能进入“待验证观察”，不得晋升 Session 权威。
- L3/L5：RecoveryDelta v2 的单首动作语义保持冻结，供历史/acceptance 恢复。
  新 code-change handoff 发布 v4：首动作只允许带 path 的 `read_file`，并作为最小
  `EvidenceContract` 第一项。L3 逐段证明完整且新鲜的证据覆盖后只开放
  `submit_change_decision`；只有 Worker typed 选择 `edit` 才按声明 `{tool,path}`
  顺序开放 `edit_file`/`write_file` 与冻结 CheckContract。证据范围不限制修改目标，
  `need_context` 可扩展证据；`hypothesis_rejected`/`blocked` 安全交回 L5。v3 按原
  read→同路径 edit→check 语义恢复，参数错误不消耗 stage。
- `recovery_directive` 是单值端口；Graph retry 必须替换旧代。L3 为历史脏数据
  选择最后一代并用 `recovery_action_gated.directive_count` 暴露歧义，禁止再按首条
  输入静默执行旧指令。
- RecoveryDelta v1 只供历史恢复；v1 的 `first_required_action` 自由文本不得在
  新图中重新出现。v1-v3 均不得静默迁移为 v4。

## 变更纪律

1. 不在冻结版本内修改字段含义、默认权限、状态迁移或重放规则。
2. Prompt、schema、handler、Session snapshot、Graph replay、Trace/Test Runner
   必须在同一变更中对账；任何一侧缺失都不算完成。
3. 经验阈值进入 `policycatalog` 或 schema，Prompt 只引用“冻结契约”“有界窗口”
   等语义，不要求模型估算精确数字。
4. 新版本先定义旧版本恢复/拒绝规则，再切换 current alias；禁止按 provider、
   模型名或考题名称分支。

## 原冻结验证边界（历史）

本次按用户要求不启动真实/模拟模型，也不运行普通单元测试。只运行针对冻结接缝
的 Go benchmark（普通测试 `-run '^$'`）以及跳过 startup probe 的真实二进制
启动冒烟；benchmark 负责证明 v2 decode 与首动作 ToolRouter 可执行，启动冒烟
负责证明最终装配可启动。

本基线的实际验证结果记录于
[`2026-08-30-1030-contract-freeze-recovery-first-action.md`](../test-issues/2026-08-30-1030-contract-freeze-recovery-first-action.md)。

## 2026-08-30 L2-L4 接缝跟进

首轮 Recovery v3 用户复测后，L2 以新版本 `agentgo.observation-delta/v3`
修正模型 claim 的 authority，v2 历史恢复语义不变；L3 冻结 SWE targeted 的
exact command 并把单动作 fan-out 尾部改为 skipped；L4 让 finalizing 优先于
rollover/deadline/fuse。SWE Test Runner 同步改为按 committed Recovery Task 去重。

本次跟进按用户要求只编写回归源码，不运行测试、构建或冒烟；验证边界与问题编号
见 [`2026-08-30-2002-l2-l4-recovery-closure.md`](../test-issues/2026-08-30-2002-l2-l4-recovery-closure.md)。

## 2026-08-30 Recovery Evidence v4 修订

用户复测达到 `task_resolved=6/8`、`architecture_ok=8/8` 后，剩余失败表明 v3
把“读到一个目标文件”错误等同于“必须立刻修改同一文件”。v4 将责任重新分层：
L5 只冻结最小证据合同，L3 只证明覆盖并执行 typed decision，L4 仍以真实 mutation
和 CheckRecord 判完成，业务方案正确性留给模型。当前修订已运行定向/全量回归、
SWE Test Runner 单测，并由本地 Responses fake provider 驱动真实 Windows 二进制
完成 Recovery、Acceptance 与 Delivery promotion。详细证据见
[`2026-08-30-2356-recovery-evidence-contract-v4.md`](../test-issues/2026-08-30-2356-recovery-evidence-contract-v4.md)。
