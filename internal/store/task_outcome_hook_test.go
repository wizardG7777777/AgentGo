package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/taskcontract"
)

type coordinatorFake struct {
	store          *MemoryTaskStore
	steps          []string
	settleErr      error
	refreshedCalls int
}

func (f *coordinatorFake) PrepareTerminalIntent(TerminalOutcomeIntent) (string, error) {
	f.steps = append(f.steps, "prepare")
	return "terminal-intent:1", nil
}

func (f *coordinatorFake) SettleTerminalIntent(string) (TerminalCheckpointBinding, error) {
	f.steps = append(f.steps, "settle")
	if f.settleErr != nil {
		return TerminalCheckpointBinding{}, f.settleErr
	}
	task, err := f.store.GetTask("task-two-phase")
	if err != nil || task.Status != model.TaskStatusProcessing {
		return TerminalCheckpointBinding{}, errors.New("Task 在 settle 前已提前终态")
	}
	if err := f.store.RecordResultField(task.ID, "late", "forbidden"); err == nil {
		return TerminalCheckpointBinding{}, errors.New("TerminalIntent fence 未阻止 Result 改写")
	}
	if err := f.store.AppendToolCall(task.ID, ToolCallRecord{CallID: "late-call", ToolName: "read_file", Success: true}); err != nil {
		return TerminalCheckpointBinding{}, err
	}
	return TerminalCheckpointBinding{CheckpointRef: "checkpoint:sealed", CheckpointState: "sealed"}, nil
}

func (f *coordinatorFake) CommitTerminalOutcome(_ string, refreshed TerminalOutcomeIntent, _ TerminalCheckpointBinding) (string, error) {
	f.steps = append(f.steps, "outcome")
	f.refreshedCalls = len(refreshed.ToolCalls)
	return "outcome:sha256:two-phase", nil
}

func TestTerminalOutcomeHookRunsBeforeAllTerminalStates(t *testing.T) {
	tests := []struct {
		name   string
		status model.TaskStatus
		drive  func(*MemoryTaskStore, *model.Task) error
	}{
		{name: "completed", status: model.TaskStatusCompleted, drive: func(s *MemoryTaskStore, task *model.Task) error {
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				return err
			}
			return s.SubmitResultWithFields("agent-1", task.ID, "完成", map[string]string{"coverage": "full"})
		}},
		{name: "failed", status: model.TaskStatusFailed, drive: func(s *MemoryTaskStore, task *model.Task) error {
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				return err
			}
			return s.FailTask("agent-1", task.ID, "失败")
		}},
		{name: "blocked", status: model.TaskStatusBlocked, drive: func(s *MemoryTaskStore, task *model.Task) error {
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				return err
			}
			return s.CommitBlockedResult("agent-1", task.ID, "等待", map[string]string{"gap": "input"}, "缺输入", "waiting_input")
		}},
		{name: "cancelled-pre-attempt", status: model.TaskStatusCancelled, drive: func(s *MemoryTaskStore, task *model.Task) error {
			return s.TransitionStateWithCancelSource(task.ID, model.TaskStatusPending, model.TaskStatusCancelled, "user")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := NewMemoryTaskStore(nil, 16, 1, 60)
			task := &model.Task{ID: "task-" + test.name, Description: "任务"}
			if err := s.PublishTask(task); err != nil {
				t.Fatal(err)
			}
			var candidate *model.Task
			s.SetTerminalOutcomeHook(func(intent TerminalOutcomeIntent) (string, error) {
				candidate = intent.Task
				return "outcome:sha256:" + test.name, nil
			})
			if err := test.drive(s, task); err != nil {
				t.Fatalf("驱动终态: %v", err)
			}
			got, _ := s.GetTask(task.ID)
			if candidate == nil || candidate.Status != test.status || candidate.CompletedAt.IsZero() {
				t.Fatalf("hook 候选不完整: %+v", candidate)
			}
			if got.Status != test.status || got.OutcomeRef != "outcome:sha256:"+test.name {
				t.Fatalf("终态/ref 未一起生效: %+v", got)
			}
			if test.status == model.TaskStatusCancelled && candidate.AttemptID != "" {
				t.Fatalf("pre-attempt 取消不得伪造 AttemptID: %+v", candidate)
			}
		})
	}
}

