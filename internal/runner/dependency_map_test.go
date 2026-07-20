package runner

import (
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/tools"
)

// resolveToolGroups 必须把 Interaction 服务、Session 与等待钩子完整透传给
// ShellGroup，否则灰名单命令会 fail-closed 或丢失当前 Session 投影。
func TestResolveToolGroups_WiresInteractionDependencies(t *testing.T) {
	holder := &CurrentTaskHolder{}
	calls := 0
	hook := func(waiting bool) { calls++ }
	interactions := interaction.NewService(nil)
	sessionID := func() string { return "session-test" }

	groups := resolveToolGroups("w-1", RunnerDeps{Interactions: interactions, SessionID: sessionID}, holder,
		agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, hook)

	var shellGroup *tools.ShellGroup
	var metaGroup *tools.MetaGroup
	for i := range groups {
		if sg, ok := groups[i].(tools.ShellGroup); ok {
			shellGroup = &sg
		}
		if mg, ok := groups[i].(tools.MetaGroup); ok {
			metaGroup = &mg
		}
	}
	if shellGroup == nil {
		t.Fatal("resolveToolGroups 应包含 ShellGroup")
	}
	if shellGroup.Interactions != interactions {
		t.Fatal("ShellGroup.Interactions 未接线")
	}
	if shellGroup.SessionID == nil || shellGroup.SessionID() != "session-test" {
		t.Fatal("ShellGroup.SessionID 未接线")
	}
	if shellGroup.InteractionWaitHook == nil {
		t.Fatal("ShellGroup.InteractionWaitHook 未接线")
	}
	if metaGroup == nil {
		t.Fatal("resolveToolGroups 应包含 MetaGroup")
	}
	if metaGroup.Interactions != interactions {
		t.Fatal("MetaGroup.Interactions 未接线")
	}
	if metaGroup.SessionID == nil || metaGroup.SessionID() != "session-test" {
		t.Fatal("MetaGroup.SessionID 未接线")
	}
	if metaGroup.InteractionWaitHook == nil {
		t.Fatal("MetaGroup.InteractionWaitHook 未接线")
	}

	shellGroup.InteractionWaitHook(true)
	shellGroup.InteractionWaitHook(false)
	metaGroup.InteractionWaitHook(true)
	metaGroup.InteractionWaitHook(false)
	if calls != 4 {
		t.Errorf("透传的钩子被调用 4 次，实际 %d", calls)
	}
}
