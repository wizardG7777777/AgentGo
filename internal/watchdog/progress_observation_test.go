package watchdog

import (
	"errors"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
)

type fakeProgressReader struct {
	checkpoint *loopcontract.ProgressCheckpoint
	ok         bool
	err        error
	calls      int
}

func (f *fakeProgressReader) LoadCheckpoint(string) (*loopcontract.ProgressCheckpoint, bool, error) {
	f.calls++
	return f.checkpoint, f.ok, f.err
}

func publishLoopTask(t *testing.T, s interface {
	PublishTask(*model.Task) error
	ClaimTask(string, string) error
	GetTask(string) (*model.Task, error)
}, now time.Time) *model.Task {
	t.Helper()
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: "run-watchdog", CreatedAt: now.Add(-time.Minute),
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
	}
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressInvestigationV1)
	if !ok {
		t.Fatal("缺少 investigation progress contract")
	}
	task := &model.Task{
		RunID: run.RunID, RunContract: run,
		RunPhase: runcontract.PhaseExecution, ProgressContract: &profile.Contract,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent,
		Description:      "新 Loop 任务", TimeoutSeconds: 1,
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func validWatchdogCheckpoint(task *model.Task, updatedAt, now time.Time) *loopcontract.ProgressCheckpoint {
	return &loopcontract.ProgressCheckpoint{
		Schema: loopcontract.CheckpointSchemaV1, CheckpointID: "checkpoint-1", Version: 1,
		RunID: task.RunID, TaskID: task.ID, AttemptID: task.AttemptID,
		Contract: loopcontract.ProgressContractRef{
			ContractID: "progress:test/v1", ContractDigest: "sha256:test", PolicyRef: "policy:test/v1",
		},
		LastAnyProgressAt: updatedAt, LastDeliverableProgressAt: updatedAt,
		InterventionStage: loopcontract.StageRunning,
		Deadlines: loopcontract.DeadlineSet{
			Run: runcontract.DeadlineBudget{
				Scope: runcontract.ScopeRun, HardDeadlineAt: now.Add(time.Hour),
				FinalizationReserve: time.Minute, RecoveryReserve: time.Minute,
			},
			Attempt: runcontract.DeadlineBudget{
				Scope: runcontract.ScopeAttempt, HardDeadlineAt: now.Add(30 * time.Minute),
			},
		},
		UpdatedAt: updatedAt,
	}
}

func watchdogObservations(events []model.Event) []model.Event {
	var observations []model.Event
	for _, event := range events {
		if event.Type == model.EventWatchdogObservation {
			observations = append(observations, event)
		}
	}
	return observations
}

func TestWatchdogNewLoopIgnoresLegacyTimeoutWhenCheckpointFresh(t *testing.T) {
	w, s, ch := newTestWatchdog()
	now := time.Now().UTC()
	w.now = func() time.Time { return now }
	task := publishLoopTask(t, s, now)
	setTaskTiming(t, s, task.ID, now.Add(-time.Minute), now.Add(-10*time.Second))
	reader := &fakeProgressReader{checkpoint: validWatchdogCheckpoint(task, now.Add(-time.Second), now), ok: true}
	w.ProgressReader = reader

	inspectAll(w)
	events := drainEvents(ch)
	if alerts := watchdogAlerts(events); len(alerts) != 0 {
		t.Fatalf("新 Loop 不应产生 legacy processing_overtime: %+v", alerts)
	}
	if observations := watchdogObservations(events); len(observations) != 0 {
		t.Fatalf("新鲜 checkpoint 不应产生 liveness observation: %+v", observations)
	}
	got, err := s.GetTask(task.ID)
	if err != nil || got.Status != model.TaskStatusProcessing {
		t.Fatalf("Watchdog 不得迁移新 Loop Task: status=%v err=%v", got.Status, err)
	}
}

func TestWatchdogPublishesTypedHeartbeatStalledWithoutTransition(t *testing.T) {
	w, s, ch := newTestWatchdog()
	now := time.Now().UTC()
	w.now = func() time.Time { return now }
	task := publishLoopTask(t, s, now)
	checkpoint := validWatchdogCheckpoint(task, now.Add(-3*time.Minute), now)
	reader := &fakeProgressReader{checkpoint: checkpoint, ok: true}
	w.ProgressReader = reader

	inspectAll(w)
	observations := watchdogObservations(drainEvents(ch))
	if len(observations) != 1 || observations[0].Observation == nil {
		t.Fatalf("typed heartbeat observation=%+v，期望 1 条", observations)
	}
	got := observations[0].Observation
	if got.Kind != model.WatchdogHeartbeatStalled || got.CheckpointState != model.WatchdogCheckpointStale ||
		got.CheckpointID != checkpoint.CheckpointID || got.InterventionStage != loopcontract.StageRunning {
		t.Fatalf("heartbeat observation 字段错误: %+v", got)
	}
	if taskState, err := s.GetTask(task.ID); err != nil || taskState.Status != model.TaskStatusProcessing {
		t.Fatalf("heartbeat 观测不得迁移 Task: status=%v err=%v", taskState.Status, err)
	}

	inspectAll(w)
	if duplicate := watchdogObservations(drainEvents(ch)); len(duplicate) != 0 {
		t.Fatalf("同一 checkpoint 不应重复报告: %+v", duplicate)
	}
	checkpoint.CheckpointID = "checkpoint-2"
	checkpoint.Version++
	inspectAll(w)
	if rearmed := watchdogObservations(drainEvents(ch)); len(rearmed) != 1 {
		t.Fatalf("新 checkpoint 应重新武装 heartbeat 观测: %+v", rearmed)
	}
}

func TestWatchdogPublishesTypedHardDeadlineRiskFromCheckpoint(t *testing.T) {
	w, s, ch := newTestWatchdog()
	now := time.Now().UTC()
	w.now = func() time.Time { return now }
	task := publishLoopTask(t, s, now)
	checkpoint := validWatchdogCheckpoint(task, now.Add(-time.Second), now)
	checkpoint.Deadlines.Attempt.HardDeadlineAt = now.Add(30 * time.Second)
	checkpoint.Deadlines.Attempt.InterventionAt = now.Add(-time.Second)
	w.ProgressReader = &fakeProgressReader{checkpoint: checkpoint, ok: true}

	inspectAll(w)
	events := drainEvents(ch)
	if alerts := watchdogAlerts(events); len(alerts) != 0 {
		t.Fatalf("deadline 风险不得走 legacy 文本 wake: %+v", alerts)
	}
	observations := watchdogObservations(events)
	if len(observations) != 1 || observations[0].Observation == nil {
		t.Fatalf("hard deadline observation=%+v，期望 1 条", observations)
	}
	got := observations[0].Observation
	if got.Kind != model.WatchdogHardDeadlineRisk || got.DeadlineScope != runcontract.ScopeAttempt ||
		got.DeadlineState != model.WatchdogDeadlineAtRisk || !got.HardDeadlineAt.Equal(checkpoint.Deadlines.Attempt.HardDeadlineAt) {
		t.Fatalf("hard deadline observation 字段错误: %+v", got)
	}
	if taskState, err := s.GetTask(task.ID); err != nil || taskState.Status != model.TaskStatusProcessing {
		t.Fatalf("deadline 风险观测不得迁移/取消 Task: status=%v err=%v", taskState.Status, err)
	}

	// 到达同一 typed hard deadline 后以 exceeded 状态再报告一次，但仍不接管
	// L4 状态迁移或 caller cancellation authority。
	w.now = func() time.Time { return now.Add(31 * time.Second) }
	inspectAll(w)
	exceeded := watchdogObservations(drainEvents(ch))
	if len(exceeded) != 1 || exceeded[0].Observation == nil ||
		exceeded[0].Observation.Kind != model.WatchdogHardDeadlineRisk ||
		exceeded[0].Observation.DeadlineState != model.WatchdogDeadlineExceeded {
		t.Fatalf("hard deadline exceeded observation=%+v", exceeded)
	}
	if taskState, err := s.GetTask(task.ID); err != nil || taskState.Status != model.TaskStatusProcessing {
		t.Fatalf("hard deadline exceeded 仍只能观测: status=%v err=%v", taskState.Status, err)
	}
}

func TestWatchdogMissingOrUnreadableCheckpointReportsAfterLease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state model.WatchdogCheckpointState
		read  *fakeProgressReader
	}{
		{name: "not_wired", state: model.WatchdogCheckpointMissing, read: nil},
		{name: "missing", state: model.WatchdogCheckpointMissing, read: &fakeProgressReader{}},
		{name: "read_error", state: model.WatchdogCheckpointReadError, read: &fakeProgressReader{err: errors.New("journal poisoned")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s, ch := newTestWatchdog()
			now := time.Now().UTC()
			w.now = func() time.Time { return now }
			task := publishLoopTask(t, s, now)
			setTaskTiming(t, s, task.ID, now.Add(-10*time.Minute), now.Add(-3*time.Minute))
			if tc.read != nil {
				w.ProgressReader = tc.read
			}

			inspectAll(w)
			observations := watchdogObservations(drainEvents(ch))
			if len(observations) != 1 || observations[0].Observation == nil ||
				observations[0].Observation.Kind != model.WatchdogHeartbeatStalled ||
				observations[0].Observation.CheckpointState != tc.state {
				t.Fatalf("不可用 checkpoint observation=%+v", observations)
			}
			if got, err := s.GetTask(task.ID); err != nil || got.Status != model.TaskStatusProcessing {
				t.Fatalf("不可用 checkpoint 只能观测: status=%v err=%v", got.Status, err)
			}
		})
	}
}

func TestWatchdogDoesNotObserveOrOverrideCallerCancelledTask(t *testing.T) {
	w, s, ch := newTestWatchdog()
	now := time.Now().UTC()
	w.now = func() time.Time { return now }
	task := publishLoopTask(t, s, now)
	w.ProgressReader = &fakeProgressReader{
		checkpoint: validWatchdogCheckpoint(task, now.Add(-10*time.Minute), now), ok: true,
	}
	if err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}
	_ = drainEvents(ch)

	inspectAll(w)
	if observations := watchdogObservations(drainEvents(ch)); len(observations) != 0 {
		t.Fatalf("caller 已取消的 Task 不应再产生 Loop liveness 观测: %+v", observations)
	}
	got, err := s.GetTask(task.ID)
	if err != nil || got.Status != model.TaskStatusCancelled {
		t.Fatalf("Watchdog 不得覆盖 caller cancellation: status=%v err=%v", got.Status, err)
	}
}
