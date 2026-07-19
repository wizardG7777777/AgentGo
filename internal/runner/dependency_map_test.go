package runner

import (
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/tools"
)

// A2：resolveToolGroups 必须把审批等待钩子透传给 ShellGroup，
// 否则 waiting_approval 状态机接线在 runner 路径上断裂。
func TestResolveToolGroups_WiresApprovalWaitHook(t *testing.T) {
	holder := &CurrentTaskHolder{}
	calls := 0
	hook := func(waiting bool) { calls++ }

	groups := resolveToolGroups("w-1", RunnerDeps{}, holder,
		agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, hook)

	var shellGroup *tools.ShellGroup
	for i := range groups {
		if sg, ok := groups[i].(tools.ShellGroup); ok {
			shellGroup = &sg
			break
		}
	}
	if shellGroup == nil {
		t.Fatal("resolveToolGroups 应包含 ShellGroup")
	}
	if shellGroup.ApprovalWaitHook == nil {
		t.Fatal("ShellGroup.ApprovalWaitHook 未接线")
	}

	shellGroup.ApprovalWaitHook(true)
	shellGroup.ApprovalWaitHook(false)
	if calls != 2 {
		t.Errorf("透传的钩子被调用 2 次，实际 %d", calls)
	}
}
