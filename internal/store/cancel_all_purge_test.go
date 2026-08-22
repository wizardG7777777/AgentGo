package store

import (
	"context"
	"fmt"
	"testing"
	"testing/quick"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

// drainEvents 非阻塞排空公告板事件通道。
func drainEvents(ch chan model.Event) []model.Event {
	var out []model.Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func countBoardEvents(events []model.Event, eventType model.EventType) int {
	n := 0
	for _, ev := range events {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

// --- CancelAllNonTerminal ---

func TestCancelAllNonTerminal_MixedStates(t *testing.T) {
	s, ch := newTestStore(64, 100)
	reg := NewTaskCancelRegistry()
	s.SetCancelRegistry(reg)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	pending1 := publishTestTask(t, s, "pending-1")
	pending2 := publishTestTask(t, s, "pending-2")

	processing := publishTestTask(t, s, "processing-1")
	if err := s.ClaimTask("agent-1", processing.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	ctx := reg.GetOrCreate(context.Background(), processing.ID)

	completed := publishTestTask(t, s, "completed-1")
	if err := s.ClaimTask("agent-2", completed.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.SubmitResult("agent-2", completed.ID, "done"); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}

	blocked := publishTestTask(t, s, "blocked-1")
	if err := s.BlockTaskBySystem(blocked.ID, "路由失败"); err != nil {
		t.Fatalf("BlockTaskBySystem: %v", err)
	}

	n, err := s.CancelAllNonTerminal("session_force_close")
	if err != nil {
		t.Fatalf("CancelAllNonTerminal: %v", err)
	}
	if n != 3 {
		t.Fatalf("取消数量 = %d，期望 3（2 pending + 1 processing）", n)
	}

	// 只有 pending / processing 转为 cancelled
	for _, id := range []string{pending1.ID, pending2.ID, processing.ID} {
		got, err := s.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if got.Status != model.TaskStatusCancelled {
			t.Errorf("任务 %s 状态 = %s，期望 cancelled", id, got.Status)
		}
		if got.CompletedAt.IsZero() {
			t.Errorf("任务 %s 的 CompletedAt 应已设置", id)
		}
	}

	// 已是终态的任务不动
	if got, _ := s.GetTask(completed.ID); got.Status != model.TaskStatusCompleted {
		t.Errorf("completed 任务状态 = %s，期望保持 completed", got.Status)
	}
	if got, _ := s.GetTask(blocked.ID); got.Status != model.TaskStatusBlocked {
		t.Errorf("blocked 任务状态 = %s，期望保持 blocked", got.Status)
	}

	// 取消来源正确登记，且 per-task context 已取消
	if src := reg.Source(processing.ID); src != "session_force_close" {
		t.Errorf("取消来源 = %q，期望 session_force_close", src)
	}
	select {
	case <-ctx.Done():
	default:
		t.Error("processing 任务的 cancel context 应已取消")
	}

	// 公告板事件：每个被取消任务恰好一条 EventTaskCancelled
	events := drainEvents(ch)
	if got := countBoardEvents(events, model.EventTaskCancelled); got != 3 {
		t.Errorf("task_cancelled 公告板事件数 = %d，期望 3", got)
	}

	// history：每个被取消任务一条 task_cancelled，payload 带来源
	cancelledHist := 0
	for _, ev := range em.Events() {
		if ev.EventType != session.HistEventTaskCancelled {
			continue
		}
		cancelledHist++
		if ev.Payload["cancel_source"] != "session_force_close" {
			t.Errorf("history payload cancel_source = %v，期望 session_force_close", ev.Payload["cancel_source"])
		}
	}
	if cancelledHist != 3 {
		t.Errorf("task_cancelled history 事件数 = %d，期望 3", cancelledHist)
	}
}

func TestCancelAllNonTerminal_EmptyAndNilRegistry(t *testing.T) {
	s, _ := newTestStore(10, 100)

	if n, err := s.CancelAllNonTerminal("src"); err != nil || n != 0 {
		t.Errorf("空 Store 取消数量应为 0，实际 %d", n)
	}

	// 未注入 cancel registry 时同样安全（nil 降级）
	task := publishTestTask(t, s, "no-registry")
	if n, err := s.CancelAllNonTerminal(""); err != nil || n != 1 {
		t.Fatalf("取消数量应为 1，实际 %d", n)
	}
	if got, _ := s.GetTask(task.ID); got.Status != model.TaskStatusCancelled {
		t.Errorf("状态 = %s，期望 cancelled", got.Status)
	}
}

// TestProperty_CancelAllNonTerminal 性质：任意混合状态下，返回值等于
// pending+processing 任务数；这些任务全部转为 cancelled 并各发一条
// 公告板事件；终态任务状态不变；已登记 context 的取消来源正确。
func TestProperty_CancelAllNonTerminal(t *testing.T) {
	prop := func(nPending, nProcessing, nCompleted, nBlocked uint8) bool {
		s, ch := newTestStore(256, 100)
		reg := NewTaskCancelRegistry()
		s.SetCancelRegistry(reg)

		var liveIDs []string                        // pending + processing，应被取消
		var processingIDs []string                  // 登记了 cancel context 的子集
		frozen := make(map[string]model.TaskStatus) // 终态任务 → 期望保持的状态

		for i := 0; i < int(nPending%5); i++ {
			task := &model.Task{Description: fmt.Sprintf("pending-%d", i)}
			if err := s.PublishTask(task); err != nil {
				return false
			}
			liveIDs = append(liveIDs, task.ID)
		}
		for i := 0; i < int(nProcessing%5); i++ {
			task := &model.Task{Description: fmt.Sprintf("processing-%d", i)}
			if err := s.PublishTask(task); err != nil {
				return false
			}
			if err := s.ClaimTask(fmt.Sprintf("agent-p%d", i), task.ID); err != nil {
				return false
			}
			reg.GetOrCreate(context.Background(), task.ID)
			liveIDs = append(liveIDs, task.ID)
			processingIDs = append(processingIDs, task.ID)
		}
		for i := 0; i < int(nCompleted%5); i++ {
			task := &model.Task{Description: fmt.Sprintf("completed-%d", i)}
			if err := s.PublishTask(task); err != nil {
				return false
			}
			if err := s.ClaimTask(fmt.Sprintf("agent-c%d", i), task.ID); err != nil {
				return false
			}
			if err := s.SubmitResult(fmt.Sprintf("agent-c%d", i), task.ID, "done"); err != nil {
				return false
			}
			frozen[task.ID] = model.TaskStatusCompleted
		}
		for i := 0; i < int(nBlocked%5); i++ {
			task := &model.Task{Description: fmt.Sprintf("blocked-%d", i)}
			if err := s.PublishTask(task); err != nil {
				return false
			}
			if err := s.BlockTaskBySystem(task.ID, "路由失败"); err != nil {
				return false
			}
			frozen[task.ID] = model.TaskStatusBlocked
		}

		wantCancelled := len(liveIDs)
		if got, err := s.CancelAllNonTerminal("pbt-source"); err != nil || got != wantCancelled {
			return false
		}

		for _, id := range liveIDs {
			got, err := s.GetTask(id)
			if err != nil || got.Status != model.TaskStatusCancelled {
				return false
			}
		}
		for id, wantStatus := range frozen {
			got, err := s.GetTask(id)
			if err != nil || got.Status != wantStatus {
				return false
			}
		}
		for _, id := range processingIDs {
			if reg.Source(id) != "pbt-source" {
				return false
			}
		}
		return countBoardEvents(drainEvents(ch), model.EventTaskCancelled) == wantCancelled
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatalf("CancelAllNonTerminal property 失败: %v", err)
	}
}

// --- PurgeAll ---

func TestPurgeAll_ClearsEverything(t *testing.T) {
	s, ch := newTestStore(64, 100)
	reg := NewTaskCancelRegistry()
	s.SetCancelRegistry(reg)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	pending := publishTestTask(t, s, "pending")
	processing := publishTestTask(t, s, "processing")
	if err := s.ClaimTask("agent-1", processing.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	ctx := reg.GetOrCreate(context.Background(), processing.ID)
	completed := publishTestTask(t, s, "completed")
	if err := s.ClaimTask("agent-2", completed.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.SubmitResult("agent-2", completed.ID, "done"); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}

	// 按任务索引的派生状态：tool calls 账本 + ReadSet
	if err := s.AppendToolCall(processing.ID, ToolCallRecord{ToolName: "read_file", Timestamp: time.Now()}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	if err := s.UpsertReadSet(processing.ID, "/abs/path.go", model.ReadInfo{ReadAt: time.Now()}); err != nil {
		t.Fatalf("UpsertReadSet: %v", err)
	}

	drainEvents(ch) // 丢弃累积事件，便于验证 PurgeAll 不再发新事件

	s.PurgeAll()

	all, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("PurgeAll 后 ScanAll 应为空，实际 %d 个任务", len(all))
	}
	for _, id := range []string{pending.ID, processing.ID, completed.ID} {
		if _, err := s.GetTask(id); err != ErrTaskNotFound {
			t.Errorf("PurgeAll 后 GetTask(%s) 应报未找到，实际 err = %v", id, err)
		}
	}
	if snaps := s.ExportSnapshot(); len(snaps) != 0 {
		t.Errorf("PurgeAll 后 ExportSnapshot 应为空，实际 %d 条", len(snaps))
	}
	if recs, _ := s.QueryToolCalls(processing.ID, ""); len(recs) != 0 {
		t.Errorf("PurgeAll 后 tool calls 账本应为空，实际 %d 条", len(recs))
	}

	// cancel registry 条目清空：旧 ctx 已取消，来源记录已清
	select {
	case <-ctx.Done():
	default:
		t.Error("PurgeAll 应取消全部 per-task context")
	}
	if src := reg.Source(processing.ID); src != "" {
		t.Errorf("PurgeAll 后取消来源应清空，实际 %q", src)
	}

	// PurgeAll 不发公告板事件
	if events := drainEvents(ch); len(events) != 0 {
		t.Errorf("PurgeAll 不应发公告板事件，实际 %d 条", len(events))
	}

	// Store 回到刚构造状态，可继续复用
	reborn := publishTestTask(t, s, "reborn")
	got, err := s.GetTask(reborn.ID)
	if err != nil {
		t.Fatalf("PurgeAll 后 GetTask: %v", err)
	}
	if got.Status != model.TaskStatusPending {
		t.Errorf("PurgeAll 后新发布任务状态 = %s，期望 pending", got.Status)
	}
}
