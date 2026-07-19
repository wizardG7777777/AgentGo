package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/model"

	"github.com/google/uuid"
)

const (
	defaultNoProgressLimit        = 3
	maxPendingReplanRequests      = 256
	maxAcknowledgedReplanRequests = 512
	maxReplanAuditEntries         = 512
)

type Coordinator struct {
	store    *Store
	backend  TaskBackend
	verifier AcceptanceVerifier

	// authorityMu serializes controller replacement with cross-store actions
	// (for example Task cancellation) that cannot be committed in PlanStore.
	authorityMu sync.Mutex

	signalMu sync.Mutex
	signals  map[string]chan struct{}

	acceptanceMu    sync.Mutex
	noProgressLimit int
}

func NewCoordinator(store *Store, backend TaskBackend) *Coordinator {
	if store == nil {
		store = NewMemoryStore()
	}
	return &Coordinator{
		store: store, backend: backend, signals: make(map[string]chan struct{}),
		noProgressLimit: defaultNoProgressLimit,
	}
}

func (c *Coordinator) Store() *Store { return c.store }

func (c *Coordinator) SetAcceptanceVerifier(verifier AcceptanceVerifier) {
	c.acceptanceMu.Lock()
	defer c.acceptanceMu.Unlock()
	c.verifier = verifier
}

type CreateInput struct {
	PlanID     string
	RootTaskID string
	Budget     model.PlanBudget
	CreatedAt  time.Time
}

func (c *Coordinator) Create(ctx context.Context, in CreateInput) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.PlanID == "" {
		if in.RootTaskID != "" {
			in.PlanID = in.RootTaskID
		} else {
			in.PlanID = uuid.NewString()
		}
	}
	now := in.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	p := model.Plan{
		ID: in.PlanID, RootTaskID: in.RootTaskID,
		Status: model.PlanStatusRunning, ExecutionMode: model.ExecutionModeNormal,
		Nodes: make(map[string]model.PlanNode), CurrentNodeIDs: []string{},
		ActiveDecisionTaskID:  in.RootTaskID,
		AcceptanceSpecs:       make(map[string]model.AcceptanceSpec),
		AcceptanceRuns:        make(map[string]model.AcceptanceRun),
		AcceptanceResults:     make(map[string]model.AcceptanceResult),
		PendingReplanRequests: make(map[string]model.ReplanRequest),
		Budget:                in.Budget, Usage: model.BudgetUsage{StartedAt: now},
		CreatedAt: now, UpdatedAt: now,
	}
	p.CurrentGraphDigest = ComputeGraphDigest(&p)
	err := c.store.update(func(state *persistentState) error {
		if _, exists := state.Plans[p.ID]; exists {
			return ErrPlanAlreadyExists
		}
		state.Plans[p.ID] = &planRecord{Plan: p}
		normalizeRecord(state.Plans[p.ID])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.store.GetPlan(p.ID)
}

// ActivateController records the only Scheduler controller allowed to mutate
// a live Plan. Controller selection is runtime state, not a DAG revision.
func (c *Coordinator) ActivateController(ctx context.Context, planID, taskID string) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("controller task id is required")
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
		if p.Status != model.PlanStatusRunning {
			return ErrPlanPaused
		}
		if p.ActiveDecisionTaskID == taskID {
			return nil
		}
		p.ActiveDecisionTaskID = taskID
		p.ExecutionStateVersion++
		p.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.store.GetPlan(planID)
}

// WithControllerLease keeps the active controller stable while action commits
// a related fact in another store. PlanStore-only mutations should continue to
// use WithControllerAuthority and its in-transaction identity check.
func (c *Coordinator) WithControllerLease(ctx context.Context, planID, taskID string, action func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if action == nil {
		return fmt.Errorf("controller action is required")
	}
	c.authorityMu.Lock()
	defer c.authorityMu.Unlock()
	p, err := c.store.GetPlan(planID)
	if err != nil {
		return err
	}
	if p.Status != model.PlanStatusRunning {
		return ErrPlanPaused
	}
	if p.ActiveDecisionTaskID != taskID {
		return fmt.Errorf("%w: controller=%s active=%s plan=%s", ErrControllerConflict, taskID, p.ActiveDecisionTaskID, p.ID)
	}
	return action()
}

