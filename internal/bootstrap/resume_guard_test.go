package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

type staleResumeTraceCapture struct {
	events []trace.Event
}

func (c *staleResumeTraceCapture) Dispatch(event trace.Event) {
	c.events = append(c.events, event)
}

func TestProtectStaleAutomaticResume(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	original := &session.Snapshot{
		Version: 3,
		SavedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		Tasks: []session.TaskSnapshot{
			{ID: "pending", Status: string(model.TaskStatusPending), PendingSince: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)},
			{ID: "processing", Status: string(model.TaskStatusProcessing), Agents: []string{"worker-1"}},
			{ID: "completed", Status: string(model.TaskStatusCompleted)},
		},
		Mailboxes: []session.MailboxSnapshot{{
			OwnerID:  "worker-1",
			Messages: []session.MessageSnapshot{{From: "scheduler", To: "worker-1", Content: "old wake"}},
		}},
	}

	got, protected := protectStaleAutomaticResume(original, false, time.Hour, now)
	if len(protected) != 2 {
		t.Fatalf("protected=%d, want 2", len(protected))
	}
	if got == original {
		t.Fatal("陈旧保护不应原地改写 SessionManager 持有的恢复快照")
	}
	for _, index := range []int{0, 1} {
		task := got.Tasks[index]
		if task.Status != string(model.TaskStatusBlocked) {
			t.Fatalf("task %s status=%s, want blocked", task.ID, task.Status)
		}
		if !strings.Contains(task.Error, "stale_resume_guard") {
			t.Fatalf("task %s error=%q, want stale_resume_guard", task.ID, task.Error)
		}
		if len(task.Agents) != 0 || task.PendingSince != "" || task.CompletedAt == "" {
			t.Fatalf("task %s terminal fields not normalized: %+v", task.ID, task)
		}
	}
	if got.Tasks[2].Status != string(model.TaskStatusCompleted) {
		t.Fatalf("terminal task was changed: %+v", got.Tasks[2])
	}
	if original.Tasks[0].Status != string(model.TaskStatusPending) || original.Tasks[1].Status != string(model.TaskStatusProcessing) {
		t.Fatal("原始快照被改写")
	}
	if len(got.Mailboxes) != 0 {
		t.Fatalf("陈旧自动恢复不应重放 mailbox: %#v", got.Mailboxes)
	}
	if len(original.Mailboxes) != 1 {
		t.Fatal("原始 mailbox 快照被改写")
	}
}

func TestProtectStaleAutomaticResumeBypassesFreshExplicitAndDisabled(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	fresh := &session.Snapshot{
		SavedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		Tasks:   []session.TaskSnapshot{{ID: "pending", Status: string(model.TaskStatusPending)}},
	}
	if got, blocks := protectStaleAutomaticResume(fresh, false, time.Hour, now); got != fresh || len(blocks) != 0 {
		t.Fatalf("fresh snapshot changed: got=%p want=%p protected=%d", got, fresh, len(blocks))
	}

	old := &session.Snapshot{
		SavedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		Tasks:   []session.TaskSnapshot{{ID: "pending", Status: string(model.TaskStatusPending)}},
	}
	if got, blocks := protectStaleAutomaticResume(old, true, time.Hour, now); got != old || len(blocks) != 0 {
		t.Fatalf("explicit resume must bypass guard: got=%p want=%p protected=%d", got, old, len(blocks))
	}
	if got, blocks := protectStaleAutomaticResume(old, false, 0, now); got != old || len(blocks) != 0 {
		t.Fatalf("disabled guard changed snapshot: got=%p want=%p protected=%d", got, old, len(blocks))
	}
}

