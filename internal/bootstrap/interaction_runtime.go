package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/ui"
)

const (
	purposePlanReview interaction.Purpose = "plan_review"
	purposePlanPause  interaction.Purpose = "plan_pause"

	planInteractionHandler = "plan_control"

	actionPlanExecute   = "plan.execute"
	actionPlanRevise    = "plan.revise"
	actionPlanCancel    = "plan.cancel"
	actionPlanContinue  = "plan.continue"
	actionPlanConverge  = "plan.converge"
	actionPlanTerminate = "plan.terminate"
)

var errStaleInteraction = errors.New("interaction no longer matches runtime state")

// startInteractionRuntime reconciles durable Plan pauses into Interaction
// requests. SessionID records where a request was created, but pending requests
// stay process-visible because AgentGo runtime work survives Session switches.
// PlanStore remains the execution authority; Interaction is only the authority
// for choosing among the offered control-plane actions.
func (s *System) startInteractionRuntime(ctx context.Context) {
	if s == nil || s.Interactions == nil || s.PlanCoordinator == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := s.reconcilePlanInteractions(ctx); err != nil && ctx.Err() == nil {
				s.emitInteractionStatus(fmt.Sprintf("[interaction] Plan 交互同步失败: %v", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *System) currentSessionID() string {
	if s == nil || s.SessionMgr == nil {
		return ""
	}
	current := s.SessionMgr.Current()
	if current == nil {
		return ""
	}
	return current.Metadata.SessionID
}

func (s *System) reconcilePlanInteractions(ctx context.Context) error {
	plans, err := s.PlanCoordinator.Store().ListPlans()
	if err != nil {
		return err
	}
	sessionID := s.currentSessionID()
	modesSnapshot := modes.Snapshot{}
	if s.Scheduler != nil && s.Scheduler.Modes != nil {
		modesSnapshot = s.Scheduler.Modes.Snapshot()
	}

	eligible := make(map[string]model.Plan)
	for _, p := range plans {
		if p.Status == model.PlanStatusPausedAwaitingDecision || p.Status == model.PlanStatusBlocked {
			eligible[p.ID] = p
		}
	}

	existing, err := s.Interactions.List(ctx, interaction.Filter{
		States: []interaction.State{interaction.StatePending, interaction.StateResolving}})
	if err != nil {
		return err
	}
	for _, request := range existing {
		if request.Resolution.Handler != planInteractionHandler {
			continue
		}
		p, ok := eligible[request.Subject.PlanID]
		if request.State == interaction.StateResolving {
			// A response owns the effect while resolving. The Plan CAS below decides
			// whether it is stale; reconciliation must not race-cancel it.
			continue
		}
		if !ok || !planRequestMatches(request, p, modesSnapshot) {
			_, cancelErr := s.Interactions.Cancel(ctx, request.ID, request.Version,
				"Plan 或工作模式已变化；旧问题不再有效")
			if cancelErr != nil && !errors.Is(cancelErr, interaction.ErrVersionConflict) &&
				!errors.Is(cancelErr, interaction.ErrInvalidTransition) {
				return cancelErr
			}
		}
	}

	for _, p := range eligible {
		request := buildPlanInteraction(p, sessionID, modesSnapshot)
		current, getErr := s.Interactions.Get(ctx, request.ID)
		if getErr == nil {
			if current.State == interaction.StatePending || current.State == interaction.StateResolving {
				continue
			}
			// A terminal request with the same bound facts must not be recreated.
			// Successful effects change Plan state/version; mode drift changes the ID.
			continue
		}
		if !errors.Is(getErr, interaction.ErrNotFound) {
			return getErr
		}
		if _, createErr := s.Interactions.Create(ctx, request); createErr != nil &&
			!errors.Is(createErr, interaction.ErrDuplicateID) {
			return createErr
		}
	}
	return nil
}

func planRequestMatches(request interaction.Request, p model.Plan, snapshot modes.Snapshot) bool {
	return request.Subject.Version == p.ExecutionStateVersion &&
		request.Subject.Digest == planInteractionBindingDigest(p, snapshot) &&
		request.Metadata["pause_reason"] == p.PauseReason &&
		request.Metadata["gate_mode"] == snapshot.Gate &&
		request.Metadata["exec_mode"] == snapshot.Exec &&
		request.Metadata["topo_mode"] == snapshot.Topo
}

// planInteractionBindingDigest binds the answer to every Plan fact that can
// change the meaning of the offered choices. In particular, Plan.Review.Text
// is covered independently of the execution graph digest.
func planInteractionBindingDigest(p model.Plan, snapshot modes.Snapshot) string {
	reviewText := ""
	if p.Review != nil {
		reviewText = p.Review.Text
	}
	payload := strings.Join([]string{
		p.ID,
		strconv.FormatInt(p.ExecutionStateVersion, 10),
		strconv.FormatInt(p.CurrentRevision, 10),
		p.CurrentGraphDigest,
		p.PauseReason,
		snapshot.Gate,
		snapshot.Exec,
		snapshot.Topo,
		reviewText,
	}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func buildPlanInteraction(p model.Plan, sessionID string, snapshot modes.Snapshot) interaction.CreateRequest {
	bindingDigest := planInteractionBindingDigest(p, snapshot)
	purpose := purposePlanPause
	prompt := fmt.Sprintf("Plan %s 已暂停，需要你的决定。\n原因：%s", p.ID, p.PauseReason)
	options := []interaction.Option{
		{ID: "continue_bounded", Label: "限额继续", Description: "按当前非零预算的 25% 增加一次有界额度", ActionRef: actionPlanContinue},
		{ID: "converge_delivery", Label: "收敛交付", Description: "增加同样的有界额度，但要求只完成最小可验收交付", ActionRef: actionPlanConverge},
		{ID: "terminate_plan", Label: "终止请求", Description: "终止 Plan 并取消剩余任务", ActionRef: actionPlanTerminate},
	}
	allowText := false
	if p.PauseReason == plan.PauseReasonPlanReview {
		purpose = purposePlanReview
		planText := "（计划正文不可用）"
		if p.Review != nil && strings.TrimSpace(p.Review.Text) != "" {
			planText = p.Review.Text
		}
		prompt = fmt.Sprintf("Scheduler 已提交 Plan %s，请审阅后选择下一步。\n\n%s", p.ID, planText)
		options = []interaction.Option{
			{ID: "execute_plan", Label: "执行计划", Description: "按当前模式执行这份计划", ActionRef: actionPlanExecute},
			{ID: "revise_plan", Label: "要求修改", Description: "附上修改意见并让 Scheduler 重新提交", RequiresText: true, ActionRef: actionPlanRevise},
			{ID: "cancel_request", Label: "取消请求", Description: "终止 Plan 并取消剩余任务", ActionRef: actionPlanCancel},
		}
		allowText = true
	}

	return interaction.CreateRequest{
		ID:            fmt.Sprintf("plan_%s_%d_%s", p.ID, p.ExecutionStateVersion, bindingDigest[:16]),
		SessionID:     sessionID,
		Kind:          interaction.KindChoice,
		Purpose:       purpose,
		Prompt:        prompt,
		Options:       options,
		AllowFreeText: allowText,
		Origin:        interaction.Origin{Component: "scheduler", TaskID: p.ActiveDecisionTaskID},
		Subject: interaction.Subject{Kind: "plan", ID: p.ID, PlanID: p.ID,
			TaskID: p.ActiveDecisionTaskID, Version: p.ExecutionStateVersion, Digest: bindingDigest},
		Resolution: interaction.ResolutionSpec{Handler: planInteractionHandler, TargetID: p.ID,
			PlanID: p.ID, TaskID: p.ActiveDecisionTaskID, EventType: "__scheduler__"},
		Metadata: map[string]string{
			"pause_reason":  p.PauseReason,
			"plan_revision": strconv.FormatInt(p.CurrentRevision, 10),
			"graph_digest":  p.CurrentGraphDigest,
			"gate_mode":     snapshot.Gate,
			"exec_mode":     snapshot.Exec,
			"topo_mode":     snapshot.Topo,
		},
	}
}

// resolveInteraction is the single trusted response router used by TUI/Web.
// BeginResolve provides first-writer-wins; the Plan handler then performs its
// own expected-reason/version CAS before Complete makes the answer terminal.
func (s *System) resolveInteraction(ctx context.Context, input interaction.ResolveInput) (interaction.Request, error) {
	if s == nil || s.Interactions == nil {
		return interaction.Request{}, fmt.Errorf("interaction service is not initialized")
	}
	locked, err := s.Interactions.BeginResolve(ctx, input)
	if err != nil {
		return interaction.Request{}, err
	}
	if locked.State == interaction.StateResolved {
		return locked, nil
	}
	if locked.State != interaction.StateResolving {
		return interaction.Request{}, fmt.Errorf("%w: state=%s", interaction.ErrInvalidTransition, locked.State)
	}

	var effectErr error
	switch locked.Resolution.Handler {
	case planInteractionHandler:
		effectErr = s.applyPlanInteraction(ctx, locked)
	case shell.ResolutionHandlerShellCommand:
		// The waiting Shell adapter owns the captured command/filter effect. The
		// control plane only locks the answer and marks it ready for Await.
		effectErr = nil
	case tools.ResolutionHandlerFileWrite:
		// 与 shell_command 相同：服务端零 effect。strict 模式下写文件副作用由
		// 等待中的工具 wrapper（tools.FileWriteApprover）持有并在 Await 返回后
		// 复核绑定；控制面只锁定回答并 Complete。
		effectErr = nil
	case tools.ResolutionHandlerAgentResponse:
		// Ordinary Agent questions have no privileged control-plane effect. The
		// waiting tool receives the validated answer after Complete. Shell
		// authorization and Plan control keep their dedicated trusted handlers.
		effectErr = nil
	default:
		effectErr = fmt.Errorf("unknown interaction handler %q", locked.Resolution.Handler)
	}
	if effectErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if errors.Is(effectErr, errStaleInteraction) || errors.Is(effectErr, plan.ErrPauseConflict) ||
			errors.Is(effectErr, plan.ErrPlanPaused) || errors.Is(effectErr, plan.ErrPlanTerminal) {
			failed, failErr := s.Interactions.Fail(cleanupCtx, locked.ID, locked.Version, effectErr.Error())
			if failErr != nil {
				return interaction.Request{}, errors.Join(effectErr, failErr)
			}
			return failed, effectErr
		}
		released, releaseErr := s.Interactions.Release(cleanupCtx, locked.ID, locked.Version, effectErr.Error())
		if releaseErr != nil {
			return interaction.Request{}, errors.Join(effectErr, releaseErr)
		}
		return released, effectErr
	}
	// The effect is already committed. Do not let a disconnected HTTP client or
	// closing TUI strand the request in resolving and leave a Shell waiter stuck.
	completeCtx, cancelComplete := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelComplete()
	completed, err := s.Interactions.Complete(completeCtx, locked.ID, locked.Version)
	if err != nil {
		return interaction.Request{}, err
	}
	return completed, nil
}

func (s *System) applyPlanInteraction(ctx context.Context, request interaction.Request) error {
	if s.PlanCoordinator == nil || s.Store == nil || s.Scheduler == nil || s.Scheduler.Modes == nil {
		return fmt.Errorf("plan interaction dependencies are not initialized")
	}
	option, ok := request.SelectedOption()
	if !ok {
		return fmt.Errorf("%w: selected option missing", interaction.ErrInvalidOption)
	}
	expectedModes := modes.Snapshot{
		Gate: request.Metadata["gate_mode"],
		Exec: request.Metadata["exec_mode"],
		Topo: request.Metadata["topo_mode"],
	}
	// 模式 setter 与这段 effect 共用 modes.Store 的串行化边界。
	// 这不只是“提交前再读一次”：从校验到 PlanStore/TaskStore
	// 提交完成期间，三轴都不可以并发漂移。
	err := s.Scheduler.Modes.WithSnapshot(expectedModes, func() error {
		p, err := s.PlanCoordinator.Store().GetPlan(request.Subject.PlanID)
		if err != nil {
			return err
		}
		if !planRequestMatches(request, *p, expectedModes) {
			return fmt.Errorf("%w: plan %s changed after interaction creation", errStaleInteraction, p.ID)
		}
		topo, err := modes.ParseTopoMode(expectedModes.Topo)
		if err != nil {
			return err
		}
		schedulerAgentID := ""
		if s.Scheduler.Agent != nil {
			schedulerAgentID = s.Scheduler.Agent.ID
		}

		var summary string
		switch option.ActionRef {
		case actionPlanExecute:
			summary, err = approvePlanReviewRequest(ctx, s.Store, s.PlanCoordinator, schedulerAgentID,
				p.ID, request.Metadata["pause_reason"], request.Subject.Version, topo)
		case actionPlanRevise:
			summary, err = revisePlanReviewRequest(ctx, s.Store, s.PlanCoordinator, schedulerAgentID,
				p, request.Metadata["pause_reason"], request.Subject.Version, request.Response.Text)
		case actionPlanCancel:
			summary, err = rejectPlanReviewRequest(ctx, s.Store, s.PlanCoordinator,
				p.ID, request.Metadata["pause_reason"], request.Subject.Version)
		case actionPlanContinue:
			summary, err = resumePausedPlanRequest(ctx, s.Store, s.PlanCoordinator, schedulerAgentID,
				p, request.Metadata["pause_reason"], request.Subject.Version, plan.PauseResolutionContinue, topo)
		case actionPlanConverge:
			summary, err = resumePausedPlanRequest(ctx, s.Store, s.PlanCoordinator, schedulerAgentID,
				p, request.Metadata["pause_reason"], request.Subject.Version, plan.PauseResolutionConverge, topo)
		case actionPlanTerminate:
			summary, err = terminatePausedPlanRequest(ctx, s.Store, s.PlanCoordinator,
				p.ID, request.Metadata["pause_reason"], request.Subject.Version)
		default:
			err = fmt.Errorf("unknown plan action %q", option.ActionRef)
		}
		if err == nil && summary != "" {
			s.emitInteractionStatus("[interaction] " + summary)
		}
		return err
	})
	if errors.Is(err, modes.ErrSnapshotChanged) {
		return fmt.Errorf("%w: %v", errStaleInteraction, err)
	}
	return err
}

func revisePlanReviewRequest(ctx context.Context, taskStore store.TaskStore, coordinator *plan.Coordinator,
	schedulerAgentID string, p *model.Plan, expectedReason string, expectedVersion int64, feedback string) (string, error) {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return "", fmt.Errorf("修改计划必须提供反馈")
	}
	resume := reservedPlanController(p, schedulerAgentID,
		fmt.Sprintf("【plan-gate】用户要求修改 Plan %s。请根据以下反馈修订计划；不要发布任何 implementation 任务。修订完成后再次调用 submit_plan_for_review，等待新的 Interaction。\n\n--- 用户反馈 ---\n%s\n--- 反馈结束 ---", p.ID, feedback))
	if err := taskStore.PublishTask(resume); err != nil {
		return "", fmt.Errorf("预发布计划修订 controller 失败: %w", err)
	}
	updated, err := coordinator.ResolvePause(ctx, plan.ResolvePauseInput{
		PlanID: p.ID, Resolution: plan.PauseResolutionContinue,
		AuthorizedBy: "user", Reason: "plan-revision-requested",
		NextControllerTaskID: resume.ID, ExpectedPauseReason: expectedReason,
		ExpectedStateVersion: expectedVersion,
	})
	if err != nil {
		_ = store.TransitionStateWithCancelSource(taskStore, resume.ID, model.TaskStatusPending, model.TaskStatusCancelled, "system")
		return "", err
	}
	return fmt.Sprintf("已要求修改 Plan %s；修订 controller %s 已创建", updated.ID, resume.ID[:8]), nil
}

func resumePausedPlanRequest(ctx context.Context, taskStore store.TaskStore, coordinator *plan.Coordinator,
	schedulerAgentID string, p *model.Plan, expectedReason string, expectedVersion int64,
	resolution string, topo modes.TopoMode) (string, error) {
	instruction := "继续协调团队完成剩余工作"
	if topo == modes.TopoSolo {
		instruction = "不要调用 publish_task；直接使用可用工具亲自完成剩余工作"
	}
	if resolution == plan.PauseResolutionConverge {
		instruction += "，只做最小可验收交付并尽快进入正式验收"
	}
	resume := reservedPlanController(p, schedulerAgentID,
		fmt.Sprintf("用户通过 Interaction 选择 %s Plan %s。%s。原暂停原因：%s",
			resolution, p.ID, instruction, expectedReason))
	if err := taskStore.PublishTask(resume); err != nil {
		return "", fmt.Errorf("预发布恢复 controller 失败: %w", err)
	}
	override := boundedPlanContinuation(p.Budget)
	updated, err := coordinator.ResolvePause(ctx, plan.ResolvePauseInput{
		PlanID: p.ID, Resolution: resolution, Override: override,
		AuthorizedBy: "user", Reason: "interaction-" + resolution,
		NextControllerTaskID: resume.ID, ExpectedPauseReason: expectedReason,
		ExpectedStateVersion: expectedVersion,
	})
	if err != nil {
		_ = store.TransitionStateWithCancelSource(taskStore, resume.ID, model.TaskStatusPending, model.TaskStatusCancelled, "system")
		return "", err
	}
	return fmt.Sprintf("Plan %s 已按 %s 恢复；controller %s 已创建", updated.ID, resolution, resume.ID[:8]), nil
}

func terminatePausedPlanRequest(ctx context.Context, taskStore store.TaskStore, coordinator *plan.Coordinator,
	planID, expectedReason string, expectedVersion int64) (string, error) {
	updated, err := coordinator.ResolvePause(ctx, plan.ResolvePauseInput{
		PlanID: planID, Resolution: plan.PauseResolutionTerminate,
		AuthorizedBy: "user", Reason: "interaction-terminate",
		ExpectedPauseReason: expectedReason, ExpectedStateVersion: expectedVersion,
	})
	if err != nil {
		return "", err
	}
	cancelled, err := cancelPlanTasks(taskStore, planID)
	if err != nil {
		return fmt.Sprintf("Plan %s 已终止；已取消 %d 个任务，部分任务扫尾未完成（%v），Plan 终态仍已生效",
			updated.ID, cancelled, err), nil
	}
	return fmt.Sprintf("Plan %s 已终止，共取消 %d 个任务", updated.ID, cancelled), nil
}

func reservedPlanController(p *model.Plan, schedulerAgentID, description string) *model.Task {
	return &model.Task{
		ID: uuid.NewString(), PlanID: p.ID, NodeRole: model.PlanNodeRoleController,
		PlanMutationSource: "control-reserved", EventType: "__scheduler__", EventSource: "user",
		ParentTaskID: p.RootTaskID, ReplyToAgentID: schedulerAgentID, BatchID: p.RootTaskID,
		Priority: 100, TimeoutSeconds: scheduler.SchedulerTaskTimeoutSec,
		MaxConcurrency: 1, Description: description,
	}
}

func boundedPlanContinuation(budget model.PlanBudget) model.ExecutionOverride {
	quarterInt := func(value int64) int64 {
		if value <= 0 {
			return 0 // zero means unlimited; never turn it into a finite limit
		}
		return max(int64(1), value/4)
	}
	addedTime := time.Duration(0)
	if budget.MaxWallTime > 0 {
		addedTime = budget.MaxWallTime / 4
		if addedTime < time.Minute {
			addedTime = time.Minute
		}
	}
	addedCost := float64(0)
	if budget.MaxCost > 0 {
		addedCost = math.Max(0.01, budget.MaxCost/4)
	}
	return model.ExecutionOverride{
		AddedTasks: quarterInt(budget.MaxTasksCreated), AddedActiveTasks: quarterInt(budget.MaxActiveTasks),
		AddedPlanRevisions: quarterInt(budget.MaxPlanRevisions), AddedAcceptanceRuns: quarterInt(budget.MaxAcceptanceRuns),
		AddedTokens: quarterInt(budget.MaxTokens), AddedTime: addedTime, AddedCost: addedCost,
	}
}

func cancelPlanTasks(taskStore store.TaskStore, planID string) (int, error) {
	tasks, err := taskStore.ScanAll()
	if err != nil {
		return 0, fmt.Errorf("读取任务列表失败: %w", err)
	}
	cancelled := 0
	var cleanupErr error
	for _, task := range tasks {
		if task == nil || task.PlanID != planID || model.IsTerminal(task.Status) {
			continue
		}
		if err := cancelTaskTwoPhase(taskStore, task.ID, "user"); err == nil {
			cancelled++
		} else {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("取消任务 %s: %w", task.ID, err))
		}
	}
	return cancelled, cleanupErr
}

func (s *System) emitInteractionStatus(message string) {
	if s == nil || s.StatusCh == nil || strings.TrimSpace(message) == "" {
		return
	}
	select {
	case s.StatusCh <- message:
	default:
	}
}

func (s *System) interruptPendingInteractions(reason string) {
	if s == nil || s.Interactions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	requests, err := s.Interactions.List(ctx, interaction.Filter{States: []interaction.State{interaction.StatePending}})
	if err != nil {
		return
	}
	for _, request := range requests {
		_, _ = s.Interactions.Interrupt(ctx, request.ID, request.Version, reason)
	}
}

// respondPlanReviewByPrefix keeps /plan approve|reject as a compatibility
// command, but routes it through the same Interaction CAS/effect pipeline used
// by TUI buttons and Web. It is not a second Plan authorization path.
func (s *System) respondPlanReviewByPrefix(ctx context.Context, idPrefix, optionID string) (string, error) {
	if err := s.reconcilePlanInteractions(ctx); err != nil {
		return "", err
	}
	request, err := s.resolvePlanReviewInteractionByPrefix(ctx, idPrefix)
	if err != nil {
		return "", err
	}
	resolved, err := s.resolveInteraction(ctx, interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version,
		OptionID: optionID, RespondedBy: "command",
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Interaction %s 已完成：%s", resolved.ID, optionID), nil
}

func (s *System) resolvePlanReviewInteractionByPrefix(ctx context.Context, idPrefix string) (interaction.Request, error) {
	if s == nil || s.Interactions == nil {
		return interaction.Request{}, fmt.Errorf("interaction service is not initialized")
	}
	requests, err := s.Interactions.List(ctx, interaction.Filter{
		Purpose: purposePlanReview,
		States:  []interaction.State{interaction.StatePending},
	})
	if err != nil {
		return interaction.Request{}, err
	}
	if idPrefix != "" && len(idPrefix) < 4 {
		return interaction.Request{}, fmt.Errorf("Plan ID 前缀过短（至少 4 个字符）: %s", idPrefix)
	}
	matches := make([]interaction.Request, 0, len(requests))
	for _, request := range requests {
		if idPrefix == "" || strings.HasPrefix(request.Subject.PlanID, idPrefix) {
			matches = append(matches, request)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return interaction.Request{}, fmt.Errorf("当前没有匹配的待审计划 Interaction")
	}
	ids := make([]string, 0, len(matches))
	for _, request := range matches {
		ids = append(ids, request.Subject.PlanID)
	}
	sort.Strings(ids)
	return interaction.Request{}, fmt.Errorf("有 %d 个匹配的待审计划，请指定更长的 Plan ID 前缀:\n  %s",
		len(ids), strings.Join(ids, "\n  "))
}

func (s *System) pendingPlanReviewsFromInteractions(ctx context.Context) ([]ui.PlanReviewItem, error) {
	if s == nil || s.Interactions == nil {
		return nil, fmt.Errorf("interaction service is not initialized")
	}
	if err := s.reconcilePlanInteractions(ctx); err != nil {
		return nil, err
	}
	requests, err := s.Interactions.List(ctx, interaction.Filter{
		Purpose: purposePlanReview,
		States:  []interaction.State{interaction.StatePending},
	})
	if err != nil {
		return nil, err
	}
	items := make([]ui.PlanReviewItem, 0, len(requests))
	for _, request := range requests {
		items = append(items, ui.PlanReviewItem{
			PlanID: request.Subject.PlanID, SubmittedAt: request.CreatedAt,
			Excerpt: truncateRunes(request.Prompt, planReviewExcerptRunes),
		})
	}
	return items, nil
}
