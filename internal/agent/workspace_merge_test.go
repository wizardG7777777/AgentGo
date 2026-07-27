package agent

// workspace_merge_test.go 覆盖「按任务写时复制执行隔离」在 agent 执行面的行为：
//   - 认领隔离任务时 Materialize + Activate（任务结束 restore）；
//   - 成功终态（自然完成 / finalization 短路两条路径）在 SubmitResult 前合并，
//     成功后 Cleanup——用真 *workspace.Manager（t.TempDir() 主根）断言真实
//     合并产物落回主根；
//   - 合并冲突 → 任务 failed（reason 含 workspace_conflict 与冲突清单）+
//     自动登记高优 ReplanRequest（不 Cleanup 保留现场）；
//   - 认领点 fail-closed（未知模式 / 执行面未装配）；
//   - 无 Isolation 任务零影响；失败路径不 merge 不 Cleanup。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/workspace"
)

// countingManager 包装真 *workspace.Manager 并计数生命周期调用；
// mergeResult/mergeErr 非零时短路返回预置值（构造冲突/失败场景），
// 否则委托真实现（真实 COW + 合并语义）。
type countingManager struct {
	real             *workspace.Manager
	materializeCalls int
	mergeCalls       int
	cleanupCalls     int
	mergeResult      *workspace.MergeResult
	mergeErr         error
}

func (c *countingManager) Materialize(taskID string) (*workspace.View, error) {
	c.materializeCalls++
	return c.real.Materialize(taskID)
}

func (c *countingManager) MergeTask(ctx context.Context, taskID, agentID string) (*workspace.MergeResult, error) {
	c.mergeCalls++
	if c.mergeResult != nil || c.mergeErr != nil {
		return c.mergeResult, c.mergeErr
	}
	return c.real.MergeTask(ctx, taskID, agentID)
}

func (c *countingManager) Cleanup(taskID string) error {
	c.cleanupCalls++
	return c.real.Cleanup(taskID)
}

// fakeReplanRequester 捕获自动登记的 ReplanRequest。
type fakeReplanRequester struct {
	calls int
	req   model.ReplanRequest
	err   error
}

func (f *fakeReplanRequester) RequestReplan(_ context.Context, req model.ReplanRequest) (*model.ReplanRequest, error) {
	f.calls++
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	out := req
	return &out, nil
}

// publishIsolationTask 发布并认领一个带执行隔离声明的任务。
func publishIsolationTask(t *testing.T, s store.TaskStore, agentID, planID string) string {
	t.Helper()
	task := &model.Task{
		Description: "隔离任务",
		EventType:   "code",
		PlanID:      planID,
		Capability:  &model.NodeCapability{Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace}},
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	return task.ID
}

// 自然完成路径（react_loop_exit:natural）：认领时物化并换入视图，执行期
// 经 Swapper.WritePath 的写入落 workspace（真实 COW），任务标记 completed
// 前真合并把新文件落回主根，成功后 Cleanup 删除 workspace 目录并恢复视图。
func TestProcessTask_IsolationNaturalCompletionMergesAndCleansUp(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
	swapper := workspace.NewSwapper(mainRoot)
	replan := &fakeReplanRequester{}

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID, "")
	target := filepath.Join(mainRoot, "out.txt")

	var ag *Agent
	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		// 执行期：视图已换入。
		view := swapper.ActiveView()
		if view == nil {
			t.Error("执行期 Swapper 应有活动视图")
			return ExecuteResult{Output: "done"}, nil
		}
		// 模拟工具层隔离写入：WritePath 解析到 workspace 副本后落盘。
		wsPath, err := swapper.WritePath(target)
		if err != nil {
			t.Errorf("WritePath: %v", err)
			return ExecuteResult{Output: "done"}, nil
		}
		if wsPath == target {
			t.Error("隔离生效时 WritePath 不应原样返回主根路径")
		}
		if err := os.WriteFile(wsPath, []byte("隔离产出内容"), 0o644); err != nil {
			t.Errorf("写 workspace 副本: %v", err)
		}
		// 写入后 ReadPath 应命中副本。
		if got := swapper.ReadPath(target); got != wsPath {
			t.Errorf("写后 ReadPath 应命中副本 %s，实际 %s", wsPath, got)
		}
		return ExecuteResult{Output: "done"}, nil
	}
	ag = NewAgent(agentID, "code", s, r, exec, 5)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.WorkspaceReplanRequester = replan
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", task.Status, task.Error)
	}
	if mgr.materializeCalls != 1 || mgr.mergeCalls != 1 || mgr.cleanupCalls != 1 {
		t.Fatalf("生命周期调用 = 物化 %d / 合并 %d / 清理 %d，want 1/1/1",
			mgr.materializeCalls, mgr.mergeCalls, mgr.cleanupCalls)
	}
	// 真合并产物：新文件落回主根。
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "隔离产出内容" {
		t.Fatalf("合并后主根应有隔离产出: data=%q err=%v", data, err)
	}
	// Cleanup 删除 workspace 目录（含 manifest 与副本）。
	wsDir := filepath.Join(mainRoot, ".agentgo", "workspaces", taskID)
	if _, err := os.Stat(wsDir); !os.IsNotExist(err) {
		t.Fatalf("Cleanup 后 workspace 目录应被删除: stat err=%v", err)
	}
	// 任务结束视图恢复；Manager 不再持有活动视图。
	if swapper.ActiveView() != nil {
		t.Fatal("任务结束后 Swapper 视图应恢复为 nil")
	}
	if mgr.real.ActiveView(taskID) != nil {
		t.Fatal("Cleanup 后 Manager 不应再持有活动视图")
	}
	// 无冲突不登记 replan。
	if replan.calls != 0 {
		t.Fatalf("成功路径不应登记 replan，实际 %d 次", replan.calls)
	}
}

