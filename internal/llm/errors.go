package llm

import "fmt"

// ErrRecoverable 表示可恢复错误（408/429/502/503/504），调用方应触发重试。
type ErrRecoverable struct {
	Err     error
	Code    string // 厂商错误码（如 rate_limit_exceeded）
	Message string // 厂商错误消息
}

func (e *ErrRecoverable) Error() string { return e.Err.Error() }
func (e *ErrRecoverable) Unwrap() error { return e.Err }

// ErrUnrecoverable 表示不可恢复错误（400/401/403/404/405/500），任务应直接标记失败。
type ErrUnrecoverable struct {
	Err        error
	StatusCode int    // HTTP 状态码
	Code       string // 厂商错误码（如 model_not_found）
	Message    string // 厂商错误消息
	Endpoint   string // LLM endpoint（如 https://api.deepseek.com/v1），用于诊断提示
}

func (e *ErrUnrecoverable) Error() string { return e.Err.Error() }
func (e *ErrUnrecoverable) Unwrap() error { return e.Err }

// ErrBadResponse 表示 LLM 返回了无法解析的响应（JSON 畸形、参数解析失败等），
// 调用方应触发简单重试。
type ErrBadResponse struct {
	Err error
}

func (e *ErrBadResponse) Error() string { return fmt.Sprintf("bad LLM response: %v", e.Err) }
func (e *ErrBadResponse) Unwrap() error { return e.Err }

// ErrUnknownRole 表示消息中出现了未知的 role 值。
type ErrUnknownRole struct {
	Role string
}

func (e *ErrUnknownRole) Error() string { return fmt.Sprintf("unknown message role: %q", e.Role) }