// CompleteWithoutExecution closes the control envelope used for conversation,
// status queries and read-only inspection. It is deliberately unavailable
// once a Task-backed graph or acceptance contract has been created; those
// Plans must use formal acceptance instead.
func (c *Coordinator) CompleteWithoutExecution(ctx context.Context, planID string) (*model.Plan, error) {
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
		if len(p.PendingReplanRequests) != 0 {
			return ErrPlanPendingRequests
		}
		if len(p.Nodes) != 0 || len(p.CurrentNodeIDs) != 0 ||
			p.CurrentRevision != 0 || p.Usage.TasksCreated != 0 || p.Usage.AcceptanceRuns != 0 ||
			p.CurrentAcceptanceSpecID != "" || p.CurrentAcceptanceSpecRevision != 0 {
			return fmt.Errorf("plan %s has entered execution and requires formal acceptance", p.ID)
		}
		p.Status = model.PlanStatusCompletedNoExecution
		p.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.store.GetPlan(planID)
}

type RegisterTaskInput struct {
	PlanID           string
	ObservedRevision int64
	Node             model.PlanNode
}

// RegisterTask adds one Task-backed node and advances only CurrentRevision.
// ExecutionStateVersion is unaffected until a runtime mutation is recorded.
func (c *Coordinator) RegisterTask(ctx context.Context, in RegisterTaskInput) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.Node.TaskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	var postErr error
	var notify bool
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
		if p.CurrentRevision != in.ObservedRevision {
			return fmt.Errorf("%w: observed=%d current=%d", ErrRevisionConflict, in.ObservedRevision, p.CurrentRevision)
		}
		if _, exists := p.Nodes[in.Node.TaskID]; exists {
			return ErrNodeAlreadyExists
		}
		if reason := budgetReason(p, budgetDelta{revisions: 1, tasks: 1, active: 1}); reason != "" {
			now := time.Now().UTC()
			pausePlan(p, reason, "register task rejected by budget", now)
			p.ExecutionStateVersion++
			_, _, _ = appendRequest(rec, model.ReplanRequest{
				PlanID: p.ID, SourceTaskID: in.Node.TaskID, SourceEvent: "budget",
				ReasonCode: "budget_exhausted", ObservedRevision: p.CurrentRevision,
				ObservedStateVersion: p.ExecutionStateVersion, Urgency: model.ReplanUrgencyHigh,
			})
			notify = true
			postErr = fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
			return nil
		}
		nextRevision := p.CurrentRevision + 1
		node := in.Node
		node.Dependencies = sortedUniqueStrings(node.Dependencies)
		node.Supersedes = sortedUniqueStrings(node.Supersedes)
		node.CreatedRevision = nextRevision
		if node.Status == "" {
			node.Status = model.TaskStatusPending
		}
		p.Nodes[node.TaskID] = node
		p.CurrentNodeIDs = sortedUniqueStrings(append(p.CurrentNodeIDs, node.TaskID))
		if err := validateCurrentGraph(p); err != nil {
			return err
		}
		p.CurrentRevision = nextRevision
		p.Usage.PlanRevisions++
		p.Usage.TasksCreated++
		if !model.IsTerminal(node.Status) {
			p.Usage.ActiveTasks++
		}
		p.CurrentGraphDigest = ComputeGraphDigest(p)
		now := time.Now().UTC()
		addedWarning, warningErr := appendSoftBudgetRequest(rec, p, now)
		if warningErr != nil {
			return warningErr
		}
		if addedWarning {
			notify = true
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
	plan, getErr := c.store.GetPlan(in.PlanID)
	if postErr != nil {
		return plan, postErr
	}
	return plan, getErr
}

type TaskMutation struct {
	Kind               string
	Status             model.TaskStatus
	AcceptanceRunID    string
	Summary            string
	FailureFingerprint string
	ArtifactRefs       []string
	TraceRef           string
	Wake               bool
	ReasonCode         string
	SourceEvent        string
	Urgency            model.ReplanUrgency
	IdempotencyKey     string
	OccurredAt         time.Time
}

