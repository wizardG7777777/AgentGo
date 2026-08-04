package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// newSubmitGroup 构造一个已注入提交通道的 PlanControlGroup（runner 装配形态）。
func newSubmitGroup(taskStore store.TaskStore, holder TaskHolder) (PlanControlGroup, *fakeFinalizationNotifier, *agent.SubmitState) {
	notifier := &fakeFinalizationNotifier{}
	state := agent.NewSubmitState()
	return PlanControlGroup{
		Store:                taskStore,
		Holder:               holder,
		AgentID:              "worker-1",
		FinalizationNotifier: notifier,
		SubmitState:          state,
	}, notifier, state
}

func publishPlainTask(t *testing.T, s store.TaskStore, task *model.Task) *model.Task {
	t.Helper()
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	return task
}

// submit_task_result 只在 FinalizationNotifier 与 SubmitState 都注入时注册；
// 注册不依赖任何控制面装配（普通 runner 形态也应可用）。
func TestSubmitTaskResultRegisteredOnlyWhenChannelInjected(t *testing.T) {
	registered := func(g PlanControlGroup) bool {
		reg := agent.NewToolRegistry()
		g.Register(reg)
		for _, def := range reg.Defs() {
			if def.Name == "submit_task_result" {
				return true
			}
		}
		return false
	}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	holder := &fakeHolder{id: "t1"}

	if registered(PlanControlGroup{Store: s, Holder: holder}) {
		t.Error("未注入提交通道时不应注册 submit_task_result（scheduler 装配形态）")
	}
	if registered(PlanControlGroup{Store: s, Holder: holder, FinalizationNotifier: &fakeFinalizationNotifier{}}) {
		t.Error("仅注入 FinalizationNotifier 时不应注册")
	}
	if registered(PlanControlGroup{Store: s, Holder: holder, SubmitState: agent.NewSubmitState()}) {
		t.Error("仅注入 SubmitState 时不应注册")
	}
	g, _, _ := newSubmitGroup(s, holder)
	if !registered(g) {
		t.Error("两个通道都注入时必须注册")
	}
}

// 成功路径（普通非图任务）：提交暂存 + finalized 标记 + 中文确认文本。
func TestSubmitTaskResultSuccess(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "write report", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})

	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary":          "报告已写入 report.md",
		"checks_performed": "go build, go test ./...",
		"evidence":         "report.md",
		"remaining_risks":  "覆盖率未测",
	})
	if err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	if !notifier.marked {
		t.Error("成功后必须 MarkTaskFinalized")
	}
	if !strings.Contains(reply, "停止调用其他工具") {
		t.Errorf("确认文本应提醒停止调用其他工具，实际：%s", reply)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Summary != "报告已写入 report.md" {
		t.Errorf("Summary = %q", sub.Summary)
	}
	if len(sub.ChecksPerformed) != 2 || sub.ChecksPerformed[0] != "go build" || sub.ChecksPerformed[1] != "go test ./..." {
		t.Errorf("ChecksPerformed 逗号拆分错误: %v", sub.ChecksPerformed)
	}
	if len(sub.Evidence) != 1 || len(sub.RemainingRisks) != 1 {
		t.Errorf("Evidence/RemainingRisks 拆分错误: %v / %v", sub.Evidence, sub.RemainingRisks)
	}
}

// 拒绝对象（C6b）：__scheduler__ 任务（指引用 report_done）。NodeRole 已随
// Plan 删除，不再按角色拒绝——图节点任务（GraphID 非空）可以正常提交。
func TestSubmitTaskResultRejectsSchedulerTask(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "sched", EventType: "__scheduler__"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "done"})
	if err == nil || !strings.Contains(err.Error(), "report_done") {
		t.Fatalf("scheduler 任务应被拒绝并指引 report_done，实际 err=%v", err)
	}
	if notifier.marked {
		t.Error("拒绝时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("拒绝时不应暂存提交")
	}

	// 图节点任务不是 scheduler 任务：正常接受（验收语义由 verdict/event 承担）。
	graphTask := publishPlainTask(t, s, &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-1", NodeID: "verify", ActivationID: "verify@1",
	})
	g2, notifier2, _ := newSubmitGroup(s, &fakeHolder{id: graphTask.ID})
	if _, err := g2.submitTaskResult(context.Background(), map[string]any{"summary": "验收完成", "verdict": "pass"}); err != nil {
		t.Fatalf("图节点任务应可正常提交: %v", err)
	}
	if !notifier2.marked {
		t.Error("图节点任务成功后必须 MarkTaskFinalized")
	}
}

