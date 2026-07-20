package interaction

import "errors"

var (
	ErrInvalidRequest    = errors.New("interaction: 请求无效")
	ErrInvalidOption     = errors.New("interaction: 选项无效")
	ErrNotFound          = errors.New("interaction: 请求不存在")
	ErrDuplicateID       = errors.New("interaction: 请求 ID 已存在")
	ErrVersionConflict   = errors.New("interaction: 版本冲突")
	ErrAlreadyAnswered   = errors.New("interaction: 请求已由其他回答占用")
	ErrInvalidTransition = errors.New("interaction: 状态转换无效")
	ErrCancelled         = errors.New("interaction: 请求已取消")
	ErrExpired           = errors.New("interaction: 请求已过期")
	ErrFailed            = errors.New("interaction: 请求处理失败")
	ErrInterrupted       = errors.New("interaction: 请求已中断")
)
