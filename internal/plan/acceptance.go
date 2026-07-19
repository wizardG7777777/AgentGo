package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentgo/internal/model"

	"github.com/google/uuid"
)

const builtinCurrentGraphCriterionID = "builtin.current_graph_completed"

const (
	criterionCheckCurrentGraph = "current_graph_completed"
	criterionCheckCommandExit  = "command_exit"
	criterionCheckFileHash     = "file_hash"
	criterionCheckTaskStatus   = "task_status"
	criterionCheckEvidence     = "evidence"
	criterionCheckManual       = "manual"

	maxAcceptanceCriteria      = 64
	maxAcceptanceCriteriaBytes = 64 << 10
	maxAcceptanceSpecID        = 128
	maxAcceptanceCreatedBy     = 256
	maxAcceptanceCriterionID   = 128
	maxAcceptanceDescription   = 2048
	maxAcceptanceCheck         = 64
	maxAcceptanceTarget        = 4096
	maxAcceptanceExpected      = 2048

	maxAcceptanceResultInputBytes = 768 << 10
	maxAcceptanceResultItems      = 64
	maxAcceptanceEvidenceItems    = 256
	maxAcceptanceEvidenceIDs      = 64
	maxAcceptanceResultText       = 4096
	maxAcceptanceEvidenceOutput   = 64 << 10
	maxAcceptanceEvidenceCommand  = 8192
	maxAcceptanceResultIDField    = 256
)

var builtinCurrentGraphCriterion = model.Criterion{
	ID:          builtinCurrentGraphCriterionID,
	Description: "Every Task targeted by this AcceptanceRun and each blocking dependency is completed",
	Source:      model.AcceptanceAuthorityBuiltin, Required: true,
	Scope: model.AcceptanceScopePlan, Check: "current_graph_completed",
	Expected: "completed", BuiltinHardRule: true,
}

// TaskSpec is the plan package's narrow task publication DTO. The integration
// layer adapts it to model.Task without making this package depend on TaskStore.
type TaskSpec struct {
	PlanID       string
	Description  string
	EventType    string
	Role         model.PlanNodeRole
	Dependencies []string
	ParentTaskID string
	ReplyToAgentID string
	BatchID      string
	Metadata     map[string]string
}

type TaskBackend interface {
	PublishTask(context.Context, TaskSpec) (taskID string, err error)
}

// AcceptanceVerifier is the integration hook for facts owned outside this
// package: Task terminal state, artifact existence/hash and actual command
// execution. Core acceptance checks are still applied after this hook.
type AcceptanceVerifier interface {
	VerifyAcceptance(context.Context, *model.Plan, model.AcceptanceRun, model.AcceptanceResult) error
}

func (c *Coordinator) DefineAcceptanceSpec(ctx context.Context, planID string, spec model.AcceptanceSpec) (*model.AcceptanceSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(spec.Criteria) == 0 {
		return nil, fmt.Errorf("acceptance spec requires at least one project, user, or scheduler criterion")
	}
	if len(spec.ID) > maxAcceptanceSpecID {
		return nil, fmt.Errorf("acceptance spec id exceeds %d bytes", maxAcceptanceSpecID)
	}
	if len(spec.CreatedBy) > maxAcceptanceCreatedBy {
		return nil, fmt.Errorf("acceptance spec created_by exceeds %d bytes", maxAcceptanceCreatedBy)
	}
	criteria, err := withBuiltinCriteria(spec.Criteria)
	if err != nil {
		return nil, err
	}
	spec.Criteria = criteria
	if len(spec.Criteria) > maxAcceptanceCriteria {
		return nil, fmt.Errorf("acceptance spec has %d criteria; maximum is %d", len(spec.Criteria), maxAcceptanceCriteria)
	}
	seen := make(map[string]bool, len(spec.Criteria))
	for i := range spec.Criteria {
		criterion := &spec.Criteria[i]
		if strings.TrimSpace(criterion.ID) == "" || strings.TrimSpace(criterion.Description) == "" || strings.TrimSpace(criterion.Check) == "" {
			return nil, fmt.Errorf("acceptance criterion id, description and check are required")
		}
		if err := validateCriterionSize(*criterion); err != nil {
			return nil, err
		}
		if err := validateCriterionCheck(*criterion); err != nil {
			return nil, err
		}
		switch criterion.Source {
		case model.AcceptanceAuthorityBuiltin, model.AcceptanceAuthorityUser,
			model.AcceptanceAuthorityProject, model.AcceptanceAuthorityScheduler:
		default:
			return nil, fmt.Errorf("acceptance criterion %s has invalid source %q", criterion.ID, criterion.Source)
		}
		switch criterion.Scope {
		case model.AcceptanceScopeTask, model.AcceptanceScopeMilestone, model.AcceptanceScopePlan:
		default:
			return nil, fmt.Errorf("acceptance criterion %s has invalid scope %q", criterion.ID, criterion.Scope)
		}
		if seen[criterion.ID] {
			return nil, fmt.Errorf("duplicate acceptance criterion %s", criterion.ID)
		}
		seen[criterion.ID] = true
	}
	criteriaJSON, err := json.Marshal(spec.Criteria)
	if err != nil {
		return nil, fmt.Errorf("encode acceptance criteria: %w", err)
	}
	if len(criteriaJSON) > maxAcceptanceCriteriaBytes {
		return nil, fmt.Errorf("acceptance criteria encode to %d bytes; maximum is %d", len(criteriaJSON), maxAcceptanceCriteriaBytes)
	}
	var stored model.AcceptanceSpec
	err = c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[planID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if err := ensureControllerAuthority(ctx, p); err != nil {
			return err
		}
		if model.IsPlanTerminal(p.Status) {
			return ErrPlanTerminal
		}
		if p.CurrentAcceptanceSpecID != "" {
			previous := p.AcceptanceSpecs[p.CurrentAcceptanceSpecID]
			if err := validateProtectedCriteria(previous, spec); err != nil {
				return err
			}
		}
		if spec.ID == "" {
			spec.ID = uuid.NewString()
		}
		spec.PlanID = planID
		spec.Revision = p.CurrentAcceptanceSpecRevision + 1
		if spec.CreatedAt.IsZero() {
			spec.CreatedAt = time.Now().UTC()
		}
		p.AcceptanceSpecs[spec.ID] = spec
		p.CurrentAcceptanceSpecID = spec.ID
		p.CurrentAcceptanceSpecRevision = spec.Revision
		p.UpdatedAt = time.Now().UTC()
		stored = spec
		return nil
	})
	if err != nil {
		return nil, err
	}
	cp, err := cloneJSON(stored)
	return &cp, err
}

