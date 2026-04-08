package tools

import "testing"

func TestDefaultWorkdir_ReturnsProjectRoot(t *testing.T) {
	w := &DefaultWorkdir{ProjectRoot: "/project"}
	if got := w.Get(); got != "/project" {
		t.Errorf("应返回 ProjectRoot，实际: %s", got)
	}
}
