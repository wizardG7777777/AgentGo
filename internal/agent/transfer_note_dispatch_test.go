package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"agentgo/internal/model"
)

// handleFailure 的 TransferNote 分类策略回归锁（2026-04-25 引入）：
//
//   - overflow 场景   → 调 L1（允许一次 LLM 压缩调用）
//   - willTerminate  → 调 L1（下游 + crashReport 会读）
//   - 其他 transient → 走 L3 mechanical，零 LLM 调用
//
// 这组测试通过"记录 executor 是否在 handleFailure 之后被再调用一次"
// 区分 L1 路径和 L3 路径。L1 会触发 generateTransferNote → a.Execute；
// L3 是纯代码，不碰 executor。

// captureExec 包装一个 main executor，能独立记录"handleFailure 后的 L1 调用"。
// 做法：记录 callCount；当 callCount 超过"主调用应有的次数"时说明触发了 L1。
type captureCall struct {
	total atomic.Int32
}

func (c *captureCall) inc() { c.total.Add(1) }
func (c *captureCall) n() int {
	return int(c.total.Load())
}

func TestHandleFailure_TransientRecoverable_SkipsL1(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "transient fail", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-skipL1", task.ID); err != nil {
		t.Fatal(err)
	}

	cap := &captureCall{}
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		cap.inc()
		return ExecuteResult{}, &ErrRecoverable{Err: errors.New("network timeout")}
	}

	ag := NewAgent("agent-skipL1", "code", s, r, executor, 5)
	ag.MaxRetries = 3 // 3 次重试上限——第 1 次失败远未到 terminal，不该调 L1
	ag.processTask(context.Background(), task.ID)

	// 一次 processTask，一次主 executor 调用。若 L1 触发，会再有第 2 次调用。
	if cap.n() != 1 {
		t.Errorf("executor 调用次数 = %d, want 1（普通 transient 应跳过 L1）", cap.n())
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending（RetryRollback 后）", got.Status)
	}
	// L3 mechanical 应该写入了 note——格式含 level="raw" 标签
	if got.TransferNote == "" {
		t.Error("TransferNote 为空——L3 兜底应该始终写入非空")
	}
	if !strings.Contains(got.TransferNote, "level=\"raw\"") {
		t.Errorf("TransferNote 不包含 L3 标记：\n%s", got.TransferNote)
	}
}

func TestHandleFailure_TerminalFailure_TriggersL1(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "will terminate", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	cap := &captureCall{}
	// executor 永远失败，L1 也失败——L1 被调用但降级到 L3。
	// 关键观察：终态时 executor 被调用 2 次（主 + L1），而 transient 只被调用 1 次。
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		cap.inc()
		return ExecuteResult{}, &ErrRecoverable{Err: errors.New("still failing")}
	}

	ag := NewAgent("agent-terminal", "code", s, r, executor, 5)
	ag.MaxRetries = 1 // 让第二次失败就触发 terminal

	// iter 1：RetryCount 从 0 → 1（普通 transient，跳过 L1，1 次 executor 调用）
	if err := s.ClaimTask("agent-terminal", task.ID); err != nil {
		t.Fatal(err)
	}
	ag.processTask(context.Background(), task.ID)
	firstIterCalls := cap.n()
	if firstIterCalls != 1 {
		t.Errorf("iter 1 executor 调用 = %d, want 1（transient skip L1）", firstIterCalls)
	}

	// iter 2：RetryCount=1 >= MaxRetries=1，willTerminate=true，走 L1 路径（+1 次 executor 调用）
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("iter 1 后 status = %s, want pending", got.Status)
	}
	if err := s.ClaimTask("agent-terminal", task.ID); err != nil {
		t.Fatal(err)
	}
	ag.processTask(context.Background(), task.ID)

	secondIterCalls := cap.n() - firstIterCalls
	if secondIterCalls != 2 {
		t.Errorf("iter 2 executor 调用 = %d, want 2（terminal 触发 L1：主 + L1）", secondIterCalls)
	}

	got, err = s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
}

func TestHandleFailure_ContextOverflow_TriggersL1(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "overflow fail", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-overflow", task.ID); err != nil {
		t.Fatal(err)
	}

	cap := &captureCall{}
	// 用 "length" / "截断" / "context" 这类关键字触发 isContextOverflow
	// （参考 agent.go::isContextOverflow 的匹配规则）
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		cap.inc()
		return ExecuteResult{}, &ErrRecoverable{Err: errors.New("prompt exceeds context length")}
	}

	ag := NewAgent("agent-overflow", "code", s, r, executor, 5)
	ag.MaxRetries = 5 // 远未到 terminal——willTerminate=false，但 overflow=true 应触发 L1
	ag.processTask(context.Background(), task.ID)

	if cap.n() != 2 {
		t.Errorf("executor 调用 = %d, want 2（overflow 应触发 L1：主 + L1）", cap.n())
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending（非 terminal）", got.Status)
	}
	if got.TransferNote == "" {
		t.Error("TransferNote 为空——overflow 路径应该有 note")
	}
}

// TestHandleFailure_Unrecoverable_UsesL3Only 确认不可恢复错误分支
// 的行为不变（本次重构不触碰这条路径——它早就只用 L3）。
func TestHandleFailure_Unrecoverable_UsesL3Only(t *testing.T) {
	s, r, _ := setup()
	task := &model.Task{Description: "unrecoverable", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-unrecov", task.ID); err != nil {
		t.Fatal(err)
	}

	cap := &captureCall{}
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		cap.inc()
		return ExecuteResult{}, errors.New("authentication denied") // 非 ErrRecoverable
	}

	ag := NewAgent("agent-unrecov", "code", s, r, executor, 5)
	ag.MaxRetries = 5
	ag.processTask(context.Background(), task.ID)

	if cap.n() != 1 {
		t.Errorf("executor 调用 = %d, want 1（不可恢复错误 L3 only）", cap.n())
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
}
