package plan

// supersede_redirect_test.go 覆盖 2026-07-27 真实运行事故的修复：
// supersede 退休节点时，剩余当前节点（含 acceptance 角色）指向退休节点的
// 依赖边必须重定向到替代节点——否则 validateCurrentGraph 以
// ErrDependencyNotFound 回滚整个替代，plan 楔死。

import (
	"context"
	"testing"

	"agentgo/internal/model"
)

// 注册一个指定角色的节点（registerNode 恒为 implementation，acceptance 角色需要显式指定）。
func registerNodeWithRole(t *testing.T, c *Coordinator, planID string, revision int64, id string, role model.PlanNodeRole, deps ...string) {
	t.Helper()
	if _, err := c.RegisterTask(context.Background(), RegisterTaskInput{
		PlanID: planID, ObservedRevision: revision,
		Node: model.PlanNode{TaskID: id, Title: id, Role: role, Dependencies: deps},
	}); err != nil {
		t.Fatalf("RegisterTask(%s): %v", id, err)
	}
}

// 事故场景：B 依赖 A，acceptance 角色节点 C 也依赖 A；退休 A 并用 R 替代，
// B 与 C 的依赖边都必须改写为 R（修复前整个 supersede 被 digest 校验回滚）。
func TestSupersedeExisting_RewritesDependentsIncludingAcceptanceRole(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-redirect", model.PlanBudget{})

	registerNode(t, c, p.ID, 0, "A")
	p = registerNode(t, c, p.ID, 1, "B", "A")
	registerNodeWithRole(t, c, p.ID, 2, "C", model.PlanNodeRoleAcceptance, "A")
	p = registerNode(t, c, p.ID, 3, "R")
	digestBefore := p.CurrentGraphDigest

	updated, err := c.SupersedeExisting(context.Background(), SupersedeExistingInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		RetireTaskIDs: []string{"A"}, ReplacementTaskIDs: []string{"R"}, Reason: "A 失败，R 替代",
	})
	if err != nil {
		t.Fatalf("SupersedeExisting 应成功（依赖边被改写），实际: %v", err)
	}
	if got := updated.Nodes["B"].Dependencies; len(got) != 1 || got[0] != "R" {
		t.Fatalf("B.Dependencies = %v，want [R]", got)
	}
	if got := updated.Nodes["C"].Dependencies; len(got) != 1 || got[0] != "R" {
		t.Fatalf("acceptance 角色节点 C.Dependencies = %v，want [R]（角色不做区分）", got)
	}
	if got := updated.Nodes["A"].Dependencies; got != nil {
		t.Fatalf("退休节点 A 应被压缩（Dependencies=nil），实际 %v", got)
	}
	if updated.Nodes["A"].SupersededBy != "R" {
		t.Fatalf("A.SupersededBy = %q，want R", updated.Nodes["A"].SupersededBy)
	}
	if updated.CurrentGraphDigest == digestBefore {
		t.Fatal("替代后 digest 应变化")
	}
	for _, id := range updated.CurrentNodeIDs {
		if id == "A" {
			t.Fatal("A 不应仍在当前图")
		}
	}
}

// 多替代节点：依赖展开为对全部替代节点的依赖（排序去重）。
func TestSupersedeExisting_MultipleReplacementsExpandDependency(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-multi", model.PlanBudget{})

	registerNode(t, c, p.ID, 0, "A")
	p = registerNode(t, c, p.ID, 1, "B", "A")
	p = registerNode(t, c, p.ID, 2, "R1")
	p = registerNode(t, c, p.ID, 3, "R2")

	updated, err := c.SupersedeExisting(context.Background(), SupersedeExistingInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		RetireTaskIDs: []string{"A"}, ReplacementTaskIDs: []string{"R2", "R1"}, Reason: "扇出替代",
	})
	if err != nil {
		t.Fatalf("SupersedeExisting: %v", err)
	}
	got := updated.Nodes["B"].Dependencies
	if len(got) != 2 || got[0] != "R1" || got[1] != "R2" {
		t.Fatalf("B.Dependencies = %v，want [R1 R2]（展开且排序去重）", got)
	}
}

// ApplySupersede（新建替代节点路径）同样有边改写语义。
func TestApplySupersede_RewritesDependents(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-apply", model.PlanBudget{})

	registerNode(t, c, p.ID, 0, "A")
	p = registerNode(t, c, p.ID, 1, "B", "A")

	updated, err := c.ApplySupersede(context.Background(), SupersedeInput{
		PlanID: p.ID, ObservedRevision: p.CurrentRevision,
		RetireTaskIDs: []string{"A"},
		ReplacementNodes: []model.PlanNode{{
			TaskID: "R-new", Title: "R-new", Role: model.PlanNodeRoleImplementation,
		}},
		Reason: "A 失败，新节点替代",
	})
	if err != nil {
		t.Fatalf("ApplySupersede 应成功，实际: %v", err)
	}
	if got := updated.Nodes["B"].Dependencies; len(got) != 1 || got[0] != "R-new" {
		t.Fatalf("B.Dependencies = %v，want [R-new]", got)
	}
	if got := updated.Nodes["R-new"].Supersedes; len(got) != 1 || got[0] != "A" {
		t.Fatalf("R-new.Supersedes = %v，want [A]", got)
	}
}

// EnsureAcceptanceRun：目标含非 completed 节点时追加 PlanWarning（不阻断）；
// 全部 completed 时无此 warning。
func TestEnsureAcceptanceRun_WarnsOnIncompleteTargets(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-warn", model.PlanBudget{})

	registerNode(t, c, p.ID, 0, "A")
	registerNode(t, c, p.ID, 1, "B")
	// A 置为 failed，B 置为 completed。
	if _, err := c.RecordTaskMutation(context.Background(), p.ID, "A", TaskMutation{Status: model.TaskStatusFailed}); err != nil {
		t.Fatalf("RecordTaskMutation(A failed): %v", err)
	}
	if _, err := c.RecordTaskMutation(context.Background(), p.ID, "B", TaskMutation{Status: model.TaskStatusCompleted}); err != nil {
		t.Fatalf("RecordTaskMutation(B completed): %v", err)
	}
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatalf("DefineAcceptanceSpec: %v", err)
	}

	// 目标含 failed 的 A → 必须有 warning 但 run 仍创建。
	run, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan, RunnerKind: "verify",
	})
	if err != nil || !created {
		t.Fatalf("EnsureAcceptanceRun: created=%v err=%v", created, err)
	}
	if run == nil {
		t.Fatal("run 不应为 nil")
	}
	planAfter, err := c.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	found := false
	for _, w := range planAfter.Warnings {
		if w.Code == "acceptance_target_incomplete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("含 failed 目标时应追加 acceptance_target_incomplete warning，实际: %+v", planAfter.Warnings)
	}

	// 仅 completed 目标（B）→ 无新 warning。
	before := len(planAfter.Warnings)
	if _, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopeTask, TargetTaskIDs: []string{"B"}, RunnerKind: "verify",
	}); err != nil {
		t.Fatalf("EnsureAcceptanceRun(B): %v", err)
	}
	planAfter2, _ := c.Store().GetPlan(p.ID)
	for _, w := range planAfter2.Warnings[before:] {
		if w.Code == "acceptance_target_incomplete" {
			t.Fatalf("全部 completed 的目标不应触发 warning，实际: %+v", w)
		}
	}
}
