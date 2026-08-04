package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// findReplanWakeTask 在公告板上查找某任务的「通用 replan 唤醒任务」
// （EventType=__scheduler__ 且描述含幂等标记 [replan-request: <taskID>/replan]，
// 见 replan_wake.go）。找到多于一条时判失败——幂等契约要求同一任务的
// 未处理唤醒至多一个；未找到返回 nil。
func findReplanWakeTask(t *testing.T, s store.TaskStore, taskID string) *model.Task {
	t.Helper()
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	marker := replanRequestMarker(taskID)
	var found *model.Task
	for _, tk := range tasks {
		if tk == nil || tk.EventType != "__scheduler__" {
			continue
		}
		if strings.Contains(tk.Description, marker) {
			if found != nil {
				t.Fatalf("同一任务不应有多个未处理 replan 唤醒任务: %s 与 %s", found.ID, tk.ID)
			}
			found = tk
		}
	}
	return found
}

// TestRuntimeLoopFuse_BlocksTaskAndPublishesReplanWake 验证 emergency fuse
// 触发时的完整收口（V6 docs/nextUpgrade-V6.md §5 升级思路 8 + C6b replan 唤醒）：
//   - 任务 processing → blocked（终态），而非 RetryRollback / failed
//   - emit KindRuntimeLoopFuseTriggered 事件
//   - 非图任务发布 reason_code=runtime_loop_fuse 的通用 replan 唤醒任务
//     （EventType=__scheduler__，描述首行幂等标记），交 Scheduler 裁决
//   - 绝不自动重跑同一 Task（无 KindTaskRetry、RetryCount 不增、状态非 pending）
func TestRuntimeLoopFuse_BlocksTaskAndPublishesReplanWake(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "fuse trip", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-fuse", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	var callCount int32
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		atomic.AddInt32(&callCount, 1)
		// 永远调用工具 = 永不自然完成的死循环，必须由 fuse 兜底
		return ExecuteResult{Output: "looping", ToolCalled: true}, nil
	}

	ag := NewAgent("agent-fuse", "code", s, r, executor)
	ag.loopFuse = 5 // 测试覆盖口：生产恒为 emergencyLoopFuse=10000

	ag.processTask(context.Background(), task.ID)

	// fuse=5 → i=0..4 共 5 次 LLM 调用后，i=5 触发 fuse，executor 不再被调用
	if got := atomic.LoadInt32(&callCount); got != 5 {
		t.Errorf("executor 调用次数 = %d, want 5（fuse 阈值处应停止调用 LLM）", got)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusBlocked {
		t.Errorf("status = %s, want blocked（fuse 触发后任务必须进 blocked 终态）", got.Status)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0（fuse 路径不得重试回滚）", got.RetryCount)
	}
	if !strings.Contains(got.Error, "emergency loop fuse") {
		t.Errorf("task.Error = %q, want 含 emergency loop fuse 原因", got.Error)
	}

	// replan 唤醒任务：公告板上恰好一条，形状符合 replan_wake.go 契约。
	wake := findReplanWakeTask(t, s, task.ID)
	if wake == nil {
		t.Fatal("非图任务 fuse 后应发布通用 replan 唤醒任务")
	}
	if firstLine, _, _ := strings.Cut(wake.Description, "\n"); firstLine != replanRequestMarker(task.ID) {
		t.Errorf("唤醒任务描述首行 = %q, want 幂等标记 %q", firstLine, replanRequestMarker(task.ID))
	}
	if wake.EventSource != "replan-request" {
		t.Errorf("EventSource = %q, want replan-request", wake.EventSource)
	}
	if wake.ParentTaskID != task.ID {
		t.Errorf("ParentTaskID = %q, want %s", wake.ParentTaskID, task.ID)
	}
	if wake.MaxConcurrency != 1 {
		t.Errorf("MaxConcurrency = %d, want 1", wake.MaxConcurrency)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务刻意不携带图身份，实际 graph=%s node=%s activation=%s",
			wake.GraphID, wake.NodeID, wake.ActivationID)
	}
	if !strings.Contains(wake.Description, "reason_code=runtime_loop_fuse") {
		t.Errorf("唤醒任务描述应含 reason_code=runtime_loop_fuse，实际: %s", wake.Description)
	}

	// trace 事实：有 runtime_loop_fuse_triggered 与 task_blocked，
	// 且整个生命周期没有 task_retry（不得自动重跑）
	events := p1fixesReadTraceEvents(t, traceDir)
	var fuseEv, blockedEv *trace.Event
	for i, ev := range events {
		if ev.TaskID != task.ID {
			continue
		}
		switch ev.Kind {
		case trace.KindRuntimeLoopFuseTriggered:
			fuseEv = &events[i]
		case trace.KindTaskBlocked:
			blockedEv = &events[i]
		case trace.KindTaskRetry:
			t.Fatalf("fuse 路径不得 emit KindTaskRetry（不得自动重跑同一 Task）: %+v", ev)
		}
	}
	if fuseEv == nil {
		t.Fatalf("未 emit runtime_loop_fuse_triggered，事件：%s", eventKinds(events))
	}
	if fuseEv.Loop != 5 {
		t.Errorf("fuse 事件 Loop = %d, want 5", fuseEv.Loop)
	}
	if blockedEv == nil {
		t.Fatalf("未 emit task_blocked，事件：%s", eventKinds(events))
	}
	if blockedEv.Transition == nil || blockedEv.Transition.Cause != "runtime_loop_fuse" ||
		blockedEv.Transition.NewStatus != string(model.TaskStatusBlocked) {
		t.Errorf("task_blocked Transition = %+v, want cause=runtime_loop_fuse new=blocked", blockedEv.Transition)
	}
}

