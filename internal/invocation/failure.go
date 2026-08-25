// Package invocation 定义 Model Invocation 基础层与 L4 Loop 之间的稳定契约。
//
// 本包只描述一次模型调用发生了什么，不决定 Task 是否重试、压缩 Context 或
// 进入终态。恢复策略属于 L4，SDK/provider 适配属于 internal/llm。
package invocation

import (
	"errors"
	"fmt"
	"time"
)

// FailureKind 是一次 Invocation 失败的封闭事实类别。
type FailureKind string

const (
	FailureRequestTimeout     FailureKind = "request_timeout"
	FailureCallerCancelled    FailureKind = "caller_cancelled"
	FailureAttemptDeadline    FailureKind = "attempt_deadline"
	FailureActivationDeadline FailureKind = "activation_deadline"
	FailureTransport          FailureKind = "transport_failure"
	FailureRateLimited        FailureKind = "rate_limited"
	// FailureProviderQuotaExhausted 表示 provider 账户/项目的计费额度或余额
	// 已耗尽。它不同于瞬时 429 rate limit，也不同于认证失败；同一 Run 内
	// 重试或改 Context 都不会恢复，必须等待外部资源状态改变。
	FailureProviderQuotaExhausted FailureKind = "provider_quota_exhausted"
	FailureProviderUnavailable    FailureKind = "provider_unavailable"
	FailureContextWindowExceeded  FailureKind = "context_window_exceeded"
	FailureContextAssembly        FailureKind = "context_assembly_rejected"
	FailureOutputTruncated        FailureKind = "output_truncated"
	FailureOutputLimitExceeded    FailureKind = "output_limit_exceeded"
	FailureMalformedResponse      FailureKind = "malformed_response"
	// FailureActionContractRejected 表示 response 结构合法，但 ToolRouter/L3
	// 阶段契约拒绝了动作。它不能混入 provider malformed_response。
	FailureActionContractRejected FailureKind = "action_contract_rejected"
	FailureContentFiltered        FailureKind = "content_filtered"
	FailureAuth                   FailureKind = "auth_failure"
	FailurePermissionDenied       FailureKind = "permission_denied"
	FailureModelUnavailable       FailureKind = "model_unavailable"
	FailureInvalidRequest         FailureKind = "invalid_request"
	FailureProtocolIncompatible   FailureKind = "protocol_incompatible"
	FailureUnknown                FailureKind = "unknown"
)

// Phase 标识失败发生在 Invocation 的哪个阶段。
type Phase string

const (
	PhaseRequestBuild     Phase = "request_build"
	PhaseRequestEncode    Phase = "request_encode"
	PhaseConnect          Phase = "connect"
	PhaseRequestSend      Phase = "request_send"
	PhaseResponseHeaders  Phase = "response_headers"
	PhaseStreamReceive    Phase = "stream_receive"
	PhaseStreamAccumulate Phase = "stream_accumulate"
	PhaseResponseDecode   Phase = "response_decode"
	PhaseResponseValidate Phase = "response_validate"
	PhaseToolCallValidate Phase = "tool_call_validate"
	PhaseUsageSettle      Phase = "usage_settle"
)

// Origin 标识失败事实最初由哪个边界产生。
type Origin string

const (
	OriginCaller    Origin = "caller"
	OriginRuntime   Origin = "runtime"
	OriginTransport Origin = "transport"
	OriginProvider  Origin = "provider"
	OriginProtocol  Origin = "protocol"
)

// TimeoutScope 标识 deadline/cancel 的权威作用域。
type TimeoutScope string

const (
	TimeoutNone       TimeoutScope = "none"
	TimeoutInvocation TimeoutScope = "invocation_request"
	TimeoutAttempt    TimeoutScope = "attempt"
	TimeoutActivation TimeoutScope = "activation"
	TimeoutGraph      TimeoutScope = "graph"
	TimeoutRun        TimeoutScope = "run"
	TimeoutCaller     TimeoutScope = "caller"
)

// UsageState 描述失败调用的 usage 是否已经可靠结算。
type UsageState string

const (
	UsageUnknown UsageState = "unknown"
	UsagePartial UsageState = "partial"
	UsageSettled UsageState = "settled"
)

// Failure 是跨层稳定的 Invocation 失败事实。
//
// Cause 只供进程内 errors.Is/As 与诊断使用；持久化层应记录其余有界字段和脱敏
// message，不直接序列化任意 provider/SDK 错误对象。
type Failure struct {
	Schema         string        `json:"schema"`
	Kind           FailureKind   `json:"kind"`
	Phase          Phase         `json:"phase"`
	Origin         Origin        `json:"origin"`
	TimeoutScope   TimeoutScope  `json:"timeout_scope,omitempty"`
	ProviderCode   string        `json:"provider_code,omitempty"`
	HTTPStatus     int           `json:"http_status,omitempty"`
	FinishReason   string        `json:"finish_reason,omitempty"`
	RetryAfter     time.Duration `json:"retry_after,omitempty"`
	SnapshotID     string        `json:"snapshot_id,omitempty"`
	InvocationID   string        `json:"invocation_id,omitempty"`
	ProviderPolicy string        `json:"provider_policy,omitempty"`
	UsageState     UsageState    `json:"usage_state"`
	Partial        bool          `json:"partial,omitempty"`
	Cause          error         `json:"-"`
}

