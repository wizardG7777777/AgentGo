package plan

import (
	"context"
	"testing"

	"agentgo/internal/model"
)

// 唤醒门控：task_completed 在阶段内仍有非终态节点时不投递 ReplanRequest；
// 阶段内最后一个节点终态才投递；失败类信号不受门控，立即投递。
func TestTaskCompletedWakeGatedUntilPhaseTerminal(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-wake-gate", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work-a")
	p = registerNode(t, c, p.ID, 1, "work-b")

	completed := TaskMutation{
		Kind: "status", Status: model.TaskStatusCompleted, Wake: true,
		ReasonCode: "task_completed", SourceEvent: "status",
	}

	// 中间完成：work-b 仍 pending → 不投递、不唤醒。
	_, notified, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: p.ID, TaskID: "work-a", Mutation: completed},
	})
	if errs[0] != nil {
		t.Fatal(errs[0])
	}
	if notified[0] {
		t.Fatal("intermediate task_completed must not deliver a wake request")
	}
	if _, ok, err := c.TrySignal(p.ID); err != nil || ok {
		t.Fatalf("signal after intermediate completion = ok:%v err:%v, want none", ok, err)
	}

	// 阶段内最后一个节点终态 → 投递并唤醒。
	_, notified, errs = c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: p.ID, TaskID: "work-b", Mutation: completed},
	})
	if errs[0] != nil {
		t.Fatal(errs[0])
	}
	if !notified[0] {
		t.Fatal("last phase node completion must deliver a wake request")
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || !containsString(signal.Reasons, "task_completed") {
		t.Fatalf("phase-terminal signal = %+v ok:%v err:%v", signal, ok, err)
	}
}

func TestTaskFailedWakeNotGated(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-wake-gate-fail", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work-a")
	p = registerNode(t, c, p.ID, 1, "work-b")

	// work-b 仍 pending，但失败类信号必须立即投递（需要 Scheduler 决策修复）。
	_, notified, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: p.ID, TaskID: "work-a", Mutation: TaskMutation{
			Kind: "status", Status: model.TaskStatusFailed, Wake: true,
			ReasonCode: "task_failed", SourceEvent: "status", Urgency: model.ReplanUrgencyHigh,
		}},
	})
	if errs[0] != nil {
		t.Fatal(errs[0])
	}
	if !notified[0] {
		t.Fatal("task_failed must not be gated by running peers")
	}
	if _, ok, err := c.TrySignal(p.ID); err != nil || !ok {
		t.Fatalf("failure signal = ok:%v err:%v, want delivered", ok, err)
	}
}

// 同批多个节点同时完成：按序应用保证只有最后一个投递唤醒。
func TestTaskCompletedWakeGateWithinSingleBatch(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-wake-gate-batch", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work-a")
	p = registerNode(t, c, p.ID, 1, "work-b")
	p = registerNode(t, c, p.ID, 2, "work-c")

	completed := TaskMutation{
		Kind: "status", Status: model.TaskStatusCompleted, Wake: true,
		ReasonCode: "task_completed", SourceEvent: "status",
	}
	_, notified, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: p.ID, TaskID: "work-a", Mutation: completed},
		{PlanID: p.ID, TaskID: "work-b", Mutation: completed},
		{PlanID: p.ID, TaskID: "work-c", Mutation: completed},
	})
	for i := range errs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
	}
	if notified[0] || notified[1] || !notified[2] {
		t.Fatalf("notified = %v, want [false false true] (only the last completion wakes)", notified)
	}
}
