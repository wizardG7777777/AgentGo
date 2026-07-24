package plan

import (
	"time"

	"agentgo/internal/model"
)

// RuntimeSummary is a bounded, read-only projection of the Plan facts needed
// by live status surfaces. It intentionally excludes acceptance evidence,
// retired nodes and audit history so a 500ms UI poll does not clone an
// ever-growing Plan through JSON.
type RuntimeSummary struct {
	PlanID                 string
	Status                 model.PlanStatus
	Revision               int64
	ExecutionStateVersion  int64
	HandledStateVersion    int64
	TasksTotal             int
	TasksPending           int
	TasksProcessing        int
	TasksCompleted         int
	TasksFailed            int
	TasksCancelled         int
	PendingReplans         int
	AcceptanceRunID        string
	AcceptanceStatus       string
	AcceptanceVerdict      model.AcceptanceVerdict
	AcceptanceAttempt      int
	AcceptanceSpecRevision int64
	PauseReason            string
	BudgetUsedPercent      float64
}

// RuntimeSummaryForController chooses the Plan owned by controllerTaskID. If
// the Scheduler is between controller turns, it falls back to the newest
// non-terminal Plan, then the newest terminal Plan for post-completion status.
func (s *Store) RuntimeSummaryForController(controllerTaskID string, now time.Time) (*RuntimeSummary, error) {
	if s == nil {
		return nil, ErrPlanNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var matched, active, latest *model.Plan
	for _, rec := range s.state.Plans {
		if rec == nil {
			continue
		}
		p := &rec.Plan
		if latest == nil || p.UpdatedAt.After(latest.UpdatedAt) {
			latest = p
		}
		if !model.IsPlanTerminal(p.Status) && (active == nil || p.UpdatedAt.After(active.UpdatedAt)) {
			active = p
		}
		if controllerTaskID != "" && (p.ActiveDecisionTaskID == controllerTaskID || p.RootTaskID == controllerTaskID) &&
			(matched == nil || p.UpdatedAt.After(matched.UpdatedAt)) {
			matched = p
		}
	}
	selected := matched
	if selected == nil {
		selected = active
	}
	if selected == nil {
		selected = latest
	}
	if selected == nil {
		return nil, ErrPlanNotFound
	}
	return runtimeSummaryFromPlan(selected, now), nil
}

func runtimeSummaryFromPlan(p *model.Plan, now time.Time) *RuntimeSummary {
	state := &RuntimeSummary{
		PlanID: p.ID, Status: p.Status, Revision: p.CurrentRevision,
		ExecutionStateVersion: p.ExecutionStateVersion, HandledStateVersion: p.HandledStateVersion,
		PendingReplans: len(p.PendingReplanRequests), PauseReason: p.PauseReason,
		AcceptanceSpecRevision: p.CurrentAcceptanceSpecRevision,
		BudgetUsedPercent:      runtimeBudgetUsedPercent(p, now),
	}
	for _, taskID := range p.CurrentNodeIDs {
		node, ok := p.Nodes[taskID]
		if !ok || node.Role == model.PlanNodeRoleController {
			continue
		}
		state.TasksTotal++
		switch node.Status {
		case model.TaskStatusPending:
			state.TasksPending++
		case model.TaskStatusProcessing:
			state.TasksProcessing++
		case model.TaskStatusCompleted:
			state.TasksCompleted++
		case model.TaskStatusCancelled:
			state.TasksCancelled++
		case model.TaskStatusFailed, model.TaskStatusBlocked:
			state.TasksFailed++
		}
	}

	var latest *model.AcceptanceRun
	for _, runValue := range p.AcceptanceRuns {
		run := runValue
		state.AcceptanceAttempt++
		if run.SpecID != p.CurrentAcceptanceSpecID || run.SpecRevision != p.CurrentAcceptanceSpecRevision ||
			run.TargetPlanRevision != p.CurrentRevision || run.TargetGraphDigest != p.CurrentGraphDigest {
			continue
		}
		if latest == nil || run.CreatedAt.After(latest.CreatedAt) {
			latest = &run
		}
	}
	if latest != nil {
		state.AcceptanceRunID = latest.ID
		state.AcceptanceStatus = latest.Status
		if result, ok := p.AcceptanceResults[latest.ResultID]; ok {
			state.AcceptanceVerdict = result.Verdict
			if result.Status != "" {
				state.AcceptanceStatus = string(result.Status)
			}
		}
	}
	return state
}

func runtimeBudgetUsedPercent(p *model.Plan, now time.Time) float64 {
	maxRatio := 0.0
	consider := func(used, limit float64) {
		if limit > 0 && used/limit > maxRatio {
			maxRatio = used / limit
		}
	}
	consider(float64(p.Usage.PlanRevisions), float64(p.Budget.MaxPlanRevisions))
	consider(float64(p.Usage.TasksCreated), float64(p.Budget.MaxTasksCreated))
	consider(float64(p.Usage.ActiveTasks), float64(p.Budget.MaxActiveTasks))
	consider(float64(p.Usage.AcceptanceRuns), float64(p.Budget.MaxAcceptanceRuns))
	consider(float64(p.Usage.TokensUsed), float64(p.Budget.MaxTokens))
	consider(p.Usage.CostUsed, p.Budget.MaxCost)
	if p.Budget.MaxWallTime > 0 && !p.Usage.StartedAt.IsZero() {
		elapsed := now.Sub(p.Usage.StartedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		consider(float64(elapsed), float64(p.Budget.MaxWallTime))
	}
	return maxRatio * 100
}
