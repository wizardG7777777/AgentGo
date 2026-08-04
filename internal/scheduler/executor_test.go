package scheduler

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/probe"
	"agentgo/internal/store"
)

// makeInnerExecutor 返回一个 mock TaskExecutor，记录每次调用的 history 长度
// 并返回固定结果。
func makeInnerExecutor(callCount *int32, capturedHistory *[]agent.HistoryEntry) agent.TaskExecutor {
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []agent.HistoryEntry) (agent.ExecuteResult, error) {
		atomic.AddInt32(callCount, 1)
		// 拷贝防止 caller 修改
		hCopy := make([]agent.HistoryEntry, len(history))
		copy(hCopy, history)
		*capturedHistory = hCopy
		return agent.ExecuteResult{
			Output:     "ok",
			ToolCalled: false,
		}, nil
	}
}

// TestSchedulerExecutorBlocksLaterToolWhenControllerCancelledInSameResponse 验证
// ToolDispatchGuard 的活性检查：同一 LLM 响应中，前一个工具把 controller 任务
// 取消后，后一个工具的派发被 guard 拒绝（ctx 未取消 + 任务仍 processing 才放行）。
func TestSchedulerExecutorBlocksLaterToolWhenControllerCancelledInSameResponse(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	root := &model.Task{Description: "scheduler controller", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}

	var secondSideEffect int32
	toolReg := agent.NewToolRegistry()
	toolReg.Register("cancel_controller", "cancel current controller", nil, func(context.Context, map[string]any) (string, error) {
		err := store.TransitionStateWithCancelSource(
			taskStore, root.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler",
		)
		return "cancelled", err
	})
	toolReg.Register("second_side_effect", "must be blocked", nil, func(context.Context, map[string]any) (string, error) {
		atomic.AddInt32(&secondSideEffect, 1)
		return "ran", nil
	})
	client := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "cancel-first", Name: "cancel_controller", Arguments: map[string]any{}},
			{ID: "side-effect-second", Name: "second_side_effect", Arguments: map[string]any{}},
		},
		FinishReason: llm.FinishReasonToolCalls,
	}}}
	exec := &SchedulerExecutor{
		Inner: agent.NewLLMExecutor(client, toolReg, nil, taskStore, nil, ""),
		Store: taskStore, Cfg: config.DefaultConfig(),
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exec.requireToolDispatch(cancelledCtx, root); err == nil ||
		!strings.Contains(err.Error(), "context is no longer active") {
		t.Fatalf("cancelled dispatch context err=%v, want liveness rejection", err)
	}

	result, err := exec.Execute(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&secondSideEffect) != 0 {
		t.Fatal("second Scheduler side effect ran after the controller Task was cancelled")
	}
	if len(result.ToolResults) != 2 || !strings.Contains(result.ToolResults[1].Content, "not processing") {
		t.Fatalf("second tool did not receive durable controller lease rejection: %+v", result.ToolResults)
	}
}

// TestSchedulerExecutor_LegacyDownstreamWaitUnblocksOnTerminal 验证 legacy 路径的
// 下游等待机制（waitForDownstreamTasks + BatchUpdateCh）：已汇报过进度后，依赖
// SchedulerBatch 的下游任务（reactor 触发的 verifier 等）未终态时 Execute 阻塞
// 等待，下游到达终态并收到 BatchUpdateCh 信号后放行进入 Inner。
func TestSchedulerExecutor_LegacyDownstreamWaitUnblocksOnTerminal(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 8), 10, 1, 60)
	schedTask := &model.Task{Description: "legacy scheduler", EventType: "__scheduler__"}
	if err := s.PublishTask(schedTask); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler", schedTask.ID); err != nil {
		t.Fatal(err)
	}

	batchTask := &model.Task{Description: "completed batch member"}
	if err := s.PublishTask(batchTask); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker", batchTask.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult("worker", batchTask.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSchedulerBatch(schedTask.ID, batchTask.ID); err != nil {
		t.Fatal(err)
	}
	downstream := &model.Task{Description: "legacy downstream", Dependencies: []string{batchTask.ID}}
	if err := s.PublishTask(downstream); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("verifier", downstream.ID); err != nil {
		t.Fatal(err)
	}

	batchCh := make(chan struct{}, 1)
	var innerCalls int32
	exec := &SchedulerExecutor{
		Store: s, Cfg: &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}},
		BatchUpdateCh:         batchCh,
		WaitTimeout:           50 * time.Millisecond,
		DownstreamWaitTimeout: 2 * time.Second,
		Inner: func(context.Context, *model.Task, map[string]string, []agent.HistoryEntry) (agent.ExecuteResult, error) {
			atomic.AddInt32(&innerCalls, 1)
			return agent.ExecuteResult{Output: "downstream done", ToolCalled: true}, nil
		},
		lastTaskID:       schedTask.ID,
		progressReported: true,
	}

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 下游任务未终态时 Execute 应阻塞在下游等待，Inner 不被调用
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&innerCalls); got != 0 {
		t.Fatalf("downstream pending 时 Inner 被调用 %d 次，want 0", got)
	}

	// 下游到达终态 + BatchUpdateCh 信号后放行
	if err := s.SubmitResult("verifier", downstream.ID, "verified"); err != nil {
		t.Fatal(err)
	}
	batchCh <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("下游终态后 Execute 未放行")
	}
	if got := atomic.LoadInt32(&innerCalls); got != 1 {
		t.Fatalf("Inner calls=%d, want 1", got)
	}
}

