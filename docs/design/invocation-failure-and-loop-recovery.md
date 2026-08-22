# Invocation Failure / Loop Recovery 契约

> 状态：Accepted Design，SWE-019/020/025/027 implementation complete / Responses single-task verified<br>
> 日期：2026-08-23<br>
> 归属：Model Invocation 基础层 + L4 Loop Engineering<br>
> 对应问题：SWE-014<br>
> 上位规范：[`五层工程架构规范`](five-layer-engineering-architecture.md)<br>
> 关联设计：[`Context Snapshot / Item Budget`](context-snapshot-item-budget.md)、
> [`Loop Progress Contract / Checkpoint / Deadline`](loop-progress-checkpoint-and-deadline.md)<br>
> 统一路线图：[`SWE 架构修复统一实施路线图`](swe-architecture-repair-roadmap.md)

## 0.1 2026-08-23 实施状态

SWE-019 修订已接入：`OutputBudget` 新增 tool-call count 与 arguments-total；
`ContextBinding` 携带由 Context v7 completion reserve/Replay v2 派生的动态预算，
L4 action reservation 再取剩余预算最小值。新 Run 的 Scheduler 不再使用
record-only Lease，而是冻结真实 registry ceiling，并按 draft-create/configure/
validate/commit/start/recovery/final-report phase 生成同源 advertise/dispatch
ToolRouter；每个 Scheduler 响应只允许一个阶段动作。`startup_probe=tool` 真实执行
Responses typed function-call + required nonce fixture；text-only、错误 call identity
或空参数 provider 不再被“ping 成功”误判兼容。

SWE-027 已把 Model Invocation 新主链切换为 OpenAI Responses typed output items：
message/reasoning/function_call 分型由服务端信封决定，正文不作工具语义识别；
partial argument delta 只计预算并与 final item 对账，完成 item 通过 L2 RequiredExact
carrier 原样 replay。Chat Completions 仅保留显式 compatibility protocol。

已进入生产主链：封闭 `InvocationFailure`/FailureKind/phase/origin/scope、Cause
保留、typed RecoveryDecision、request/caller/attempt deadline 区分、SSE 与非流式
绝对输出上限、partial response 不入 History/不 dispatch、同一次采样冻结的
ToolRouterSnapshot，以及 Invocation→Loop settlement/Trace 的结构化字段。相关
focused fixtures、包测试和全仓 compile-only 已通过。

生产调用已统一为 `ContextCompiler → Snapshot Put → ContextBinding → llm.Invoke`；
production source 不再直接调用 `Client.Chat`。canonical `InvocationFailure` 在
L4 决策中优先，兼容 `ErrRecoverable` 只在没有 canonical failure 的显式 legacy/
本地 Loop 路径生效，`isContextOverflow` 与字符串分类已退出生产控制流。

Proposal Acceptance 已改成 exact typed verdict tool，最终 wire 只要求
`verdict`，非 pass 再要求有界 `issue_code/message`；framework 生成稳定 ref。
仓库 full/race/vet/build、真实二进制和最新 provider 单题双门已通过。仍开放
多 provider structured code/capability fixture、兼容 wrapper 删除、三平台与8题
rollout；故外部 closure 仍保持 validation-open。

## 1. 决策摘要

本设计建立 Model Invocation 到 L4 Loop 的稳定失败事实契约，禁止 Loop 从自由
文本猜测错误类型，并将“发生了什么”与“接下来是否重试”分开。

本设计固定以下决策：

1. Model Invocation 返回封闭、版本化的 `InvocationFailure`；L4 不解析
   `error.Error()`、provider message 或日志文本决定控制流。
2. `recoverable/unrecoverable` 不再是基础层的主要错误类型。它们是 L4 在具体
   Run/Attempt budget 下产生的恢复决策，而不是传输事实。
3. FailureKind 至少区分 request timeout、caller cancellation、Activation
   deadline、transport failure、rate limit、provider unavailable、context-window
   exceeded、output truncated、local output limit、malformed response、content
   filtered、auth、invalid request 和 unknown。
