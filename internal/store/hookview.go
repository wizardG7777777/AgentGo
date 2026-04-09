package store

import "agentgo/internal/model"

// StoreHookView 是 hook 系统访问 Store 的只读视图（外加 AppendArtifact 这一个
// postCall hook 的约定写操作）。
//
// 设计原则（hookSystem.md §11.1.1 / §11.4）：
//   - hook 构造时拿到的是本接口，不是完整 TaskStore，防止 hook 侵入编排能力
//   - 除 AppendArtifact 外全部只读——AppendArtifact 是 RecordArtifactHook 的
//     唯一写入途径，其语义是"事实登记"而非状态变更
//   - AppendToolCall 不在本接口上——它由 llm_executor.go 通过独立闭包写入
//     （C4.3 方案 A），避免 hook 能自己塞历史做作弊
//
// MemoryTaskStore 自动实现本接口（接口子集）：三个方法的签名与
// MemoryTaskStore.GetTask/AppendArtifact/QueryToolCalls 一致（后者通过
// GetToolCallHistory 语义包装）。因此在 bootstrap 里可以直接 `var v StoreHookView = taskStore` 赋值。
type StoreHookView interface {
	// GetTask 返回任务的只读快照，错误语义与 TaskStore.GetTask 一致
	// （任务不存在时返回 ErrTaskNotFound）。
	GetTask(taskID string) (*model.Task, error)

	// AppendArtifact 把文件路径追加到任务产物清单。
	// 由 RecordArtifactHook（PostCall on write_file/edit_file）调用。
	// path 应当是相对项目根的相对路径，由调用方负责标准化。
	AppendArtifact(taskID string, path string) error

	// GetToolCallHistory 返回任务的完整工具调用历史，按时间升序。
	// 任务不存在时返回 nil（hook 需要容忍这种情形，例如任务已被淘汰）。
	// 返回值是内部数据的浅拷贝，调用方可以安全遍历。
	GetToolCallHistory(taskID string) []ToolCallRecord
}

// GetToolCallHistory 实现 StoreHookView 接口的简化包装——内部委托给
// QueryToolCalls(taskID, "") 返回全任务历史。把 error 吞掉是有意的设计：
// hook 查询历史失败不应当阻塞工具调用链路，hook 只需要看到"能查到什么"。
func (s *MemoryTaskStore) GetToolCallHistory(taskID string) []ToolCallRecord {
	recs, _ := s.QueryToolCalls(taskID, "")
	return recs
}

// 编译期断言：MemoryTaskStore 必须自动满足 StoreHookView 接口。
// 如果未来接口签名漂移，编译会立即失败，提醒调用方同步更新。
var _ StoreHookView = (*MemoryTaskStore)(nil)