func TestSchedulerExecutor_NoBatch_DirectExecute(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	result, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Output != "ok" {
		t.Errorf("Output %q, want ok", result.Output)
	}
	if calls != 1 {
		t.Errorf("Inner called %d times, want 1", calls)
	}
}

func TestSchedulerExecutor_InjectsBoardSnapshotIntoHistory(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// history 应当有 1 条 IncomingMail entry，包含 board snapshot JSON
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if mail == "" {
		t.Fatal("IncomingMail should be non-empty")
	}
	if !strings.Contains(mail, `"worker_count": 2`) {
		t.Errorf("snapshot should contain worker_count, got: %s", mail)
	}
	if !strings.Contains(mail, `"exec_mode": "normal"`) {
		t.Errorf("snapshot should contain exec_mode=normal, got: %s", mail)
	}
}

// TestSchedulerExecutor_ModesStoreLiveSwitch 验证 SchedulerExecutor 每次 Execute
// 重读两轴 store：运行期切换 exec / topo 轴后，下一次快照立即反映新值。
func TestSchedulerExecutor_ModesStoreLiveSwitch(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	modeStore := modes.DefaultStore()
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
		Modes:         modeStore,
	}

	// 运行期切换两轴
	modeStore.SetExec(modes.ExecStrict)
	modeStore.SetTopo(modes.TopoSolo)

	if _, err := exec.Execute(context.Background(), schedTask, nil, nil); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	for _, want := range []string{`"exec_mode": "strict"`, `"topo_mode": "solo"`} {
		if !strings.Contains(mail, want) {
			t.Errorf("快照缺少 %s，got: %s", want, mail)
		}
	}
	if strings.Contains(mail, `"mode":`) {
		t.Errorf("快照不应再含旧 \"mode\" 字段，got: %s", mail)
	}
}

func TestSchedulerExecutor_BatchPending_WaitsUntilComplete(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	// scheduler 自身 task
	schedTask := &model.Task{Description: "sched", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// 一个 processing 子任务
	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	batchCh := make(chan struct{}, 1)
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   2 * time.Second,
	}

	// 开一个 goroutine 调 Execute；它应当阻塞在等待 batch
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 50ms 后 Inner 不应被调用（仍在等）
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("Inner should not be called while batch pending, got %d calls", calls)
	}

	// 把 child 标记为完成 + broadcast
	s.SubmitResult("worker-1", child.ID, "done")
	batchCh <- struct{}{}

	// 现在 Execute 应当解锁并返回
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not unblock after batch completion")
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("Inner should be called exactly once after wait, got %d", calls)
	}
}

func TestSchedulerExecutor_BatchUpdateChannelWakesWait(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	batchCh := make(chan struct{}, 1)
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   10 * time.Second, // 长 timeout，确保是 channel 唤醒不是兜底
	}

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 等一下让 goroutine 进入 wait
	time.Sleep(50 * time.Millisecond)

	// 完成 child 并通过 channel 唤醒
	s.SubmitResult("worker-1", child.ID, "done")
	batchCh <- struct{}{}

	select {
	case <-done:
		// 应当在 100ms 内完成（远小于 10s timeout）
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not unblock via channel signal")
	}
}

