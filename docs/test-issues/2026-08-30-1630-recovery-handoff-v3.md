# Recovery handoff v3 与最新指令一致性

> 日期：2026-08-30
> 状态：实现完成，等待用户运行真实 SWE 复测
> 范围：SWE-100～SWE-102

## 1. 事故证据

最新 Qwen Flask-8 批次机械架构门为 8/8，但三道失败题均为零 patch。
`pass-context-dispatch` 的 `recovery@2` 已提交
`first_action=edit_file(src/flask/app.py)`，随后 `work@3` 的首轮 ToolRouter 却仍只
暴露上一代 `read_file`。Graph replay 复制旧 `recovery_directive` 后追加新值，L3
又按正序返回首条，造成旧代抢占。

另外，`session-access-tracking` 与 `teardown-callbacks` 虽正确执行首读 gate，
ToolResult settled 后立即恢复完整工具面，模型再次进入 read/grep 循环，说明 v2
单首动作契约不足以把弱模型从调查推进到 mutation。

## 2. 修复

- **SWE-100 / L5+L2**：`recovery_directive` 改为单值端口；retry replay 先移除旧代
  再追加当前代。历史脏输入由 L3 选择最后一代，不再选择首条。
- **SWE-101 / L3+L4**：发布 `agentgo.recovery-delta/v3`。新 code-change recovery
  的首动作只能是目标 `read_file`；L3 随后机械推进同路径 `edit_file` 与冻结
  CheckContract 检查，形成 CheckRecord 后恢复普通业务工具面。v2 不原地变义，
  acceptance recovery 与历史快照继续按 v2 运行。
- **SWE-102 / Test Runner**：新增 `recovery_action_gated` trace，并把成功 retry、
  first-action gate 与实际 tool call 逐次对账。缺失、不一致或多 directive 都使
  `architecture_ok=false`，不再让 typed blocked 掩盖 handoff 错接。

## 3. 验证边界

本轮只执行不调用模型的 Go/Python 定向测试、benchmark、构建和跳过 provider probe
的启动冒烟。真实 Flask-8 结果由用户自行运行；在新批次完成前不得声称三道失败题
已经 resolved。

## 4. 本地验证证据

- `go test ./...`：全包通过。
- `py -3.13 -X utf8 -m unittest scripts/swe_test_runner/runner_test.py`：62 项通过。
- Python `py_compile`：Runner、Runner tests 与 local fake-provider smoke 均通过。
- v3 benchmark：`BenchmarkDecodeRecoveryDeltaV3` 与
  `BenchmarkRecoveryHandoffV3MutationPolicy` 通过。
- `go build -o agentgo.exe .` 通过；`-skip-startup-probe` 二进制装配输出
  “系统就绪”，stdin EOF 后丢弃空 Session 并以零退出码关闭。

上述验证不包含 provider 请求或真实 SWE task/batch。

## 5. 首轮用户复测跟进

首轮复测在 `pass-context-dispatch` 观察到两个真实 work handoff 均完整执行 v3
gate，`directive_count=1`，因此 `recovery_action_gate_missing` 不是 L3 首动作
漏接。事故来自 Recovery Controller 成功 finalizing 后被 L4 rollover，形成四条
raw retry receipt 对两条真实 gate；Test Runner 又按 receipt 总数比较而误报。

同轮还发现模型 Observation claim 被错误投影为 confirmed，以及 `targeted`
CheckContract 没有冻结 suite manifest 的完整测试范围。后续版本化修复与未执行的
回归规格见 [`2026-08-30-2002-l2-l4-recovery-closure.md`](2026-08-30-2002-l2-l4-recovery-closure.md)。
