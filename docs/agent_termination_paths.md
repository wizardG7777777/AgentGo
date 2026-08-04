# processTask reactLoop 终止路径

本文档详细列出 `internal/agent/agent.go` 中 `processTask` 的 reactLoop 的所有终止路径。

> **V6 变更**：固定轮数上限（`agent_max_loops` / `MaxLoops`）已删除，循环形为 `for i := 0; ; i++`，不再因到达轮数终止，也不再有「MaxLoops 耗尽 → TransferNote → RetryRollback」路径。循环由结构化终态、取消、deadline、预算共同约束；另有一个不可配置的 emergency fuse（`emergencyLoopFuse = 10000`）兜底程序性死循环，触发后任务进 blocked 并登记 replan，绝不自动重跑同一 Task（路径 9）。
>
> **注意**：defer 链保证所有路径都会执行 `trace.CloseTask`、`OnTaskEnd`、状态机收尾。

## 终止路径总览

| 编号 | 触发条件 | 代码位置 | 是否调用 SubmitResult | 是否 emit trace 事件 | 是否写 TransferNote | 任务最终状态 | 是否检查 MaxRetries |
|:----:|----------|----------|:---------------------:|:--------------------:|:-------------------:|:-------------|:-------------------:|
| 1 | `a.Store.GetTask(taskID)` 返回 error | reactLoop 前 | 否 | 否 | 否 | processing（卡住，靠 watchdog 或 panic defer 兜底） | 否 |
| 2 | `select { case <-ctx.Done(): return }` 每轮循环开头检查 | reactLoop 入口处 | 否 | 是（KindTaskCancelled） | 否 | cancelled | 否 |
| 3 | `a.FinalizationChecker != nil && a.FinalizationChecker.IsFinalized()` | reactLoop 内（LLM 调用前） | 是（用 lastOutput） | 是（KindTaskSubmitted + KindTaskCompleted） | 是（lastOutput 直写） | completed | 否 |
| 4 | `execErr != nil` 且可恢复（`errors.As`）且 `task.RetryCount >= a.MaxRetries` | `handleFailure` → `terminateTask` | 否 | 是（KindTaskFailed） | 是（buildTransferNote，contextOverflow 时先激进压缩 history） | failed | 是 |
| 5 | `execErr != nil` 且可恢复且 `task.RetryCount < a.MaxRetries` | `handleFailure` | 否 | 是（KindTaskRetry） | 是（buildTransferNote / L3） | processing → pending（通过 RetryRollback） | 是 |
| 6 | `execErr != nil` 且不可恢复（`!errors.As(execErr, &recoverable)`） | `handleFailure` 不可恢复分支 | 否 | 是（KindTaskFailed） | 是（mechanicalTransferNote，纯机械 L3，不调 LLM） | failed | 否 |
| 7 | ExpectedArtifacts 校验失败（`!result.ToolCalled \|\| result.Finalized` 且 `len(check.Missing) > 0`） | reactLoop 内 | 否 | 是（KindError） | 是（buildTransferNote，通过 handleFailure 可恢复路径） | 由 handleFailure 决定（可恢复→重试或终止） | 是 |
| 8 | ExpectedArtifacts 校验通过（`len(check.Missing) == 0`，含 Drifted 容忍） | reactLoop 内 | 是（lastOutput） | 是（KindTaskSubmitted + KindTaskCompleted，仅 SubmitResult 成功时） | 是（lastOutput 直写） | completed（taskSuccess=true） | 否 |
| 9 | emergency fuse：循环计数 `i >= a.loopFuseLimit()`（默认 10000），判定程序性死循环 | reactLoop 顶部（ctx 取消检查之后） | 否 | 是（KindRuntimeLoopFuseTriggered + KindTaskBlocked） | 否（刻意不再调 LLM；LastHistory 落盘仅供排查） | blocked（终态，不重跑）+ 登记高优 ReplanRequest（ReasonCode=runtime_loop_fuse） | 否 |
| 10 | processTask 中任意代码 panic，被 `defer recover()` 捕获 | defer func | 否（调 FailTask） | 是（KindTaskFailed） | 是（mechanicalTransferNote，纯机械 L3） | failed | 否 |

## 补充说明

- **路径 1** 是 reactLoop 正常流程外的"提前退出"，任务状态保持 processing，需依赖外部 watchdog 或 panic defer 兜底。
- **路径 4、5、7** 走 `handleFailure` 分支，根据可恢复性和重试次数决定终止还是回滚。
- **路径 6** 是不可恢复错误（如 LLM 客户端崩溃），立即终止，不检查 MaxRetries。
- **路径 9**（emergency fuse）是 V6 引入的程序缺陷防御兜底，不是正常终止条件：fuse 路径不再发起任何 LLM 调用（连 L1 TransferNote 也不做），任务进 blocked 终态并交 Scheduler 经 replan 裁决，恢复只能产生新 Task。
- **路径 10**（panic 恢复）是 Sprint 3 #5 引入的安全网，用纯机械 L3 生成 TransferNote，避免 panic 后任务永久卡死。

## TransferNote 写入方式对照

| 方式 | 适用场景 | LLM 调用 |
|------|----------|----------|
| lastOutput 直写 | 正常完成（路径 3、8）、FinalizationChecker | 否（直接复用 LLM 最后输出） |
| buildTransferNote | 可恢复错误重试前（路径 4、5、7） | 是（L1 追加指令，失败后 L3 兜底） |
| mechanicalTransferNote | 不可恢复错误（路径 6）、panic 恢复（路径 10） | 否（纯机械拼装） |