func validateCriterionSize(criterion model.Criterion) error {
	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"id", criterion.ID, maxAcceptanceCriterionID},
		{"description", criterion.Description, maxAcceptanceDescription},
		{"check", criterion.Check, maxAcceptanceCheck},
		{"target", criterion.Target, maxAcceptanceTarget},
		{"expected", criterion.Expected, maxAcceptanceExpected},
	}
	for _, field := range fields {
		if len(field.value) > field.limit {
			return fmt.Errorf("acceptance criterion %s %s exceeds %d bytes", criterion.ID, field.name, field.limit)
		}
	}
	return nil
}

func validateCriterionCheck(criterion model.Criterion) error {
	switch criterion.Check {
	case criterionCheckCommandExit:
		if strings.TrimSpace(criterion.Target) == "" {
			return fmt.Errorf("acceptance criterion %s command_exit target is required", criterion.ID)
		}
		exitCode, err := strconv.Atoi(criterion.Expected)
		if err != nil || exitCode < 0 || exitCode > 255 || strconv.Itoa(exitCode) != criterion.Expected {
			return fmt.Errorf("acceptance criterion %s command_exit expected must be a canonical exit code integer from 0 to 255", criterion.ID)
		}
		return nil
	case criterionCheckFileHash:
		if strings.TrimSpace(criterion.Target) == "" {
			return fmt.Errorf("acceptance criterion %s file_hash target is required", criterion.ID)
		}
		return nil
	case criterionCheckTaskStatus:
		if strings.TrimSpace(criterion.Target) == "" {
			return fmt.Errorf("acceptance criterion %s task_status target is required", criterion.ID)
		}
		switch model.TaskStatus(criterion.Expected) {
		case model.TaskStatusPending, model.TaskStatusProcessing, model.TaskStatusCompleted,
			model.TaskStatusCancelled, model.TaskStatusFailed, model.TaskStatusBlocked:
			return nil
		default:
			return fmt.Errorf("acceptance criterion %s task_status expected is invalid: %q", criterion.ID, criterion.Expected)
		}
	case criterionCheckEvidence, criterionCheckManual:
		return nil
	case criterionCheckCurrentGraph:
		if sameProtectedCriterion(criterion, builtinCurrentGraphCriterion) {
			return nil
		}
		return fmt.Errorf("acceptance criterion %s may not redefine the control-plane current graph check", criterion.ID)
	default:
		return fmt.Errorf("acceptance criterion %s uses unsupported check %q", criterion.ID, criterion.Check)
	}
}

func withBuiltinCriteria(criteria []model.Criterion) ([]model.Criterion, error) {
	out := append([]model.Criterion(nil), criteria...)
	for _, criterion := range out {
		if criterion.ID == builtinCurrentGraphCriterionID ||
			criterion.Source == model.AcceptanceAuthorityBuiltin || criterion.BuiltinHardRule {
			return nil, fmt.Errorf("%w: criterion %s attempts to define control-plane builtin authority", ErrAcceptanceSpecWeakening, criterion.ID)
		}
	}
	out = append(out, builtinCurrentGraphCriterion)
	return out, nil
}

func validateProtectedCriteria(previous, next model.AcceptanceSpec) error {
	nextByID := make(map[string]model.Criterion, len(next.Criteria))
	for _, criterion := range next.Criteria {
		nextByID[criterion.ID] = criterion
	}
	for _, criterion := range previous.Criteria {
		protected := criterion.BuiltinHardRule || criterion.Source == model.AcceptanceAuthorityBuiltin ||
			criterion.Source == model.AcceptanceAuthorityUser
		if !protected {
			continue
		}
		replacement, ok := nextByID[criterion.ID]
		if !ok || !sameProtectedCriterion(criterion, replacement) {
			return fmt.Errorf("%w: criterion %s", ErrAcceptanceSpecWeakening, criterion.ID)
		}
	}
	return nil
}

func sameProtectedCriterion(a, b model.Criterion) bool {
	return a.ID == b.ID && a.Description == b.Description && a.Source == b.Source &&
		a.Required == b.Required && a.Scope == b.Scope && a.Check == b.Check &&
		a.Target == b.Target && a.Expected == b.Expected && a.BuiltinHardRule == b.BuiltinHardRule
}

type EnsureAcceptanceRunInput struct {
	PlanID        string
	Scope         model.AcceptanceScope
	TargetTaskIDs []string
	RunnerKind    string
	Description   string
	Dependencies  []string
	ParentTaskID  string
	ReplyToAgentID string
	BatchID       string
}

