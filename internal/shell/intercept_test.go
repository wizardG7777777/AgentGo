package shell

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
)

func TestCommandFilter_DefaultRules(t *testing.T) {
	filter := NewCommandFilter(DefaultBlacklist, DefaultGreylist)
	tests := []struct {
		command string
		action  string
	}{
		{"rm -rf /", "block"},
		{"echo ok && rm -rf /home/user", "block"},
		{"mkfs.ext4 /dev/sda1", "block"},
		{"git push origin main", "ask"},
		{"sudo git push --force", "ask"},
		{"pip install requests", "ask"},
		{"go test ./...", "allow"},
		{"git status", "allow"},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			action, pattern := filter.Check(test.command)
			if action != test.action {
				t.Fatalf("Check(%q) = %q, want %q", test.command, action, test.action)
			}
			if action != "allow" && pattern == "" {
				t.Fatal("block/ask 必须返回匹配的原始模式")
			}
		})
	}
}

func TestCommandFilter_BlacklistPriorityAndInvalidRegex(t *testing.T) {
	filter := NewCommandFilter([]string{"[invalid", `danger`}, []string{"[invalid", `danger`})
	action, _ := filter.Check("danger")
	if action != "block" {
		t.Fatalf("黑名单应优先，got %q", action)
	}
}

func TestCommandFilter_RuntimeWhitelist(t *testing.T) {
	filter := NewCommandFilter(DefaultBlacklist, DefaultGreylist)
	if err := filter.AddRuntimeWhitelist(`git\s+push`); err != nil {
		t.Fatal(err)
	}
	if err := filter.AddRuntimeWhitelist(`git\s+push`); err != nil {
		t.Fatalf("重复添加应幂等: %v", err)
	}
	if got := filter.RuntimeWhitelist(); len(got) != 1 || got[0] != `git\s+push` {
		t.Fatalf("RuntimeWhitelist = %v", got)
	}
	if action, _ := filter.Check("git push origin main"); action != "allow" {
		t.Fatalf("白名单未短路灰名单: %s", action)
	}
	if err := filter.AddRuntimeWhitelist(`rm`); err != nil {
		t.Fatal(err)
	}
	if action, _ := filter.Check("rm -rf /"); action != "block" {
		t.Fatalf("运行时白名单不能覆盖黑名单: %s", action)
	}
	if err := filter.AddRuntimeWhitelist(""); err == nil {
		t.Fatal("空 pattern 应报错")
	}
	if err := filter.AddRuntimeWhitelist("[bad"); err == nil {
		t.Fatal("非法正则应报错")
	}
}

func waitPending(t *testing.T, service *interaction.Service, count int) []interaction.Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		requests, err := service.ListPending(context.Background(), "")
		if err != nil {
			t.Fatalf("ListPending: %v", err)
		}
		if len(requests) >= count {
			return requests
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 条 Interaction 超时，当前 %d", count, len(requests))
		}
		time.Sleep(time.Millisecond)
	}
}

func answerShellRequest(t *testing.T, service *interaction.Service, request interaction.Request, optionID, text string) interaction.Request {
	t.Helper()
	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version,
		OptionID: optionID, Text: text, RespondedBy: "test-user",
	})
	if err != nil {
		t.Fatalf("BeginResolve(%s): %v", optionID, err)
	}
	completed, err := service.Complete(context.Background(), locked.ID, locked.Version)
	if err != nil {
		t.Fatalf("Complete(%s): %v", optionID, err)
	}
	return completed
}

func testWrapper(
	t *testing.T,
	filter *CommandFilter,
	service *interaction.Service,
	inner agent.ToolFunc,
	waitHook func(bool),
) agent.ToolFunc {
	t.Helper()
	// nil modeStore 等价 normal——既有用例覆盖的正是旧行为保持不变。
	return WrapShellTool(inner, filter, service, func() string { return "session-test" }, "worker-1", waitHook, nil)
}

func TestWrapShellTool_BlockAndAllow(t *testing.T) {
	var calls atomic.Int32
	inner := func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "ok", nil
	}
	filter := NewCommandFilter([]string{`^blocked$`}, []string{`^grey$`})
	wrapper := testWrapper(t, filter, nil, inner, nil)
	if _, err := wrapper(context.Background(), map[string]any{"command": "blocked"}); err == nil || !strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("黑名单 error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("黑名单命令不应执行")
	}
	out, err := wrapper(context.Background(), map[string]any{"command": "safe"})
	if err != nil || out != "ok" || calls.Load() != 1 {
		t.Fatalf("安全命令 = %q, %v, calls=%d", out, err, calls.Load())
	}
}

