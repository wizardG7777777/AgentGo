package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
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

// C6b：Plan 归属守卫已随其整包删除——不归属任何控制面的 pending
// 任务可被外部调用方（TUI /cancel）取消，取消来源记为 "user"。
func TestGuardedCancel_ExternalCallerCancelsFreePendingTask(t *testing.T) {
	s, reg := newGuardedCancelStore(t)
	target := &model.Task{ID: "free-pending-1", Description: "x"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	// 模拟 runner 认领时注册的 cancel context——来源只对有注册的任务落账。
	reg.GetOrCreate(context.Background(), target.ID)

	if err := GuardedCancel(context.Background(), s, target.ID, "user"); err != nil {
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

// processing 中的任务走第二条转换分支（processing→cancelled）。
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

	if err := GuardedCancel(context.Background(), s, target.ID, "user"); err != nil {
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

// C6b 新语义：任何任务都可被外部调用方取消——带图身份（GraphID）的任务
// 也不再有归属守卫，直接两段式转换。
func TestGuardedCancel_ExternalCallerCancelsGraphTask(t *testing.T) {
	s, reg := newGuardedCancelStore(t)
	target := &model.Task{
		ID: "graph-node-1", Description: "x", EventType: "code",
		GraphID: "g-1", NodeID: "implement", ActivationID: "implement@1",
	}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	reg.GetOrCreate(context.Background(), target.ID)

	if err := GuardedCancel(context.Background(), s, target.ID, "user"); err != nil {
		t.Fatalf("外部调用方取消图任务不应被拒绝: %v", err)
	}
	got, getErr := s.GetTask(target.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("状态 = %s, want cancelled", got.Status)
	}
}

// scheduler 经 cancel_task 取消任务时来源记为 "scheduler"。
func TestGuardedCancel_SchedulerSourceRecorded(t *testing.T) {
	s, reg := newGuardedCancelStore(t)
	target := &model.Task{ID: "target-1", Description: "x"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	reg.GetOrCreate(context.Background(), target.ID)

	if err := GuardedCancel(context.Background(), s, target.ID, "scheduler"); err != nil {
		t.Fatalf("scheduler 取消任务失败: %v", err)
	}
	if src := reg.Source(target.ID); src != "scheduler" {
		t.Fatalf("取消来源 = %q, want scheduler", src)
	}
}

// 目标不存在时报错措辞：「取消任务失败 (id=...)」。
func TestGuardedCancel_TargetNotFound(t *testing.T) {
	s, _ := newGuardedCancelStore(t)
	err := GuardedCancel(context.Background(), s, "no-such-task", "user")
	if err == nil || !strings.Contains(err.Error(), "取消任务失败 (id=no-such-task)") {
		t.Fatalf("应报取消任务失败, err=%v", err)
	}
}

// 已终态任务两段转换都失败，返回「取消任务失败」错误。
func TestGuardedCancel_TerminalTaskRejected(t *testing.T) {
	s, _ := newGuardedCancelStore(t)
	target := &model.Task{ID: "done-1", Description: "x"}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionState(target.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}

	err := GuardedCancel(context.Background(), s, target.ID, "user")
	if err == nil || !strings.Contains(err.Error(), "取消任务失败 (id=done-1)") {
		t.Fatalf("取消终态任务应失败, err=%v", err)
	}
}