// TestRuntimeLoopFuse_GraphTaskSkipsReplanWake 图任务（GraphID 非空）触发
// fuse 时仍进 blocked，但不发布 replan 唤醒任务——终态由 graph-terminal-feed
// 回填引擎按边条件路由，无需唤醒 Scheduler。
func TestRuntimeLoopFuse_GraphTaskSkipsReplanWake(t *testing.T) {
	setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{
		Description: "graph fuse", EventType: "code",
		GraphID: "graph-1", NodeID: "node-a", ActivationID: "node-a@1",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-fuse-graph", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "looping", ToolCalled: true}, nil
	}
	ag := NewAgent("agent-fuse-graph", "code", s, r, executor)
	ag.loopFuse = 3

	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusBlocked {
		t.Errorf("status = %s, want blocked", got.Status)
	}
	if wake := findReplanWakeTask(t, s, task.ID); wake != nil {
		t.Errorf("图任务不应发布 replan 唤醒任务，实际: %+v", wake)
	}
}

// TestPublishReplanWakeTask_Idempotent 幂等契约：同一任务已有未处理（非终态）
// 唤醒任务时，重复触发不重复发布（保留首条）。
func TestPublishReplanWakeTask_Idempotent(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "dup replan", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-dup", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent("agent-dup", "code", s, r, executor)

	ag.publishReplanWakeTask(task, task.ID, "workspace_conflict", "第一次详情")
	ag.publishReplanWakeTask(task, task.ID, "workspace_conflict", "第二次详情")

	// findReplanWakeTask 已断言唯一性；再确认保留的是首条。
	wake := findReplanWakeTask(t, s, task.ID)
	if wake == nil {
		t.Fatal("应存在一条 replan 唤醒任务")
	}
	if !strings.Contains(wake.Description, "第一次详情") {
		t.Errorf("重复触发应保留首条唤醒任务，实际描述: %s", wake.Description)
	}
}

// TestReactLoop_NoFixedRoundCap 验证固定轮数上限已删除（V6 升级思路 5）：
// Loop 跑过旧默认值 50 轮仍不终止，最终由结构化终态（自然完成）收尾。
func TestReactLoop_NoFixedRoundCap(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "no round cap", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-nocap", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	const totalRounds = 60 // 越过旧默认上限 50
	var callCount int32
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		n := atomic.AddInt32(&callCount, 1)
		if int(n) < totalRounds {
			return ExecuteResult{Output: "working", ToolCalled: true}, nil
		}
		// 第 60 轮自然完成（结构化终态）
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-nocap", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	if got := atomic.LoadInt32(&callCount); got != totalRounds {
		t.Fatalf("executor 调用次数 = %d, want %d（固定轮数上限已删除，Loop 应跑过旧默认值 50）", got, totalRounds)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed（自然完成为唯一正常收尾）", got.Status)
	}
}

// TestReactLoop_FuseDoesNotFireBelowThreshold fuse 不是正常终止条件：
// 阈值之内自然完成的任务不得触发 fuse 事件，状态为 completed。
func TestReactLoop_FuseDoesNotFireBelowThreshold(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "below fuse", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-below", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	var callCount int32
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		n := atomic.AddInt32(&callCount, 1)
		if int(n) < 3 {
			return ExecuteResult{Output: "working", ToolCalled: true}, nil
		}
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-below", "code", s, r, executor)
	ag.loopFuse = 5
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	for _, ev := range p1fixesReadTraceEvents(t, traceDir) {
		if ev.Kind == trace.KindRuntimeLoopFuseTriggered {
			t.Fatalf("阈值之内不得触发 fuse 事件: %+v", ev)
		}
	}
}
