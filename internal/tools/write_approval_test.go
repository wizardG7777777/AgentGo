package tools

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/modes"
	"agentgo/internal/shell"
)

func strictStore() *modes.Store {
	return modes.NewStore(modes.GateImmediate, modes.ExecStrict, modes.TopoTeam)
}

func waitFileWritePending(t *testing.T, service *interaction.Service, count int) []interaction.Request {
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
			t.Fatalf("等待 %d 条 file_write Interaction 超时，当前 %d", count, len(requests))
		}
		time.Sleep(time.Millisecond)
	}
}

func fileWritePendingCount(t *testing.T, service *interaction.Service) int {
	t.Helper()
	requests, err := service.ListPending(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(requests)
}

func answerFileWrite(t *testing.T, service *interaction.Service, request interaction.Request, optionID, text string) {
	t.Helper()
	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version,
		OptionID: optionID, Text: text, RespondedBy: "test-user",
	})
	if err != nil {
		t.Fatalf("BeginResolve(%s): %v", optionID, err)
	}
	if _, err := service.Complete(context.Background(), locked.ID, locked.Version); err != nil {
		t.Fatalf("Complete(%s): %v", optionID, err)
	}
}

// strict 下 write_file：Create 的协议字段逐项断言（Kind/Purpose/Subject/Digest/
// Prompt 含路径），allow_once 放行且 inner 恰好执行一次；第二次调用重新询问。
func TestFileWriteApprover_StrictCreateProtocolAndAllowOnce(t *testing.T) {
	service := interaction.NewService(nil)
	var calls atomic.Int32
	var waiting atomic.Bool
	approver := NewFileWriteApprover(strictStore(), service,
		func() string { return "session-test" }, "worker-1", func(value bool) { waiting.Store(value) })
	inner := agent.ToolFunc(func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		if waiting.Load() {
			t.Error("Await 返回后应先退出 waiting 状态，再执行 inner")
		}
		return "written", nil
	})
	wrapped := approver.WrapHandler("write_file")(inner)

	content := "第一行\n第二行\n第三行"
	ctx := agent.WithAgentContext(context.Background(), "worker-1", "task-9", 0)
	type outcome struct {
		out string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := wrapped(ctx, map[string]any{"path": "/repo/a.go", "content": content})
		done <- outcome{out, err}
	}()

	request := waitFileWritePending(t, service, 1)[0]
	payloadDigest := sha256Hex([]byte(content))
	digest := fileWriteDigest("write_file", "/repo/a.go", payloadDigest)
	if request.Kind != interaction.KindAuthorization || request.Purpose != PurposeFileWrite {
		t.Fatalf("Kind/Purpose = %s/%s", request.Kind, request.Purpose)
	}
	if request.SessionID != "session-test" {
		t.Fatalf("SessionID = %q", request.SessionID)
	}
	if !strings.Contains(request.Prompt, "write_file") || !strings.Contains(request.Prompt, "/repo/a.go") {
		t.Fatalf("Prompt 必须含工具名与路径: %q", request.Prompt)
	}
	if !strings.Contains(request.Prompt, "覆盖写入") || !strings.Contains(request.Prompt, "第一行") {
		t.Fatalf("Prompt 必须含操作摘要与内容预览: %q", request.Prompt)
	}
	if request.Subject.Kind != "file_write" || request.Subject.ID != "/repo/a.go" ||
		request.Subject.TaskID != "task-9" || request.Subject.Digest != digest {
		t.Fatalf("Subject = %+v", request.Subject)
	}
	if request.Resolution.Handler != ResolutionHandlerFileWrite ||
		request.Resolution.TargetID != digest ||
		request.Resolution.AgentID != "worker-1" || request.Resolution.TaskID != "task-9" {
		t.Fatalf("Resolution = %+v", request.Resolution)
	}
	if request.Origin.Component != "file_write" || request.Origin.AgentID != "worker-1" ||
		request.Origin.TaskID != "task-9" {
		t.Fatalf("Origin = %+v", request.Origin)
	}
	if request.Metadata[metadataFileTool] != "write_file" ||
		request.Metadata[metadataFilePath] != "/repo/a.go" ||
		request.Metadata[metadataFilePayloadDigest] != payloadDigest ||
		request.Metadata[metadataFileDigest] != digest {
		t.Fatalf("Metadata = %+v", request.Metadata)
	}
	wantOptions := []string{shell.ActionAllowOnce, shell.ActionDeny, shell.ActionGuidance, shell.ActionAllowSession}
	if len(request.Options) != len(wantOptions) {
		t.Fatalf("Options = %+v", request.Options)
	}
	for i, want := range wantOptions {
		if request.Options[i].ID != want || request.Options[i].ActionRef != want {
			t.Fatalf("option[%d] = %+v, want %s", i, request.Options[i], want)
		}
	}
	if !request.Options[2].RequiresText {
		t.Fatal("guidance 必须 RequiresText")
	}

	answerFileWrite(t, service, request, shell.ActionAllowOnce, "")
	res := <-done
	if res.err != nil || res.out != "written" || calls.Load() != 1 {
		t.Fatalf("allow_once result = %q, %v, calls=%d", res.out, res.err, calls.Load())
	}

	// allow_once 只放行一次：第二次同路径调用必须重新创建 Interaction。
	go func() {
		out, err := wrapped(ctx, map[string]any{"path": "/repo/a.go", "content": content})
		done <- outcome{out, err}
	}()
	request2 := waitFileWritePending(t, service, 1)[0]
	if request2.ID == request.ID {
		t.Fatal("第二次调用必须创建新的 Interaction")
	}
	answerFileWrite(t, service, request2, shell.ActionAllowOnce, "")
	res = <-done
	if res.err != nil || calls.Load() != 2 {
		t.Fatalf("第二次 allow_once result = %v, calls=%d", res.err, calls.Load())
	}
}