func TestProtectStaleAutomaticResumeFailsClosedOnInvalidSavedAt(t *testing.T) {
	snap := &session.Snapshot{
		SavedAt: "not-a-time",
		Tasks:   []session.TaskSnapshot{{ID: "pending", Status: string(model.TaskStatusPending)}},
	}
	got, protected := protectStaleAutomaticResume(snap, false, time.Hour, time.Now())
	if len(protected) != 1 || got.Tasks[0].Status != string(model.TaskStatusBlocked) {
		t.Fatalf("invalid saved_at must fail closed: protected=%d task=%+v", len(protected), got.Tasks[0])
	}
	if !strings.Contains(got.Tasks[0].Error, "saved_at") {
		t.Fatalf("reason must explain invalid saved_at: %q", got.Tasks[0].Error)
	}
}

func TestProtectStaleAutomaticResumeFailsClosedOnClearlyFutureSavedAt(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	snap := &session.Snapshot{
		SavedAt: now.Add(2 * time.Minute).Format(time.RFC3339Nano),
		Tasks:   []session.TaskSnapshot{{ID: "pending", Status: string(model.TaskStatusPending)}},
	}
	got, protected := protectStaleAutomaticResume(snap, false, time.Hour, now)
	if len(protected) != 1 || got.Tasks[0].Status != string(model.TaskStatusBlocked) {
		t.Fatalf("future saved_at must fail closed: protected=%d task=%+v", len(protected), got.Tasks[0])
	}
	if !strings.Contains(got.Tasks[0].Error, "超前") {
		t.Fatalf("reason must explain future saved_at: %q", got.Tasks[0].Error)
	}
}

