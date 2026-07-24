package store

import "agentgo/internal/model"

// LegacyRequestTaskIDs returns the request-tree scope for a Scheduler root
// that has no managed PlanStore record. Modern tasks share the root PlanID;
// imported compatibility snapshots may instead rely on SchedulerBatch plus
// explicit ParentTaskID edges.
//
// BatchID, Dependencies, and EventSource are intentionally excluded: they are
// observability/routing fields, not ownership edges.
func LegacyRequestTaskIDs(tasks []*model.Task, controllerID string) map[string]struct{} {
	byID := make(map[string]*model.Task, len(tasks))
	for _, task := range tasks {
		if task != nil {
			byID[task.ID] = task
		}
	}
	controller := byID[controllerID]
	if controller == nil {
		return nil
	}

	children := make(map[string][]*model.Task)
	for _, task := range tasks {
		if task != nil && task.ParentTaskID != "" {
			children[task.ParentTaskID] = append(children[task.ParentTaskID], task)
		}
	}
	visible := make(map[string]struct{})
	queue := make([]string, 0, len(tasks))
	add := func(task *model.Task) {
		if task == nil {
			return
		}
		if _, exists := visible[task.ID]; exists {
			return
		}
		visible[task.ID] = struct{}{}
		queue = append(queue, task.ID)
	}
	add(controller)
	for _, id := range controller.SchedulerBatch {
		if task := byID[id]; task != nil && legacyPlanCompatible(controller, task) {
			add(task)
		}
	}
	if controller.PlanID != "" {
		for _, task := range tasks {
			if task != nil && task.PlanID == controller.PlanID {
				add(task)
			}
		}
	}

	// Expand the explicit descendant closure for old snapshots whose children
	// do not yet carry an inherited PlanID.
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		for _, child := range children[parentID] {
			if legacyPlanCompatible(controller, child) {
				add(child)
			}
		}
	}
	return visible
}

func legacyPlanCompatible(controller, task *model.Task) bool {
	return controller.PlanID == "" || task.PlanID == "" || controller.PlanID == task.PlanID
}
