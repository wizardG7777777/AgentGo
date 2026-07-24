package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
	"agentgo/internal/modes"
	"agentgo/internal/roster"
	"agentgo/internal/tools"
)

// newWriteToolRegistry 按 runner.New 的工具装配路径构造只含写工具的 registry，
// 供 wrapFileWriteApproval 装配断言（New 构造的 ToolRegistry 不外露）。
func newWriteToolRegistry(t *testing.T, projectRoot string) *agent.ToolRegistry {
	t.Helper()
	registry := agent.NewToolRegistry()
	readGroup := tools.LocalReadGroup{
		Workdir: &tools.DefaultWorkdir{ProjectRoot: projectRoot},
		Cache:   agent.NewFileStateCache(1),
	}
	tools.RegisterGroups(registry, tools.LocalWriteGroup{
		LocalReadGroup: readGroup,
		Roster:         roster.NewMemoryRoster(),
		AgentID:        "worker-1",
	})
	return registry
}

func dispatchWriteFile(registry *agent.ToolRegistry, path, content string) (string, error) {
	return registry.Dispatch(context.Background(), llm.ToolCall{
		Name:      "write_file",
		Arguments: map[string]any{"path": path, "content": content},
	})
}

// 装配断言：strict + Interaction 服务缺失时，runner 的 write_file 被 fail-closed 拦截。
func TestWrapFileWriteApproval_StrictBlocksWithoutService(t *testing.T) {
	dir := t.TempDir()
	registry := newWriteToolRegistry(t, dir)
	deps := RunnerDeps{Modes: modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)}
	wrapFileWriteApproval(registry, deps, "worker-1", nil)

	target := filepath.Join(dir, "a.txt")
	_, err := dispatchWriteFile(registry, target, "x")
	if err == nil || !strings.Contains(err.Error(), "Interaction 服务不可用") {
		t.Fatalf("strict 下缺少 Interaction 服务应 fail-closed: %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("被拦截的写入不应落盘: %v", statErr)
	}
}

// 装配断言：nil Modes（等价 normal）时 write_file 透传执行。
func TestWrapFileWriteApproval_NormalPassthrough(t *testing.T) {
	dir := t.TempDir()
	registry := newWriteToolRegistry(t, dir)
	wrapFileWriteApproval(registry, RunnerDeps{}, "worker-1", nil)

	target := filepath.Join(dir, "a.txt")
	out, err := dispatchWriteFile(registry, target, "hello")
	if err != nil || !strings.Contains(out, "文件已写入") {
		t.Fatalf("normal 下写入应成功: out=%q err=%v", out, err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "hello" {
		t.Fatalf("落盘内容 = %q", data)
	}
}

// 装配闭环：strict 下 write_file 创建 file_write 审批请求，用户 allow_once 后落盘。
func TestWrapFileWriteApproval_StrictFullCycle(t *testing.T) {
	dir := t.TempDir()
	registry := newWriteToolRegistry(t, dir)
	service := interaction.NewService(nil)
	deps := RunnerDeps{
		Modes:        modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam),
		Interactions: service,
		SessionID:    func() string { return "session-test" },
	}
	wrapFileWriteApproval(registry, deps, "worker-1", nil)

	target := filepath.Join(dir, "b.txt")
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := dispatchWriteFile(registry, target, "approved")
		done <- result{out, err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	var pending []interaction.Request
	for {
		pending, _ = service.ListPending(context.Background(), "")
		if len(pending) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("strict 下未创建 file_write 审批请求")
		}
		time.Sleep(time.Millisecond)
	}
	request := pending[0]
	if request.Purpose != tools.PurposeFileWrite || request.Resolution.Handler != tools.ResolutionHandlerFileWrite {
		t.Fatalf("Purpose/Handler = %s/%s", request.Purpose, request.Resolution.Handler)
	}
	if !strings.Contains(request.Prompt, target) {
		t.Fatalf("Prompt 应含目标路径: %q", request.Prompt)
	}
	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "allow_once",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), locked.ID, locked.Version); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("allow_once 后写入应成功: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("授权后 write_file 未返回")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "approved" {
		t.Fatalf("落盘内容 = %q", data)
	}
}

// resolveToolGroups 必须把 Modes 透传给 ShellGroup（exec 轴短路依赖），
// 否则 strict/yolo 对 runner 的 run_shell 不生效。
func TestResolveToolGroups_WiresModesToShellGroup(t *testing.T) {
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecYolo, modes.TopoTeam)
	groups := resolveToolGroups("w-1", RunnerDeps{Modes: modeStore}, &CurrentTaskHolder{},
		agent.NewFinalizationHolder(), agent.NewSubmitState(),
		agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, nil)

	var shellGroup *tools.ShellGroup
	for i := range groups {
		if sg, ok := groups[i].(tools.ShellGroup); ok {
			shellGroup = &sg
		}
	}
	if shellGroup == nil {
		t.Fatal("resolveToolGroups 应包含 ShellGroup")
	}
	if shellGroup.Modes != modeStore {
		t.Fatal("ShellGroup.Modes 未接线到 RunnerDeps.Modes")
	}
}