// PlanTaskMutation 把一条 Task 事实变更绑定到目标 Plan 节点，供
// RecordTaskMutations 批量提交（C1：多条变更合并为一次落盘）。
type PlanTaskMutation struct {
	PlanID   string
	TaskID   string
	Mutation TaskMutation
}

// RecordTaskMutation advances only ExecutionStateVersion. When Wake is true,
// the associated ReplanRequest is appended in the same durable transaction.
func (c *Coordinator) RecordTaskMutation(ctx context.Context, planID, taskID string, mutation TaskMutation) (int64, error) {
	versions, errs := c.RecordTaskMutations(ctx, []PlanTaskMutation{{PlanID: planID, TaskID: taskID, Mutation: mutation}})
	if errs[0] != nil {
		return 0, errs[0]
	}
	return versions[0], nil
}

// RecordTaskMutations 与逐条 RecordTaskMutation 语义一致，但整批共享一次
// 状态克隆 + 一次原子落盘（C1 批落盘）。返回值与 mutations 对齐：
// versions[i]/errs[i] 对应第 i 条；单条失败不影响批内其余变更（该条回滚），
// 落盘本身失败时全部条目标记失败（内存状态未前进，调用方可整体重试）。
// 唤醒信号按 Plan 去重后统一发射（信号通道容量为 1，语义与逐条等价）。
func (c *Coordinator) RecordTaskMutations(ctx context.Context, mutations []PlanTaskMutation) ([]int64, []error) {
	versions := make([]int64, len(mutations))
	if err := ctx.Err(); err != nil {
		errs := make([]error, len(mutations))
		for i := range errs {
			errs[i] = err
		}
		return versions, errs
	}
	notifyFlags := make([]bool, len(mutations))
	fns := make([]func(*persistentState) error, len(mutations))
	for i, m := range mutations {
		fns[i] = func(state *persistentState) error {
			version, notify, err := applyTaskMutationOp(state, m.PlanID, m.TaskID, m.Mutation)
			if err != nil {
				return err
			}
			versions[i] = version
			notifyFlags[i] = notify
			return nil
		}
	}
	errs := c.store.updateBatch(fns...)
	notified := make(map[string]bool)
	for i := range mutations {
		if errs[i] != nil || !notifyFlags[i] || notified[mutations[i].PlanID] {
			continue
		}
		notified[mutations[i].PlanID] = true
		c.notify(mutations[i].PlanID)
	}
	return versions, errs
}

