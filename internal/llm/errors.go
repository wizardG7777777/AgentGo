package llm

import "fmt"

// ErrRecoverable 表示可恢复错误（429 限流、5xx 服务端错误），调用方应触发重试。
type ErrRecoverable struct {
	Err error
}

func (e *ErrRecoverable) Error() string { return e.Err.Error() }
func (e *ErrRecoverable) Unwrap() error { return e.Err }

// ErrUnrecoverable 表示不可恢复错误（401/403/404），任务应直接标记失败。
type ErrUnrecoverable struct {
	Err error
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
