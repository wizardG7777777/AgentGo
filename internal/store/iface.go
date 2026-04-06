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

	// Non-atomic read operations (snapshot, no lock required)

	QueryAvailable(eventType string) ([]*model.Task, error)
	GetTask(taskID string) (*model.Task, error)
	GetDependencyResults(taskID string) (map[string]string, error)
	ScanAll() ([]*model.Task, error)
}
