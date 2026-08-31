# Observation 可信度、Recovery Check 与 finalizing 收口

> 日期：2026-08-30
> 状态：实现完成，回归测试已编写但按用户要求未执行
> 范围：SWE-103～SWE-106

## 1. 最新证据

`pass-context-dispatch` 最新 Qwen 运行达到 100 次模型调用后仍为 3 个 pytest
失败，候选 workspace 只在 `src/flask/app.py` 增加未接线的签名检测 helper。
该运行同时暴露四个独立的机械问题：

- TaskMemory 把仅由 `edit_file` / `read_file` receipt 支撑的自然语言陈述
  “AppContext 首参重构已完成”投影为 `confirmed=true`；证据归属成立，但语义
  结论与实际 diff 不一致。
- suite manifest 为 `tests/test_reqctx.py tests/test_subclassing.py`，RunContract
  的 `targeted` 却没有 exact command；Recovery Worker 自行缩窄为只执行
  `tests/test_reqctx.py`。
- Recovery 单动作阶段虽然只会 dispatch 首个调用，batch preflight 仍检查尾部
  调用的 allowlist；模型返回 `edit_file + read_content_ref + grep_search` 时整批
  被拒绝，没有利用已有的 skipped-result fence。
- 两个 Recovery Controller 都出现 `task_finalizing → task_retry`：成功
  `submit_recovery_decision` 后，同 Turn 的 L4 rollover 覆盖了 finalizing，下一
  Attempt 再提交一次。SWE Test Runner 按四条 raw retry receipt 对两条实际
  first-action gate，误报 `recovery_action_gate_missing`。

## 2. 修复

- **SWE-103 / L2**：发布 `agentgo.observation-delta/v3`。模型 facts 由 framework
  固定写成 `authority=inferred`，TaskMemory 在“待验证观察”独立段渲染，不能
  混入 confirmed 或晋升 Session 权威。v2 对象按旧 confirmed 语义恢复，不原地
  改写历史 wire。
- **SWE-104 / L3 CheckContract**：SWE Test Runner 从当前任务 manifest 的
  `test_files` 生成 POSIX sh / PowerShell 共通的 exact targeted pytest 命令；
  Recovery check ToolRouter 同时 const 冻结 `check_id`、`kind` 与 `command`，
  `run_check` 继续在 Shell 前逐字执法。
- **SWE-105 / L3 fan-out**：单动作 phase 仍校验全部 call_id 和工具名协议格式，
  但 allowlist 只校验真正会 dispatch 的首个调用；合法尾部工具统一生成 skipped
  receipt，不再拒绝首个正确动作。
- **SWE-106 / L4 + Test Runner**：Turn settlement 后一旦 FinalizationChecker
  已置位，只结算 token、History、TaskMemory 并进入唯一终态事务，跳过所有
  rollover/intervention；循环顶部 finalization 又前移到 Attempt deadline 与
  runtime fuse 之前。Test Runner 只对已出现 `task_result_committed` 的 Recovery
  Task 取最终一次成功裁决，Attempt 重放 receipt 不重复计数。

## 3. 回归源码

- `internal/taskmem/observation_test.go`：v3 inferred 投影与 authority fail-closed。
- `internal/taskmem/render_test.go`：inferred 只能进入“待验证观察”。
- `internal/tools/observation_test.go`：新写入 receipt 使用 v3。
- `internal/agent/recovery_action_gate_test.go`：Recovery check 冻结完整三元组。
- `internal/agent/tool_router_snapshot_test.go`：正确首调用 + 未授权尾部 fan-out。
- `internal/agent/loop_progress_enforcement_test.go`：finalizing 压过同 Turn rollover。
- `scripts/swe_test_runner/runner_test.py`：exact targeted 命令与 Recovery receipt
  Attempt 去重。

## 4. 验证边界

本次遵从用户要求，不运行 Go/Python 测试、构建、二进制冒烟、provider probe 或
真实 SWE。上述测试仅作为待执行的回归规格；在用户完成验证前，不得把
SWE-103～106 或后续 Flask-8 行为收益描述成已验证关闭。
