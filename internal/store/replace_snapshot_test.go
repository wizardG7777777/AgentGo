package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

// snapTask 构造一条最小合法 TaskSnapshot：时间字段留空（零值），导入路径
// 容忍空时间串并对 pending 快照补全新的 PendingSince。
func snapTask(id string, status string, deps ...string) session.TaskSnapshot {
	return session.TaskSnapshot{
		ID:             id,
		Description:    "快照任务-" + id,
		Status:         status,
		Dependencies:   deps,
		MaxConcurrency: 1,
		TimeoutSeconds: 300,
	}
}

// TestReplaceSnapshot_ReplacesNonEmptyBoard 非空板上整体替换：旧任务（含
// processing）与按任务索引的派生状态（tool calls 账本）全部消失，新快照
// 任务以原 ID 落板；替换过程不发任何公告板 / history 事件。
func TestReplaceSnapshot_ReplacesNonEmptyBoard(t *testing.T) {
	s, ch := newTestStore(64, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	oldPending := publishTestTask(t, s, "旧-pending")
	oldProcessing := publishTestTask(t, s, "旧-processing")
	if err := s.ClaimTask("agent-1", oldProcessing.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.AppendToolCall(oldProcessing.ID, ToolCallRecord{ToolName: "read_file"}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	drainEvents(ch) // 丢弃替换前累积事件，便于验证替换本身不发新事件
	em.mu.Lock()
	em.events = nil
	em.mu.Unlock()

	err := s.ReplaceSnapshot([]session.TaskSnapshot{
		snapTask("new-pending", "pending"),
		snapTask("new-done", "completed"),
	})
	if err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// 旧任务全部消失
	for _, id := range []string{oldPending.ID, oldProcessing.ID} {
		if _, err := s.GetTask(id); err != ErrTaskNotFound {
			t.Errorf("替换后 GetTask(%s) 应报未找到，实际 err = %v", id, err)
		}
	}
	if recs, _ := s.QueryToolCalls(oldProcessing.ID, ""); len(recs) != 0 {
		t.Errorf("替换后旧任务的 tool calls 账本应为空，实际 %d 条", len(recs))
	}

	// 新快照任务以原 ID、原状态落板
	all, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("替换后 ScanAll 应为 2 个任务，实际 %d", len(all))
	}
	gotPending, err := s.GetTask("new-pending")
	if err != nil {
		t.Fatalf("GetTask(new-pending): %v", err)
	}
	if gotPending.Status != model.TaskStatusPending {
		t.Errorf("new-pending 状态 = %s，期望 pending", gotPending.Status)
	}
	if gotPending.Description != "快照任务-new-pending" {
		t.Errorf("new-pending 描述 = %q，期望 快照任务-new-pending", gotPending.Description)
	}
	if gotDone, err := s.GetTask("new-done"); err != nil || gotDone.Status != model.TaskStatusCompleted {
		t.Errorf("new-done 应存在且为 completed，实际 err = %v", err)
	}

	// 替换不发公告板 / history 事件（见 ReplaceSnapshot 注释的事件语义结论）
	if events := drainEvents(ch); len(events) != 0 {
		t.Errorf("ReplaceSnapshot 不应发公告板事件，实际 %d 条", len(events))
	}
	if n := len(em.Events()); n != 0 {
		t.Errorf("ReplaceSnapshot 不应发 history 事件，实际 %d 条", n)
	}
}

// TestReplaceSnapshot_PendingClaimable 替换进去的 pending 任务无需任何事件，
// 下一轮 QueryAvailable 轮询即可见并可经 ClaimTask 认领。
func TestReplaceSnapshot_PendingClaimable(t *testing.T) {
	s, _ := newTestStore(64, 100)

	if err := s.ReplaceSnapshot([]session.TaskSnapshot{snapTask("t-1", "pending")}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	avail, err := s.QueryAvailable("", "agent-1")
	if err != nil {
		t.Fatalf("QueryAvailable: %v", err)
	}
	if len(avail) != 1 || avail[0].ID != "t-1" {
		t.Fatalf("QueryAvailable 应只返回 t-1，实际 %v", avail)
	}
	if err := s.ClaimTask("agent-1", "t-1"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	got, _ := s.GetTask("t-1")
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("认领后状态 = %s，期望 processing", got.Status)
	}
}

// TestReplaceSnapshot_TerminalPreserved 终态任务替换后保持终态：不出现在
// 可认领集合，也不能再被认领。
func TestReplaceSnapshot_TerminalPreserved(t *testing.T) {
	s, _ := newTestStore(64, 100)

	if err := s.ReplaceSnapshot([]session.TaskSnapshot{snapTask("t-done", "completed")}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	got, err := s.GetTask("t-done")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("状态 = %s，期望保持 completed", got.Status)
	}
	if avail, _ := s.QueryAvailable("", "agent-1"); len(avail) != 0 {
		t.Errorf("终态任务不应出现在可认领集合，实际 %d 个", len(avail))
	}
	if err := s.ClaimTask("agent-1", "t-done"); err == nil || !strings.Contains(err.Error(), "cannot claim") {
		t.Errorf("认领终态任务应被拒，实际 err = %v", err)
	}
}

// TestReplaceSnapshot_DependenciesPreserved 任务 ID 与依赖关系随替换保留：
// 依赖已 completed 的 pending 任务可认领；依赖未 completed / 依赖缺失的不可认领。
func TestReplaceSnapshot_DependenciesPreserved(t *testing.T) {
	s, _ := newTestStore(64, 100)

	err := s.ReplaceSnapshot([]session.TaskSnapshot{
		snapTask("dep-done", "completed"),
		snapTask("child-ready", "pending", "dep-done"),
		snapTask("dep-pending", "pending"),
		snapTask("child-waiting", "pending", "dep-pending"),
		snapTask("child-orphan", "pending", "ghost-dep"),
	})
	if err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// 依赖已完成 → 可认领，且认领后依赖字段原样保留
	if err := s.ClaimTask("agent-1", "child-ready"); err != nil {
		t.Fatalf("child-ready 应可认领，实际 err = %v", err)
	}
	got, _ := s.GetTask("child-ready")
	if len(got.Dependencies) != 1 || got.Dependencies[0] != "dep-done" {
		t.Errorf("child-ready 依赖 = %v，期望 [dep-done]", got.Dependencies)
	}

	// 依赖仍是 pending → 依赖未满足
	if err := s.ClaimTask("agent-1", "child-waiting"); !errors.Is(err, ErrDependencyNotMet) {
		t.Errorf("child-waiting 认领应报依赖未满足，实际 err = %v", err)
	}

	// 依赖不在快照内 → 依赖未满足
	if err := s.ClaimTask("agent-1", "child-orphan"); !errors.Is(err, ErrDependencyNotMet) {
		t.Errorf("child-orphan 认领应报依赖未满足，实际 err = %v", err)
	}
}

// TestReplaceSnapshot_EmptySnapshotClearsBoard 空快照替换等于清空公告板，
// 且 cancelRegistry 一并复位（与 PurgeAll 同纪律）。
func TestReplaceSnapshot_EmptySnapshotClearsBoard(t *testing.T) {
	s, _ := newTestStore(64, 100)
	reg := NewTaskCancelRegistry()
	s.SetCancelRegistry(reg)

	task := publishTestTask(t, s, "将被清空")
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	ctx := reg.GetOrCreate(context.Background(), task.ID)

	if err := s.ReplaceSnapshot(nil); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if all, _ := s.ScanAll(); len(all) != 0 {
		t.Errorf("空快照替换后 ScanAll 应为空，实际 %d 个任务", len(all))
	}
	if snaps := s.ExportSnapshot(); len(snaps) != 0 {
		t.Errorf("空快照替换后 ExportSnapshot 应为空，实际 %d 条", len(snaps))
	}

	// 旧任务的 per-task context 已取消，registry 条目清空
	select {
	case <-ctx.Done():
	default:
		t.Error("ReplaceSnapshot 应取消全部旧 per-task context")
	}
	if src := reg.Source(task.ID); src != "" {
		t.Errorf("ReplaceSnapshot 后取消来源应清空，实际 %q", src)
	}

	// Store 可继续复用
	reborn := publishTestTask(t, s, "重生")
	if got, err := s.GetTask(reborn.ID); err != nil || got.Status != model.TaskStatusPending {
		t.Errorf("替换后新发布任务应为 pending，实际 err = %v", err)
	}
}

// unsupportedFakeStore 实现 TaskStore 主接口但不具备 ReplaceSnapshot 可选
// 能力，用于验证 iface 助手在不支持时返回错误而非静默降级。
type unsupportedFakeStore struct{}

func (unsupportedFakeStore) PublishTask(*model.Task) error             { return nil }
func (unsupportedFakeStore) ClaimTask(string, string) error            { return nil }
func (unsupportedFakeStore) SubmitResult(string, string, string) error { return nil }
func (unsupportedFakeStore) TransitionState(string, model.TaskStatus, model.TaskStatus) error {
	return nil
}
func (unsupportedFakeStore) FailTask(string, string, string) error                   { return nil }
func (unsupportedFakeStore) FailTaskBySystem(string, string) error                   { return nil }
func (unsupportedFakeStore) RetryRollback(string, string, string) error              { return nil }
func (unsupportedFakeStore) AppendOutput(string, string, string) error               { return nil }
func (unsupportedFakeStore) RecordLastHistory(string, []byte) error                  { return nil }
func (unsupportedFakeStore) AppendArtifact(string, string) error                     { return nil }
func (unsupportedFakeStore) AppendSchedulerBatch(string, string) error               { return nil }
func (unsupportedFakeStore) ClearSchedulerBatch(string) error                        { return nil }
func (unsupportedFakeStore) AppendToolCall(string, ToolCallRecord) error             { return nil }
func (unsupportedFakeStore) QueryToolCalls(string, string) ([]ToolCallRecord, error) { return nil, nil }
func (unsupportedFakeStore) RecordLastResponse(string, string) error                 { return nil }
func (unsupportedFakeStore) QueryAvailable(string, string) ([]*model.Task, error)    { return nil, nil }
func (unsupportedFakeStore) GetTask(string) (*model.Task, error)                     { return nil, ErrTaskNotFound }
func (unsupportedFakeStore) GetDependencyResults(string) (map[string]string, error)  { return nil, nil }
func (unsupportedFakeStore) GetDependencyArtifacts(string) (map[string][]string, error) {
	return nil, nil
}
func (unsupportedFakeStore) ScanAll() ([]*model.Task, error) { return nil, nil }

// TestReplaceSnapshotHelper iface 助手：MemoryTaskStore 走可选接口真正替换；
// 不支持该能力的 Store 返回中文错误。
func TestReplaceSnapshotHelper(t *testing.T) {
	s, _ := newTestStore(64, 100)
	var ts TaskStore = s
	if err := ReplaceSnapshot(ts, []session.TaskSnapshot{snapTask("via-helper", "pending")}); err != nil {
		t.Fatalf("ReplaceSnapshot 助手: %v", err)
	}
	if _, err := s.GetTask("via-helper"); err != nil {
		t.Errorf("助手替换后 GetTask(via-helper) 应命中，实际 err = %v", err)
	}

	err := ReplaceSnapshot(unsupportedFakeStore{}, nil)
	if err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Errorf("不支持的 Store 应报「不支持」错误，实际 err = %v", err)
	}
}
