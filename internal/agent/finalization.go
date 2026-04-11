package agent

import "sync"

// FinalizationChecker 是 TaskExecutor 的可选接口。
// agent.Run 在每轮 reactLoop 之前检查 IsFinalized()，
// 返回 true 时立即走"任务完成"路径（等价于 LLM 返回了 !ToolCalled）。
type FinalizationChecker interface {
	IsFinalized() bool
}

// FinalizationHolder 是一个通用的任务完成状态持有者，
// 同时实现 tools.FinalizationNotifier（通过适配）和 FinalizationChecker。
//
// 用法：
//   - agent 在 OnTaskStart(taskID) 时调用 holder.Set(taskID)
//   - finalization tool 成功执行后调用 holder.MarkTaskFinalized()
//   - agent.Run reactLoop 检查 holder.IsFinalized() 或 result.Finalized
//   - agent 在 OnTaskEnd 时调用 holder.Set("") 清空状态
type FinalizationHolder struct {
	mu        sync.RWMutex
	taskID    string
	finalized bool
}

// NewFinalizationHolder 创建一个新的 FinalizationHolder。
func NewFinalizationHolder() *FinalizationHolder {
	return &FinalizationHolder{}
}

// Set 设置当前任务ID，同时清空 finalized 标志。
// 应在 OnTaskStart 时调用。
func (h *FinalizationHolder) Set(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.taskID = taskID
	h.finalized = false
}

// Get 返回当前任务ID。
func (h *FinalizationHolder) Get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.taskID
}

// MarkTaskFinalized 实现 tools.FinalizationNotifier 接口。
func (h *FinalizationHolder) MarkTaskFinalized() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finalized = true
}

// IsFinalized 实现 FinalizationChecker 接口。
func (h *FinalizationHolder) IsFinalized() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.finalized
}