// applyTaskMutationOp 把一条 Task 事实变更应用到（已克隆的）持久化状态上。
// 成功时返回新的 ExecutionStateVersion 以及是否需要唤醒 Scheduler。
// 中途返回错误时状态可能已被部分改写，调用方（updateBatch）负责回滚。
func applyTaskMutationOp(state *persistentState, planID, taskID string, mutation TaskMutation) (int64, bool, error) {
	rec, ok := state.Plans[planID]
	if !ok {
		return 0, false, ErrPlanNotFound
	}
	p := &rec.Plan
	planTerminal := model.IsPlanTerminal(p.Status)
	node, ok := p.Nodes[taskID]
	if !ok {
		return 0, false, ErrNodeNotFound
	}
	wasTerminal := model.IsTerminal(node.Status)
	if mutation.Status != "" {
		node.Status = mutation.Status
	}
	if mutation.Summary != "" {
		node.Summary = mutation.Summary
	}
	if node.RetiredRevision == 0 {
		if mutation.FailureFingerprint != "" {
			node.FailureFingerprint = mutation.FailureFingerprint
		}
		if mutation.ArtifactRefs != nil {
			node.ArtifactRefs = sortedUniqueStrings(mutation.ArtifactRefs)
		}
		if mutation.TraceRef != "" {
			node.TraceRef = mutation.TraceRef
		}
	}
	p.Nodes[taskID] = node
	isTerminal := model.IsTerminal(node.Status)
	if isTerminal && node.Role == model.PlanNodeRoleAcceptance && mutation.AcceptanceRunID != "" {
		run, exists := p.AcceptanceRuns[mutation.AcceptanceRunID]
		if !exists {
			return 0, false, fmt.Errorf("%w: acceptance task %s references run %s", ErrAcceptanceRunNotFound, taskID, mutation.AcceptanceRunID)
		}
		if run.RunnerTaskID != taskID {
			return 0, false, fmt.Errorf("acceptance run %s belongs to runner %s, not %s", run.ID, run.RunnerTaskID, taskID)
		}
		if run.ResultID == "" && (run.Status == "pending" || run.Status == "running") {
			if node.Status == model.TaskStatusCompleted {
				run.Status = "runner_completed_without_result"
			} else {
				run.Status = "runner_" + string(node.Status)
			}
			run.CompletedAt = mutation.OccurredAt
			if run.CompletedAt.IsZero() {
				run.CompletedAt = time.Now().UTC()
			}
			p.AcceptanceRuns[run.ID] = run
			if indexedID, indexed := rec.AcceptanceRunKeys[run.Key]; indexed && indexedID == run.ID {
				delete(rec.AcceptanceRunKeys, run.Key)
			}
		} else if run.ResultID != "" && node.Status != model.TaskStatusCompleted {
			// Keep the submitted result as immutable audit evidence, but a runner
			// that failed/cancelled/blocked after submission cannot authorize Plan
			// finalization. Release the idempotency key so a fresh formal run can
			// prove the same graph/spec under a completed runner lease.
			run.Status = "runner_" + string(node.Status) + "_after_result"
			p.AcceptanceRuns[run.ID] = run
			if indexedID, indexed := rec.AcceptanceRunKeys[run.Key]; indexed && indexedID == run.ID {
				delete(rec.AcceptanceRunKeys, run.Key)
			}
		}
	}
	if !wasTerminal && isTerminal && p.Usage.ActiveTasks > 0 {
		p.Usage.ActiveTasks--
	} else if wasTerminal && !isTerminal {
		p.Usage.ActiveTasks++
	}
	p.ExecutionStateVersion++
	version := p.ExecutionStateVersion
	p.UpdatedAt = time.Now().UTC()
	notify := false
	if mutation.Wake && !planTerminal && node.RetiredRevision == 0 {
		req := model.ReplanRequest{
			PlanID: planID, SourceTaskID: taskID, SourceEvent: mutation.SourceEvent,
			ReasonCode: mutation.ReasonCode, ObservedRevision: p.CurrentRevision,
			ObservedStateVersion: version, Urgency: normalizedUrgency(mutation.Urgency),
			IdempotencyKey: mutation.IdempotencyKey, CreatedAt: mutation.OccurredAt,
		}
		if _, _, err := appendRequest(rec, req); err != nil {
			return 0, false, err
		}
		notify = true
	}
	return version, notify, nil
}

func (c *Coordinator) RequestReplan(ctx context.Context, request model.ReplanRequest) (*model.ReplanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request.ReasonCode = strings.TrimSpace(request.ReasonCode)
	if request.ReasonCode == "" {
		return nil, fmt.Errorf("replan request requires reason_code")
	}
	if len([]rune(request.ReasonCode)) > 128 || len([]rune(request.SourceEvent)) > 128 ||
		len([]rune(request.Detail)) > 2000 || len([]rune(request.IdempotencyKey)) > 512 {
		return nil, fmt.Errorf("replan request exceeds control-plane field limits")
	}
	var stored model.ReplanRequest
	var created bool
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[request.PlanID]
		if !ok {
			return ErrPlanNotFound
		}
		if model.IsPlanTerminal(rec.Plan.Status) {
			return ErrPlanTerminal
		}
		var err error
		stored, created, err = appendRequest(rec, request)
		if err != nil || !created {
			return err
		}
		// The request itself is a new durable control-plane fact. Give it a
		// unique execution-state version so a Scheduler cannot acknowledge a
		// same-version request that arrived while its LLM decision was running.
		rec.Plan.ExecutionStateVersion++
		stored.ObservedStateVersion = rec.Plan.ExecutionStateVersion
		rec.Plan.PendingReplanRequests[stored.ID] = stored
		rec.Plan.UpdatedAt = time.Now().UTC()
		return err
	})
	if err != nil {
		return nil, err
	}
	if created {
		c.notify(request.PlanID)
	}
	cp := stored
	return &cp, nil
}

