package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

type mockLLM struct{}

func (m *mockLLM) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	return llm.Response{Content: "ok"}, nil
}

// fakeBundle 构造一个最小化的 *scheduler.Bundle 给 CLI 测试使用。
// CLI 只需要 Bundle.Mode（用于 /mode 切换），其他字段为 nil 即可。
func fakeBundle() *scheduler.Bundle {
	return &scheduler.Bundle{
		Mode: scheduler.NewModeStore(),
	}
}

func setup() (store.TaskStore, *scheduler.Bundle, chan model.Event) {
	ch := make(chan model.Event, 64)
	_ = config.DefaultConfig() // 保留 config 引入避免 lint 警告
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	sched := fakeBundle()
	return s, sched, ch
}

func TestCLI_QuitCommand(t *testing.T) {
	s, sched, ch := setup()
	input := strings.NewReader("/quit\n")
	output := &bytes.Buffer{}

	cancelled := false
	cancelFn := func() { cancelled = true }

	c := New(s, ch, cancelFn, sched, nil, nil, input, output)
	c.Run(context.Background())

	if !cancelled {
		t.Error("cancelFn should have been called on /quit")
	}
	if !strings.Contains(output.String(), "正在关闭") {
		t.Errorf("output should contain quit message, got: %s", output.String())
	}
}

func TestCLI_StatusCommand_NoTasks(t *testing.T) {
	s, sched, ch := setup()
	input := strings.NewReader("/status\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	if !strings.Contains(output.String(), "无活跃任务") {
		t.Errorf("output should show no active tasks, got: %s", output.String())
	}
}

func TestCLI_StatusCommand_WithTasks(t *testing.T) {
	s, sched, ch := setup()

	task := &model.Task{Description: "测试任务", EventType: "code"}
	s.PublishTask(task)

	input := strings.NewReader("/status\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	if !strings.Contains(output.String(), "pending") {
		t.Errorf("output should show pending task, got: %s", output.String())
	}
}

func TestCLI_CancelCommand(t *testing.T) {
	s, sched, ch := setup()

	task := &model.Task{Description: "待取消任务"}
	s.PublishTask(task)

	input := strings.NewReader("/cancel " + task.ID + "\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCancelled {
		t.Errorf("task status = %s, want cancelled", got.Status)
	}
}

