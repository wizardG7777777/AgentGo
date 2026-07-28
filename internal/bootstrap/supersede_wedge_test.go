package bootstrap

// supersede_wedge_test.go 是 2026-07-27 真实运行事故（plan 楔死）的端到端回归：
//
//	事故链：工作节点 A 失败 → 验收 run 的目标集含 failed 的 A（无预警）→
//	验收 runner 依赖 A，认领闸要求 completed 而永远 pending →
//	scheduler 用 supersede 替代 A，又因 acceptance 节点的依赖边未改写
//	被 digest 校验回滚 → plan 永久楔死。
//
//	三处修复在此联动验证：
//	#3 EnsureAcceptanceRun 对非 completed 目标追加 PlanWarning（不阻断）；
//	#1 验收 runner（AcceptanceRunID 非空）认领闸放宽为依赖终态即可；
//	#2 supersede 改写全部剩余当前节点（含 acceptance 角色）的依赖边。

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

func TestSupersededAcceptanceWedgeEndToEnd(t *testing.T) {
	taskStore, coordinator := newPlannedStore(t, t.TempDir())

	root := &model.Task{Description: "wedge goal", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	mkWork := func(desc string) *model.Task {
		return &model.Task{
			Description: desc, EventSource: root.ID,
			NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "scheduler",
		}
	}
	workA := mkWork("implement A")
	workB := mkWork("implement B")
	if err := taskStore.PublishTask(workA); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.PublishTask(workB); err != nil {
		t.Fatal(err)
	}

	// A 失败（保留在当前图中，与事故同形）；B 完成。
	if err := store.TransitionStateWithCancelSource(taskStore, workA.ID, model.TaskStatusPending, model.TaskStatusFailed, "test"); err != nil {
		t.Fatalf("fail A: %v", err)
	}
	if err := taskStore.ClaimTask("worker", workB.ID); err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if err := taskStore.SubmitResult("worker", workB.ID, "B done"); err != nil {
		t.Fatalf("complete B: %v", err)
	}

	if _, err := coordinator.DefineAcceptanceSpec(context.Background(), root.PlanID, model.AcceptanceSpec{
		CreatedBy: "scheduler",
		Criteria: []model.Criterion{{
			ID: "goal", Description: "goal satisfied", Source: model.AcceptanceAuthorityUser,
			Required: true, Scope: model.AcceptanceScopePlan, Check: "evidence", Expected: "pass",
		}},
	}); err != nil {
		t.Fatalf("DefineAcceptanceSpec: %v", err)
	}

	// 事故第 3 层：目标含 failed 的 A，run 仍创建但必须有 warning（修复 #3）。
	run, created, err := coordinator.EnsureAcceptanceRun(context.Background(), plan.EnsureAcceptanceRunInput{
		PlanID: root.PlanID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created || run.RunnerTaskID == "" {
		t.Fatalf("EnsureAcceptanceRun: run=%+v created=%v err=%v", run, created, err)
	}
	planAfterRun, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	warnFound := false
	for _, w := range planAfterRun.Warnings {
		if w.Code == "acceptance_target_incomplete" && strings.Contains(w.Message, workA.ID) {
			warnFound = true
		}
	}
	if !warnFound {
		t.Fatalf("目标含 failed 节点时应追加 acceptance_target_incomplete warning: %+v", planAfterRun.Warnings)
	}

	// 事故第 1 层：runner 依赖含 failed 的 A，修复 #1 后仍应可认领（判负语义）。
	runnerTask, err := taskStore.GetTask(run.RunnerTaskID)
	if err != nil {
		t.Fatalf("GetTask(runner): %v", err)
	}
	if runnerTask.AcceptanceRunID == "" {
		t.Fatal("runner 应携带 AcceptanceRunID 标记")
	}
	if err := taskStore.ClaimTask("verifier", run.RunnerTaskID); err != nil {
		t.Fatalf("验收 runner 依赖 failed 节点应可认领（修复 #1），实际: %v", err)
	}

	// 事故第 2 层：supersede 用替代节点 R 退休 A——修复 #2 后边改写覆盖
	// acceptance 节点，不再被 digest 校验回滚。
	replacement := mkWork("arbitration R")
	if err := taskStore.PublishTask(replacement); err != nil {
		t.Fatal(err)
	}
	planBefore, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatalf("GetPlan(before): %v", err)
	}
	updated, err := coordinator.SupersedeExisting(context.Background(), plan.SupersedeExistingInput{
		PlanID: root.PlanID, ObservedRevision: planBefore.CurrentRevision,
		RetireTaskIDs: []string{workA.ID}, ReplacementTaskIDs: []string{replacement.ID},
		Reason: "A 失败，R 替代",
	})
	if err != nil {
		t.Fatalf("supersede 应成功（修复 #2），实际: %v", err)
	}
	runnerNode := updated.Nodes[run.RunnerTaskID]
	wantDeps := map[string]bool{workB.ID: true, replacement.ID: true}
	if len(runnerNode.Dependencies) != len(wantDeps) {
		t.Fatalf("acceptance 节点依赖 = %v，want 包含 B 与 R", runnerNode.Dependencies)
	}
	for _, dep := range runnerNode.Dependencies {
		if !wantDeps[dep] {
			t.Fatalf("acceptance 节点依赖 = %v，want 包含 B 与 R", runnerNode.Dependencies)
		}
	}
	if updated.Nodes[workA.ID].SupersededBy != replacement.ID {
		t.Fatalf("A.SupersededBy = %q，want %s", updated.Nodes[workA.ID].SupersededBy, replacement.ID)
	}
}
