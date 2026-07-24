package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

func TestStructuredSubmissionFormat(t *testing.T) {
	sub := &StructuredSubmission{
		TaskID:          "t1",
		Summary:         "写入了 report.md",
		ChecksPerformed: []string{"go build", "go test ./internal/..."},
		Evidence:        []string{"report.md"},
	}
	out := sub.Format()
	if !strings.HasPrefix(out, "## 任务结果摘要\n\n写入了 report.md") {
		t.Errorf("Format 应以 summary 开头，实际：\n%s", out)
	}
	for _, want := range []string{"## 已执行的检查", "- go build", "- go test ./internal/...", "## 证据", "- report.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format 缺少 %q，实际：\n%s", want, out)
		}
	}
	// 空节必须省略
	for _, unwanted := range []string{"## 残余风险", "## 阻塞原因", "## 重规划请求"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("空节 %q 不应出现，实际：\n%s", unwanted, out)
		}
	}

	blocked := &StructuredSubmission{TaskID: "t2", Summary: "卡在权限", BlockedReason: "无权访问 X", RequestReplan: true}
	out = blocked.Format()
	if !strings.Contains(out, "## 阻塞原因\n\n无权访问 X") || !strings.Contains(out, "## 重规划请求") {
		t.Errorf("blocked/replan 分节缺失，实际：\n%s", out)
	}
}

func TestSubmitStatePutTake(t *testing.T) {
	s := NewSubmitState()
	if _, ok := s.Take("t1"); ok {
		t.Fatal("空 SubmitState Take 应返回 false")
	}
	s.Put(&StructuredSubmission{TaskID: "t1", Summary: "v1"})
	// 同任务重复 Put 以最新一次为准
	s.Put(&StructuredSubmission{TaskID: "t1", Summary: "v2"})
	s.Put(&StructuredSubmission{TaskID: "t2", Summary: "other"})
	sub, ok := s.Take("t1")
	if !ok || sub.Summary != "v2" {
		t.Fatalf("Take(t1) = %+v, %t；期望 v2", sub, ok)
	}
	// Take 即取即删
	if _, ok := s.Take("t1"); ok {
		t.Fatal("Take 后应已删除")
	}
	// 其他任务不受影响
	if _, ok := s.Take("t2"); !ok {
		t.Fatal("Take(t1) 不应影响 t2")
	}
	// nil / 空 TaskID 防御
	s.Put(nil)
	s.Put(&StructuredSubmission{Summary: "no task"})
}

// submit_task_result 路径：finalization 短路分支消费 SubmitState，
// 渲染文本成为 SubmitResult / LastResponse 权威负载，TransferNote=summary，
// Transition.Cause=submit_task_result。
func TestFinalizationShortCircuitConsumesSubmitState(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "structured submit", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	const agentID = "agent-sub"
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	state := NewSubmitState()
	executor := func(_ context.Context, task *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		// 模拟 submit_task_result 工具：校验通过后 Put 结构化提交；
		// finalized 标志由 flipFinalizationChecker 在下一轮 loop 顶部提供。
		state.Put(&StructuredSubmission{
			TaskID:          task.ID,
			Summary:         "写入了 report.md 并通过测试",
			ChecksPerformed: []string{"go build", "go test ./..."},
			Evidence:        []string{"report.md"},
			RemainingRisks:  []string{"覆盖率未达标"},
		})
		return ExecuteResult{Output: "progress", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, executor, 5)
	ag.FinalizationChecker = &flipFinalizationChecker{}
	ag.SubmitState = state
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("任务状态 = %s，期望 completed", got.Status)
	}
	result := got.Results[agentID]
	for _, want := range []string{"## 任务结果摘要", "写入了 report.md 并通过测试", "- go build", "- report.md", "- 覆盖率未达标"} {
		if !strings.Contains(result, want) {
			t.Errorf("SubmitResult 负载缺少 %q，实际：\n%s", want, result)
		}
	}
	if got.TransferNote != "写入了 report.md 并通过测试" {
		t.Errorf("TransferNote = %q，期望 summary", got.TransferNote)
	}
	if got.LastResponse != result {
		t.Errorf("LastResponse 应等于渲染后的权威结果文本")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("结构化提交应已被短路分支消费（Take 即取即删）")
	}

	events := p1fixesReadTraceEvents(t, traceDir)
	var cause string
	for _, ev := range events {
		if ev.Kind == trace.KindTaskCompleted && ev.TaskID == task.ID && ev.Transition != nil {
			cause = ev.Transition.Cause
			break
		}
	}
	if cause != "submit_task_result" {
		t.Errorf("Transition.Cause = %q，期望 submit_task_result（事件：%s）", cause, eventKinds(events))
	}
}

// 兼容路径：SubmitState 已装配但无暂存提交时，短路分支行为与 report_done 时代一致
// （lastOutput 收尾、Cause=finalization_short_circuit、不写 LastResponse）。
func TestFinalizationShortCircuitWithoutSubmissionKeepsCompatBehavior(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "compat short circuit", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	const agentID = "agent-compat"
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "progress output", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, executor, 5)
	ag.FinalizationChecker = &flipFinalizationChecker{}
	ag.SubmitState = NewSubmitState() // 已装配但为空
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Results[agentID] != "progress output" {
		t.Errorf("SubmitResult = %q，期望 lastOutput 原文", got.Results[agentID])
	}
	if got.TransferNote != "progress output" {
		t.Errorf("TransferNote = %q，期望 lastOutput 原文", got.TransferNote)
	}
	if got.LastResponse != "" {
		t.Errorf("兼容路径不应写 LastResponse，实际 %q", got.LastResponse)
	}
	events := p1fixesReadTraceEvents(t, traceDir)
	var cause string
	for _, ev := range events {
		if ev.Kind == trace.KindTaskCompleted && ev.TaskID == task.ID && ev.Transition != nil {
			cause = ev.Transition.Cause
			break
		}
	}
	if cause != "finalization_short_circuit" {
		t.Errorf("Transition.Cause = %q，期望 finalization_short_circuit", cause)
	}
}