// deny / guidance 都不得执行 inner；guidance 把用户文本回灌给 LLM。
func TestFileWriteApprover_DenyAndGuidanceDoNotWrite(t *testing.T) {
	tests := []struct {
		name      string
		optionID  string
		text      string
		wantError string
	}{
		{name: "deny", optionID: shell.ActionDeny, wantError: "拒绝"},
		{name: "guidance", optionID: shell.ActionGuidance, text: "先写到临时目录", wantError: "先写到临时目录"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := interaction.NewService(nil)
			var executed atomic.Bool
			approver := NewFileWriteApprover(strictStore(), service, nil, "worker-1", nil)
			wrapped := approver.WrapHandler("write_file")(
				func(context.Context, map[string]any) (string, error) {
					executed.Store(true)
					return "", nil
				})
			done := make(chan error, 1)
			go func() {
				_, err := wrapped(context.Background(), map[string]any{"path": "/repo/a.go", "content": "x"})
				done <- err
			}()
			request := waitFileWritePending(t, service, 1)[0]
			answerFileWrite(t, service, request, test.optionID, test.text)
			err := <-done
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want 含 %q", err, test.wantError)
			}
			if executed.Load() {
				t.Fatal("deny/guidance 不得执行写入")
			}
		})
	}
}

// allow_session：同路径（含 ./ 归一）放行不再询问；异路径仍询问；
// 另一个 wrapper 实例不共享该记忆（并发多 wrapper 各自独立）。
func TestFileWriteApprover_AllowSessionExactPath(t *testing.T) {
	service := interaction.NewService(nil)
	var calls atomic.Int32
	approver := NewFileWriteApprover(strictStore(), service, nil, "worker-1", nil)
	inner := agent.ToolFunc(func(context.Context, map[string]any) (string, error) {
		calls.Add(1)
		return "written", nil
	})
	wrapped := approver.WrapHandler("write_file")(inner)

	done := make(chan error, 1)
	go func() {
		_, err := wrapped(context.Background(), map[string]any{"path": "/repo/a.go", "content": "x"})
		done <- err
	}()
	request := waitFileWritePending(t, service, 1)[0]
	answerFileWrite(t, service, request, shell.ActionAllowSession, "")
	if err := <-done; err != nil {
		t.Fatalf("allow_session: %v", err)
	}

	// 同路径第二次直接放行，不创建 Interaction。
	if _, err := wrapped(context.Background(), map[string]any{"path": "/repo/a.go", "content": "y"}); err != nil {
		t.Fatalf("allow_session 后同路径应直接放行: %v", err)
	}
	// 路径经 filepath.Clean 归一："./a.go" 与 "a.go" 视为同一路径。
	wrappedRel := approver.WrapHandler("edit_file")(inner)
	if _, err := wrappedRel(context.Background(),
		map[string]any{"path": "/repo/./a.go", "old_str": "x", "new_str": "y"}); err != nil {
		t.Fatalf("归一后的同一路径应直接放行: %v", err)
	}
	if calls.Load() != 3 || fileWritePendingCount(t, service) != 0 {
		t.Fatalf("calls=%d pending=%d", calls.Load(), fileWritePendingCount(t, service))
	}

	// 异路径仍询问。
	go func() {
		_, err := wrapped(context.Background(), map[string]any{"path": "/repo/b.go", "content": "z"})
		done <- err
	}()
	request2 := waitFileWritePending(t, service, 1)[0]
	answerFileWrite(t, service, request2, shell.ActionDeny, "")
	if err := <-done; err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("异路径应仍询问并被拒绝: %v", err)
	}

	// 另一个 wrapper 实例不共享放行记忆：同路径对它仍询问。
	other := NewFileWriteApprover(strictStore(), service, nil, "worker-2", nil)
	wrappedOther := other.WrapHandler("write_file")(inner)
	go func() {
		_, err := wrappedOther(context.Background(), map[string]any{"path": "/repo/a.go", "content": "w"})
		done <- err
	}()
	request3 := waitFileWritePending(t, service, 1)[0]
	answerFileWrite(t, service, request3, shell.ActionAllowOnce, "")
	if err := <-done; err != nil {
		t.Fatalf("独立 wrapper 的 allow_once: %v", err)
	}
}

