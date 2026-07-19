package plan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/model"
)

// C1 批落盘：N 条变更合并为一次克隆 + 一次原子落盘，版本逐条递增且顺序保持。
func TestRecordTaskMutationsBatchAppliesInOrderAndPersistsOnce(t *testing.T) {
	dir := t.TempDir()
	ps, err := OpenStore(filepath.Join(dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := NewCoordinator(ps, nil)
	p := createTestPlan(t, c, "p-batch", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "task-a")
	p = registerNode(t, c, p.ID, p.CurrentRevision, "task-b")
	registerNode(t, c, p.ID, p.CurrentRevision, "task-c")

	baseline := ps.PersistCount()
	versions, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: "p-batch", TaskID: "task-a", Mutation: TaskMutation{Status: model.TaskStatusProcessing}},
		{PlanID: "p-batch", TaskID: "task-b", Mutation: TaskMutation{Status: model.TaskStatusProcessing}},
		{PlanID: "p-batch", TaskID: "task-a", Mutation: TaskMutation{
			Status: model.TaskStatusCompleted, Wake: true,
			SourceEvent: "status", ReasonCode: "task_completed", IdempotencyKey: "batch-a-done",
		}},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	if !(versions[0] < versions[1] && versions[1] < versions[2]) {
		t.Fatalf("versions not strictly increasing (FIFO apply): %v", versions)
	}
	if got := ps.PersistCount() - baseline; got != 1 {
		t.Fatalf("persist count delta=%d, want exactly 1 (coalesced fsync)", got)
	}
	got, err := ps.GetPlan("p-batch")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionStateVersion != versions[2] {
		t.Fatalf("ExecutionStateVersion=%d, want %d", got.ExecutionStateVersion, versions[2])
	}
	if got.Nodes["task-a"].Status != model.TaskStatusCompleted ||
		got.Nodes["task-b"].Status != model.TaskStatusProcessing ||
		got.Nodes["task-c"].Status != model.TaskStatusPending {
		t.Fatalf("batch statuses not applied in order: %+v", got.Nodes)
	}
	// wake 变更在同一 durable 事务内追加 ReplanRequest
	if len(got.PendingReplanRequests) != 1 {
		t.Fatalf("pending replan requests=%d, want 1", len(got.PendingReplanRequests))
	}
	signal, ok, err := c.TrySignal("p-batch")
	if err != nil || !ok || signal.LatestExecutionStateVersion != versions[2] {
		t.Fatalf("TrySignal=%+v ok=%v err=%v", signal, ok, err)
	}
}

// C1 批内失败隔离：中间一条引用不存在节点 → 仅该条回滚报错，其余照常落盘，
// 且整批仍只落盘一次。
func TestRecordTaskMutationsBatchFailureIsolation(t *testing.T) {
	dir := t.TempDir()
	ps, err := OpenStore(filepath.Join(dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := NewCoordinator(ps, nil)
	p := createTestPlan(t, c, "p-isolation", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "task-a")
	registerNode(t, c, p.ID, p.CurrentRevision, "task-b")

	baseline := ps.PersistCount()
	versions, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: "p-isolation", TaskID: "task-a", Mutation: TaskMutation{Status: model.TaskStatusCompleted}},
		{PlanID: "p-isolation", TaskID: "ghost", Mutation: TaskMutation{Status: model.TaskStatusCompleted}},
		{PlanID: "p-isolation", TaskID: "task-b", Mutation: TaskMutation{Status: model.TaskStatusFailed}},
	})
	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("good mutations failed: %v", errs)
	}
	if !errors.Is(errs[1], ErrNodeNotFound) {
		t.Fatalf("errs[1]=%v, want ErrNodeNotFound", errs[1])
	}
	if versions[0] == 0 || versions[2] == 0 || versions[1] != 0 {
		t.Fatalf("versions=%v, want failed entry zeroed", versions)
	}
	if got := ps.PersistCount() - baseline; got != 1 {
		t.Fatalf("persist count delta=%d, want exactly 1 despite per-item failure", got)
	}
	got, err := ps.GetPlan("p-isolation")
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes["task-a"].Status != model.TaskStatusCompleted || got.Nodes["task-b"].Status != model.TaskStatusFailed {
		t.Fatalf("successful batch entries not applied: %+v", got.Nodes)
	}
}

