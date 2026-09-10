package agent

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
	"context"
	"errors"
	"strings"
	"testing"
)

type failingResultFieldsStore struct {
	store.TaskStore
	err error
}

func (s *failingResultFieldsStore) SubmitResultWithFields(string, string, string, map[ // 空节必须省略
// 同任务重复 Put 以最新一次为准
// Take 即取即删
// 其他任务不受影响
// nil / 空 TaskID 防御
// submit_task_result 路径：finalization 短路分支消费 SubmitState，
// 渲染文本成为 SubmitResult / LastResponse 权威负载，
// Transition.Cause=submit_task_result。
// 模拟 submit_task_result 工具：校验通过后 Put 结构化提交；
// finalized 标志由 flipFinalizationChecker 在下一轮 loop 顶部提供。
// 自定义 result object 在 finalization 短路时以单一内部 carrier 落盘；
// Task.Results 仍保持 map[string]string，Graph bridge 负责类型保真展开。
// 兼容路径：SubmitState 已装配但无暂存提交时，短路分支行为与 report_done 时代一致
// （lastOutput 收尾、Cause=finalization_short_circuit、不写 LastResponse）。
// 已装配但为空
string]string) error {
	return s.err
}
func (s *failingResultFieldsStore) CommitBlockedResult(string, string, string, map[string]string, string, string) error {
	return s.err
}
func (s *failingResultFieldsStore) FreezeTaskLease(taskID string, lease *model.ExecutionLease) (*model.ExecutionLease, bool, error) {
	return store.FreezeTaskLease(s.TaskStore, taskID, lease)
}
func (s *failingResultFieldsStore) RevokeTaskLease(taskID string) (*model.ExecutionLease, bool, error) {
	return store.RevokeTaskLease(s.TaskStore, taskID)
}
func TestStructuredSubmissionFormat(t *testing.T) {
	sub := &StructuredSubmission{TaskID: "t1", Summary: "写入了 report.md", ChecksPerformed: []string{"go build", "go test ./internal/..."}, Evidence: []string{"report.md"}}
	out := sub.Format()
	if !strings.HasPrefix(out, "## 任务结果摘要\n\n写入了 report.md") {
		t.Errorf("Format 应以 summary 开头，实际：\n%s", out)
	}
	for _, want := range []string{"## 已执行的检查", "- go build", "- go test ./internal/...", "## 证据", "- report.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format 缺少 %q，实际：\n%s", want, out)
		}
	}
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
	s.Put(&StructuredSubmission{TaskID: "t1", Summary: "v2"})
	s.Put(&StructuredSubmission{TaskID: "t2", Summary: "other"})
	sub, ok := s.Take("t1")
	if !ok || sub.Summary != "v2" {
		t.Fatalf("Take(t1) = %+v, %t；期望 v2", sub, ok)
	}
	if _, ok := s.Take("t1"); ok {
		t.Fatal("Take 后应已删除")
	}
	if _, ok := s.Take("t2"); !ok {
		t.Fatal("Take(t1) 不应影响 t2")
	}
	s.Put(nil)
	s.Put(&StructuredSubmission{Summary: "no task"})
}

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
	executor := func(_ context.Context, task *model.Task, _ map[string]string, _ []contextcontract.HistoryEntry, actionBudget llm.OutputBudget) (ExecuteResult, error) {
		state.Put(&StructuredSubmission{TaskID: task.ID, Summary: "写入了 report.md 并通过测试", ChecksPerformed: []string{"go build", "go test ./..."}, Evidence: []string{"report.md"}, RemainingRisks: []string{"覆盖率未达标"}})
		return ExecuteResult{Output: "progress", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, executor)
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

func TestFinalizationShortCircuitWritesStructuredResultCarrier(t *testing.T) {
	setupTraceWriter(t)
	s, r, _ := setup()
	task := &model.Task{Description: "graph structured submit", EventType: "code", GraphID: "g-1"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	const agentID = "agent-structured"
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	state := NewSubmitState()
	executor := func(_ context.Context, task *model.Task, _ map[string]string, _ []contextcontract.HistoryEntry, actionBudget llm.OutputBudget) (ExecuteResult, error) {
		state.Put(&StructuredSubmission{TaskID: task.ID, Summary: "覆盖度已裁决", ResultJSON: `{"coverage":"gap","metrics":{"score":2,"ready":true}}`})
		return ExecuteResult{Output: "progress", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, executor)
	ag.FinalizationChecker = &flipFinalizationChecker{}
	ag.SubmitState = state
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := got.Results[StructuredResultStorageKey]
	decoded, err := DecodeStructuredResult(raw)
	if err != nil {
		t.Fatalf("carrier 解码失败: %v（raw=%q）", err, raw)
	}
	if decoded["coverage"] != "gap" {
		t.Fatalf("carrier 未保留 coverage: %+v", decoded)
	}
	if _, leaked := got.Results["coverage"]; leaked {
		t.Fatal("字符串型 Task.Results 不应平铺自定义字段；须由 Graph bridge 解码")
	}
}
func TestFinalizationShortCircuitStructuredBlockedCommitFailClosed(t *testing.T) {
	traceDir := setupTraceWriter(t)
	base, r, _ := setup()
	task := &model.Task{Description: "graph structured blocked", EventType: "code", GraphID: "g-1"}
	if err := base.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	const agentID = "agent-structured-blocked-fail"
	if err := base.ClaimTask(agentID, task.ID); err != nil {
		t.Fatal(err)
	}
	state := NewSubmitState()
	executor := func(_ context.Context, task *model.Task, _ map[string]string, _ []contextcontract.HistoryEntry, actionBudget llm.OutputBudget) (ExecuteResult, error) {
		state.Put(&StructuredSubmission{TaskID: task.ID, Summary: "缺少目录", Status: SubmitStatusBlocked, BlockedReason: "上游未提供目录", ResultJSON: `{"missing":"catalog"}`})
		return ExecuteResult{Output: "progress", ToolCalled: true}, nil
	}
	writeErr := errors.New("模拟 blocked 原子提交故障")
	ag := NewAgent(agentID, "code", &failingResultFieldsStore{TaskStore: base, err: writeErr}, r, executor)
	ag.FinalizationChecker = &flipFinalizationChecker{}
	ag.SubmitState = state
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)
	got, err := base.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusFailed || !strings.Contains(got.Error, writeErr.Error()) {
		t.Fatalf("blocked 原子提交失败必须 fail-closed，实际 status=%s error=%q", got.Status, got.Error)
	}
	for _, key := range []string{StructuredResultStorageKey, agentID, "event", "verdict"} {
		if _, ok := got.Results[key]; ok {
			t.Fatalf("blocked 原子提交失败不得遗留字段 %q: %#v", key, got.Results)
		}
	}
	events := p1fixesReadTraceEvents(t, traceDir)
	tr := findTransition(events, trace.KindTaskFailed, task.ID)
	if tr == nil || tr.Cause != "structured_result_persist_failed" {
		t.Fatalf("失败 trace 应记录 structured_result_persist_failed，实际 %+v", tr)
	}
}

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
	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []contextcontract.HistoryEntry, actionBudget llm.OutputBudget) (ExecuteResult, error) {
		return ExecuteResult{Output: "progress output", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, executor)
	ag.FinalizationChecker = &flipFinalizationChecker{}
	ag.SubmitState = NewSubmitState()
	ag.TextOnlyReportsDir = t.TempDir()
	ag.processTask(context.Background(), task.ID)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Results[agentID] != "progress output" {
		t.Errorf("SubmitResult = %q，期望 lastOutput 原文", got.Results[agentID])
	}
	if got.LastResponse != "" {
		t.Errorf("兼容路径不应写 LastResponse，实际 %q", got.LastResponse)
	}
	if _, ok := got.Results["event"]; ok {
		t.Error("未携带 event 的提交不应写 Results[\"event\"] 键")
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
