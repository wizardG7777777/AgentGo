package runner

import (
	"slices"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/shell"
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

	groups := resolveToolGroups("w-1", nil, RunnerDeps{Interactions: interactions, SessionID: sessionID}, holder,
		agent.NewFinalizationHolder(), agent.NewSubmitState(),
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

// mustShellGroup 从 resolveToolGroups 结果中取出 ShellGroup（不存在则失败）。
func mustShellGroup(t *testing.T, groups []tools.ToolGroup) *tools.ShellGroup {
	t.Helper()
	for i := range groups {
		if sg, ok := groups[i].(tools.ShellGroup); ok {
			return &sg
		}
	}
	t.Fatal("resolveToolGroups 应包含 ShellGroup")
	return nil
}

// 验收角色（白名单含 submit_acceptance_result）的 ShellGroup 必须注入
// 验收加固灰名单：verifier 不授文件写工具，run_shell 的写倾向命令需要
// 升级为灰名单 Interaction 审批（worktree 隔离落地前的过渡收紧）。
func TestResolveToolGroups_AcceptanceRoleGetsHardenedShell(t *testing.T) {
	groups := resolveToolGroups("verifier-1",
		[]string{"read_file", "run_shell", "submit_acceptance_result", "request_replan"},
		RunnerDeps{}, &CurrentTaskHolder{},
		agent.NewFinalizationHolder(), agent.NewSubmitState(),
		agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, nil)

	shellGroup := mustShellGroup(t, groups)
	if !slices.Equal(shellGroup.ExtraGreylist, shell.AcceptanceHardeningGreylist) {
		t.Fatalf("验收角色 ExtraGreylist 应为 shell.AcceptanceHardeningGreylist，实际 %v", shellGroup.ExtraGreylist)
	}
}

// 非验收语境行为完全不变：普通执行 / 只读 / 未配置白名单的 runtime 不注入
// 加固灰名单。submit_task_result 是普通执行节点的提交工具，不是验收信号。
func TestResolveToolGroups_NonAcceptanceRoleKeepsShellUnchanged(t *testing.T) {
	for _, allowed := range [][]string{
		{"read_file", "run_shell", "submit_task_result"},
		{"read_file", "run_shell"},
		nil, // 单测直构场景：nil 白名单不加固（生产 kind 必有非空白名单）
	} {
		groups := resolveToolGroups("w-1", allowed, RunnerDeps{}, &CurrentTaskHolder{},
			agent.NewFinalizationHolder(), agent.NewSubmitState(),
			agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, nil)
		if sg := mustShellGroup(t, groups); sg.ExtraGreylist != nil {
			t.Errorf("非验收角色 ExtraGreylist 应为 nil，allowed=%v 实际 %v", allowed, sg.ExtraGreylist)
		}
	}
}
