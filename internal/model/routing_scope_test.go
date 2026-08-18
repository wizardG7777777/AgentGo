package model

import "testing"

func TestRouteScopesAreNamespacedAndEmptySafe(t *testing.T) {
	if got := TaskRouteScope(""); got != "" {
		t.Fatalf("empty task route scope=%q, want empty", got)
	}
	if got := GraphRouteScope(""); got != "" {
		t.Fatalf("empty Graph route scope=%q, want empty", got)
	}

	const sameRawID = "same-id"
	taskScope := TaskRouteScope(sameRawID)
	graphScope := GraphRouteScope(sameRawID)
	if taskScope != "task:same-id" || graphScope != "graph:same-id" {
		t.Fatalf("unexpected route scopes: task=%q graph=%q", taskScope, graphScope)
	}
	if taskScope == graphScope {
		t.Fatal("task and Graph route scopes collided for the same raw ID")
	}
}