4. timeout 作用域用不同 context/cancel cause 和绝对 deadline 区分，不能因为
   Go 错误文本都含 `context deadline exceeded` 就合并。
5. provider 错误分类优先使用结构化 HTTP status、error code、finish reason 和
   adapter contract；未知自由文本不升级为 context-window overflow。
6. LLM→Agent 桥接保留同一个 `InvocationFailure` 和 Cause 链，不再重新包装成
   只有 `ErrRecoverable{Err}` 的第二套 Agent 错误。
7. request timeout/rate limit/provider unavailable 默认重试同一个冻结
   ContextSnapshot；使用新 InvocationID，但不压缩 History、不改变 Prompt/Lease。
8. 真正 context-window exceeded 才由 L4 请求 L2 生成带 parent ref 的恢复
   ContextSnapshot；改变 Context policy 时创建新 Attempt。
9. `finish_reason=length`/output truncated 不等于 input context overflow，不能通过
   压缩输入历史处理。
10. malformed response、tool arguments 损坏、本地流式 output limit 分别使用
    独立 policy，并受 SWE-011 ProgressCheckpoint/Attempt budget 约束。
11. caller cancellation 和权威上级取消优先于 retry/no-progress；停止后不得被
    普通可恢复错误覆盖为 failed。
12. Trace 保留 FailureKind、phase、scope、provider code、HTTP status、snapshot
    和 policy identity；原始 message 只作脱敏诊断，不参与决策。

## 2. 事故与根因

第五轮中，多个 Scheduler Invocation 在约 300 秒结束并返回：

```text
context deadline exceeded
```

该时长与配置的单次 LLM timeout 一致，属于 request deadline。当前调用链却是：

```text
SDK 非 API error
  → llm.ErrRecoverable
  → agent.ErrRecoverable
  → isContextOverflow(err.Error())
  → strings.Contains("context")
  → 误判 context-window overflow
  → snip/compress History
  → RetryRollback
```

结果是传输超时触发了错误的 L2 恢复动作，丢弃仍有价值的 Scheduler 历史，却
没有修复真正的请求 deadline。

技术上 Go error unwrap 链仍存在，但当前类型只表达“可否重试”，没有表达失败
事实的稳定 kind；L4 最终只能回退到字符串猜测。这是跨层契约缺失。

## 3. 分层责任

| 边界 | 拥有 | 不拥有 |
|---|---|---|
| Model Invocation | SDK/HTTP/SSE/finish reason/provider code 分类，Failure 事实 | Task 是否重试、History 压缩 |
| L2 Context | context-window 恢复 Snapshot 和摘要/ref policy | 判断 transport timeout |
| L3 Harness | client binding、Store、执行环境和 Observation 持久化 | Retry policy |
| **L4 Loop** | 恢复决策、预算、Attempt、cancel、deadline、终态 | SDK/provider 文本解析 |
| L5 Graph | 消费 TaskOutcome/blocked/failed，决定 replan/change | 单次 Invocation retry |

## 4. InvocationFailure

### 4.1 类型

```go
type InvocationFailure struct {
    Schema          string
    Kind            FailureKind
    Phase           InvocationPhase
    Origin          FailureOrigin
    TimeoutScope    TimeoutScope
    ProviderCode    string
    HTTPStatus      int
    FinishReason    FinishReason
    RetryAfter      time.Duration
    SnapshotID      string
    InvocationID    string
    ProviderPolicy  string
    UsageState      UsageState
    Partial         bool
    Cause           error
}
```

`Error()` 只用于展示；决策字段必须可直接读取。`Unwrap()` 保留 Cause，供
`errors.Is/As` 和诊断使用。

### 4.2 FailureKind

```text
request_timeout
caller_cancelled
activation_deadline
transport_failure
rate_limited
provider_unavailable
context_window_exceeded
output_truncated
output_limit_exceeded
malformed_response
content_filtered
auth_failure
permission_denied
model_unavailable
invalid_request
protocol_incompatible
unknown
```

