package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// 结构化 blocked（V6 §5 升级思路 2+3）：submit_task_result status=blocked
// 的提交经 finalization 短路进入 commitStructuredBlocked 收尾事务——
// 任务落 blocked 终态（cause=agent_reported_blocked）、结果摘要与原因保留、
// 依赖它的任务不可认领、非图任务在终态落盘后发布 replan 唤醒任务。

// runStructuredSubmission 驱动一次结构化提交走完 finalization 短路，
// 返回任务终态与 trace 事件。
func runStructuredSubmission(t *testing.T, task *model.Task, sub *StructuredSubmission) (store.TaskStore, *model.Task, *taskmem.Store, []trace.Event) {
	t.Helper()
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	const agentID = "agent-blocked"
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	state := NewSubmitState()
	executor := func(_ context.Context, tk *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		// 模拟 submit_task_result 工具：Put 结构化提交；finalized 标志由
		// flipFinalizationChecker 在下一轮 loop 顶部提供。
		sub.TaskID = tk.ID
		state.Put(sub)
		return ExecuteResult{Output: "progress", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, executor)
	tmStore := taskmem.NewStore(t.TempDir())
	ag.TaskMemStore = tmStore
	ag.FinalizationChecker = &flipFinalizationChecker{}
	ag.SubmitState = state
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, got, tmStore, p1fixesReadTraceEvents(t, traceDir)
}

func findTransition(events []trace.Event, kind trace.EventKind, taskID string) *trace.Transition {
	for _, ev := range events {
		if ev.Kind == kind && ev.TaskID == taskID && ev.Transition != nil {
			return ev.Transition
		}
	}
	return nil
}

func TestFinalizationShortCircuitStructuredBlocked(t *testing.T) {
	task := &model.Task{Description: "数据库迁移", EventType: "code"}
	s, got, tmStore, events := runStructuredSubmission(t, task, &StructuredSubmission{
		Summary:       "数据库迁移缺少权限",
		BlockedReason: "无生产库写权限",
		Status:        SubmitStatusBlocked,
	})

	if got.Status != model.TaskStatusBlocked {
		t.Fatalf("任务状态 = %s，期望 blocked（error: %s）", got.Status, got.Error)
	}
	if !strings.Contains(got.Error, "无生产库写权限") {
		t.Errorf("task.Error 应保留阻塞原因，实际：%q", got.Error)
	}
	mem, err := tmStore.Load(got.ID)
	if err != nil || mem == nil || !mem.Sealed {
		t.Fatalf("blocked 任务的 Task Memory 应已封存: mem=%+v err=%v", mem, err)
	}
	if len(mem.Blockers) != 1 || !strings.Contains(mem.Blockers[0], "无生产库写权限") {
		t.Fatalf("blocked_reason 应在终态迁移前进入 Task Memory.Blockers: %+v", mem.Blockers)
	}
	// 结果摘要保留在与 SubmitResult 同键位的 Results[agentID] 中。
	if !strings.Contains(got.Results["agent-blocked"], "数据库迁移缺少权限") {
		t.Errorf("Results[agent] 应保留结果摘要，实际：%q", got.Results["agent-blocked"])
	}
	if got.LastResponse != got.Results["agent-blocked"] {
		t.Errorf("LastResponse 应等于渲染后的权威结果文本")
	}

	// 终态与提交账本：KindTaskBlocked 与 task_result_committed 都带
	// cause=agent_reported_blocked（与系统兜底拦截区分）。
	if tr := findTransition(events, trace.KindTaskBlocked, got.ID); tr == nil || tr.Cause != "agent_reported_blocked" || tr.NewStatus != string(model.TaskStatusBlocked) {
		t.Errorf("task_blocked 的 Transition 应为 agent_reported_blocked→blocked，实际 %+v", tr)
	}
	if tr := findTransition(events, trace.KindTaskResultCommitted, got.ID); tr == nil ||
		tr.Cause != "agent_reported_blocked" || tr.PrevStatus != string(model.TaskStatusProcessing) || tr.NewStatus != string(model.TaskStatusBlocked) {
		t.Errorf("task_result_committed 的 Transition 应为 processing→blocked/agent_reported_blocked，实际 %+v", tr)
	}

	// blocked 永远不满足依赖：依赖它的任务不可认领。
	dep := &model.Task{Description: "下游", EventType: "code", Dependencies: []string{got.ID}}
	if err := s.PublishTask(dep); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-down", dep.ID); err == nil {
		t.Error("依赖 blocked 任务的下游不应可认领（认领闸只认 completed）")
	}

	// 同一收尾事务：blocked 终态落盘后发布 replan 唤醒任务。
	wake := findReplanWakeTask(t, s, got.ID)
	if wake == nil {
		t.Fatal("非图 blocked 任务应在终态落盘后发布 replan 唤醒任务")
	}
	if !strings.Contains(wake.Description, "agent_reported_blocked") || !strings.Contains(wake.Description, "无生产库写权限") {
		t.Errorf("唤醒任务描述应含 reason_code 与阻塞原因，实际：%s", wake.Description)
	}

	// 重启韧性：store 快照往返后 blocked 终态与唤醒任务俱在。
	src, ok := s.(*store.MemoryTaskStore)
	if !ok {
		t.Fatalf("setup store 类型 %T 不是 *MemoryTaskStore", s)
	}
	snaps := src.ExportSnapshot()
	restored := store.NewMemoryTaskStore(nil, 100, 2, 300)
	if err := restored.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	rt, err := restored.GetTask(got.ID)
	if err != nil {
		t.Fatalf("快照恢复后读取 blocked 任务: %v", err)
	}
	if rt.Status != model.TaskStatusBlocked || !strings.Contains(rt.Error, "无生产库写权限") {
		t.Errorf("快照恢复后任务应为 blocked 且保留原因，实际 status=%s error=%q", rt.Status, rt.Error)
	}
	rw, err := restored.GetTask(wake.ID)
	if err != nil {
		t.Fatalf("快照恢复后读取唤醒任务: %v", err)
	}
	if model.IsTerminal(rw.Status) || rw.EventType != "__scheduler__" {
		t.Errorf("快照恢复后唤醒任务应保持非终态 __scheduler__，实际 %+v", rw)
	}
}

