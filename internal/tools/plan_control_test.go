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

// findReplanWake 在公告板上找通用 replan 唤醒任务（按幂等标记匹配）。
func findReplanWake(t *testing.T, s store.TaskStore, marker string) []*model.Task {
	t.Helper()
	all, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	var wakes []*model.Task
	for _, task := range all {
		if task.EventType == "__scheduler__" && strings.Contains(task.Description, marker) {
			wakes = append(wakes, task)
		}
	}
	return wakes
}

func newNonGraphReplanGroup(t *testing.T) (PlanControlGroup, store.TaskStore, *model.Task) {
	t.Helper()
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := &model.Task{Description: "implementation", EventType: "code"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	group := PlanControlGroup{Store: taskStore, Holder: &fakeHolder{id: task.ID}}
	return group, taskStore, task
}

// 注册语义（C6b）：request_replan 在 Store+Holder 非 nil 时恒注册；
// submit_task_result 只在提交通道（FinalizationNotifier+SubmitState）注入时注册；
// Store/Holder 缺一整个组不注册。
func TestPlanControlGroupRegisterRules(t *testing.T) {
	registered := func(g PlanControlGroup) map[string]bool {
		reg := agent.NewToolRegistry()
		g.Register(reg)
		names := map[string]bool{}
		for _, def := range reg.Defs() {
			names[def.Name] = true
		}
		return names
	}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	holder := &fakeHolder{id: "t1"}

	names := registered(PlanControlGroup{Store: s, Holder: holder})
	if !names["request_replan"] {
		t.Error("Store+Holder 注入时 request_replan 必须恒注册")
	}
	if names["submit_task_result"] {
		t.Error("未注入提交通道时不应注册 submit_task_result（不完整装配）")
	}
	if got := registered(PlanControlGroup{Holder: holder}); len(got) != 0 {
		t.Errorf("Store 为 nil 时整个组不注册，实际 %v", got)
	}
	if got := registered(PlanControlGroup{Store: s}); len(got) != 0 {
		t.Errorf("Holder 为 nil 时整个组不注册，实际 %v", got)
	}
}

// 非图任务 request_replan（C6b 通用 replan 路径）：不触碰任何服务端控制面
// 状态，只发布 __scheduler__ 唤醒任务（描述含幂等标记、reason_code/urgency/
// detail 与处理指引，其 task_published 事件即审计事实），唤醒任务自身不携带图身份。
func TestRequestReplanNonGraphTaskPublishesGenericWake(t *testing.T) {
	group, taskStore, task := newNonGraphReplanGroup(t)
	d := installGraphTraceCapture(t)

	reply, err := group.requestReplan(context.Background(), map[string]any{
		"reason_code": "worker_blocked", "urgency": "high", "detail": "依赖缺失",
	})
	if err != nil {
		t.Fatalf("非图任务 request_replan 不应报错: %v", err)
	}
	if !strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("返回值应含登记确认，实际：%s", reply)
	}

	// 唤醒任务落板：__scheduler__ 路由、replan-request 来源、幂等标记、
	// 请求上下文与处理指引；parent 指向请求者；同一时刻只允许一个 Scheduler 处理。
	marker := replanRequestMarker(task.ID)
	if !strings.Contains(marker, task.ID+"/replan") {
		t.Fatalf("幂等标记形态错误：%s", marker)
	}
	wakes := findReplanWake(t, taskStore, marker)
	if len(wakes) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，实际 %d", len(wakes))
	}
	wake := wakes[0]
	if wake.Status != model.TaskStatusPending {
		t.Errorf("唤醒任务应为 pending，实际 %s", wake.Status)
	}
	if wake.EventSource != "replan-request" {
		t.Errorf("唤醒任务 EventSource 应为 replan-request，实际 %q", wake.EventSource)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不得携带图身份（会被当作图节点回填）: %+v", wake)
	}
	for _, want := range []string{"worker_blocked", "high", "依赖缺失", task.ID, "code", "处理指引"} {
		if !strings.Contains(wake.Description, want) {
			t.Errorf("唤醒任务描述缺少 %q，实际：%s", want, wake.Description)
		}
	}
	if wake.ParentTaskID != task.ID {
		t.Errorf("唤醒任务应以请求者任务为 parent，实际 %q", wake.ParentTaskID)
	}
	if wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务 MaxConcurrency 应为 1，实际 %d", wake.MaxConcurrency)
	}

	// 审计事实：唤醒任务的 task_published 事件携请求者上下文（C6c 起
	// 不再单独 emit replan 时代事件）。
	published := d.ofKind(trace.KindTaskPublished)
	found := false
	for _, ev := range published {
		if ev.TaskID == wake.ID && strings.Contains(ev.Description, "worker_blocked") {
			found = true
		}
	}
	if !found {
		t.Errorf("唤醒任务的 task_published 事件未携请求上下文: %+v", published)
	}
}

// 幂等：同一任务已有未处理（非终态）唤醒任务时重复请求不刷屏；旧唤醒任务
// 终态（Scheduler 已处理）后允许再次登记。
func TestRequestReplanNonGraphTaskIdempotent(t *testing.T) {
	group, taskStore, task := newNonGraphReplanGroup(t)
	d := installGraphTraceCapture(t)
	args := map[string]any{"reason_code": "worker_blocked", "detail": "依赖缺失"}
	marker := replanRequestMarker(task.ID)

	if _, err := group.requestReplan(context.Background(), args); err != nil {
		t.Fatalf("首次请求: %v", err)
	}
	reply, err := group.requestReplan(context.Background(), args)
	if err != nil {
		t.Fatalf("重复请求不应报错: %v", err)
	}
	if !strings.Contains(reply, "无需重复") {
		t.Errorf("重复请求应返回幂等提示，实际：%s", reply)
	}
	if wakes := findReplanWake(t, taskStore, marker); len(wakes) != 1 {
		t.Fatalf("重复请求不应新增唤醒任务，实际 %d 个", len(wakes))
	}
	if n := len(d.ofKind(trace.KindTaskPublished)); n != 1 {
		t.Errorf("被抑制的重复请求不应再发布任务（仅唤醒任务一条），实际 %d 条", n)
	}

	// 旧唤醒任务终态（Scheduler 已处理）后，同一任务允许再次登记。
	wakes := findReplanWake(t, taskStore, marker)
	if err := store.TransitionStateWithCancelSource(taskStore, wakes[0].ID, model.TaskStatusPending, model.TaskStatusCancelled, "test"); err != nil {
		t.Fatalf("终态化旧唤醒任务: %v", err)
	}
	reply, err = group.requestReplan(context.Background(), args)
	if err != nil {
		t.Fatalf("旧唤醒终态后的再次请求: %v", err)
	}
	if strings.Contains(reply, "无需重复") {
		t.Errorf("旧唤醒任务已终态，不应再幂等抑制，实际：%s", reply)
	}
	if wakes := findReplanWake(t, taskStore, marker); len(wakes) != 2 {
		t.Fatalf("旧唤醒终态后应发布新唤醒任务，实际共 %d 个", len(wakes))
	}
}

// 无当前任务上下文时 request_replan 直接报错。
func TestRequestReplanNoTaskContext(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	group := PlanControlGroup{Store: taskStore, Holder: &fakeHolder{id: ""}}
	if _, err := group.requestReplan(context.Background(), map[string]any{"reason_code": "x"}); err == nil ||
		!strings.Contains(err.Error(), "no current task context") {
		t.Fatalf("空任务上下文应报错，实际: %v", err)
	}
}