// event 参数（C5b）：随结构化提交暂存并去空白，供 agent 收尾时写入
// task.Results["event"] 驱动 Graph 事件形态转移条件。
func TestSubmitTaskResultEventParam(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "graph node", EventType: "code", GraphID: "g-1"})
	g, _, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "材料已就绪", "event": " ready ",
	}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Event != "ready" {
		t.Errorf("Event = %q，应去空白为 ready", sub.Event)
	}

	// 省略 event：零值暂存，不写 Results["event"]（agent 收尾路径按空串跳过）。
	task2 := publishPlainTask(t, s, &model.Task{Description: "plain node", EventType: "code"})
	g2, _, state2 := newSubmitGroup(s, &fakeHolder{id: task2.ID})
	if _, err := g2.submitTaskResult(context.Background(), map[string]any{"summary": "完成"}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub2, _ := state2.Take(task2.ID)
	if sub2 == nil || sub2.Event != "" {
		t.Errorf("未传 event 时 Event 应为零值，实际 %+v", sub2)
	}

	// Graph 条件校验只接受固定事件词表；自定义 event 若被暂存，会令所有
	// 合法边都无法命中并把图误置 failed，因此在 finalizing 前拒绝。
	task3 := publishPlainTask(t, s, &model.Task{Description: "graph node", EventType: "code", GraphID: "g-2"})
	g3, notifier3, state3 := newSubmitGroup(s, &fakeHolder{id: task3.ID})
	if _, err := g3.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成", "event": "custom.done",
	}); err == nil || !strings.Contains(err.Error(), "事件词表") {
		t.Fatalf("自定义 Graph event 应在收尾前拒绝，实际 err=%v", err)
	}
	if notifier3.marked {
		t.Error("非法 event 不得进入 finalizing")
	}
	if _, ok := state3.Take(task3.ID); ok {
		t.Error("非法 event 不得写入 SubmitState")
	}
}

// verdict 参数（C5b）：随结构化提交暂存并去空白，供 agent 收尾时写入
// task.Results["verdict"] 驱动 Graph acceptance 节点的路径边条件。
func TestSubmitTaskResultVerdictParam(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "acceptance node", EventType: "code", GraphID: "g-1"})
	g, _, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": " fixable ",
	}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Verdict != "fixable" {
		t.Errorf("Verdict = %q，应去空白为 fixable", sub.Verdict)
	}

	// 省略 verdict：零值暂存，不写 Results["verdict"]。
	task2 := publishPlainTask(t, s, &model.Task{Description: "plain node", EventType: "code"})
	g2, _, state2 := newSubmitGroup(s, &fakeHolder{id: task2.ID})
	if _, err := g2.submitTaskResult(context.Background(), map[string]any{"summary": "完成"}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub2, _ := state2.Take(task2.ID)
	if sub2 == nil || sub2.Verdict != "" {
		t.Errorf("未传 verdict 时 Verdict 应为零值，实际 %+v", sub2)
	}
}

func TestSubmitTaskResultRejectsEmptySummary(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "  "}); err == nil {
		t.Fatal("空白 summary 应报错")
	}
	if _, err := g.submitTaskResult(context.Background(), map[string]any{}); err == nil {
		t.Fatal("缺 summary 应报错")
	}
	if notifier.marked {
		t.Error("summary 为空时不应 MarkTaskFinalized")
	}
}

// ExpectedArtifacts 缺失：返回含失败原因的校验错误，且不标记 finalized——
// LLM 应在本轮循环内补写文件后重试。
func TestSubmitTaskResultRejectsMissingExpectedArtifacts(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{
		Description: "d", EventType: "code", ExpectedArtifacts: []string{"out.md"},
	})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "假装完成"})
	if err == nil {
		t.Fatal("缺失 expected_artifacts 应拒绝")
	}
	if !strings.Contains(err.Error(), "缺失的预期文件") || !strings.Contains(err.Error(), "out.md") {
		t.Errorf("错误应包含 BuildArtifactFailureReason 文本，实际：%v", err)
	}
	if notifier.marked {
		t.Error("校验失败时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("校验失败时不应暂存提交")
	}
}

// 非图任务带 request_replan=true（C6b）：提交生效的同时附带发布通用 replan
// 唤醒任务（与 request_replan 工具同机制，幂等键 <taskID>/replan），确认文本
// 附带登记提示。
func TestSubmitTaskResultNonGraphRequestReplanPublishesWake(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成但建议重估", "request_replan": true,
	})
	if err != nil {
		t.Fatalf("非图任务带 request_replan 不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("提交仍应生效")
	}
	sub, _ := state.Take(task.ID)
	if sub == nil || !sub.RequestReplan {
		t.Error("RequestReplan 标志应保留在结构化提交里")
	}
	if !strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("确认文本应附带 replan 登记提示，实际：%s", reply)
	}
	wakes := findReplanWake(t, s, replanRequestMarker(task.ID))
	if len(wakes) != 1 {
		t.Fatalf("应附带发布 1 个 replan 唤醒任务，实际 %d", len(wakes))
	}
	wake := wakes[0]
	if wake.EventSource != "replan-request" || wake.ParentTaskID != task.ID || wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务路由元数据错误: %+v", wake)
	}
	if !strings.Contains(wake.Description, "submit_request_replan") {
		t.Errorf("唤醒任务描述应含 reason_code=submit_request_replan，实际：%s", wake.Description)
	}
}