func appendRequest(rec *planRecord, request model.ReplanRequest) (model.ReplanRequest, bool, error) {
	p := &rec.Plan
	if request.PlanID == "" {
		request.PlanID = p.ID
	}
	if request.PlanID != p.ID {
		return model.ReplanRequest{}, false, fmt.Errorf("request plan %s does not match %s", request.PlanID, p.ID)
	}
	if request.ObservedRevision == 0 {
		request.ObservedRevision = p.CurrentRevision
	}
	if request.ObservedStateVersion == 0 {
		request.ObservedStateVersion = p.ExecutionStateVersion
	}
	request.Urgency = normalizedUrgency(request.Urgency)
	if request.CreatedAt.IsZero() {
		request.CreatedAt = time.Now().UTC()
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = fmt.Sprintf("%s|%s|%s|%s|%d", request.PlanID,
			request.SourceTaskID, request.SourceEvent, request.ReasonCode, request.ObservedStateVersion)
	}
	if existingID, ok := rec.RequestKeyIndex[request.IdempotencyKey]; ok {
		if existing, ok := p.PendingReplanRequests[existingID]; ok {
			return existing, false, nil
		}
		if existing, ok := rec.AcknowledgedRequests[existingID]; ok {
			return existing, false, nil
		}
	}
	// A PlanSignal aggregates pending requests, so retaining an unbounded event
	// backlog adds no scheduling value. Reserve the last slot for a durable
	// overflow marker and fold further unique events into it until Scheduler
	// acknowledges the current epoch.
	if len(p.PendingReplanRequests) >= maxPendingReplanRequests-1 {
		overflowKey := fmt.Sprintf("%s|replan-overflow|%d", p.ID, p.HandledStateVersion)
		if existingID, ok := rec.RequestKeyIndex[overflowKey]; ok {
			if existing, pending := p.PendingReplanRequests[existingID]; pending {
				if urgencyRank(request.Urgency) > urgencyRank(existing.Urgency) {
					existing.Urgency = request.Urgency
					p.PendingReplanRequests[existingID] = existing
					return existing, true, nil
				}
				return existing, false, nil
			}
		}
		request.SourceTaskID = ""
		request.SourceEvent = "control_plane"
		request.ReasonCode = "replan_overflow"
		request.Detail = fmt.Sprintf("additional requests coalesced after %d pending entries", maxPendingReplanRequests-1)
		request.IdempotencyKey = overflowKey
	}
	if request.ID == "" {
		request.ID = uuid.NewString()
	}
	p.PendingReplanRequests[request.ID] = request
	rec.RequestKeyIndex[request.IdempotencyKey] = request.ID
	return request, true, nil
}