未知 kind 在 schema 演进中 fail-closed；不得降级成 retry forever。

### 4.3 InvocationPhase

```text
request_build
request_encode
connect
request_send
response_headers
stream_receive
stream_accumulate
response_decode
response_validate
tool_call_validate
usage_settle
```

Phase 帮助 L4/Trace 判断是否存在 partial、是否可能产生 provider 侧费用和是否可
复用相同 Snapshot，但不直接决定重试。

### 4.4 TimeoutScope

```text
none
invocation_request
attempt
activation
graph
run
caller
```

## 5. timeout 作用域

### 5.1 context 结构

L4 持有 Activation/Attempt context，Model Invocation 只在其下派生单次 request
context：

```text
Run context
  → Graph context
    → Activation context
      → Attempt context
        → Invocation request context
```

每层使用绝对 deadline 和明确 cancel cause。单次请求建议使用
`context.WithTimeoutCause` 或等价机制写入 `ErrInvocationDeadline`。

### 5.2 分类顺序

Invocation 返回错误时按以下顺序：

1. 检查权威父 context/cancel registry/store cancellation；
2. 检查 request child context 的 cancel cause；
3. 检查结构化 provider/API error；
4. 检查 transport/net error；
5. 检查 stream/decode/validation failure；
6. 无法分类则 unknown。

父 context cancelled 时不能被子 request timeout 覆盖。Invocation 执行中发生取消，
返回后 L4 必须再次检查权威取消状态，再决定 Task terminal trace。

## 6. provider 错误规范化

### 6.1 结构化来源优先

分类依据优先级：

```text
context cause
  > provider structured error code
  > HTTP status
  > finish_reason
  > transport error type
  > decoder/validator typed error
  > unknown
```

provider message 只进入脱敏 diagnostics。仅当 adapter 对某 provider/version 有
fixture 验证且结构化字段缺失时，才能使用窄、版本化兼容规则；规则仍输出稳定
FailureKind，不能散落在 L4。

### 6.2 典型映射

| 原始事实 | FailureKind |
|---|---|
| request child deadline cause | request_timeout |
| parent/caller cancel | caller_cancelled 或对应上层 scope |
| HTTP 429 | rate_limited |
| HTTP 502/503/504 | provider_unavailable |
| provider code `context_length_exceeded` | context_window_exceeded |
| `finish_reason=length` | output_truncated |
| 本地 SSE output cap | output_limit_exceeded |
| tool args JSON 无法解析 | malformed_response |
| 401/invalid key | auth_failure |
| provider 不支持所需 replay/strict contract | protocol_incompatible |

HTTP 400 不能一律等同 context overflow；必须看结构化 code 或 adapter contract。

## 7. L4 RecoveryPolicy

### 7.1 决策输出

```go
type RecoveryDecision struct {
    Action           RecoveryAction
    FailureKind      FailureKind
    PolicyID         string
    Charge           BudgetUsage
    Backoff          time.Duration
    ReuseSnapshot    bool
    NewAttempt       bool
    ContextReason    string
    TerminalReason   string
}
```

`RecoveryAction`：

```text
retry_same_snapshot
rebuild_context
start_new_attempt
wait_backoff
request_intervention
fail
block
cancel
```

### 7.2 默认矩阵

| FailureKind | 默认动作 | Context 行为 |
|---|---|---|
| request_timeout | 有界 retry_same_snapshot | 不改 History/Context |
| transport_failure | 有界 retry_same_snapshot | 不改 |
| rate_limited | RetryAfter/backoff 后重试 | 不改 |
| provider_unavailable | 有界重试或介入 | 不改 |
| context_window_exceeded | rebuild_context/new Attempt | L2 恢复 Snapshot |
| output_truncated | output recovery/new Attempt | 不按输入溢出压缩 |
| output_limit_exceeded | policy rollover/blocked | partial 不入 History |
| malformed_response | protocol recovery/new Attempt | 默认不压缩 Context |
| caller_cancelled | cancel | 不重试 |
| activation/graph/run deadline | blocked/failed/cancel 按 authority | 不重试 |
| auth/permission/invalid request | fail-fast | 不重试 |
| protocol_incompatible | block/change provider | 不猜测降级 |
| unknown | 保守有限重试后介入/blocked | 禁止自动压缩 |

