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
	WorktreePath   string // 任务关联的 worktree 路径，空表示未启用隔离
	CreatedAt      time.Time
	StartedAt      time.Time
	CompletedAt    time.Time
}
