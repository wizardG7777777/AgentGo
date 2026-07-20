package shell

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"

	"agentgo/internal/interaction"
	"agentgo/internal/modes"
)

// strict 模式：原本直接放行的普通命令也必须逐条创建 Interaction；
// 无灰名单模式可捕获时不提供 allow_session 选项。
func TestWrapShellTool_StrictAsksAllCommands(t *testing.T) {
	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
	var calls atomic.Int32
	inner := func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "ok", nil
	}
	// 过滤器无任何灰名单模式——"go build" 在 normal 下会直接放行。
	wrapper := WrapShellTool(inner, NewCommandFilter(nil, nil), service,
		func() string { return "session-test" }, "worker-1", nil, modeStore)

	done := make(chan error, 1)
	go func() {
		_, err := wrapper(context.Background(), map[string]any{"command": "go build ./..."})
		done <- err
	}()
	request := waitPending(t, service, 1)[0]
	if !strings.Contains(request.Prompt, "strict 模式逐条审批") ||
		!strings.Contains(request.Prompt, "go build ./...") {
		t.Fatalf("strict Prompt 应说明逐条审批并含命令原文: %q", request.Prompt)
	}
	for _, option := range request.Options {
		if option.ID == ActionAllowSession {
			t.Fatalf("无灰名单模式时不应提供 allow_session: %+v", request.Options)
		}
	}
	if len(request.Options) != 3 {
		t.Fatalf("strict 全量审批应只有 3 个选项，实际 %d", len(request.Options))
	}
	answerShellRequest(t, service, request, ActionAllowOnce, "")
	if err := <-done; err != nil {
		t.Fatalf("allow_once 后应执行: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner calls = %d", calls.Load())
	}
}

// strict 模式：灰名单命令仍带捕获模式，allow_session 可加入运行时白名单，
// 此后同模式命令直接放行。
func TestWrapShellTool_StrictGreyKeepsAllowSession(t *testing.T) {
	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
	filter := NewCommandFilter(nil, []string{`^git push`})
	var calls atomic.Int32
	wrapper := WrapShellTool(func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "ok", nil
	}, filter, service, func() string { return "session-test" }, "worker-1", nil, modeStore)

	done := make(chan error, 1)
	go func() {
		_, err := wrapper(context.Background(), map[string]any{"command": "git push origin main"})
		done <- err
	}()
	request := waitPending(t, service, 1)[0]
	if !strings.Contains(request.Prompt, "灰名单命令") {
		t.Fatalf("灰名单命中应保留灰名单措辞: %q", request.Prompt)
	}
	var hasAllowSession bool
	for _, option := range request.Options {
		if option.ID == ActionAllowSession {
			hasAllowSession = true
		}
	}
	if !hasAllowSession {
		t.Fatalf("灰名单命中必须提供 allow_session: %+v", request.Options)
	}
	answerShellRequest(t, service, request, ActionAllowSession, "")
	if err := <-done; err != nil {
		t.Fatalf("allow_session: %v", err)
	}
	if got := filter.RuntimeWhitelist(); len(got) != 1 || got[0] != `^git push` {
		t.Fatalf("白名单应加入捕获模式，got %v", got)
	}
	// 第二次同模式调用直接放行，不再创建 Interaction。
	if _, err := wrapper(context.Background(), map[string]any{"command": "git push origin dev"}); err != nil {
		t.Fatalf("白名单命令应直接放行: %v", err)
	}
	if calls.Load() != 2 || waitPendingCount(t, service) != 0 {
		t.Fatalf("calls=%d pending=%d", calls.Load(), waitPendingCount(t, service))
	}
}

// strict 模式：黑名单依旧硬拒，不创建 Interaction。
func TestWrapShellTool_StrictBlacklistStillBlocked(t *testing.T) {
	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
	var executed atomic.Bool
	wrapper := WrapShellTool(func(context.Context, map[string]any) (string, error) {
		executed.Store(true)
		return "", nil
	}, NewCommandFilter([]string{`rm\s+-rf\s+/`}, nil), service,
		func() string { return "session-test" }, "worker-1", nil, modeStore)

	_, err := wrapper(context.Background(), map[string]any{"command": "rm -rf /"})
	if err == nil || !strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("strict 下黑名单应硬拒: %v", err)
	}
	if executed.Load() || waitPendingCount(t, service) != 0 {
		t.Fatalf("黑名单不得执行也不得创建 Interaction: executed=%v", executed.Load())
	}
}

// strict 模式：运行时白名单（用户 allow_session 的显式授权）命中仍直接放行。
func TestWrapShellTool_StrictWhitelistStillAllowed(t *testing.T) {
	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
	filter := NewCommandFilter(nil, []string{`^git push`})
	if err := filter.AddRuntimeWhitelist(`^git push`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	wrapper := WrapShellTool(func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "ok", nil
	}, filter, service, func() string { return "session-test" }, "worker-1", nil, modeStore)

	if _, err := wrapper(context.Background(), map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("白名单命令在 strict 下应直接放行: %v", err)
	}
	if calls.Load() != 1 || waitPendingCount(t, service) != 0 {
		t.Fatalf("calls=%d pending=%d", calls.Load(), waitPendingCount(t, service))
	}
}

