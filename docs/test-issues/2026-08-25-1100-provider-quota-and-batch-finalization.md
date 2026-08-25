# SWE-054～056：Provider 额度、Invocation intervention 与批次异常收口

> 日期：2026-08-25
> 状态：closed / repository-validation-complete / external-validation-complete

## 事故事实

批次 `2026-08-25 01:09:43 +0800` 只生成 6 份新 `result.json`：前 5 题
Graph/Judge success；`secret-key-rotation` 的 Worker 修改与最终 Judge 已通过，
但 Acceptance 第 6 轮收到 HTTP 402 `Insufficient Balance`，Graph typed failed；
下一题 `session-access-tracking` 的启动期 function-call probe 同样收到 402，
AgentGo 按 `startup_probe_failure_action=exit` 正确退出，最后一题未启动。

外部 runner 随异常直接离开 `command_batch`，没有重写本轮 summary；当时
`.batch_start` 为 01:09，而 `summary.json` mtime 仍为上一批 22:33。监控汇报的
`5/8` 因而混合了“5 个 Graph success、1 个 Judge success/Graph failed、1 个
startup infra error、1 个 not-run”，不能解释为三道业务失败。

## 五层归属

| 编号 | 主责 | 根因 | 后果 |
|---|---|---|---|
| SWE-054 | L3 Model Invocation | HTTP 402/余额耗尽落入 `FailureUnknown`，没有 provider quota/billing 类型 | 外部资源故障无法与协议、模型或普通 unknown 分开，startup/trace 诊断含混 |
| SWE-055 | L4 Loop | policy 返回 `RecoveryRequestIntervene`，`Agent.handleFailure` 没有执行分支 | unknown 静默落入 `non_recoverable_error`，没有 durable L4→L5 intervention |
| SWE-056 | 外部 SWE runner | early process exit 只报 healthz；task exception 绕过 summary；旧结果仍留在固定目录 | 当前批次不完整却显示成普通 X/8，summary authority 陈旧 |

L1 Prompt、L2 Context 与 L5 Graph 没有本次根因。L5 正确传播 Acceptance failed，
final-report provider 调用再次 402 后也由确定性 fallback 完成。

## 正式修复

### L3：ProviderQuotaExhausted

- `agentgo.invocation-failure/v1` 增加 `provider_quota_exhausted`。
- HTTP 402 以及结构化 provider code `insufficient_quota`、
  `insufficient_balance`、`billing_hard_limit_reached`、`billing_not_active`
  统一映射为该类型；不从普通正文猜测恢复动作。
- quota 与 429 rate limit 分离：429 可在冻结 snapshot 上有界 retry；quota 是
  Run 外部资源变化，当前 Run 直接 blocked，不重试、不压缩 Context、不让 Graph
  recovery 假装能补充余额。

### L4：RecoveryAction 穷尽执行

- `RecoveryRequestIntervene` 从已结算 Turn 的 ProgressCheckpoint 生成 durable
  `LoopInterventionRequested(reason=unsafe_unknown)`，随后当前 Activation blocked；
  不再落入通用 failed。
- `RecoveryCancel` 通过带 cancel source 的 Store 原子迁移到 cancelled。
- quota 使用明确 `provider_quota_exhausted` reason code blocked；unknown 与 quota
  不再共用终态路径。

### Python runner：批次事务

- AgentGo 在 healthz 前退出时，runner 读取有界脱敏启动错误，输出
  `provider_quota_exhausted/provider_auth_failed/provider_permission_denied/`
  `startup_probe_failed/healthz_timeout` 等稳定 reason code，并保留 exit code/log ref。
- batch 级直连 provider 探针同样把 HTTP 402/401/403 映射为 typed
  infrastructure error；这些不可重试条件首击即停，不进入三次探针等待。
- batch 开始立即清空旧 summary authority；循环使用 `try/finally`，任何题目
  exception 都原子生成本轮 summary。
- summary 固定按 tasks.csv 生成全量行，并区分 `completed`、
  `completed_with_infrastructure_error`、`infrastructure_error`、`not_run`；历史
  result/judge 只要 mtime 早于 batch_start 就不会进入当前行。
- 输出分别报告 completed 分母、task correctness、architecture、infra error 与
  not-run，不再把 incomplete batch 显示成普通“5/8 完成率”。
- provider quota 在运行中出现时立即停止后续题，保留已经完成的 Judge/Graph事实，
  返回 harness/infrastructure exit code 1。

## 验证与关闭条件

仓库验证已覆盖 HTTP 402 typed failure、quota blocked 无重试、unknown durable
intervention、early-exit startup reason、异常 finally summary 与旧行不复用；
`go test ./...`、`go vet ./...`、`go build -o agentgo .`、`git diff --check`、
42 项 Python runner 测试均通过。真实二进制 fake-provider 继续证明单顶层 Graph
success，Worker/Acceptance 均跨 Observation，Ledger/trace model calls 为 37/37，
Check 与 final-report 完整收口。

外部关闭条件：补充 provider 额度后，由指定监控任务执行新 8 题；summary 必须
mtime 晚于 batch_start 且 8 行均来自当前 batch，`infrastructure_error=0`、
`not_run=0`、`invocation_failures.provider_quota_exhausted=0`。业务正确率与
architecture_ok 继续分别报告。

### 外部关闭证据

provider 恢复后的当前 summary mtime 为 `2026-08-25 12:02:06 +0800`，晚于
`.batch_start=2026-08-25 11:19:31 +0800`。8 行 `run_state=completed`，stale、
infrastructure_error、not_run 均为 0；`provider_quota_exhausted=0`，所有题目
Judge resolved、Graph outcome success、architecture/infrastructure 均通过。
新终端汇总明确输出 `batch_status complete completed 8/8 task_resolved 8/8
architecture_ok 8/8 infrastructure_error 0 not_run 0`。SWE-054～056 关闭。
