package bootstrap

import (
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/ui"
)

func TestApplyActivitySnapshotProjectsActiveTools(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	info := ui.AgentCard{ID: "scheduler-1"}
	(&System{}).applyActivitySnapshot(&info, agent.ActivitySnapshot{
		AgentID: "scheduler-1", Phase: "tooling",
		ActiveTools: []agent.ToolActivity{{CallID: "call-1", ToolName: "continue_waiting", StartedAt: started}},
	})
	if len(info.ActiveTools) != 1 || info.ActiveTools[0].CallID != "call-1" ||
		info.ActiveTools[0].Tool != "continue_waiting" || !info.ActiveTools[0].StartedAt.Equal(started) {
		t.Fatalf("active tool projection = %+v", info.ActiveTools)
	}
}
