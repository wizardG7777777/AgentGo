package store

import (
	"context"
	"testing"
)

func TestCancelRegistry_GetOrCreate(t *testing.T) {
	r := NewTaskCancelRegistry()
	parent := context.Background()

	// 首次创建
	ctx1 := r.GetOrCreate(parent, "task-1")
	if ctx1 == nil {
		t.Fatal("expected non-nil context")
	}
	select {
	case <-ctx1.Done():
		t.Fatal("context should not be cancelled")
	default:
	}

	// 同一 taskID 返回同一 context（多代理共享）
	ctx2 := r.GetOrCreate(parent, "task-1")
	if ctx1 != ctx2 {
		t.Error("expected same context for same taskID")
	}

	// 不同 taskID 返回不同 context
	ctx3 := r.GetOrCreate(parent, "task-2")
	if ctx1 == ctx3 {
		t.Error("expected different context for different taskID")
	}
}

func TestCancelRegistry_Cancel(t *testing.T) {
	r := NewTaskCancelRegistry()
	parent := context.Background()

	ctx := r.GetOrCreate(parent, "task-1")

	r.Cancel("task-1")

	select {
	case <-ctx.Done():
		// 预期行为
	default:
		t.Error("context should be cancelled after Cancel()")
	}

	// Cancel 后 GetOrCreate 应创建新 context
	ctx2 := r.GetOrCreate(parent, "task-1")
	select {
	case <-ctx2.Done():
		t.Error("new context should not be cancelled")
	default:
	}
}

func TestCancelRegistry_CancelWithSource(t *testing.T) {
	r := NewTaskCancelRegistry()
	parent := context.Background()

	ctx := r.GetOrCreate(parent, "task-1")
	r.CancelWithSource("task-1", "scheduler")

	select {
	case <-ctx.Done():
		// 预期行为
	default:
		t.Error("context should be cancelled after CancelWithSource()")
	}
	if got := r.Source("task-1"); got != "scheduler" {
		t.Errorf("Source=%q, want scheduler", got)
	}

	r.Remove("task-1")
	if got := r.Source("task-1"); got != "" {
		t.Errorf("Source after Remove=%q, want empty", got)
	}
}

func TestCancelRegistry_Remove(t *testing.T) {
	r := NewTaskCancelRegistry()
	parent := context.Background()

	ctx := r.GetOrCreate(parent, "task-1")

	r.Remove("task-1")

	// Remove 也会 cancel context（释放资源）
	select {
	case <-ctx.Done():
		// 预期行为
	default:
		t.Error("context should be cancelled after Remove()")
	}
}

func TestCancelRegistry_CancelNonexistent(t *testing.T) {
	r := NewTaskCancelRegistry()

	// 不应 panic
	r.Cancel("nonexistent")
	r.Remove("nonexistent")
}

func TestCancelRegistry_ParentCancel(t *testing.T) {
	r := NewTaskCancelRegistry()
	parent, parentCancel := context.WithCancel(context.Background())

	ctx := r.GetOrCreate(parent, "task-1")

	// 取消 parent 应该级联取消 task context
	parentCancel()

	select {
	case <-ctx.Done():
		// 预期行为：parent 取消级联到子 context
	default:
		t.Error("task context should be cancelled when parent is cancelled")
	}
}