func TestCLI_ModeToggle(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("/mode\n/mode\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	out := output.String()
	if !strings.Contains(out, "计划模式") {
		t.Errorf("first /mode should switch to plan mode, got: %s", out)
	}
	if !strings.Contains(out, "即时模式") {
		t.Errorf("second /mode should switch back to immediate mode, got: %s", out)
	}
}

func TestCLI_FreeText_SendsEvent(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("分析 auth 模块\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	go c.Run(ctx)

	// 读取 eventCh，验证收到用户输入事件
	select {
	case evt := <-ch:
		if evt.Type != model.EventUserInput {
			t.Errorf("event type = %s, want user_input", evt.Type)
		}
		if evt.Payload["text"] != "分析 auth 模块" {
			t.Errorf("event text = %q, want %q", evt.Payload["text"], "分析 auth 模块")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestCLI_EmptyLine_Ignored(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("\n\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	// channel 应该是空的（空行不发事件）
	select {
	case evt := <-ch:
		t.Errorf("unexpected event: %v", evt)
	default:
		// 预期行为
	}
}

func TestCLI_UnknownCommand(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("/unknown\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	if !strings.Contains(output.String(), "未知命令") {
		t.Errorf("output should show unknown command error, got: %s", output.String())
	}
}

func TestCLI_HelpCommand(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("/help\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	out := output.String()
	if !strings.Contains(out, "/status") || !strings.Contains(out, "/quit") {
		t.Errorf("help output should list commands, got: %s", out)
	}
}

func TestCLI_HelpCommand_IncludesSteer(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("/help\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, input, output)
	c.Run(context.Background())

	if !strings.Contains(output.String(), "/steer") {
		t.Errorf("help output should include /steer, got: %s", output.String())
	}
}

func TestCLI_SteerCommand_SendsUserMessage(t *testing.T) {
	s, sched, ch := setup()
	reg := mailbox.NewRegistry(4)
	mb := reg.Register("worker-1", "")

	output := &bytes.Buffer{}
	c := New(s, ch, func() {}, sched, reg, nil, strings.NewReader(""), output)

	c.handleLine("/steer worker-1 请改用 JSON 格式")

	msgs := mb.Drain()
	if len(msgs) != 1 {
		t.Fatalf("worker-1 mailbox message count = %d, want 1", len(msgs))
	}
	if msgs[0].From != "user" {
		t.Errorf("message.From = %q, want %q", msgs[0].From, "user")
	}
	if msgs[0].To != "worker-1" {
		t.Errorf("message.To = %q, want %q", msgs[0].To, "worker-1")
	}
	if msgs[0].Content != "请改用 JSON 格式" {
		t.Errorf("message.Content = %q, want %q", msgs[0].Content, "请改用 JSON 格式")
	}
	if msgs[0].SentAt.IsZero() {
		t.Error("message.SentAt should be set")
	}
	if !strings.Contains(output.String(), "[steer] 已向 worker-1 发送用户消息") {
		t.Errorf("output should contain steer success message, got: %s", output.String())
	}
}

func TestCLI_SteerCommand_InvalidUsage(t *testing.T) {
	s, sched, ch := setup()
	reg := mailbox.NewRegistry(4)
	reg.Register("worker-1", "")

	output := &bytes.Buffer{}
	c := New(s, ch, func() {}, sched, reg, nil, strings.NewReader(""), output)

	c.handleLine("/steer worker-1")

	if !strings.Contains(output.String(), "用法: /steer <agentID> <消息内容>") {
		t.Errorf("output should contain usage message, got: %s", output.String())
	}
}

func TestCLI_SteerCommand_MailboxDisabled(t *testing.T) {
	s, sched, ch := setup()
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, nil, nil, strings.NewReader(""), output)
	c.handleLine("/steer worker-1 hello")

	if !strings.Contains(output.String(), "邮箱系统未启用") {
		t.Errorf("output should contain mailbox disabled message, got: %s", output.String())
	}
}

func TestCLI_EOF_TriggersShutdown(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("") // 空输入 → 立即 EOF
	output := &bytes.Buffer{}

	cancelled := false
	cancelFn := func() { cancelled = true }

	c := New(s, ch, cancelFn, sched, nil, nil, input, output)
	c.Run(context.Background())

	if !cancelled {
		t.Error("cancelFn should have been called on EOF")
	}
}

func TestCLI_Approval_Granted(t *testing.T) {
	s, sched, ch := setup()
	approvalCh := make(chan shell.ApprovalRequest, 1)
	output := &bytes.Buffer{}

	pr, pw := io.Pipe()
	defer pw.Close()

	c := New(s, ch, func() {}, sched, nil, approvalCh, pr, output)

	replyCh := make(chan shell.ApprovalReply, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Send approval request after Run starts, so it lands in the select loop
	time.Sleep(50 * time.Millisecond)
	approvalCh <- shell.ApprovalRequest{
		AgentID: "worker-1",
		Command: "git push origin main",
		ReplyCh: replyCh,
	}

	// Wait for handleApproval to be reading from lineCh, then write the answer
	time.Sleep(50 * time.Millisecond)
	pw.Write([]byte("y\n"))

	select {
	case reply := <-replyCh:
		if !reply.Approved {
			t.Error("expected Approved=true, got false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for approval reply")
	}

	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(output.String(), "已放行") {
		t.Errorf("output should contain '已放行', got: %s", output.String())
	}

	pw.Write([]byte("/quit\n"))
}

func TestCLI_Approval_Denied(t *testing.T) {
	s, sched, ch := setup()
	approvalCh := make(chan shell.ApprovalRequest, 1)
	output := &bytes.Buffer{}

	pr, pw := io.Pipe()
	defer pw.Close()

	c := New(s, ch, func() {}, sched, nil, approvalCh, pr, output)

	replyCh := make(chan shell.ApprovalReply, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	approvalCh <- shell.ApprovalRequest{
		AgentID: "worker-2",
		Command: "chmod 777 /tmp/secret",
		ReplyCh: replyCh,
	}

	time.Sleep(50 * time.Millisecond)
	pw.Write([]byte("n\n"))

	select {
	case reply := <-replyCh:
		if reply.Approved {
			t.Error("expected Approved=false, got true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for approval reply")
	}

	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(output.String(), "已拒绝") {
		t.Errorf("output should contain '已拒绝', got: %s", output.String())
	}

	pw.Write([]byte("/quit\n"))
}

func TestCLI_Approval_UserGuidance(t *testing.T) {
	s, sched, ch := setup()
	approvalCh := make(chan shell.ApprovalRequest, 1)
	output := &bytes.Buffer{}

	pr, pw := io.Pipe()
	defer pw.Close()

	c := New(s, ch, func() {}, sched, nil, approvalCh, pr, output)

	replyCh := make(chan shell.ApprovalReply, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	approvalCh <- shell.ApprovalRequest{
		AgentID: "worker-3",
		Command: "git push origin main",
		ReplyCh: replyCh,
	}

	time.Sleep(50 * time.Millisecond)
	pw.Write([]byte("请改用 dry-run\n"))

	select {
	case reply := <-replyCh:
		if reply.Approved {
			t.Error("expected Approved=false, got true")
		}
		if reply.Message != "请改用 dry-run" {
			t.Errorf("expected Message=%q, got %q", "请改用 dry-run", reply.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for approval reply")
	}

	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(output.String(), "已将指导发送给") {
		t.Errorf("output should contain '已将指导发送给', got: %s", output.String())
	}

	pw.Write([]byte("/quit\n"))
}

func TestCLI_Approval_ContextCancel(t *testing.T) {
	s, sched, ch := setup()
	approvalCh := make(chan shell.ApprovalRequest, 1)
	output := &bytes.Buffer{}

	// Use a pipe so stdin blocks forever (no input available)
	pr, pw := io.Pipe()
	defer pw.Close()

	c := New(s, ch, func() {}, sched, nil, approvalCh, pr, output)

	replyCh := make(chan shell.ApprovalReply, 1)
	approvalCh <- shell.ApprovalRequest{
		AgentID: "worker-4",
		Command: "git reset --hard",
		ReplyCh: replyCh,
	}

	ctx, cancel := context.WithCancel(context.Background())

	go c.Run(ctx)

	// Give Run time to start and pick up the approval request
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case reply := <-replyCh:
		if reply.Approved {
			t.Error("expected Approved=false on context cancel, got true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for approval reply after context cancel")
	}
}

func TestCLI_Approval_Multiple_Queued(t *testing.T) {
	s, sched, ch := setup()
	approvalCh := make(chan shell.ApprovalRequest, 2)
	output := &bytes.Buffer{}

	pr, pw := io.Pipe()
	defer pw.Close()

	c := New(s, ch, func() {}, sched, nil, approvalCh, pr, output)

	replyCh1 := make(chan shell.ApprovalReply, 1)
	replyCh2 := make(chan shell.ApprovalReply, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Run(ctx)

	// Send first approval request
	time.Sleep(50 * time.Millisecond)
	approvalCh <- shell.ApprovalRequest{
		AgentID: "worker-a",
		Command: "git push",
		ReplyCh: replyCh1,
	}

	time.Sleep(50 * time.Millisecond)
	pw.Write([]byte("y\n"))

	select {
	case reply := <-replyCh1:
		if !reply.Approved {
			t.Error("first request: expected Approved=true, got false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first approval reply")
	}

	// Send second approval request
	time.Sleep(50 * time.Millisecond)
	approvalCh <- shell.ApprovalRequest{
		AgentID: "worker-b",
		Command: "chmod 755 deploy.sh",
		ReplyCh: replyCh2,
	}

	time.Sleep(50 * time.Millisecond)
	pw.Write([]byte("n\n"))

	select {
	case reply := <-replyCh2:
		if reply.Approved {
			t.Error("second request: expected Approved=false, got true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for second approval reply")
	}

	pw.Write([]byte("/quit\n"))
}
