package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// captureGraphTraceDispatcher 收集 Emit 的 trace 事件，供图路径测试按 kind 断言。
type captureGraphTraceDispatcher struct{ events []trace.Event }

func (d *captureGraphTraceDispatcher) Dispatch(ev trace.Event) { d.events = append(d.events, ev) }

func (d *captureGraphTraceDispatcher) ofKind(kind trace.EventKind) []trace.Event {
	var out []trace.Event
	for _, ev := range d.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// installGraphTraceCapture 挂载事件捕获 dispatcher，t.Cleanup 恢复原值。
func installGraphTraceCapture(t *testing.T) *captureGraphTraceDispatcher {
	t.Helper()
	d := &captureGraphTraceDispatcher{}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(d)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })
	return d
}

// findGraphChangeWake 在公告板上找 graph change 唤醒任务（按幂等标记匹配）。
func findGraphChangeWake(t *testing.T, s store.TaskStore, marker string) []*model.Task {
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

func newGraphReplanGroup(t *testing.T) (PlanControlGroup, store.TaskStore, *model.Task) {
	t.Helper()
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	graphTask := &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-1", NodeID: "implement", ActivationID: "implement@1",
	}
	if err := taskStore.PublishTask(graphTask); err != nil {
		t.Fatal(err)
	}
	group := PlanControlGroup{Store: taskStore, Holder: &fakeHolder{id: graphTask.ID}}
	return group, taskStore, graphTask
}

// 图任务 request_replan（C5d）：emit graph_change_requested 事件 + 发布
// __scheduler__ 唤醒任务（含图/节点/activation 上下文与 patch_graph 指引），
// 唤醒任务自身不携带图身份（避免被当作图节点终态回填）。
func TestRequestReplanGraphTaskPublishesSchedulerWake(t *testing.T) {
	group, taskStore, graphTask := newGraphReplanGroup(t)
	d := installGraphTraceCapture(t)

	reply, err := group.requestReplan(context.Background(), map[string]any{
		"reason_code": "route_missing", "urgency": "high", "detail": "verify 节点无可用路由",
	})
	if err != nil {
		t.Fatalf("图任务 request_replan 不应报错: %v", err)
	}
	if !strings.Contains(reply, "已登记") || !strings.Contains(reply, "g-1") || !strings.Contains(reply, "implement@1") {
		t.Errorf("返回值应含登记确认与图上下文，实际：%s", reply)
	}

	// 唤醒任务落板：__scheduler__ 路由、graph-change-request 来源、幂等标记、
	// 图上下文与 patch_graph 指引。
	marker := graphChangeMarker("g-1/implement@1/change")
	wakes := findGraphChangeWake(t, taskStore, marker)
	if len(wakes) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，实际 %d", len(wakes))
	}
	wake := wakes[0]
	if wake.Status != model.TaskStatusPending {
		t.Errorf("唤醒任务应为 pending，实际 %s", wake.Status)
	}
	if wake.EventSource != "graph-change-request" {
		t.Errorf("唤醒任务 EventSource 应为 graph-change-request，实际 %q", wake.EventSource)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不得携带图身份（会被当作图节点回填）: %+v", wake)
	}
	for _, want := range []string{"g-1", "implement", "implement@1", "route_missing", "high", "verify 节点无可用路由", graphTask.ID, "patch_graph", "base_revision"} {
		if !strings.Contains(wake.Description, want) {
			t.Errorf("唤醒任务描述缺少 %q，实际：%s", want, wake.Description)
		}
	}
	if wake.ParentTaskID != graphTask.ID {
		t.Errorf("唤醒任务应以请求者任务为 parent，实际 %q", wake.ParentTaskID)
	}
	if wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务 MaxConcurrency 应为 1，实际 %d", wake.MaxConcurrency)
	}

	// graph_change_requested 事件：携带图/节点/activation 与请求者 task_id。
	events := d.ofKind(trace.KindGraphChangeRequested)
	if len(events) != 1 {
		t.Fatalf("应 emit 1 条 graph_change_requested，实际 %d", len(events))
	}
	ev := events[0]
	if ev.TaskID != graphTask.ID || ev.GraphID != "g-1" || ev.NodeID != "implement" ||
		ev.ActivationID != "implement@1" || ev.Reason != "route_missing" {
		t.Errorf("graph_change_requested 载荷不符: %+v", ev)
	}
}