// 非图任务带 blocked_reason（C6b）：随提交发布高优 replan 唤醒任务
// （reason_code=submit_blocked，urgency=high，详情含 blocked_reason）。
func TestSubmitTaskResultBlockedPublishesHighUrgencyWake(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "implementation", EventType: "code"})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "数据库迁移缺少权限", "blocked_reason": "无生产库写权限",
	})
	if err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	if !notifier.marked {
		t.Error("提交应生效")
	}
	if !strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("确认文本应附带 replan 登记提示，实际：%s", reply)
	}
	wakes := findReplanWake(t, s, replanRequestMarker(task.ID))
	if len(wakes) != 1 {
		t.Fatalf("应附带发布 1 个 replan 唤醒任务，实际 %d", len(wakes))
	}
	desc := wakes[0].Description
	for _, want := range []string{"submit_blocked", "high", "无生产库写权限", "数据库迁移缺少权限"} {
		if !strings.Contains(desc, want) {
			t.Errorf("唤醒任务描述缺少 %q，实际：%s", want, desc)
		}
	}
}

// 图任务带 blocked_reason/request_replan（C6b）：不发布 replan 唤醒任务——
// 图路由由 graph-terminal-feed 终态回填驱动，提交本身照常生效。
func TestSubmitTaskResultGraphTaskSkipsReplanWake(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-1", NodeID: "implement", ActivationID: "implement@1",
	})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "依赖缺失", "blocked_reason": "上游产物未就绪", "request_replan": true,
	})
	if err != nil {
		t.Fatalf("图任务提交不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("图任务提交仍应生效")
	}
	if strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("图任务不应附带 replan 登记提示，实际：%s", reply)
	}
	if wakes := findReplanWake(t, s, "[replan-request:"); len(wakes) != 0 {
		t.Errorf("图任务不应发布通用 replan 唤醒任务，实际 %d 个", len(wakes))
	}
	if wakes := findGraphChangeWake(t, s, "[graph-change-request:"); len(wakes) != 0 {
		t.Errorf("图任务提交不应发布 graph change 唤醒任务，实际 %d 个", len(wakes))
	}
}

