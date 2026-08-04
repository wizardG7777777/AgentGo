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
	mainRoot := t.TempDir()
	mgr := &countingManager{real: workspace.NewManager(mainRoot, nil)}
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
