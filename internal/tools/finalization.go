package tools

// FinalizationNotifier 由 finalization tool 用来通知 agent
// "当前任务已经完成"。
//
// 实现通常是 *FinalizationHolder（在 agent 包定义），由 agent 在 OnTaskStart 时设置任务ID，
// finalization tool（如 report_done）在成功执行后调用 MarkTaskFinalized()。
type FinalizationNotifier interface {
	MarkTaskFinalized()
}
