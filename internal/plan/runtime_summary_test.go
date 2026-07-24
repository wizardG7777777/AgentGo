package plan

import (
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestRuntimeSummaryForControllerProjectsBoundedCurrentState(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	target := model.Plan{
		ID: "plan-1", RootTaskID: "controller-1", ActiveDecisionTaskID: "controller-1",
		Status: model.PlanStatusRunning, CurrentRevision: 7, ExecutionStateVersion: 19,
		HandledStateVersion: 18, CurrentGraphDigest: "digest-7", UpdatedAt: now.Add(-time.Minute),
		CurrentAcceptanceSpecID: "spec-1", CurrentAcceptanceSpecRevision: 3,
		CurrentNodeIDs: []string{"controller", "done", "running", "failed", "cancelled"},
		Nodes: map[string]model.PlanNode{
			"controller": {TaskID: "controller", Role: model.PlanNodeRoleController, Status: model.TaskStatusProcessing},
			"done":       {TaskID: "done", Role: model.PlanNodeRoleInvestigation, Status: model.TaskStatusCompleted},
			"running":    {TaskID: "running", Role: model.PlanNodeRoleImplementation, Status: model.TaskStatusProcessing},
			"failed":     {TaskID: "failed", Role: model.PlanNodeRoleVerification, Status: model.TaskStatusBlocked},
			"cancelled":  {TaskID: "cancelled", Role: model.PlanNodeRoleImplementation, Status: model.TaskStatusCancelled},
		},
		PendingReplanRequests: map[string]model.ReplanRequest{"replan-1": {ID: "replan-1"}},
		AcceptanceRuns: map[string]model.AcceptanceRun{
			"old":   {ID: "old", SpecID: "spec-1", SpecRevision: 3, TargetPlanRevision: 6, TargetGraphDigest: "digest-6", CreatedAt: now.Add(-time.Minute)},
			"run-2": {ID: "run-2", SpecID: "spec-1", SpecRevision: 3, TargetPlanRevision: 7, TargetGraphDigest: "digest-7", Status: "completed", ResultID: "result-2", CreatedAt: now},
		},
		AcceptanceResults: map[string]model.AcceptanceResult{
			"result-2": {ID: "result-2", Verdict: model.AcceptanceVerdictFail, Status: model.AcceptanceResultValid,
				Evidence: []model.Evidence{{ID: "large-evidence", Output: string(make([]byte, 10000))}}},
		},
		Budget: model.PlanBudget{MaxTokens: 1000, MaxTasksCreated: 20},
		Usage:  model.BudgetUsage{TokensUsed: 680, TasksCreated: 5, StartedAt: now.Add(-time.Hour)},
	}
	newer := model.Plan{
		ID: "plan-newer", RootTaskID: "controller-2", ActiveDecisionTaskID: "controller-2",
		Status: model.PlanStatusRunning, UpdatedAt: now,
	}
	s := NewMemoryStore()
	s.state.Plans[target.ID] = &planRecord{Plan: target}
	s.state.Plans[newer.ID] = &planRecord{Plan: newer}
	normalizeRecord(s.state.Plans[target.ID])
	normalizeRecord(s.state.Plans[newer.ID])

	got, err := s.RuntimeSummaryForController("controller-1", now)
	if err != nil || got == nil {
		t.Fatalf("RuntimeSummaryForController: got=%+v err=%v", got, err)
	}
	if got.PlanID != "plan-1" || got.Revision != 7 || got.ExecutionStateVersion != 19 || got.HandledStateVersion != 18 {
		t.Fatalf("identity/state projection = %+v", got)
	}
	if got.TasksTotal != 4 || got.TasksCompleted != 1 || got.TasksProcessing != 1 || got.TasksFailed != 1 || got.TasksCancelled != 1 || got.PendingReplans != 1 {
		t.Fatalf("DAG projection = %+v", got)
	}
	if got.AcceptanceAttempt != 2 || got.AcceptanceRunID != "run-2" || got.AcceptanceStatus != "valid" || got.AcceptanceVerdict != model.AcceptanceVerdictFail {
		t.Fatalf("acceptance projection = %+v", got)
	}
	if got.BudgetUsedPercent != 68 {
		t.Fatalf("budget percent = %v, want 68", got.BudgetUsedPercent)
	}

	fallback, err := s.RuntimeSummaryForController("unknown", now)
	if err != nil || fallback.PlanID != "plan-newer" {
		t.Fatalf("newest active fallback = %+v, err=%v", fallback, err)
	}
}
