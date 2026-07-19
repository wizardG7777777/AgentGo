package watchdog

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// PlanRouteRegistry is the smallest runtime-routing authority Watchdog needs.
// scheduler.AgentRegistry satisfies it without making watchdog depend on the
// scheduler package.
type PlanRouteRegistry interface {
	CanRouteForPlan(planID, eventType string, requiredTools ...string) bool
}

// RouteResolver answers whether a pending Task has a runtime listener that may
// claim it. It intentionally reports route existence only; worker busy/idle
// capacity is not inferred here.
type RouteResolver interface {
	HasRunnableRoute(planID, eventType string) bool
}

// RouteResolverFunc adapts a function to RouteResolver.
type RouteResolverFunc func(planID, eventType string) bool

func (f RouteResolverFunc) HasRunnableRoute(planID, eventType string) bool {
	return f != nil && f(planID, eventType)
}

// NewRuntimeRouteResolver adapts AgentRegistry while preserving the built-in
// Scheduler route, which is not registered alongside ordinary worker routes.
func NewRuntimeRouteResolver(registry PlanRouteRegistry) RouteResolver {
	return RouteResolverFunc(func(planID, eventType string) bool {
		if eventType == "__scheduler__" {
			return true
		}
		return registry != nil && registry.CanRouteForPlan(planID, eventType)
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

type Watchdog struct {
	Store         store.TaskStore
	Config        *config.Config
	EventCh       chan<- model.Event
	Roster        roster.Roster
	MailRegistry  *mailbox.Registry // 2026-04-25 P1：超时/级联取消时向 task.EventSource 汇报
	RouteResolver RouteResolver

	pendingMu           sync.Mutex
	pendingObservations map[string]pendingObservation
	now                 func() time.Time
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
	w.prunePendingObservations(tasks)

	// 花名册兜底清理：清除不属于任何活跃代理的残留声明
	w.cleanupStaleClaims(tasks)
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
	// 超时检测：processing 时间 > timeout * 1.1
	if task.TimeoutSeconds > 0 && !task.StartedAt.IsZero() {
		threshold := time.Duration(float64(task.TimeoutSeconds)*1.1) * time.Second
		elapsed := time.Since(task.StartedAt)
		if elapsed > threshold {
			log.Printf("[watchdog] task %s timeout detected (elapsed: %v, threshold: %v)", task.ID, elapsed, threshold)
			reason := fmt.Sprintf("任务超时：已运行 %v，阈值 %v", elapsed.Round(time.Second), threshold)
			if err := w.Store.FailTaskBySystem(task.ID, reason); err != nil {
				log.Printf("[watchdog] FailTaskBySystem task %s failed: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			w.sendCrashReport(task, reason, elapsed)
			return
		}
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

func (w *Watchdog) checkPendingTask(task *model.Task) {
	// Dependency state is authoritative before queue-age classification. A
	// healthy but incomplete dependency is normal waiting, not starvation.
	for _, depID := range task.Dependencies {
		dep, err := w.Store.GetTask(depID)
		if err != nil {
			// 依赖缺失，视为失败
			log.Printf("[watchdog] task %s dependency %s not found, cancelling", task.ID, depID)
			if err := store.TransitionStateWithCancelSource(w.Store, task.ID, model.TaskStatusPending, model.TaskStatusCancelled, "dependency_failure"); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 不存在", depID)
			w.sendCrashReport(task, reason, w.pendingElapsed(task))
			w.clearPendingObservation(task.ID)
			return
		}
		if dep.Status == model.TaskStatusFailed || dep.Status == model.TaskStatusCancelled {
			log.Printf("[watchdog] task %s dependency %s is %s, cascade cancelling", task.ID, depID, dep.Status)
			if err := store.TransitionStateWithCancelSource(w.Store, task.ID, model.TaskStatusPending, model.TaskStatusCancelled, "dependency_failure"); err != nil {
				log.Printf("[watchdog] 级联取消 task %s 失败: %v", task.ID, err)
			}
			w.sendAlert(task.ID)
			reason := fmt.Sprintf("级联取消：依赖任务 %s 已 %s", depID, dep.Status)
			w.sendCrashReport(task, reason, w.pendingElapsed(task))
			w.clearPendingObservation(task.ID)
			return
		}
		if dep.Status == model.TaskStatusBlocked {
			reason := fmt.Sprintf("dependency_blocked: 依赖任务 %s 已 blocked", depID)
			if err := w.blockPendingTask(task.ID, reason); err != nil {
				log.Printf("[watchdog] block downstream task %s after dependency %s blocked failed: %v", task.ID, depID, err)
			} else {
				w.sendPendingAlert(task.ID, "dependency_blocked", reason)
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

	// QueryAvailable reuses the TaskStore's Plan CanClaim hook. A paused,
	// blocked, retired, or otherwise control-plane-reserved Task must not age
	// into a queue failure while it is deliberately ineligible for claiming.
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
	if !w.RouteResolver.HasRunnableRoute(task.PlanID, task.EventType) {
		w.checkUnroutableTask(task)
		return
	}
	w.checkRoutableQueueWait(task)
}

func (w *Watchdog) pendingTaskIsClaimable(task *model.Task) (bool, error) {
	available, err := w.Store.QueryAvailable(task.EventType)
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
		"no_compatible_route: event_type=%q plan_id=%q remained unavailable for %v",
		task.EventType, task.PlanID, elapsed.Round(time.Second))
	if !observation.alerted {
		w.markPendingAlerted(task.ID, pendingObservationUnroutable, observation.since)
		w.sendPendingAlert(task.ID, "no_compatible_route", reason)
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
		"claim_starvation: compatible route exists for event_type=%q plan_id=%q but current pending lease has waited %v; task remains pending",
		task.EventType, task.PlanID, elapsed.Round(time.Second))
	w.markPendingAlerted(task.ID, pendingObservationRoutable, observation.since)
	w.sendPendingAlert(task.ID, "claim_starvation", reason)
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

func (w *Watchdog) prunePendingObservations(tasks []*model.Task) {
	pending := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if task != nil && task.Status == model.TaskStatusPending {
			pending[task.ID] = struct{}{}
		}
	}
	w.pendingMu.Lock()
	for taskID := range w.pendingObservations {
		if _, ok := pending[taskID]; !ok {
			delete(w.pendingObservations, taskID)
		}
	}
	w.pendingMu.Unlock()
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

func (w *Watchdog) sendPendingAlert(taskID, reasonCode, reason string) {
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

// sendCrashReport 在 watchdog 外部杀掉任务时，向显式 ReplyToAgentID（或仍可
// 路由的 legacy EventSource）发一封结构化崩溃汇报邮件，补齐上级侧
// "为什么死"的上下文。
//
// 与 agent.sendCrashReport 对称——agent 负责"自己死了告诉上级"，watchdog 负责
// "外部判定你死了告诉上级"。两者并存，从两个视角覆盖任务终态的可观测性。
//
// 静默跳过的情形：
//   - MailRegistry 未注入（测试场景 / 配置关闭）
//   - task 为 nil（防御）
//   - ReplyToAgentID 与 legacy EventSource 都无法解析为当前可路由邮箱
func (w *Watchdog) sendCrashReport(task *model.Task, reason string, elapsed time.Duration) {
	if w.MailRegistry == nil || task == nil {
		return
	}

	taskID := task.ID

	// 重读一次拿最新的 Agents / Artifacts（刚刚的 FailTaskBySystem / TransitionState
	// 可能更新了状态字段；Artifacts 则可能是 worker 临死前写下的）。
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

	short := shortID(taskID)
	summary := fmt.Sprintf("watchdog 判定任务 %s 死亡：%s", short, truncate(reason, 60))

	var sb strings.Builder
	fmt.Fprintf(&sb, "Watchdog 外部杀掉了任务 %s。\n", taskID)
	fmt.Fprintf(&sb, "任务描述: %s\n", desc)

	if len(task.Agents) > 0 {
		fmt.Fprintf(&sb, "执行代理: %v\n", task.Agents)
	} else {
		sb.WriteString("执行代理: <无，任务从未被认领>\n")
	}

	fmt.Fprintf(&sb, "Watchdog 判定: %s\n", reason)
	fmt.Fprintf(&sb, "elapsed: %v\n", elapsed.Round(time.Second))

	// 最近 3 条工具调用（"死前最后动作"）。用 StoreHookView.GetToolCallHistory
	// 弱耦合获取——MemoryTaskStore 已实现该接口（store/hookview.go:71 编译期断言）。
	// 未实现的 Store 降级为不输出这段 body。
	if v, ok := w.Store.(store.StoreHookView); ok {
		if history := v.GetToolCallHistory(taskID); len(history) > 0 {
			start := len(history) - 3
			if start < 0 {
				start = 0
			}
			sb.WriteString("\n死前最近工具调用:\n")
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
		sb.WriteString("（代理并非完全没干活——可考虑接收漂移产物或据此调整下一次发布。）\n")
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
	if err := w.MailRegistry.Send(msg); err != nil {
		log.Printf("[watchdog] 发送崩溃汇报给 %s 失败: %v", recipient, err)
	} else {
		log.Printf("[watchdog] 已向 %s 汇报任务 %s 死亡 (%s)", recipient, short, truncate(reason, 40))
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
