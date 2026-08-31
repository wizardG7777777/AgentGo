package watchdog

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/loopcontract"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
)

// PlanRouteRegistry is the smallest runtime-routing authority Watchdog needs.
// scheduler.AgentRegistry satisfies it without making watchdog depend on the
// scheduler package.
//
// ownerScope is the same namespaced task/Graph ownership used by publish-time
// validation. Watchdog must not pass an empty scope for dynamic Teams.
type PlanRouteRegistry interface {
	CanRouteForPlan(ownerScope, eventType string, requiredTools ...string) bool
}

// RouteResolver answers whether a pending Task has a runtime listener that may
// claim it. It intentionally reports route existence only; worker busy/idle
// capacity is not inferred here.
type RouteResolver interface {
	HasRunnableRoute(task *model.Task) bool
}

// ProgressReader 是 Watchdog 对 L4 authority 的只读窄接口。Watchdog 只读取
// 最新 checkpoint 判断 liveness，不写 checkpoint，也不迁移 Task 状态。
type ProgressReader interface {
	LoadCheckpoint(taskID string) (*loopcontract.ProgressCheckpoint, bool, error)
}

// RouteResolverFunc adapts a function to RouteResolver.
type RouteResolverFunc func(task *model.Task) bool

func (f RouteResolverFunc) HasRunnableRoute(task *model.Task) bool {
	return f != nil && task != nil && f(task)
}

// NewRuntimeRouteResolver adapts AgentRegistry while preserving the built-in
// Scheduler route, which is not registered alongside ordinary worker routes.
func NewRuntimeRouteResolver(registry PlanRouteRegistry) RouteResolver {
	return RouteResolverFunc(func(task *model.Task) bool {
		if task.EventType == "__scheduler__" {
			return true
		}
		ownerScope := task.RouteScope
		if ownerScope == "" && task.GraphID != "" {
			ownerScope = model.GraphRouteScope(task.GraphID)
		} else if ownerScope == "" && task.ParentTaskID != "" {
			ownerScope = model.TaskRouteScope(task.ParentTaskID)
		}
		var required []string
		if task.Capability != nil {
			required = task.Capability.Tools
		}
		return registry != nil && registry.CanRouteForPlan(ownerScope, task.EventType, required...)
	})
}

type pendingObservationKind string

const (
	pendingObservationRoutable   pendingObservationKind = "routable"
	pendingObservationUnroutable pendingObservationKind = "unroutable"
	defaultPendingGraceSec                              = 300
)

type pendingObservation struct {
	kind    pendingObservationKind
	since   time.Time
	alerted bool
}

type progressObservationKey struct {
	taskID string
	kind   model.WatchdogObservationKind
}

// WorkspaceCleaner 是 Watchdog 清扫孤儿 workspace 所需的最小控制面接口。
// *workspace.Manager 天然满足（见下方编译期断言）；测试可注入 fake。
type WorkspaceCleaner interface {
	// ListWorkspaces 返回物理 workspace 与持久化 owner，不从目录名猜身份。
	ListWorkspaces() ([]workspace.Record, error)
	// InUse 报告是否仍有 Agent Activation 持有活动租约。
	InUse(workspaceID string) bool
	// Cleanup 删除任务 workspace 目录并注销活动视图。
	Cleanup(workspaceID string) error
}

// 编译期断言：*workspace.Manager 满足清扫接口。
var _ WorkspaceCleaner = (*workspace.Manager)(nil)

// WorkspaceRetentionResolver 裁决非 Task workspace 的持久化生命周期。
// known=false 必须 fail-closed 保留现场；Watchdog 无权猜测 Delivery 终态。
type WorkspaceRetentionResolver interface {
	RetainWorkspace(record workspace.Record) (retain bool, known bool)
}

type WorkspaceRetentionResolverFunc func(record workspace.Record) (bool, bool)

func (f WorkspaceRetentionResolverFunc) RetainWorkspace(record workspace.Record) (bool, bool) {
	if f == nil {
		return true, false
	}
	return f(record)
}

type Watchdog struct {
	Store         store.TaskStore
	Config        *config.Config
	EventCh       chan<- model.Event
	Roster        roster.Roster
	MailRegistry  *mailbox.Registry // 超时告警 / 级联取消时向 task.ReplyToAgentID（或 legacy EventSource）汇报
	SessionID     func() string     // 当前 Session identity；新 Run 报告邮件 envelope 使用
	RouteResolver RouteResolver
	// ProgressReader 读取 L4 durable checkpoint。nil 时新任务仍不会回退到
	// TimeoutSeconds；超过 heartbeat lease 后只报告 checkpoint missing。
	ProgressReader     ProgressReader
	WorkspaceRetention WorkspaceRetentionResolver
	// WorkspaceManager 是 workspace 控制面（nil-safe）：注入后每个巡检周期
	// 顺带清扫孤儿 workspace（任务不存在或已达终态的任务目录）。
	// nil 时跳过——保持既有测试与最小装配行为不变。
	WorkspaceManager WorkspaceCleaner

	// workspaceExemptions 是 workspace 清扫豁免集（taskID 集合）。豁免属于
	// 「冻结 session 的非终态任务」：冻结后任务不在活跃公告板上，但其
	// workspace 目录归冻结 session 所有（解冻重排后以同一 taskID 重排回
	// pending 复用），没有豁免会被 cleanupWorkspaceOrphans 误判孤儿清掉。
	// 豁免是纯进程内状态，进程重启后由 bootstrap 枚举冻结 session 快照重建。
	exemptMu            sync.Mutex
	workspaceExemptions map[string]struct{}

	pendingMu           sync.Mutex
	pendingObservations map[string]pendingObservation

	// overtimeWarned 记录已发超时告警的 processing 任务及执行身份。优先用
	// AttemptID，避免 Windows 时钟同 tick 时新租约与旧 StartedAt 相同。
	overtimeMu     sync.Mutex
	overtimeWarned map[string]overtimeIdentity

	// progressObserved 对同一 Task/typed fault 保存最近一次事实指纹。Checkpoint
	// 更新或 Attempt/deadline 变化后可重新报告，轮询本身不会刷屏。
	progressMu       sync.Mutex
	progressObserved map[progressObservationKey]string

	now func() time.Time
}

