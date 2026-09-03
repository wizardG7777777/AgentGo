# Recovery v4 skipped fan-out 被误判为证据失败

> 日期：2026-08-31
> 状态：Closed / Flask-8 architecture 8/8、Qwen business 5/8 verified
> 范围：SWE-111～SWE-117

## 1. 外部证据

`pass-context-dispatch` 最新单题 Run
`run-swe-pass-context-dispatch-7b031ccf-5d73-4c41-9e64-d159dce73ee9`
最终 Judge 为 `3 failed, 487 passed`、`patch_lines=0`。真正令
`architecture_ok=false` 的 incident 是 `recovery_action_gate_mismatch`。

Recovery v4 在 `work@2` 的真实顺序为：

1. `read_file(src/flask/app.py, offset=1, limit=160)` 成功；
2. 下一轮 provider 返回四个分页读取，L3 按单动作契约只执行首个
   `offset=161`，其余 call_id 记录 `phase_single_action_fanout` skipped；
3. Evidence Ledger 把带原始 `offset=321` 参数的 skipped ToolResult 当成读取失败，
   错误进入 `agent:recovery-evidence-unavailable`；
4. 该 gate 又携带 `path=src/flask/app.py`，但唯一工具
   `submit_change_decision(blocked|hypothesis_rejected)` 没有 path 参数，SWE Test
   Runner 因此正确记录 gate/tool args mismatch；
5. Worker 的后续 `read_file` 被 L3 拒绝，最终只能提交
   `hypothesis_rejected`，Graph blocked 且没有补丁。

## 2. 五层归因

- **L1**：模型曾尝试继续读文件，不是 Prompt 要求盲改。
- **L2**：前两页正文均成功进入 History；文件级 hash 相同是正常的新鲜度身份，
  不是重复页证据。
- **L3**：根因。未 dispatch 的 skipped receipt 错误改变 Evidence Ledger 状态；
  decision gate 同时声明了不存在的 path 参数约束。
- **L4**：Observation-before-blocked 正常工作，不是本次根因。
- **L5**：Recovery retry 与第二次 blocked 裁决均正常持久化。

## 3. 修复

- 将 skipped ToolResult 与真实 error 分开识别；Evidence file 与 ContentRef 两条
  覆盖账本均完全忽略 skipped，只有真实 dispatch 后错误才进入
  `evidence_unavailable`。
- `evidence_unavailable` 继续只开放
  `submit_change_decision(hypothesis_rejected|blocked)`，但不再把失败证据路径写入
  通用 gate `path`，避免声明工具并不存在的参数契约。
- 不改变 `agentgo.recovery-delta/v4`、ChangeDecision schema、权限或状态迁移；
  v1-v3 恢复语义不变。
- **SWE-113**：v4 evidence gate 将 `force_full=true` 写入 runtime gate 与脱敏
  trace，并在首个调用 dispatch 前校验 path/ref/check/offset/limit/force_full 等
  已冻结参数。provider 忽略 JSON schema const 时返回可修正的
  `action_contract_rejected`，不得把错误参数交给真实工具；SWE Test Runner 同步对账。
- **SWE-114 / SWE Test Runner**：旧 incident 以全局 `error_text` 分别查找
  `recovery_delta` 与“缺少/非法”等词，可能把一条已修正的 strategy 长度错误和
  另一条无关 grep 缺参拼成永久架构事故。新判定按同一
  `submit_recovery_decision` ToolResult/Task 关联；同 Task 后续成功裁决并
  `task_result_committed` 即视为已修正，只有未收口 rejection 才令架构门失败。
- **SWE-115 / L4**：完整八题与补跑显示复杂题常在 periodic Observation 第二次
  item 校验失败后以 `control_contract_unstable` 终结，而无效 facts/closure 并未
  产生副作用或终态 authority。发布 `progress:code-change/v7`：周期性 checkpoint
  有界失败后保留 Raw History 并恢复业务；rollover/intervention/terminal 仍
  fail-closed。v6 快照保持原 durable failure 语义，不按模型/provider 特判。