func TestPrepareRecoveredSnapshotOverlaysPlanTerminalFactBeforeStaleGuard(t *testing.T) {
	_, coordinator := newPlannedStore(t, t.TempDir())
	p, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: "plan-terminal-overlay", RootTaskID: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err = coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{
			TaskID: "work-completed", Title: "completed before snapshot flush",
			Role: model.PlanNodeRoleImplementation, Status: model.TaskStatusProcessing,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RecordTaskMutation(context.Background(), p.ID, "work-completed", plan.TaskMutation{
		Status: model.TaskStatusCompleted,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	snap := &session.Snapshot{
		SavedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		Tasks: []session.TaskSnapshot{{
			ID: "work-completed", PlanID: p.ID,
			NodeRole:     string(model.PlanNodeRoleImplementation),
			Status:       string(model.TaskStatusPending),
			PendingSince: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		}},
	}
	prepared, blocks := prepareRecoveredSnapshot(&System{PlanCoordinator: coordinator}, snap, false, time.Hour, now)
	if len(blocks) != 0 {
		t.Fatalf("authoritative terminal Plan fact was stale-blocked: %#v", blocks)
	}
	if got := prepared.Tasks[0]; got.Status != string(model.TaskStatusCompleted) || got.PendingSince != "" || got.CompletedAt == "" {
		t.Fatalf("terminal Plan fact was not preserved: %+v", got)
	}
}

func TestEmitStaleResumeBlocksWritesBlockedTerminalTruth(t *testing.T) {
	capture := &staleResumeTraceCapture{}
	traceDir := t.TempDir()
	writer, err := trace.NewWriter(traceDir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	previousWriter := trace.Default()
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefault(writer)
	trace.SetDefaultDispatcher(capture)
	t.Cleanup(func() {
		trace.SetDefault(previousWriter)
		trace.SetDefaultDispatcher(previousDispatcher)
		_ = writer.Close()
	})

	emitStaleResumeBlocks([]staleResumeBlock{{
		TaskID:     "task-stale",
		PrevStatus: string(model.TaskStatusProcessing),
		Reason:     "stale_resume_guard: test",
	}})

	if len(capture.events) != 0 {
		t.Fatalf("stale recovery audit reached Reactor dispatcher: events=%d", len(capture.events))
	}
	content := readJSONLContents(t, traceDir)
	for _, want := range []string{"task_blocked", "task-stale", "stale_resume_guard", "processing", "blocked"} {
		if !strings.Contains(content, want) {
			t.Fatalf("physical stale resume trace missing %q: %s", want, content)
		}
	}
}

func TestRestoreRuntimeBeforeReactorActivationSuppressesReconcileDispatch(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	p, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: "plan-recovery-no-reactor", RootTaskID: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err = coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		Node: model.PlanNode{
			TaskID: "terminal-during-recovery", Title: "terminal snapshot",
			Role: model.PlanNodeRoleImplementation, Status: model.TaskStatusProcessing,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	preexisting := &staleResumeTraceCapture{}
	runtimeDispatcher := &staleResumeTraceCapture{}
	previousWriter := trace.Default()
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefault(nil)
	trace.SetDefaultDispatcher(preexisting)
	t.Cleanup(func() {
		trace.SetDefault(previousWriter)
		trace.SetDefaultDispatcher(previousDispatcher)
	})

	snap := &session.Snapshot{Tasks: []session.TaskSnapshot{{
		ID: "terminal-during-recovery", Description: "terminal snapshot",
		Status: string(model.TaskStatusBlocked), PlanID: p.ID,
		NodeRole:    string(model.PlanNodeRoleImplementation),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}}
	if err := restoreRuntimeBeforeReactorActivation(
		&System{Store: taskStore, PlanCoordinator: coordinator}, snap, nil, runtimeDispatcher,
	); err != nil {
		t.Fatalf("restoreRuntimeBeforeReactorActivation: %v", err)
	}
	recoveredPlan, err := coordinator.Store().GetPlan(p.ID)
	if err != nil || len(recoveredPlan.PendingReplanRequests) == 0 {
		t.Fatalf("test did not exercise recovery replan emission: plan=%+v err=%v", recoveredPlan, err)
	}
	if len(preexisting.events) != 0 || len(runtimeDispatcher.events) != 0 {
		t.Fatalf("recovery leaked to dispatcher: preexisting=%d runtime=%d", len(preexisting.events), len(runtimeDispatcher.events))
	}
	if trace.DefaultDispatcher() != runtimeDispatcher {
		t.Fatal("runtime dispatcher was not installed after successful recovery")
	}
	trace.Emit(trace.Event{Kind: trace.KindTaskPublished, TaskID: "after-recovery"})
	if len(runtimeDispatcher.events) != 1 || runtimeDispatcher.events[0].TaskID != "after-recovery" {
		t.Fatalf("runtime dispatcher did not activate after recovery: %#v", runtimeDispatcher.events)
	}
}

func TestRestoreRuntimeBeforeReactorActivationFailureLeavesDispatcherDetached(t *testing.T) {
	preexisting := &staleResumeTraceCapture{}
	runtimeDispatcher := &staleResumeTraceCapture{}
	previousWriter := trace.Default()
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefault(nil)
	trace.SetDefaultDispatcher(preexisting)
	t.Cleanup(func() {
		trace.SetDefault(previousWriter)
		trace.SetDefaultDispatcher(previousDispatcher)
	})

	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 1), 10, 1, 60)
	err := restoreRuntimeBeforeReactorActivation(
		&System{Store: taskStore},
		&session.Snapshot{Tasks: []session.TaskSnapshot{{
			ID: "malformed-recovery-task", Status: string(model.TaskStatusPending), CreatedAt: "not-a-time",
		}}},
		nil,
		runtimeDispatcher,
	)
	if err == nil || !strings.Contains(err.Error(), "restore tasks") {
		t.Fatalf("malformed recovery should fail closed: %v", err)
	}
	if trace.DefaultDispatcher() != nil {
		t.Fatal("runtime dispatcher was installed despite failed recovery")
	}
	if len(preexisting.events) != 0 || len(runtimeDispatcher.events) != 0 {
		t.Fatalf("failed recovery leaked events: preexisting=%d runtime=%d", len(preexisting.events), len(runtimeDispatcher.events))
	}
}

func TestRunPeriodicSnapshotsTicksAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPeriodicSnapshots(ctx, 5*time.Millisecond, func() { calls <- struct{}{} })
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("periodic snapshot did not tick")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("periodic snapshot loop did not stop with context")
	}
}
