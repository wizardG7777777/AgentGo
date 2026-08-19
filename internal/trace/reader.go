package trace

// ReadEvents 读取一个 trace JSONL 文件的全部事件，是 readAllEvents 的导出封装：
// 即使中途读取失败也返回已完整读到的事件，与 trace CLI 的 partial timeline
// 语义保持一致。供需要读取持久化 trace 的集成测试与诊断代码复用。
func ReadEvents(path string) ([]Event, error) {
	return readAllEvents(path)
}
