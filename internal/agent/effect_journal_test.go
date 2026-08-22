package agent

// effect_journal_test.go 覆盖 V6 §4 H2b 在 agent 执行面的埋点：
// workspace 合并（mergeWorkspaceBeforeComplete）产生 never_replay 账——
// 成功路径 settled 载合并结果，冲突路径 settled 载冲突摘要（合并是状态
// 迁移，禁止自动重放，冲突走 replan）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/effect"
	"agentgo/internal/model"
	"agentgo/internal/workspace"
)

// openAgentJournal 打开临时目录下的 Effect Journal 并登记 Windows 句柄清理。
func openAgentJournal(t *testing.T) *effect.Journal {
	t.Helper()
	j, err := effect.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// 成功合并：产生 never_replay 账，settled 载 fast_forward/auto_merged 摘要。
func TestProcessTask_IsolationMergeEffectJournal(t *testing.T) {
	s, r, _ := setup()
	j := openAgentJournal(t)
	rawRoot := t.TempDir()
	realManager := workspace.NewManager(rawRoot, nil)
	mainRoot := realManager.ProjectRoot()
	mgr := &countingManager{real: realManager}
	swapper := workspace.NewSwapper(mainRoot)

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID)
	target := filepath.Join(mainRoot, "out.txt")

	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		wsPath, err := swapper.WritePath(target)
		if err != nil {
			t.Errorf("WritePath: %v", err)
			return ExecuteResult{Output: "done"}, nil
		}
		if err := os.WriteFile(wsPath, []byte("隔离产出"), 0o644); err != nil {
			t.Errorf("写 workspace 副本: %v", err)
		}
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.EffectJournal = j
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", task.Status, task.Error)
	}

	got := j.Query(taskID)
	if len(got) != 1 {
		t.Fatalf("合并应产生 1 条副作用账，实际 %d", len(got))
	}
	e := got[0]
	if e.Kind != effect.KindWorkspaceMerge || e.Policy != effect.PolicyNeverReplay {
		t.Fatalf("合并账应为 workspace_merge/never_replay: %+v", e)
	}
	if e.Status != effect.StatusSettled {
		t.Fatalf("合并成功应 settled，实际 %s", e.Status)
	}
	if !strings.HasPrefix(e.ResultSummary, "merged: ") ||
		!strings.Contains(e.ResultSummary, "fast_forward=") {
		t.Fatalf("ResultSummary 应载合并结果: %q", e.ResultSummary)
	}
}

// 合并冲突：settled 载 conflict 摘要（结果已知——未合并、现场保留走
// replan），任务转 failed。
func TestProcessTask_IsolationMergeConflictEffectJournal(t *testing.T) {
	s, r, _ := setup()
	j := openAgentJournal(t)
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
				},
			}},
		},
	}
	swapper := workspace.NewSwapper(mainRoot)

	const agentID = "agent-iso"
	taskID := publishIsolationTask(t, s, agentID)

	exec := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag := NewAgent(agentID, "code", s, r, exec)
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.EffectJournal = j
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", task.Status)
	}

	got := j.Query(taskID)
	if len(got) != 1 {
		t.Fatalf("冲突合并应产生 1 条副作用账，实际 %d", len(got))
	}
	e := got[0]
	if e.Kind != effect.KindWorkspaceMerge || e.Policy != effect.PolicyNeverReplay {
		t.Fatalf("账目 kind/policy 不符: %+v", e)
	}
	if e.Status != effect.StatusSettled || !strings.Contains(e.ResultSummary, "conflict: files=1") {
		t.Fatalf("冲突结果已知，应 settled 载 conflict 摘要: %+v", e)
	}
}

// workspace merge 的 Prepare 失败必须在 MergeTask 前阻断。
func TestProcessTask_IsolationMergePrepareFailureStopsMerge(t *testing.T) {
	s, r, _ := setup()
	j := openAgentJournal(t)
	if err := j.Close(); err != nil {
		t.Fatalf("Close journal: %v", err)
	}
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
	swapper := workspace.NewSwapper(mainRoot)
	taskID := publishIsolationTask(t, s, "agent-iso")

	ag := NewAgent("agent-iso", "code", s, r,
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			return ExecuteResult{Output: "done"}, nil
		})
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.EffectJournal = j
	ag.processTask(context.Background(), taskID)

	if mgr.mergeCalls != 0 {
		t.Fatalf("Prepare 失败后不得进入 MergeTask，实际 %d", mgr.mergeCalls)
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed || !strings.Contains(task.Error, "may_have_happened=false") {
		t.Fatalf("任务应因 Prepare authority 失败收口: status=%s error=%q", task.Status, task.Error)
	}
}

// closingMergeManager 在真实合并已落盘后关闭 Journal，精确构造
// “副作用已发生、Settle authority 失败”的窗口。
type closingMergeManager struct {
	base    *countingManager
	journal *effect.Journal
}

func (m *closingMergeManager) Materialize(taskID string) (*workspace.View, error) {
	return m.base.Materialize(taskID)
}

func (m *closingMergeManager) MergeTask(ctx context.Context, taskID, agentID string) (*workspace.MergeResult, error) {
	result, err := m.base.MergeTask(ctx, taskID, agentID)
	if err == nil {
		_ = m.journal.Close()
	}
	return result, err
}

func (m *closingMergeManager) Cleanup(taskID string) error { return m.base.Cleanup(taskID) }

func TestProcessTask_IsolationMergeSettleFailureIsAuthorityFailure(t *testing.T) {
	s, r, _ := setup()
	j := openAgentJournal(t)
	rawRoot := t.TempDir()
	realManager := workspace.NewManager(rawRoot, nil)
	mainRoot := realManager.ProjectRoot()
	base := &countingManager{real: realManager}
	mgr := &closingMergeManager{base: base, journal: j}
	swapper := workspace.NewSwapper(mainRoot)
	taskID := publishIsolationTask(t, s, "agent-iso")
	target := filepath.Join(mainRoot, "settle-failed.txt")

	ag := NewAgent("agent-iso", "code", s, r,
		func(context.Context, *model.Task, map[string]string, []HistoryEntry) (ExecuteResult, error) {
			wsPath, err := swapper.WritePath(target)
			if err != nil {
				return ExecuteResult{}, err
			}
			if err := os.WriteFile(wsPath, []byte("已合并但未结算"), 0o644); err != nil {
				return ExecuteResult{}, err
			}
			return ExecuteResult{Output: "done"}, nil
		})
	ag.WorkspaceManager = mgr
	ag.WorkspaceActivator = swapper
	ag.EffectJournal = j
	ag.processTask(context.Background(), taskID)

	if base.mergeCalls != 1 || base.cleanupCalls != 0 {
		t.Fatalf("Settle 失败时应保留 workspace 现场: merge=%d cleanup=%d", base.mergeCalls, base.cleanupCalls)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "已合并但未结算" {
		t.Fatalf("真实合并已发生: data=%q err=%v", data, err)
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed || !strings.Contains(task.Error, "may_have_happened=true") {
		t.Fatalf("Settle 失败应以 authority failure 阻断 completed: status=%s error=%q", task.Status, task.Error)
	}
}
