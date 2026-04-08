package store

import "agentgo/internal/model"

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

	// RecordLastResponse 持久化 agent 最近一次 LLM 非工具响应（worker 的"我做完了"那句话）。
	// 在 SubmitResult 成功路径和 ExpectedArtifacts 校验失败路径都会调用——
	// 这样即使任务最终失败，scheduler 也能在快照里看到 worker 自述了什么，
	// 而不是只看到一个干瘪的 "重试次数耗尽" 错误。
	RecordLastResponse(taskID string, content string) error

	// Non-atomic read operations (snapshot, no lock required)

	QueryAvailable(eventType string) ([]*model.Task, error)
	GetTask(taskID string) (*model.Task, error)
	GetDependencyResults(taskID string) (map[string]string, error)
	// GetDependencyArtifacts 返回 taskID 所有依赖任务实际写入的文件路径，
	// 按依赖任务的 ID 分组：map[parent_task_id][]artifact_path。
	// 由 agent.processTask 在任务启动时调用，把结果注入下游 worker 的 user prompt。
	GetDependencyArtifacts(taskID string) (map[string][]string, error)
	ScanAll() ([]*model.Task, error)
}
