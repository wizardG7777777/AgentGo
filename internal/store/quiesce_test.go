package store

import (
	"errors"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

// requireQuiesced 断言 err 是静默围栏拒绝：包装 ErrStoreQuiesced 且中文
// 消息含「静默」字样。
func requireQuiesced(t *testing.T, err error, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("静默期间 %s 应被拒绝，实际成功", op)
	}
	if !errors.Is(err, ErrStoreQuiesced) {
		t.Fatalf("静默期间 %s 的错误应包装 ErrStoreQuiesced，实际 %v", op, err)
	}
	if !strings.Contains(err.Error(), "静默") {
		t.Fatalf("静默期间 %s 的错误消息应含「静默」，实际 %q", op, err.Error())
	}
}

// clearEmitter 清空 mockEmitter 已累积事件（同包测试辅助）。
func clearEmitter(em *mockEmitter) {
	em.mu.Lock()
	em.events = nil
	em.mu.Unlock()
}

// quiesceTestBoard 构造带一个 pending 任务与一个 processing 任务（agent-1
// 认领、已冻结租约）的 Store，返回事件通道与发射器。
func quiesceTestBoard(t *testing.T) (*MemoryTaskStore, chan model.Event, *mockEmitter, *model.Task, *model.Task) {
	t.Helper()
	s, ch := newTestStore(64, 100)
	em := &mockEmitter{}
	s.SetHistoryEmitter(em)

	pending := publishTestTask(t, s, "待认领")
	processing := publishTestTask(t, s, "执行中")
	if err := s.ClaimTask("agent-1", processing.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	lease := newTestLease(processing.ID)
	lease.Digest = lease.ComputeDigest()
	if _, frozen, err := s.FreezeTaskLease(processing.ID, lease); err != nil || !frozen {
		t.Fatalf("预置租约冻结失败: frozen=%t err=%v", frozen, err)
	}
	return s, ch, em, pending, processing
}

// TestQuiesce_FencesAllTransitions 静默期间全部状态迁移入口被拒绝：
// 返回含「静默」的 ErrStoreQuiesced，不改状态、不发公告板事件、不发 history。
func TestQuiesce_FencesAllTransitions(t *testing.T) {
	s, ch, em, pending, processing := quiesceTestBoard(t)
	drainEvents(ch)
	clearEmitter(em)

	s.EnterQuiesce()

	requireQuiesced(t, s.PublishTask(&model.Task{Description: "新任务"}), "PublishTask")
	requireQuiesced(t, s.ClaimTask("agent-2", pending.ID), "ClaimTask")
	requireQuiesced(t, s.SubmitResult("agent-1", processing.ID, "done"), "SubmitResult")
	requireQuiesced(t, s.SubmitResultWithFields("agent-1", processing.ID, "done", map[string]string{"event": "ready"}), "SubmitResultWithFields")
	requireQuiesced(t, s.FailTask("agent-1", processing.ID, "失败"), "FailTask")
	requireQuiesced(t, s.FailTaskBySystem(processing.ID, "系统失败"), "FailTaskBySystem")
	requireQuiesced(t, s.TransitionState(pending.ID, model.TaskStatusPending, model.TaskStatusCancelled), "TransitionState")
	// 经 iface 助手走带取消来源的迁移，同样被围栏
	requireQuiesced(t, TransitionStateWithCancelSource(s, pending.ID, model.TaskStatusPending, model.TaskStatusCancelled, "test"), "TransitionStateWithCancelSource")
	requireQuiesced(t, s.BlockTaskBySystem(pending.ID, "路由失败"), "BlockTaskBySystem")
	requireQuiesced(t, s.BlockProcessingTaskBySystem(processing.ID, "自报 blocked", "agent_reported_blocked"), "BlockProcessingTaskBySystem")
	requireQuiesced(t, s.CommitBlockedResult("agent-1", processing.ID, "blocked", map[string]string{"structured": "value"}, "缺少输入", "agent_reported_blocked"), "CommitBlockedResult")
	requireQuiesced(t, s.RetryRollback("agent-1", processing.ID, "重试"), "RetryRollback")
	requireQuiesced(t, s.RecordResultField(processing.ID, "event", "completed"), "RecordResultField")
	requireQuiesced(t, s.RecordResultFields(processing.ID, map[string]string{"event": "completed", "verdict": "pass"}), "RecordResultFields")
	if _, _, err := s.RevokeTaskLease(processing.ID); err != nil {
		requireQuiesced(t, err, "RevokeTaskLease")
	} else {
		t.Fatal("静默期间 RevokeTaskLease 应被拒绝，实际成功")
	}
	if n, err := s.CancelAllNonTerminal("test"); !errors.Is(err, ErrStoreQuiesced) || n != 0 {
		t.Fatalf("静默期间 CancelAllNonTerminal 应返回 0/ErrStoreQuiesced，实际 n=%d err=%v", n, err)
	}

	// 状态零变化：两个任务保持原状态、原认领、原重试计数、原租约
	gotPending, _ := s.GetTask(pending.ID)
	if gotPending.Status != model.TaskStatusPending || len(gotPending.Agents) != 0 {
		t.Errorf("pending 任务被改动: status=%s agents=%v", gotPending.Status, gotPending.Agents)
	}
	gotProc, _ := s.GetTask(processing.ID)
	if gotProc.Status != model.TaskStatusProcessing {
		t.Errorf("processing 任务状态 = %s，期望 processing", gotProc.Status)
	}
	if len(gotProc.Agents) != 1 || gotProc.Agents[0] != "agent-1" {
		t.Errorf("processing 任务认领列表 = %v，期望 [agent-1]", gotProc.Agents)
	}
	if gotProc.RetryCount != 0 {
		t.Errorf("RetryCount = %d，期望 0（RetryRollback 未生效）", gotProc.RetryCount)
	}
	if _, ok := gotProc.Results["event"]; ok {
		t.Error("Results 不应出现 event 键（RecordResultField 未生效）")
	}
	if gotProc.Lease == nil || gotProc.Lease.Revoked {
		t.Error("租约应保持未撤销（RevokeTaskLease 未生效）")
	}
	if all, _ := s.ScanAll(); len(all) != 2 {
		t.Errorf("ScanAll 应仍为 2 个任务，实际 %d", len(all))
	}

	// 零事件、零 history
	if events := drainEvents(ch); len(events) != 0 {
		t.Errorf("静默期间不应发公告板事件，实际 %d 条", len(events))
	}
	if n := len(em.Events()); n != 0 {
		t.Errorf("静默期间不应发 history 事件，实际 %d 条", n)
	}
}

// TestQuiesce_ReadOnlyAndLedgerUnaffected 静默期间只读方法与执行账本写
// （非状态迁移）不受影响——Reset 前存活 agent 的正常路径不被注入错误。
func TestQuiesce_ReadOnlyAndLedgerUnaffected(t *testing.T) {
	s, _, _, pending, processing := quiesceTestBoard(t)

	s.EnterQuiesce()

	if avail, err := s.QueryAvailable("", ""); err != nil || len(avail) != 1 || avail[0].ID != pending.ID {
		t.Errorf("静默期间 QueryAvailable 应正常返回 pending 任务，实际 %v err=%v", avail, err)
	}
	if _, err := s.GetTask(processing.ID); err != nil {
		t.Errorf("静默期间 GetTask 应正常: %v", err)
	}
	if all, _ := s.ScanAll(); len(all) != 2 {
		t.Errorf("静默期间 ScanAll 应返回 2 个任务，实际 %d", len(all))
	}
	if snaps := s.ExportSnapshot(); len(snaps) != 2 {
		t.Errorf("静默期间 ExportSnapshot 应返回 2 条，实际 %d", len(snaps))
	}
	if _, err := s.GetDependencyResults(processing.ID); err != nil {
		t.Errorf("静默期间 GetDependencyResults 应正常: %v", err)
	}

	// 执行账本写（Reset 前存活 agent 的正常路径）不受限
	if err := s.AppendOutput("agent-1", processing.ID, "部分输出"); err != nil {
		t.Errorf("静默期间 AppendOutput 应不受限: %v", err)
	}
	if err := s.AppendToolCall(processing.ID, ToolCallRecord{ToolName: "read_file"}); err != nil {
		t.Errorf("静默期间 AppendToolCall 应不受限: %v", err)
	}
	if err := s.RecordLastHistory(processing.ID, []byte("[]")); err != nil {
		t.Errorf("静默期间 RecordLastHistory 应不受限: %v", err)
	}
	if err := s.RecordLastResponse(processing.ID, "最后响应"); err != nil {
		t.Errorf("静默期间 RecordLastResponse 应不受限: %v", err)
	}
	if err := s.AppendArtifact(processing.ID, "out.md"); err != nil {
		t.Errorf("静默期间 AppendArtifact 应不受限: %v", err)
	}
	// 租约已预置，再次冻结走复用路径（非迁移），同样不受限
	if _, _, err := s.FreezeTaskLease(processing.ID, newTestLease(processing.ID)); err != nil {
		t.Errorf("静默期间 FreezeTaskLease 应不受限: %v", err)
	}
}

// TestQuiesce_ReplaceSnapshotAllowedDuringWindow 解冻路径关键断言：静默
// 期间 ReplaceSnapshot 不受限——第⑤步正是靠它把目标 session 任务换上板。
func TestQuiesce_ReplaceSnapshotAllowedDuringWindow(t *testing.T) {
	s, _, _, pending, processing := quiesceTestBoard(t)

	s.EnterQuiesce()

	err := s.ReplaceSnapshot([]session.TaskSnapshot{snapTask("thawed-1", "pending")})
	if err != nil {
		t.Fatalf("静默期间 ReplaceSnapshot 应不受限: %v", err)
	}
	for _, id := range []string{pending.ID, processing.ID} {
		if _, err := s.GetTask(id); err != ErrTaskNotFound {
			t.Errorf("替换后旧任务 %s 应消失，实际 err = %v", id, err)
		}
	}
	if got, err := s.GetTask("thawed-1"); err != nil || got.Status != model.TaskStatusPending {
		t.Fatalf("替换后 thawed-1 应为 pending，实际 err = %v", err)
	}

	// 退出静默后新任务可正常认领
	s.ExitQuiesce()
	if err := s.ClaimTask("agent-9", "thawed-1"); err != nil {
		t.Fatalf("退出静默后认领 thawed-1 失败: %v", err)
	}
}

// TestQuiesce_ExitRestores 退出静默后全部状态迁移入口恢复。
func TestQuiesce_ExitRestores(t *testing.T) {
	s, ch, _, pending, processing := quiesceTestBoard(t)

	s.EnterQuiesce()
	requireQuiesced(t, s.SubmitResult("agent-1", processing.ID, "done"), "SubmitResult")

	s.ExitQuiesce()

	newTask := publishTestTask(t, s, "恢复后新任务")
	if err := s.ClaimTask("agent-2", pending.ID); err != nil {
		t.Fatalf("退出静默后 ClaimTask 应恢复: %v", err)
	}
	if err := s.SubmitResult("agent-1", processing.ID, "done"); err != nil {
		t.Fatalf("退出静默后 SubmitResult 应恢复: %v", err)
	}
	if got, _ := s.GetTask(processing.ID); got.Status != model.TaskStatusCompleted {
		t.Errorf("退出静默后提交应生效为 completed，实际 %s", got.Status)
	}
	if got, _ := s.GetTask(newTask.ID); got.Status != model.TaskStatusPending {
		t.Errorf("新发布任务应为 pending，实际 %s", got.Status)
	}
	if n := countBoardEvents(drainEvents(ch), model.EventTaskCompleted); n != 1 {
		t.Errorf("退出静默后应发出 1 条 completed 事件，实际 %d", n)
	}
}

// TestQuiesce_IdempotentEnterExit 重复 Enter/Exit 不 panic 且语义正确：
// 双 Enter 后围栏仍生效，双 Exit 后 store 完全恢复。
func TestQuiesce_IdempotentEnterExit(t *testing.T) {
	s, _ := newTestStore(10, 100)

	s.EnterQuiesce()
	s.EnterQuiesce() // 重复 Enter 不 panic
	requireQuiesced(t, s.PublishTask(&model.Task{Description: "x"}), "PublishTask")

	s.ExitQuiesce()
	s.ExitQuiesce() // 重复 Exit 不 panic
	if err := s.PublishTask(&model.Task{Description: "恢复"}); err != nil {
		t.Fatalf("双 Exit 后 PublishTask 应恢复: %v", err)
	}
}

// TestQuiesceHelpers iface 助手：MemoryTaskStore 走可选接口生效；不支持
// 该能力的 Store 返回中文错误。
func TestQuiesceHelpers(t *testing.T) {
	s, _ := newTestStore(10, 100)
	var ts TaskStore = s

	if err := EnterQuiesce(ts); err != nil {
		t.Fatalf("EnterQuiesce 助手: %v", err)
	}
	requireQuiesced(t, s.PublishTask(&model.Task{Description: "x"}), "PublishTask")
	if err := ExitQuiesce(ts); err != nil {
		t.Fatalf("ExitQuiesce 助手: %v", err)
	}
	if err := s.PublishTask(&model.Task{Description: "恢复"}); err != nil {
		t.Fatalf("助手退出静默后 PublishTask 应恢复: %v", err)
	}

	if err := EnterQuiesce(unsupportedFakeStore{}); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Errorf("不支持的 Store 应报「不支持」错误，实际 err = %v", err)
	}
	if err := ExitQuiesce(unsupportedFakeStore{}); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Errorf("不支持的 Store 应报「不支持」错误，实际 err = %v", err)
	}
}