func (c *Coordinator) TrySignal(planID string) (model.PlanSignal, bool, error) {
	rec, err := c.store.viewRecord(planID)
	if err != nil {
		return model.PlanSignal{}, false, err
	}
	if len(rec.Plan.PendingReplanRequests) == 0 {
		return model.PlanSignal{}, false, nil
	}
	requests := make([]model.ReplanRequest, 0, len(rec.Plan.PendingReplanRequests))
	for _, request := range rec.Plan.PendingReplanRequests {
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].CreatedAt.Equal(requests[j].CreatedAt) {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	signal := model.PlanSignal{
		PlanID: planID, Urgency: model.ReplanUrgencyNormal,
		LatestExecutionStateVersion: rec.Plan.ExecutionStateVersion,
		CreatedAt:                   requests[0].CreatedAt,
	}
	for _, request := range requests {
		signal.RequestIDs = append(signal.RequestIDs, request.ID)
		signal.SourceTaskIDs = append(signal.SourceTaskIDs, request.SourceTaskID)
		signal.Reasons = append(signal.Reasons, request.ReasonCode)
		if urgencyRank(request.Urgency) > urgencyRank(signal.Urgency) {
			signal.Urgency = request.Urgency
		}
	}
	signal.SourceTaskIDs = sortedUniqueStrings(signal.SourceTaskIDs)
	signal.Reasons = sortedUniqueStrings(signal.Reasons)
	return signal, true, nil
}

func (c *Coordinator) NextSignal(ctx context.Context, planID string) (model.PlanSignal, error) {
	ch := c.signalChannel(planID)
	for {
		if signal, ok, err := c.TrySignal(planID); err != nil || ok {
			return signal, err
		}
		select {
		case <-ctx.Done():
			return model.PlanSignal{}, ctx.Err()
		case <-ch:
		}
	}
}

func (c *Coordinator) Acknowledge(ctx context.Context, planID string, handledStateVersion int64) error {
	return c.AcknowledgeDecision(ctx, planID, handledStateVersion, model.PlanDecisionContinueWaiting, "")
}

func (c *Coordinator) AcknowledgeDecision(ctx context.Context, planID string, handledStateVersion int64, decision model.PlanDecision, detail string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := false
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[planID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if err := ensureControllerAuthority(ctx, p); err != nil {
			return err
		}
		if handledStateVersion > p.ExecutionStateVersion {
			return fmt.Errorf("handled state version %d exceeds current %d", handledStateVersion, p.ExecutionStateVersion)
		}
		var acknowledged []string
		for id, request := range p.PendingReplanRequests {
			if request.ObservedStateVersion <= handledStateVersion {
				delete(p.PendingReplanRequests, id)
				rec.AcknowledgedRequests[id] = request
				acknowledged = append(acknowledged, id)
			}
		}
		if handledStateVersion > p.HandledStateVersion {
			p.HandledStateVersion = handledStateVersion
		}
		sort.Strings(acknowledged)
		p.ReplanAudit = append(p.ReplanAudit, model.ReplanAudit{
			At: time.Now().UTC(), Decision: decision, RequestIDs: acknowledged,
			HandledStateVersion: handledStateVersion, Detail: detail,
		})
		if len(p.ReplanAudit) > maxReplanAuditEntries {
			p.ReplanAudit = append([]model.ReplanAudit(nil), p.ReplanAudit[len(p.ReplanAudit)-maxReplanAuditEntries:]...)
		}
		pruneAcknowledgedRequests(rec)
		p.UpdatedAt = time.Now().UTC()
		remaining = len(p.PendingReplanRequests) > 0
		return nil
	})
	if err == nil && remaining {
		c.notify(planID)
	}
	return err
}

func pruneAcknowledgedRequests(rec *planRecord) {
	if len(rec.AcknowledgedRequests) <= maxAcknowledgedReplanRequests {
		return
	}
	requests := make([]model.ReplanRequest, 0, len(rec.AcknowledgedRequests))
	for _, request := range rec.AcknowledgedRequests {
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].CreatedAt.Equal(requests[j].CreatedAt) {
			return requests[i].ID < requests[j].ID
		}
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	for _, request := range requests[:len(requests)-maxAcknowledgedReplanRequests] {
		delete(rec.AcknowledgedRequests, request.ID)
		if rec.RequestKeyIndex[request.IdempotencyKey] == request.ID {
			delete(rec.RequestKeyIndex, request.IdempotencyKey)
		}
	}
}

type SupersedeInput struct {
	PlanID           string
	ObservedRevision int64
	RetireTaskIDs    []string
	ReplacementNodes []model.PlanNode
	Reason           string
}