- **SWE-116 / L1**：v7 真实复测中 Worker 已在 Observation next candidates 明确
  声明编辑 `sessions.py`，恢复业务后仍继续 read/grep，最终从未 mutation。Worker
  Prompt 将 Observation 定义为行动承诺边界：模型自己声明具体 edit/write 且无新证据
  否定时，下一普通业务动作必须执行；不安全方案必须标为 need_context/待证伪。
  Prompt 不包含机械阈值，Recovery/control 阶段仍以 L3 ToolRouter 为唯一权威。
- **SWE-117 / L2+L3**：Prompt 复测仍在成功 Observation 后继续 read_file，证明自然
  语言承诺不足。发布 `agentgo.observation-delta/v4`：新增 typed next_action；模型
  自选 mutate 时声明 edit_file/write_file + 项目相对 path，下一普通业务轮只开放该
  mutation 并在 handler 前校验路径。continue/need_context/verify/blocked 不触发动作；
  Recovery gate 优先，v2/v3 历史对象不迁移。

## 4. 验证

- 新增真实事故形状回归：第一页成功；第二个 Turn 首分页成功、后续分页 skipped；
  下一 gate 必须继续 `read_file offset=321`，不得进入 evidence unavailable。
- 证据真实读取失败的安全退出回归继续通过，并新增 decision gate `path` 必须为空的断言。
- 冻结参数执行门新增正反回归：`force_full=true` 放行，false 在 dispatch 前以
  `action_contract_rejected` 拒绝；SWE Test Runner 同步验证 trace mismatch。
- `go test ./internal/agent ./internal/trace`、`go test ./...`、`go vet ./...`：通过。
- SWE Test Runner Python 单测 66 项：通过。
- `go build -trimpath -o .\agentgo.exe .`：通过。
- 本地 fake provider 驱动真实 Windows 二进制：Graph success、Acceptance/Delivery
  committed、Run Budget reservation 全部结算；工作区/索引 LF 审计与
  `git diff --check` 通过。

## 5. 验证边界

修复证明 L3 不再因 provider fan-out 自行截断证据读取；它本身不保证小模型能够
正确实现 Flask dispatch 兼容逻辑。第 6 节已经用相同模型完成整批复测，并以新的
`result.json`、Recovery gate 序列、补丁和最终 Judge 将架构收益与模型能力分开。

## 6. 最终完整批次证据（2026-09-01）

同一 `qwen3.8-flash`、当前 Progress v7 + Observation v4 + Recovery v4 完整批次：

- `batch_status=complete`，completed `8/8`，infrastructure/not_run 均为 0；
- `architecture_ok=8/8`，无 `control_contract_unstable`、quota、外部 hard kill；
- `task_resolved=5/8`：automatic-options、context-push-order、ipv6-server-name、
  ipv6-session-txn、secret-key-rotation resolved；
- pass-context-dispatch、session-access-tracking、teardown-callbacks 业务失败。

Observation v4 已在真实任务形成 `next_action=mutate → agent:observation-commitment →
edit_file`，并产生非空 workspace revision。三道剩余失败题的最终审计为：

- session-access-tracking 与 teardown-callbacks 均多次 mutation 并运行 targeted check，
  但候选代码未满足测试语义；
- pass-context-dispatch 主要选择 continue/need_context，Recovery 中选择 mutate 时完整
  EvidenceContract 尚未覆盖即到期；
- 所有错误候选均被 Check/Acceptance/Graph 隔离，未 promotion 主根。

因此 SWE-111～117 的架构接缝已获外部 closure；剩余 3 题是当前模型的代码语义、
上下文范围选择和失败反馈利用能力边界，不能通过为具体 Flask 答案增加 Gate 解决。
