package watchdog

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

func newTestWatchdog() (*Watchdog, store.TaskStore, chan model.Event) {
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.Infra.Store.DefaultTimeoutSec = 300
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	w := New(s, cfg, ch, r, nil, RouteResolverFunc(func(*model.Task) bool { return true }))
	return w, s, ch
}

// inspectAll runs inspection on ALL tasks (not sampled) for deterministic testing.
func inspectAll(w *Watchdog) {
	tasks, _ := w.Store.ScanAll()
	for _, task := range tasks {
		w.checkTask(task)
	}
}

func setTaskTiming(t *testing.T, s store.TaskStore, taskID string, createdAt, startedAt time.Time) {
	t.Helper()
	mem, ok := s.(*store.MemoryTaskStore)
	if !ok {
		t.Fatal("test store is not MemoryTaskStore")
	}
	if err := mem.SetTaskTiming(taskID, createdAt, startedAt); err != nil {
		t.Fatalf("SetTaskTiming: %v", err)
	}
}

func setTaskPendingSince(t *testing.T, s store.TaskStore, taskID string, pendingSince time.Time) {
	t.Helper()
	mem, ok := s.(*store.MemoryTaskStore)
	if !ok {
		t.Fatal("test store is not MemoryTaskStore")
	}
	if err := mem.SetTaskPendingSince(taskID, pendingSince); err != nil {
		t.Fatalf("SetTaskPendingSince: %v", err)
	}
}

func drainEvents(ch <-chan model.Event) []model.Event {
	var events []model.Event
	for {
		select {
		case evt := <-ch:
			events = append(events, evt)
		default:
			return events
		}
	}
}

func watchdogAlerts(events []model.Event) []model.Event {
	var alerts []model.Event
	for _, evt := range events {
		if evt.Type == model.EventWatchdogAlert {
			alerts = append(alerts, evt)
		}
	}
	return alerts
}

type fakePlanRouteRegistry struct {
	available bool
	calls     int
	owner     string
	eventType string
	required  []string
}

func (f *fakePlanRouteRegistry) CanRouteForPlan(owner, eventType string, required ...string) bool {
	f.calls++
	f.owner = owner
	f.eventType = eventType
	f.required = append([]string(nil), required...)
	return f.available
}

func TestRuntimeRouteResolverPreservesBuiltInSchedulerRoute(t *testing.T) {
	registry := &fakePlanRouteRegistry{}
	resolver := NewRuntimeRouteResolver(registry)
	if !resolver.HasRunnableRoute(&model.Task{EventType: "__scheduler__"}) {
		t.Fatal("built-in scheduler route must be runnable without AgentRegistry registration")
	}
	if registry.calls != 0 {
		t.Fatalf("scheduler route unexpectedly queried registry %d times", registry.calls)
	}
	if resolver.HasRunnableRoute(&model.Task{EventType: "missing"}) {
		t.Fatal("unregistered ordinary route reported runnable")
	}
	registry.available = true
	if !resolver.HasRunnableRoute(&model.Task{EventType: "explore"}) {
		t.Fatal("registered ordinary route reported unavailable")
	}
}

func TestRuntimeRouteResolverUsesNamespacedTaskAndGraphScope(t *testing.T) {
	registry := &fakePlanRouteRegistry{available: true}
	resolver := NewRuntimeRouteResolver(registry)
	legacy := &model.Task{
		EventType: "team:legacy", ParentTaskID: "controller-1",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}},
	}
	if !resolver.HasRunnableRoute(legacy) {
		t.Fatal("legacy Team route should be runnable")
	}
	if registry.owner != model.TaskRouteScope("controller-1") || registry.eventType != "team:legacy" ||
		len(registry.required) != 1 || registry.required[0] != "read_file" {
		t.Fatalf("legacy route probe mismatch: owner=%q event=%q required=%v", registry.owner, registry.eventType, registry.required)
	}

	graphTask := &model.Task{EventType: "team:graph", GraphID: "g1"}
	if !resolver.HasRunnableRoute(graphTask) {
		t.Fatal("Graph Team route should be runnable")
	}
	if registry.owner != model.GraphRouteScope("g1") || registry.eventType != "team:graph" {
		t.Fatalf("Graph route probe mismatch: owner=%q event=%q", registry.owner, registry.eventType)
	}

	explicit := &model.Task{EventType: "team:explicit", GraphID: "g-provenance", RouteScope: model.GraphRouteScope("g-frozen")}
	if !resolver.HasRunnableRoute(explicit) || registry.owner != model.GraphRouteScope("g-frozen") {
		t.Fatalf("durable RouteScope must override provenance fallback: owner=%q", registry.owner)
	}
}

