package scheduler

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/model"
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

func TestSchedulerExecutor_NoBatch_DirectExecute(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

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
	cfg := &config.Config{WorkerCount: 2}

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
	if !strings.Contains(mail, `"mode": "immediate"`) {
		t.Errorf("snapshot should contain mode=immediate, got: %s", mail)
	}
}

func TestSchedulerExecutor_BatchPending_WaitsUntilComplete(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

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
	cfg := &config.Config{WorkerCount: 1}

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
	cfg := &config.Config{WorkerCount: 1}

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
	cfg := &config.Config{WorkerCount: 1}

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
	cfg := &config.Config{WorkerCount: 1}

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
