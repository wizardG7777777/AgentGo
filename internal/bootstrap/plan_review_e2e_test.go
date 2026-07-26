package bootstrap

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// traceCaptureDispatcher 捕获测试期间的全部 trace 事件，供失败诊断。
type traceCaptureDispatcher struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *traceCaptureDispatcher) Dispatch(ev trace.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

// planGateScriptedLLM 是 plan-gate 端到端测试的脚本化 LLM：按 responses
// 顺序返回，超出后返回 "done" 文本响应（与 scheduler 集成测试的 mock 同型）。
type planGateScriptedLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
	callLog   []string // 每次调用的诊断信息（消息数 / 工具数 / 末条消息摘要）
}

func (s *planGateScriptedLLM) Chat(_ context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := ""
	if len(msgs) > 0 {
		last = msgs[len(msgs)-1].Content
		if len(last) > 60 {
			last = last[:60]
		}
	}
	s.callLog = append(s.callLog, fmt.Sprintf("call#%d msgs=%d tools=%d last=%q", s.calls+1, len(msgs), len(tools), last))
	if s.calls < len(s.responses) {
		r := s.responses[s.calls]
		s.calls++
		return r, nil
	}
	s.calls++
	return llm.Response{Content: "done"}, nil
}

// waitForCondition 以 20ms 轮询直到 cond 成立或超时；超时即失败。
func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// dumpPlanGateState 在失败路径输出诊断：LLM 调用次数、全部任务状态、Plan 状态。
func dumpPlanGateState(t *testing.T, s store.TaskStore, coord *plan.Coordinator, mock *planGateScriptedLLM) {
	t.Helper()
	mock.mu.Lock()
	calls := mock.calls
	callLog := append([]string(nil), mock.callLog...)
	mock.mu.Unlock()
	t.Logf("LLM calls = %d", calls)
	for _, line := range callLog {
		t.Logf("  %s", line)
	}
	tasks, _ := s.ScanAll()
	for _, task := range tasks {
		t.Logf("task %s status=%s eventType=%q role=%q planID=%s desc=%.40q err=%q",
			task.ID[:8], task.Status, task.EventType, task.NodeRole, task.PlanID, task.Description, task.Error)
	}
	plans, _ := coord.Store().ListPlans()
	for _, p := range plans {
		t.Logf("plan %s status=%s active=%s nodes=%v", p.ID[:8], p.Status, p.ActiveDecisionTaskID[:8], p.CurrentNodeIDs)
	}
}