type overtimeIdentity struct {
	AttemptID string
	StartedAt time.Time
}

// New 构造 Watchdog。mbReg 为 nil 时 sendCrashReport 会静默跳过——保持向后兼容
// （既有 watchdog 单元测试通过 newTestWatchdog 构造时不传 mbReg，行为不变）。
func New(s store.TaskStore, cfg *config.Config, eventCh chan<- model.Event, r roster.Roster, mbReg *mailbox.Registry, routes ...RouteResolver) *Watchdog {
	w := &Watchdog{
		Store:               s,
		Config:              cfg,
		EventCh:             eventCh,
		Roster:              r,
		MailRegistry:        mbReg,
		pendingObservations: make(map[string]pendingObservation),
		now:                 time.Now,
	}
	if len(routes) > 0 {
		w.RouteResolver = routes[0]
	}
	return w
}

// Run starts the watchdog's ticker-driven inspection loop.
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(w.Config.Infra.Watchdog.IntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.inspect()
		}
	}
}

// RunOnce performs a single inspection cycle. Exposed for testing.
func (w *Watchdog) RunOnce() {
	w.inspect()
}

func (w *Watchdog) inspect() {
	tasks, err := w.Store.ScanAll()
	if err != nil {
		log.Printf("[watchdog] ScanAll error: %v", err)
		return
	}

	for _, task := range tasks {
		w.checkTask(task)
	}
	w.pruneObservations(tasks)

	// 花名册兜底清理：清除不属于任何活跃代理的残留声明
	w.cleanupStaleClaims(tasks)

	// workspace 孤儿兜底清理：任务已终态 / 已消失的任务目录（合并成功的
	// 正常清理由执行面负责，这里只兜失败 / 取消 / 崩溃的残留）
	w.cleanupWorkspaceOrphans()
}

func (w *Watchdog) checkTask(task *model.Task) {
	if task == nil {
		return
	}
	switch task.Status {
	case model.TaskStatusProcessing:
		w.clearPendingObservation(task.ID)
		w.checkProcessingTask(task)
	case model.TaskStatusPending:
		w.checkPendingTask(task)
	default:
		w.clearPendingObservation(task.ID)
	}
}

func (w *Watchdog) checkProcessingTask(task *model.Task) {
	// 新 Loop 只读取 checkpoint heartbeat 与绝对 deadline，发布 typed
	// observation；不发送会被 Activator 翻译成文本 Task 的 legacy alert，
	// 更不抢 L4 的 Reminder/Rollover/Blocked 状态迁移权。
	if task.RunContract != nil || task.ProgressContract != nil {
		w.observeLoopLiveness(task)
	} else {
		w.checkLegacyExpectedDuration(task)
	}

	// 级联取消：依赖任务失败或被取消
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			log.Printf("[watchdog] task %s dependency %s not found (processing), cancelling", task.ID, depID)
			if err := store.TransitionStateWithCancelSource(w.Store, task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "dependency_failure"); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 不存在", depID)
			w.sendCrashReport(task, reason, time.Since(task.StartedAt))
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s (processing), cascade cancelling", task.ID, depID, dep.Status)
			if err := store.TransitionStateWithCancelSource(w.Store, task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "dependency_failure"); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 已 %s", depID, dep.Status)
			w.sendCrashReport(task, reason, time.Since(task.StartedAt))
			return
		}
	}
}

// checkLegacyExpectedDuration 保留旧 Task 的一次性兼容告警。阈值只读取已
// 迁移的 ExpectedDuration；TimeoutSeconds 不再是运行时控制输入。
func (w *Watchdog) checkLegacyExpectedDuration(task *model.Task) {
	if task.ExpectedDuration > 0 && !task.StartedAt.IsZero() {
		threshold := task.ExpectedDuration
		elapsed := nonNegativeDuration(w.currentTime().Sub(task.StartedAt))
		if elapsed > threshold && w.markProcessingOvertime(task.ID, task.AttemptID, task.StartedAt) {
			log.Printf("[watchdog] legacy task %s 超过 ExpectedDuration 告警 (elapsed: %v, 预期: %v)", task.ID, elapsed.Round(time.Second), threshold)
			reason := fmt.Sprintf("processing_overtime: 任务已运行 %v，超过预期时长 %v；watchdog 不干预，请人工或上级代理检查",
				elapsed.Round(time.Second), threshold)
			w.sendStructuredAlert(task.ID, "processing_overtime", reason)
			w.sendOvertimeWarning(task, reason, elapsed, threshold)
		}
	}
}