// 图任务的结构化 blocked：终态落 blocked、不发 replan 唤醒任务（图路由由
// graph-terminal-feed 驱动）；event/verdict 键刻意不写 Results——graph 的
// eventNameOf 让 Result["event"] 优先于终态映射，写入会把 blocked 节点
// 错误路由成事件命中。
func TestFinalizationShortCircuitStructuredBlockedGraphTask(t *testing.T) {
	task := &model.Task{
		Description: "图节点", EventType: "code",
		GraphID: "g-1", NodeID: "implement", ActivationID: "implement@1",
	}
	s, got, _, events := runStructuredSubmission(t, task, &StructuredSubmission{
		Summary:       "上游产物未就绪",
		BlockedReason: "依赖缺失",
		Status:        SubmitStatusBlocked,
		Event:         "ready",
		Verdict:       "pass",
	})

	if got.Status != model.TaskStatusBlocked {
		t.Fatalf("图任务状态 = %s，期望 blocked", got.Status)
	}
	if _, ok := got.Results["event"]; ok {
		t.Error("blocked 收尾不应写 Results[\"event\"]（会把节点错误路由成事件命中）")
	}
	if _, ok := got.Results["verdict"]; ok {
		t.Error("blocked 收尾不应写 Results[\"verdict\"]")
	}
	if wake := findReplanWakeTask(t, s, got.ID); wake != nil {
		t.Errorf("图任务不应发布 replan 唤醒任务，实际 %+v", wake)
	}
	if tr := findTransition(events, trace.KindTaskResultCommitted, got.ID); tr == nil || tr.NewStatus != string(model.TaskStatusBlocked) {
		t.Errorf("图任务同样应有 task_result_committed(blocked)，实际 %+v", tr)
	}
}

// 回归：status 缺省（completed）的结构化提交走既有 completed 收尾，
// 并补记 task_result_committed(completed/submit_task_result)。
func TestFinalizationShortCircuitCompletedEmitsResultCommitted(t *testing.T) {
	task := &model.Task{Description: "普通提交", EventType: "code"}
	_, got, _, events := runStructuredSubmission(t, task, &StructuredSubmission{
		Summary: "写入了 report.md 并通过测试",
	})

	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("任务状态 = %s，期望 completed（error: %s）", got.Status, got.Error)
	}
	if !strings.Contains(got.Results["agent-blocked"], "写入了 report.md 并通过测试") {
		t.Errorf("Results[agent] 应为渲染后的权威结果文本，实际：%q", got.Results["agent-blocked"])
	}
	tr := findTransition(events, trace.KindTaskResultCommitted, got.ID)
	if tr == nil || tr.NewStatus != string(model.TaskStatusCompleted) || tr.Cause != "submit_task_result" {
		t.Errorf("task_result_committed 应为 completed/submit_task_result，实际 %+v", tr)
	}
}