矩阵由 framework policy catalog 版本化；Scheduler 不能修改为无限重试。

## 8. Snapshot、Attempt 与 Retry

### 8.1 transport retry

request timeout/transport/rate limit 的重试：

- 同 AttemptID；
- 同 PromptBuildRef；
- 同 ContextSnapshotID/digest；
- 同 ExecutionLease；
- 新 InvocationID；
- 不追加失败 partial assistant Turn；
- retry usage/time 计入 SWE-011 budget。

### 8.2 Context recovery

真正 context-window overflow：

```text
InvocationFailure(context_window_exceeded)
  → L4 RecoveryDecision(rebuild_context)
  → L2 ContextRecoveryRequest
  → ContextSnapshot(parent_snapshot_ref, recovery_reason)
```

如果 recovery 改变 ContextPolicy、PromptBuild 或 provider replay contract，必须新建
Attempt。L2 保留原始 Turn/Result refs，只改变选择/转换视图，不破坏审计历史。

### 8.3 malformed/output recovery

malformed response 与 output limit 不能自动解释为 Context 太长。恢复可以：

- 同 Snapshot 做一次协议重试；
- 新 Attempt 启用已验证 strict tool schema；
- 更换 provider/model capability；
- 请求 SWE-011 intervention；
- blocked。

不得把错误内容原样写回 History 作为下一轮修复提示的大正文。

## 9. 与 Context/Progress 设计的接口

### 9.1 Context Snapshot

每个 Failure 绑定 SnapshotID 和 ContextPolicyDigest。L2 可据此判断：

- 是输入预算问题还是输出问题；
- 哪个 Fragment/AtomicGroup 需要转换；
- provider replay 是否允许 rebuild；
- parent Snapshot 和 Manifest 如何记录。

### 9.2 TurnSettlementDelta

Invocation success/failure、usage 和 partial 状态进入
`TurnSettlementDelta`，由 L4 ProgressEvaluator/Checkpoint 计费。传输失败不是
业务进展，但会消耗 Attempt 时间/调用预算。

重复相同 FailureKind/phase/snapshot fingerprint 不能无限重置 retry/no-progress
预算。

## 10. Trace 与诊断

`llm_call_end` 或其替代事件至少投影：

```text
failure_kind
failure_phase
failure_origin
timeout_scope
provider_code
http_status
finish_reason
usage_state
partial
snapshot_id
invocation_id
provider_policy_id
```

L4 另投影：

```text
recovery_action
recovery_policy_id
reuse_snapshot
new_attempt
budget_charge
backoff
terminal_reason
```

Trace 中保留的 Error 文本只供脱敏排障；CLI 查询以 typed 字段聚合，不用 regex
重新分类历史事件。

## 11. 包边界与端口

建议将稳定 DTO 放在不依赖 SDK 的边界包：

```text
internal/invocation
  InvocationContract / InvocationFailure / FailureKind / UsageState

internal/llm
  OpenAI-compatible SDK adapter / SSE / provider normalization

internal/loop
  RecoveryPolicy / RecoveryDecision / Attempt state

internal/contextcompiler
  ContextRecoveryRequest / ContextSnapshot

internal/agent
  迁移 adapter，最终不再定义第二套 ErrRecoverable
```

建议端口：

```go
type ModelInvoker interface {
    Invoke(ctx context.Context, snapshot ContextSnapshot,
        contract InvocationContract) (InvocationResult, *InvocationFailure)
}

type RecoveryPlanner interface {
    Decide(contract LoopContract, checkpoint ProgressCheckpoint,
        failure InvocationFailure) (RecoveryDecision, error)
}
```

Failure 和 Decision 必须可序列化为有界 durable record；Cause 不直接序列化，只
保存脱敏 diagnostics 和稳定 kind/code。

