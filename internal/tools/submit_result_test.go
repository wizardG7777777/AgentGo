package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

// newSubmitGroup 构造一个已注入提交通道的 PlanControlGroup（runner 装配形态）。
func newSubmitGroup(taskStore store.TaskStore, holder TaskHolder, coordinator *plan.Coordinator) (PlanControlGroup, *fakeFinalizationNotifier, *agent.SubmitState) {
	notifier := &fakeFinalizationNotifier{}
	state := agent.NewSubmitState()
	return PlanControlGroup{
		Coordinator:          coordinator,
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
// 且注册不依赖 Plan 控制面（Coordinator nil 时也应可用——无 Plan 兼容任务）。
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
	g, _, _ := newSubmitGroup(s, holder, nil)
	if !registered(g) {
		t.Error("两个通道都注入时必须注册（即使 Coordinator 为 nil）")
	}
}

// 成功路径（无 Plan 兼容任务）：提交暂存 + finalized 标记 + 中文确认文本。
func TestSubmitTaskResultSuccessNoPlan(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "write report", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID}, nil)

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

// 三类拒绝对象：controller 角色 / scheduler EventType / acceptance 角色。
func TestSubmitTaskResultRejectsControlPlaneRoles(t *testing.T) {
	cases := []struct {
		name    string
		task    *model.Task
		wantErr string
	}{
		{"controller", &model.Task{Description: "ctl", NodeRole: model.PlanNodeRoleController}, "report_done"},
		{"scheduler_event_type", &model.Task{Description: "sched", EventType: "__scheduler__"}, "report_done"},
		{"acceptance", &model.Task{Description: "acc", NodeRole: model.PlanNodeRoleAcceptance}, "submit_acceptance_result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := publishPlainTask(t, s, tc.task)
			g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID}, nil)
			_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "done"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应拒绝并指引 %s，实际 err=%v", tc.wantErr, err)
			}
			if notifier.marked {
				t.Error("拒绝时不应 MarkTaskFinalized")
			}
			if _, ok := state.Take(task.ID); ok {
				t.Error("拒绝时不应暂存提交")
			}
		})
	}
}

func TestSubmitTaskResultRejectsEmptySummary(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID}, nil)
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
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID}, nil)
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

// 无 Plan 任务带 request_replan：没有控制面可登记，跳过 ReplanRequest，不报错。
func TestSubmitTaskResultNoPlanSkipsReplan(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID}, nil)
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成但建议重估", "request_replan": true,
	}); err != nil {
		t.Fatalf("无 Plan 任务带 request_replan 不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("提交仍应生效")
	}
	sub, _ := state.Take(task.ID)
	if sub == nil || !sub.RequestReplan {
		t.Error("RequestReplan 标志应保留在结构化提交里")
	}
}

// 有 Plan 且 blocked_reason 非空：随提交持久化 ReplanRequest
// （SourceEvent=submit_task_result，urgency=high）。
func TestSubmitTaskResultBlockedPersistsReplanRequest(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := publishControllerPlan(t, taskStore, coordinator, "plan", "", model.PlanBudget{})
	worker := &model.Task{Description: "implementation", PlanID: controller.PlanID, NodeRole: model.PlanNodeRoleImplementation}
	if err := taskStore.PublishTask(worker); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.RegisterTask(context.Background(), plan.RegisterTaskInput{
		PlanID: controller.PlanID, ObservedRevision: 0,
		Node: model.PlanNode{TaskID: worker.ID, Title: worker.Description, Role: worker.NodeRole},
	}); err != nil {
		t.Fatal(err)
	}

	g, notifier, _ := newSubmitGroup(taskStore, &fakeHolder{id: worker.ID}, coordinator)
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "数据库迁移缺少权限", "blocked_reason": "无生产库写权限",
	})
	if err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	if !notifier.marked {
		t.Error("提交应生效")
	}
	if !strings.Contains(reply, "ReplanRequest") {
		t.Errorf("确认文本应提及 ReplanRequest 登记，实际：%s", reply)
	}
	p, err := coordinator.Store().GetPlan(controller.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.PendingReplanRequests) != 1 {
		t.Fatalf("PendingReplanRequests = %d，期望 1: %+v", len(p.PendingReplanRequests), p.PendingReplanRequests)
	}
	for _, req := range p.PendingReplanRequests {
		if req.SourceEvent != "submit_task_result" {
			t.Errorf("SourceEvent = %q", req.SourceEvent)
		}
		if req.Urgency != model.ReplanUrgencyHigh {
			t.Errorf("blocked 时 Urgency = %q，期望 high", req.Urgency)
		}
		if req.SourceTaskID != worker.ID {
			t.Errorf("SourceTaskID = %q", req.SourceTaskID)
		}
		if !strings.Contains(req.Detail, "无生产库写权限") {
			t.Errorf("Detail 应包含 blocked_reason，实际：%q", req.Detail)
		}
	}
}