func TestWrapShellTool_NilServiceFailsClosed(t *testing.T) {
	var executed atomic.Bool
	wrapper := testWrapper(t, NewCommandFilter(nil, []string{`^grey$`}), nil,
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		}, nil)
	started := time.Now()
	_, err := wrapper(context.Background(), map[string]any{"command": "grey"})
	if err == nil || !strings.Contains(err.Error(), "Interaction 服务不可用") {
		t.Fatalf("nil service error = %v", err)
	}
	if executed.Load() {
		t.Fatal("nil Interaction 服务绝不能执行命令")
	}
	if time.Since(started) > time.Second {
		t.Fatal("nil Interaction 服务不应永久阻塞")
	}
}

func TestWrapShellTool_RequestProtocolAndAllowOnce(t *testing.T) {
	service := interaction.NewService(nil)
	var executed atomic.Bool
	var waiting atomic.Bool
	var innerSawWaiting atomic.Bool
	wrapper := testWrapper(t, NewCommandFilter(nil, []string{`^git push`}), service,
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			innerSawWaiting.Store(waiting.Load())
			return "executed", nil
		}, func(value bool) { waiting.Store(value) })
	ctx := agent.WithAgentContext(context.Background(), "worker-1", "task-123", 0)
	done := make(chan struct {
		out string
		err error
	}, 1)
	go func() {
		out, err := wrapper(ctx, map[string]any{
			"command": "git push origin main", "working_dir": "/repo",
		})
		done <- struct {
			out string
			err error
		}{out, err}
	}()

	request := waitPending(t, service, 1)[0]
	digest := shellCommandDigest("git push origin main", `^git push`, "/repo")
	if request.SessionID != "session-test" || request.Kind != interaction.KindAuthorization ||
		request.Purpose != PurposeShellCommand || request.Resolution.Handler != ResolutionHandlerShellCommand {
		t.Fatalf("请求基本协议错误: %+v", request)
	}
	if request.Origin.AgentID != "worker-1" || request.Origin.TaskID != "task-123" ||
		request.Subject.TaskID != "task-123" || request.Subject.Digest != digest {
		t.Fatalf("Origin/Subject = %+v / %+v", request.Origin, request.Subject)
	}
	if request.Metadata[MetadataCommand] != "git push origin main" ||
		request.Metadata[MetadataPattern] != `^git push` ||
		request.Metadata[MetadataWorkingDir] != "/repo" ||
		request.Metadata[MetadataDigest] != digest {
		t.Fatalf("Metadata = %+v", request.Metadata)
	}
	wantActions := []string{ActionAllowOnce, ActionDeny, ActionGuidance, ActionAllowSession}
	if len(request.Options) != len(wantActions) {
		t.Fatalf("Options = %+v", request.Options)
	}
	for index, want := range wantActions {
		if request.Options[index].ID != want || request.Options[index].ActionRef != want {
			t.Fatalf("option[%d] = %+v", index, request.Options[index])
		}
	}
	if !request.Options[2].RequiresText {
		t.Fatal("guidance 必须 RequiresText")
	}
	answerShellRequest(t, service, request, ActionAllowOnce, "")
	result := <-done
	if result.err != nil || result.out != "executed" || !executed.Load() {
		t.Fatalf("allow_once result = %q, %v, executed=%v", result.out, result.err, executed.Load())
	}
	if innerSawWaiting.Load() {
		t.Fatal("Await 返回后应先退出 waiting 状态，再执行 inner")
	}
}

func TestWrapShellTool_DenyAndGuidanceDoNotExecute(t *testing.T) {
	tests := []struct {
		name      string
		optionID  string
		text      string
		wantError string
	}{
		{name: "deny", optionID: ActionDeny, wantError: "拒绝"},
		{name: "guidance", optionID: ActionGuidance, text: "先运行 dry-run", wantError: "先运行 dry-run"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := interaction.NewService(nil)
			var executed atomic.Bool
			wrapper := testWrapper(t, NewCommandFilter(nil, []string{`^grey$`}), service,
				func(context.Context, map[string]any) (string, error) {
					executed.Store(true)
					return "", nil
				}, nil)
			done := make(chan error, 1)
			go func() {
				_, err := wrapper(context.Background(), map[string]any{"command": "grey"})
				done <- err
			}()
			request := waitPending(t, service, 1)[0]
			answerShellRequest(t, service, request, test.optionID, test.text)
			err := <-done
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v", err)
			}
			if executed.Load() {
				t.Fatal("deny/guidance 不得执行命令")
			}
		})
	}
}