// yolo 模式：灰名单 ask 自动放行，不创建 Interaction，并输出中文审计日志。
func TestWrapShellTool_YoloAutoAllowsGrey(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecYolo, modes.TopoTeam)
	var calls atomic.Int32
	wrapper := WrapShellTool(func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "ok", nil
	}, NewCommandFilter(nil, []string{`^git push`}), service,
		func() string { return "session-test" }, "worker-1", nil, modeStore)

	out, err := wrapper(context.Background(), map[string]any{"command": "git push origin main"})
	if err != nil || out != "ok" {
		t.Fatalf("yolo 灰名单应自动放行: out=%q err=%v", out, err)
	}
	if calls.Load() != 1 || waitPendingCount(t, service) != 0 {
		t.Fatalf("yolo 不应创建 Interaction: calls=%d", calls.Load())
	}
	if !strings.Contains(buf.String(), "[yolo] 灰名单命令已自动放行") ||
		!strings.Contains(buf.String(), "git push origin main") {
		t.Fatalf("缺少 yolo 审计日志: %q", buf.String())
	}
}

// yolo 模式：黑名单依旧硬拒；普通命令行为不变。
func TestWrapShellTool_YoloBlacklistStillBlocked(t *testing.T) {
	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecYolo, modes.TopoTeam)
	var calls atomic.Int32
	wrapper := WrapShellTool(func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "ok", nil
	}, NewCommandFilter([]string{`rm\s+-rf\s+/`}, []string{`^git push`}), service,
		func() string { return "session-test" }, "worker-1", nil, modeStore)

	if _, err := wrapper(context.Background(), map[string]any{"command": "rm -rf /"}); err == nil ||
		!strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("yolo 下黑名单应硬拒: %v", err)
	}
	if _, err := wrapper(context.Background(), map[string]any{"command": "go test ./..."}); err != nil {
		t.Fatalf("yolo 下普通命令应直接放行: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner calls = %d", calls.Load())
	}
}

// 并发场景：strict 下多条普通命令各自创建独立请求，回答按 digest 绑定互不串扰。
func TestWrapShellTool_StrictConcurrentRequestsCorrelated(t *testing.T) {
	service := interaction.NewService(nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
	inner := func(_ context.Context, args map[string]any) (string, error) {
		return args["command"].(string), nil
	}
	wrapper := WrapShellTool(inner, NewCommandFilter(nil, nil), service,
		func() string { return "session-test" }, "worker-1", nil, modeStore)
	type result struct {
		command string
		out     string
		err     error
	}
	results := make(chan result, 2)
	for _, command := range []string{"cmd-allow", "cmd-deny"} {
		go func(command string) {
			out, err := wrapper(context.Background(), map[string]any{"command": command})
			results <- result{command: command, out: out, err: err}
		}(command)
	}
	requests := waitPending(t, service, 2)
	for _, request := range requests {
		option := ActionDeny
		if request.Metadata[MetadataCommand] == "cmd-allow" {
			option = ActionAllowOnce
		}
		answerShellRequest(t, service, request, option, "")
	}
	for range 2 {
		res := <-results
		if res.command == "cmd-allow" {
			if res.err != nil || res.out != res.command {
				t.Fatalf("allow result = %+v", res)
			}
		} else if res.err == nil || !strings.Contains(res.err.Error(), "拒绝") {
			t.Fatalf("deny result = %+v", res)
		}
	}
}

// execModeOf nil 安全：nil store 必须等价 normal。
func TestExecModeOf_NilStoreIsNormal(t *testing.T) {
	if got := execModeOf(nil); got != modes.ExecNormal {
		t.Fatalf("execModeOf(nil) = %v, want normal", got)
	}
}

// IsRuntimeWhitelisted：只有 allow_session 加入的模式命中，黑名单不受影响。
func TestCommandFilter_IsRuntimeWhitelisted(t *testing.T) {
	filter := NewCommandFilter([]string{`rm\s+-rf\s+/`}, nil)
	if filter.IsRuntimeWhitelisted("anything") {
		t.Fatal("空白名单不应命中")
	}
	if err := filter.AddRuntimeWhitelist(`^git push`); err != nil {
		t.Fatal(err)
	}
	if !filter.IsRuntimeWhitelisted("git push origin main") {
		t.Fatal("已加入白名单的模式应命中")
	}
	if filter.IsRuntimeWhitelisted("git pull") {
		t.Fatal("未加入白名单的命令不应命中")
	}
}
