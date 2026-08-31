package bootstrap

// 本文件是 C4 的 bootstrap 级装配冒烟：新 root Scheduler 必须含
// Draft/Commit/Start，且不再暴露 submit_graph；runtime read/patch compatibility
// 与 Graph controller 的结构化收尾通道仍在。
// （跨包握手断言——各包单测都绿不代表装配接上）。

import (
	"io"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/graph"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/reactor"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

func TestSchedulerGraphToolsAssembled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProjectRoot = t.TempDir()
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}

	ch := make(chan model.Event, 64)
	taskStore := store.NewMemoryTaskStore(ch, 100, 2, 300)
	reactorReg := reactor.NewRegistry()
	t.Cleanup(func() { reactorReg.Quiesce(0) })
	gs, rt, err := wireGraphRuntime(cfg, taskStore, reactorReg, nil, nil)
	if err != nil {
		t.Fatalf("wireGraphRuntime 应成功: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() }) // Windows 纪律：先 Close 再让 TempDir 清理
	authoringStore, err := graph.NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoringStore.Close() })
	policies, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	authoringRuntime := &graph.AuthoringRuntime{Authoring: authoringStore, Runtime: rt}

	// 与 bootstrap.go Step 5 同款调用形态（graphRuntime/graphStore 注入）。
	bundle := scheduler.New(
		taskStore, roster.NewMemoryRoster(), &planGateScriptedLLM{}, ch, cfg,
		nil, mailbox.NewRegistry(8), nil, nil, nil, nil, nil, nil, nil, nil,
		io.Discard, io.Discard, nil, rt, gs, nil,
		scheduler.GraphAuthoringDeps{
			Store: authoringStore, Runtime: authoringRuntime,
			Compiler: graph.DefinitionCompiler{Policies: policies},
		},
	)
	if bundle == nil || bundle.SchedulerExec == nil || bundle.ToolReg == nil {
		t.Fatal("scheduler.New 应返回非空 Bundle（含 ToolReg）")
	}
	registered := map[string]bool{}
	for _, d := range bundle.ToolReg.Defs() {
		registered[d.Name] = true
	}
	for _, name := range []string{
		"create_graph_draft", "patch_graph_draft", "read_graph_draft",
		"validate_graph_draft", "commit_graph_draft", "start_graph",
		"propose_graph_change", "read_graph_change", "validate_graph_change", "commit_graph_change",
		"submit_graph_change_decision",
		"read_graph", "patch_graph", "submit_task_result",
	} {
		if !registered[name] {
			t.Errorf("Scheduler 工具注册表应含 %s（C4 Graph 控制面）", name)
		}
	}
	if registered["submit_graph"] {
		t.Error("新 root Scheduler registry 不得暴露一体化 submit_graph")
	}
}
