# Windows SWE EOL 与 Watchdog Attempt 身份修复（2026-08-27）

## 状态

已修复。最终 Windows Flask-8 批次在冻结版本上达到业务门与架构门双 8/8，
没有 infrastructure error、not-run、stale 或测试篡改。

## 事故形状与五层归因

### L3 Harness：CRLF 被误判为测试篡改

首轮八题的 Graph 与最终 pytest 均成功，但 Judge 全部返回 `test_tampered`。
实际 `model.patch` 只修改 `src/flask/`；受保护测试在 Git clean filter 后的 blob
与目标 fix commit 完全一致，工作树也没有 tests diff。差异只来自 Windows checkout
把仓库 LF 展开为 CRLF，而旧 Judge 直接比较 `git show` 的 LF bytes 与工作树 raw
bytes。

修复后，prepare 在 Agent 启动前把受保护测试文件的实际工作树 bytes、存在状态与
SHA-256 冻结为 `agentgo.swe-test-baseline/v1` manifest，并原子写到 ProjectRoot
之外的 run 目录。Judge 对 schema、精确文件集合、存在状态、文件类型与 SHA-256
进行同源比较；manifest 缺失或非法时以 `test_baseline_manifest_invalid` fail-closed。

L1 worker prompt 原本已经明确禁止修改 `tests/`，因此没有用重复提示词掩盖 L3
机械判定缺陷。L2 Context 与 L5 Graph 在该事故中没有发现需要修改的责任边界。

### L4 Loop/Watchdog：Windows 同 tick 重试未重新武装告警

Windows 全仓测试稳定暴露 `TestWatchdog_OvertimeWarningRearmsOnRetry` 失败。
Watchdog 原先只用 `StartedAt` 作为一次性超时告警的执行身份；两次 Claim 发生在同一
时钟 tick 时，新 Attempt 可能得到相同时间值，因而被误认为旧执行租约。

修复后去重身份优先使用 Store 在每次 Claim 边界生成的稳定 `AttemptID`，只有旧
快照缺少 AttemptID 时才回退 `StartedAt`。回归测试固定两个不同 Attempt 使用完全
相同的 StartedAt，验证两次均告警且同一 Attempt 不重复。

## 专用测试拓扑

`setting.swe-flask.yaml` 使用 2 个 Explorer、2 个 Worker、1 个 Verifier；Scheduler
与全部执行代理均由 `SWE_MODEL` 渲染为同一模型。本轮最终运行使用
`deepseek-v4-flash`。

## 回归证据

Windows PowerShell 验证结果：

- `py -3.13 -X utf8 -m unittest scripts/swe_harness/harness_test.py`：50 项通过；
- Watchdog 同 tick 定向测试连续 20 次通过；
- `go test ./...`：全部通过；
- `go build -o agentgo.exe .`：通过；
- `py -3.13 -X utf8 scripts/swe_harness/harness.py probe --timeout 45`：Responses
  typed function-call 探针通过；
- 最终 `.batch_start` 绑定的 `summary.json`：8 行全部 completed，
  `task_resolved=8/8`、`architecture_ok=8/8`，所有 Judge 为 `resolved` 且
  `tampered=false`，`infrastructure_error=0`、`not_run=0`、`stale=0`。
