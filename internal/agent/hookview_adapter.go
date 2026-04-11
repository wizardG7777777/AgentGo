package agent

import (
	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// NewStoreHookAdapter 把 store.StoreHookView 包装为 hook.AgentStoreView。
//
// 为什么需要适配器：
//
//   - hook 包不能导入 store 包（会让 hook/builtin 的所有 hook 隐式依赖 store，
//     同时引入循环：store 实现了 hook.AgentStoreView → store 依赖 hook）
//   - 因此 hook.AgentStoreView 定义了独立的值类型 AgentTaskView / AgentToolCallRecord，
//     与 store 包的 model.Task / store.ToolCallRecord 解耦
//   - 本适配器负责双向翻译：调用 store.GetTask / GetToolCallHistory，
//     把返回值转为 hook 包的本地类型
//
// 使用：bootstrap 在构造 Agent 时
//
//	adapter := agent.NewStoreHookAdapter(taskStore)
//	a.HookStoreView = adapter
func NewStoreHookAdapter(sv store.StoreHookView) hook.AgentStoreView {
	return &storeHookAdapter{sv: sv}
}

type storeHookAdapter struct {
	sv store.StoreHookView
}

// GetTask 翻译 store.StoreHookView.GetTask 的返回值。
// store 的 GetTask 在任务不存在时返回 (nil, error)；hook 接口简化为 (view, bool)。
func (a *storeHookAdapter) GetTask(taskID string) (hook.AgentTaskView, bool) {
	t, err := a.sv.GetTask(taskID)
	if err != nil || t == nil {
		return hook.AgentTaskView{}, false
	}
	// 浅拷贝切片字段，防止 hook 通过引用修改 store 内部状态。
	// model.Task 本身已经是 Store 内部的快照副本，但 Artifacts / Dependencies
	// 是切片（引用），再复制一层保险。
	artifacts := make([]string, len(t.Artifacts))
	copy(artifacts, t.Artifacts)
	deps := make([]string, len(t.Dependencies))
	copy(deps, t.Dependencies)
	return hook.AgentTaskView{
		ID:           t.ID,
		Description:  t.Description,
		Status:       string(t.Status),
		Artifacts:    artifacts,
		RetryCount:   t.RetryCount,
		EventType:    t.EventType,
		Dependencies: deps,
	}, true
}

// GetToolCallHistory 翻译 store.ToolCallRecord 列表为 hook 层类型。
// 只投影 hook 层需要的字段——ToolName / Args / Success。
// Timestamp 和 AgentID 故意不暴露（hook 不需要知道是谁/什么时候调的）。
func (a *storeHookAdapter) GetToolCallHistory(taskID string) []hook.AgentToolCallRecord {
	recs := a.sv.GetToolCallHistory(taskID)
	if len(recs) == 0 {
		return nil
	}
	out := make([]hook.AgentToolCallRecord, 0, len(recs))
	for _, r := range recs {
		// 浅拷贝 Args map 防止 hook 修改 store 内的引用
		args := make(map[string]any, len(r.Args))
		for k, v := range r.Args {
			args[k] = v
		}
		out = append(out, hook.AgentToolCallRecord{
			ToolName: r.ToolName,
			Args:     args,
			Success:  r.Success,
		})
	}
	return out
}
