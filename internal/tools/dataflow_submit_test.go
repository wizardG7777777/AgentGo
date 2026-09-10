package tools

import (
	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

type dataflowTestResultValidator struct{ reject bool }

func (v dataflowTestResultValidator) ValidateAgentTaskResult(_, _ string, value map[string]any) error {
	if v.reject {
		return fmt.Errorf("结果不满足声明")
	}
	if value["summary"] == nil {
		return fmt.Errorf("缺少 summary")
	}
	return nil
}

type dataflowTestPlanning struct{ calls int }

func (p *dataflowTestPlanning) RequestPlanning(_, _, _, _ string) error { p.calls++; return nil }

func newDataflowSubmitFixture(t *testing.T) (PlanControlGroup, *fakeFinalizationNotifier, *agent.SubmitState, *model.Task) {
	t.Helper()
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{Description: "独立任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	task, _ = s.GetTask(task.ID)
	notifier := &fakeFinalizationNotifier{}
	state := agent.NewSubmitState()
	return PlanControlGroup{Store: s, Holder: &fakeHolder{id: task.ID}, AgentID: "worker", FinalizationNotifier: notifier, SubmitState: state}, notifier, state, task
}

func TestDataflowSubmitPreservesTypedResultAndFences(t *testing.T) {
	g, n, state, task := newDataflowSubmitFixture(t)
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "处理完成", "result": map[string]any{"status": "业务字段", "measure": 3.5, "ready": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !n.marked {
		t.Fatal("没有进入 finalizing")
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("缺少提交")
	}
	var result map[string]any
	_ = json.Unmarshal([]byte(sub.ResultJSON), &result)
	if result["measure"] != 3.5 || result["ready"] != true || result["status"] != "业务字段" {
		t.Fatal("结果类型被改写")
	}
	if _, err = g.submitTaskResult(context.Background(), map[string]any{"summary": "再次提交"}); err == nil {
		t.Fatal("不能再次提交")
	}
}
func TestDataflowSubmitRejectsRetiredArguments(t *testing.T) {
	for _, name := range []string{"event", "verdict", "cited_evidence", "request_replan"} {
		t.Run(name, func(t *testing.T) {
			g, n, _, _ := newDataflowSubmitFixture(t)
			if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "完成", name: "pass"}); err == nil || n.marked {
				t.Fatal("旧控制参数不应兼容")
			}
		})
	}
}
func TestDataflowBlockedRequiresReason(t *testing.T) {
	g, n, _, _ := newDataflowSubmitFixture(t)
	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "无法完成", "status": "blocked"}); err == nil || n.marked {
		t.Fatal("无原因 blocked 应拒绝")
	}
	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "无法完成", "status": "blocked", "blocked_reason": "等待外部输入"}); err != nil {
		t.Fatal(err)
	}
}
func TestDataflowResultToolDirectoryHasNoVerdict(t *testing.T) {
	g, _, _, _ := newDataflowSubmitFixture(t)
	reg := agent.NewToolRegistry()
	g.Register(reg)
	if len(reg.Names()) != 2 {
		t.Fatal(reg.Names())
	}
	for _, d := range reg.Defs() {
		if d.Name == "submit_task_result" {
			props := d.Parameters["properties"].(map[string]any)
			if props["verdict"] != nil || props["event"] != nil {
				t.Fatal("模型仍看见旧结果路由参数")
			}
		}
	}
}
