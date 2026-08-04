package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// p1fixesReadTraceEvents 读 traceDir 里所有 JSONL 事件。
// 用于断言特定 EventKind 是否 emit。
func p1fixesReadTraceEvents(t *testing.T, traceDir string) []trace.Event {
	t.Helper()
	var events []trace.Event
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("read trace dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(traceDir, entry.Name()))
		if err != nil {
			t.Fatalf("read trace file: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var ev trace.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal trace event: %v (line=%s)", err, line)
			}
			events = append(events, ev)
		}
	}
	return events
}

// setupTraceWriter 挂上临时 trace writer，返回 traceDir + cleanup。
func setupTraceWriter(t *testing.T) string {
	t.Helper()
	traceDir := t.TempDir()
	tw, err := trace.NewWriter(traceDir, 0)
	if err != nil {
		t.Fatalf("new trace writer: %v", err)
	}
	t.Cleanup(func() { tw.Close() })
	oldDefault := trace.Default()
	trace.SetDefault(tw)
	t.Cleanup(func() { trace.SetDefault(oldDefault) })
	return traceDir
}

// ============================================================================
// P1 #2：新增 EventKind task_retry / task_failed / task_cancelled
// 必须在对应路径 emit，让 trace 账本能覆盖全部终态。
// ============================================================================

// TestP1_TraceEmit_TaskRetry_OnRecoverableError 验证 handleFailure 可恢复错误
// 路径触发 RetryRollback 时 emit KindTaskRetry，Reason 前缀 "recoverable_error:"。
func TestP1_TraceEmit_TaskRetry_OnRecoverableError(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "retry on recoverable", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-p1d", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, &ErrRecoverable{Err: errors.New("429 rate limit")}
	}

	ag := NewAgent("agent-p1d", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	events := p1fixesReadTraceEvents(t, traceDir)
	var found *trace.Event
	for i, ev := range events {
		if ev.Kind == trace.KindTaskRetry && ev.TaskID == task.ID {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("未 emit KindTaskRetry，事件：%s", eventKinds(events))
	}
	if !strings.HasPrefix(found.Reason, "recoverable_error:") {
		t.Errorf("Reason=%q, want prefix \"recoverable_error:\"", found.Reason)
	}
	if !strings.Contains(found.Reason, "429 rate limit") {
		t.Errorf("Reason=%q, want contains %q", found.Reason, "429 rate limit")
	}
}

// TestP1_TraceEmit_TaskFailed_OnTerminate 验证 terminateTask 触发时 emit
// KindTaskFailed 事件。
func TestP1_TraceEmit_TaskFailed_OnTerminate(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "will be terminated", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-p1e", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	// 不可恢复错误 → handleFailure 的 else 分支 → terminateTask
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, errors.New("unrecoverable boom")
	}

	ag := NewAgent("agent-p1e", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	events := p1fixesReadTraceEvents(t, traceDir)
	var found *trace.Event
	for i, ev := range events {
		if ev.Kind == trace.KindTaskFailed && ev.TaskID == task.ID {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("未 emit KindTaskFailed，事件：%s", eventKinds(events))
	}
	if found.AgentID != "agent-p1e" {
		t.Errorf("AgentID=%q, want %q", found.AgentID, "agent-p1e")
	}
	if !strings.Contains(found.Reason, "unrecoverable boom") {
		t.Errorf("Reason=%q, want contains %q", found.Reason, "unrecoverable boom")
	}
}

// TestP1_TraceEmit_TaskCancelled_OnCtxDone 验证外部 ctx 取消时 emit
// KindTaskCancelled 事件。
func TestP1_TraceEmit_TaskCancelled_OnCtxDone(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "will be cancelled", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-p1f", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		t.Errorf("executor 不该被调用 — ctx 应在 loop 顶部就被检测到取消")
		return ExecuteResult{}, nil
	}

	ag := NewAgent("agent-p1f", "code", s, r, executor)

	ctx, cancel := context.WithCancel(WithCancelSource(context.Background(), "user"))
	cancel() // 立即取消
	ag.processTask(ctx, task.ID)

	events := p1fixesReadTraceEvents(t, traceDir)
	var found *trace.Event
	for i, ev := range events {
		if ev.Kind == trace.KindTaskCancelled && ev.TaskID == task.ID {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("未 emit KindTaskCancelled，事件：%s", eventKinds(events))
	}
	if found.AgentID != "agent-p1f" {
		t.Errorf("AgentID=%q, want %q", found.AgentID, "agent-p1f")
	}
	if found.Reason == "" {
		t.Error("Reason 应包含 ctx.Err()，当前为空")
	}
	if found.Transition == nil {
		t.Fatal("Transition 应填充，当前为 nil")
	}
	if found.Transition.CancelSource != "user" {
		t.Errorf("CancelSource=%q, want user", found.Transition.CancelSource)
	}
}

// TestTraceUpgrade_TaskFailedCause_OnRecoverableRetriesExhausted 验证可恢复错误
// 重试预算耗尽时的 Cause 是 recoverable_error_retries_exhausted。
func TestTraceUpgrade_TaskFailedCause_OnRecoverableRetriesExhausted(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "recoverable exhausts retries", EventType: "code"}
	s.PublishTask(task)

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, &ErrRecoverable{Err: errors.New("persistent 429")}
	}

	ag := NewAgent("agent-trace-v5", "code", s, r, executor)
	ag.MaxRetries = 1

	if err := s.ClaimTask("agent-trace-v5", task.ID); err != nil {
		t.Fatalf("ClaimTask first: %v", err)
	}
	ag.processTask(context.Background(), task.ID)

	if err := s.ClaimTask("agent-trace-v5", task.ID); err != nil {
		t.Fatalf("ClaimTask second: %v", err)
	}
	ag.processTask(context.Background(), task.ID)

	events := p1fixesReadTraceEvents(t, traceDir)
	var found *trace.Event
	for i := range events {
		ev := events[i]
		if ev.Kind == trace.KindTaskFailed && ev.TaskID == task.ID {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("未 emit KindTaskFailed，事件：%s", eventKinds(events))
	}
	if found.Transition == nil {
		t.Fatal("Transition 应填充，当前为 nil")
	}
	if found.Transition.Cause != "recoverable_error_retries_exhausted" {
		t.Errorf("Cause=%q, want recoverable_error_retries_exhausted", found.Transition.Cause)
	}
	if found.Transition.RetryCount != 1 {
		t.Errorf("RetryCount=%d, want 1", found.Transition.RetryCount)
	}
}

// eventKinds 返回事件 kind 列表的紧凑字符串，用于测试失败时的诊断输出。
func eventKinds(events []trace.Event) string {
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind))
	}
	return "[" + strings.Join(kinds, ", ") + "]"
}
