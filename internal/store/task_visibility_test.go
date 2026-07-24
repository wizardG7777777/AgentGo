package store

import (
	"testing"

	"agentgo/internal/model"
)

func TestLegacyRequestTaskIDsUsesOnlyOwnershipEdges(t *testing.T) {
	controller := &model.Task{ID: "root", PlanID: "root", SchedulerBatch: []string{"batch"}}
	tasks := []*model.Task{
		controller,
		{ID: "same-plan", PlanID: "root"},
		{ID: "batch"},
		{ID: "descendant", ParentTaskID: "batch"},
		{ID: "different-plan", PlanID: "other", ParentTaskID: "root"},
		{ID: "batch-label", BatchID: "root"},
		{ID: "dependency-label", Dependencies: []string{"same-plan"}},
		{ID: "event-label", EventSource: "root"},
	}
	visible := LegacyRequestTaskIDs(tasks, controller.ID)
	for _, id := range []string{"root", "same-plan", "batch", "descendant"} {
		if _, ok := visible[id]; !ok {
			t.Errorf("expected %s in legacy request scope", id)
		}
	}
	for _, id := range []string{"different-plan", "batch-label", "dependency-label", "event-label"} {
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
