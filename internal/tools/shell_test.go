package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/llm"
	"agentgo/internal/shell"
)

// newTestShellGroup 构造一个常规单元测试用 ShellGroup，带宽度为 1 的审批通道。
func newTestShellGroup(t *testing.T, fallbackDir string, filter *shell.CommandFilter) (ShellGroup, chan shell.ApprovalRequest) {
	t.Helper()
	approvalCh := make(chan shell.ApprovalRequest, 1)
	g := ShellGroup{
		Workdir:    &DefaultWorkdir{ProjectRoot: fallbackDir},
		TimeoutSec: 10,
		ApprovalCh: approvalCh,
		AgentID:    "test-agent",
		Filter:     filter,
	}
	return g, approvalCh
}

// dispatchRunShell 新建 registry 注册 ShellGroup 并通过 Dispatch 调用 run_shell。
func dispatchRunShell(ctx context.Context, g ShellGroup, args map[string]any) (string, error) {
	r := agent.NewToolRegistry()
	g.Register(r)
	return r.Dispatch(ctx, llm.ToolCall{Name: "run_shell", Arguments: args})
}

// emptyFilter 返回一个空 filter，所有命令都会被放行。
func emptyFilter() *shell.CommandFilter {
	return shell.NewCommandFilter(nil, nil)
}

func TestShellGroup_Register_OneTool(t *testing.T) {
	r := agent.NewToolRegistry()
	g, _ := newTestShellGroup(t, t.TempDir(), nil)
	g.Register(r)

	defs := r.Defs()
	if len(defs) != 1 {
		t.Fatalf("expected exactly 1 tool, got %d", len(defs))
	}
	if defs[0].Name != "run_shell" {
		t.Fatalf("expected run_shell, got %s", defs[0].Name)
	}
}

func TestRunShell_BasicEcho(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	g, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	out, err := dispatchRunShell(context.Background(), g, map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") {
		t.Errorf("expected exit_code 0, got: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected output to contain 'hello', got: %s", out)
	}
}

func TestRunShell_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	g, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	out, err := dispatchRunShell(context.Background(), g, map[string]any{
		"command": "false",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if strings.Contains(out, "exit_code: 0") {
		t.Errorf("expected non-zero exit code, got: %s", out)
	}
}

func TestRunShell_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	g, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	start := time.Now()
	_, err := dispatchRunShell(context.Background(), g, map[string]any{
		"command":     "sleep 5",
		"timeout_sec": float64(1),
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("expected timeout error message, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("expected to return within ~2s, took %v", elapsed)
	}
}

func TestRunShell_WorkingDirOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	tmp := t.TempDir()
	sentinel := "sentinel_file.txt"
	if err := os.WriteFile(filepath.Join(tmp, sentinel), []byte("x"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// fallback 指向一个不同的空目录
	other := t.TempDir()
	g, _ := newTestShellGroup(t, other, emptyFilter())

	out, err := dispatchRunShell(context.Background(), g, map[string]any{
		"command":     "ls",
		"working_dir": tmp,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, sentinel) {
		t.Errorf("expected output to contain sentinel %q, got: %s", sentinel, out)
	}
}

func TestRunShell_WorkingDirFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	tmp := t.TempDir()
	sentinel := "fallback_sentinel.txt"
	if err := os.WriteFile(filepath.Join(tmp, sentinel), []byte("x"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	g, _ := newTestShellGroup(t, tmp, emptyFilter())

	out, err := dispatchRunShell(context.Background(), g, map[string]any{
		"command": "ls",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, sentinel) {
		t.Errorf("expected output to contain sentinel %q, got: %s", sentinel, out)
	}
}

func TestRunShell_BlacklistBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	// danger_marker 不对应任何真实命令，即使拦截失败也不会产生破坏。
	filter := shell.NewCommandFilter([]string{`^danger_marker$`}, nil)
	g, _ := newTestShellGroup(t, t.TempDir(), filter)

	_, err := dispatchRunShell(context.Background(), g, map[string]any{
		"command": "danger_marker",
	})
	if err == nil {
		t.Fatalf("expected blacklist error, got nil")
	}
	if !strings.Contains(err.Error(), "黑名单") {
		t.Errorf("expected blacklist error message, got: %v", err)
	}
}

func TestRunShell_GraylistTriggersApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}
	filter := shell.NewCommandFilter(nil, []string{`^echo gray$`})
	g, approvalCh := newTestShellGroup(t, t.TempDir(), filter)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)

	go func() {
		out, err := dispatchRunShell(context.Background(), g, map[string]any{
			"command": "echo gray",
		})
		done <- result{out, err}
	}()

	// 等待审批请求到达
	select {
	case req := <-approvalCh:
		if req.AgentID != "test-agent" {
			t.Errorf("expected agent id test-agent, got %s", req.AgentID)
		}
		if req.Command != "echo gray" {
			t.Errorf("expected command 'echo gray', got %s", req.Command)
		}
		// 批准放行
		req.ReplyCh <- shell.ApprovalReply{Approved: true}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for approval request")
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("unexpected err after approval: %v", res.err)
		}
		if !strings.Contains(res.out, "gray") {
			t.Errorf("expected output to contain 'gray', got %s", res.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run_shell did not return after approval")
	}
}
