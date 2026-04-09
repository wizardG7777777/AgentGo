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

	// LastResponse 是 agent 最近一次 LLM 非工具响应的原始文本（worker 的"我做完了"那句话）。
	// 在每次 worker 提交"无 tool call"响应时由 Store.RecordLastResponse 写入；
	// 与 Results 不同，即使 ExpectedArtifacts 校验失败导致任务回滚重试，
	// LastResponse 也会保留——scheduler 可以借此看到 worker 自述了什么，
	// 即便任务最终崩溃，也不至于只看到一个"重试次数耗尽"的空错误。
	LastResponse string

	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}
