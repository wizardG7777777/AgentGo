package plan

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/model"

	"github.com/google/uuid"
)

const (
	PauseResolutionContinue  = "continue"
	PauseResolutionConverge  = "converge"
	PauseResolutionTerminate = "terminate"
)

// PauseReasonPlanReview 是 PauseForReview 写入的固定 PauseReason：
// gate=plan 模式下 Scheduler 已提交执行计划，Plan 挂起等待用户通过
// Interaction 做出审阅选择。
const PauseReasonPlanReview = "plan_review"

type budgetDelta struct {
	revisions  int64
	tasks      int64
	active     int64
	acceptance int64
	tokens     int64
	cost       float64
}

func budgetReason(p *model.Plan, delta budgetDelta) string {
	return budgetReasonAt(p, delta, time.Now())
}

func budgetReasonAt(p *model.Plan, delta budgetDelta, now time.Time) string {
	b := p.Budget
	u := p.Usage
	if b.MaxPlanRevisions > 0 && u.PlanRevisions+delta.revisions > b.MaxPlanRevisions {
		return "budget_exhausted:plan_revisions"
	}
	if b.MaxTasksCreated > 0 && u.TasksCreated+delta.tasks > b.MaxTasksCreated {
		return "budget_exhausted:tasks_created"
	}
	if b.MaxActiveTasks > 0 && u.ActiveTasks+delta.active > b.MaxActiveTasks {
		return "budget_exhausted:active_tasks"
	}
	if b.MaxAcceptanceRuns > 0 && u.AcceptanceRuns+delta.acceptance > b.MaxAcceptanceRuns {
		return "budget_exhausted:acceptance_runs"
	}
	if b.MaxTokens > 0 && u.TokensUsed+delta.tokens > b.MaxTokens {
		return "budget_exhausted:tokens"
	}
	if b.MaxCost > 0 && u.CostUsed+delta.cost > b.MaxCost {
		return "budget_exhausted:cost"
	}
	if b.MaxWallTime > 0 && !u.StartedAt.IsZero() && now.Sub(u.StartedAt) > b.MaxWallTime {
		return "budget_exhausted:wall_time"
	}
	return ""
}

func pausePlan(p *model.Plan, reason, message string, now time.Time) {
	p.Status = model.PlanStatusPausedAwaitingDecision
	p.PauseReason = reason
	p.Warnings = append(p.Warnings, model.PlanWarning{Code: reason, Message: message, CreatedAt: now})
	p.UpdatedAt = now
}

