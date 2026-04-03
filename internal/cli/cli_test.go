package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

type mockLLM struct{}

func (m *mockLLM) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	return llm.Response{Content: "ok"}, nil
}

func setup() (store.TaskStore, *scheduler.Scheduler, chan model.Event) {
	ch := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.SchedulerTickerSec = 100 // 避免 ticker 干扰
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	sched := scheduler.New(s, &mockLLM{}, ch, cfg)
	return s, sched, ch
}

func TestCLI_QuitCommand(t *testing.T) {
	s, sched, ch := setup()
	input := strings.NewReader("/quit\n")
	output := &bytes.Buffer{}

	cancelled := false
	cancelFn := func() { cancelled = true }

	c := New(s, ch, cancelFn, sched, input, output)
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

	c := New(s, ch, func() {}, sched, input, output)
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

	c := New(s, ch, func() {}, sched, input, output)
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

	c := New(s, ch, func() {}, sched, input, output)
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

	c := New(s, ch, func() {}, sched, input, output)
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

	c := New(s, ch, func() {}, sched, input, output)

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

	c := New(s, ch, func() {}, sched, input, output)
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

	c := New(s, ch, func() {}, sched, input, output)
	c.Run(context.Background())

	if !strings.Contains(output.String(), "未知命令") {
		t.Errorf("output should show unknown command error, got: %s", output.String())
	}
}

func TestCLI_HelpCommand(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("/help\n/quit\n")
	output := &bytes.Buffer{}

	c := New(s, ch, func() {}, sched, input, output)
	c.Run(context.Background())

	out := output.String()
	if !strings.Contains(out, "/status") || !strings.Contains(out, "/quit") {
		t.Errorf("help output should list commands, got: %s", out)
	}
}

func TestCLI_EOF_TriggersShutdown(t *testing.T) {
	s, sched, ch := setup()

	input := strings.NewReader("") // 空输入 → 立即 EOF
	output := &bytes.Buffer{}

	cancelled := false
	cancelFn := func() { cancelled = true }

	c := New(s, ch, cancelFn, sched, input, output)
	c.Run(context.Background())

	if !cancelled {
		t.Error("cancelFn should have been called on EOF")
	}
}
