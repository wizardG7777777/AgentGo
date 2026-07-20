package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
	"agentgo/internal/modes"
	"agentgo/internal/shell"
)

func newTestShellGroup(t *testing.T, fallbackDir string, filter *shell.CommandFilter) (ShellGroup, *interaction.Service) {
	t.Helper()
	interactions := interaction.NewService(nil)
	return ShellGroup{
		Workdir:      &DefaultWorkdir{ProjectRoot: fallbackDir},
		TimeoutSec:   10,
		Interactions: interactions,
		SessionID:    func() string { return "session-tools-test" },
		AgentID:      "test-agent",
		Filter:       filter,
	}, interactions
}

func dispatchRunShell(ctx context.Context, group ShellGroup, args map[string]any) (string, error) {
	registry := agent.NewToolRegistry()
	group.Register(registry)
	return registry.Dispatch(ctx, llm.ToolCall{Name: "run_shell", Arguments: args})
}

func emptyFilter() *shell.CommandFilter { return shell.NewCommandFilter(nil, nil) }

func waitToolInteraction(t *testing.T, service *interaction.Service) interaction.Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		requests, err := service.ListPending(context.Background(), "session-tools-test")
		if err != nil {
			t.Fatal(err)
		}
		if len(requests) == 1 {
			return requests[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 Shell Interaction 超时，pending=%d", len(requests))
		}
		time.Sleep(time.Millisecond)
	}
}

func completeToolInteraction(t *testing.T, service *interaction.Service, request interaction.Request, optionID string) {
	t.Helper()
	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: optionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), locked.ID, locked.Version); err != nil {
		t.Fatal(err)
	}
}

func TestShellGroup_Register_OneTool(t *testing.T) {
	registry := agent.NewToolRegistry()
	group, _ := newTestShellGroup(t, t.TempDir(), nil)
	group.Register(registry)
	defs := registry.Defs()
	if len(defs) != 1 || defs[0].Name != "run_shell" {
		t.Fatalf("Defs = %+v", defs)
	}
}

func TestRunShell_BasicEcho(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	group, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "echo hello"})
	if err != nil || !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "hello") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRunShell_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	group, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "false"})
	if err != nil || strings.Contains(out, "exit_code: 0") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRunShell_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	group, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	started := time.Now()
	_, err := dispatchRunShell(context.Background(), group, map[string]any{
		"command": "sleep 5", "timeout_sec": float64(1),
	})
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("命令超时返回过慢")
	}
}

func TestRunShell_WorkingDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	t.Run("override", func(t *testing.T) {
		override := t.TempDir()
		if err := os.WriteFile(filepath.Join(override, "override.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		group, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
		out, err := dispatchRunShell(context.Background(), group, map[string]any{
			"command": "ls", "working_dir": override,
		})
		if err != nil || !strings.Contains(out, "override.txt") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		fallback := t.TempDir()
		if err := os.WriteFile(filepath.Join(fallback, "fallback.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		group, _ := newTestShellGroup(t, fallback, emptyFilter())
		out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "ls"})
		if err != nil || !strings.Contains(out, "fallback.txt") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
}

func TestRunShell_BlacklistBlocked(t *testing.T) {
	group, _ := newTestShellGroup(t, t.TempDir(), shell.NewCommandFilter([]string{`^danger_marker$`}, nil))
	_, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "danger_marker"})
	if err == nil || !strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunShell_InteractionAllowAndFallbackDirectoryBinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip real shell execution on windows")
	}
	fallback := t.TempDir()
	group, service := newTestShellGroup(t, fallback, shell.NewCommandFilter(nil, []string{`^echo gray$`}))
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "echo gray"})
		done <- result{out: out, err: err}
	}()
	request := waitToolInteraction(t, service)
	if request.Metadata[shell.MetadataWorkingDir] != fallback {
		t.Fatalf("Interaction working_dir = %q, want fallback %q",
			request.Metadata[shell.MetadataWorkingDir], fallback)
	}
	if request.SessionID != "session-tools-test" || request.Origin.AgentID != "test-agent" {
		t.Fatalf("request = %+v", request)
	}
	completeToolInteraction(t, service, request, shell.ActionAllowOnce)
	select {
	case got := <-done:
		if got.err != nil || !strings.Contains(got.out, "gray") {
			t.Fatalf("result = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("授权后 run_shell 未返回")
	}
}

func TestShellGroup_InteractionWaitHook(t *testing.T) {
	group, service := newTestShellGroup(t, t.TempDir(), shell.NewCommandFilter(nil, []string{`^gray_marker$`}))
	var mu sync.Mutex
	var calls []bool
	group.InteractionWaitHook = func(waiting bool) {
		mu.Lock()
		calls = append(calls, waiting)
		mu.Unlock()
	}
	done := make(chan error, 1)
	go func() {
		_, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "gray_marker"})
		done <- err
	}()
	request := waitToolInteraction(t, service)
	completeToolInteraction(t, service, request, shell.ActionDeny)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("hook calls = %v", calls)
	}
}