func (w *Watchdog) observeLoopLiveness(task *model.Task) {
	now := w.currentTime()
	lease := w.progressHeartbeatLease()
	if w.ProgressReader == nil {
		w.observeUnavailableCheckpoint(task, now, lease, model.WatchdogCheckpointMissing)
		return
	}
	checkpoint, ok, err := w.ProgressReader.LoadCheckpoint(task.ID)
	if err != nil {
		log.Printf("[watchdog] task %s 读取 ProgressCheckpoint 失败: %v", task.ID, err)
		w.observeUnavailableCheckpoint(task, now, lease, model.WatchdogCheckpointReadError)
		return
	}
	if !ok || checkpoint == nil {
		w.observeUnavailableCheckpoint(task, now, lease, model.WatchdogCheckpointMissing)
		return
	}
	if err := validateObservedCheckpoint(task, checkpoint); err != nil {
		log.Printf("[watchdog] task %s ProgressCheckpoint 无效: %v", task.ID, err)
		observation := model.WatchdogObservation{
			Kind: model.WatchdogHeartbeatStalled, TaskID: task.ID,
			RunID: task.RunID, AttemptID: task.AttemptID,
			CheckpointID: checkpoint.CheckpointID, CheckpointState: model.WatchdogCheckpointInvalid,
			CheckpointUpdatedAt: checkpoint.UpdatedAt, HeartbeatLease: lease,
			InterventionStage: checkpoint.InterventionStage, ObservedAt: now,
		}
		fingerprint := checkpoint.CheckpointID + "|invalid|" + checkpoint.UpdatedAt.UTC().Format(time.RFC3339Nano)
		w.publishProgressObservation(observation, fingerprint)
		return
	}
	if task.AttemptID != "" && checkpoint.AttemptID != task.AttemptID {
		// claim/retry 与 LoopStore rollover 分属两个 authority；短暂不一致是
		// 合法装配窗口。Watchdog 不读取旧 Attempt deadline，只有窗口超过
		// heartbeat lease 才报告 old_attempt。
		w.observeUnavailableCheckpoint(task, now, lease, model.WatchdogCheckpointOldAttempt)
		return
	}

	if nonNegativeDuration(now.Sub(checkpoint.UpdatedAt)) > lease {
		observation := model.WatchdogObservation{
			Kind: model.WatchdogHeartbeatStalled, TaskID: task.ID,
			RunID: checkpoint.RunID, AttemptID: checkpoint.AttemptID,
			CheckpointID: checkpoint.CheckpointID, CheckpointState: model.WatchdogCheckpointStale,
			CheckpointUpdatedAt: checkpoint.UpdatedAt, HeartbeatLease: lease,
			InterventionStage: checkpoint.InterventionStage, ObservedAt: now,
		}
		fingerprint := checkpoint.CheckpointID + "|stale|" + checkpoint.UpdatedAt.UTC().Format(time.RFC3339Nano)
		w.publishProgressObservation(observation, fingerprint)
	}

	if deadline, riskAt, ok := mostUrgentDeadlineRisk(checkpoint, now); ok {
		observation := model.WatchdogObservation{
			Kind: model.WatchdogHardDeadlineRisk, TaskID: task.ID,
			RunID: checkpoint.RunID, AttemptID: checkpoint.AttemptID,
			CheckpointID: checkpoint.CheckpointID, CheckpointState: model.WatchdogCheckpointAvailable,
			CheckpointUpdatedAt: checkpoint.UpdatedAt, HeartbeatLease: lease,
			InterventionStage: checkpoint.InterventionStage,
			DeadlineScope:     deadline.Scope, DeadlineState: deadlineObservationState(deadline, now), RiskAt: riskAt,
			HardDeadlineAt: deadline.HardDeadlineAt, ObservedAt: now,
		}
		fingerprint := checkpoint.AttemptID + "|" + string(deadline.Scope) + "|" +
			deadline.HardDeadlineAt.UTC().Format(time.RFC3339Nano) + "|" + riskAt.UTC().Format(time.RFC3339Nano) +
			"|" + string(observation.DeadlineState)
		w.publishProgressObservation(observation, fingerprint)
	}
}

func (w *Watchdog) observeUnavailableCheckpoint(task *model.Task, now time.Time, lease time.Duration, state model.WatchdogCheckpointState) {
	if task == nil || task.StartedAt.IsZero() || nonNegativeDuration(now.Sub(task.StartedAt)) <= lease {
		return
	}
	observation := model.WatchdogObservation{
		Kind: model.WatchdogHeartbeatStalled, TaskID: task.ID,
		RunID: task.RunID, AttemptID: task.AttemptID,
		CheckpointState: state, HeartbeatLease: lease, ObservedAt: now,
	}
	fingerprint := task.AttemptID + "|" + string(state) + "|" + task.StartedAt.UTC().Format(time.RFC3339Nano)
	w.publishProgressObservation(observation, fingerprint)
}

