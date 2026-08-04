package tools

import (
	"context"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/shell"
	"agentgo/internal/trace"
)

// installShellTraceCapture 替换包级默认 trace Dispatcher 并在测试结束时还原。
// 复用 plan_control_graph_test.go 的 captureGraphTraceDispatcher（同包）。
func installShellTraceCapture(t *testing.T) *captureGraphTraceDispatcher {
	t.Helper()
	d := &captureGraphTraceDispatcher{}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(d)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })
	return d
}

func shellExecutedEvents(d *captureGraphTraceDispatcher) []trace.Event {
	var out []trace.Event
	for _, ev := range d.events {
		if ev.Kind == trace.KindShellExecuted {
			out = append(out, ev)
		}
	}
	return out
}

// D4：run_shell 每次真实执行完成必须恰好 emit 一条 KindShellExecuted，
// 携带命令、退出码、耗时、结果与 task/agent 标识。
// 命令用 shell 内建 echo，Windows(cmd /C) 与 Unix(sh -c) 均可运行。
func TestRunShell_EmitsShellExecuted_Success(t *testing.T) {
	d := installShellTraceCapture(t)
	g, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())

	ctx := agent.WithAgentContext(context.Background(), "agent-from-ctx", "task-42", 1)
	out, err := dispatchRunShell(ctx, g, map[string]any{"command": "echo hello-trace"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	events := shellExecutedEvents(d)
	if len(events) != 1 {
		t.Fatalf("KindShellExecuted count=%d want 1 (all=%+v)", len(events), d.events)
	}
	ev := events[0]
	if ev.TaskID != "task-42" {
		t.Errorf("TaskID=%q want task-42", ev.TaskID)
	}
	if ev.AgentID != "test-agent" {
		t.Errorf("AgentID=%q want test-agent", ev.AgentID)
	}
	if ev.Tool != "run_shell" {
		t.Errorf("Tool=%q want run_shell", ev.Tool)
	}
	if ev.ShellExec == nil {
		t.Fatal("ShellExec payload missing")
	}
	if ev.ShellExec.Command != "echo hello-trace" {
		t.Errorf("Command=%q want 'echo hello-trace'", ev.ShellExec.Command)
	}
	if ev.ShellExec.ExitCode != 0 {
		t.Errorf("ExitCode=%d want 0", ev.ShellExec.ExitCode)
	}
	if ev.ShellExec.Outcome != "success" {
		t.Errorf("Outcome=%q want success", ev.ShellExec.Outcome)
	}
	if ev.ShellExec.DurationMS < 0 {
		t.Errorf("DurationMS=%d should be >= 0", ev.ShellExec.DurationMS)
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp should be set by trace.Emit")
	}
}

// D4：非零退出同样 emit 一条事件，Outcome=failure 且带退出码。
func TestRunShell_EmitsShellExecuted_NonZeroExit(t *testing.T) {
	d := installShellTraceCapture(t)
	g, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())

	// exit 是 cmd 与 sh 共有内建，两端退出码均为 3
	out, err := dispatchRunShell(context.Background(), g, map[string]any{"command": "exit 3"})
	if err != nil {
		t.Fatalf("非零退出不应返回 error（结果经字符串传达）: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	events := shellExecutedEvents(d)
	if len(events) != 1 {
		t.Fatalf("KindShellExecuted count=%d want 1", len(events))
	}
	exec := events[0].ShellExec
	if exec == nil {
		t.Fatal("ShellExec payload missing")
	}
	if exec.ExitCode != 3 {
		t.Errorf("ExitCode=%d want 3", exec.ExitCode)
	}
	if exec.Outcome != "failure" {
		t.Errorf("Outcome=%q want failure", exec.Outcome)
	}
}

// D4：黑名单拦截的命令不进入执行路径，不得 emit shell_executed。
func TestRunShell_Blocked_NoEmit(t *testing.T) {
	d := installShellTraceCapture(t)
	filter := shell.NewCommandFilter([]string{`^danger_marker_trace$`}, nil)
	g, _ := newTestShellGroup(t, t.TempDir(), filter)

	if _, err := dispatchRunShell(context.Background(), g, map[string]any{"command": "danger_marker_trace"}); err == nil {
		t.Fatal("expected blacklist error")
	}
	if got := len(shellExecutedEvents(d)); got != 0 {
		t.Fatalf("blocked command emitted %d shell_executed events, want 0", got)
	}
}