func TestSchedulerExecutor_TimeoutFallback(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	// 不发 batchCh 信号，依靠 timeout 兜底
	batchCh := make(chan struct{})
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   100 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil)
		done <- err
	}()

	// 200ms 时让 child 完成（依靠 timeout 触发的下一次 check 应当看到）
	time.Sleep(150 * time.Millisecond)
	s.SubmitResult("worker-1", child.ID, "done")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Execute did not unblock via timeout fallback")
	}
}

func TestSchedulerExecutor_ContextCancellation(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)

	batchCh := make(chan struct{})
	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: batchCh,
		WaitTimeout:   10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, schedTask, nil, nil)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected context cancellation error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return after context cancel")
	}
}

func TestSchedulerExecutor_BatchAllTerminalSkipsWait(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// batch 中所有任务都已 completed
	c1 := &model.Task{Description: "c1"}
	s.PublishTask(c1)
	s.ClaimTask("worker-1", c1.ID)
	s.SubmitResult("worker-1", c1.ID, "done")
	s.AppendSchedulerBatch(schedTask.ID, c1.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
	}

	start := time.Now()
	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("Execute took %v with all-terminal batch, should be near-instant", elapsed)
	}
	if calls != 1 {
		t.Errorf("Inner called %d times, want 1", calls)
	}
}

// ---- filterNonTerminalChildren ----

func TestFilterNonTerminalChildren(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)

	pendingTask := &model.Task{Description: "pending"}
	s.PublishTask(pendingTask)

	completedTask := &model.Task{Description: "done"}
	s.PublishTask(completedTask)
	s.ClaimTask("w", completedTask.ID)
	s.SubmitResult("w", completedTask.ID, "ok")

	failedTask := &model.Task{Description: "fail"}
	s.PublishTask(failedTask)
	s.ClaimTask("w", failedTask.ID)
	s.FailTask("w", failedTask.ID, "boom")

	pending := filterNonTerminalChildren(s, []string{
		pendingTask.ID,
		completedTask.ID,
		failedTask.ID,
		"nonexistent",
	})

	if len(pending) != 1 || pending[0] != pendingTask.ID {
		t.Errorf("expected only pending task, got %v", pending)
	}
}

// ---- ToolHealth 传递到 board snapshot ----

func TestSchedulerExecutor_ToolHealth_PassedToSnapshot(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	// 创建一个 ToolHealthStatus，其中 web_search 不可用
	th := probe.NewToolHealthStatus()
	th.Record(probe.ProbeResult{
		Tool:      "web_search",
		Available: false,
		Error:     "search_api_key 未配置",
	})
	th.Record(probe.ProbeResult{
		Tool:      "web_fetch",
		Available: true,
	})

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
		ToolHealth:    th,
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if mail == "" {
		t.Fatal("IncomingMail should be non-empty")
	}

	// Board snapshot should contain unavailable_tools with web_search
	if !strings.Contains(mail, `"unavailable_tools"`) {
		t.Errorf("snapshot should contain unavailable_tools field, got: %s", mail)
	}
	if !strings.Contains(mail, `"web_search"`) {
		t.Errorf("snapshot should list web_search as unavailable, got: %s", mail)
	}
	// web_fetch is available, so it should NOT appear in unavailable_tools
	if strings.Contains(mail, `"web_fetch"`) {
		t.Errorf("snapshot should not list web_fetch (it's available), got: %s", mail)
	}
}

func TestSchedulerExecutor_ToolHealth_Nil_NoUnavailableTools(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)

	var calls int32
	var capturedHistory []agent.HistoryEntry
	exec := &SchedulerExecutor{
		Inner:         makeInnerExecutor(&calls, &capturedHistory),
		Store:         s,
		Cfg:           cfg,
		BatchUpdateCh: make(chan struct{}),
		WaitTimeout:   100 * time.Millisecond,
		// ToolHealth: nil — backward compatible
	}

	_, err := exec.Execute(context.Background(), schedTask, nil, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail

	// With nil ToolHealth, unavailable_tools should be omitted (backward compat)
	if strings.Contains(mail, `"unavailable_tools"`) {
		t.Errorf("snapshot should NOT contain unavailable_tools when ToolHealth is nil, got: %s", mail)
	}
}