func (c *Coordinator) EnsureAcceptanceRun(ctx context.Context, in EnsureAcceptanceRunInput) (*model.AcceptanceRun, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	c.acceptanceMu.Lock()
	defer c.acceptanceMu.Unlock()

	var run model.AcceptanceRun
	created := false
	var budgetErr error
	var budgetNotify bool
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[in.PlanID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if err := ensureControllerAuthority(ctx, p); err != nil {
			return err
		}
		if err := ensurePlanMutable(p); err != nil {
			return err
		}
		if p.CurrentAcceptanceSpecID == "" {
			return ErrAcceptanceSpecNotDefined
		}
		scope := in.Scope
		if scope == "" {
			scope = model.AcceptanceScopePlan
		}
		switch scope {
		case model.AcceptanceScopeTask, model.AcceptanceScopeMilestone, model.AcceptanceScopePlan:
		default:
			return fmt.Errorf("invalid acceptance scope %q", scope)
		}
		targets := sortedUniqueStrings(in.TargetTaskIDs)
		if len(targets) == 0 && scope == model.AcceptanceScopePlan {
			for _, taskID := range p.CurrentNodeIDs {
				if p.Nodes[taskID].Role != model.PlanNodeRoleAcceptance {
					targets = append(targets, taskID)
				}
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("acceptance run requires at least one current target task")
		}
		current := make(map[string]bool, len(p.CurrentNodeIDs))
		for _, taskID := range p.CurrentNodeIDs {
			current[taskID] = true
		}
		for _, taskID := range targets {
			if !current[taskID] {
				return fmt.Errorf("%w: acceptance target %s is not in the current graph", ErrNodeNotFound, taskID)
			}
		}
		key := acceptanceRunKey(p, scope, targets)
		if existingID, ok := rec.AcceptanceRunKeys[key]; ok {
			existing := p.AcceptanceRuns[existingID]
			if acceptanceRunReusable(p, existing) {
				run = existing
				return nil
			}
			delete(rec.AcceptanceRunKeys, key)
		}
		if reason := budgetReason(p, budgetDelta{acceptance: 1}); reason != "" {
			now := time.Now().UTC()
			pausePlan(p, reason, "acceptance run rejected by budget", now)
			p.ExecutionStateVersion++
			_, _, _ = appendRequest(rec, model.ReplanRequest{
				PlanID: p.ID, SourceEvent: "budget", ReasonCode: "budget_exhausted",
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency: model.ReplanUrgencyHigh,
			})
			budgetNotify = true
			budgetErr = fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
			return nil
		}
		run = model.AcceptanceRun{
			ID: uuid.NewString(), Key: key, PlanID: p.ID,
			SpecID: p.CurrentAcceptanceSpecID, SpecRevision: p.CurrentAcceptanceSpecRevision,
			Scope: scope, TargetPlanRevision: p.CurrentRevision,
			TargetGraphDigest: p.CurrentGraphDigest, TargetTaskIDs: targets,
			RunnerKind: in.RunnerKind, Status: "pending",
			CreatedAt: nextAcceptanceRunCreatedAt(p, time.Now().UTC()),
		}
		p.AcceptanceRuns[run.ID] = run
		rec.AcceptanceRunKeys[key] = run.ID
		p.Usage.AcceptanceRuns++
		now := time.Now().UTC()
		addedWarning, warningErr := appendSoftBudgetRequest(rec, p, now)
		if warningErr != nil {
			return warningErr
		}
		if addedWarning {
			budgetNotify = true
		}
		p.UpdatedAt = now
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if budgetNotify {
		c.notify(in.PlanID)
	}
	if budgetErr != nil {
		return nil, false, budgetErr
	}
	if !created || c.backend == nil {
		cp, cloneErr := cloneJSON(run)
		return &cp, created, cloneErr
	}

	description := in.Description
	p, _ := c.store.GetPlan(run.PlanID)
	spec := p.AcceptanceSpecs[run.SpecID]
	criteriaJSON, _ := json.MarshalIndent(spec.Criteria, "", "  ")
	formalContext := fmt.Sprintf("Run formal acceptance for plan %s. AcceptanceRunID=%s; target PlanRevision=%d; GraphDigest=%s; AcceptanceSpec=%s revision=%d; target Task IDs=%s. Evaluate every non-system criterion below and submit the structured result with submit_acceptance_result. The builtin.current_graph_completed result is generated by the control plane.\nCriteria:\n%s",
		run.PlanID, run.ID, run.TargetPlanRevision, run.TargetGraphDigest, run.SpecID, run.SpecRevision,
		strings.Join(run.TargetTaskIDs, ","), string(criteriaJSON))
	if description == "" {
		description = formalContext
	} else {
		description = strings.TrimSpace(description) + "\n\n" + formalContext
	}
	taskID, publishErr := c.backend.PublishTask(ctx, TaskSpec{
		PlanID: run.PlanID, Description: description, EventType: in.RunnerKind,
		Role: model.PlanNodeRoleAcceptance, Dependencies: sortedUniqueStrings(append(in.Dependencies, run.TargetTaskIDs...)),
		ParentTaskID: in.ParentTaskID, ReplyToAgentID: in.ReplyToAgentID, BatchID: in.BatchID,
		Metadata: map[string]string{"acceptance_run_id": run.ID, "acceptance_spec_id": run.SpecID},
	})
	updateErr := c.store.update(func(state *persistentState) error {
		rec := state.Plans[in.PlanID]
		stored := rec.Plan.AcceptanceRuns[run.ID]
		if publishErr != nil {
			stored.Status = "publish_failed"
			rec.Plan.Warnings = append(rec.Plan.Warnings, model.PlanWarning{
				Code: "acceptance_publish_failed", Message: publishErr.Error(), CreatedAt: time.Now().UTC(),
			})
		} else {
			stored.RunnerTaskID = taskID
			stored.Status = "running"
			// Publishing the acceptance Task may itself register a current DAG node.
			// Rebase atomically and recompute Plan-scope work targets so a concurrent
			// non-acceptance node can never sit inside the digest but outside scope.
			delete(rec.AcceptanceRunKeys, stored.Key)
			if stored.Scope == model.AcceptanceScopePlan {
				stored.TargetTaskIDs = currentPlanAcceptanceTargets(&rec.Plan)
			}
			stored.TargetPlanRevision = rec.Plan.CurrentRevision
			stored.TargetGraphDigest = rec.Plan.CurrentGraphDigest
			stored.Key = acceptanceRunKey(&rec.Plan, stored.Scope, stored.TargetTaskIDs)
			rec.AcceptanceRunKeys[stored.Key] = stored.ID
		}
		rec.Plan.AcceptanceRuns[run.ID] = stored
		rec.Plan.UpdatedAt = time.Now().UTC()
		run = stored
		return nil
	})
	if updateErr != nil {
		return nil, true, updateErr
	}
	if publishErr != nil {
		return &run, true, publishErr
	}
	return &run, true, nil
}

func hasValidPassResult(p *model.Plan, run model.AcceptanceRun) bool {
	if run.ResultID == "" {
		return false
	}
	result, ok := p.AcceptanceResults[run.ResultID]
	return ok && result.Status == model.AcceptanceResultValid && result.Verdict == model.AcceptanceVerdictPass
}

func acceptanceRunReusable(p *model.Plan, run model.AcceptanceRun) bool {
	if run.Status == "pending" || run.Status == "running" {
		return true
	}
	if !hasValidPassResult(p, run) {
		return false
	}
	if run.RunnerTaskID == "" {
		// In-memory/core callers without a Task backend retain the historical
		// behavior. Production runs always bind a runner Task.
		return true
	}
	runner, ok := p.Nodes[run.RunnerTaskID]
	if !ok || runner.Role != model.PlanNodeRoleAcceptance {
		return false
	}
	// A submitted result freezes the runner's tools, but the runner still owns
	// one text-only completion round. Reuse that in-flight run rather than
	// spawning duplicates; only completed runners provide a reusable PASS.
	return !model.IsTerminal(runner.Status) || runner.Status == model.TaskStatusCompleted
}

// MarkMissingAcceptanceRunners closes AcceptanceRuns whose durable runner Task
// is absent during crash recovery, plus unbound pending runs left behind when a
// crash occurred between the first PlanStore commit and backend publication. It
// never fabricates a Task or node terminal fact: only the run lease and its
// idempotency index are updated atomically, so the surrounding recovery flow
// can keep the Plan blocked until a user chooses whether to resume it.
func (c *Coordinator) MarkMissingAcceptanceRunners(ctx context.Context, planID string, missingTaskIDs []string) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	missing := make(map[string]bool, len(missingTaskIDs))
	for _, taskID := range missingTaskIDs {
		if taskID = strings.TrimSpace(taskID); taskID != "" {
			missing[taskID] = true
		}
	}
	var notify bool
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[planID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if model.IsPlanTerminal(p.Status) {
			return ErrPlanTerminal
		}
		now := time.Now().UTC()
		changed := 0
		for runID, run := range p.AcceptanceRuns {
			unboundPublish := run.RunnerTaskID == "" && run.ResultID == "" && run.Status == "pending"
			missingRunner := run.RunnerTaskID != "" && missing[run.RunnerTaskID]
			if (!unboundPublish && !missingRunner) || strings.HasSuffix(run.Status, "_on_recovery") {
				continue
			}
			if unboundPublish {
				run.Status = "publish_abandoned_on_recovery"
			} else if run.ResultID == "" {
				run.Status = "runner_missing_on_recovery"
			} else {
				run.Status = "runner_missing_after_result_on_recovery"
			}
			if run.CompletedAt.IsZero() {
				run.CompletedAt = now
			}
			p.AcceptanceRuns[runID] = run
			if indexedID, indexed := rec.AcceptanceRunKeys[run.Key]; indexed && indexedID == run.ID {
				delete(rec.AcceptanceRunKeys, run.Key)
			}
			changed++
		}
		if changed > 0 {
			p.ExecutionStateVersion++
			p.Warnings = append(p.Warnings, model.PlanWarning{
				Code:      "acceptance_runner_missing_on_recovery",
				Message:   fmt.Sprintf("%d acceptance runner task(s) were absent from the recovered Task snapshot", changed),
				CreatedAt: now,
			})
			if _, created, requestErr := appendRequest(rec, model.ReplanRequest{
				PlanID: p.ID, SourceEvent: "recovery", ReasonCode: "acceptance_runner_recovery",
				Detail:           fmt.Sprintf("%d acceptance runner lease(s) were closed during recovery", changed),
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency:        model.ReplanUrgencyHigh,
				IdempotencyKey: fmt.Sprintf("acceptance-runner-recovery:%d", p.ExecutionStateVersion),
				CreatedAt:      now,
			}); requestErr != nil {
				return requestErr
			} else if created {
				notify = true
			}
			p.UpdatedAt = now
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notify {
		c.notify(planID)
	}
	return c.store.GetPlan(planID)
}

func acceptanceRunKey(p *model.Plan, scope model.AcceptanceScope, targets []string) string {
	return fmt.Sprintf("%s|%s|%d|%s|%d|%s|%s", p.ID, p.CurrentAcceptanceSpecID,
		p.CurrentAcceptanceSpecRevision, scope, p.CurrentRevision, p.CurrentGraphDigest,
		strings.Join(targets, ","))
}

// SubmitAcceptanceResult stores the first result for an AcceptanceRun. The
// created return value is false for an idempotent replay, allowing callers to
// avoid recording progress or emitting completion events more than once.
func (c *Coordinator) SubmitAcceptanceResult(ctx context.Context, result model.AcceptanceResult) (*model.AcceptanceResult, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validateAcceptanceResultBounds(result); err != nil {
		return nil, false, err
	}
	c.acceptanceMu.Lock()
	defer c.acceptanceMu.Unlock()
	c.signalMu.Lock()
	noProgressLimit := c.noProgressLimit
	c.signalMu.Unlock()

	planSnapshot, err := c.store.GetPlan(result.PlanID)
	if err != nil {
		return nil, false, err
	}
	runSnapshot, ok := planSnapshot.AcceptanceRuns[result.RunID]
	if !ok {
		return nil, false, ErrAcceptanceRunNotFound
	}
	if runSnapshot.ResultID != "" {
		existing, exists := planSnapshot.AcceptanceResults[runSnapshot.ResultID]
		if !exists {
			return nil, false, fmt.Errorf("acceptance run %s references missing result %s", runSnapshot.ID, runSnapshot.ResultID)
		}
		cp, cloneErr := cloneJSON(existing)
		if cloneErr != nil {
			return nil, false, cloneErr
		}
		if cp.Status == model.AcceptanceResultStale {
			return &cp, false, ErrAcceptanceStale
		}
		return &cp, false, nil
	}

	var verifierErr error
	if c.verifier != nil {
		verifierErr = c.verifier.VerifyAcceptance(ctx, planSnapshot, runSnapshot, result)
	}
	var stored model.AcceptanceResult
	var postErr error
	created := false
	notify := false
	err = c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[result.PlanID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		run, ok := p.AcceptanceRuns[result.RunID]
		if !ok {
			return ErrAcceptanceRunNotFound
		}
		if run.ResultID != "" {
			stored = p.AcceptanceResults[run.ResultID]
			if stored.Status == model.AcceptanceResultStale {
				postErr = ErrAcceptanceStale
			}
			return nil
		}
		if result.ID == "" {
			result.ID = uuid.NewString()
		}
		result.PlanID = p.ID
		result.RunID = run.ID
		completedAt := time.Now().UTC()
		// G6：创建钳制（nextAcceptanceRunCreatedAt）可能让 run.CreatedAt 领先
		// 墙钟 1ns（同一时钟刻度内连续创建 run）。submission 时间必须不早于
		// run.CreatedAt，否则 result.CreatedAt 与内建 evidence 的 RecordedAt
		// 会触发 "evidence predates the acceptance run" 校验。
		if completedAt.Before(run.CreatedAt) {
			completedAt = run.CreatedAt
		}
		if result.CreatedAt.IsZero() {
			result.CreatedAt = completedAt
		}
		for i := range result.Evidence {
			if result.Evidence[i].RecordedAt.IsZero() {
				// The control plane ingestion time is the minimum reliable timestamp;
				// integration verifiers still prove the underlying file/command fact.
				result.Evidence[i].RecordedAt = result.CreatedAt
			}
		}
		staleReason := acceptanceStaleReason(p, run)
		if staleReason != "" {
			result.Status = model.AcceptanceResultStale
			result.Verdict = model.AcceptanceVerdictStale
			result.Reason = staleReason
			run.Status = "stale"
			postErr = fmt.Errorf("%w: %s", ErrAcceptanceStale, staleReason)
		} else {
			result.Status = model.AcceptanceResultValid
			spec := p.AcceptanceSpecs[run.SpecID]
			reason := applyBuiltinAcceptanceFacts(p, run, &result)
			if reason == "" {
				reason = acceptanceConstraintReason(spec, run, result)
			}
			if verifierErr != nil {
				reason = "external acceptance fact verification failed: " + verifierErr.Error()
			}
			if reason != "" {
				result.Verdict = model.AcceptanceVerdictFail
				result.Reason = reason
				postErr = fmt.Errorf("%w: %s", ErrAcceptanceConstraint, reason)
			}
			run.Status = "completed"
		}
		run.ResultID = result.ID
		run.CompletedAt = completedAt
		p.AcceptanceRuns[run.ID] = run
		p.AcceptanceResults[result.ID] = result
		p.ExecutionStateVersion++
		p.UpdatedAt = time.Now().UTC()
		reason := "acceptance_completed"
		urgency := model.ReplanUrgencyNormal
		if result.Status == model.AcceptanceResultStale {
			reason = "acceptance_stale"
			urgency = model.ReplanUrgencyHigh
		}
		_, _, _ = appendRequest(rec, model.ReplanRequest{
			PlanID: p.ID, SourceTaskID: result.SubmittedByTaskID, SourceEvent: "acceptance_result",
			ReasonCode: reason, ObservedRevision: p.CurrentRevision,
			ObservedStateVersion: p.ExecutionStateVersion, Urgency: urgency,
			IdempotencyKey: "acceptance-result:" + result.ID,
		})
		if result.Status == model.AcceptanceResultValid {
			snapshot := AcceptanceProgressSnapshot(p, result)
			snapshot.CapturedAt = completedAt
			madeProgress := MeasurableAcceptanceProgress(p.ProgressHistory, snapshot, result.Verdict)
			if applyProgressLocked(rec, p, snapshot, madeProgress, noProgressLimit) {
				notify = true
			}
		}
		notify = true
		stored = result
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if notify {
		c.notify(result.PlanID)
	}
	cp, cloneErr := cloneJSON(stored)
	if cloneErr != nil {
		return nil, created, cloneErr
	}
	return &cp, created, postErr
}

func validateAcceptanceResultBounds(result model.AcceptanceResult) error {
	if len(result.CriterionResults) > maxAcceptanceResultItems {
		return fmt.Errorf("acceptance result has %d criterion results; maximum is %d", len(result.CriterionResults), maxAcceptanceResultItems)
	}
	if len(result.Evidence) > maxAcceptanceEvidenceItems {
		return fmt.Errorf("acceptance result has %d evidence items; maximum is %d", len(result.Evidence), maxAcceptanceEvidenceItems)
	}
	if len(result.ResidualRisks) > maxAcceptanceResultItems || len(result.RecommendedActions) > maxAcceptanceResultItems {
		return fmt.Errorf("acceptance result risk/action lists exceed %d items", maxAcceptanceResultItems)
	}
	for name, value := range map[string]string{
		"id": result.ID, "run_id": result.RunID, "plan_id": result.PlanID,
		"submitted_by_task_id": result.SubmittedByTaskID,
	} {
		if len(value) > maxAcceptanceResultIDField {
			return fmt.Errorf("acceptance result %s exceeds %d bytes", name, maxAcceptanceResultIDField)
		}
	}
	if len(result.FailureFingerprint) > maxAcceptanceResultText || len(result.Reason) > maxAcceptanceResultText {
		return fmt.Errorf("acceptance result fingerprint/reason exceeds %d bytes", maxAcceptanceResultText)
	}
	for _, item := range result.CriterionResults {
		if len(item.CriterionID) > maxAcceptanceResultIDField || len(item.Summary) > maxAcceptanceResultText ||
			len(item.FailureFingerprint) > maxAcceptanceResultText {
			return fmt.Errorf("acceptance criterion result %s exceeds field limits", item.CriterionID)
		}
		if len(item.EvidenceIDs) > maxAcceptanceEvidenceIDs {
			return fmt.Errorf("acceptance criterion result %s references more than %d evidence items", item.CriterionID, maxAcceptanceEvidenceIDs)
		}
		for _, evidenceID := range item.EvidenceIDs {
			if len(evidenceID) > maxAcceptanceResultIDField {
				return fmt.Errorf("acceptance evidence id exceeds %d bytes", maxAcceptanceResultIDField)
			}
		}
	}
	for _, ev := range result.Evidence {
		if len(ev.ID) > maxAcceptanceResultIDField || len(ev.Kind) > maxAcceptanceResultIDField ||
			len(ev.FileHash) > maxAcceptanceResultIDField || len(ev.TaskID) > maxAcceptanceResultIDField {
			return fmt.Errorf("acceptance evidence %s exceeds identity field limits", ev.ID)
		}
		if len(ev.Command) > maxAcceptanceEvidenceCommand {
			return fmt.Errorf("acceptance evidence %s command exceeds %d bytes", ev.ID, maxAcceptanceEvidenceCommand)
		}
		if len(ev.Output) > maxAcceptanceEvidenceOutput {
			return fmt.Errorf("acceptance evidence %s output exceeds %d bytes", ev.ID, maxAcceptanceEvidenceOutput)
		}
		if len(ev.FilePath) > maxAcceptanceTarget {
			return fmt.Errorf("acceptance evidence %s file path exceeds %d bytes", ev.ID, maxAcceptanceTarget)
		}
	}
	for _, values := range [][]string{result.ResidualRisks, result.RecommendedActions} {
		for _, value := range values {
			if len(value) > maxAcceptanceResultText {
				return fmt.Errorf("acceptance result risk/action entry exceeds %d bytes", maxAcceptanceResultText)
			}
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode acceptance result: %w", err)
	}
	if len(encoded) > maxAcceptanceResultInputBytes {
		return fmt.Errorf("acceptance result encodes to %d bytes; maximum is %d", len(encoded), maxAcceptanceResultInputBytes)
	}
	return nil
}

func acceptanceStaleReason(p *model.Plan, run model.AcceptanceRun) string {
	if run.TargetPlanRevision != p.CurrentRevision {
		return fmt.Sprintf("target plan revision %d is not current %d", run.TargetPlanRevision, p.CurrentRevision)
	}
	if run.TargetGraphDigest != p.CurrentGraphDigest {
		return "target graph digest is not current"
	}
	if run.SpecID != p.CurrentAcceptanceSpecID || run.SpecRevision != p.CurrentAcceptanceSpecRevision {
		return "target acceptance specification is not current"
	}
	return ""
}

func acceptanceConstraintReason(spec model.AcceptanceSpec, run model.AcceptanceRun, result model.AcceptanceResult) string {
	if run.RunnerTaskID != "" && result.SubmittedByTaskID != run.RunnerTaskID {
		return "acceptance result was not submitted by the registered runner task"
	}
	switch result.Verdict {
	case model.AcceptanceVerdictPass, model.AcceptanceVerdictFail,
		model.AcceptanceVerdictBlocked, model.AcceptanceVerdictDisputed:
	default:
		return "unsupported acceptance verdict: " + string(result.Verdict)
	}
	criteria := make(map[string]model.Criterion, len(spec.Criteria))
	for _, criterion := range spec.Criteria {
		criteria[criterion.ID] = criterion
	}
	results := make(map[string]model.CriterionResult, len(result.CriterionResults))
	for _, criterionResult := range result.CriterionResults {
		if _, known := criteria[criterionResult.CriterionID]; !known {
			return "criterion result references unknown criterion: " + criterionResult.CriterionID
		}
		if _, duplicate := results[criterionResult.CriterionID]; duplicate {
			return "duplicate criterion result: " + criterionResult.CriterionID
		}
		switch criterionResult.Verdict {
		case model.AcceptanceVerdictPass, model.AcceptanceVerdictFail,
			model.AcceptanceVerdictBlocked, model.AcceptanceVerdictDisputed:
		default:
			return "unsupported criterion verdict: " + string(criterionResult.Verdict)
		}
		results[criterionResult.CriterionID] = criterionResult
	}
	evidence := make(map[string]model.Evidence, len(result.Evidence))
	for _, ev := range result.Evidence {
		if ev.ID == "" {
			return "evidence id is required"
		}
		if ev.RecordedAt.Before(run.CreatedAt) {
			return "evidence predates the acceptance run: " + ev.ID
		}
		if ev.Command != "" && ev.ExitCode == nil {
			return "command evidence lacks an exit code: " + ev.ID
		}
		if ev.FilePath != "" && ev.FileHash == "" {
			return "file evidence lacks a digest: " + ev.ID
		}
		if _, duplicate := evidence[ev.ID]; duplicate {
			return "duplicate evidence id: " + ev.ID
		}
		evidence[ev.ID] = ev
	}
	for _, criterion := range spec.Criteria {
		criterionResult, ok := results[criterion.ID]
		if criterion.Required && !ok {
			return "missing required criterion result: " + criterion.ID
		}
		if !ok {
			continue
		}
		if result.Verdict == model.AcceptanceVerdictPass && criterion.Required && criterionResult.Verdict != model.AcceptanceVerdictPass {
			return "required criterion did not pass: " + criterion.ID
		}
		if criterionResult.Verdict == model.AcceptanceVerdictPass && len(criterionResult.EvidenceIDs) == 0 {
			return "passing criterion lacks evidence: " + criterion.ID
		}
		for _, evidenceID := range criterionResult.EvidenceIDs {
			ev, ok := evidence[evidenceID]
			if !ok {
				return "criterion references missing evidence: " + evidenceID
			}
			if criterionResult.Verdict == model.AcceptanceVerdictPass {
				if !isSubstantiveEvidence(ev) {
					return "passing criterion references empty evidence: " + evidenceID
				}
				if reason := criterionEvidenceReason(criterion, ev); reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

func isSubstantiveEvidence(ev model.Evidence) bool {
	if strings.TrimSpace(ev.Kind) == "" {
		return false
	}
	if strings.TrimSpace(ev.Output) != "" {
		return true
	}
	if strings.TrimSpace(ev.Command) != "" && ev.ExitCode != nil {
		return true
	}
	return strings.TrimSpace(ev.FilePath) != "" && strings.TrimSpace(ev.FileHash) != ""
}

func criterionEvidenceReason(criterion model.Criterion, ev model.Evidence) string {
	switch criterion.Check {
	case criterionCheckCommandExit:
		if ev.Command == "" || ev.ExitCode == nil {
			return "command_exit criterion lacks command evidence: " + criterion.ID
		}
		if criterion.Target != "" && ev.Command != criterion.Target {
			return "command evidence target mismatch: " + criterion.ID
		}
		if strconv.Itoa(*ev.ExitCode) != criterion.Expected {
			return "command exit code does not match expected value: " + criterion.ID
		}
	case criterionCheckFileHash:
		if ev.FilePath == "" || ev.FileHash == "" {
			return "file_hash criterion lacks file evidence: " + criterion.ID
		}
		if criterion.Target != "" && ev.FilePath != criterion.Target {
			return "file evidence target mismatch: " + criterion.ID
		}
		if criterion.Expected != "" && !strings.EqualFold(ev.FileHash, criterion.Expected) {
			return "file hash does not match expected value: " + criterion.ID
		}
	case criterionCheckTaskStatus:
		if ev.TaskID == "" || (criterion.Target != "" && ev.TaskID != criterion.Target) || ev.Output != criterion.Expected {
			return "task status evidence does not match expected value: " + criterion.ID
		}
	case criterionCheckEvidence, criterionCheckManual, criterionCheckCurrentGraph:
		// These checks intentionally accept free-form evidence. The shared
		// substantive-evidence gate above still rejects ID-only placeholders.
	default:
		return "unsupported acceptance check: " + criterion.Check
	}
	return ""
}

func applyBuiltinAcceptanceFacts(p *model.Plan, run model.AcceptanceRun, result *model.AcceptanceResult) string {
	filteredResults := result.CriterionResults[:0]
	for _, item := range result.CriterionResults {
		if item.CriterionID != builtinCurrentGraphCriterionID {
			filteredResults = append(filteredResults, item)
		}
	}
	result.CriterionResults = filteredResults
	filteredEvidence := result.Evidence[:0]
	for _, ev := range result.Evidence {
		if !strings.HasPrefix(ev.ID, "system:task-status:") {
			filteredEvidence = append(filteredEvidence, ev)
		}
	}
	result.Evidence = filteredEvidence

	criterionResult := model.CriterionResult{CriterionID: builtinCurrentGraphCriterionID, Verdict: model.AcceptanceVerdictPass}
	if len(run.TargetTaskIDs) == 0 {
		criterionResult.Verdict = model.AcceptanceVerdictFail
		result.CriterionResults = append(result.CriterionResults, criterionResult)
		return "built-in current graph check has no target tasks"
	}
	for _, taskID := range run.TargetTaskIDs {
		node, ok := p.Nodes[taskID]
		if !ok || node.Status != model.TaskStatusCompleted {
			criterionResult.Verdict = model.AcceptanceVerdictFail
			criterionResult.FailureFingerprint = "task_not_completed:" + taskID
			result.CriterionResults = append(result.CriterionResults, criterionResult)
			return "built-in target task is not completed: " + taskID
		}
		for _, depID := range node.Dependencies {
			dep, exists := p.Nodes[depID]
			if !exists || dep.Status != model.TaskStatusCompleted {
				criterionResult.Verdict = model.AcceptanceVerdictFail
				criterionResult.FailureFingerprint = "dependency_not_completed:" + depID
				result.CriterionResults = append(result.CriterionResults, criterionResult)
				return "built-in dependency is not completed: " + depID
			}
		}
		evidenceID := "system:task-status:" + taskID
		criterionResult.EvidenceIDs = append(criterionResult.EvidenceIDs, evidenceID)
		result.Evidence = append(result.Evidence, model.Evidence{
			ID: evidenceID, Kind: "system.task_status", TaskID: taskID,
			Output: string(model.TaskStatusCompleted), RecordedAt: result.CreatedAt,
		})
	}
	result.CriterionResults = append(result.CriterionResults, criterionResult)
	return ""
}

func (c *Coordinator) Finalize(ctx context.Context, planID string, verdict model.AcceptanceVerdict) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[planID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if err := ensureControllerAuthority(ctx, p); err != nil {
			return err
		}
		if model.IsPlanTerminal(p.Status) {
			return ErrPlanTerminal
		}
		if verdict != model.AcceptanceVerdictPass {
			return fmt.Errorf("only a current PASS may finalize a Plan; verdict %s must replan, pause, or use an explicitly authorized user termination", verdict)
		}
		if !hasCurrentAcceptanceVerdict(p, model.AcceptanceVerdictPass) {
			return ErrAcceptanceNotPassed
		}
		p.Status = model.PlanStatusPassed
		p.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.store.GetPlan(planID)
}

func hasCurrentAcceptanceVerdict(p *model.Plan, verdict model.AcceptanceVerdict) bool {
	expectedTargets := currentPlanAcceptanceTargets(p)
	if len(expectedTargets) == 0 {
		return false
	}
	var latest model.AcceptanceRun
	found := false
	for _, run := range p.AcceptanceRuns {
		if acceptanceStaleReason(p, run) != "" ||
			run.Scope != model.AcceptanceScopePlan || !sameStringSet(run.TargetTaskIDs, expectedTargets) {
			continue
		}
		if !found || acceptanceRunIsLater(run, latest) {
			latest = run
			found = true
		}
	}
	if !found || latest.ResultID == "" {
		return false
	}
	if latest.RunnerTaskID != "" {
		runner, ok := p.Nodes[latest.RunnerTaskID]
		if !ok || runner.Role != model.PlanNodeRoleAcceptance || runner.Status != model.TaskStatusCompleted {
			return false
		}
	}
	result, ok := p.AcceptanceResults[latest.ResultID]
	return ok && result.Status == model.AcceptanceResultValid && result.Verdict == verdict
}

// nextAcceptanceRunCreatedAt 返回 run 的创建时间，保证严格晚于同 Plan 内
// 既有全部 run 的 CreatedAt 与 CompletedAt（必要时 +1ns）。这使 CreatedAt
// 成为 per-plan 单调递增序号并随 run 一起持久化：两个 run 的 CompletedAt
// 相同（亚毫秒连续完成）时，acceptanceRunIsLater 由创建顺序确定性决胜，
// 不再落到随机 UUID 字符串比较（G6）。实际偏移至多数纳秒，不影响观测语义。
// 修复前创建的 legacy run 不参与本钳制，其平局仍走 CreatedAt→UUID 兜底。
func nextAcceptanceRunCreatedAt(p *model.Plan, now time.Time) time.Time {
	latest := now
	for _, run := range p.AcceptanceRuns {
		if run.CreatedAt.After(latest) {
			latest = run.CreatedAt
		}
		if run.CompletedAt.After(latest) {
			latest = run.CompletedAt
		}
	}
	if !now.After(latest) {
		return latest.Add(time.Nanosecond)
	}
	return now
}

func acceptanceRunIsLater(candidate, current model.AcceptanceRun) bool {
	// 主键：完成时间（未完成回退创建时间）。
	candidateAt := candidate.CompletedAt
	if candidateAt.IsZero() {
		candidateAt = candidate.CreatedAt
	}
	currentAt := current.CompletedAt
	if currentAt.IsZero() {
		currentAt = current.CreatedAt
	}
	if !candidateAt.Equal(currentAt) {
		return candidateAt.After(currentAt)
	}
	// 次键：CreatedAt——由 nextAcceptanceRunCreatedAt 保证 per-plan 单调递增，
	// 即创建顺序决胜，确定性。
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	// 兜底：UUID 字典序——仅用于 legacy/手工构造且 CreatedAt 也相同的数据，
	// 保证确定性（同一持久化输入恒得同一结果），不承诺恢复真实创建顺序。
	return candidate.ID > current.ID
}

func currentPlanAcceptanceTargets(p *model.Plan) []string {
	var targets []string
	for _, taskID := range p.CurrentNodeIDs {
		if node, ok := p.Nodes[taskID]; ok && node.Role != model.PlanNodeRoleAcceptance {
			targets = append(targets, taskID)
		}
	}
	return sortedUniqueStrings(targets)
}

func sameStringSet(a, b []string) bool {
	a = sortedUniqueStrings(a)
	b = sortedUniqueStrings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