// TestPlanGate_EndToEnd_SubmitApproveExecute 是 plan-gate 模式的端到端
// 集成测试，走通完整链路：
//
//  1. gate=plan，用户输入经 Activator 发布根 controller 任务；
//  2. scheduler LLM 调 submit_plan_for_review → Plan 挂起为 plan_review，
//     根 controller 协作挂起为 blocked，批准前 DAG 中没有任何执行节点；
//  3. bootstrap approvePlanReview（用户在 /plan 上的批准动作）→ 预发布
//     control-reserved 新 controller + ResolvePause 原子恢复；
//  4. 保留 controller 被 scheduler agent 认领，LLM 继续 publish_task →
//     执行节点注册进 Plan DAG（批准后才开始派发）。
func TestPlanGate_EndToEnd_SubmitApproveExecute(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	s.SetTaskPlanHooks(makeTaskPlanHooks(coord, nil))
	cfg := config.DefaultConfig()
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}
	modeStore := modes.NewStore(modes.GatePlan, modes.ExecNormal, modes.TopoTeam)

	const planText = "# 执行计划\n1. 写 hello.go（预期产物）\n2. 正式验收"
	// LLM 调用序列（与执行边界严格对应）：
	//   call#1（根 controller 第 1 轮）：submit_plan_for_review → Plan 挂起。
	//     挂起后根 controller 的第 2 轮在 SchedulerExecutor 入口的
	//     requireRunnablePlan 边界直接返回 ErrExecutionSuspended——不再产生
	//     LLM 调用，任务协作挂起为 blocked。
	//   call#2（保留 controller 第 1 轮）：pause_resolved 信号唤醒，publish_task
	//     派发执行节点。
	//   call#3（保留 controller 第 2 轮）：文本收尾（此后 waitForPlanSignal
	//     阻塞等待执行节点终态，直到测试取消 ctx）。
	mockLLM := &planGateScriptedLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "submit_plan_for_review",
			Arguments: map[string]any{"plan": planText}}}},
		{ToolCalls: []llm.ToolCall{{ID: "call_2", Name: "publish_task",
			Arguments: map[string]any{"description": "写 hello.go", "event_type": "", "node_role": "implementation"}}}},
		{Content: "已按批准的计划派发执行。"},
	}}
	bundle := scheduler.New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil,
		nil, nil, nil, io.Discard, io.Discard, modeStore, coord)
	// text-only 兜底落盘目录隔离到 TempDir，避免在包目录下留测试残留文件。
	bundle.Agent.TextOnlyReportsDir = t.TempDir()

	// 捕获全部 trace 事件供失败诊断（测试结束即卸下）。
	traceCap := &traceCaptureDispatcher{}
	trace.SetDefaultDispatcher(traceCap)
	defer trace.SetDefaultDispatcher(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bundle.Activator.Run(ctx) }()
	go func() { defer wg.Done(); bundle.Agent.Run(ctx) }()

	ch <- model.Event{Type: model.EventUserInput, Payload: map[string]string{"text": "写一个 hello.go"}}

	// ── 阶段 1：Plan 挂起为 plan_review，根 controller 协作挂起为 blocked ──
	var paused model.Plan
	var rootTask *model.Task
	waitForCondition(t, 10*time.Second, "Plan 进入 plan_review 挂起", func() bool {
		plans, err := coord.Store().ListPlans()
		if err != nil || len(plans) != 1 {
			return false
		}
		p := plans[0]
		if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != plan.PauseReasonPlanReview {
			return false
		}
		task, err := s.GetTask(p.ID) // 根 controller 任务 ID == PlanID
		if err != nil || task.Status != model.TaskStatusBlocked {
			return false // 挂起已发生，但 controller 收尾尚未落盘
		}
		paused, rootTask = p, task
		return true
	})
	if paused.Review == nil || paused.Review.Text != planText {
		t.Fatalf("计划文本未持久化: %+v", paused.Review)
	}
	if paused.Review.SubmittedBy != rootTask.ID {
		t.Fatalf("SubmittedBy = %q, want 根 controller %q", paused.Review.SubmittedBy, rootTask.ID)
	}
	// 批准前的关键不变量：DAG 中没有任何执行节点，公告板上只有根 controller。
	if len(paused.CurrentNodeIDs) != 0 {
		t.Fatalf("批准前不应有执行节点: %v", paused.CurrentNodeIDs)
	}
	if tasks, _ := s.ScanAll(); len(tasks) != 1 {
		t.Fatalf("批准前公告板应只有根 controller，实际 %d 个任务", len(tasks))
	}
	// gate=plan 下待批准列表可见该 Plan。
	items, err := listPendingPlanReviews(coord)
	if err != nil || len(items) != 1 || items[0].PlanID != paused.ID {
		t.Fatalf("待批准列表 = %+v, err=%v", items, err)
	}

	// ── 阶段 2：用户批准（/plan approve 的 bootstrap 后端）──
	summary, err := approvePlanReview(context.Background(), s, coord, bundle.Agent.ID, "")
	if err != nil {
		t.Fatalf("approvePlanReview: %v", err)
	}
	if !strings.Contains(summary, "已选择执行") {
		t.Fatalf("执行选择摘要 = %q", summary)
	}
	resumed, err := coord.Store().GetPlan(paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != model.PlanStatusRunning {
		t.Fatalf("批准后 status = %s, want running", resumed.Status)
	}
	resumeTask, err := s.GetTask(resumed.ActiveDecisionTaskID)
	if err != nil {
		t.Fatalf("保留 controller 任务不存在: %v", err)
	}
	if resumeTask.PlanMutationSource != "control-reserved" || !strings.Contains(resumeTask.Description, planText) {
		t.Fatalf("保留 controller 未携带已批准计划: %+v", resumeTask)
	}

	// ── 阶段 3：保留 controller 被认领，按批准的计划派发执行节点 ──
	var execTask *model.Task
	phase3 := func() bool {
		tasks, _ := s.ScanAll()
		for _, task := range tasks {
			if task.Description == "写 hello.go" && task.PlanID == paused.ID {
				execTask = task
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !phase3() {
		time.Sleep(20 * time.Millisecond)
	}
	if execTask == nil {
		dumpPlanGateState(t, s, coord, mockLLM)
		traceCap.mu.Lock()
		for _, ev := range traceCap.events {
			t.Logf("trace %s task=%s agent=%s reason=%q", ev.Kind, ev.TaskID, ev.AgentID, ev.Reason)
		}
		traceCap.mu.Unlock()
		t.Fatal("等待超时: 批准后执行节点注册进 DAG")
	}
	if execTask.NodeRole != model.PlanNodeRoleImplementation {
		t.Fatalf("执行节点角色 = %q, want implementation", execTask.NodeRole)
	}
	latest, err := coord.Store().GetPlan(paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.CurrentNodeIDs) != 1 || latest.CurrentNodeIDs[0] != execTask.ID {
		t.Fatalf("执行节点未注册进 Plan DAG: %+v", latest.CurrentNodeIDs)
	}
	if node := latest.Nodes[execTask.ID]; node.Status == "" {
		t.Fatalf("DAG 节点事实缺失: %+v", node)
	}

	cancel()
	wg.Wait()
}
