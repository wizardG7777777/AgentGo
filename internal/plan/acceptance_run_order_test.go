package plan

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/model"
)

// TestAcceptanceRunIsLater_TieBrokenByMonotonicCreatedAt 钉住 G6 的决胜规则：
// CompletedAt 相同的平局由单调递增的 CreatedAt（创建顺序）决胜，而不是随机
// UUID；两个 run 的 UUID 字典序被故意安排成与创建顺序相反，若实现仍用 UUID
// 决胜本测试必挂。
func TestAcceptanceRunIsLater_TieBrokenByMonotonicCreatedAt(t *testing.T) {
	completed := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	older := model.AcceptanceRun{
		ID: "zzzzzzz-older", CreatedAt: completed.Add(-time.Second), CompletedAt: completed,
	}
	newer := model.AcceptanceRun{
		ID: "aaaaaaa-newer", CreatedAt: completed.Add(-time.Second).Add(time.Nanosecond), CompletedAt: completed,
	}
	if !acceptanceRunIsLater(newer, older) {
		t.Error("later-created run must win a CompletedAt tie regardless of UUID order")
	}
	if acceptanceRunIsLater(older, newer) {
		t.Error("earlier-created run must not win a CompletedAt tie")
	}

	// 全部时间戳相同（legacy/手工构造数据）时退化为 UUID 字典序，保证确定性。
	a := model.AcceptanceRun{ID: "aaaa", CreatedAt: completed, CompletedAt: completed}
	b := model.AcceptanceRun{ID: "bbbb", CreatedAt: completed, CompletedAt: completed}
	if !acceptanceRunIsLater(b, a) || acceptanceRunIsLater(a, b) {
		t.Error("full tie should fall back to deterministic UUID comparison")
	}

	// 主键仍是完成时间：完成更晚的 run 无条件胜出。
	finishedLater := model.AcceptanceRun{
		ID: "0000", CreatedAt: completed.Add(-time.Hour), CompletedAt: completed.Add(time.Second),
	}
	if !acceptanceRunIsLater(finishedLater, newer) {
		t.Error("CompletedAt remains the primary ordering key")
	}
}

// TestEnsureAcceptanceRun_CreatedAtStrictlyIncreasing 验证创建钳制端到端生效：
// 第二个 run 的 CreatedAt 严格晚于第一个 run 的 CreatedAt 与 CompletedAt，
// 即使两者在亚毫秒窗口内连续创建（Windows 时钟粒度下时间戳相同）。
func TestEnsureAcceptanceRun_CreatedAtStrictlyIncreasing(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-run-order", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "work")
	completePlanNode(t, c, p.ID, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}

	first, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 完成第一个 run，使其持有 CompletedAt——钳制必须同时覆盖它。
	if _, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
		RunID: first.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictFail,
		CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictFail}},
	}); err != nil {
		t.Fatal(err)
	}
	second, created, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{
		PlanID: p.ID, Scope: model.AcceptanceScopePlan,
	})
	if err != nil || !created {
		t.Fatalf("second run: created=%v err=%v", created, err)
	}

	storedFirst, err := c.Store().GetAcceptanceRun(p.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedFirst.CompletedAt.IsZero() {
		t.Fatal("first run should be completed")
	}
	if !second.CreatedAt.After(storedFirst.CreatedAt) {
		t.Errorf("second.CreatedAt=%v not strictly after first.CreatedAt=%v",
			second.CreatedAt, storedFirst.CreatedAt)
	}
	if !second.CreatedAt.After(storedFirst.CompletedAt) {
		t.Errorf("second.CreatedAt=%v not strictly after first.CompletedAt=%v",
			second.CreatedAt, storedFirst.CompletedAt)
	}
	if !acceptanceRunIsLater(*second, *storedFirst) {
		t.Error("later-created run must be ordered later even with tied timestamps")
	}
}
