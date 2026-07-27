package runner

// workspace_wiring_test.go 验证「按任务写时复制执行隔离」的 runner 接线：
//   - New() 为每个 Runner 构造独立 *workspace.Swapper，作为 Workdir 注入
//     LocalRead/LocalWrite 组、作为 ActiveViewer 注入 shell 组；
//   - 共享 *workspace.Manager 与 per-runner Swapper 赋到 Agent 新字段，
//     replan 通道在有 PlanCoordinator 时接线；
//   - resolveToolGroups 对未实现 ActiveViewer 的 Workdir（如单测直构的
//     DefaultWorkdir）保持旧行为（ShellGroup.ActiveViewer=nil）。

import (
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/workspace"
)

// resolveToolGroups：Swapper 同时作为 Workdir（读/写组）与 ActiveViewer
// （shell 组）注入；DefaultWorkdir 不满足 ActiveViewer 时 shell 保持旧行为。
func TestResolveToolGroups_InjectsSwapperAsWorkdirAndActiveViewer(t *testing.T) {
	swapper := workspace.NewSwapper(t.TempDir())
	groups := resolveToolGroups("w-1", nil, RunnerDeps{}, &CurrentTaskHolder{},
		agent.NewFinalizationHolder(), agent.NewSubmitState(),
		agent.NewFileStateCache(1), swapper, nil)

	var readGroup *tools.LocalReadGroup
	var writeGroup *tools.LocalWriteGroup
	for i := range groups {
		switch g := groups[i].(type) {
		case tools.LocalReadGroup:
			rg := g
			readGroup = &rg
		case tools.LocalWriteGroup:
			wg := g
			writeGroup = &wg
		}
	}
	if readGroup == nil || writeGroup == nil {
		t.Fatal("resolveToolGroups 应包含 LocalReadGroup 与 LocalWriteGroup")
	}
	if readGroup.Workdir != swapper {
		t.Fatal("LocalReadGroup.Workdir 应为 per-runner Swapper")
	}
	if writeGroup.Workdir != swapper {
		t.Fatal("LocalWriteGroup.Workdir 应为 per-runner Swapper")
	}
	// Swapper 满足 PathOverlayer——写工具能经它解析隔离写入位置。
	if _, ok := interface{}(writeGroup.Workdir).(tools.PathOverlayer); !ok {
		t.Fatal("LocalWriteGroup.Workdir 应实现 tools.PathOverlayer")
	}

	shellGroup := mustShellGroup(t, groups)
	if shellGroup.ActiveViewer != swapper {
		t.Fatal("ShellGroup.ActiveViewer 应为同一 Swapper")
	}

	// 旧行为回归：DefaultWorkdir 未实现 ActiveViewer 时 shell 不装配该字段。
	plain := resolveToolGroups("w-1", nil, RunnerDeps{}, &CurrentTaskHolder{},
		agent.NewFinalizationHolder(), agent.NewSubmitState(),
		agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, nil)
	if sg := mustShellGroup(t, plain); sg.ActiveViewer != nil {
		t.Fatalf("DefaultWorkdir 装配时 ShellGroup.ActiveViewer 应为 nil，实际 %T", sg.ActiveViewer)
	}
}

// New() 末端接线：共享 Manager 与 per-runner Swapper 到达 Agent 字段；
// PlanCoordinator 非 nil 时 replan 通道接通，nil 时保持 nil（隔离冲突只
// 转 failed，不登记 replan）。
func TestNewWiresWorkspaceFieldsToAgent(t *testing.T) {
	mainRoot := t.TempDir()
	mgr := workspace.NewManager(mainRoot, nil)
	deps := RunnerDeps{
		Store:            store.NewMemoryTaskStore(nil, 32, 1, 60),
		Roster:           roster.NewMemoryRoster(),
		LLMClient:        idleTestLLM{},
		ProjectRoot:      mainRoot,
		WorkspaceManager: mgr,
		PlanCoordinator:  plan.NewCoordinator(plan.NewMemoryStore(), nil),
	}

	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-1", Kind: "worker", AllowedTools: []string{"read_file"},
		AgentMaxLoops: 4,
	}, deps)
	a := rn.Agent()
	if a.WorkspaceManager != mgr {
		t.Fatal("Agent.WorkspaceManager 应为 deps 注入的共享 Manager")
	}
	swapper, ok := a.WorkspaceActivator.(*workspace.Swapper)
	if !ok || swapper == nil {
		t.Fatalf("Agent.WorkspaceActivator 应为 *workspace.Swapper，实际 %T", a.WorkspaceActivator)
	}
	if swapper.Get() != mainRoot {
		t.Fatalf("Swapper 主根 = %s，want %s", swapper.Get(), mainRoot)
	}
	if a.WorkspaceReplanRequester == nil {
		t.Fatal("有 PlanCoordinator 时 Agent.WorkspaceReplanRequester 应接线")
	}

	// 无 PlanCoordinator：replan 通道保持 nil，其余字段不受影响。
	deps.PlanCoordinator = nil
	rn = New(config.AgentRuntimeConfig{
		InstanceID: "worker-2", Kind: "worker", AllowedTools: []string{"read_file"},
		AgentMaxLoops: 4,
	}, deps)
	a = rn.Agent()
	if a.WorkspaceReplanRequester != nil {
		t.Fatal("无 PlanCoordinator 时 Agent.WorkspaceReplanRequester 应为 nil")
	}
	if a.WorkspaceManager != mgr || a.WorkspaceActivator == nil {
		t.Fatal("无 PlanCoordinator 时 Manager/Activator 接线不应受影响")
	}
}
