package model

import "time"

type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusProcessing TaskStatus = "processing"
	TaskStatusCompleted  TaskStatus = "completed"
	TaskStatusCancelled  TaskStatus = "cancelled"
	TaskStatusFailed     TaskStatus = "failed"
)

// ValidTransitions defines the allowed state machine transitions.
var ValidTransitions = map[TaskStatus][]TaskStatus{
	TaskStatusPending:    {TaskStatusProcessing, TaskStatusCancelled, TaskStatusFailed},
	TaskStatusProcessing: {TaskStatusCompleted, TaskStatusFailed, TaskStatusCancelled, TaskStatusPending},
}

// IsValidTransition checks whether transitioning from one status to another is allowed.
func IsValidTransition(from, to TaskStatus) bool {
	allowed, ok := ValidTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// IsTerminal returns true if the status is a terminal state (completed, cancelled, failed).
func IsTerminal(status TaskStatus) bool {
	return status == TaskStatusCompleted || status == TaskStatusCancelled || status == TaskStatusFailed
}

type Task struct {
	ID             string
	Description    string
	Priority       int
	Dependencies   []string
	Status         TaskStatus
	Agents         []string
	MaxConcurrency int
	Results        map[string]string
	Error          string
	RetryCount     int
	RetryReasons   []string
	LastHistory    []byte // JSON 序列化的历史记录，重试时恢复上下文
	TimeoutSeconds int
	EventSource    string
	EventType      string
	TriggerRule    string
	SystemPrompt   string // 可选的自定义 system prompt，非空时覆盖 Worker 默认 prompt
	PartialOutput  string // 执行中的部分输出，用于流式进度展示
	Depth          int    // 子任务嵌套深度，根任务为 0

	// MailChainDepth 是该任务被第几层邮件唤醒。
	// 用户 /steer 触发的初始任务为 0；被 chain_depth=N 的邮件唤醒的任务为 N。
	// MetaGroup.sendMessage 在构造 outgoing message 时读取此值并 +1 写入 msg.ChainDepth；
	// MailNotifier 在发布 wake task 时根据收件箱内未读邮件的最大 ChainDepth 设置该字段。
	// Phase 2 引入；零值兼容现有任务。
	MailChainDepth int

	// Artifacts 是任务执行期间通过 write_file/edit_file 实际写入的文件路径列表，
	// 路径为相对项目根的相对路径，自动去重。
	// 由 Store.AppendArtifact 在工具调用成功后写入。
	// 用途：下游依赖任务可以通过 Store.GetDependencyArtifacts 拿到这个列表，
	// 注入到自己的 user prompt 中，避免凭空捏造上游产出。
	Artifacts []string

	// ExpectedArtifacts 是发布者声明的"本任务必须产出的文件路径"清单。
	// 任务结束时 agent.processTask 会校验 Artifacts 是否包含全部 ExpectedArtifacts，
	// 缺失则任务失败重试。这是 Level 3 的硬性合约校验。
	// 路径同样为相对项目根的相对路径。
	ExpectedArtifacts []string

	// TransferNote 是一份压缩的跨 agent 交接备忘，供下游任务或重试时恢复上下文。
	//
	// 生成路径（见 internal/agent/transfer_note.go）：
	//   - 成功路径：agent 在 SubmitResult 之前把 lastOutput（LLM 最终响应）直接
	//     写入本字段。lastOutput 本身已经是合理的自述总结，不需额外压缩
	//   - 失败路径：agent.buildTransferNote 两级调用链
	//       L1：生成 <transfer-request> prompt 让 LLM 做最后一次自压缩
	//       L3：机械拼装（无 LLM 调用）——任务目标 + 工具轨迹 + Artifacts + 最后响应
	//
	// 读取路径：
	//   - 依赖链场景：下游 agent 在 processTask 入口通过 Store.GetDependencyTransferNotes
	//     读取所有上游任务的 TransferNote，以 <upstream-transfer-notes> 形式注入首条 history
	//   - 重试换手场景：接手者通过 task.TransferNote 直接读取前任的备忘，
	//     以 <transfer-note> 形式注入首条 history
	//
	// 与 LastHistory 的关系（2026-04-12 引入时保持共存）：
	//   - LastHistory 是完整的历史序列化，重试时完整恢复上下文（可能很大）
	//   - TransferNote 是精炼文本（默认 < 3000 tokens），跨 agent 更友好
	//   - 两者并存，重试时优先用 TransferNote，LastHistory 作为 fallback
	//   - 等 TransferNote 实测稳定后再决定是否删除 LastHistory
	TransferNote string

	// LastResponse 是 agent 最近一次 LLM 非工具响应的原始文本（worker 的"我做完了"那句话）。
	// 在每次 worker 提交"无 tool call"响应时由 Store.RecordLastResponse 写入；
	// 与 Results 不同，即使 ExpectedArtifacts 校验失败导致任务回滚重试，
	// LastResponse 也会保留——scheduler 可以借此看到 worker 自述了什么，
	// 即便任务最终崩溃，也不至于只看到一个"重试次数耗尽"的空错误。
	LastResponse string

	// SchedulerBatch 是 scheduler agent 当前 reactLoop 跟踪的子任务 ID 列表。
	// 由 SchedulerGroup.publishTask 在每次发布时追加；report_done 时清空。
	// 仅在 EventType="__scheduler__" 任务上有意义；其他 task 该字段为空。
	//
	// 与 Dependencies 的关键差异：
	//   - Dependencies 用于 worker 任务间的依赖（watchdog 会级联取消失败的依赖）
	//   - SchedulerBatch 仅供 SchedulerExecutor 等待 batch 完成（终态而非严格 completed）
	//
	// Phase 3 引入；零值兼容现有调用方。
	SchedulerBatch []string

	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}