const FailureSchemaV1 = "agentgo.invocation-failure/v1"

var (
	// ErrAttemptDeadline 等 cause 由 L4 创建子 context 时使用，使 SDK 返回相同
	// context deadline 文本时仍可机械区分权威作用域。
	ErrAttemptDeadline    = errors.New("attempt deadline reached")
	ErrActivationDeadline = errors.New("activation deadline reached")
	ErrGraphDeadline      = errors.New("graph deadline reached")
	ErrRunDeadline        = errors.New("run deadline reached")
)

// NewFailure 创建带默认 schema/unknown usage 的失败事实。
func NewFailure(kind FailureKind, phase Phase, origin Origin, cause error) *Failure {
	return &Failure{
		Schema:       FailureSchemaV1,
		Kind:         kind,
		Phase:        phase,
		Origin:       origin,
		TimeoutScope: TimeoutNone,
		UsageState:   UsageUnknown,
		Cause:        cause,
	}
}

func (f *Failure) Error() string {
	if f == nil {
		return "invocation failure"
	}
	if f.Cause != nil {
		return f.Cause.Error()
	}
	return fmt.Sprintf("invocation failure: kind=%s phase=%s", f.Kind, f.Phase)
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

// Validate 在 Failure 进入 durable DTO/Trace 前校验封闭词表。Cause 可以为空，
// 因为恢复路径只持久化结构化字段和脱敏诊断。
func (f *Failure) Validate() error {
	if f == nil {
		return errors.New("InvocationFailure 为空")
	}
	if f.Schema != FailureSchemaV1 {
		return fmt.Errorf("InvocationFailure schema=%q，无效", f.Schema)
	}
	if !validFailureKind(f.Kind) {
		return fmt.Errorf("InvocationFailure kind=%q，无效", f.Kind)
	}
	if !validPhase(f.Phase) {
		return fmt.Errorf("InvocationFailure phase=%q，无效", f.Phase)
	}
	if !validOrigin(f.Origin) {
		return fmt.Errorf("InvocationFailure origin=%q，无效", f.Origin)
	}
	if f.TimeoutScope != "" && !validTimeoutScope(f.TimeoutScope) {
		return fmt.Errorf("InvocationFailure timeout_scope=%q，无效", f.TimeoutScope)
	}
	if !validUsageState(f.UsageState) {
		return fmt.Errorf("InvocationFailure usage_state=%q，无效", f.UsageState)
	}
	if f.HTTPStatus < 0 {
		return fmt.Errorf("InvocationFailure http_status=%d，无效", f.HTTPStatus)
	}
	return nil
}

func validFailureKind(kind FailureKind) bool {
	switch kind {
	case FailureRequestTimeout, FailureCallerCancelled, FailureAttemptDeadline, FailureActivationDeadline,
		FailureTransport, FailureRateLimited, FailureProviderQuotaExhausted, FailureProviderUnavailable,
		FailureContextWindowExceeded, FailureContextAssembly, FailureOutputTruncated, FailureOutputLimitExceeded,
		FailureMalformedResponse, FailureActionContractRejected, FailureContentFiltered, FailureAuth,
		FailurePermissionDenied, FailureModelUnavailable, FailureInvalidRequest,
		FailureProtocolIncompatible, FailureUnknown:
		return true
	default:
		return false
	}
}

func validPhase(phase Phase) bool {
	switch phase {
	case PhaseRequestBuild, PhaseRequestEncode, PhaseConnect, PhaseRequestSend,
		PhaseResponseHeaders, PhaseStreamReceive, PhaseStreamAccumulate,
		PhaseResponseDecode, PhaseResponseValidate, PhaseToolCallValidate,
		PhaseUsageSettle:
		return true
	default:
		return false
	}
}

func validOrigin(origin Origin) bool {
	switch origin {
	case OriginCaller, OriginRuntime, OriginTransport, OriginProvider, OriginProtocol:
		return true
	default:
		return false
	}
}

func validTimeoutScope(scope TimeoutScope) bool {
	switch scope {
	case TimeoutNone, TimeoutInvocation, TimeoutAttempt, TimeoutActivation,
		TimeoutGraph, TimeoutRun, TimeoutCaller:
		return true
	default:
		return false
	}
}

func validUsageState(state UsageState) bool {
	switch state {
	case UsageUnknown, UsagePartial, UsageSettled:
		return true
	default:
		return false
	}
}

// FailureCarrier 由兼容错误包装实现，使迁移期间 L4 仍能提取 canonical Failure。
type FailureCarrier interface {
	InvocationFailure() *Failure
}

// FromError 沿 error chain 查找 canonical Failure。它不解析错误文本。
func FromError(err error) (*Failure, bool) {
	if err == nil {
		return nil, false
	}
	var direct *Failure
	if errors.As(err, &direct) && direct != nil {
		return direct, true
	}
	var carrier FailureCarrier
	if errors.As(err, &carrier) {
		if failure := carrier.InvocationFailure(); failure != nil {
			return failure, true
		}
	}
	return nil, false
}

// IsContextWindowExceeded 是 L2 Context recovery 的唯一基础层判定入口。
func IsContextWindowExceeded(err error) bool {
	failure, ok := FromError(err)
	return ok && failure.Kind == FailureContextWindowExceeded
}