func TestShellGroup_NilInteractionWaitHook(t *testing.T) {
	group, service := newTestShellGroup(t, t.TempDir(), shell.NewCommandFilter(nil, []string{`^gray_marker$`}))
	done := make(chan error, 1)
	go func() {
		_, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "gray_marker"})
		done <- err
	}()
	request := waitToolInteraction(t, service)
	completeToolInteraction(t, service, request, shell.ActionDeny)
	if err := <-done; err == nil {
		t.Fatal("拒绝应返回 error")
	}
}

func TestShellGroup_NilInteractionServiceFailsClosed(t *testing.T) {
	group := ShellGroup{
		Workdir: &DefaultWorkdir{ProjectRoot: t.TempDir()}, AgentID: "test-agent",
		Filter: shell.NewCommandFilter(nil, []string{`^gray_marker$`}),
	}
	_, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "gray_marker"})
	if err == nil || !strings.Contains(err.Error(), "Interaction 服务不可用") {
		t.Fatalf("error = %v", err)
	}
}

// exec=strict 经 ShellGroup.Modes 接线生效：原本直接放行的普通命令也会
// 创建 shell_command 审批请求；用户 allow_once 后才真正执行。
func TestShellGroup_ModesStrictAsksPlainCommand(t *testing.T) {
	group, service := newTestShellGroup(t, t.TempDir(), emptyFilter())
	group.Modes = modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "echo strict-ask"})
		done <- result{out: out, err: err}
	}()
	request := waitToolInteraction(t, service)
	if !strings.Contains(request.Prompt, "strict 模式逐条审批") {
		t.Fatalf("strict Prompt = %q", request.Prompt)
	}
	completeToolInteraction(t, service, request, shell.ActionAllowOnce)
	select {
	case got := <-done:
		if got.err != nil || !strings.Contains(got.out, "strict-ask") {
			t.Fatalf("result = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("授权后 run_shell 未返回")
	}
}

// exec=yolo 经 ShellGroup.Modes 接线生效：灰名单命令自动放行，不创建 Interaction。
func TestShellGroup_ModesYoloAutoAllowsGrey(t *testing.T) {
	group, service := newTestShellGroup(t, t.TempDir(), shell.NewCommandFilter(nil, []string{`^echo yolo$`}))
	group.Modes = modes.NewStore(modes.GateImmediate, modes.ExecYolo, modes.TopoTeam)
	out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": "echo yolo"})
	if err != nil || !strings.Contains(out, "yolo") {
		t.Fatalf("yolo 灰名单应自动放行: out=%q err=%v", out, err)
	}
	if pending, listErr := service.ListPending(context.Background(), ""); listErr != nil || len(pending) != 0 {
		t.Fatalf("yolo 不应创建 Interaction: pending=%d err=%v", len(pending), listErr)
	}
}