func validateObservedCheckpoint(task *model.Task, checkpoint *loopcontract.ProgressCheckpoint) error {
	if task == nil || checkpoint == nil {
		return fmt.Errorf("task/checkpoint 为空")
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	if checkpoint.TaskID != task.ID {
		return fmt.Errorf("checkpoint task_id=%q 与 task=%q 不一致", checkpoint.TaskID, task.ID)
	}
	if task.RunID != "" && checkpoint.RunID != task.RunID {
		return fmt.Errorf("checkpoint run_id=%q 与 task run_id=%q 不一致", checkpoint.RunID, task.RunID)
	}
	return nil
}

func mostUrgentDeadlineRisk(checkpoint *loopcontract.ProgressCheckpoint, now time.Time) (runcontract.DeadlineBudget, time.Time, bool) {
	if checkpoint == nil {
		return runcontract.DeadlineBudget{}, time.Time{}, false
	}
	deadlines := []runcontract.DeadlineBudget{checkpoint.Deadlines.Attempt}
	if checkpoint.Deadlines.Activation != nil {
		deadlines = append(deadlines, *checkpoint.Deadlines.Activation)
	}
	if checkpoint.Deadlines.Graph != nil {
		deadlines = append(deadlines, *checkpoint.Deadlines.Graph)
	}
	deadlines = append(deadlines, checkpoint.Deadlines.Run)

	var selected runcontract.DeadlineBudget
	var selectedRiskAt time.Time
	found := false
	for _, deadline := range deadlines {
		riskAt := deadlineRiskAt(deadline)
		if riskAt.IsZero() || now.Before(riskAt) {
			continue
		}
		// 多个层级同时进入风险窗时，最早的 hard deadline 最紧急；这样
		// Attempt 到达风险窗后会自然覆盖先前的 Run/Graph reserve 告警。
		if !found || deadline.HardDeadlineAt.Before(selected.HardDeadlineAt) {
			selected = deadline
			selectedRiskAt = riskAt
			found = true
		}
	}
	return selected, selectedRiskAt, found
}

func deadlineRiskAt(deadline runcontract.DeadlineBudget) time.Time {
	if deadline.HardDeadlineAt.IsZero() {
		return time.Time{}
	}
	if !deadline.InterventionAt.IsZero() && !deadline.InterventionAt.After(deadline.HardDeadlineAt) {
		return deadline.InterventionAt
	}
	const maxDuration = time.Duration(1<<63 - 1)
	reserve := deadline.FinalizationReserve
	if deadline.RecoveryReserve > maxDuration-reserve {
		// 非法溢出输入不允许把风险窗回绕到未来；退化为 hard deadline 本身。
		return deadline.HardDeadlineAt
	}
	reserve += deadline.RecoveryReserve
	return deadline.HardDeadlineAt.Add(-reserve)
}

func deadlineObservationState(deadline runcontract.DeadlineBudget, now time.Time) model.WatchdogDeadlineState {
	if now.Before(deadline.HardDeadlineAt) {
		return model.WatchdogDeadlineAtRisk
	}
	return model.WatchdogDeadlineExceeded
}

func (w *Watchdog) progressHeartbeatLease() time.Duration {
	seconds := 0
	if w.Config != nil {
		seconds = w.Config.Infra.Watchdog.ProgressHeartbeatGraceSec
	}
	if seconds <= 0 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

func (w *Watchdog) publishProgressObservation(observation model.WatchdogObservation, fingerprint string) {
	key := progressObservationKey{taskID: observation.TaskID, kind: observation.Kind}
	w.progressMu.Lock()
	if w.progressObserved == nil {
		w.progressObserved = make(map[progressObservationKey]string)
	}
	if w.progressObserved[key] == fingerprint {
		w.progressMu.Unlock()
		return
	}
	w.progressObserved[key] = fingerprint
	w.progressMu.Unlock()

	payload := map[string]string{
		"reason_code":        string(observation.Kind),
		"checkpoint_state":   string(observation.CheckpointState),
		"intervention_stage": string(observation.InterventionStage),
	}
	if observation.CheckpointID != "" {
		payload["checkpoint_id"] = observation.CheckpointID
	}
	if observation.DeadlineScope != "" {
		payload["deadline_scope"] = string(observation.DeadlineScope)
		payload["deadline_state"] = string(observation.DeadlineState)
		payload["hard_deadline_at"] = observation.HardDeadlineAt.UTC().Format(time.RFC3339Nano)
		payload["risk_at"] = observation.RiskAt.UTC().Format(time.RFC3339Nano)
	}
	select {
	case w.EventCh <- model.Event{
		Type: model.EventWatchdogObservation, TaskID: observation.TaskID,
		Payload: payload, Observation: &observation,
	}:
	default:
	}
}

// emitPendingCascadeCancelTrace 为 pending→cancelled 的级联取消补发 trace 事件。
// processing 任务的取消事件由正在执行的 agent 在 ctx.Done() 分支 emit；
// pending 任务没有执行者，若不在此补发，排队期间被级联取消的任务在 trace
// 中完全不可见（2026-07-21 验收空转事故里多个排队验收任务即是如此）。
func emitPendingCascadeCancelTrace(taskID, reason string) {
	trace.Emit(trace.Event{
		Kind:   trace.KindTaskCancelled,
		TaskID: taskID,
		Reason: reason,
		Transition: &trace.Transition{
			PrevStatus:   string(model.TaskStatusPending),
			NewStatus:    string(model.TaskStatusCancelled),
			Cause:        "cascade_dependency_failure",
			CancelSource: "dependency_failure",
		},
	})
}

func (w *Watchdog) checkPendingTask(task *model.Task) {
	// Dependency state is authoritative before queue-age classification. A
	// healthy but incomplete dependency is normal waiting, not starvation.
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			// 依赖缺失，视为失败
			log.Printf("[watchdog] task %s dependency %s not found, cancelling", task.ID, depID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 不存在", depID)
			if err := store.TransitionStateWithCancelSource(w.Store, task.ID, model.TaskStatusPending, model.TaskStatusCancelled, "dependency_failure"); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			} else {
				emitPendingCascadeCancelTrace(task.ID, reason)
			}
			w.sendAlert(task.ID)
			w.sendCrashReport(task, reason, w.pendingElapsed(task))
			w.clearPendingObservation(task.ID)
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s, cascade cancelling", task.ID, depID, dep.Status)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 已 %s", depID, dep.Status)
			if err := store.TransitionStateWithCancelSource(w.Store, task.ID, model.TaskStatusPending, model.TaskStatusCancelled, "dependency_failure"); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			} else {
				emitPendingCascadeCancelTrace(task.ID, reason)
			}
			w.sendAlert(task.ID)
			w.sendCrashReport(task, reason, w.pendingElapsed(task))
			w.clearPendingObservation(task.ID)
			return
		}
		if dep.Status == model.TaskStatusBlocked {
			reason := fmt.Sprintf("dependency_blocked: 依赖任务 %s 已 blocked", depID)
			if err := w.blockPendingTask(task.ID, reason); err != nil {
				log.Printf("[watchdog] block downstream task %s after dependency %s blocked failed: %v", task.ID, depID, err)
			} else {
				w.sendStructuredAlert(task.ID, "dependency_blocked", reason)
				log.Printf("[watchdog] task %s blocked because dependency %s is blocked", task.ID, depID)
			}
			w.clearPendingObservation(task.ID)
			return
		}
		if dep.Status != model.TaskStatusCompleted {
			w.clearPendingObservation(task.ID)
			return
		}
	}

	// QueryAvailable 复用 TaskStore 的可认领性判定（依赖完成度、并发上限、
	// per-node 能力）。被刻意保留为不可认领的任务不应在排队中老化为队列失败。
	claimable, err := w.pendingTaskIsClaimable(task)
	if err != nil {
		log.Printf("[watchdog] task %s claimability probe failed: %v", task.ID, err)
		w.clearPendingObservation(task.ID)
		return
	}
	if !claimable {
		w.clearPendingObservation(task.ID)
		return
	}

	// A nil resolver is an intentionally conservative compatibility mode for
	// tests/alternate embeddings: route truth is unknown, so Watchdog observes
	// no destructive pending terminal.
	if w.RouteResolver == nil {
		w.clearPendingObservation(task.ID)
		return
	}
	if !w.RouteResolver.HasRunnableRoute(task) {
		w.checkUnroutableTask(task)
		return
	}
	w.checkRoutableQueueWait(task)
}

