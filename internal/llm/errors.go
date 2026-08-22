package llm

import (
	"fmt"

	"agentgo/internal/invocation"
)

// ErrRecoverable 是外部兼容包装；internal L4 只消费 Failure.Kind 决定恢复，
// 不得仅因该 Go 类型存在就触发重试。
type ErrRecoverable struct {
	Err     error
	Code    string // 厂商错误码（如 rate_limit_exceeded）
	Message string // 厂商错误消息
	Failure *invocation.Failure
}

func (e *ErrRecoverable) Error() string { return e.Err.Error() }
func (e *ErrRecoverable) Unwrap() error { return e.Err }
func (e *ErrRecoverable) InvocationFailure() *invocation.Failure {
	if e == nil {
		return nil
	}
	return e.Failure
}

// ErrUnrecoverable 是外部兼容包装；internal L4 仍以 Failure.Kind 为唯一事实。
type ErrUnrecoverable struct {
	Err        error
	StatusCode int    // HTTP 状态码
	Code       string // 厂商错误码（如 model_not_found）
	Message    string // 厂商错误消息
	Endpoint   string // LLM endpoint（如 https://api.deepseek.com/v1），用于诊断提示
	Failure    *invocation.Failure
}

func (e *ErrUnrecoverable) Error() string { return e.Err.Error() }
func (e *ErrUnrecoverable) Unwrap() error { return e.Err }
func (e *ErrUnrecoverable) InvocationFailure() *invocation.Failure {
	if e == nil {
		return nil
	}
	return e.Failure
}

// ErrBadResponse 表示 LLM 返回了无法解析的响应（JSON 畸形、参数解析失败等），
// 调用方应触发简单重试。
type ErrBadResponse struct {
	Err     error
	Failure *invocation.Failure
}

func (e *ErrBadResponse) Error() string { return fmt.Sprintf("bad LLM response: %v", e.Err) }
func (e *ErrBadResponse) Unwrap() error { return e.Err }
func (e *ErrBadResponse) InvocationFailure() *invocation.Failure {
	if e == nil {
		return nil
	}
	return e.Failure
}

// ErrUnknownRole 表示消息中出现了未知的 role 值。
type ErrUnknownRole struct {
	Role string
}

func (e *ErrUnknownRole) Error() string { return fmt.Sprintf("unknown message role: %q", e.Role) }