func (c *Coordinator) ApplySupersede(ctx context.Context, in SupersedeInput) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	retired := sortedUniqueStrings(in.RetireTaskIDs)
	if len(retired) == 0 || len(in.ReplacementNodes) == 0 {
		return nil, fmt.Errorf("supersede requires retired and replacement nodes")
	}
	replacementIDs := make([]string, 0, len(in.ReplacementNodes))
	for _, replacement := range in.ReplacementNodes {
		if replacement.TaskID == "" {
			return nil, fmt.Errorf("replacement task id is required")
		}
		replacementIDs = append(replacementIDs, replacement.TaskID)
	}
	if len(sortedUniqueStrings(replacementIDs)) != len(replacementIDs) {
		return nil, fmt.Errorf("supersede replacement task ids must be unique")
	}
	if id, overlaps := firstStringSetOverlap(retired, replacementIDs); overlaps {
		return nil, fmt.Errorf("supersede retire and replacement sets overlap at %s", id)
	}
	var postErr error
	var notify bool
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
		if p.CurrentRevision != in.ObservedRevision {
			return fmt.Errorf("%w: observed=%d current=%d", ErrRevisionConflict, in.ObservedRevision, p.CurrentRevision)
		}
		current := make(map[string]bool, len(p.CurrentNodeIDs))
		for _, id := range p.CurrentNodeIDs {
			current[id] = true
		}
		for _, id := range retired {
			if !current[id] {
				return fmt.Errorf("%w: %s is not current", ErrNodeNotFound, id)
			}
		}
		if reason := budgetReason(p, budgetDelta{revisions: 1, tasks: int64(len(in.ReplacementNodes)), active: int64(len(in.ReplacementNodes))}); reason != "" {
			now := time.Now().UTC()
			pausePlan(p, reason, "supersede rejected by budget", now)
			p.ExecutionStateVersion++
			_, _, _ = appendRequest(rec, model.ReplanRequest{
				PlanID: p.ID, SourceEvent: "budget", ReasonCode: "budget_exhausted",
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency: model.ReplanUrgencyHigh,
			})
			notify = true
			postErr = fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
			return nil
		}
		nextRevision := p.CurrentRevision + 1
		for _, replacement := range in.ReplacementNodes {
			if _, exists := p.Nodes[replacement.TaskID]; exists {
				return fmt.Errorf("%w: %s", ErrNodeAlreadyExists, replacement.TaskID)
			}
			replacement.CreatedRevision = nextRevision
			replacement.Dependencies = sortedUniqueStrings(replacement.Dependencies)
			replacement.Supersedes = sortedUniqueStrings(append(replacement.Supersedes, retired...))
			if replacement.Status == "" {
				replacement.Status = model.TaskStatusPending
			}
			p.Nodes[replacement.TaskID] = replacement
			current[replacement.TaskID] = true
		}
		for _, id := range retired {
			p.Nodes[id] = compactRetiredNode(p.Nodes[id], nextRevision, replacementIDs, in.Reason)
			delete(current, id)
		}
		p.CurrentNodeIDs = p.CurrentNodeIDs[:0]
		for id := range current {
			p.CurrentNodeIDs = append(p.CurrentNodeIDs, id)
		}
		sort.Strings(p.CurrentNodeIDs)
		if err := validateCurrentGraph(p); err != nil {
			return err
		}
		p.CurrentRevision = nextRevision
		p.Usage.PlanRevisions++
		p.Usage.TasksCreated += int64(len(in.ReplacementNodes))
		p.Usage.ActiveTasks += int64(len(in.ReplacementNodes))
		p.CurrentGraphDigest = ComputeGraphDigest(p)
		now := time.Now().UTC()
		addedWarning, warningErr := appendSoftBudgetRequest(rec, p, now)
		if warningErr != nil {
			return warningErr
		}
		if addedWarning {
			notify = true
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
	plan, getErr := c.store.GetPlan(in.PlanID)
	if postErr != nil {
		return plan, postErr
	}
	return plan, getErr
}

func ensurePlanMutable(p *model.Plan) error {
	if model.IsPlanTerminal(p.Status) {
		return ErrPlanTerminal
	}
	if p.Status == model.PlanStatusPausedAwaitingDecision || p.Status == model.PlanStatusBlocked {
		return ErrPlanPaused
	}
	return nil
}

func normalizedUrgency(urgency model.ReplanUrgency) model.ReplanUrgency {
	if urgency == model.ReplanUrgencyHigh {
		return urgency
	}
	return model.ReplanUrgencyNormal
}

func urgencyRank(urgency model.ReplanUrgency) int {
	if urgency == model.ReplanUrgencyHigh {
		return 1
	}
	return 0
}

func (c *Coordinator) signalChannel(planID string) chan struct{} {
	c.signalMu.Lock()
	defer c.signalMu.Unlock()
	ch := c.signals[planID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		c.signals[planID] = ch
	}
	return ch
}

func (c *Coordinator) notify(planID string) {
	ch := c.signalChannel(planID)
	select {
	case ch <- struct{}{}:
	default:
	}
}