// status=blocked（V6 §5）：结构化 blocked 提交被接受——Status 随提交暂存、
// finalized 标记、确认文本指向 blocked 终态收尾；工具层不再附带发布 replan
// 唤醒任务（终态落盘后的唤醒由 agent 收尾路径负责），并 emit task_finalizing。
func TestSubmitTaskResultStatusBlockedAccepted(t *testing.T) {
	d := installShellTraceCapture(t)
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "数据库迁移", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "数据库迁移缺少权限", "blocked_reason": "无生产库写权限", "status": "blocked",
	})
	if err != nil {
		t.Fatalf("status=blocked 提交不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("提交应生效（MarkTaskFinalized）")
	}
	sub, ok := state.Take(task.ID)
	if !ok || sub == nil {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Status != "blocked" || sub.BlockedReason != "无生产库写权限" {
		t.Errorf("Status/BlockedReason = %q/%q，期望 blocked/无生产库写权限", sub.Status, sub.BlockedReason)
	}
	if !strings.Contains(reply, "blocked 终态") {
		t.Errorf("确认文本应指向 blocked 终态收尾，实际：%s", reply)
	}
	// 工具层不附带唤醒任务：终态落盘后的 replan 唤醒由 agent 收尾路径负责。
	if wakes := findReplanWake(t, s, replanRequestMarker(task.ID)); len(wakes) != 0 {
		t.Errorf("status=blocked 时工具层不应附带发布 replan 唤醒任务，实际 %d 个", len(wakes))
	}
	// task_finalizing：自述终态 blocked。
	var found *trace.Event
	for i, ev := range d.events {
		if ev.Kind == trace.KindTaskFinalizing && ev.TaskID == task.ID {
			found = &d.events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("应 emit task_finalizing 事件")
	}
	if found.Transition == nil || found.Transition.NewStatus != "blocked" {
		t.Errorf("task_finalizing 应携带自述终态 blocked，实际 %+v", found.Transition)
	}
	if found.AgentID != "worker-1" {
		t.Errorf("task_finalizing.AgentID = %q，期望 worker-1", found.AgentID)
	}
}

// status=blocked 缺 blocked_reason：拒绝且不标记 finalized。
func TestSubmitTaskResultStatusBlockedRequiresReason(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "无法完成", "status": "blocked",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked_reason") {
		t.Fatalf("status=blocked 缺 blocked_reason 应报错，实际 err=%v", err)
	}
	if notifier.marked {
		t.Error("校验失败时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("校验失败时不应暂存提交")
	}
}

// status 只接受 completed/blocked：failed、cancelled（系统路径专属）与
// 未知值一律拒绝。
func TestSubmitTaskResultStatusRejectsInvalidValues(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	for _, bad := range []string{"failed", "cancelled", "done"} {
		task := publishPlainTask(t, s, &model.Task{Description: "d-" + bad, EventType: "code"})
		g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
		_, err := g.submitTaskResult(context.Background(), map[string]any{
			"summary": "s", "status": bad,
		})
		if err == nil || !strings.Contains(err.Error(), "completed / blocked") {
			t.Errorf("status=%q 应被拒绝并提示合法值，实际 err=%v", bad, err)
		}
		if notifier.marked {
			t.Errorf("status=%q 被拒绝时不应 MarkTaskFinalized", bad)
		}
	}
}

// status 缺省 = completed：行为与现网一致（暂存 Status=completed、正常发布
// request_replan 唤醒任务），task_finalizing 携带 completed。
func TestSubmitTaskResultDefaultStatusCompleted(t *testing.T) {
	d := installShellTraceCapture(t)
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, _, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成但建议重估", "request_replan": true,
	}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub, _ := state.Take(task.ID)
	if sub == nil || sub.Status != "completed" {
		t.Errorf("缺省 status 应归一为 completed，实际 %+v", sub)
	}
	if wakes := findReplanWake(t, s, replanRequestMarker(task.ID)); len(wakes) != 1 {
		t.Errorf("completed+request_replan 应照旧附带唤醒任务，实际 %d 个", len(wakes))
	}
	var found *trace.Event
	for i, ev := range d.events {
		if ev.Kind == trace.KindTaskFinalizing && ev.TaskID == task.ID {
			found = &d.events[i]
			break
		}
	}
	if found == nil || found.Transition == nil || found.Transition.NewStatus != "completed" {
		t.Errorf("task_finalizing 应携带自述终态 completed，实际 %+v", found)
	}
}

// 唯一终态提交者：已 finalized 后再次调用 submit_task_result 返回中文错误，
// 不重复提交、不改变已暂存的首次提交。使用真实 FinalizationHolder（实现
// IsFinalized）；旧式 fake notifier 不实现该接口时守卫退化为不检查。
func TestSubmitTaskResultRejectsDuplicateAfterFinalized(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	finHolder := agent.NewFinalizationHolder()
	finHolder.Set(task.ID)
	state := agent.NewSubmitState()
	g := PlanControlGroup{
		Store: s, Holder: &fakeHolder{id: task.ID}, AgentID: "worker-1",
		FinalizationNotifier: finHolder, SubmitState: state,
	}
	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "首次提交"}); err != nil {
		t.Fatalf("首次提交应成功: %v", err)
	}
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "重复提交"})
	if err == nil || !strings.Contains(err.Error(), "只能成功提交一次") {
		t.Fatalf("重复提交应返回中文错误，实际 err=%v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok || sub.Summary != "首次提交" {
		t.Errorf("首次提交不应被覆盖，实际 %+v (ok=%t)", sub, ok)
	}
}

// evidence_items（G1b）：合法 JSON 数组原样暂存（经 StructuredSubmission
// 写入 Results["evidence"] 供图侧服务端核验）；非法 JSON 在提交边界拒绝
// ——任务保持未 finalized，agent 本轮内可修正后重新提交。
func TestSubmitTaskResultEvidenceItems(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "acceptance", EventType: "acceptance.verify", GraphID: "g-1"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})

	// 非法 JSON：拒绝、不标记 finalized、不暂存。
	_, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": "pass", "evidence_items": "{not an array",
	})
	if err == nil || !strings.Contains(err.Error(), "evidence_items") {
		t.Fatalf("非法 evidence_items 应被拒绝并点名参数，实际 err=%v", err)
	}
	if notifier.marked {
		t.Error("拒绝时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("拒绝时不应暂存提交")
	}

	// 合法 JSON 数组：接受并原样暂存（首尾空白已规范化）。
	valid := `  [{"criterion":"测试通过","type":"command","value":"go test ./..."}]  `
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": "pass", "evidence_items": valid,
	}); err != nil {
		t.Fatalf("合法 evidence_items 应被接受: %v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.EvidenceItems != strings.TrimSpace(valid) {
		t.Errorf("EvidenceItems 应原样保留 JSON 数组，实际 %q", sub.EvidenceItems)
	}
}
