package llm

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
