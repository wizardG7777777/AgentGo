package worker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/cli"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

type approvalE2ESchedulerLLM struct{}

func (m *approvalE2ESchedulerLLM) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	return llm.Response{Content: "ok"}, nil
}

type approvalE2EWorkerLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	callIndex int
}

func (m *approvalE2EWorkerLLM) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}

type safeWriteBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *safeWriteBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *safeWriteBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func waitForTaskCompleted(t *testing.T, s store.TaskStore, taskID string, timeout time.Duration) *model.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := s.GetTask(taskID)
		if err == nil && task.Status == model.TaskStatusCompleted {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := s.GetTask(taskID)
	t.Fatalf("task %s not completed in time, last status=%v", taskID, task.Status)
	return nil
}

func waitOutputContains(t *testing.T, out *safeWriteBuffer, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output did not contain %q in time; output=%s", needle, out.String())
}

func runShellApprovalCase(t *testing.T, command string, userInput *string, cancelCLIOnPrompt bool) (partialOutput string, cliOutput string) {
	t.Helper()

	eventCh := make(chan model.Event, 64)
	cfg := config.DefaultConfig()
	cfg.SchedulerTickerSec = 100
	cfg.ProjectRoot = t.TempDir()
	cfg.ShellTimeoutSec = 10

	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	r := roster.NewMemoryRoster()
	approvalCh := make(chan shell.ApprovalRequest, 8)

	workerLLM := &approvalE2EWorkerLLM{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Name: "run_shell",
						Arguments: map[string]any{
							"command": command,
						},
					},
				},
			},
			{Content: "done"},
		},
	}
	w := NewWithID("worker-1", s, r, workerLLM, cfg, nil, nil, approvalCh, nil, nil, nil)
	w.agent.PollInterval = 10 * time.Millisecond
	w.agent.IdleThreshold = 0

	// Phase 3: scheduler.New 签名扩展，CLI 此 e2e 测试只用 mode store，所以传 nil 桥接组件
	sched := scheduler.New(s, r, &approvalE2ESchedulerLLM{}, eventCh, cfg, nil, nil, nil, nil, nil, nil)

	pr, pw := io.Pipe()
	out := &safeWriteBuffer{}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	cliCtx, cancelCLI := context.WithCancel(context.Background())

	c := cli.New(s, eventCh, cancelWorker, sched, nil, approvalCh, pr, out)

	workerDone := make(chan struct{})
	cliDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		w.Run(workerCtx)
	}()
	go func() {
		defer close(cliDone)
		c.Run(cliCtx)
	}()

	task := &model.Task{Description: "run shell e2e", EventType: ""}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask failed: %v", err)
	}

	if userInput != nil || cancelCLIOnPrompt {
		waitOutputContains(t, out, "命令审批请求", 2*time.Second)
		if cancelCLIOnPrompt {
			cancelCLI()
		} else {
			if _, err := pw.Write([]byte(*userInput + "\n")); err != nil {
				t.Fatalf("write user input failed: %v", err)
			}
		}
	}

	doneTask := waitForTaskCompleted(t, s, task.ID, 3*time.Second)

	cancelCLI()
	cancelWorker()
	_ = pw.Close()

	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
	select {
	case <-cliDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cli did not stop")
	}

	return doneTask.PartialOutput, out.String()
}

func TestShellApprovalE2E_GreylistApproved(t *testing.T) {
	answer := "y"
	partial, output := runShellApprovalCase(t, "git push origin main", &answer, false)

	if !strings.Contains(output, "已放行") {
		t.Fatalf("CLI output should contain approval granted message, got: %s", output)
	}
	if !strings.Contains(partial, "[run_shell] exit_code:") {
		t.Fatalf("partial output should contain executed run_shell result, got: %s", partial)
	}
}

func TestShellApprovalE2E_GreylistDenied(t *testing.T) {
	answer := "n"
	partial, output := runShellApprovalCase(t, "git push origin main", &answer, false)

	if !strings.Contains(output, "已拒绝") {
		t.Fatalf("CLI output should contain deny message, got: %s", output)
	}
	if !strings.Contains(partial, "命令被用户拒绝") {
		t.Fatalf("partial output should contain user denied error, got: %s", partial)
	}
	if strings.Contains(partial, "[run_shell] exit_code:") {
		t.Fatalf("denied path should not execute shell command, got: %s", partial)
	}
}

func TestShellApprovalE2E_GreylistUserGuidance(t *testing.T) {
	answer := "请先 dry-run 再继续"
	partial, output := runShellApprovalCase(t, "git push origin main", &answer, false)

	if !strings.Contains(output, "已将指导发送给") {
		t.Fatalf("CLI output should contain guidance message, got: %s", output)
	}
	if !strings.Contains(partial, "用户指导: 请先 dry-run 再继续") {
		t.Fatalf("partial output should contain user guidance, got: %s", partial)
	}
}

func TestShellApprovalE2E_BlacklistBlockedWithoutApproval(t *testing.T) {
	partial, output := runShellApprovalCase(t, "rm -rf /", nil, false)

	if strings.Contains(output, "命令审批请求") {
		t.Fatalf("blacklist command should not enter approval flow, output: %s", output)
	}
	if !strings.Contains(partial, "黑名单") {
		t.Fatalf("partial output should contain blacklist blocked error, got: %s", partial)
	}
}

func TestShellApprovalE2E_SafeCommandBypassesApproval(t *testing.T) {
	partial, output := runShellApprovalCase(t, "echo hello", nil, false)

	if strings.Contains(output, "命令审批请求") {
		t.Fatalf("safe command should bypass approval, output: %s", output)
	}
	if !strings.Contains(partial, "[run_shell] exit_code: 0") {
		t.Fatalf("safe command should execute directly, got: %s", partial)
	}
}

func TestShellApprovalE2E_CLICancelDuringApproval(t *testing.T) {
	partial, _ := runShellApprovalCase(t, "git push origin main", nil, true)

	if !(strings.Contains(partial, "命令被用户拒绝") || strings.Contains(partial, "命令审批超时")) {
		t.Fatalf("cancel during approval should produce denied/timeout path, got: %s", partial)
	}
}
