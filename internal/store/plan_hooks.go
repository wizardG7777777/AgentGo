package store

import (
	"time"

	"agentgo/internal/model"
)

// TaskMutationKind classifies committed Task facts that affect a Plan's
// ExecutionStateVersion. It does not imply that Scheduler must replan.
type TaskMutationKind string

const (
	TaskMutationPublished TaskMutationKind = "published"
	TaskMutationStatus    TaskMutationKind = "status"
	TaskMutationResult    TaskMutationKind = "result"
	TaskMutationArtifact  TaskMutationKind = "artifact"
	TaskMutationProgress  TaskMutationKind = "progress"
)

type TaskMutation struct {
	Kind       TaskMutationKind
	Task       *model.Task
	FromStatus model.TaskStatus
	ToStatus   model.TaskStatus
	Detail     string
	At         time.Time
}

// TaskPlanHooks keeps TaskStore independent from the Plan implementation.
// Prepare runs before a Task becomes visible and may populate Plan metadata or
// reject an unauthorized graph mutation. Mutated runs after a Task fact has
// committed; failures cannot roll the Task write back and are therefore logged.
type TaskPlanHooks struct {
	Prepare func(task *model.Task, parent *model.Task) error
	// CanClaim is evaluated immediately before a pending Task is exposed or
	// claimed. It lets the Plan control plane suspend new work while a Plan is
	// paused/blocked without making store depend on internal/plan.
	CanClaim func(task *model.Task) error
	// CanEvict protects Task facts still referenced by a live Plan from the
	// global terminal FIFO. Returning false pins the Task until a later sweep.
	CanEvict func(task *model.Task) bool
	Mutated  func(TaskMutation) error
}
