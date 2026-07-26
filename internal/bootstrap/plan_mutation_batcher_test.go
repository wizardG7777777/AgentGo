package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

// batcherTestMutation 造一条指向 planID 的普通节点状态变更。
func batcherTestMutation(planID, taskID string, status model.TaskStatus) store.TaskMutation {
	return store.TaskMutation{
		Kind:       store.TaskMutationStatus,
		Task:       &model.Task{ID: taskID, PlanID: planID, Status: status},
		FromStatus: model.TaskStatusProcessing,
		ToStatus:   status,
	}
}

// C1：FIFO——同一通道的 N 条变更按入队顺序应用（跨批次拍平后仍有序）。
func TestPlanMutationBatcherAppliesInFIFOOrder(t *testing.T) {
	goroutinesBefore := runtime.NumGoroutine()
	var mu sync.Mutex
	var seq []string
	b := newPlanMutationBatcher(nil)
	t.Cleanup(b.Stop)
	b.apply = func(_ *plan.Coordinator, batch []store.TaskMutation) []error {
		mu.Lock()
		for _, m := range batch {
			seq = append(seq, m.Task.ID)
		}
		mu.Unlock()
		return make([]error, len(batch))
	}

	const n = 50
	for i := 0; i < n; i++ {
		if err := b.submit(batcherTestMutation("p-fifo", fmt.Sprintf("t-%02d", i), model.TaskStatusCompleted)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(seq) != n {
		t.Fatalf("applied=%d, want %d", len(seq), n)
	}
	for i, id := range seq {
		if want := fmt.Sprintf("t-%02d", i); id != want {
			t.Fatalf("seq[%d]=%s, want %s (FIFO violated)", i, id, want)
		}
	}
	// flusher 必须随 Stop 退出，不泄漏 goroutine（无 CGO/-race，用计数 + exited 双断言）。
	select {
	case <-b.exited:
	default:
		t.Fatal("flusher goroutine still running after Stop")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > goroutinesBefore && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > goroutinesBefore {
		t.Fatalf("goroutines=%d, want <= %d (flusher leaked)", got, goroutinesBefore)
	}
}

// C1：调用方解耦——apply 被人为注入 300ms 延迟时，store 的变更调用
// （PublishTask/ClaimTask/FailTask）仍即时返回，落盘在 flusher 上完成。
func TestPlanMutationBatcherDecouplesCallerFromSlowApply(t *testing.T) {
	var applied int
	var mu sync.Mutex
	b := newPlanMutationBatcher(nil)
	t.Cleanup(b.Stop)
	b.apply = func(_ *plan.Coordinator, batch []store.TaskMutation) []error {
		time.Sleep(300 * time.Millisecond) // 模拟慢盘 fsync
		mu.Lock()
		applied += len(batch)
		mu.Unlock()
		return make([]error, len(batch))
	}

	s := store.NewMemoryTaskStore(nil, 32, 1, 60)
	t.Cleanup(func() { _ = s.Close() })
	s.SetTaskPlanHooks(store.TaskPlanHooks{Mutated: b.submit})

	task := &model.Task{Description: "decouple caller from slow plan persist", PlanID: "p-slow"}
	start := time.Now()
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("PublishTask blocked on slow plan persist: %v", elapsed)
	}
	start = time.Now()
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("ClaimTask blocked on slow plan persist: %v", elapsed)
	}
	start = time.Now()
	if err := s.FailTask("worker-1", task.ID, "boom"); err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("FailTask blocked on slow plan persist: %v", elapsed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.Stop()
	mu.Lock()
	defer mu.Unlock()
	if applied != 3 {
		t.Fatalf("applied=%d, want 3 (published + claim + fail)", applied)
	}
}

// C1 合并落盘：K 条快速连续变更 → 真实 plan store 上的 fsync 级落盘次数
// 显著小于 K（计数经 plan.Store.PersistCount 观测），且全部生效。
func TestPlanMutationBatcherCoalescesPersists(t *testing.T) {
	dir := t.TempDir()
	ps, err := plan.OpenStore(filepath.Join(dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := plan.NewCoordinator(ps, nil)
	ctx := context.Background()
	p, err := c.Create(ctx, plan.CreateInput{PlanID: "p-coalesce", RootTaskID: "p-coalesce-root", Budget: model.PlanBudget{}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const k = 8
	for i := 0; i < k; i++ {
		p, err = c.RegisterTask(ctx, plan.RegisterTaskInput{
			PlanID: p.ID, ObservedRevision: p.CurrentRevision,
			Node: model.PlanNode{TaskID: fmt.Sprintf("w-%d", i), Title: "w", Role: model.PlanNodeRoleImplementation},
		})
		if err != nil {
			t.Fatalf("RegisterTask %d: %v", i, err)
		}
	}
	baseline := ps.PersistCount()

	// 首批 apply 阻塞在 gate 上，直到 K 条全部入队——确定性构造合并窗口：
	// 首条单独一批，其余 K-1 条已在队列中被一次排空合并。
	gate := make(chan struct{})
	var gateOnce sync.Once
	b := newPlanMutationBatcher(c)
	t.Cleanup(b.Stop)
	b.apply = func(coord *plan.Coordinator, batch []store.TaskMutation) []error {
		gateOnce.Do(func() { <-gate })
		return applyPlannedMutationBatch(coord, batch)
	}
	for i := 0; i < k; i++ {
		if err := b.submit(batcherTestMutation(p.ID, fmt.Sprintf("w-%d", i), model.TaskStatusCompleted)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	close(gate)

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.Stop()

	persists := ps.PersistCount() - baseline
	if persists <= 0 || persists > 2 {
		t.Fatalf("persists=%d, want 1..2 (coalesced); K=%d 逐条应为 %d", persists, k, k)
	}
	if persists >= int64(k) {
		t.Fatalf("persists=%d not fewer than K=%d mutations", persists, k)
	}
	got, err := ps.GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < k; i++ {
		node := got.Nodes[fmt.Sprintf("w-%d", i)]
		if node.Status != model.TaskStatusCompleted {
			t.Fatalf("node w-%d status=%s, want completed", i, node.Status)
		}
	}
}

// C1 失败重试保持 FIFO：首次实际合并出的批次全部失败后，下一批必须先按
// 原顺序重试该批，再接上退避期间的新入队项。首次批次大小由 goroutine
// 调度决定，连续 submit 不保证在 flusher 第一次排空前全部入队。
func TestPlanMutationBatcherRetriesFailedBatchInOrder(t *testing.T) {
	var mu sync.Mutex
	var attempts [][]string
	calls := 0
	releaseFirstFailure := make(chan struct{})
	b := newPlanMutationBatcher(nil)
	t.Cleanup(b.Stop)
	b.apply = func(_ *plan.Coordinator, batch []store.TaskMutation) []error {
		mu.Lock()
		calls++
		attempt := calls
		ids := make([]string, len(batch))
		for i, m := range batch {
			ids[i] = m.Task.ID
		}
		attempts = append(attempts, ids)
		mu.Unlock()
		if attempt == 1 {
			// 允许 flusher 在任意一个 submit 后形成首批，但必须等其余
			// 变更全部入队后才注入失败，消除 10ms 退避上的调度假设。
			<-releaseFirstFailure
			errs := make([]error, len(batch))
			for i := range errs {
				errs[i] = errors.New("transient persist failure")
			}
			return errs
		}
		return make([]error, len(batch))
	}

	for _, id := range []string{"t0", "t1", "t2"} {
		if err := b.submit(batcherTestMutation("p-retry", id, model.TaskStatusCompleted)); err != nil {
			t.Fatalf("submit %s: %v", id, err)
		}
	}
	close(releaseFirstFailure)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("attempts=%v, want exactly one failed attempt and one retry", attempts)
	}
	first, retry := attempts[0], attempts[1]
	if len(first) == 0 || len(first) > len(retry) {
		t.Fatalf("invalid retry batches: first=%v retry=%v", first, retry)
	}
	for i := range first {
		if retry[i] != first[i] {
			t.Fatalf("failed batch is not the retry prefix: first=%v retry=%v", first, retry)
		}
	}
	wantRetry := []string{"t0", "t1", "t2"}
	if len(retry) != len(wantRetry) {
		t.Fatalf("retry=%v, want all outstanding mutations %v", retry, wantRetry)
	}
	for i := range wantRetry {
		if retry[i] != wantRetry[i] {
			t.Fatalf("retry=%v, want FIFO order %v", retry, wantRetry)
		}
	}
}

// C1 Shutdown 语义：持续失败时 Drain 按 2s 界超时，Stop 丢弃残余并计数，
// flusher 退出（不泄漏）。
func TestPlanMutationBatcherDrainTimeoutThenStopDrops(t *testing.T) {
	b := newPlanMutationBatcher(nil)
	b.apply = func(_ *plan.Coordinator, batch []store.TaskMutation) []error {
		errs := make([]error, len(batch))
		for i := range errs {
			errs[i] = errors.New("persist down")
		}
		return errs
	}

	const n = 5
	for i := 0; i < n; i++ {
		if err := b.submit(batcherTestMutation("p-drop", fmt.Sprintf("t-%d", i), model.TaskStatusFailed)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := b.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain err=%v, want context.DeadlineExceeded", err)
	}

	stopped := make(chan struct{})
	go func() { b.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return promptly while apply keeps failing")
	}
	select {
	case <-b.exited:
	default:
		t.Fatal("flusher goroutine still running after Stop")
	}
	if got := b.Dropped(); got != n {
		t.Fatalf("dropped=%d, want %d", got, n)
	}
}

// C1：Stop 之后的 submit 直接丢弃并计数，不阻塞调用方、不报错
// （store 的 Mutated hook 约定：失败会进 store backlog，关停期必须避免）。
func TestPlanMutationBatcherSubmitAfterStopIsDropped(t *testing.T) {
	b := newPlanMutationBatcher(nil)
	b.Stop()
	if err := b.submit(batcherTestMutation("p-late", "t-late", model.TaskStatusCompleted)); err != nil {
		t.Fatalf("submit after Stop should return nil, got %v", err)
	}
	if got := b.Dropped(); got != 1 {
		t.Fatalf("dropped=%d, want 1", got)
	}
	// 无未决项时 Drain 立即返回。
	if err := b.Drain(context.Background()); err != nil {
		t.Fatalf("Drain on drained+stopped batcher: %v", err)
	}
}

// C1 端到端：真实 store + batcher + 真实 coordinator/plan store。
// agent 关键路径（FailTaskBySystem，watchdog 也用这条）只入队；
// Drain 后 plan 事实（节点终态 + wake ReplanRequest）已落盘。
func TestPlanMutationBatcherEndToEndThroughStore(t *testing.T) {
	dir := t.TempDir()
	ps, err := plan.OpenStore(filepath.Join(dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := plan.NewCoordinator(ps, nil)
	ctx := context.Background()
	p, err := c.Create(ctx, plan.CreateInput{PlanID: "p-e2e", RootTaskID: "p-e2e-root", Budget: model.PlanBudget{}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	s := store.NewMemoryTaskStore(nil, 32, 1, 60)
	t.Cleanup(func() { _ = s.Close() })
	b := newPlanMutationBatcher(c)
	t.Cleanup(b.Stop)
	hooks := makeTaskPlanHooks(c, nil)
	hooks.Mutated = b.submit
	s.SetTaskPlanHooks(hooks)

	// Prepare hook 经 acceptance 控制路径注册节点（NodeRole 显式指定，保持 implementation）。
	task := &model.Task{
		ID: "work-1", Description: "e2e", PlanID: p.ID,
		NodeRole: model.PlanNodeRoleImplementation, PlanMutationSource: "acceptance",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.FailTaskBySystem(task.ID, "watchdog timeout"); err != nil {
		t.Fatalf("FailTaskBySystem: %v", err)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := b.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	b.Stop()

	got, err := ps.GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	node := got.Nodes["work-1"]
	if node.Status != model.TaskStatusFailed {
		t.Fatalf("plan node status=%s, want failed (mutation persisted)", node.Status)
	}
	if len(got.PendingReplanRequests) == 0 {
		t.Fatal("wake ReplanRequest was not persisted by the batcher")
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || signal.Urgency != model.ReplanUrgencyHigh {
		t.Fatalf("TrySignal=%+v ok=%v err=%v, want high-urgency failure wake", signal, ok, err)
	}
}
