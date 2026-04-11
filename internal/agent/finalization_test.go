package agent

import (
	"sync"
	"testing"
)

// TestFinalizationHolder_Lifecycle 测试 FinalizationHolder 的基本生命周期
func TestFinalizationHolder_Lifecycle(t *testing.T) {
	h := NewFinalizationHolder()

	// 初始状态：taskID 为空，finalized=false
	if h.Get() != "" {
		t.Error("new holder should have empty taskID")
	}
	if h.IsFinalized() {
		t.Error("new holder should have finalized=false")
	}

	// Set 设置 taskID，finalized 仍为 false
	h.Set("task-1")
	if h.Get() != "task-1" {
		t.Errorf("Get() = %q, want task-1", h.Get())
	}
	if h.IsFinalized() {
		t.Error("Set should not change finalized flag")
	}

	// MarkTaskFinalized 设置 finalized=true
	h.MarkTaskFinalized()
	if !h.IsFinalized() {
		t.Error("MarkTaskFinalized should make IsFinalized return true")
	}
	// taskID 不变
	if h.Get() != "task-1" {
		t.Errorf("Get() = %q, want task-1 after MarkTaskFinalized", h.Get())
	}

	// Set 新 task 清空 finalized
	h.Set("task-2")
	if h.Get() != "task-2" {
		t.Errorf("Get() = %q, want task-2", h.Get())
	}
	if h.IsFinalized() {
		t.Error("Set with new task ID should clear finalized flag")
	}

	// Set("") 也清空 finalized（OnTaskEnd 路径）
	h.MarkTaskFinalized()
	h.Set("")
	if h.Get() != "" {
		t.Errorf("Get() = %q, want empty string after Set(\"\")", h.Get())
	}
	if h.IsFinalized() {
		t.Error("Set with empty ID should clear finalized flag")
	}
}

// TestFinalizationHolder_ConcurrentAccess 测试并发安全性
func TestFinalizationHolder_ConcurrentAccess(t *testing.T) {
	h := NewFinalizationHolder()

	// 并发写入和读取
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func(id int) {
			defer wg.Done()
			h.Set("task-concurrent")
		}(i)
		go func(id int) {
			defer wg.Done()
			h.MarkTaskFinalized()
		}(i)
		go func(id int) {
			defer wg.Done()
			_ = h.IsFinalized()
			_ = h.Get()
		}(i)
	}
	wg.Wait()

	// 最终状态应该是确定的（不会 panic）
	_ = h.Get()
	_ = h.IsFinalized()
}

// TestFinalizationHolder_InterfaceCompliance 测试接口实现
func TestFinalizationHolder_InterfaceCompliance(t *testing.T) {
	h := NewFinalizationHolder()

	// 测试 FinalizationChecker 接口
	var _ FinalizationChecker = h
	if h.IsFinalized() {
		t.Error("new holder should not be finalized")
	}

	// 测试 FinalizationNotifier 接口（通过适配）
	var notifier interface{ MarkTaskFinalized() } = h
	notifier.MarkTaskFinalized()
	if !h.IsFinalized() {
		t.Error("MarkTaskFinalized should finalize the holder")
	}
}
