package trace

// ReadEvents 读取一个 trace JSONL 文件的全部事件，是 readAllEvents 的导出封装：
// 即使中途读取失败也返回已完整读到的事件，与 trace CLI 的 partial timeline
// 语义保持一致。供进程外的评测驱动器（internal/eval）收割运行事实使用。
func ReadEvents(path string) ([]Event, error) {
	return readAllEvents(path)
}
