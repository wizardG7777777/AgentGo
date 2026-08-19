package watchdog

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// 2026-04-25 P1 行为测试：验证 watchdog 超时告警/级联取消时向任务的显式
// ReplyToAgentID（或安全的 legacy EventSource）发送结构化汇报邮件。
// 2026-08-19 修订：watchdog 不再杀死超时任务，超时路径改发「超时告警」
// 邮件（任务仍在运行）；级联取消路径仍发崩溃汇报。
//
// 覆盖三个语义层面：
//   - 超时路径发告警邮件（不杀死任务）
//   - 级联取消路径发崩溃汇报邮件
//   - EventSource="" 时静默跳过（顶层任务不打扰 user）

func newWatchdogWithMailbox(t *testing.T) (*Watchdog, store.TaskStore, *mailbox.Registry, *mailbox.Mailbox) {
	t.Helper()
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.Infra.Store.DefaultTimeoutSec = 300
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mbReg := mailbox.NewRegistry(16)
	schedBox := mbReg.Register("scheduler", "")
	w := New(s, cfg, ch, r, mbReg)
	return w, s, mbReg, schedBox
}

func TestWatchdog_SendsOvertimeWarning_OnTimeout(t *testing.T) {
	w, s, _, schedBox := newWatchdogWithMailbox(t)

	task := &model.Task{
		Description:    "超时告警验证任务",
		TimeoutSeconds: 1,
		EventSource:    "scheduler",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("publish task: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("claim task: %v", err)
	}
	// 把 StartedAt 回拨 10s 触发超时（阈值 = 1s）
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-10*time.Second))

	inspectAll(w)

	// 超时只告警，不杀死：任务保持 processing
	if got, _ := s.GetTask(task.ID); got.Status != model.TaskStatusProcessing {
		t.Fatalf("task.status = %s, want processing（watchdog 不再杀超时任务）", got.Status)
	}

	msgs := schedBox.Drain()
	if len(msgs) == 0 {
		t.Fatal("scheduler 应收到至少一条超时告警邮件，实际 0 条")
	}
	var found bool
	for _, m := range msgs {
		if m.From != "watchdog" {
			continue
		}
		if !strings.Contains(m.Content, task.ID) {
			t.Errorf("邮件正文应含完整 task ID %s；实际: %s", task.ID, m.Content)
		}
		if !strings.Contains(m.Content, "超时") && !strings.Contains(m.Summary, "超时") {
			t.Errorf("邮件应含 '超时' 关键字；summary=%q content=%q", m.Summary, m.Content)
		}
		if !strings.Contains(m.Content, "未终止") {
			t.Errorf("超时告警邮件应声明任务未被终止；content=%q", m.Content)
		}
		if m.Priority != mailbox.PriorityHigh {
			t.Errorf("超时告警邮件优先级应为 high，实际: %s", m.Priority)
		}
		if m.Type != mailbox.MsgTypeInfo {
			t.Errorf("超时告警邮件类型应为 info，实际: %s", m.Type)
		}
		found = true
	}
	if !found {
		t.Errorf("未找到 from=watchdog 的超时告警邮件；共收到 %d 条：%+v", len(msgs), msgs)
	}
}

func TestWatchdog_OvertimeWarningPrefersExplicitReplyRecipient(t *testing.T) {
	w, s, _, schedBox := newWatchdogWithMailbox(t)

	task := &model.Task{
		Description:    "explicit reply route",
		TimeoutSeconds: 1,
		ReplyToAgentID: "scheduler",
		EventSource:    "parent-task-id-is-not-a-mailbox",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-10*time.Second))

	inspectAll(w)

	msgs := schedBox.Drain()
	if len(msgs) != 1 || msgs[0].To != "scheduler" || !strings.Contains(msgs[0].Content, task.ID) {
		t.Fatalf("explicit reply recipient messages = %+v", msgs)
	}
}

func TestWatchdog_OvertimeWarningSkipsUnroutableLegacyEventSource(t *testing.T) {
	w, s, _, schedBox := newWatchdogWithMailbox(t)

	task := &model.Task{
		Description:    "legacy parent task ID",
		TimeoutSeconds: 1,
		EventSource:    "parent-task-id-is-not-a-mailbox",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-10*time.Second))

	inspectAll(w)

	if msgs := schedBox.Drain(); len(msgs) != 0 {
		t.Fatalf("unroutable legacy EventSource produced overtime warning: %+v", msgs)
	}
}

