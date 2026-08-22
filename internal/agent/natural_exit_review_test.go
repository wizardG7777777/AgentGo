// natural_exit_review_test.go 覆盖 2026-08-20 SWE-001 的纯文本自然退出审查：
//   - 图节点任务：文本退出被拒（nudge 注入），超限后按可恢复错误收口；
//   - scheduler 根任务：零证据审查经 NaturalExitReviewer 裁决，拒绝时注入提醒；
//   - 非图普通任务：文本自然完成行为不变（回归护栏）。
package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// TestProcessTask_GraphNodeTextExitNudgesThenRecoverable 图节点任务连续纯文本
// 退出：每次注入 submit_task_result 提醒，第 maxUnstructuredExitNudges+1 次
// 按可恢复错误回滚（pending 待重试），不得记 completed。
func TestProcessTask_GraphNodeTextExitNudgesThenRecoverable(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{
		Description: "图节点实现", EventType: "code",
		GraphID: "g-text-exit", NodeID: "impl", ActivationID: "impl@1", GraphNodeKind: "agent",
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	var histories [][]HistoryEntry
	executor := func(_ context.Context, _ *model.Task, _ map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		histories = append(histories, append([]HistoryEntry(nil), history...))
		return ExecuteResult{Output: "中段分析，没有结构化提交", ToolCalled: false}, nil
	}
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	if len(histories) != maxUnstructuredExitNudges+1 {
		t.Fatalf("文本退出应提醒 %d 次后收口，实际 LLM 调用 %d 次", maxUnstructuredExitNudges, len(histories))
	}
	for i, h := range histories[1:] {
		found := false
		for _, e := range h {
			if strings.Contains(e.IncomingMail, "system-reminder") && strings.Contains(e.IncomingMail, "submit_task_result") {
				found = true
			}
		}
		if !found {
			t.Errorf("第 %d 次提醒未注入 system-reminder: %+v", i+1, h)
		}
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == model.TaskStatusCompleted {
		t.Fatal("图节点纯文本退出不得记 completed")
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("可恢复错误应回滚 pending 重试: status=%s error=%s", got.Status, got.Error)
	}
}

// fakeExitReviewer 记录调用次数、toolFailed 入参，并按 allowOn/retryOn 裁决。
type fakeExitReviewer struct {
	calls      int
	allowOn    int
	retryOn    int
	toolFailed []bool
}

func (f *fakeExitReviewer) ReviewNaturalExit(_ context.Context, _ *model.Task, _ string, toolFailed bool) hook.NaturalExitDecision {
	f.calls++
	f.toolFailed = append(f.toolFailed, toolFailed)
	if f.retryOn > 0 && f.calls >= f.retryOn {
		return hook.NaturalExitDecision{Retry: true}
	}
	if f.calls >= f.allowOn {
		return hook.NaturalExitDecision{Allow: true}
	}
	return hook.NaturalExitDecision{Allow: false, Nudge: "<system-reminder>零证据，确认后再答</system-reminder>"}
}

// TestProcessTask_SchedulerRootTextExitReviewed 非图 scheduler 任务的纯文本
// 收口经 NaturalExitReviewer 裁决：第一次拒绝注入提醒，第二次放行完成。
func TestProcessTask_SchedulerRootTextExitReviewed(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "用户请求", EventType: "__scheduler__"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler-1", task.ID); err != nil {
		t.Fatal(err)
	}
	var histories [][]HistoryEntry
	executor := func(_ context.Context, _ *model.Task, _ map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		histories = append(histories, append([]HistoryEntry(nil), history...))
		return ExecuteResult{Output: "直接答复", ToolCalled: false}, nil
	}
	reviewer := &fakeExitReviewer{allowOn: 2}
	ag := NewAgent("scheduler-1", "__scheduler__", s, r, executor)
	ag.NaturalExitReviewer = reviewer
	ag.processTask(context.Background(), task.ID)

	if reviewer.calls != 2 {
		t.Fatalf("审查应裁决 2 次（拒绝 1 + 确认放行 1），实际 %d", reviewer.calls)
	}
	if len(histories) != 2 {
		t.Fatalf("第一次拒绝后应继续循环，实际 LLM 调用 %d 次", len(histories))
	}
	found := false
	for _, e := range histories[1] {
		if strings.Contains(e.IncomingMail, "零证据") {
			found = true
		}
	}
	if !found {
		t.Error("拒绝时应把 Nudge 注入历史")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("确认放行后应正常完成: status=%s", got.Status)
	}
}

// TestProcessTask_NonGraphWorkerTextExitUnchanged 非图普通任务（无审查器）的
// 纯文本自然完成行为不变——SWE-001 不改变 v3 兼容路径。
func TestProcessTask_NonGraphWorkerTextExitUnchanged(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "普通任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "完成", ToolCalled: false}, nil
	}
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("非图普通任务文本自然完成不应受影响: status=%s", got.Status)
	}
}