func TestTerminalOutcomeHookFailureLeavesTaskUnchanged(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{ID: "task-atomic", Description: "任务"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetTask(task.ID)
	s.SetTerminalOutcomeHook(func(TerminalOutcomeIntent) (string, error) {
		return "", errors.New("fsync failed")
	})
	err := s.SubmitResultWithFields("agent-1", task.ID, "完成", map[string]string{"coverage": "full"})
	if err == nil || !strings.Contains(err.Error(), "fsync failed") {
		t.Fatalf("hook 失败必须上抛: %v", err)
	}
	after, _ := s.GetTask(task.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("hook 失败后 live Task 出现半状态:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestOutcomeRefSurvivesSnapshotAndRecoveredProjection(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{ID: "task-snapshot", Description: "任务", GraphDefinitionDigestVersion: "definition/v1"}
	if err := taskcontract.Start(task, loopcontract.WorkVerification, "test-authoring/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatalf("建立 authoring Task 运行契约: %v", err)
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRecoveredTaskOutcome(task.ID, "outcome:sha256:recovered", model.TaskStatusBlocked,
		map[string]string{"gap": "input"}, "缺输入", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	snapshot := s.ExportSnapshot()
	restored := NewMemoryTaskStore(nil, 16, 1, 60)
	if err := restored.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	got, _ := restored.GetTask(task.ID)
	if got.OutcomeRef != "outcome:sha256:recovered" || got.Status != model.TaskStatusBlocked || got.Results["gap"] != "input" ||
		got.GraphDefinitionDigestVersion != "definition/v1" {
		t.Fatalf("恢复 outcome 投影丢失: %+v", got)
	}
}

func TestTerminalOutcomeCoordinatorOrdersFenceSettlementOutcomeAndTaskCAS(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 1, 60)
	coordinator := &coordinatorFake{store: s}
	s.SetTerminalOutcomeCoordinator(coordinator)
	task := &model.Task{ID: "task-two-phase", Description: "任务"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult("agent-1", task.ID, "完成"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(task.ID)
	if strings.Join(coordinator.steps, ",") != "prepare,settle,outcome" || coordinator.refreshedCalls != 1 {
		t.Fatalf("两阶段顺序/settlement enrichment 错误: steps=%v calls=%d", coordinator.steps, coordinator.refreshedCalls)
	}
	if got.Status != model.TaskStatusCompleted || got.OutcomeRef != "outcome:sha256:two-phase" {
		t.Fatalf("Outcome 后 Task CAS 未生效: %+v", got)
	}
}

func TestTerminalOutcomeCoordinatorFailureKeepsFenceUntilRecovery(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 1, 60)
	coordinator := &coordinatorFake{store: s, settleErr: errors.New("pending action")}
	s.SetTerminalOutcomeCoordinator(coordinator)
	task := &model.Task{ID: "task-two-phase", Description: "任务"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult("agent-1", task.ID, "完成"); err == nil {
		t.Fatal("settlement 失败必须阻止 Task terminal")
	}
	current, _ := s.GetTask(task.ID)
	if current.Status != model.TaskStatusProcessing || current.OutcomeRef != "" {
		t.Fatalf("失败后 Task 不得提前终态: %+v", current)
	}
	if err := s.RecordResultField(task.ID, "late", "value"); err == nil || !strings.Contains(err.Error(), "TerminalIntent") {
		t.Fatalf("pending intent 后 fence 必须持续: %v", err)
	}
	if err := s.ApplyRecoveredTaskOutcome(task.ID, "outcome:sha256:recovered", model.TaskStatusCompleted,
		map[string]string{"agent-1": "完成"}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordResultField(task.ID, "late", "value"); !errors.Is(err, ErrTaskNotProcessing) {
		t.Fatalf("恢复后应清 fence 并由终态状态拒绝，实际 %v", err)
	}
}
