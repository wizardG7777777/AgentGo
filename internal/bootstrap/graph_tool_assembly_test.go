package bootstrap

// 本文件是 C5b 的 bootstrap 级装配冒烟：scheduler.New 以真实 GraphRuntime/
// GraphStore 装配后，Scheduler 工具注册表中必须含 submit_graph / patch_graph
// （跨包握手断言——各包单测都绿不代表装配接上）。

import (
	"io"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
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
	gs, rt, err := wireGraphRuntime(cfg, taskStore, reactorReg, nil)
	if err != nil {
		t.Fatalf("wireGraphRuntime 应成功: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() }) // Windows 纪律：先 Close 再让 TempDir 清理

	// 与 bootstrap.go Step 5 同款调用形态（graphRuntime/graphStore 注入）。
	bundle := scheduler.New(
		taskStore, roster.NewMemoryRoster(), &planGateScriptedLLM{}, ch, cfg,
		nil, mailbox.NewRegistry(8), nil, nil, nil, nil, nil, nil, nil, nil,
		io.Discard, io.Discard, nil, rt, gs, nil,
	)
	if bundle == nil || bundle.SchedulerExec == nil || bundle.ToolReg == nil {
		t.Fatal("scheduler.New 应返回非空 Bundle（含 ToolReg）")
	}
	registered := map[string]bool{}
	for _, d := range bundle.ToolReg.Defs() {
		registered[d.Name] = true
	}
	for _, name := range []string{"submit_graph", "read_graph", "patch_graph"} {
		if !registered[name] {
			t.Errorf("Scheduler 工具注册表应含 %s（C5b 图控制面）", name)
		}
	}
}
