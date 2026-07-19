package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

// newGuardedCancelStore 建一个带取消注册表的真实 MemoryTaskStore，
// 用于断言取消来源落账（CancelRegistry.Source）。
func newGuardedCancelStore(t *testing.T) (*store.MemoryTaskStore, *store.TaskCancelRegistry) {
	t.Helper()
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reg := store.NewTaskCancelRegistry()
	s.SetCancelRegistry(reg)
	return s, reg
}

// D2：外部调用方（TUI /cancel，无 Plan 上下文）取消 Plan 托管任务必须被
// 拒绝——与 LLM cancel_task 的拒绝语义同出一源（GuardedCancel）。
func TestGuardedCancel_ExternalCallerRefusedOnPlanOwnedTask(t *testing.T) {
	s, _ := newGuardedCancelStore(t)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	root := &model.Task{ID: "root-ctrl", Description: "root", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController, PlanID: "plan-1"}
	if err := s.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: "plan-1", RootTaskID: root.ID}); err != nil {
		t.Fatal(err)
	}
	target := &model.Task{ID: "plan-member-1", Description: "x", PlanID: "plan-1"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}

	err := GuardedCancel(context.Background(), s, coordinator, "", target.ID, "user")
	if err == nil || !strings.Contains(err.Error(), "cancel_task 被拒绝") {
		t.Fatalf("外部调用方取消 Plan 托管任务应被拒绝, err=%v", err)
	}
	got, getErr := s.GetTask(target.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("被拒绝后任务状态不应改变: %s", got.Status)
	}
}

// D2：不归属 Plan 的 pending 任务可被外部调用方取消，取消来源记为 "user"。
func TestGuardedCancel_ExternalCallerCancelsFreePendingTask(t *testing.T) {
	s, reg := newGuardedCancelStore(t)
	target := &model.Task{ID: "free-pending-1", Description: "x"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	// 模拟 runner 认领时注册的 cancel context——来源只对有注册的任务落账。
	reg.GetOrCreate(context.Background(), target.ID)

	if err := GuardedCancel(context.Background(), s, nil, "", target.ID, "user"); err != nil {
		t.Fatalf("取消自由任务失败: %v", err)
	}
	got, getErr := s.GetTask(target.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("状态 = %s, want cancelled", got.Status)
	}
	if src := reg.Source(target.ID); src != "user" {
		t.Fatalf("取消来源 = %q, want user", src)
	}
}

// D2：processing 中的自由任务走第二条转换分支（processing→cancelled）。
func TestGuardedCancel_ExternalCallerCancelsProcessingTask(t *testing.T) {
	s, reg := newGuardedCancelStore(t)
	target := &model.Task{ID: "free-processing-1", Description: "x"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-1", target.ID); err != nil {
		t.Fatal(err)
	}
	reg.GetOrCreate(context.Background(), target.ID)

	if err := GuardedCancel(context.Background(), s, nil, "", target.ID, "user"); err != nil {
		t.Fatalf("取消 processing 任务失败: %v", err)
	}
	got, getErr := s.GetTask(target.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("状态 = %s, want cancelled", got.Status)
	}
	if src := reg.Source(target.ID); src != "user" {
		t.Fatalf("取消来源 = %q, want user", src)
	}
}

// D2：目标不存在时报错措辞与抽取前 cancel_task 一致。
func TestGuardedCancel_TargetNotFound(t *testing.T) {
	s, _ := newGuardedCancelStore(t)
	err := GuardedCancel(context.Background(), s, nil, "", "no-such-task", "user")
	if err == nil || !strings.Contains(err.Error(), "读取待取消任务失败") {
		t.Fatalf("应报读取待取消任务失败, err=%v", err)
	}
}

// D2：LLM 工具侧语义保持——active controller 可取消本 Plan 成员（租约路径）。
func TestGuardedCancel_ActiveControllerCancelsPlanMember(t *testing.T) {
	s, reg := newGuardedCancelStore(t)
	controller := &model.Task{ID: "ctrl-1", Description: "c", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController, PlanID: "plan-1"}
	if err := s.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: "plan-1", RootTaskID: controller.ID}); err != nil {
		t.Fatal(err)
	}
	target := &model.Task{ID: "member-1", Description: "x", PlanID: "plan-1"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	reg.GetOrCreate(context.Background(), target.ID)

	if err := GuardedCancel(context.Background(), s, coordinator, controller.ID, target.ID, "scheduler"); err != nil {
		t.Fatalf("active controller 取消本 Plan 任务应成功: %v", err)
	}
	got, getErr := s.GetTask(target.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("状态 = %s, want cancelled", got.Status)
	}
	if src := reg.Source(target.ID); src != "scheduler" {
		t.Fatalf("取消来源 = %q, want scheduler", src)
	}
}

// D2：LLM 工具侧语义保持——controller 不能取消其他 Plan 的任务。
func TestGuardedCancel_ControllerRefusedOnForeignPlanTask(t *testing.T) {
	s, _ := newGuardedCancelStore(t)
	controller := &model.Task{ID: "ctrl-1", Description: "c", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController, PlanID: "plan-1"}
	if err := s.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: "plan-1", RootTaskID: controller.ID}); err != nil {
		t.Fatal(err)
	}
	target := &model.Task{ID: "foreign-1", Description: "x", PlanID: "plan-2"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}

	err := GuardedCancel(context.Background(), s, coordinator, controller.ID, target.ID, "scheduler")
	if err == nil || !strings.Contains(err.Error(), "不属于当前 Plan") {
		t.Fatalf("取消其他 Plan 任务应被拒绝, err=%v", err)
	}
	got, getErr := s.GetTask(target.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("被拒绝后任务状态不应改变: %s", got.Status)
	}
}
