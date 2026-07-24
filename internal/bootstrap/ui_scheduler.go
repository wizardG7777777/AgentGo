package bootstrap

import (
	"time"

	"agentgo/internal/ui"
)

// schedulerControlState selects the Plan governed by the Scheduler's current
// controller task. When the controller is temporarily idle (for example while
// waiting on runners or an Interaction), the newest non-terminal Plan remains
// visible so the TUI does not lose the actual control-plane state.
func (s *System) schedulerControlState(controllerTaskID string) *ui.SchedulerControlState {
	if s == nil || s.PlanStore == nil {
		return nil
	}
	summary, err := s.PlanStore.RuntimeSummaryForController(controllerTaskID, time.Now())
	if err != nil || summary == nil {
		return nil
	}
	return &ui.SchedulerControlState{
		PlanID: summary.PlanID, Status: string(summary.Status), Revision: summary.Revision,
		ExecutionStateVersion: summary.ExecutionStateVersion, HandledStateVersion: summary.HandledStateVersion,
		TasksTotal: summary.TasksTotal, TasksPending: summary.TasksPending, TasksProcessing: summary.TasksProcessing,
		TasksCompleted: summary.TasksCompleted, TasksFailed: summary.TasksFailed, TasksCancelled: summary.TasksCancelled,
		PendingReplans: summary.PendingReplans, AcceptanceRunID: summary.AcceptanceRunID,
		AcceptanceStatus: summary.AcceptanceStatus, AcceptanceVerdict: string(summary.AcceptanceVerdict),
		AcceptanceAttempt: summary.AcceptanceAttempt, AcceptanceSpecRevision: summary.AcceptanceSpecRevision,
		PauseReason: summary.PauseReason, BudgetUsedPercent: summary.BudgetUsedPercent,
	}
}
