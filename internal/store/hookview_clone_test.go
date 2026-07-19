package store

import (
	"testing"

	"agentgo/internal/model"
)

// TestScanPendingByEventSource_ReturnsClones 钉住 E6 修复：
// ScanPendingByEventSource 此前返回 store 内部 *model.Task 裸指针，
// 调用方可直接改穿内部状态；现在必须返回 clone——改返回值不影响
// store 内部态（GetTask 仍看到原始值），且两次扫描互不影响。
func TestScanPendingByEventSource_ReturnsClones(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "dedup candidate", EventSource: "hook", EventType: "wake"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	var view StoreHookView = s
	got := view.ScanPendingByEventSource("hook", "wake")
	if len(got) != 1 {
		t.Fatalf("ScanPendingByEventSource 返回 %d 条，want 1", len(got))
	}

	// 改穿返回值——若返回的是内部指针，这几处写入会直接污染 store。
	got[0].Description = "MUTATED"
	got[0].Priority = 999
	got[0].Status = model.TaskStatusCancelled

	internal, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if internal.Description != "dedup candidate" {
		t.Errorf("store 内部 Description 被改穿: %q", internal.Description)
	}
	if internal.Priority != 0 {
		t.Errorf("store 内部 Priority 被改穿: %d", internal.Priority)
	}
	if internal.Status != model.TaskStatusPending {
		t.Errorf("store 内部 Status 被改穿: %q", internal.Status)
	}

	// 再次扫描拿到的是全新 clone，不受上一次调用方写入影响。
	again := view.ScanPendingByEventSource("hook", "wake")
	if len(again) != 1 {
		t.Fatalf("再次扫描返回 %d 条，want 1（调用方把 Status 改穿导致漏匹配）", len(again))
	}
	if again[0] == got[0] {
		t.Error("两次扫描返回同一指针——应为各自独立的 clone")
	}
	if again[0].Description != "dedup candidate" {
		t.Errorf("再次扫描的 Description=%q，want 原始值", again[0].Description)
	}
}