func TestWatchdog_OvertimeWarningNeverTargetsParentTaskID(t *testing.T) {
	w, s, mbReg, _ := newWatchdogWithMailbox(t)
	parentBox := mbReg.Register("parent-task-uuid", "")

	parent := &model.Task{ID: "parent-task-uuid", Description: "parent"}
	if err := s.PublishTask(parent); err != nil {
		t.Fatal(err)
	}
	task := &model.Task{
		ID: "child-task-uuid", Description: "legacy parent happened to have a mailbox",
		TimeoutSeconds: 1, EventSource: parent.ID, ParentTaskID: parent.ID,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-10*time.Second))

	inspectAll(w)

	if msgs := parentBox.Drain(); len(msgs) != 0 {
		t.Fatalf("parent Task ID received a watchdog overtime warning: %+v", msgs)
	}
}

func TestWatchdog_SendsCrashReport_OnCascadeCancel(t *testing.T) {
	w, s, _, schedBox := newWatchdogWithMailbox(t)

	// A 任务先失败
	taskA := &model.Task{Description: "A 先失败", EventSource: "scheduler"}
	if err := s.PublishTask(taskA); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	if err := s.TransitionState(taskA.ID, model.TaskStatusPending, model.TaskStatusFailed); err != nil {
		t.Fatalf("fail A: %v", err)
	}

	// B 任务依赖 A，pending 状态——下一次 inspect 时触发级联取消
	taskB := &model.Task{
		Description:  "B 等 A，会被级联取消",
		Dependencies: []string{taskA.ID},
		EventSource:  "scheduler",
	}
	if err := s.PublishTask(taskB); err != nil {
		t.Fatalf("publish B: %v", err)
	}

	inspectAll(w)

	// B 应该被 cancelled
	got, _ := s.GetTask(taskB.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("B.status = %s, want cancelled", got.Status)
	}

	// scheduler 收到的邮件：至少一条与 B 相关（可能也有 A 的，因为 A 本身是 failed 不触发——但保险起见过滤）
	msgs := schedBox.Drain()
	var bMsg *mailbox.Message
	for i := range msgs {
		if msgs[i].From == "watchdog" && strings.Contains(msgs[i].Content, taskB.ID) {
			bMsg = &msgs[i]
			break
		}
	}
	if bMsg == nil {
		t.Fatalf("未收到 B 的级联取消汇报邮件；全部 %d 条：%+v", len(msgs), msgs)
	}
	if !strings.Contains(bMsg.Content, "级联取消") {
		t.Errorf("B 的汇报邮件应含 '级联取消'；content=%q", bMsg.Content)
	}
	if !strings.Contains(bMsg.Content, taskA.ID) {
		t.Errorf("B 的汇报邮件应提到依赖 A 的 ID；content=%q", bMsg.Content)
	}
}

func TestWatchdog_SkipsOvertimeWarning_WhenEventSourceEmpty(t *testing.T) {
	w, s, _, schedBox := newWatchdogWithMailbox(t)

	// 顶层任务（无 EventSource）超时——不应发邮件（scheduler 自己就是顶层，没人可汇报）
	task := &model.Task{
		Description:    "顶层任务超时",
		TimeoutSeconds: 1,
		// EventSource intentionally left empty
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-10*time.Second))

	inspectAll(w)

	// 超时只告警不杀死：任务保持 processing
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Fatalf("task.status = %s, want processing（超时只告警）", got.Status)
	}

	// 但不应有任何邮件进入 scheduler 收件箱（因为 EventSource 为空）
	msgs := schedBox.Drain()
	for _, m := range msgs {
		if m.From == "watchdog" {
			t.Errorf("EventSource 为空时 watchdog 不应发超时告警；实际收到：%+v", m)
		}
	}
}

// TestWatchdog_SkipsOvertimeWarning_WhenEventSourceIsUser 确认 EventSource="user"
// 时也跳过——顶层人机交互任务超时不应骚扰用户邮箱（用户用 CLI 直接看状态）。
func TestWatchdog_SkipsOvertimeWarning_WhenEventSourceIsUser(t *testing.T) {
	w, s, mbReg, _ := newWatchdogWithMailbox(t)

	userBox := mbReg.Register("user", "")

	task := &model.Task{
		Description:    "用户发起的顶层任务超时",
		TimeoutSeconds: 1,
		EventSource:    "user",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	setTaskTiming(t, s, task.ID, time.Time{}, time.Now().Add(-10*time.Second))

	inspectAll(w)

	msgs := userBox.Drain()
	for _, m := range msgs {
		if m.From == "watchdog" {
			t.Errorf("EventSource='user' 时 watchdog 不应发超时告警；实际收到：%+v", m)
		}
	}
}