## 12. 迁移切片

### Slice 0：特征化

- 固定 `context deadline exceeded` 误压缩事故；
- 固定 429/5xx/auth/context code/finish reason/malformed fixtures；
- 固定 caller cancellation after blocking Execute；
- 证明当前 agent bridge 只按 recoverable 二分。

### Slice 1：Invocation contract types

- 新增 FailureKind/Phase/Scope/UsageState；
- 保持 SDK adapter 旧接口兼容；
- Trace shadow 输出 typed classification；
- 对比旧/新分类，不改变控制流。

### Slice 2：timeout cause 与 provider normalization

- 分层 context/cancel cause；
- 结构化 API/HTTP/finish reason 映射；
- provider adapter fixture；
- unknown fail-closed。

### Slice 3：删除字符串分类

- L4 RecoveryPolicy 接管；
- 删除 `isContextOverflow` 控制流；
- 删除/退役 agent.ErrRecoverable 第二套包装；
- retry reason 保存 stable kind + bounded diagnostics。

### Slice 4：Context/Attempt recovery

- request timeout 复用 Snapshot；
- context-window overflow 请求 L2 rebuild；
- output/malformed 使用独立 policy；
- 接入 ProgressCheckpoint budget。

### Slice 5：E2E 与 legacy 退出

- httptest/SSE/provider error fixtures；
- 真实二进制启动；
- 断言 Trace/Checkpoint/History/Task terminal；
- SWE 单题对照；
- 无调用方后删除旧 ErrRecoverable/文本 matcher。

## 13. 验证矩阵

| 输入 | FailureKind | L4 默认 | 不变量 |
|---|---|---|---|
| `context.DeadlineExceeded`，request cause | request_timeout | same snapshot retry | 不压缩 |
| parent caller cancelled | caller_cancelled | cancel | 不重试 |
| Activation hard deadline | activation_deadline | blocked/terminal | 不续命 |
| provider context code | context_window_exceeded | L2 rebuild | 新 Snapshot |
| `finish_reason=length` | output_truncated | output recovery | 不当输入溢出 |
| local SSE cap | output_limit_exceeded | rollover/blocked | partial 不入 History |
| tool args JSON 损坏 | malformed_response | protocol policy | 不 dispatch |
| HTTP 429 | rate_limited | RetryAfter | 有界 |
| HTTP 503 | provider_unavailable | backoff | 有界 |
| HTTP 401 | auth_failure | fail-fast | RetryCount 不增 |
| message 含 `context`、无 code | unknown/原结构类 | 不自动压缩 | 文本不驱动 |

恢复测试还必须覆盖：

- wrapped Cause 的 `errors.Is/As`；
- stream partial + cancel race；
- caller cancel 与 request timeout 同时发生时 authority 优先级；
- retry 同 Snapshot/new Invocation identity；
- Context rebuild parent ref；
- ProgressCheckpoint 不因 retry 清零；
- Trace 不泄露 key、完整 response body 或敏感 request。

## 14. 完成定义

只有以下全部满足，SWE-014 才可标记 fixed：

1. Model Invocation 返回封闭、版本化 InvocationFailure；
2. L4 不再以字符串匹配分类任何 Invocation failure；
3. request/caller/Activation/Graph/Run timeout scope 可机械区分；
4. provider structured code/HTTP/finish reason/transport/decoder 分类稳定；
5. request timeout 不触发 Context 压缩；
6. context-window exceeded 只经 L2 Context recovery；
7. output truncated/local output limit/malformed response 使用独立 policy；
8. transport retry 复用同 Snapshot 且不写 partial History；
9. caller cancellation 不被普通失败覆盖；
10. retry/no-progress/deadline budget 与 SWE-011 checkpoint 对账；
11. Trace 保留 stable kind、phase、scope、policy 和 identity；
12. 单测、SSE fixtures、恢复测试、race、全量测试、真实二进制与 SWE 回归通过；
13. 旧 ErrRecoverable 二分和 `isContextOverflow` 控制流已退出生产路径。