func appendSoftBudgetWarnings(p *model.Plan, now time.Time) []string {
	type metric struct {
		name string
		used float64
		max  float64
	}
	metrics := []metric{
		{"plan_revisions", float64(p.Usage.PlanRevisions), float64(p.Budget.MaxPlanRevisions)},
		{"tasks_created", float64(p.Usage.TasksCreated), float64(p.Budget.MaxTasksCreated)},
		{"active_tasks", float64(p.Usage.ActiveTasks), float64(p.Budget.MaxActiveTasks)},
		{"acceptance_runs", float64(p.Usage.AcceptanceRuns), float64(p.Budget.MaxAcceptanceRuns)},
		{"tokens", float64(p.Usage.TokensUsed), float64(p.Budget.MaxTokens)},
		{"cost", p.Usage.CostUsed, p.Budget.MaxCost},
	}
	if p.Budget.MaxWallTime > 0 && !p.Usage.StartedAt.IsZero() {
		elapsed := now.Sub(p.Usage.StartedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		metrics = append(metrics, metric{
			name: "wall_time",
			used: float64(elapsed),
			max:  float64(p.Budget.MaxWallTime),
		})
	}
	existing := make(map[string]bool, len(p.Warnings))
	for _, warning := range p.Warnings {
		existing[warning.Code] = true
	}
	var added []string
	for _, metric := range metrics {
		if metric.max <= 0 || metric.used/metric.max < 0.8 || metric.used > metric.max {
			continue
		}
		code := "budget_warning:" + metric.name
		if existing[code] {
			continue
		}
		p.Warnings = append(p.Warnings, model.PlanWarning{
			Code: code, Message: fmt.Sprintf("%s budget is %.0f%% consumed", metric.name, metric.used/metric.max*100), CreatedAt: now,
		})
		added = append(added, code)
	}
	return added
}

// appendSoftBudgetRequest records newly-crossed soft limits as one durable
// Plan fact. Callers invoke it immediately after committing usage so every
// budget-consuming path has identical 80% warning semantics.
func appendSoftBudgetRequest(rec *planRecord, p *model.Plan, now time.Time) (bool, error) {
	warnings := appendSoftBudgetWarnings(p, now)
	if len(warnings) == 0 {
		return false, nil
	}
	p.ExecutionStateVersion++
	_, _, err := appendRequest(rec, model.ReplanRequest{
		PlanID: p.ID, SourceEvent: "budget", ReasonCode: "budget_warning",
		Detail: strings.Join(warnings, ","), ObservedRevision: p.CurrentRevision,
		ObservedStateVersion: p.ExecutionStateVersion, Urgency: model.ReplanUrgencyNormal,
	})
	return err == nil, err
}

func (c *Coordinator) RecordUsage(ctx context.Context, planID string, tokens int64, cost float64) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var postErr error
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
		wasRunning := p.Status == model.PlanStatusRunning
		p.Usage.TokensUsed += tokens
		p.Usage.CostUsed += cost
		p.ExecutionStateVersion++
		now := time.Now().UTC()
		// Usage may race with a user/acceptance decision that already suspended
		// the Plan. Preserve the accounting fact without overwriting that
		// higher-authority state or creating a second pause reason.
		if !wasRunning {
			p.UpdatedAt = now
			return nil
		}
		addedWarning, warningErr := appendSoftBudgetRequest(rec, p, now)
		if warningErr != nil {
			return warningErr
		}
		if addedWarning {
			notify = true
		}
		p.UpdatedAt = now
		if reason := budgetReasonAt(p, budgetDelta{}, now); reason != "" {
			pausePlan(p, reason, "execution usage reached the configured budget", now)
			_, _, _ = appendRequest(rec, model.ReplanRequest{
				PlanID: planID, SourceEvent: "budget", ReasonCode: "budget_exhausted",
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency: model.ReplanUrgencyHigh,
			})
			notify = true
			postErr = fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notify {
		c.notify(planID)
	}
	p, getErr := c.store.GetPlan(planID)
	if postErr != nil {
		return p, postErr
	}
	return p, getErr
}

// CheckBudget evaluates time-driven budget limits without fabricating usage.
// It exists primarily for a Scheduler that may spend an entire interval
// waiting for a PlanSignal: wall time must still atomically pause the Plan and
// create a durable wake request even when no Task mutation occurs.
func (c *Coordinator) CheckBudget(ctx context.Context, planID string) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Tool dispatch checks this boundary frequently. Avoid cloning, fsyncing and
	// atomically replacing the complete PlanStore when neither a hard limit nor
	// a previously-unrecorded soft warning is due. The mutating path below
	// re-evaluates under the Store writer lock before committing.
	current, err := c.store.GetPlan(planID)
	if err != nil {
		return nil, err
	}
	if current.Status != model.PlanStatusRunning {
		return current, nil
	}
	now := time.Now().UTC()
	probe := *current
	probe.Warnings = append([]model.PlanWarning(nil), current.Warnings...)
	if budgetReasonAt(current, budgetDelta{}, now) == "" && len(appendSoftBudgetWarnings(&probe, now)) == 0 {
		return current, nil
	}
	var postErr error
	var notify bool
	err = c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[planID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if p.Status != model.PlanStatusRunning {
			return nil
		}
		now := time.Now().UTC()
		if reason := budgetReasonAt(p, budgetDelta{}, now); reason != "" {
			pausePlan(p, reason, "execution reached the configured budget", now)
			p.ExecutionStateVersion++
			if _, _, err := appendRequest(rec, model.ReplanRequest{
				PlanID: planID, SourceEvent: "budget", ReasonCode: "budget_exhausted",
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency: model.ReplanUrgencyHigh,
			}); err != nil {
				return err
			}
			notify = true
			postErr = fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
			return nil
		}
		addedWarning, err := appendSoftBudgetRequest(rec, p, now)
		if err != nil {
			return err
		}
		if addedWarning {
			notify = true
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
	p, getErr := c.store.GetPlan(planID)
	if postErr != nil {
		return p, postErr
	}
	return p, getErr
}

// MarkBlocked suspends automatic execution for an external condition while
// preserving a resumable Plan and its evidence.
func (c *Coordinator) MarkBlocked(ctx context.Context, planID, reason string) (*model.Plan, error) {
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
		p.Status = model.PlanStatusBlocked
		p.PauseReason = reason
		p.ExecutionStateVersion++
		now := time.Now().UTC()
		p.Warnings = append(p.Warnings, model.PlanWarning{Code: "blocked", Message: reason, CreatedAt: now})
		_, _, _ = appendRequest(rec, model.ReplanRequest{
			PlanID: planID, SourceEvent: "scheduler_decision", ReasonCode: "blocked",
			ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
			Urgency: model.ReplanUrgencyHigh,
		})
		p.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.notify(planID)
	return c.store.GetPlan(planID)
}

// PauseForReview 把 Running 的 Plan 主动挂起为 PausedAwaitingDecision，
// PauseReason 固定为 plan_review——这是 gate=plan 模式下 Scheduler 提交执行
// 计划、等待用户审阅选择的入口。reviewText 是提交给用户审阅的计划全文，
// 随同一事务写入 Plan.Review 持久化（PlanStore 原子落盘），崩溃恢复后仍可
// 供 Interaction effect handler 读取并复制进新 controller 任务描述。
//
// 状态守卫：终态报 ErrPlanTerminal，非 Running（已暂停 / 阻塞）报
// ErrPlanPaused；重复提交由调用方（submit_plan_for_review 工具）按
// PauseReason 预读做幂等，本函数保持严格写语义。
//
// 与预算挂起不同，这里不追加 ReplanRequest 也不 notify：调用方（当前
// controller）正处于唤醒态，挂起后它的 Execute 边界检查会自行以
// ErrExecutionSuspended 收尾；恢复信号由用户在 Interaction 中选择执行后，
// 受信任控制面调用 ResolvePause 所产生的 pause_resolved 请求提供。
func (c *Coordinator) PauseForReview(ctx context.Context, planID, reason, reviewText string) (*model.Plan, error) {
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
		if p.Status != model.PlanStatusRunning {
			return ErrPlanPaused
		}
		now := time.Now().UTC()
		pausePlan(p, PauseReasonPlanReview, reason, now)
		p.Review = &model.PlanReview{
			Text:        reviewText,
			SubmittedBy: controllerAuthorityFrom(ctx),
			SubmittedAt: now,
		}
		p.ExecutionStateVersion++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.store.GetPlan(planID)
}

type ResolvePauseInput struct {
	PlanID               string
	Resolution           string
	Override             model.ExecutionOverride
	AuthorizedBy         string
	Reason               string
	NextControllerTaskID string
	// ExpectedPauseReason / ExpectedStateVersion 把一次用户决定绑定到创建
	// Interaction 时看到的暂停事实。零值保持内部兼容调用的既有语义；控制面
	// 回答必须同时填写，校验在 PlanStore 写事务内完成，避免先读后写竞态。
	ExpectedPauseReason  string
	ExpectedStateVersion int64
}

func (c *Coordinator) ResolvePause(ctx context.Context, in ResolvePauseInput) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.AuthorizedBy) == "" {
		return nil, fmt.Errorf("pause resolution requires explicit user authorization")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, fmt.Errorf("pause resolution requires an explicit user reason")
	}
	if (in.Resolution == PauseResolutionContinue || in.Resolution == PauseResolutionConverge) &&
		strings.TrimSpace(in.NextControllerTaskID) == "" {
		return nil, fmt.Errorf("pause resolution requires a reserved next controller task id")
	}
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	notify := false
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[in.PlanID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if in.ExpectedPauseReason != "" && p.PauseReason != in.ExpectedPauseReason {
			return fmt.Errorf("%w: pause reason=%q, expected %q",
				ErrPauseConflict, p.PauseReason, in.ExpectedPauseReason)
		}
		if in.ExpectedStateVersion > 0 && p.ExecutionStateVersion != in.ExpectedStateVersion {
			return fmt.Errorf("%w: execution state version=%d, expected %d",
				ErrPauseConflict, p.ExecutionStateVersion, in.ExpectedStateVersion)
		}
		if p.Status != model.PlanStatusPausedAwaitingDecision && p.Status != model.PlanStatusBlocked {
			return ErrPlanPaused
		}
		now := time.Now().UTC()
		override := in.Override
		if override.ID == "" {
			override.ID = uuid.NewString()
		}
		if override.CreatedAt.IsZero() {
			override.CreatedAt = now
		}
		override.Resolution = in.Resolution
		override.AuthorizedBy = in.AuthorizedBy
		override.Reason = in.Reason
		switch in.Resolution {
		case PauseResolutionContinue, PauseResolutionConverge:
			p.Budget.MaxTasksCreated = addLimit(p.Budget.MaxTasksCreated, override.AddedTasks)
			p.Budget.MaxActiveTasks = addLimit(p.Budget.MaxActiveTasks, override.AddedActiveTasks)
			p.Budget.MaxPlanRevisions = addLimit(p.Budget.MaxPlanRevisions, override.AddedPlanRevisions)
			p.Budget.MaxAcceptanceRuns = addLimit(p.Budget.MaxAcceptanceRuns, override.AddedAcceptanceRuns)
			p.Budget.MaxTokens = addLimit(p.Budget.MaxTokens, override.AddedTokens)
			p.Budget.MaxWallTime = addDurationLimit(p.Budget.MaxWallTime, override.AddedTime)
			p.Budget.MaxCost = addFloatLimit(p.Budget.MaxCost, override.AddedCost)
			p.Overrides = append(p.Overrides, override)
			p.Status = model.PlanStatusRunning
			// Transfer authority in the same durable transaction that resumes the
			// Plan. The previous controller can never regain a runnable window.
			p.ActiveDecisionTaskID = strings.TrimSpace(in.NextControllerTaskID)
			p.ExecutionMode = model.ExecutionModeNormal
			if in.Resolution == PauseResolutionConverge {
				p.ExecutionMode = model.ExecutionModeConverge
			}
			p.PauseReason = ""
			p.ExecutionStateVersion++
			_, _, _ = appendRequest(rec, model.ReplanRequest{
				PlanID: p.ID, SourceEvent: "pause_resolved", ReasonCode: "pause_resolved",
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency: model.ReplanUrgencyNormal,
			})
			notify = true
		case PauseResolutionTerminate:
			// Termination applies no budget increase, but the authorization record
			// is still durable so audit can prove who chose it and why.
			override.AddedTasks = 0
			override.AddedActiveTasks = 0
			override.AddedPlanRevisions = 0
			override.AddedAcceptanceRuns = 0
			override.AddedTokens = 0
			override.AddedTime = 0
			override.AddedCost = 0
			p.Overrides = append(p.Overrides, override)
			p.Status = model.PlanStatusCancelledByUser
			p.PauseReason = ""
			p.ExecutionStateVersion++
		default:
			return ErrInvalidPauseResolution
		}
		p.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notify {
		c.notify(in.PlanID)
	}
	return c.store.GetPlan(in.PlanID)
}

// TerminatePlan moves a non-terminal Plan (Running / PausedAwaitingDecision /
// Blocked) directly to CancelledByUser. It backs the user-driven "cancel the
// latest request tree" path, where no controller is available to resolve a
// pause. Like the ResolvePause terminate branch, termination applies no budget
// increase, but the authorization record (AuthorizedBy / Reason) is still
// durable so audit can prove who chose it and why; both are required.
//
// No notify is sent: TrySignal only yields a signal while PendingReplanRequests
// is non-empty, so a bare notify would wake NextSignal into an immediate
// re-sleep. Prompt wakeup of a waiting controller comes from cancelling the
// controller Task itself (per-task cancel ctx fires NextSignal's ctx.Done),
// which is the caller's job; the timeout heartbeat in waitForPlanSignal is the
// fallback. This mirrors the ResolvePause terminate branch, which also commits
// no wake request.
func (c *Coordinator) TerminatePlan(ctx context.Context, planID, authorizedBy, reason string) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(authorizedBy) == "" {
		return nil, fmt.Errorf("plan termination requires explicit user authorization")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("plan termination requires an explicit user reason")
	}
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
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
		// Termination applies no budget increase, but the authorization record
		// is still durable so audit can prove who chose it and why.
		p.Overrides = append(p.Overrides, model.ExecutionOverride{
			ID:           uuid.NewString(),
			Resolution:   PauseResolutionTerminate,
			AuthorizedBy: authorizedBy,
			Reason:       reason,
			CreatedAt:    now,
		})
		p.Status = model.PlanStatusCancelledByUser
		p.PauseReason = ""
		p.ExecutionStateVersion++
		p.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.store.GetPlan(planID)
}

