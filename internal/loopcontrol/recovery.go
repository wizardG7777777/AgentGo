// Package loopcontrol 实现 L4 Loop 的纯控制决策。
//
// 本包不调用模型、不构造 Context、不执行工具，也不修改 Graph。它只把跨层稳定
// failure facts 映射成恢复动作；具体预算和 checkpoint 在后续切片接入。
package loopcontrol

import "agentgo/internal/invocation"

// RecoveryAction 是 L4 对一次 Invocation failure 的封闭默认动作。
type RecoveryAction string

const (
	RecoveryRetrySameSnapshot RecoveryAction = "retry_same_snapshot"
	RecoveryRebuildContext    RecoveryAction = "rebuild_context"
	RecoveryStartNewAttempt   RecoveryAction = "start_new_attempt"
	RecoveryRequestIntervene  RecoveryAction = "request_intervention"
	RecoveryFail              RecoveryAction = "fail"
	RecoveryBlock             RecoveryAction = "block"
	RecoveryCancel            RecoveryAction = "cancel"
)

// RecoveryDecision 只表达策略结果，不执行副作用。
type RecoveryDecision struct {
	Action        RecoveryAction
	FailureKind   invocation.FailureKind
	ReuseSnapshot bool
	NewAttempt    bool
	ContextReason string
}

// DecideInvocationFailure 返回 framework v1 默认策略。未来 policy catalog 可以
// 选择更严格的预算，但不得把 cancel/auth/permission 改成无限 retry。
func DecideInvocationFailure(failure *invocation.Failure) RecoveryDecision {
	if failure == nil {
		return RecoveryDecision{Action: RecoveryFail, FailureKind: invocation.FailureUnknown}
	}
	decision := RecoveryDecision{FailureKind: failure.Kind}
	switch failure.Kind {
	case invocation.FailureActionContractRejected:
		decision.Action = RecoveryRetrySameSnapshot
		decision.ReuseSnapshot = true
	case invocation.FailureRequestTimeout, invocation.FailureTransport,
		invocation.FailureRateLimited, invocation.FailureProviderUnavailable:
		decision.Action = RecoveryRetrySameSnapshot
		decision.ReuseSnapshot = true
	case invocation.FailureContextWindowExceeded:
		decision.Action = RecoveryRebuildContext
		decision.NewAttempt = true
		decision.ContextReason = string(invocation.FailureContextWindowExceeded)
	case invocation.FailureOutputTruncated, invocation.FailureOutputLimitExceeded,
		invocation.FailureMalformedResponse:
		decision.Action = RecoveryStartNewAttempt
		decision.NewAttempt = true
	case invocation.FailureCallerCancelled:
		decision.Action = RecoveryCancel
	case invocation.FailureAttemptDeadline, invocation.FailureActivationDeadline:
		decision.Action = RecoveryBlock
	case invocation.FailureContextAssembly:
		decision.Action = RecoveryBlock
	case invocation.FailureProviderQuotaExhausted:
		// 余额/计费额度是 Run 外部资源。继续 retry、重建 Context 或让 Graph
		// recovery 改策略都不会恢复；冻结为 blocked，等待操作者补充资源后
		// 由新 Run 继续。
		decision.Action = RecoveryBlock
	case invocation.FailureAuth, invocation.FailurePermissionDenied,
		invocation.FailureModelUnavailable, invocation.FailureInvalidRequest,
		invocation.FailureContentFiltered, invocation.FailureProtocolIncompatible:
		decision.Action = RecoveryFail
	case invocation.FailureUnknown:
		decision.Action = RecoveryRequestIntervene
	default:
		decision.Action = RecoveryFail
	}
	return decision
}

// IsRetryPath 表示当前兼容 Agent 可以通过 RetryRollback 进入下一 Attempt。它不
// 表示预算无限；MaxRetries/ProgressCheckpoint 仍必须约束次数。
func (d RecoveryDecision) IsRetryPath() bool {
	switch d.Action {
	case RecoveryRetrySameSnapshot, RecoveryRebuildContext, RecoveryStartNewAttempt:
		return true
	default:
		return false
	}
}
