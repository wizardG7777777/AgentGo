package store

import (
	"testing"

	"agentgo/internal/model"
)

func TestLegacyRequestTaskIDsUsesOnlyOwnershipEdges(t *testing.T) {
	controller := &model.Task{ID: "root", SchedulerBatch: []string{"batch"}}
	tasks := []*model.Task{
		controller,
		{ID: "batch"},
		{ID: "descendant", ParentTaskID: "batch"},
		{ID: "child", ParentTaskID: "root"},
		{ID: "unrelated"},
		{ID: "batch-label", BatchID: "root"},
		{ID: "dependency-label", Dependencies: []string{"batch"}},
		{ID: "event-label", EventSource: "root"},
	}
	visible := LegacyRequestTaskIDs(tasks, controller.ID)
	for _, id := range []string{"root", "batch", "descendant", "child"} {
		if _, ok := visible[id]; !ok {
			t.Errorf("expected %s in legacy request scope", id)
		}
	}
	for _, id := range []string{"unrelated", "batch-label", "dependency-label", "event-label"} {
		if _, ok := visible[id]; ok {
			t.Errorf("authority-only label admitted %s", id)
		}
	}
}

func TestLegacyRequestTaskIDsFailsClosedWithoutController(t *testing.T) {
	visible := LegacyRequestTaskIDs([]*model.Task{{ID: "unrelated"}}, "missing")
	if len(visible) != 0 {
		t.Fatalf("missing controller exposed tasks: %v", visible)
	}
}