func addLimit(current, added int64) int64 {
	if added <= 0 {
		return current
	}
	if current == 0 {
		return added
	}
	return current + added
}

func addDurationLimit(current, added time.Duration) time.Duration {
	if added <= 0 {
		return current
	}
	if current == 0 {
		return added
	}
	return current + added
}

func addFloatLimit(current, added float64) float64 {
	if added <= 0 {
		return current
	}
	if current == 0 {
		return added
	}
	return current + added
}

func (c *Coordinator) SetNoProgressLimit(limit int) {
	if limit <= 0 {
		limit = defaultNoProgressLimit
	}
	c.signalMu.Lock()
	c.noProgressLimit = limit
	c.signalMu.Unlock()
}

func (c *Coordinator) RecordProgress(ctx context.Context, planID string, snapshot model.ProgressSnapshot, madeProgress bool) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.signalMu.Lock()
	limit := c.noProgressLimit
	c.signalMu.Unlock()
	notify := false
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[planID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if model.IsPlanTerminal(p.Status) {
			return ErrPlanTerminal
		}
		// Generic callers may supply their own progress decision. Formal
		// acceptance uses SubmitAcceptanceResult, which evaluates and appends
		// against current history inside its own atomic transaction.
		notify = applyProgressLocked(rec, p, snapshot, madeProgress, limit)
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
