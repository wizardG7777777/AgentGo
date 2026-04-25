package runner

import "sync"

// CurrentTaskHolder 线程安全地保存 Runner 当前正在执行的任务 ID。
// 实现 tools.TaskHolder 接口（Get），供 MetaGroup.publish_task 工具
// 在发布子任务时定位父任务、检查深度限制。
//
// 取自 internal/worker.currentTaskHolder + internal/explorer.currentTaskHolder
// 的合并版（v4 §11.6.6：两份重复的 holder 合并到一份）。两份原版除了所属包不同
// 外完全一致，无功能差异。
type CurrentTaskHolder struct {
	mu sync.Mutex
	id string
}

// Set 设置当前任务 ID（由 Agent.OnTaskStart 调用）。
func (h *CurrentTaskHolder) Set(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.id = id
}

// Get 返回当前任务 ID（由 publish_task 工具读取）。空串表示空闲。
func (h *CurrentTaskHolder) Get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}
