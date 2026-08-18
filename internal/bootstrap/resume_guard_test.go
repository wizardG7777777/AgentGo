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

type resumeTraceCapture struct {
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

func (c *resumeTraceCapture) Dispatch(event trace.Event) {
	c.events = append(c.events, event)
}

// 2026-08 二期：进入会话（--resume / 解冻）不再自动续跑——恢复快照中的全部
// 非终态任务无条件阻断为 blocked（与 saved_at 新旧无关、无显式恢复豁免），
// 终态任务与邮箱快照原样保留（邮箱属历史上下文，只有任务不重新派发）。
func TestGuardRecoveredSnapshotNoAutoResume(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	original := &session.Snapshot{
		Version: 4,
		SavedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
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

	got, blocks := guardRecoveredSnapshotNoAutoResume(original, now)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%d, want 2", len(blocks))
	}
	for _, b := range blocks {
		if b.Cause != "no_auto_resume" {
			t.Fatalf("block cause=%q, want no_auto_resume", b.Cause)
		}
	}
	if got == original {
		t.Fatal("守卫不应原地改写 SessionManager 持有的恢复快照")
	}
	for _, index := range []int{0, 1} {
		task := got.Tasks[index]
		if task.Status != string(model.TaskStatusBlocked) {
			t.Fatalf("task %s status=%s, want blocked", task.ID, task.Status)
		}
		if !strings.Contains(task.Error, "no_auto_resume") {
			t.Fatalf("task %s error=%q, want no_auto_resume", task.ID, task.Error)
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
		t.Fatal("守卫不得通过共享 Lease 指针改写原始快照")
	}
	if len(got.Mailboxes) != 1 {
		t.Fatalf("邮箱属历史上下文应原样保留: %#v", got.Mailboxes)
	}
}

func TestGuardRecoveredSnapshotNoAutoResumeEdgeCases(t *testing.T) {
	if got, blocks := guardRecoveredSnapshotNoAutoResume(nil, time.Now()); got != nil || len(blocks) != 0 {
		t.Fatalf("nil 快照应原样返回: got=%v blocks=%d", got, len(blocks))
	}
	terminalOnly := &session.Snapshot{Tasks: []session.TaskSnapshot{
		{ID: "done", Status: string(model.TaskStatusCompleted)},
		{ID: "stopped", Status: string(model.TaskStatusCancelled)},
	}}
	got, blocks := guardRecoveredSnapshotNoAutoResume(terminalOnly, time.Now())
	if len(blocks) != 0 {
		t.Fatalf("全终态快照不应产生阻断: %#v", blocks)
	}
	if got.Tasks[0].Status != string(model.TaskStatusCompleted) || got.Tasks[1].Status != string(model.TaskStatusCancelled) {
		t.Fatalf("终态任务不得受影响: %+v", got.Tasks)
	}
}

func TestEmitResumeBlocksWritesBlockedTerminalTruth(t *testing.T) {
	capture := &resumeTraceCapture{}
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

	emitResumeBlocks([]resumeBlock{{
		TaskID:     "task-stale",
		PrevStatus: string(model.TaskStatusProcessing),
		Reason:     "no_auto_resume: test",
	}})

	if len(capture.events) != 0 {
		t.Fatalf("resume audit reached Reactor dispatcher: events=%d", len(capture.events))
	}
	content := readJSONLContents(t, traceDir)
	for _, want := range []string{"task_blocked", "task-stale", "no_auto_resume", "processing", "blocked"} {
		if !strings.Contains(content, want) {
			t.Fatalf("physical resume trace missing %q: %s", want, content)
		}
	}
}

// 恢复全程 dispatcher 保持分离：恢复自身的审计事件（含恢复期间产生的事件）
// 不得到达 Reactor 分发器；恢复成功后才安装运行时 dispatcher。
func TestRestoreRuntimeBeforeReactorActivationSuppressesReconcileDispatch(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 4), 32, 1, 60)

	preexisting := &resumeTraceCapture{}
	runtimeDispatcher := &resumeTraceCapture{}
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
	preexisting := &resumeTraceCapture{}
	runtimeDispatcher := &resumeTraceCapture{}
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
