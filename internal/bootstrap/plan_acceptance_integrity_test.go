package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

func TestSafeEvidencePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("not project evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "evidence.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if _, err := safeEvidencePath(root, "evidence.txt"); err == nil || !strings.Contains(err.Error(), "escapes project root") {
		t.Fatalf("symlink escape was accepted: %v", err)
	}
}

func TestFormalAcceptanceRejectsForgedTaskStatusEvidence(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())
	root := &model.Task{Description: "task status acceptance", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{
		Description: "implement", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.SubmitResult("worker", work.ID, "implemented"); err != nil {
		t.Fatal(err)
	}

	// This ordinary Task is deliberately not part of the Plan. Its actual
	// status is pending, while the submitted evidence will claim completed.
	probe := &model.Task{Description: "external status probe", EventType: "probe"}
	if err := taskStore.PublishTask(probe); err != nil {
		t.Fatal(err)
	}
	if probe.Status != model.TaskStatusPending {
		t.Fatalf("probe status=%s want pending", probe.Status)
	}

	_, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "probe.completed", Description: "probe task completed", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "task_status",
			Target: probe.ID, Expected: string(model.TaskStatusCompleted),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, created, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictPass,
		SubmittedByTaskID: run.RunnerTaskID,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "probe.completed", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-probe"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-probe", Kind: "task_status", TaskID: probe.ID,
			Output: string(model.TaskStatusCompleted), RecordedAt: run.CreatedAt.Add(time.Millisecond),
		}},
	})
	if !created || !errors.Is(err, plan.ErrAcceptanceConstraint) || result == nil ||
		!strings.Contains(result.Reason, "actual status is \"pending\"") {
		t.Fatalf("forged task status was accepted: created=%v result=%+v err=%v", created, result, err)
	}
}

func TestFormalAcceptanceRejectsCommandFromWrongWorkingDirectory(t *testing.T) {
	projectRoot := t.TempDir()
	wrongWorkingDir := filepath.Join(projectRoot, "passing-subtree")
	if err := os.Mkdir(wrongWorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	taskStore, coordinator := newPlannedStore(t, projectRoot)
	root := &model.Task{Description: "command cwd acceptance", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	work := &model.Task{
		Description: "implement", EventSource: root.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
	}
	if err := taskStore.PublishTask(work); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker", work.ID); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.SubmitResult("worker", work.ID, "implemented"); err != nil {
		t.Fatal(err)
	}

	_, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "tests", Description: "project tests pass", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "command_exit",
			Target: "go test ./...", Expected: "0",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if err := taskStore.AppendToolCall(run.RunnerTaskID, store.ToolCallRecord{
		Timestamp: run.CreatedAt.Add(time.Millisecond), AgentID: "verifier", ToolName: "run_shell",
		Args: map[string]any{
			"command":     "go test ./...",
			"working_dir": wrongWorkingDir,
		},
		Success: true, ExitCode: &exitCode,
	}); err != nil {
		t.Fatal(err)
	}

	result, created, err := coordinator.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: run.ID, PlanID: root.PlanID, Verdict: model.AcceptanceVerdictPass,
		SubmittedByTaskID: run.RunnerTaskID,
		CriterionResults: []model.CriterionResult{{
			CriterionID: "tests", Verdict: model.AcceptanceVerdictPass, EvidenceIDs: []string{"ev-tests"},
		}},
		Evidence: []model.Evidence{{
			ID: "ev-tests", Kind: "command", Command: "go test ./...", ExitCode: &exitCode,
			RecordedAt: run.CreatedAt.Add(2 * time.Millisecond),
		}},
	})
	if !created || !errors.Is(err, plan.ErrAcceptanceConstraint) || result == nil ||
		result.Verdict != model.AcceptanceVerdictFail ||
		!strings.Contains(result.Reason, "no successful run_shell fact") {
		t.Fatalf("wrong-cwd command was accepted: created=%v result=%+v err=%v", created, result, err)
	}
}