// finalization 短路路径（submit_task_result 同款出口）：合并同样发生在
// SubmitResult 之前。executor 在首轮标记 finalized 并继续调工具，下一轮
// loop 顶部短路完成。
func TestProcessTask_IsolationShortCircuitMergesBeforeComplete(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
	swapper := workspace.NewSwapper(mainRoot)

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID, "")
	target := filepath.Join(mainRoot, "short.txt")

	holder := NewFinalizationHolder()
	execCalls := 0
	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		execCalls++
		wsPath, err := swapper.WritePath(target)
		if err != nil {
			t.Errorf("WritePath: %v", err)
		} else if err := os.WriteFile(wsPath, []byte("短路产出"), 0o644); err != nil {
			t.Errorf("写 workspace 副本: %v", err)
		}
		holder.MarkTaskFinalized() // 模拟 submit_task_result 成功后的信号
		return ExecuteResult{Output: "已提交", ToolCalled: true}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	ag.FinalizationChecker = holder
	ag.OnTaskStart = func(id string) { holder.Set(id) }
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", task.Status, task.Error)
	}
	if mgr.mergeCalls != 1 || mgr.cleanupCalls != 1 {
		t.Fatalf("短路路径合并/清理调用 = %d/%d，want 1/1", mgr.mergeCalls, mgr.cleanupCalls)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "短路产出" {
		t.Fatalf("合并后主根应有短路产出: data=%q err=%v", data, err)
	}
}

// 合并冲突：任务转 failed（reason 含 workspace_conflict 与冲突文件清单）、
// 自动登记高优 ReplanRequest（冲突文件与冲突区域数写进原因）、不 Cleanup。
func TestProcessTask_IsolationMergeConflictFailsAndReplans(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	conflictedPath := filepath.Join(mainRoot, "conflicted.txt")
	mgr := &countingManager{
		real: workspace.NewManager(mainRoot, nil),
		mergeResult: &workspace.MergeResult{
			Conflicted: true,
			Reports: []workspace.FileReport{{
				Path:    conflictedPath,
				Outcome: workspace.OutcomeConflict,
				Conflicts: []workspace.ConflictRegion{
					{BaseStart: 3, BaseEnd: 5},
					{BaseStart: 9, BaseEnd: 9},
				},
			}},
		},
	}
	swapper := workspace.NewSwapper(mainRoot)
	replan := &fakeReplanRequester{}

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID, "plan-1")

	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.WorkspaceReplanRequester = replan
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", task.Status)
	}
	if !strings.Contains(task.Error, "workspace_conflict") || !strings.Contains(task.Error, conflictedPath) {
		t.Fatalf("失败原因应含 workspace_conflict 与冲突文件清单，实际: %s", task.Error)
	}
	if mgr.mergeCalls != 1 {
		t.Fatalf("合并调用 = %d，want 1", mgr.mergeCalls)
	}
	if mgr.cleanupCalls != 0 {
		t.Fatalf("冲突保留现场，不应 Cleanup，实际 %d 次", mgr.cleanupCalls)
	}
	// 自动 replan：同款 RequestReplan 通道，高优，原因含冲突文件与区域数。
	if replan.calls != 1 {
		t.Fatalf("应自动登记 1 次 replan，实际 %d", replan.calls)
	}
	req := replan.req
	if req.PlanID != "plan-1" || req.SourceTaskID != taskID {
		t.Fatalf("replan 归属错误: PlanID=%s SourceTaskID=%s", req.PlanID, req.SourceTaskID)
	}
	if req.ReasonCode != "workspace_conflict" {
		t.Fatalf("ReasonCode = %s，want workspace_conflict", req.ReasonCode)
	}
	if req.Urgency != model.ReplanUrgencyHigh {
		t.Fatalf("Urgency = %s，want high", req.Urgency)
	}
	if !strings.Contains(req.Detail, conflictedPath) || !strings.Contains(req.Detail, "2 处冲突区域") {
		t.Fatalf("replan 原因应含冲突文件与冲突区域数，实际: %s", req.Detail)
	}
}

