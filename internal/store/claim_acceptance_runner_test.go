package store

// claim_acceptance_runner_test.go 覆盖 2026-07-27 真实运行事故的修复：
// 验收 runner（AcceptanceRunID 非空）的认领期依赖检查与验收语义对齐——
// 依赖只需终态（VerifyAcceptance 对非 pass 裁决本就允许 failed 依赖）。
// 修复前含失败依赖的验收任务永远无法被认领，plan 楔死。

import (
	"errors"
	"testing"

	"agentgo/internal/model"
)

// publishDepWithStatus 发布一个依赖任务并推到指定状态。
func publishDepWithStatus(t *testing.T, s *MemoryTaskStore, id string, status model.TaskStatus) {
	t.Helper()
	dep := &model.Task{ID: id, Description: "dep-" + id}
	if err := s.PublishTask(dep); err != nil {
		t.Fatalf("PublishTask(%s): %v", id, err)
	}
	if status == model.TaskStatusPending {
		return
	}
	if err := TransitionStateWithCancelSource(s, id, model.TaskStatusPending, status, "test"); err != nil {
		t.Fatalf("TransitionStateWithCancelSource(%s → %s): %v", id, status, err)
	}
}

func TestClaimTask_AcceptanceRunnerAllowsTerminalFailedDependency(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 4, 60)
	publishDepWithStatus(t, s, "dep-failed", model.TaskStatusFailed)

	runner := &model.Task{
		ID: "runner-1", Description: "验收 runner",
		Dependencies: []string{"dep-failed"}, AcceptanceRunID: "run-1",
	}
	if err := s.PublishTask(runner); err != nil {
		t.Fatalf("PublishTask(runner): %v", err)
	}
	if err := s.ClaimTask("verifier", "runner-1"); err != nil {
		t.Fatalf("验收 runner 依赖 failed（终态）应可认领，实际: %v", err)
	}
}

func TestClaimTask_AcceptanceRunnerRejectsNonTerminalDependency(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 4, 60)
	publishDepWithStatus(t, s, "dep-processing", model.TaskStatusProcessing)

	runner := &model.Task{
		ID: "runner-2", Description: "验收 runner",
		Dependencies: []string{"dep-processing"}, AcceptanceRunID: "run-2",
	}
	if err := s.PublishTask(runner); err != nil {
		t.Fatalf("PublishTask(runner): %v", err)
	}
	if err := s.ClaimTask("verifier", "runner-2"); !errors.Is(err, ErrDependencyNotMet) {
		t.Fatalf("依赖 processing（非终态）应拒绝认领，实际: %v", err)
	}
}

func TestClaimTask_AcceptanceRunnerAllowsCancelledAndCompletedDependencies(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 4, 60)
	publishDepWithStatus(t, s, "dep-cancelled", model.TaskStatusCancelled)
	publishDepWithStatus(t, s, "dep-pending-completed", model.TaskStatusPending)
	if err := s.ClaimTask("worker", "dep-pending-completed"); err != nil {
		t.Fatalf("ClaimTask(dep): %v", err)
	}
	if err := s.SubmitResult("worker", "dep-pending-completed", "done"); err != nil {
		t.Fatalf("SubmitResult(dep): %v", err)
	}

	runner := &model.Task{
		ID: "runner-3", Description: "验收 runner",
		Dependencies: []string{"dep-cancelled", "dep-pending-completed"}, AcceptanceRunID: "run-3",
	}
	if err := s.PublishTask(runner); err != nil {
		t.Fatalf("PublishTask(runner): %v", err)
	}
	if err := s.ClaimTask("verifier", "runner-3"); err != nil {
		t.Fatalf("依赖 cancelled/completed 均应可认领，实际: %v", err)
	}
}

// 普通任务的语义不变：failed 依赖仍然拒绝认领（修复不得放宽非验收路径）。
func TestClaimTask_NormalTaskStillRejectsFailedDependency(t *testing.T) {
	s := NewMemoryTaskStore(nil, 16, 4, 60)
	publishDepWithStatus(t, s, "dep-failed", model.TaskStatusFailed)

	normal := &model.Task{ID: "normal-1", Description: "普通任务", Dependencies: []string{"dep-failed"}}
	if err := s.PublishTask(normal); err != nil {
		t.Fatalf("PublishTask(normal): %v", err)
	}
	if err := s.ClaimTask("worker", "normal-1"); !errors.Is(err, ErrDependencyNotMet) {
		t.Fatalf("普通任务的 failed 依赖应仍拒绝认领，实际: %v", err)
	}
}
