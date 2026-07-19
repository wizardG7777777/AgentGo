package store

import "agentgo/internal/model"

// BusyAgentTasks 从任务快照推导 agentID → 当前 processing 任务的映射。
// 统一 scheduler board 快照与 UI AgentInfoFn 的"谁忙"口径（D3）；调用方按
// EventType 等维度自行过滤（两处口径差异：board 只统计默认队列）。
//
// 语义：
//   - 仅统计 Status == processing 的任务；nil 任务条目安全跳过；
//   - 同一 agent 出现在多个 processing 任务的 Agents 列表时（并发认领），
//     保留先见到（first-seen）的那个任务——ScanAll 已按 CreatedAt（+ID 兜底）
//     确定序排序（D1），因此 first-seen 是确定性的；
//   - 返回的 map 按 agent 去重：一个 agent 无论认领几个任务只占一个键。
func BusyAgentTasks(tasks []*model.Task) map[string]*model.Task {
	busy := make(map[string]*model.Task)
	for _, t := range tasks {
		if t == nil || t.Status != model.TaskStatusProcessing {
			continue
		}
		for _, agentID := range t.Agents {
			if _, exists := busy[agentID]; !exists {
				busy[agentID] = t
			}
		}
	}
	return busy
}