func TestWrapShellTool_AllowSessionUsesCapturedPattern(t *testing.T) {
	service := interaction.NewService(nil)
	filter := NewCommandFilter(nil, []string{`^git push`})
	var calls atomic.Int32
	wrapper := testWrapper(t, filter, service,
		func(context.Context, map[string]any) (string, error) {
			calls.Add(1)
			return "ok", nil
		}, nil)

	done := make(chan error, 1)
	go func() {
		_, err := wrapper(context.Background(), map[string]any{"command": "git push origin main"})
		done <- err
	}()
	request := waitPending(t, service, 1)[0]
	answerShellRequest(t, service, request, ActionAllowSession, "")
	if err := <-done; err != nil {
		t.Fatalf("allow_session: %v", err)
	}
	if got := filter.RuntimeWhitelist(); len(got) != 1 || got[0] != `^git push` {
		t.Fatalf("只能加入创建时捕获的 pattern，got %v", got)
	}
	// 同一捕获模式的第二次调用直接执行，不再创建 Interaction。
	if _, err := wrapper(context.Background(), map[string]any{"command": "git push origin dev"}); err != nil {
		t.Fatalf("会话白名单未生效: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("inner calls = %d", calls.Load())
	}
	if pending := waitPendingCount(t, service); pending != 0 {
		t.Fatalf("第二次调用不应创建 Interaction，pending=%d", pending)
	}
}

func waitPendingCount(t *testing.T, service *interaction.Service) int {
	t.Helper()
	requests, err := service.ListPending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(requests)
}

func TestWrapShellTool_ContextCancelInterruptsAndBalancesHook(t *testing.T) {
	service := interaction.NewService(nil)
	var executed atomic.Bool
	var hookMu sync.Mutex
	var hookCalls []bool
	wrapper := testWrapper(t, NewCommandFilter(nil, []string{`^grey$`}), service,
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		}, func(waiting bool) {
			hookMu.Lock()
			hookCalls = append(hookCalls, waiting)
			hookMu.Unlock()
		})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := wrapper(ctx, map[string]any{"command": "grey"})
		done <- err
	}()
	request := waitPending(t, service, 1)[0]
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "取消") {
			t.Fatalf("cancel error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 wrapper 未返回")
	}
	if executed.Load() {
		t.Fatal("取消后不得执行命令")
	}
	current, err := service.Get(context.Background(), request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != interaction.StateInterrupted {
		t.Fatalf("取消后 Interaction state = %s, want interrupted", current.State)
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	if len(hookCalls) != 2 || !hookCalls[0] || hookCalls[1] {
		t.Fatalf("hook calls = %v, want [true false]", hookCalls)
	}
}

func TestWrapShellTool_InteractionCancellationFailsClosed(t *testing.T) {
	service := interaction.NewService(nil)
	var executed atomic.Bool
	wrapper := testWrapper(t, NewCommandFilter(nil, []string{`^grey$`}), service,
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		}, nil)
	done := make(chan error, 1)
	go func() {
		_, err := wrapper(context.Background(), map[string]any{"command": "grey"})
		done <- err
	}()
	request := waitPending(t, service, 1)[0]
	if _, err := service.Cancel(context.Background(), request.ID, request.Version, "来源任务已取消"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "已取消") {
			t.Fatalf("Interaction cancel error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Interaction 取消后 wrapper 未返回")
	}
	if executed.Load() {
		t.Fatal("Interaction 取消后不得执行命令")
	}
}

func TestWrapShellTool_ContextDeadlineDoesNotHangOrExecute(t *testing.T) {
	service := interaction.NewService(nil)
	var executed atomic.Bool
	wrapper := testWrapper(t, NewCommandFilter(nil, []string{`^grey$`}), service,
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := wrapper(ctx, map[string]any{"command": "grey"})
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "过期")) {
		t.Fatalf("deadline error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("超时路径发生永久阻塞")
	}
	if executed.Load() {
		t.Fatal("超时后不得执行命令")
	}
	requests, listErr := service.List(context.Background(), interaction.Filter{})
	if listErr != nil || len(requests) != 1 || !requests[0].State.IsTerminal() {
		t.Fatalf("超时后请求 = %+v, %v", requests, listErr)
	}
}

func TestWrapShellTool_ConcurrentRequestsRemainCorrelated(t *testing.T) {
	service := interaction.NewService(nil)
	filter := NewCommandFilter(nil, []string{`^grey-`})
	inner := func(_ context.Context, args map[string]any) (string, error) {
		return args["command"].(string), nil
	}
	wrapper := testWrapper(t, filter, service, inner, nil)
	type result struct {
		command string
		out     string
		err     error
	}
	results := make(chan result, 2)
	for _, command := range []string{"grey-allow", "grey-deny"} {
		go func(command string) {
			out, err := wrapper(context.Background(), map[string]any{"command": command})
			results <- result{command: command, out: out, err: err}
		}(command)
	}
	requests := waitPending(t, service, 2)
	if requests[0].ID == requests[1].ID {
		t.Fatal("并发命令必须创建不同 Interaction ID")
	}
	for _, request := range requests {
		option := ActionDeny
		if request.Metadata[MetadataCommand] == "grey-allow" {
			option = ActionAllowOnce
		}
		answerShellRequest(t, service, request, option, "")
	}
	for range 2 {
		result := <-results
		if result.command == "grey-allow" {
			if result.err != nil || result.out != result.command {
				t.Fatalf("allow result = %+v", result)
			}
		} else if result.err == nil || !strings.Contains(result.err.Error(), "拒绝") {
			t.Fatalf("deny result = %+v", result)
		}
	}
}