func (w *Watchdog) pendingTaskIsClaimable(task *model.Task) (bool, error) {
	// 探测性查询无认领方身份：agentID 传空串，QueryAvailable 跳过
	// per-node 能力过滤（该过滤需要具体认领方的白名单才能判定）。
	available, err := w.Store.QueryAvailable(task.EventType, "")
	if err != nil {
		return false, err
	}
	for _, candidate := range available {
		if candidate != nil && candidate.ID == task.ID {
			return true, nil
		}
	}
	return false, nil
}

func (w *Watchdog) checkUnroutableTask(task *model.Task) {
	now := w.currentTime()
	observation := w.observeUnroutable(task.ID, now)
	elapsed := nonNegativeDuration(now.Sub(observation.since))
	grace := w.unroutableGrace()
	if elapsed <= grace {
		return
	}

	reason := fmt.Sprintf(
		"no_compatible_route: event_type=%q remained unavailable for %v",
		task.EventType, elapsed.Round(time.Second))
	if !observation.alerted {
		w.markPendingAlerted(task.ID, pendingObservationUnroutable, observation.since)
		w.sendStructuredAlert(task.ID, "no_compatible_route", reason)
	}

	if err := w.blockPendingTask(task.ID, reason); err != nil {
		log.Printf("[watchdog] block unroutable task %s failed: %v", task.ID, err)
		return
	}
	log.Printf("[watchdog] task %s blocked: %s", task.ID, reason)
	w.clearPendingObservation(task.ID)
}

func (w *Watchdog) blockPendingTask(taskID, reason string) error {
	if blocker, ok := w.Store.(interface {
		BlockTaskBySystem(taskID, reason string) error
	}); ok {
		return blocker.BlockTaskBySystem(taskID, reason)
	}
	// Alternate stores keep source compatibility. The structured alert carries
	// the reason even though their generic transition cannot persist Error.
	return w.Store.TransitionState(taskID, model.TaskStatusPending, model.TaskStatusBlocked)
}

func (w *Watchdog) observeUnroutable(taskID string, now time.Time) pendingObservation {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	if w.pendingObservations == nil {
		w.pendingObservations = make(map[string]pendingObservation)
	}
	observation, ok := w.pendingObservations[taskID]
	if !ok || observation.kind != pendingObservationUnroutable {
		observation = pendingObservation{kind: pendingObservationUnroutable, since: now}
		w.pendingObservations[taskID] = observation
	}
	return observation
}

func (w *Watchdog) checkRoutableQueueWait(task *model.Task) {
	now := w.currentTime()
	since := task.PendingSince
	if since.IsZero() {
		// Legacy/alternate stores without a queue lease start a fresh in-memory
		// observation. Never fall back to immutable CreatedAt.
		since = now
	}
	observation := w.observePending(task.ID, pendingObservationRoutable, since)
	elapsed := nonNegativeDuration(now.Sub(observation.since))
	grace := w.claimGrace()
	if elapsed <= grace || observation.alerted {
		return
	}

	reason := fmt.Sprintf(
		"claim_starvation: compatible route exists for event_type=%q but current pending lease has waited %v; task remains pending",
		task.EventType, elapsed.Round(time.Second))
	w.markPendingAlerted(task.ID, pendingObservationRoutable, observation.since)
	w.sendStructuredAlert(task.ID, "claim_starvation", reason)
	log.Printf("[watchdog] task %s pending with runnable route: %s", task.ID, reason)
}

func (w *Watchdog) observePending(taskID string, kind pendingObservationKind, since time.Time) pendingObservation {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	if w.pendingObservations == nil {
		w.pendingObservations = make(map[string]pendingObservation)
	}
	observation, ok := w.pendingObservations[taskID]
	if !ok || observation.kind != kind || !observation.since.Equal(since) {
		observation = pendingObservation{kind: kind, since: since}
		w.pendingObservations[taskID] = observation
	}
	return observation
}

func (w *Watchdog) markPendingAlerted(taskID string, kind pendingObservationKind, since time.Time) {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	observation, ok := w.pendingObservations[taskID]
	if !ok || observation.kind != kind || !observation.since.Equal(since) {
		return
	}
	observation.alerted = true
	w.pendingObservations[taskID] = observation
}