// normal / yolo / readonly / nil store：全部透传，零 Create。
func TestFileWriteApprover_NonStrictPassthrough(t *testing.T) {
	stores := map[string]*modes.Store{
		"normal":   modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoTeam),
		"yolo":     modes.NewStore(modes.GateImmediate, modes.ExecYolo, modes.TopoTeam),
		"readonly": modes.NewStore(modes.GateImmediate, modes.ExecReadonly, modes.TopoTeam),
		"nil":      nil,
	}
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			service := interaction.NewService(nil)
			var calls atomic.Int32
			approver := NewFileWriteApprover(store, service, nil, "worker-1", nil)
			wrapped := approver.WrapHandler("write_file")(
				func(context.Context, map[string]any) (string, error) {
					calls.Add(1)
					return "written", nil
				})
			out, err := wrapped(context.Background(), map[string]any{"path": "/repo/a.go", "content": "x"})
			if err != nil || out != "written" || calls.Load() != 1 {
				t.Fatalf("透传 result = %q, %v, calls=%d", out, err, calls.Load())
			}
			if pending := fileWritePendingCount(t, service); pending != 0 {
				t.Fatalf("非 strict 不应创建 Interaction，pending=%d", pending)
			}
		})
	}
}

// strict + Interaction 服务不可用：fail-closed，inner 不得执行。
func TestFileWriteApprover_NilServiceFailsClosed(t *testing.T) {
	var executed atomic.Bool
	approver := NewFileWriteApprover(strictStore(), nil, nil, "worker-1", nil)
	wrapped := approver.WrapHandler("write_file")(
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		})
	started := time.Now()
	_, err := wrapped(context.Background(), map[string]any{"path": "/repo/a.go", "content": "x"})
	if err == nil || !strings.Contains(err.Error(), "Interaction 服务不可用") {
		t.Fatalf("nil service error = %v", err)
	}
	if executed.Load() {
		t.Fatal("nil Interaction 服务绝不能执行写入")
	}
	if time.Since(started) > time.Second {
		t.Fatal("nil Interaction 服务不应永久阻塞")
	}
}

