# processTask reactLoop 终止路径

本文档详细列出 `internal/agent/agent.go` 中 `processTask` 的 reactLoop（`for i := 0; i < a.MaxLoops; i++`）的所有终止路径。

> **注意**：defer 链保证所有路径都会执行 `trace.CloseTask`、`OnTaskEnd`、`PhaseTaskEnd` 观察 hook。

## 终止路径总览

| 编号 | 触发条件 | 代码位置 | 是否调用 SubmitResult | 是否 emit trace 事件 | 是否写 TransferNote | 任务最终状态 | 是否检查 MaxRetries |
|:----:|----------|----------|:---------------------:|:--------------------:|:-------------------:|:-------------|:-------------------:|
| 1 | `a.Store.GetTask(taskID)` 返回 error | reactLoop 前 | 否 | 否 | 否 | processing（卡住，靠 watchdog 或 panic defer 兜底） | 否 |
| 2 | `select { case <-ctx.Done(): return }` 每轮循环开头检查 | reactLoop 入口处 | 否 | 否（ctx 取消通常来自 CancelRegistry 或外部超时） | 否 | processing（卡住，靠 watchdog 兜底） | 否 |
| 3 | `a.FinalizationChecker != nil && a.FinalizationChecker.IsFinalized()` | L427–436（LLM 调用前） | 是（用 lastOutput） | 否（代码中无 trace.Emit） | 是（lastOutput 直写） | completed（SubmitResult 成功时状态变为 submitted） | 否 |
| 4 | `execErr != nil` 且可恢复（`errors.As`）且 `task.RetryCount >= a.MaxRetries` | L449–451 调 `handleFailure`，L630–634 调 `terminateTask` | 否 | 取决于 `terminateTask` → `FailTask` 实现 | 是（buildTransferNote，contextOverflow 时先激进压缩 history） | failed | 是 |
| 5 | `execErr != nil` 且可恢复且 `task.RetryCount < a.MaxRetries` | L449–451 调 `handleFailure` | 否 | 否 | 是（buildTransferNote） | processing → available（通过 RetryRollback） | 是 |
| 6 | `execErr != nil` 且不可恢复（`!errors.As(execErr, &recoverable)`） | L449–451 调 `handleFailure`，走不可恢复分支 | 否 | 取决于 `FailTask` 实现 | 是（mechanicalTransferNote，纯机械 L3，不调 LLM） | failed | 否 |
| 7 | ExpectedArtifacts 校验失败（`!result.ToolCalled \|\| result.Finalized` 且 `len(check.Missing) > 0`） | L478–490 | 否 | 是（KindError） | 是（buildTransferNote，通过 handleFailure 可恢复路径） | 由 handleFailure 决定（可恢复→重试或终止） | 是 |
| 8 | ExpectedArtifacts 校验通过（`len(check.Missing) == 0`，含 Drifted 容忍） | L492–518 | 是（lastOutput） | 是（KindTaskSubmitted + KindTaskCompleted，仅 SubmitResult 成功时） | 是（lastOutput 直写） | completed（taskSuccess=true） | 否 |
| 9 | `for` 循环自然结束（`i >= a.MaxLoops`）且 `task.RetryCount >= a.MaxRetries` | L560–575 | 否 | 取决于 `terminateTask` / `FailTask` 实现 | 是（buildTransferNote） | failed | 是 |
| 10 | `for` 循环自然结束且 `task.RetryCount < a.MaxRetries` | L560–575 | 否 | 否 | 是（buildTransferNote） | processing → available（通过 RetryRollback） | 是 |
| 11 | processTask 中任意代码 panic，被 `defer recover()` 捕获 | L240–264 的 defer func | 否（调 FailTask） | 否（但 log 记录） | 是（mechanicalTransferNote，纯机械 L3） | failed | 否 |

## 补充说明

- **路径 1、2** 是 reactLoop 正常流程外的"提前退出"，任务状态保持 processing，需依赖外部 watchdog 或 panic defer 兜底。
- **路径 4、5、7** 走 `handleFailure` 分支，根据可恢复性和重试次数决定终止还是回滚。
- **路径 6** 是不可恢复错误（如 LLM 客户端崩溃），立即终止，不检查 MaxRetries。
- **路径 9、10** 是 MaxLoops 耗尽后的循环外逻辑，在 `for` 块结束后统一处理。
- **路径 11**（panic 恢复）是 Sprint 3 #5 引入的安全网，用纯机械 L3 生成 TransferNote，避免 panic 后任务永久卡死。

## TransferNote 写入方式对照

| 方式 | 适用场景 | LLM 调用 |
|------|----------|----------|
| lastOutput 直写 | 正常完成（路径 3、8）、FinalizationChecker | 否（直接复用 LLM 最后输出） |
| buildTransferNote | 可恢复错误重试前（路径 4、5、7）、MaxLoops 耗尽（路径 9、10） | 是（L1 追加指令，失败后 L3 兜底） |
| mechanicalTransferNote | 不可恢复错误（路径 6）、panic 恢复（路径 11） | 否（纯机械拼装） |