func (w *Watchdog) clearPendingObservation(taskID string) {
	w.pendingMu.Lock()
	delete(w.pendingObservations, taskID)
	w.pendingMu.Unlock()
}

// markProcessingOvertime 按 AttemptID（旧快照才回退 StartedAt）记录并报告
// 执行租约是否首次触发超时告警；同 Attempt 重复巡检返回 false。
func (w *Watchdog) markProcessingOvertime(taskID, attemptID string, startedAt time.Time) bool {
	w.overtimeMu.Lock()
	defer w.overtimeMu.Unlock()
	if w.overtimeWarned == nil {
		w.overtimeWarned = make(map[string]overtimeIdentity)
	}
	identity := overtimeIdentity{AttemptID: attemptID, StartedAt: startedAt}
	if prev, ok := w.overtimeWarned[taskID]; ok && ((attemptID != "" && prev.AttemptID == attemptID) || (attemptID == "" && prev.AttemptID == "" && prev.StartedAt.Equal(startedAt))) {
		return false
	}
	w.overtimeWarned[taskID] = identity
	return true
}

func (w *Watchdog) pruneObservations(tasks []*model.Task) {
	pending := make(map[string]struct{}, len(tasks))
	processing := make(map[string]overtimeIdentity)
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if task.Status == model.TaskStatusPending {
			pending[task.ID] = struct{}{}
		}
		if task.Status == model.TaskStatusProcessing {
			processing[task.ID] = overtimeIdentity{AttemptID: task.AttemptID, StartedAt: task.StartedAt}
		}
	}
	w.pendingMu.Lock()
	for taskID := range w.pendingObservations {
		if _, ok := pending[taskID]; !ok {
			delete(w.pendingObservations, taskID)
		}
	}
	w.pendingMu.Unlock()
	// 超时告警标记随任务离开 processing 或换 AttemptID 而失效；旧快照无
	// AttemptID 时才比较 StartedAt，保证重试在 Windows 同 tick 仍重新武装。
	w.overtimeMu.Lock()
	for taskID, identity := range w.overtimeWarned {
		if cur, ok := processing[taskID]; !ok || (identity.AttemptID != "" && cur.AttemptID != identity.AttemptID) || (identity.AttemptID == "" && (cur.AttemptID != "" || !cur.StartedAt.Equal(identity.StartedAt))) {
			delete(w.overtimeWarned, taskID)
		}
	}
	w.overtimeMu.Unlock()
	w.progressMu.Lock()
	for key := range w.progressObserved {
		if _, ok := processing[key.taskID]; !ok {
			delete(w.progressObserved, key)
		}
	}
	w.progressMu.Unlock()
}

func (w *Watchdog) currentTime() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *Watchdog) pendingElapsed(task *model.Task) time.Duration {
	if task == nil || task.PendingSince.IsZero() {
		return 0
	}
	return nonNegativeDuration(w.currentTime().Sub(task.PendingSince))
}

func (w *Watchdog) claimGrace() time.Duration {
	seconds := 0
	if w.Config != nil {
		seconds = w.Config.Infra.Watchdog.PendingAlertGraceSec
	}
	if seconds <= 0 {
		seconds = defaultPendingGraceSec
	}
	return time.Duration(seconds) * time.Second
}

func (w *Watchdog) unroutableGrace() time.Duration {
	seconds := 0
	if w.Config != nil {
		seconds = w.Config.Infra.Watchdog.UnroutableGraceSec
	}
	if seconds <= 0 {
		seconds = defaultPendingGraceSec
	}
	return time.Duration(seconds) * time.Second
}

func nonNegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func (w *Watchdog) sendAlert(taskID string) {
	select {
	case w.EventCh <- model.Event{Type: model.EventWatchdogAlert, TaskID: taskID}:
	default:
	}
}

func (w *Watchdog) sendStructuredAlert(taskID, reasonCode, reason string) {
	select {
	case w.EventCh <- model.Event{
		Type:   model.EventWatchdogAlert,
		TaskID: taskID,
		Payload: map[string]string{
			"reason_code": reasonCode,
			"reason":      reason,
		},
	}:
	default:
	}
}

// sendCrashReport 在 watchdog 外部终止任务时（级联取消），向显式
// ReplyToAgentID（或仍可路由的 legacy EventSource）发一封结构化崩溃汇报
// 邮件，补齐上级侧 "为什么死"的上下文。
//
// 与 agent.sendCrashReport 对称——agent 负责"自己死了告诉上级"，watchdog 负责
// "外部判定你死了告诉上级"。两者并存，从两个视角覆盖任务终态的可观测性。
func (w *Watchdog) sendCrashReport(task *model.Task, reason string, elapsed time.Duration) {
	if task == nil {
		return
	}
	summary := fmt.Sprintf("watchdog 判定任务 %s 死亡：%s", shortID(task.ID), truncate(reason, 60))
	headline := fmt.Sprintf("Watchdog 外部终止了任务 %s。", task.ID)
	judgment := fmt.Sprintf("Watchdog 判定: %s", reason)
	w.mailTaskReport(task, summary, headline, judgment, elapsed)
}