// 幂等（C5d）：同一 activation 已有未处理唤醒任务时重复请求不刷屏；不同
// activation 各自独立；旧唤醒任务终态（已处理）后允许再次登记。
func TestRequestReplanGraphTaskIdempotent(t *testing.T) {
	group, taskStore, _ := newGraphReplanGroup(t)
	d := installGraphTraceCapture(t)
	args := map[string]any{"reason_code": "worker_blocked", "detail": "依赖缺失"}
	marker := graphChangeMarker("g-1/implement@1/change")

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
	if wakes := findGraphChangeWake(t, taskStore, marker); len(wakes) != 1 {
		t.Fatalf("重复请求不应新增唤醒任务，实际 %d 个", len(wakes))
	}
	if n := len(d.ofKind(trace.KindGraphChangeRequested)); n != 1 {
		t.Errorf("被抑制的重复请求不应再 emit 事件，实际 %d 条", n)
	}

	// 不同 activation（回边重进）是另一类请求，独立登记。
	graphTask2 := &model.Task{
		Description: "graph node retry", EventType: "code",
		GraphID: "g-1", NodeID: "implement", ActivationID: "implement@2",
	}
	if err := taskStore.PublishTask(graphTask2); err != nil {
		t.Fatal(err)
	}
	group2 := PlanControlGroup{Store: taskStore, Holder: &fakeHolder{id: graphTask2.ID}}
	if _, err := group2.requestReplan(context.Background(), args); err != nil {
		t.Fatalf("新 activation 的请求: %v", err)
	}
	if wakes := findGraphChangeWake(t, taskStore, graphChangeMarker("g-1/implement@2/change")); len(wakes) != 1 {
		t.Fatalf("新 activation 应独立登记唤醒任务，实际 %d 个", len(wakes))
	}

	// 旧唤醒任务终态（Scheduler 已处理）后，同一 activation 允许再次登记。
	wakes := findGraphChangeWake(t, taskStore, marker)
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
	if wakes := findGraphChangeWake(t, taskStore, marker); len(wakes) != 2 {
		t.Fatalf("旧唤醒终态后应发布新唤醒任务，实际共 %d 个", len(wakes))
	}
}

// 双路径分流：GraphID 非空走 graph change 唤醒，为空走通用 replan 唤醒；
// 两条路径互不发布对方的唤醒任务、互不 emit 对方的事件。
func TestRequestReplanRoutesByGraphIdentity(t *testing.T) {
	group, taskStore, _ := newGraphReplanGroup(t)
	d := installGraphTraceCapture(t)
	if _, err := group.requestReplan(context.Background(), map[string]any{"reason_code": "route_missing"}); err != nil {
		t.Fatalf("图任务请求: %v", err)
	}
	if wakes := findReplanWake(t, taskStore, "[replan-request:"); len(wakes) != 0 {
		t.Errorf("图路径不应发布通用 replan 唤醒任务，实际 %d 个", len(wakes))
	}

	plainTask := &model.Task{Description: "plain", EventType: "code"}
	if err := taskStore.PublishTask(plainTask); err != nil {
		t.Fatal(err)
	}
	plainGroup := PlanControlGroup{Store: taskStore, Holder: &fakeHolder{id: plainTask.ID}}
	if _, err := plainGroup.requestReplan(context.Background(), map[string]any{"reason_code": "worker_observation"}); err != nil {
		t.Fatalf("非图任务请求: %v", err)
	}
	if wakes := findGraphChangeWake(t, taskStore, "[graph-change-request:"); len(wakes) != 1 {
		t.Errorf("非图路径不应新增 graph change 唤醒任务，实际共 %d 个", len(wakes))
	}
	if n := len(d.ofKind(trace.KindGraphChangeRequested)); n != 1 {
		t.Errorf("非图路径不应再 emit graph_change_requested，实际 %d 条", n)
	}
	if wakes := findReplanWake(t, taskStore, replanRequestMarker(plainTask.ID)); len(wakes) != 1 {
		t.Errorf("非图路径应发布通用 replan 唤醒任务，实际 %d 个", len(wakes))
	}
}