// 合并执行错误（非冲突）：同样转 failed + 自动 replan。
func TestProcessTask_IsolationMergeErrorFailsAndReplans(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	mgr := &countingManager{
		real:     workspace.NewManager(mainRoot, nil),
		mergeErr: errors.New("模拟合并 IO 故障"),
	}
	swapper := workspace.NewSwapper(mainRoot)
	replan := &fakeReplanRequester{}

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID, "plan-1")

	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.WorkspaceReplanRequester = replan
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", task.Status)
	}
	if !strings.Contains(task.Error, "workspace_conflict") || !strings.Contains(task.Error, "模拟合并 IO 故障") {
		t.Fatalf("失败原因应含 workspace_conflict 与底层错误，实际: %s", task.Error)
	}
	if mgr.cleanupCalls != 0 {
		t.Fatalf("失败保留现场，不应 Cleanup，实际 %d 次", mgr.cleanupCalls)
	}
	if replan.calls != 1 || replan.req.ReasonCode != "workspace_conflict" {
		t.Fatalf("应登记 1 次 workspace_conflict replan，实际 %+v", replan.req)
	}
}

// 认领点 fail-closed：未知隔离模式直接失败，executor 从未执行。
func TestProcessTask_IsolationFailClosedUnknownMode(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
	swapper := workspace.NewSwapper(mainRoot)

	const agentID = "agent-iso"
	task := &model.Task{
		Description: "未知隔离模式任务",
		EventType:   "code",
		Capability:  &model.NodeCapability{Isolation: &model.IsolationSpec{Mode: "git-worktree"}},
	}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	execCalls := 0
	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		execCalls++
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", got.Status)
	}
	if !strings.Contains(got.Error, "隔离模式") {
		t.Fatalf("失败原因应指出未知隔离模式，实际: %s", got.Error)
	}
	if execCalls != 0 {
		t.Fatalf("fail-closed 不应执行 executor，实际 %d 次", execCalls)
	}
	if mgr.materializeCalls != 0 {
		t.Fatalf("未知模式不应物化 workspace，实际 %d 次", mgr.materializeCalls)
	}
}

// 认领点 fail-closed：执行面未装配（WorkspaceManager/WorkspaceActivator 为 nil）。
func TestProcessTask_IsolationFailClosedUnassembled(t *testing.T) {
	s, r, _ := setup()

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID, "")

	execCalls := 0
	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		execCalls++
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	// 不装配 WorkspaceManager / WorkspaceActivator
	ag.processTask(context.Background(), taskID)

	got, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", got.Status)
	}
	if !strings.Contains(got.Error, "未装配") {
		t.Fatalf("失败原因应指出执行面未装配，实际: %s", got.Error)
	}
	if execCalls != 0 {
		t.Fatalf("fail-closed 不应执行 executor，实际 %d 次", execCalls)
	}
}

// 无 Isolation 任务：装配了 Manager/Activator 也零开销短路——不物化、
// 不合并、不清理，行为完全不变。
func TestProcessTask_NoIsolationZeroOverhead(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
	swapper := workspace.NewSwapper(mainRoot)
	replan := &fakeReplanRequester{}

	const agentID = "agent-iso"
	task := &model.Task{Description: "普通任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		if swapper.ActiveView() != nil {
			t.Error("无 Isolation 任务执行期不应有活动视图")
		}
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.WorkspaceReplanRequester = replan
	ag.processTask(context.Background(), task.ID)

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", got.Status, got.Error)
	}
	if mgr.materializeCalls != 0 || mgr.mergeCalls != 0 || mgr.cleanupCalls != 0 || replan.calls != 0 {
		t.Fatalf("无 Isolation 应零开销短路，实际 物化 %d / 合并 %d / 清理 %d / replan %d",
			mgr.materializeCalls, mgr.mergeCalls, mgr.cleanupCalls, replan.calls)
	}
}

// 失败路径：隔离任务执行失败时不 merge 不 Cleanup（孤儿目录交 Watchdog 清扫）。
func TestProcessTask_IsolationFailureSkipsMergeAndCleanup(t *testing.T) {
	s, r, _ := setup()
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
	swapper := workspace.NewSwapper(mainRoot)

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID, "")

	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, errors.New("unrecoverable boom")
	}
	ag := NewAgent(agentID, "code", s, r, exec, 5)
	ag.MaxRetries = 1
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.processTask(context.Background(), taskID)

	got, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", got.Status)
	}
	if mgr.materializeCalls != 1 {
		t.Fatalf("认领时应物化 1 次，实际 %d", mgr.materializeCalls)
	}
	if mgr.mergeCalls != 0 || mgr.cleanupCalls != 0 {
		t.Fatalf("失败路径不应 merge/Cleanup，实际 合并 %d / 清理 %d", mgr.mergeCalls, mgr.cleanupCalls)
	}
	// 现场保留：workspace 目录与活动视图仍在（交 Watchdog 经 ListOrphans 清扫）。
	wsDir := filepath.Join(mainRoot, ".agentgo", "workspaces", taskID)
	if _, err := os.Stat(wsDir); err != nil {
		t.Fatalf("失败路径应保留 workspace 现场: stat err=%v", err)
	}
	if mgr.real.ActiveView(taskID) == nil {
		t.Fatal("失败路径 Manager 应保留活动视图")
	}
}
