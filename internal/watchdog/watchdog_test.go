package watchdog

import (
	"context"
	"reflect"
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
	w := New(s, cfg, ch, r, nil) // 既有测试不需要 MailRegistry；nil 让 sendCrashReport 静默跳过
	return w, s, ch
}

// inspectAll runs inspection on ALL tasks (not sampled) for deterministic testing.
func inspectAll(w *Watchdog) {
	tasks, _ := w.Store.ScanAll()
	for _, task := range tasks {
		w.checkTask(task)
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
	got, _ := s.GetTask(task.ID)
	got.StartedAt = time.Now().Add(-5 * time.Second)

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
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

func TestWatchdog_UnclaimedDetection(t *testing.T) {
	w, s, _ := newTestWatchdog()
	w.Config.Infra.Store.DefaultTimeoutSec = 1 // 1 second threshold for testing

	task := &model.Task{Description: "unclaimed task"}
	s.PublishTask(task)

	// Manipulate CreatedAt to simulate long wait
	got, _ := s.GetTask(task.ID)
	got.CreatedAt = time.Now().Add(-5 * time.Second)

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed (unclaimed)", got.Status)
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

func TestWatchdog_CascadeCancellation_Processing(t *testing.T) {
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

	// 现在将依赖任务的状态直接改为 failed（模拟依赖后续被判定失败的场景）
	depTask, _ := s.GetTask(dep.ID)
	depTask.Status = model.TaskStatusFailed

	inspectAll(w)

	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("status = %s, want cancelled (cascade from processing)", got.Status)
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
	typ := reflect.TypeOf(*w)
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