// Await 返回绑定被篡改时拒绝执行。Service 的公开协议不允许改写绑定字段，
// 因此这里直接对复核函数做逐项篡改断言——wrapper 在执行 inner 前调用它。
func TestFileWriteApprover_BindingRecheck(t *testing.T) {
	payloadDigest := sha256Hex([]byte("content"))
	digest := fileWriteDigest("write_file", "/repo/a.go", payloadDigest)
	base := interaction.Request{
		State:   interaction.StateResolved,
		Purpose: PurposeFileWrite,
		Origin:  interaction.Origin{Component: "file_write", AgentID: "worker-1", TaskID: "task-9"},
		Subject: interaction.Subject{Kind: "file_write", ID: "/repo/a.go", TaskID: "task-9", Digest: digest},
		Resolution: interaction.ResolutionSpec{
			Handler: ResolutionHandlerFileWrite, TargetID: digest, AgentID: "worker-1", TaskID: "task-9",
		},
		Metadata: map[string]string{
			metadataFileTool: "write_file", metadataFilePath: "/repo/a.go",
			metadataFilePayloadDigest: payloadDigest, metadataFileDigest: digest,
		},
	}
	if !matchesFileWriteRequest(base, "write_file", "/repo/a.go", payloadDigest, digest, "worker-1", "task-9") {
		t.Fatal("合法绑定应通过复核")
	}

	tamper := func(name string, mutate func(*interaction.Request)) {
		t.Run(name, func(t *testing.T) {
			mutated := interaction.CloneRequest(base)
			mutate(&mutated)
			if matchesFileWriteRequest(mutated, "write_file", "/repo/a.go", payloadDigest, digest, "worker-1", "task-9") {
				t.Fatalf("%s 被篡改后仍通过复核", name)
			}
		})
	}
	tamper("state", func(r *interaction.Request) { r.State = interaction.StateResolving })
	tamper("purpose", func(r *interaction.Request) { r.Purpose = "shell_command" })
	tamper("handler", func(r *interaction.Request) { r.Resolution.Handler = "shell_command" })
	tamper("target", func(r *interaction.Request) { r.Resolution.TargetID = "other-digest" })
	tamper("agent", func(r *interaction.Request) { r.Resolution.AgentID = "worker-2" })
	tamper("task", func(r *interaction.Request) { r.Subject.TaskID = "task-10" })
	tamper("path", func(r *interaction.Request) { r.Subject.ID = "/repo/b.go" })
	tamper("digest", func(r *interaction.Request) { r.Subject.Digest = "other-digest" })
	tamper("metadata-path", func(r *interaction.Request) { r.Metadata[metadataFilePath] = "/repo/c.go" })
	tamper("metadata-payload", func(r *interaction.Request) { r.Metadata[metadataFilePayloadDigest] = "x" })
}

// ctx 取消：fail-closed，请求被 best-effort 收尾为 interrupted，waitHook 配平。
func TestFileWriteApprover_ContextCancelFailsClosed(t *testing.T) {
	service := interaction.NewService(nil)
	var executed atomic.Bool
	var hookMu sync.Mutex
	var hookCalls []bool
	approver := NewFileWriteApprover(strictStore(), service, nil, "worker-1", func(waiting bool) {
		hookMu.Lock()
		hookCalls = append(hookCalls, waiting)
		hookMu.Unlock()
	})
	wrapped := approver.WrapHandler("write_file")(
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := wrapped(ctx, map[string]any{"path": "/repo/a.go", "content": "x"})
		done <- err
	}()
	request := waitFileWritePending(t, service, 1)[0]
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
		t.Fatal("取消后不得执行写入")
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

// edit_file：Prompt 含 old→new 摘要，digest 绑定 old_str+NUL+new_str。
func TestFileWriteApprover_EditFilePromptAndDigest(t *testing.T) {
	service := interaction.NewService(nil)
	approver := NewFileWriteApprover(strictStore(), service, nil, "worker-1", nil)
	var executed atomic.Bool
	wrapped := approver.WrapHandler("edit_file")(
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		})
	done := make(chan error, 1)
	go func() {
		_, err := wrapped(context.Background(),
			map[string]any{"path": "/repo/a.go", "old_str": "foo", "new_str": "foobar"})
		done <- err
	}()
	request := waitFileWritePending(t, service, 1)[0]
	if !strings.Contains(request.Prompt, "单次替换") || !strings.Contains(request.Prompt, "- 旧: foo") ||
		!strings.Contains(request.Prompt, "+ 新: foobar") {
		t.Fatalf("edit_file Prompt = %q", request.Prompt)
	}
	wantDigest := fileWriteDigest("edit_file", "/repo/a.go", sha256Hex([]byte("foo\x00foobar")))
	if request.Subject.Digest != wantDigest {
		t.Fatalf("edit_file digest = %s, want %s", request.Subject.Digest, wantDigest)
	}
	answerFileWrite(t, service, request, shell.ActionDeny, "")
	if err := <-done; err == nil {
		t.Fatal("deny 应返回错误")
	}
	if executed.Load() {
		t.Fatal("deny 不得执行编辑")
	}
}

// 预览截断：超过行数/字符上限时追加截断提示，控制 Prompt 长度适合 6 行分页。
func TestPreviewSnippet_Truncates(t *testing.T) {
	long := "1\n2\n3\n4\n5\n6\n7"
	if got := previewSnippet(long); !strings.Contains(got, "已截断") || strings.Contains(got, "6") {
		t.Fatalf("行数截断失败: %q", got)
	}
	runes := strings.Repeat("字", fileWritePreviewMaxRunes+10)
	if got := previewSnippet(runes); !strings.Contains(got, "已截断") {
		t.Fatalf("字符截断失败: %q", got)
	}
	if got := previewSnippet("a\r\nb"); strings.Contains(got, "\r") {
		t.Fatalf("CRLF 应归一: %q", got)
	}
}
