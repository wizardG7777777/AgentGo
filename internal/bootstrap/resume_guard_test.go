package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentgo/internal/effect"
	"agentgo/internal/model"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

type staleResumeTraceCapture struct {
	events []trace.Event
}

func TestProtectUnknownEffectResumeQuarantinesMatchingNonTerminalTask(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	snap := &session.Snapshot{Tasks: []session.TaskSnapshot{
		{
			ID: "task-unknown", Status: string(model.TaskStatusProcessing), Agents: []string{"worker-1"},
			Lease: &session.LeaseSnapshot{BusinessTools: []string{"run_shell"}, Digest: "lease-unknown"},
		},
		{ID: "task-clean", Status: string(model.TaskStatusPending)},
		{ID: "task-done", Status: string(model.TaskStatusCompleted)},
	}}
	decisions := []effect.RecoveryDecision{
		{EffectID: "task-unknown-1", TaskID: "task-unknown", Kind: effect.KindShell, Decision: effect.DecisionKeptUnknownManual},
		// 已核验 settled 的裁决不得隔离任务。
		{EffectID: "task-clean-1", TaskID: "task-clean", Kind: effect.KindFileWrite, Decision: effect.DecisionVerifiedSettled},
	}

	guarded, blocks := protectUnknownEffectResume(snap, decisions, now)
	if len(blocks) != 1 || blocks[0].TaskID != "task-unknown" || blocks[0].Cause != "effect_recovery_unknown" {
		t.Fatalf("unknown Effect 应产生一条任务 quarantine: %+v", blocks)
	}
	got := guarded.Tasks[0]
	if got.Status != string(model.TaskStatusBlocked) ||
		!strings.Contains(got.Error, "effect_recovery_quarantine") ||
		!strings.Contains(got.Error, "task-unknown-1") ||
		len(got.Agents) != 0 || got.CompletedAt == "" {
		t.Fatalf("匹配的 processing 任务应被可见地 blocked: %+v", got)
	}
	if got.Lease == nil || !got.Lease.Revoked || got.Lease.Digest != "lease-unknown" {
		t.Fatalf("quarantine 必须撤销既有执行租约且保留 digest: %+v", got.Lease)
	}
	if guarded.Tasks[1].Status != string(model.TaskStatusPending) || guarded.Tasks[2].Status != string(model.TaskStatusCompleted) {
		t.Fatalf("已 settled/无 unknown 的任务不得受影响: %+v", guarded.Tasks)
	}
	if snap.Tasks[0].Status != string(model.TaskStatusProcessing) {
		t.Fatal("不得原地修改 SessionManager 持有的恢复快照")
	}
	if snap.Tasks[0].Lease == nil || snap.Tasks[0].Lease.Revoked {
		t.Fatal("不得通过共享 Lease 指针改写原始恢复快照")
	}
}

func TestUnresolvedEffectTaskReasonsKeepsOnlyUnknownDecisions(t *testing.T) {
	reasons := unresolvedEffectTaskReasons([]effect.RecoveryDecision{
		{EffectID: "task-unknown-1", TaskID: "task-unknown", Kind: effect.KindShell, Decision: effect.DecisionKeptUnknownManual},
		{EffectID: "task-unknown-2", TaskID: "task-unknown", Kind: effect.KindMessage, Decision: effect.DecisionReplayableHold},
		{EffectID: "task-settled-1", TaskID: "task-settled", Kind: effect.KindFileWrite, Decision: effect.DecisionVerifiedSettled},
		{EffectID: "missing-task", Kind: effect.KindShell, Decision: effect.DecisionKeptUnknownManual},
	})

	if len(reasons) != 1 {
		t.Fatalf("Graph quarantine 索引应只保留有 task_id 的 unresolved Effect: %#v", reasons)
	}
	reason := reasons["task-unknown"]
	for _, want := range []string{"effect_recovery_quarantine", "task-unknown-1", "task-unknown-2"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("Graph quarantine 原因缺少 %q: %q", want, reason)
		}
	}
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
			{
				ID: "pending", Status: string(model.TaskStatusPending), PendingSince: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
				Lease: &session.LeaseSnapshot{BusinessTools: []string{"write_file"}, Digest: "pending-lease"},
			},
			{
				ID: "processing", Status: string(model.TaskStatusProcessing), Agents: []string{"worker-1"},
				Lease: &session.LeaseSnapshot{ControlTools: []string{"submit_task_result"}, Digest: "processing-lease"},
			},
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
		if task.Lease == nil || !task.Lease.Revoked {
			t.Fatalf("task %s terminal blocked 后仍持活租约: %+v", task.ID, task.Lease)
		}
	}
	if got.Tasks[2].Status != string(model.TaskStatusCompleted) {
		t.Fatalf("terminal task was changed: %+v", got.Tasks[2])
	}
	if original.Tasks[0].Status != string(model.TaskStatusPending) || original.Tasks[1].Status != string(model.TaskStatusProcessing) {
		t.Fatal("原始快照被改写")
	}
	if original.Tasks[0].Lease.Revoked || original.Tasks[1].Lease.Revoked {
		t.Fatal("陈旧保护不得通过共享 Lease 指针改写原始快照")
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

// C6b 起 prepareRecoveredSnapshot 只做陈旧自动恢复守卫：不再有 Plan 终态事实
// overlay（控制面已随其整包删除），陈旧快照中的非终态任务一律阻断。
func TestPrepareRecoveredSnapshotAppliesStaleGuardWithoutOverlay(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	snap := &session.Snapshot{
		SavedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		Tasks: []session.TaskSnapshot{{
			ID: "work-pending", Status: string(model.TaskStatusPending),
			PendingSince: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		}},
	}
	prepared, blocks := prepareRecoveredSnapshot(&System{}, snap, false, time.Hour, now)
	if len(blocks) != 1 {
		t.Fatalf("陈旧快照的非终态任务应被阻断: %#v", blocks)
	}
	if got := prepared.Tasks[0]; got.Status != string(model.TaskStatusBlocked) || !strings.Contains(got.Error, "stale_resume_guard") {
		t.Fatalf("stale guard 未生效: %+v", got)
	}
	if snap.Tasks[0].Status != string(model.TaskStatusPending) {
		t.Fatal("原始快照被改写")
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

// 恢复全程 dispatcher 保持分离：恢复自身的审计事件（含恢复期间产生的事件）
// 不得到达 Reactor 分发器；恢复成功后才安装运行时 dispatcher。
func TestRestoreRuntimeBeforeReactorActivationSuppressesReconcileDispatch(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)

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
		ID: "restored-pending", Description: "恢复期间导入的任务",
		Status: string(model.TaskStatusPending),
	}}}
	if err := restoreRuntimeBeforeReactorActivation(
		&System{Store: taskStore}, snap, nil, runtimeDispatcher,
	); err != nil {
		t.Fatalf("restoreRuntimeBeforeReactorActivation: %v", err)
	}
	if _, err := taskStore.GetTask("restored-pending"); err != nil {
		t.Fatalf("快照任务应已导入: %v", err)
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