func TestPendingGraceFallbackIsIndependentFromProcessingTimeout(t *testing.T) {
	w, _, _ := newTestWatchdog()
	w.Config.Infra.Store.DefaultTimeoutSec = 1
	w.Config.Infra.Watchdog.PendingAlertGraceSec = 0
	w.Config.Infra.Watchdog.UnroutableGraceSec = 0

	want := 300 * time.Second
	if got := w.claimGrace(); got != want {
		t.Fatalf("claimGrace = %v, want independent fallback %v", got, want)
	}
	if got := w.unroutableGrace(); got != want {
		t.Fatalf("unroutableGrace = %v, want independent fallback %v", got, want)
	}
}

func TestWatchdog_PrunesObservationForTaskNoLongerInStore(t *testing.T) {
	w, _, _ := newTestWatchdog()
	w.observePending("evicted-task", pendingObservationRoutable, time.Now())
	w.RunOnce()

	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	if len(w.pendingObservations) != 0 {
		t.Fatalf("stale observations were not pruned: %+v", w.pendingObservations)
	}
}

func TestWatchdog_TimeoutDetection(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:    "timeout task",
		TimeoutSeconds: 1, // 1 second timeout
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	// Manipulate StartedAt to simulate timeout
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-5*time.Second))

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed (timeout)", got.Status)
	}
	if got.Error == "" {
		t.Error("task.Error is empty, want timeout reason")
	}
}

func TestWatchdog_NoFalsePositive(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:    "healthy task",
		TimeoutSeconds: 300,
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing (no timeout)", got.Status)
	}
}

func TestWatchdog_OldCreatedAtDoesNotAgeFreshPendingLease(t *testing.T) {
	w, s, ch := newTestWatchdog()
	w.Config.Infra.Watchdog.PendingAlertGraceSec = 1

	task := &model.Task{Description: "fresh retry lease"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	freshPending := time.Now()
	setTaskTiming(t, s, task.ID, freshPending.Add(-24*time.Hour), time.Time{})
	setTaskPendingSince(t, s, task.ID, freshPending)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending; immutable CreatedAt must not drive queue timeout", got.Status)
	}
	for _, evt := range drainEvents(ch) {
		if evt.Type == model.EventWatchdogAlert {
			t.Fatalf("fresh pending lease emitted watchdog alert: %+v", evt)
		}
	}
}

