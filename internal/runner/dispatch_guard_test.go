package runner

// dispatch_guard_test.go 覆盖 V6 C6b 的工具派发活性守卫
// （requireLiveToolDispatch，取代原 Plan 控制面租约校验）：
//   - 同一次 LLM 响应携带多个有序工具调用时，前面的调用同步触发任务取消，
//     后续工具必须在派发前被拦截（不得再产生副作用）；
//   - requireLiveToolDispatch 单元语义：ctx 取消 / 任务非 processing /
//     任务消失均拒绝，nil store 退化为仅 ctx 检查。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// orderedToolClient 按调用序返回预置响应（LLM fake），耗尽后恒返回
// 纯文本完成。submit_result_runner_test.go 与本文件共用。
type orderedToolClient struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
}

func (c *orderedToolClient) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.calls
	c.calls++
	if idx < len(c.responses) {
		return c.responses[idx], nil
	}
	return llm.Response{Content: "done", FinishReason: llm.FinishReasonStop}, nil
}

func (c *orderedToolClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// cancelAfterFirstTool 是 trace Dispatcher：观察到首个 write_file 的工具结果后
// 同步把任务迁到 cancelled（模拟 Scheduler 在同一响应窗口内取消任务），
// 并记录第二个工具的结果事件。
type cancelAfterFirstTool struct {
	store      store.TaskStore
	taskID     string
	once       sync.Once
	cancelErr  chan error
	secondSeen chan trace.Event
}

func (d *cancelAfterFirstTool) Dispatch(ev trace.Event) {
	if ev.TaskID != d.taskID || ev.Kind != trace.KindToolResult {
		return
	}
	if ev.CallID == "write-first" {
		d.once.Do(func() {
			d.cancelErr <- store.TransitionStateWithCancelSource(
				d.store, d.taskID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler",
			)
		})
	}
	if ev.CallID == "write-second" {
		select {
		case d.secondSeen <- ev:
		default:
		}
	}
}

// 同一响应内任务被取消后，后续工具调用必须被活性守卫拦截：第二个 write_file
// 不得落盘，工具结果事件携带中文中止原因。
func TestRunnerBlocksLaterToolWhenTaskCancelledInSameResponse(t *testing.T) {
	root := t.TempDir()
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	cancelRegistry := store.NewTaskCancelRegistry()
	taskStore.SetCancelRegistry(cancelRegistry)
	task := &model.Task{Description: "write two files", EventType: "code"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	client := &orderedToolClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "write-first", Name: "write_file", Arguments: map[string]any{"path": "first.txt", "content": "first"}},
			{ID: "write-second", Name: "write_file", Arguments: map[string]any{"path": "second.txt", "content": "second"}},
		},
		FinishReason: llm.FinishReasonToolCalls,
	}}}
	dispatcher := &cancelAfterFirstTool{
		store: taskStore, taskID: task.ID, cancelErr: make(chan error, 1), secondSeen: make(chan trace.Event, 1),
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(dispatcher)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-dispatch-cancel", Kind: "worker", EventType: "code",
		AllowedTools: []string{"write_file"}, TaskMaxRetries: 1,
	}, RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), LLMClient: client,
		CancelRegistry: cancelRegistry, ProjectRoot: root,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rn.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("runner did not stop")
		}
	})

	select {
	case err := <-dispatcher.cancelErr:
		if err != nil {
			t.Fatalf("cancel task after first tool: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first tool never reached the cancellation boundary")
	}
	select {
	case second := <-dispatcher.secondSeen:
		if second.Error == "" {
			t.Fatalf("second tool was not rejected: %+v", second)
		}
		if !strings.Contains(second.Error, "中止本轮工具派发") {
			t.Fatalf("second tool error 应含活性守卫中文原因，实际: %s", second.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second tool result was not emitted")
	}

	if got, err := os.ReadFile(filepath.Join(root, "first.txt")); err != nil || string(got) != "first" {
		t.Fatalf("first tool result: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "second.txt")); !os.IsNotExist(err) {
		t.Fatalf("second side effect ran after cancellation: err=%v", err)
	}
}

// requireLiveToolDispatch 单元语义：只有「ctx 未取消 且 任务仍 processing」
// 才放行；nil store / nil task 退化为仅 ctx 检查（单测直构兼容）。
func TestRequireLiveToolDispatch(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{Description: "live", EventType: "code"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}

	// 放行：processing + ctx 存活。
	if err := requireLiveToolDispatch(context.Background(), taskStore, task); err != nil {
		t.Fatalf("processing 任务应放行: %v", err)
	}
	// 放行：nil store / nil task 退化（仅 ctx 检查）。
	if err := requireLiveToolDispatch(context.Background(), nil, task); err != nil {
		t.Fatalf("nil store 应退化为放行: %v", err)
	}
	if err := requireLiveToolDispatch(context.Background(), taskStore, nil); err != nil {
		t.Fatalf("nil task 应退化为放行: %v", err)
	}

	// 拒绝：ctx 已取消。
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := requireLiveToolDispatch(cancelledCtx, taskStore, task); err == nil ||
		!strings.Contains(err.Error(), "任务执行上下文已取消") {
		t.Fatalf("ctx 取消应拒绝并含中文原因，实际: %v", err)
	}

	// 拒绝：任务已迁出 processing（终态）。
	if err := store.TransitionStateWithCancelSource(
		taskStore, task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler"); err != nil {
		t.Fatal(err)
	}
	if err := requireLiveToolDispatch(context.Background(), taskStore, task); err == nil ||
		!strings.Contains(err.Error(), "非 processing") {
		t.Fatalf("非 processing 任务应拒绝并含状态说明，实际: %v", err)
	}

	// 拒绝：任务已不存在（GetTask 返回 nil 或错误都必须拒绝）。
	ghost := &model.Task{ID: "ghost-task", Description: "ghost", EventType: "code"}
	err := requireLiveToolDispatch(context.Background(), taskStore, ghost)
	if err == nil {
		t.Fatal("不存在的任务应拒绝派发")
	}
	if !strings.Contains(err.Error(), "中止本轮工具派发") {
		t.Fatalf("错误应含「中止本轮工具派发」，实际: %v", err)
	}
}

// V6 §4 H1：执行租约已撤销（终态 / finalizing 被接受）后，任何工具
// dispatch 拒绝——与 finalizing fence 互补的防御层。
func TestRequireLiveToolDispatch_RevokedLeaseRejected(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{Description: "leased", EventType: "code"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	lease := &model.ExecutionLease{
		TaskID:        task.ID,
		Attempt:       1,
		BusinessTools: []string{"read_file"},
		ControlTools:  []string{"submit_task_result"},
	}
	lease.Digest = lease.ComputeDigest()
	if _, frozen, err := taskStore.FreezeTaskLease(task.ID, lease); err != nil || !frozen {
		t.Fatalf("FreezeTaskLease: frozen=%t err=%v", frozen, err)
	}

	// 租约未撤销时放行（任务仍 processing）。
	if err := requireLiveToolDispatch(context.Background(), taskStore, task); err != nil {
		t.Fatalf("租约未撤销应放行: %v", err)
	}
	// 撤销后拒绝，原因含「执行租约已撤销」。
	if _, newly, err := taskStore.RevokeTaskLease(task.ID); err != nil || !newly {
		t.Fatalf("RevokeTaskLease: newly=%t err=%v", newly, err)
	}
	err := requireLiveToolDispatch(context.Background(), taskStore, task)
	if err == nil || !strings.Contains(err.Error(), "执行租约已撤销") {
		t.Fatalf("租约撤销后应拒绝 dispatch 并含中文原因，实际: %v", err)
	}
}
