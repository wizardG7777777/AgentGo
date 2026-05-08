package store

import (
	"time"

	"agentgo/internal/model"
)

// ToolCallRecord 记录一次工具调用的事实快照。
// 由 llm_executor.go 在每次工具调用之后（无论成功、失败，还是被 hook Abort）
// 自动写入公告板，供 hook 系统的 RequireReadBeforeWriteHook 等查询任务历史。
//
// Args 是工具调用的原始参数；Success=false 的记录包含 hook 拒绝和工具错误两种情况，
// 由 hook 消费者自行决定是否计入统计。
type ToolCallRecord struct {
	Timestamp time.Time
	AgentID   string
	ToolName  string
	Args      map[string]any
	Success   bool
}

type TaskStore interface {
	// Atomic write operations (require lock)

	PublishTask(task *model.Task) error
	ClaimTask(agentID string, taskID string) error
	SubmitResult(agentID string, taskID string, result string) error
	TransitionState(taskID string, from, to model.TaskStatus) error
	FailTask(agentID string, taskID string, reason string) error
	FailTaskBySystem(taskID string, reason string) error
	RetryRollback(agentID string, taskID string, reason string) error
	AppendOutput(agentID string, taskID string, chunk string) error

	// AppendArtifact 把一个文件路径追加到 task.Artifacts，自动去重。
	// 由 LocalWriteGroup 在 write_file/edit_file 成功后调用。
	// path 应当是相对项目根目录的相对路径（调用方负责标准化）。
	AppendArtifact(taskID string, path string) error

	// AppendSchedulerBatch 把一个子任务 ID 追加到 task.SchedulerBatch，自动去重。
	// 由 SchedulerGroup 在 scheduler 通过 publish_task 发布子任务时调用，
	// 让 SchedulerExecutor 之后能等待这一批 task 全部进入终态。
	// 仅对 EventType="__scheduler__" 任务有意义。
	// Phase 3 引入。
	AppendSchedulerBatch(taskID string, childTaskID string) error

	// ClearSchedulerBatch 清空 task.SchedulerBatch。
	// 由 SchedulerGroup.report_done 在汇报完成时调用。
	// Phase 3 引入。
	ClearSchedulerBatch(taskID string) error

	// AppendToolCall 追加一条工具调用记录到指定任务的历史。
	// 由 llm_executor.go 在每次 tools.Dispatch 之后自动写入（包括被 hook Abort 的调用）。
	// hook 系统通过 StoreHookView.GetToolCallHistory 查询这些记录做事实校对。
	AppendToolCall(taskID string, rec ToolCallRecord) error

	// QueryToolCalls 返回指定任务的工具调用历史。
	// toolName == "" 时返回该任务的全部记录；否则只返回匹配 toolName 的记录。
	// 返回切片是内部数据的浅拷贝，调用方可安全遍历修改。
	// 任务不存在时返回 (nil, nil)——hook 需要容忍这种情形。
	QueryToolCalls(taskID string, toolName string) ([]ToolCallRecord, error)

	// RecordLastResponse 持久化 agent 最近一次 LLM 非工具响应（worker 的"我做完了"那句话）。
	// 在 SubmitResult 成功路径和 ExpectedArtifacts 校验失败路径都会调用——
	// 这样即使任务最终失败，scheduler 也能在快照里看到 worker 自述了什么，
	// 而不是只看到一个干瘪的 "重试次数耗尽" 错误。
	RecordLastResponse(taskID string, content string) error

	// SetTransferNote 写入 task.TransferNote——跨 agent 交接的压缩备忘。
	// 由 agent 在任务成功（lastOutput 直传）或失败（buildTransferNote L1/L3 链）
	// 时调用。下游依赖任务通过 GetDependencyTransferNotes 读取；重试接手者通过
	// GetTask 直接读 task.TransferNote。
	// Sprint 3 #5 引入。
	SetTransferNote(taskID string, note string) error

	// Non-atomic read operations (snapshot, no lock required)

	QueryAvailable(eventType string) ([]*model.Task, error)
	GetTask(taskID string) (*model.Task, error)
	GetDependencyResults(taskID string) (map[string]string, error)
	// GetDependencyArtifacts 返回 taskID 所有依赖任务实际写入的文件路径，
	// 按依赖任务的 ID 分组：map[parent_task_id][]artifact_path。
	// 由 agent.processTask 在任务启动时调用，把结果注入下游 worker 的 user prompt。
	GetDependencyArtifacts(taskID string) (map[string][]string, error)

	// GetDependencyTransferNotes 返回 taskID 所有依赖任务的 TransferNote 文本，
	// 按依赖任务的 ID 分组：map[parent_task_id]transfer_note。
	// 依赖任务 TransferNote 为空时该条目被省略——接手者只看到非空的上游备忘。
	// 由 agent.processTask 在任务启动时调用，把结果以 <upstream-transfer-notes>
	// 形式注入下游 agent 的 user prompt。
	// Sprint 3 #5 引入。
	GetDependencyTransferNotes(taskID string) (map[string]string, error)

	ScanAll() ([]*model.Task, error)
}

type cancelSourceTransitioner interface {
	TransitionStateWithCancelSource(taskID string, from, to model.TaskStatus, cancelSource string) error
}

// TransitionStateWithCancelSource keeps TaskStore compatibility while allowing
// stores that understand cancel sources to attach structured cancellation metadata.
func TransitionStateWithCancelSource(s TaskStore, taskID string, from, to model.TaskStatus, cancelSource string) error {
	if st, ok := s.(cancelSourceTransitioner); ok {
		return st.TransitionStateWithCancelSource(taskID, from, to, cancelSource)
	}
	return s.TransitionState(taskID, from, to)
}
