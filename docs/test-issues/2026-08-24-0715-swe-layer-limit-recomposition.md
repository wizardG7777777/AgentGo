# SWE 五层限制重组：SWE-041～044

> 日期：2026-08-24
> 状态：closed / implementation-complete / local-and-external-validation-complete

## 事故事实

最新 DeepSeek 8 题批测为 `task_resolved=1/8`、`architecture_ok=8/8`。七道失败题
全部 `patch_lines=0` 且到达 `exploration=5`；唯一成功题在 exploration=2 时已
编辑并验证。六条 recovery 路径累计产生 14 次 RecoveryDelta 参数拒绝，另有
一题在零改动、零测试时得到 Graph success，最终被 Judge 判失败。

| 编号 | 层级 | 根因 | 正式修复 |
|---|---|---|---|
| SWE-041 | L2/L4 | 新 Evidence 被承认为 knowledge progress，却在第 5 轮触发固定 forced delivery | business contract 删除 exploration 终态上限；每 8 个知识 turn Observation 后继续 |
| SWE-042 | L3/L5 | RecoveryDelta 的数组/枚举/source authority 未完整进入模型工具 schema | `submit_recovery_decision` 强类型 schema；source 字段由 Runtime 自动绑定 |
| SWE-043 | L3/L4/L5 | required effect/check 只验证声明，未验证终态兑现 | `run_check`、workspace revision、TaskOutcome v2 fulfillment 与 L5 二次门 |
| SWE-044 | L2/L4/基础层 | 所有模型统一冻结 128K/32K，Run token profile 默认硬停止 | Context v9 由 1M/64K ModelCapability 派生；v2 Run profile token observe-only |

## 新主链不变量

- 有效新证据不因固定轮数终止业务 Agent；重复 digest、deadline、model/tool/
  Attempt 预算仍可介入。
- Observation checkpoint 是持久化接缝，不是终态。
- `run_shell` 的 exit=0 不是 verification authority；新 contract 只接受
  `agentgo.check-result/v1`。
- mutating completed 必须有非空 workspace revision，以及绑定同一 revision 的
  `verification` pass。
- recovery 模型不得自述 checkpoint/observation/fingerprint。
- token 默认只记账；成本限制必须来自用户显式预算，而非静默截断任务。

## Closure 条件

本地必须通过 full/race/vet/build/diff 与 fake-provider：12 个读取在第 8 轮
checkpoint 后继续、零改动提交被拒、edit→run_check→success、blocked→typed
recovery→work@2、700K 等价 Context 可编译。外部先跑 automatic-options，成功后
再跑 8 题；RecoveryDelta rejection、零 fulfillment success、固定 exploration
forced delivery 必须为零。未取得这些证据前 SWE-041～044 不关闭。

## 本地验证证据

- Python SWE runner：31 项通过；
- `go test ./...`、`go test -race ./...`、`go vet ./...`、build、diff-check 通过；
- 真实二进制 local Responses smoke：12 个唯一 evidence 读取，第 8 轮
  Observation 后继续，work@1 blocked → typed recovery → work@2，随后
  write_file → run_check → fulfillment → acceptance → final-report；最终
  `graph_outcome=success`、顶层 Graph=1、Observation=1、CheckRecord=1、
  final-report reads=2。

## 外部 closure 证据

- `automatic-options` 单题与后续批次均为 architecture/task 双通过；
- 最终 8 题 cohort 为 `architecture_ok=8/8` / `task_resolved=6/8`，
  RecoveryDelta rejection、零 fulfillment success、固定 exploration forced delivery、
  fragment/output/reasoning 事故均为零；
- 批次中两道普通模型业务失败 `pass-context-dispatch` 与
  `session-access-tracking` 随后定向复跑均为
  `architecture_ok=true / task_resolved=true`，Judge 分别 490/490 与
  490/490。不将定向复跑篡改为原批次 8/8。

外部证据已满足 closure 条件，SWE-041～044 关闭。SWE-045 的
Responses/Observation 后续修复与证据见独立记录。