// TestProcessTask_SchedulerRootTextExitRetryOnFormatCollapse 审查返回 Retry=true
// （疑工具调用格式崩盘）时按可恢复错误回滚 pending 重试，不得记 completed
// （2026-08-21 SWE-008 三态状态机的 LOOP 接线）。
func TestProcessTask_SchedulerRootTextExitRetryOnFormatCollapse(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "用户请求", EventType: "__scheduler__"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler-1", task.ID); err != nil {
		t.Fatal(err)
	}
	executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "<｜DSML｜残片", ToolCalled: false}, nil
	}
	reviewer := &fakeExitReviewer{retryOn: 1}
	ag := NewAgent("scheduler-1", "__scheduler__", s, r, executor)
	ag.NaturalExitReviewer = reviewer
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == model.TaskStatusCompleted {
		t.Fatal("格式崩盘疑似的纯文本退出不得记 completed")
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("Retry 裁决应回滚 pending 重试: status=%s error=%s", got.Status, got.Error)
	}
}

// TestProcessTask_ToolFailedPassedToReviewer LOOP 在自然退出判别处查询工具
// 失败账本并如实传入：本任务有 Success=false 记录时 reviewer 收到
// toolFailed=true，全成功/无记录时收到 false（SWE-008 状态机 S2 状态位）。
func TestProcessTask_ToolFailedPassedToReviewer(t *testing.T) {
	newSchedulerTask := func(t *testing.T, s store.TaskStore, id string) {
		t.Helper()
		if err := s.PublishTask(&model.Task{ID: id, Description: "用户请求", EventType: "__scheduler__"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ClaimTask("scheduler-1", id); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("有失败记录传入true", func(t *testing.T) {
		s, r, _ := setup()
		newSchedulerTask(t, s, "t-fail")
		if err := s.AppendToolCall("t-fail", store.ToolCallRecord{ToolName: "submit_graph", Success: false}); err != nil {
			t.Fatal(err)
		}
		reviewer := &fakeExitReviewer{allowOn: 1}
		executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
			return ExecuteResult{Output: "答复", ToolCalled: false}, nil
		}
		ag := NewAgent("scheduler-1", "__scheduler__", s, r, executor)
		ag.NaturalExitReviewer = reviewer
		ag.processTask(context.Background(), "t-fail")
		if len(reviewer.toolFailed) == 0 || !reviewer.toolFailed[0] {
			t.Fatalf("有失败记录应传入 toolFailed=true: %+v", reviewer.toolFailed)
		}
	})

	t.Run("无失败记录传入false", func(t *testing.T) {
		s, r, _ := setup()
		newSchedulerTask(t, s, "t-ok")
		if err := s.AppendToolCall("t-ok", store.ToolCallRecord{ToolName: "read_file", Success: true}); err != nil {
			t.Fatal(err)
		}
		reviewer := &fakeExitReviewer{allowOn: 1}
		executor := func(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
			return ExecuteResult{Output: "答复", ToolCalled: false}, nil
		}
		ag := NewAgent("scheduler-1", "__scheduler__", s, r, executor)
		ag.NaturalExitReviewer = reviewer
		ag.processTask(context.Background(), "t-ok")
		if len(reviewer.toolFailed) == 0 || reviewer.toolFailed[0] {
			t.Fatalf("全部调用成功应传入 toolFailed=false: %+v", reviewer.toolFailed)
		}
	})
}