// C1 落盘失败语义：writeStateAtomic 失败时内存状态不前进，批内全部条目
// （含闭包本身成功的）统一标记失败，调用方可整体重试。
func TestRecordTaskMutationsBatchPersistFailureMarksAll(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ps, err := OpenStore(filepath.Join(sub, "plan-state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := NewCoordinator(ps, nil)
	p := createTestPlan(t, c, "p-persist-fail", model.PlanBudget{})
	registerNode(t, c, p.ID, p.CurrentRevision, "task-a")

	// 把持久化目录替换为同名文件 → writeStateAtomic 的 MkdirAll 必失败。
	if err := os.Remove(filepath.Join(sub, "plan-state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}

	baseline := ps.PersistCount()
	_, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: "p-persist-fail", TaskID: "task-a", Mutation: TaskMutation{Status: model.TaskStatusCompleted}},
		{PlanID: "p-persist-fail", TaskID: "task-a", Mutation: TaskMutation{Summary: "x"}},
	})
	for i, err := range errs {
		if err == nil {
			t.Fatalf("errs[%d]=nil, want persist error for the whole batch", i)
		}
	}
	if got := ps.PersistCount() - baseline; got != 0 {
		t.Fatalf("persist count delta=%d, want 0 (nothing persisted)", got)
	}
	got, err := ps.GetPlan("p-persist-fail")
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes["task-a"].Status != model.TaskStatusPending {
		t.Fatalf("in-memory state advanced despite persist failure: %+v", got.Nodes["task-a"])
	}
}

// C1 唤醒信号：同一 Plan 的多条 wake 变更在批内只发射一次内存信号
// （信号通道容量为 1，与逐条提交语义等价），durable 请求逐条都在。
func TestRecordTaskMutationsBatchWakeRequestsAllDurable(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-wake-batch", model.PlanBudget{})
	p = registerNode(t, c, p.ID, p.CurrentRevision, "task-a")
	registerNode(t, c, p.ID, p.CurrentRevision, "task-b")

	versions, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: "p-wake-batch", TaskID: "task-a", Mutation: TaskMutation{
			Status: model.TaskStatusCompleted, Wake: true, SourceEvent: "status", ReasonCode: "task_completed",
		}},
		{PlanID: "p-wake-batch", TaskID: "task-b", Mutation: TaskMutation{
			Status: model.TaskStatusFailed, Wake: true, SourceEvent: "status", ReasonCode: "task_failed",
			Urgency: model.ReplanUrgencyHigh,
		}},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	got, err := c.Store().GetPlan("p-wake-batch")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PendingReplanRequests) != 2 {
		t.Fatalf("pending replan requests=%d, want 2 (both wake facts durable)", len(got.PendingReplanRequests))
	}
	signal, ok, err := c.TrySignal("p-wake-batch")
	if err != nil || !ok {
		t.Fatalf("TrySignal ok=%v err=%v", ok, err)
	}
	if signal.LatestExecutionStateVersion != versions[1] || signal.Urgency != model.ReplanUrgencyHigh {
		t.Fatalf("signal=%+v, want latest version %d with high urgency", signal, versions[1])
	}
	if len(signal.RequestIDs) != 2 {
		t.Fatalf("signal request ids=%v, want 2", signal.RequestIDs)
	}
	// NextSignal 依赖内存信号通道：批内去重后恰好一条 pending，读走即空。
	select {
	case <-c.signalChannel("p-wake-batch"):
	default:
		t.Fatal("in-memory wake signal missing after batch commit")
	}
	select {
	case <-c.signalChannel("p-wake-batch"):
		t.Fatal("duplicate in-memory wake signal for one batch")
	default:
	}
}

// 单条 RecordTaskMutation 走批量委托后行为不变（回归守卫）。
func TestRecordTaskMutationDelegatesToBatch(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-delegate", model.PlanBudget{})
	registerNode(t, c, p.ID, p.CurrentRevision, "task-a")

	version, err := c.RecordTaskMutation(context.Background(), "p-delegate", "task-a", TaskMutation{Status: model.TaskStatusProcessing})
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("version=%d, want 1", version)
	}
	if _, err := c.RecordTaskMutation(context.Background(), "p-delegate", "ghost", TaskMutation{Status: model.TaskStatusCompleted}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("err=%v, want ErrNodeNotFound", err)
	}
	if _, err := c.RecordTaskMutation(context.Background(), "ghost-plan", "task-a", TaskMutation{}); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("err=%v, want ErrPlanNotFound", err)
	}
}

// 批内乱序引用：同一批里后面的变更可以看到前面变更的結果（顺序应用到
// 同一演进状态，等价于逐条 update）。
func TestRecordTaskMutationsBatchSeesSequentialState(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-seq", model.PlanBudget{})
	registerNode(t, c, p.ID, p.CurrentRevision, "task-a")

	// 第一条把 task-a 置为终态（ActiveTasks 由 RegisterTask 记账），
	// 第二条再把它拉回 processing——ActiveTasks 计数必须按顺序先减后增。
	p, err := c.Store().GetPlan("p-seq")
	if err != nil {
		t.Fatal(err)
	}
	activeBefore := p.Usage.ActiveTasks
	_, errs := c.RecordTaskMutations(context.Background(), []PlanTaskMutation{
		{PlanID: "p-seq", TaskID: "task-a", Mutation: TaskMutation{Status: model.TaskStatusCompleted}},
		{PlanID: "p-seq", TaskID: "task-a", Mutation: TaskMutation{Status: model.TaskStatusProcessing}},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	got, err := c.Store().GetPlan("p-seq")
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes["task-a"].Status != model.TaskStatusProcessing {
		t.Fatalf("final status=%s, want processing (second mutation wins)", got.Nodes["task-a"].Status)
	}
	if got.Usage.ActiveTasks != activeBefore {
		t.Fatalf("ActiveTasks=%d, want %d (sequential accounting)", got.Usage.ActiveTasks, activeBefore)
	}
}