// sendOvertimeWarning 在任务超过预期时长仍运行时发超时告警邮件——与
// sendCrashReport 的关键区别：任务没有被终止，仍在运行（2026-08-19 起
// watchdog 不再杀死超时任务）。上级 Agent 或人可据此检查该分支是否异常。
func (w *Watchdog) sendOvertimeWarning(task *model.Task, reason string, elapsed, threshold time.Duration) {
	if task == nil {
		return
	}
	summary := fmt.Sprintf("watchdog 超时告警：任务 %s 已运行 %v（预期 %v），未终止",
		shortID(task.ID), elapsed.Round(time.Second), threshold)
	headline := fmt.Sprintf("任务 %s 运行超过预期时长（%v > %v），watchdog 未终止它，任务仍在运行。",
		task.ID, elapsed.Round(time.Second), threshold)
	judgment := fmt.Sprintf("Watchdog 告警: %s", reason)
	w.mailTaskReport(task, summary, headline, judgment, elapsed)
}

// mailTaskReport 是 sendCrashReport / sendOvertimeWarning 的共用装配：
// 解析收件人 → 重读任务最新状态 → 拼装正文 → 发送。
//
// 静默跳过的情形：
//   - MailRegistry 未注入（测试场景 / 配置关闭）
//   - task 为 nil（防御）
//   - ReplyToAgentID 与 legacy EventSource 都无法解析为当前可路由邮箱
func (w *Watchdog) mailTaskReport(task *model.Task, summary, headline, judgment string, elapsed time.Duration) {
	if w.MailRegistry == nil || task == nil {
		return
	}

	taskID := task.ID

	// 重读一次拿最新的 Agents / Artifacts（刚刚的状态迁移可能更新了状态字段；
	// Artifacts 则可能是 worker 最近写下的）。
	if fresh, err := w.Store.GetTask(taskID); err == nil && fresh != nil {
		task = fresh
	}
	recipient := w.MailRegistry.ResolveReplyRecipient(
		task.ReplyToAgentID, task.EventSource, task.ID, task.ParentTaskID)
	if recipient == "" {
		return
	}

	desc := task.Description
	if len([]rune(desc)) > 100 {
		desc = string([]rune(desc)[:100]) + "..."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", headline)
	fmt.Fprintf(&sb, "任务描述: %s\n", desc)

	if len(task.Agents) > 0 {
		fmt.Fprintf(&sb, "执行代理: %v\n", task.Agents)
	} else {
		sb.WriteString("执行代理: <无，任务从未被认领>\n")
	}

	fmt.Fprintf(&sb, "%s\n", judgment)
	fmt.Fprintf(&sb, "elapsed: %v\n", elapsed.Round(time.Second))

	// 最近 3 条工具调用。用 StoreHookView.GetToolCallHistory 弱耦合获取——
	// MemoryTaskStore 已实现该接口（store/hookview.go:71 编译期断言）。
	// 未实现的 Store 降级为不输出这段 body。
	if v, ok := w.Store.(store.StoreHookView); ok {
		if history := v.GetToolCallHistory(taskID); len(history) > 0 {
			start := len(history) - 3
			if start < 0 {
				start = 0
			}
			sb.WriteString("\n最近工具调用:\n")
			for _, rec := range history[start:] {
				fmt.Fprintf(&sb, "  %s %s (agent=%s success=%v)\n",
					rec.Timestamp.Format("15:04:05"), rec.ToolName, rec.AgentID, rec.Success)
			}
		}
	}

	if len(task.Artifacts) > 0 {
		sb.WriteString("\n已落盘文件:\n")
		for _, p := range task.Artifacts {
			fmt.Fprintf(&sb, "  - %s\n", p)
		}
	} else {
		sb.WriteString("\n已落盘文件: 无\n")
	}

	msg := mailbox.Message{
		From:     "watchdog",
		To:       recipient,
		Type:     mailbox.MsgTypeInfo,
		Priority: mailbox.PriorityHigh,
		Summary:  summary,
		Content:  sb.String(),
		SentAt:   time.Now(),
	}
	if task.RunID != "" {
		msg.SourceTaskID = task.ID
		msg.RunID = task.RunID
		if w.SessionID != nil {
			msg.SessionID = w.SessionID()
		}
	}
	if err := w.MailRegistry.Send(msg); err != nil {
		log.Printf("[watchdog] 发送任务报告邮件给 %s 失败: %v", recipient, err)
	} else {
		log.Printf("[watchdog] 已向 %s 发送任务 %s 报告 (%s)", recipient, shortID(taskID), truncate(summary, 40))
	}
}

// shortID 返回 UUID 的前 8 字符；短于 8 字符的按原样返回。
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// truncate 按 rune 截断字符串到 maxRunes，超过时追加省略号。
func truncate(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "..."
}

// cleanupStaleClaims 对比花名册声明与公告板活跃代理，清理残留。
func (w *Watchdog) cleanupStaleClaims(tasks []*model.Task) {
	if w.Roster == nil {
		return
	}

	// 收集所有 processing 任务中的活跃代理 ID
	activeAgents := make(map[string]bool)
	for _, task := range tasks {
		if task.Status == model.TaskStatusProcessing {
			for _, agentID := range task.Agents {
				activeAgents[agentID] = true
			}
		}
	}

	// 从花名册获取所有持有声明的代理，清理不活跃的
	claimAgents, err := w.Roster.ListAllAgents()
	if err != nil {
		return
	}
	for _, agentID := range claimAgents {
		if !activeAgents[agentID] {
			log.Printf("[watchdog] 清理代理 %s 的残留花名册声明", agentID)
			w.Roster.ReleaseAll(agentID)
		}
	}
}

// ExemptWorkspaces 把 taskID 加入 workspace 清扫豁免集（幂等，并发安全）。
// 豁免属于「冻结 session 的非终态任务」：session 冻结归档后这些任务不在
// 活跃公告板上，但其 workspace 归冻结 session 所有，解冻重排后以同一
// taskID 复用——登记豁免防止 cleanupWorkspaceOrphans 按孤儿误清。
// 空串 ID 忽略；nil 接收者安全（无 Session/测试装配路径）。
func (w *Watchdog) ExemptWorkspaces(taskIDs []string) {
	if w == nil || len(taskIDs) == 0 {
		return
	}
	w.exemptMu.Lock()
	defer w.exemptMu.Unlock()
	if w.workspaceExemptions == nil {
		w.workspaceExemptions = make(map[string]struct{}, len(taskIDs))
	}
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		w.workspaceExemptions[id] = struct{}{}
	}
}