func TestWatchdog_RoutableQueueWaitAlertsOnceWithoutFailure(t *testing.T) {
	w, s, ch := newTestWatchdog()
	w.Config.Infra.Watchdog.PendingAlertGraceSec = 10
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	task := &model.Task{Description: "queued behind busy compatible runner", EventType: "explore"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	setTaskPendingSince(t, s, task.ID, now.Add(-11*time.Second))

	inspectAll(w)
	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusPending {
		t.Fatalf("status = %s, want pending; route backlog is not task failure", got.Status)
	}
	alerts := 0
	for _, evt := range drainEvents(ch) {
		if evt.Type != model.EventWatchdogAlert {
			continue
		}
		alerts++
		if evt.Payload["reason_code"] != "claim_starvation" {
			t.Errorf("reason_code = %q, want claim_starvation", evt.Payload["reason_code"])
		}
	}
	if alerts != 1 {
		t.Fatalf("watchdog alerts = %d, want exactly one per pending lease", alerts)
	}
}

func TestWatchdog_RetryRearmsClaimAlertFromNewPendingLease(t *testing.T) {
	w, s, ch := newTestWatchdog()
	w.Config.Infra.Watchdog.PendingAlertGraceSec = 10
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	task := &model.Task{Description: "retry gets a new queue lease"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	setTaskPendingSince(t, s, task.ID, now.Add(-11*time.Second))
	inspectAll(w)
	if alerts := watchdogAlerts(drainEvents(ch)); len(alerts) != 1 || alerts[0].Payload["reason_code"] != "claim_starvation" {
		t.Fatalf("first lease alerts = %+v, want one claim_starvation", alerts)
	}

	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryRollback("worker-1", task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	retried, _ := s.GetTask(task.ID)
	if retried.PendingSince.IsZero() {
		t.Fatal("RetryRollback did not establish a new PendingSince")
	}

	now = retried.PendingSince.Add(9 * time.Second)
	inspectAll(w)
	if alerts := watchdogAlerts(drainEvents(ch)); len(alerts) != 0 {
		t.Fatalf("new pending lease alerted too early: %+v", alerts)
	}
	now = retried.PendingSince.Add(11 * time.Second)
	inspectAll(w)
	alerts := watchdogAlerts(drainEvents(ch))
	if len(alerts) != 1 || alerts[0].Payload["reason_code"] != "claim_starvation" {
		t.Fatalf("second lease alerts = %+v, want one re-armed claim_starvation", alerts)
	}
}

func TestWatchdog_UnroutableTaskBlocksAfterIndependentGrace(t *testing.T) {
	w, s, ch := newTestWatchdog()
	w.RouteResolver = RouteResolverFunc(func(*model.Task) bool { return false })
	w.Config.Infra.Watchdog.UnroutableGraceSec = 10
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	task := &model.Task{Description: "no listener", EventType: "missing-route"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	inspectAll(w) // starts the independent no-route observation
	now = now.Add(10 * time.Second)
	inspectAll(w)
	if got, _ := s.GetTask(task.ID); got.Status != model.TaskStatusPending {
		t.Fatalf("status at grace boundary = %s, want pending", got.Status)
	}

	now = now.Add(time.Second)
	inspectAll(w)
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusBlocked {
		t.Fatalf("status = %s, want blocked after no-route grace", got.Status)
	}
	if got.Error == "" || !strings.Contains(got.Error, "no_compatible_route") {
		t.Fatalf("blocked reason = %q, want no_compatible_route", got.Error)
	}
	foundAlert := false
	for _, evt := range drainEvents(ch) {
		if evt.Type == model.EventWatchdogAlert && evt.Payload["reason_code"] == "no_compatible_route" {
			foundAlert = true
		}
	}
	if !foundAlert {
		t.Fatal("missing structured no_compatible_route watchdog alert")
	}
}

func TestWatchdog_DependencyWaitDoesNotConsumeNoRouteGrace(t *testing.T) {
	w, s, _ := newTestWatchdog()
	w.RouteResolver = RouteResolverFunc(func(task *model.Task) bool { return task.EventType == "" })
	w.Config.Infra.Watchdog.UnroutableGraceSec = 10
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	dep := &model.Task{Description: "dependency"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatal(err)
	}
	task := &model.Task{
		Description: "waits normally", EventType: "missing-route",
		Dependencies: []string{dep.ID},
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	inspectAll(w)
	now = now.Add(time.Hour)
	inspectAll(w)
	if got, _ := s.GetTask(task.ID); got.Status != model.TaskStatusPending {
		t.Fatalf("dependency wait status = %s, want pending", got.Status)
	}

	if err := s.ClaimTask("dep-runner", dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult("dep-runner", dep.ID, "done"); err != nil {
		t.Fatal(err)
	}
	inspectAll(w) // no-route grace starts only now
	if got, _ := s.GetTask(task.ID); got.Status != model.TaskStatusPending {
		t.Fatalf("status immediately after dependency completion = %s, want pending", got.Status)
	}
	now = now.Add(11 * time.Second)
	inspectAll(w)
	if got, _ := s.GetTask(task.ID); got.Status != model.TaskStatusBlocked {
		t.Fatalf("status after independent no-route grace = %s, want blocked", got.Status)
	}
}

func TestWatchdog_CascadeCancellation(t *testing.T) {
	w, s, _ := newTestWatchdog()

	dep := &model.Task{Description: "dep task"}
	s.PublishTask(dep)
	// Fail the dependency
	s.TransitionState(dep.ID, model.TaskStatusPending, model.TaskStatusFailed)

	task := &model.Task{
		Description:  "depends on dep",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (cascade)", got.Status)
	}
}

func TestWatchdog_BlockedDependencyBlocksDownstreamWithReason(t *testing.T) {
	w, s, ch := newTestWatchdog()

	dep := &model.Task{Description: "unroutable dependency"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionState(dep.ID, model.TaskStatusPending, model.TaskStatusBlocked); err != nil {
		t.Fatal(err)
	}
	task := &model.Task{
		Description:  "downstream must not wait forever",
		Dependencies: []string{dep.ID},
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusBlocked {
		t.Fatalf("status = %s, want blocked when dependency is blocked", got.Status)
	}
	if !strings.Contains(got.Error, "dependency_blocked") || !strings.Contains(got.Error, dep.ID) {
		t.Fatalf("blocked reason = %q, want dependency_blocked with dependency ID", got.Error)
	}
	found := false
	for _, evt := range watchdogAlerts(drainEvents(ch)) {
		if evt.TaskID == task.ID && evt.Payload["reason_code"] == "dependency_blocked" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing structured dependency_blocked watchdog alert")
	}
}

func TestWatchdog_ContextCancellation(t *testing.T) {
	w, _, _ := newTestWatchdog()
	w.Config.Infra.Watchdog.IntervalSec = 1

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not stop")
	}
}

func TestWatchdog_RosterCleanup(t *testing.T) {
	w, s, _ := newTestWatchdog()
	r := w.Roster.(*roster.MemoryRoster)

	// 创建一个已完成的任务，代理仍有花名册声明（模拟 defer 未执行）
	task := &model.Task{Description: "done task"}
	s.PublishTask(task)
	s.ClaimTask("agent-stale", task.ID)
	s.SubmitResult("agent-stale", task.ID, "result")

	// 代理残留花名册声明
	r.TryClaim("agent-stale", "/path/to/file.go")

	// 确认声明存在
	claims, _ := r.ListByAgent("agent-stale")
	if len(claims) != 1 {
		t.Fatalf("expected 1 claim before cleanup, got %d", len(claims))
	}

	// 运行巡检
	w.RunOnce()

	// 声明应被清理（agent-stale 不在任何 processing 任务中）
	claims, _ = r.ListByAgent("agent-stale")
	if len(claims) != 0 {
		t.Errorf("expected 0 claims after cleanup, got %d", len(claims))
	}
}

func TestWatchdog_RosterCleanup_ActiveAgentPreserved(t *testing.T) {
	w, s, _ := newTestWatchdog()
	r := w.Roster.(*roster.MemoryRoster)

	// 创建一个正在执行的任务
	task := &model.Task{Description: "active task", TimeoutSeconds: 300}
	s.PublishTask(task)
	s.ClaimTask("agent-active", task.ID)

	// 代理有花名册声明
	r.TryClaim("agent-active", "/path/to/file.go")

	w.RunOnce()

	// 活跃代理的声明应保留
	claims, _ := r.ListByAgent("agent-active")
	if len(claims) != 1 {
		t.Errorf("expected 1 claim preserved for active agent, got %d", len(claims))
	}
}

func TestWatchdog_TaskSnapshotsCannotRewriteCompletedDependency(t *testing.T) {
	w, s, _ := newTestWatchdog()

	// 创建依赖任务，先让它 completed 以便后续任务能 ClaimTask
	dep := &model.Task{Description: "dep task"}
	s.PublishTask(dep)
	s.ClaimTask("setup", dep.ID)
	s.SubmitResult("setup", dep.ID, "done")

	// 创建并领取依赖 dep 的任务
	task := &model.Task{
		Description:    "processing depends on dep",
		Dependencies:   []string{dep.ID},
		TimeoutSeconds: 300,
	}
	s.PublishTask(task)
	s.ClaimTask("agent-1", task.ID)

	// 确认任务在 processing 状态
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Fatalf("precondition: status = %s, want processing", got.Status)
	}

	// 读取结果是不可变快照；外部不能把已经完成的历史事实改写为 failed。
	depTask, _ := s.GetTask(dep.ID)
	depTask.Status = model.TaskStatusFailed

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing because completed dependency is immutable", got.Status)
	}
}

func TestWatchdog_CascadeCancellation_MissingDep(t *testing.T) {
	w, s, _ := newTestWatchdog()

	task := &model.Task{
		Description:  "depends on missing",
		Dependencies: []string{"nonexistent-id"},
	}
	s.PublishTask(task)

	inspectAll(w)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (missing dep)", got.Status)
	}
}

// TestWatchdogStruct_HasMailRegistryForCrashReports 是 2026-04-23 随机测试
// 暴露的 P1 "Scheduler 对 watchdog-timeout 无感知"回归锁。
//
// 现象：watchdog 超时杀任务时，当前仅执行 FailTaskBySystem + sendAlert（发
// EventWatchdogAlert 到 EventCh），但**没有**给 task.EventSource（通常是
// scheduler）发任何邮件说明"谁死了 / 为什么死"。
//
// 连锁后果（2026-04-23 实测）：scheduler 的 LLM 看到子任务 status=failed，
// 但缺失"为什么失败"的上下文（是被 watchdog 超时杀了？还是 agent 自己
// handleFailure 失败的？），倾向于盲目重新 publish 类似任务。7 个子任务
// 连续死于同一模式却没收到任何"换策略吧"的信号。
//
// 本测试在修复前 🔴 RED：断言 Watchdog 结构体含可发邮件字段，用于向
// task.EventSource 发送 type=info, priority=high 的崩溃汇报邮件，
// 内容应至少包含：任务 ID、超时原因、elapsed、last known activity（最后
// 一次 tool call 的名字和时间，来自 store.GetToolCallHistory）。
//
// 修复方向（与 2026-04-08 历史修复记录的 agent.sendCrashReport 对称）：
//  1. Watchdog 加字段 MailRegistry（或 Mailbox/Notifier 等命名）
//  2. bootstrap.go 在 New Watchdog 时注入 mbRegistry
//  3. checkProcessingTask 超时分支在 FailTaskBySystem 之后，用 EventSource
//     作为收件人发 type=info/priority=high 邮件
//  4. 类似覆盖依赖失败级联取消 / unclaimed timeout 两条路径
func TestWatchdogStruct_HasMailRegistryForCrashReports(t *testing.T) {
	w := &Watchdog{}
	typ := reflect.TypeOf(w).Elem()
	// 可接受的字段名（命名留给实施阶段选择）
	candidates := []string{"MailRegistry", "MailSender", "Mailbox", "Mails", "Notifier"}
	for _, name := range candidates {
		if _, ok := typ.FieldByName(name); ok {
			return
		}
	}
	t.Fatalf("Watchdog 应持有可发邮件的字段（候选名 %v 之一）用于超时时向 task.EventSource 发送崩溃汇报邮件。"+
		"当前结构体仅含 %d 个字段（Store/Config/EventCh/Roster），scheduler 在子任务超时时缺失 `为什么死` 的上下文，"+
		"会盲目重试同样策略。见 2026-04-23 历史问题记录 P1",
		candidates, typ.NumField())
}
