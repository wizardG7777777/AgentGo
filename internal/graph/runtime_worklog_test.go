package graph

// runtime_worklog_test.go 覆盖上游工作记录的数据流冻结链（2026-08-21
// 上游摘要）：转移结算时 provider 按来源 Task ID 渲染一次 → EdgeInput
// 冻结（durable）→ InputBinding 透传 → 下游任务发布只读冻结文本。
// 无 provider 时行为同今（WorkLog 恒空）。

import (
	"strings"
	"testing"
)

// TestEdgeInputWorkLogFrozen provider 渲染结果随生效边冻结进
// TransitionRecord.Input，并透传到目标 activation 的 InputBinding。
func TestEdgeInputWorkLogFrozen(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	var gotTaskID string
	rt.SetWorkLogProvider(func(taskID string) string {
		gotTaskID = taskID
		return "read_file×3, edit_file×1 (exit≠0: 0)\n编辑文件: a.py"
	})
	mustSubmitRuntime(t, rt, inputGraphJSON)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-input", NodeID: "implement", ActivationID: "implement@1",
		TaskID: "task-1", Status: NodeCompleted,
		Result: map[string]any{"event": "ready"},
	})

	// provider 必须按来源 activation 的 Task ID 调用。
	if gotTaskID != "task-1" {
		t.Fatalf("provider 应按来源 Task ID 调用，实际收到 %q", gotTaskID)
	}

	// durable 层：TransitionRecord.Input.WorkLog 已冻结。
	var rec *TransitionRecord
	for i, tr := range s.Transitions("g-input") {
		if tr.SourceNodeID == "implement" {
			rec = &s.Transitions("g-input")[i]
		}
	}
	if rec == nil || rec.Input == nil {
		t.Fatal("应存在 implement 的生效转移记录")
	}
	if !strings.Contains(rec.Input.WorkLog, "read_file×3") {
		t.Fatalf("TransitionRecord.Input.WorkLog 应冻结 provider 文本: %q", rec.Input.WorkLog)
	}

	// 绑定层：目标 activation 的 InputBinding 透传同一文本。
	verify := nodeOf(t, s, "g-input", "verify")
	if verify.Execution == nil || len(verify.Execution.Input) != 1 {
		t.Fatalf("verify 应有 1 份输入绑定: %+v", verify.Execution)
	}
	if got := verify.Execution.Input[0].WorkLog; !strings.Contains(got, "read_file×3") {
		t.Fatalf("InputBinding.WorkLog 应透传冻结文本: %q", got)
	}
}

// TestEdgeInputWorkLogNilProvider 未注入 provider（老图/直构路径）时
// WorkLog 恒空，行为同今。
func TestEdgeInputWorkLogNilProvider(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, inputGraphJSON)
	mustTerminal(t, rt, TerminalFact{
		GraphID: "g-input", NodeID: "implement", ActivationID: "implement@1",
		TaskID: "task-1", Status: NodeCompleted,
		Result: map[string]any{"event": "ready"},
	})
	verify := nodeOf(t, s, "g-input", "verify")
	if verify.Execution == nil || len(verify.Execution.Input) != 1 {
		t.Fatalf("verify 应有 1 份输入绑定: %+v", verify.Execution)
	}
	if got := verify.Execution.Input[0].WorkLog; got != "" {
		t.Fatalf("无 provider 时 WorkLog 应为空: %q", got)
	}
}
