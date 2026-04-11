package scheduler

import "sync"

// SpecializedAgent 是一条"特化代理"的静态声明。
//
// AgentGo 把代理分为两类：
//
//   - **通用 worker**：默认 event_type=""，拥有完整工具集（write/edit/shell/
//     publish/web/...），是兜底执行器。本 registry **不**记录通用 worker，
//     因为它们对 scheduler 是透明的——任何默认任务都归它们处理。
//
//   - **特化代理**：声明了特定 event_type（如 Explorer 用 "explore"），通常
//     工具集受裁剪、有明确的能力边界。scheduler 需要知道这类代理的存在，
//     才能把合适的任务路由给它们（例如把"只读调查"类任务发布为
//     event_type="explore" 让 Explorer 认领，而不是让通用 worker 执行）。
//
// 本 registry 是静态注册表——所有条目在 bootstrap 阶段一次性注入，运行期
// 只读。count 也是静态的（当前 AgentGo 架构里特化代理数量在启动时就固定了，
// Explorer 永远一个实例）。未来如果需要动态注册，可以加锁变成读写分离。
type SpecializedAgent struct {
	// EventType 是该代理认领任务时匹配的 EventType 值。
	// 例如 Explorer 用 "explore"——意味着只有 publish_task 时 event_type
	// 显式设置为 "explore" 的任务才会被 Explorer 认领。
	EventType string

	// Count 是该类代理的实例数量（静态）。
	// 当前 AgentGo 每个特化类型只有 1 个实例，未来可能扩展为多个。
	Count int

	// Role 是人类可读的一句话角色描述，供 scheduler prompt 提示 LLM。
	// 例如 "read-only investigator，只能读文件 / 搜索 / 访问网页，不能
	// 写文件、执行 shell 或发布子任务"。
	//
	// 这段文本会直接拼接进 scheduler system prompt 的路由指引段，所以应
	// 当简洁、动作导向、包含能力边界。
	Role string
}

// AgentRegistry 是特化代理的静态注册表，由 bootstrap 在启动时填充。
//
// 并发模型：写（Register）在 bootstrap 单 goroutine 期完成；读（Specialized）
// 可能来自多个 scheduler Execute goroutine。用 sync.RWMutex 保证读写安全，
// 即使运行期没有写也不影响正确性。
//
// nil 安全：`(*AgentRegistry)(nil).Specialized()` 返回 nil 切片。SchedulerExecutor
// 在 Registry 为 nil 时自然退化为"无特化代理"行为，board snapshot 的
// specialized_agents 字段会被省略（omitempty）。
type AgentRegistry struct {
	mu      sync.RWMutex
	entries []SpecializedAgent
}

// NewAgentRegistry 返回一个空的 registry。
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{}
}

// Register 追加一条特化代理声明。
// 同 EventType 重复注册会合并 Count（保险：防止 bootstrap 误注册两次）。
func (r *AgentRegistry) Register(entry SpecializedAgent) {
	if r == nil || entry.EventType == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.entries {
		if existing.EventType == entry.EventType {
			// 合并 Count，Role 以后注册的为准（通常不会冲突）
			r.entries[i].Count += entry.Count
			if entry.Role != "" {
				r.entries[i].Role = entry.Role
			}
			return
		}
	}
	r.entries = append(r.entries, entry)
}

// Specialized 返回所有特化代理的快照拷贝（按 EventType 的 registry 顺序）。
// nil registry 返回 nil。
func (r *AgentRegistry) Specialized() []SpecializedAgent {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.entries) == 0 {
		return nil
	}
	out := make([]SpecializedAgent, len(r.entries))
	copy(out, r.entries)
	return out
}
