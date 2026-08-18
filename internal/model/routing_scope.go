package model

// TaskRouteScope returns the opaque dynamic-route owner for one legacy
// Scheduler controller task. The namespace keeps task IDs disjoint from Graph
// IDs even when their raw strings happen to be identical.
func TaskRouteScope(taskID string) string {
	if taskID == "" {
		return ""
	}
	return "task:" + taskID
}

// GraphRouteScope returns the opaque dynamic-route owner for one durable
// Graph. A Graph-owned Team remains visible to every Scheduler controller node
// in that Graph without becoming visible to a legacy task with the same raw ID.
func GraphRouteScope(graphID string) string {
	if graphID == "" {
		return ""
	}
	return "graph:" + graphID
}
