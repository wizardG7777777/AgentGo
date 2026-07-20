package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

// planReviewGroup 构造带三轴模式 store 的 PlanControlGroup，供
// submit_plan_for_review 工具测试使用。
func planReviewGroup(coordinator *plan.Coordinator, taskStore store.TaskStore, controllerID string, gate modes.GateMode) PlanControlGroup {
	return PlanControlGroup{
		Coordinator: coordinator, Store: taskStore,
		Holder: &fakeHolder{id: controllerID}, AgentID: "scheduler-1",
		Modes: modes.NewStore(gate, modes.ExecNormal, modes.TopoTeam),
	}
}

// TestSubmitPlanForReview_PlanGateSuspends 验证 gate=plan 主路径：
// 提交后 Plan 进入 plan_review 挂起，计划全文持久化到 Plan.Review 可读回。
func TestSubmitPlanForReview_PlanGateSuspends(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "plan-gate 请求", "user", model.PlanBudget{})
	group := planReviewGroup(coordinator, taskStore, controller.ID, modes.GatePlan)

	const planText = "# 执行计划\n1. 探索 docs/\n2. 写 report.md（预期产物）\n3. 验收"
	message, err := group.submitPlanForReview(context.Background(), map[string]any{"plan": planText})
	if err != nil {
		t.Fatalf("submitPlanForReview: %v", err)
	}
	if !strings.Contains(message, "等待用户审阅") {
		t.Fatalf("返回消息未提示等待审阅: %q", message)
	}
	p, err := coordinator.Store().GetPlan(controller.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != plan.PauseReasonPlanReview {
		t.Fatalf("plan = %s/%q, want paused_awaiting_decision/plan_review", p.Status, p.PauseReason)
	}
	if p.Review == nil || p.Review.Text != planText {
		t.Fatalf("计划文本未持久化: %+v", p.Review)
	}
	if p.Review.SubmittedBy != controller.ID {
		t.Fatalf("SubmittedBy = %q, want %q", p.Review.SubmittedBy, controller.ID)
	}
}

// TestSubmitPlanForReview_NotPlanGateIsIdempotentHint 验证 gate≠plan 时
// 幂等提示：不挂起、不报错。nil Modes（runner 装配）同样按非 plan 处理。
func TestSubmitPlanForReview_NotPlanGateIsIdempotentHint(t *testing.T) {
	for name, group := range map[string]func(coordinator *plan.Coordinator, taskStore store.TaskStore, controllerID string) PlanControlGroup{
		"immediate": func(c *plan.Coordinator, s store.TaskStore, id string) PlanControlGroup {
			return planReviewGroup(c, s, id, modes.GateImmediate)
		},
		"nil_modes": func(c *plan.Coordinator, s store.TaskStore, id string) PlanControlGroup {
			return PlanControlGroup{Coordinator: c, Store: s, Holder: &fakeHolder{id: id}, AgentID: "scheduler-1"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			taskStore := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
			coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
			controller := publishControllerPlan(t, taskStore, coordinator, "普通请求", "user", model.PlanBudget{})
			g := group(coordinator, taskStore, controller.ID)

			message, err := g.submitPlanForReview(context.Background(), map[string]any{"plan": "计划文本"})
			if err != nil {
				t.Fatalf("非 plan 模式不应报错: %v", err)
			}
			if !strings.Contains(message, "当前不是 plan 模式") {
				t.Fatalf("返回消息 = %q, want 非 plan 模式提示", message)
			}
			p, getErr := coordinator.Store().GetPlan(controller.PlanID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if p.Status != model.PlanStatusRunning || p.Review != nil {
				t.Fatalf("非 plan 模式不应挂起或写 Review: %s %+v", p.Status, p.Review)
			}
		})
	}
}

// TestSubmitPlanForReview_RepeatSubmitIsIdempotent 验证已处于 plan_review
// 的 Plan 重复提交：幂等返回"已在等待用户审阅"，且不覆盖首次提交的计划文本。
func TestSubmitPlanForReview_RepeatSubmitIsIdempotent(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "plan-gate 请求", "user", model.PlanBudget{})
	group := planReviewGroup(coordinator, taskStore, controller.ID, modes.GatePlan)

	if _, err := group.submitPlanForReview(context.Background(), map[string]any{"plan": "首版计划"}); err != nil {
		t.Fatal(err)
	}
	message, err := group.submitPlanForReview(context.Background(), map[string]any{"plan": "篡改版计划"})
	if err != nil {
		t.Fatalf("重复提交不应报错: %v", err)
	}
	if !strings.Contains(message, "已在等待用户审阅") {
		t.Fatalf("返回消息 = %q, want 幂等提示", message)
	}
	p, getErr := coordinator.Store().GetPlan(controller.PlanID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if p.Review == nil || p.Review.Text != "首版计划" {
		t.Fatalf("重复提交覆盖了首次计划文本: %+v", p.Review)
	}
}

// TestSubmitPlanForReview_RejectsEmptyPlan 验证空计划文本直接报错，
// 不挂起 Plan。
func TestSubmitPlanForReview_RejectsEmptyPlan(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "plan-gate 请求", "user", model.PlanBudget{})
	group := planReviewGroup(coordinator, taskStore, controller.ID, modes.GatePlan)

	if _, err := group.submitPlanForReview(context.Background(), map[string]any{"plan": "   "}); err == nil {
		t.Fatal("空计划文本应报错")
	}
	p, getErr := coordinator.Store().GetPlan(controller.PlanID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if p.Status != model.PlanStatusRunning {
		t.Fatalf("空计划不应挂起: %s", p.Status)
	}
}

// TestSubmitPlanForReview_RequiresController 验证非 controller 任务上下文
// 被拒绝（与普通 plan 控制面工具一致）。
func TestSubmitPlanForReview_RequiresController(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 8), 16, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	publishControllerPlan(t, taskStore, coordinator, "plan-gate 请求", "user", model.PlanBudget{})
	worker := &model.Task{Description: "worker task"}
	if err := taskStore.PublishTask(worker); err != nil {
		t.Fatal(err)
	}
	group := planReviewGroup(coordinator, taskStore, worker.ID, modes.GatePlan)

	if _, err := group.submitPlanForReview(context.Background(), map[string]any{"plan": "计划"}); err == nil {
		t.Fatal("非 controller 任务应被拒绝")
	}
}
