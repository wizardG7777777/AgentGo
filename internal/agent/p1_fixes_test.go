package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
// P1 #1：handleMaxLoops 与 handleFailure 的 buildTransferNote 调用必须在 ctx 里
// 携带 agentID / taskID / loop=-1。此前直接传入 processTask 的 ctx 或
// context.Background()，导致 L1 那次 LLM 调用在 trace/log 里无 agent_id / loop。
// ============================================================================

// TestP1_TransferNoteCtxCarriesAgentMetadata_MaxLoopsPath 验证 MaxLoops 耗尽
// 走 handleMaxLoops 路径时，L1 TransferNote 调用（第 MaxLoops+1 次 Execute）
// 接收到的 ctx 包含 agentID、taskID 和 loop=-1 的标记。
func TestP1_TransferNoteCtxCarriesAgentMetadata_MaxLoopsPath(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "force maxloops path", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-p1a", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	var capturedAgentID, capturedTaskID string
	var capturedLoop int
	var callCount int32

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		n := atomic.AddInt32(&callCount, 1)
		// 最后一次调用就是 handleMaxLoops 里的 L1 压缩——记录其 ctx 字段
		if int(n) == 3 /* MaxLoops=2 → 2 次 ReactLoop + 1 次 TransferNote = 3 */ {
			capturedAgentID = AgentIDFromContext(ctx)
			capturedTaskID = TaskIDFromContext(ctx)
			capturedLoop, _ = ctx.Value(ctxLoopNum).(int)
		}
		return ExecuteResult{Output: "loop output", ToolCalled: true}, nil
	}

	ag := NewAgent("agent-p1a", "code", s, r, executor, 2) // MaxLoops=2

	ag.processTask(context.Background(), task.ID)

	if capturedAgentID != "agent-p1a" {
		t.Errorf("L1 TransferNote ctx AgentID=%q, want %q（P1 #1 回归：ctx 未注入 agentID）",
			capturedAgentID, "agent-p1a")
	}
	if capturedTaskID != task.ID {
		t.Errorf("L1 TransferNote ctx TaskID=%q, want %q", capturedTaskID, task.ID)
	}
	if capturedLoop != -1 {
		t.Errorf("L1 TransferNote ctx Loop=%d, want -1（标记非 ReactLoop 调用）", capturedLoop)
	}
}

// TestP1_TransferNoteCtxCarriesAgentMetadata_HandleFailurePath 验证 handleFailure
// 可恢复错误路径调 L1 TransferNote 时同样带上完整 metadata。此处用
// context.Background() 作基底，手动注入 agent 信息。
//
// 2026-04-25 更新：handleFailure 的 L1 调度改为分类分派——普通 transient 走 L3
// 不再调 executor。本测试改用 context overflow 错误（"context length exceeded"）
// 走 L1 路径，验证 ctx 元数据在 L1 分支里仍然正确注入。
func TestP1_TransferNoteCtxCarriesAgentMetadata_HandleFailurePath(t *testing.T) {
	s, r, _ := setup()

	task := &model.Task{Description: "force overflow recoverable error", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-p1b", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	var capturedAgentID, capturedTaskID string
	var capturedLoop int
	var callCount int32

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			// 第一轮返回 overflow recoverable 触发 L1 路径（isContextOverflow 识别 "length"）
			return ExecuteResult{}, &ErrRecoverable{Err: errors.New("prompt exceeds context length")}
		}
		// 第二轮是 L1 TransferNote 调用
		capturedAgentID = AgentIDFromContext(ctx)
		capturedTaskID = TaskIDFromContext(ctx)
		capturedLoop, _ = ctx.Value(ctxLoopNum).(int)
		return ExecuteResult{Output: "note text", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-p1b", "code", s, r, executor, 5)

	ag.processTask(context.Background(), task.ID)

	if capturedAgentID != "agent-p1b" {
		t.Errorf("handleFailure L1 ctx AgentID=%q, want %q", capturedAgentID, "agent-p1b")
	}
	if capturedTaskID != task.ID {
		t.Errorf("handleFailure L1 ctx TaskID=%q, want %q", capturedTaskID, task.ID)
	}
	if capturedLoop != -1 {
		t.Errorf("handleFailure L1 ctx Loop=%d, want -1", capturedLoop)
	}
}

// ============================================================================
// P1 #2：新增 EventKind task_retry / task_failed / task_cancelled
// 必须在对应路径 emit，让 trace 账本能覆盖全部终态。
// ============================================================================

// TestP1_TraceEmit_TaskRetry_OnMaxLoops 验证 MaxLoops 耗尽触发 RetryRollback 时
// emit KindTaskRetry 事件，Reason 以 "max_loops:" 开头。
func TestP1_TraceEmit_TaskRetry_OnMaxLoops(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "retry on maxloops", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-p1c", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "still working", ToolCalled: true}, nil
	}

	ag := NewAgent("agent-p1c", "code", s, r, executor, 2)
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
		t.Fatalf("未 emit KindTaskRetry 事件，收到的事件：%s", eventKinds(events))
	}
	if found.AgentID != "agent-p1c" {
		t.Errorf("AgentID=%q, want %q", found.AgentID, "agent-p1c")
	}
	if !strings.HasPrefix(found.Reason, "max_loops:") {
		t.Errorf("Reason=%q, want prefix \"max_loops:\"", found.Reason)
	}
}

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

	ag := NewAgent("agent-p1d", "code", s, r, executor, 5)
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

	ag := NewAgent("agent-p1e", "code", s, r, executor, 5)
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

	ag := NewAgent("agent-p1f", "code", s, r, executor, 5)

	ctx, cancel := context.WithCancel(context.Background())
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
}

// eventKinds 返回事件 kind 列表的紧凑字符串，用于测试失败时的诊断输出。
func eventKinds(events []trace.Event) string {
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind))
	}
	return "[" + strings.Join(kinds, ", ") + "]"
}