// ClearWorkspaceExemptions 把 taskID 移出 workspace 清扫豁免集（幂等，
// 并发安全）：任务已解冻重排回活跃公告板，恢复常规存活/终态判定。
// nil 接收者安全。
func (w *Watchdog) ClearWorkspaceExemptions(taskIDs []string) {
	if w == nil || len(taskIDs) == 0 {
		return
	}
	w.exemptMu.Lock()
	defer w.exemptMu.Unlock()
	for _, id := range taskIDs {
		delete(w.workspaceExemptions, id)
	}
}

// IsWorkspaceExempt 报告 taskID 是否在 workspace 清扫豁免集中（并发安全）。
// 供观测/调试与跨包断言使用；nil 接收者安全。
func (w *Watchdog) IsWorkspaceExempt(taskID string) bool {
	if w == nil {
		return false
	}
	w.exemptMu.Lock()
	defer w.exemptMu.Unlock()
	_, ok := w.workspaceExemptions[taskID]
	return ok
}

// cleanupWorkspaceOrphans 清扫孤儿 workspace：ListOrphans 列出全部任务目录，
// 逐个对照 TaskStore——任务不存在（已被淘汰）或已达终态（含 failed/cancelled
// 的崩溃残留）的目录由 Manager.Cleanup 移除；任务仍活跃（pending/processing）
// 的目录保留，可能是正在执行或可重试的隔离任务。
// 豁免集中的 taskID 无条件跳过（豁免优先于一切存活/终态判断）：豁免属于
// 「冻结 session 的非终态任务」，其 workspace 归冻结 session 所有。
// WorkspaceManager 为 nil 时整体跳过（nil-safe，行为与注入前一致）。
func (w *Watchdog) cleanupWorkspaceOrphans() {
	mgr := w.WorkspaceManager
	if mgr == nil {
		return
	}
	records, err := mgr.ListWorkspaces()
	if err != nil {
		log.Printf("[watchdog] workspace 孤儿扫描失败: %v", err)
		return
	}
	for _, record := range records {
		workspaceID := record.WorkspaceID
		if w.IsWorkspaceExempt(workspaceID) {
			log.Printf("[watchdog] workspace 命中冻结 session 豁免，跳过清理 (workspace=%s)", workspaceID)
			continue
		}
		if mgr.InUse(workspaceID) {
			trace.Emit(trace.Event{Kind: trace.KindWorkspaceRetentionDecided, TaskID: workspaceID,
				RunID: record.Owner.RunID, GraphID: record.Owner.GraphID, Reason: "active_lease",
				Description: "retain=true owner=" + string(record.Owner.Kind)})
			log.Printf("[watchdog] workspace 仍有活动租约，跳过清理 (workspace=%s)", workspaceID)
			continue
		}
		if record.Owner.Kind == workspace.OwnerDelivery {
			if w.WorkspaceRetention == nil {
				trace.Emit(trace.Event{Kind: trace.KindWorkspaceRetentionDecided, TaskID: workspaceID,
					RunID: record.Owner.RunID, GraphID: record.Owner.GraphID, Reason: "resolver_missing",
					Description: "retain=true owner=delivery known=false"})
				log.Printf("[watchdog] Delivery workspace 缺少生命周期裁决器，保留现场 (workspace=%s graph=%s)",
					workspaceID, record.Owner.GraphID)
				continue
			}
			retain, known := w.WorkspaceRetention.RetainWorkspace(record)
			reason := "delivery_retained"
			if !known {
				reason = "delivery_liveness_unknown"
			} else if !retain {
				reason = "delivery_committed_cleanup"
			}
			trace.Emit(trace.Event{Kind: trace.KindWorkspaceRetentionDecided, TaskID: workspaceID,
				RunID: record.Owner.RunID, GraphID: record.Owner.GraphID, Reason: reason,
				Description: fmt.Sprintf("retain=%t known=%t owner=delivery", retain, known)})
			if !known || retain {
				log.Printf("[watchdog] Delivery workspace 保留 (workspace=%s graph=%s known=%t)",
					workspaceID, record.Owner.GraphID, known)
				continue
			}
			if err := mgr.Cleanup(workspaceID); err != nil {
				log.Printf("[watchdog] 清理已提交 Delivery workspace 失败 (workspace=%s): %v", workspaceID, err)
				continue
			}
			log.Printf("[watchdog] 已清理完成提交的 Delivery workspace (workspace=%s)", workspaceID)
			continue
		}
		taskID := record.Owner.TaskID
		task, err := w.Store.GetTask(taskID)
		switch {
		case err != nil || task == nil:
			// 任务已不存在（store 淘汰 / 从未登记）→ 孤儿，清理
		case model.IsTerminal(task.Status):
			// 任务已终态 → 残留的 workspace 不会再被使用，清理
		default:
			continue // 任务仍活跃，保留 workspace
		}
		if err := mgr.Cleanup(workspaceID); err != nil {
			log.Printf("[watchdog] 清理孤儿 workspace 失败 (task=%s workspace=%s): %v", taskID, workspaceID, err)
			continue
		}
		log.Printf("[watchdog] 已清理孤儿 workspace (task=%s workspace=%s)", taskID, workspaceID)
	}
}
