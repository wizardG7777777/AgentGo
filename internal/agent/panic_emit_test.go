package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// TestPanicRecovery_EmitsKindTaskFailed 是 §11.8 S11 修复的运行时双重验证。
//
// 静态扫描（terminal_emit_symmetry_test.go）保证 agent.go 中 panic-recovery
// 路径"长得对"——同函数体内 FailTask + KindTaskFailed emit 都在。本测试保证
// 这条路径"跑得对"——真的让 executor panic、processTask 通过 defer recover()
// 兜住后，trace JSONL 里能 grep 到 KindTaskFailed 事件。
//
// 修复历史：2026-04-26 §11.8 S11 落地时静态扫描首次发现 panic-recovery
// 路径调用 a.Store.FailTask 但未 emit——与 terminateTask 的对称缺失，导致
// trace 观察者对 panic 引发的任务失败完全失明。同 commit 在 agent.go:284-289
// 补 emit。本测试是该修复的运行时回归护栏。
func TestPanicRecovery_EmitsKindTaskFailed(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "will panic mid-execute", EventType: "code"}
	s.PublishTask(task)
	if err := s.ClaimTask("agent-panic-emit", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	// executor 直接 panic——触发 processTask 顶部的 defer recover() 兜底分支
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		panic("intentional test panic to drive recovery path")
	}

	ag := NewAgent("agent-panic-emit", "code", s, r, executor, 5)
	// processTask 不应再 propagate panic——defer recover() 必须吞下
	ag.processTask(context.Background(), task.ID)

	events := p1fixesReadTraceEvents(t, traceDir)

	var failed *trace.Event
	for i, ev := range events {
		if ev.Kind == trace.KindTaskFailed && ev.TaskID == task.ID {
			failed = &events[i]
			break
		}
	}
	if failed == nil {
		t.Fatalf("panic-recovery 路径未 emit KindTaskFailed——§11.8 S11 修复回退？\n实际事件 kinds: %s",
			eventKinds(events))
	}
	if failed.AgentID != "agent-panic-emit" {
		t.Errorf("AgentID=%q, want %q", failed.AgentID, "agent-panic-emit")
	}
	// agent.go:281 用 fmt.Sprintf("agent panic: %v", rec) 构造 reason，
	// 所以 reason 应包含 panic 原值
	if !strings.Contains(failed.Reason, "intentional test panic") {
		t.Errorf("Reason=%q, want contain panic message", failed.Reason)
	}
	if !strings.Contains(failed.Reason, "agent panic:") {
		t.Errorf("Reason=%q, want contain prefix \"agent panic:\"", failed.Reason)
	}
}
