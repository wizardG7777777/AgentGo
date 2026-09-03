package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestSubmitChangeDecisionValidatesMutationPlanAndEvidenceExpansion(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "change-work", Description: "修复", EventType: "code",
		GraphNodeKind: string(graph.KindAgent), GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV4,
		ContextInputs: []model.TaskContextInput{{Kind: model.TaskContextUpstreamResult,
			SourceRef: "graph-result:recovery@1", Content: recoveryV4InputForToolTest([]string{"src/a.py"})}},
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := tasks.GetTask(task.ID)
	registry := agent.NewToolRegistry()
	PlanControlGroup{Store: tasks, Holder: &fakeHolder{id: task.ID}, AgentID: "worker",
		FinalizationNotifier: &fakeFinalizationNotifier{}, SubmitState: agent.NewSubmitState()}.Register(registry)

	need := llm.ToolCall{Name: "submit_change_decision", Arguments: map[string]any{
		"decision": "need_context", "path": "src/b.py", "reason": "需要直接调用点", "summary": "扩展证据",
	}}
	result, err := registry.Dispatch(context.Background(), need)
	if err != nil || !strings.Contains(result, `"decision":"need_context"`) {
		t.Fatalf("need_context 应形成 typed receipt: result=%s err=%v", result, err)
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{AttemptID: current.AttemptID,
		CallID: "need-b", ToolName: "submit_change_decision", Args: need.Arguments, Success: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Dispatch(context.Background(), need); err == nil || !strings.Contains(err.Error(), "不得重复扩展") {
		t.Fatalf("重复 need_context 必须拒绝: %v", err)
	}
	edit := llm.ToolCall{Name: "submit_change_decision", Arguments: map[string]any{
		"decision": "edit", "edit_steps": []any{
			map[string]any{"tool": "edit_file", "path": "src/a.py"},
			map[string]any{"tool": "edit_file", "path": "src/a.py"},
			map[string]any{"tool": "write_file", "path": "tests/new_test.py"},
		}, "summary": "执行三步修改",
	}}
	result, err = registry.Dispatch(context.Background(), edit)
	if err != nil {
		t.Fatalf("合法 edit decision 被拒绝: %v", err)
	}
	var receipt changeDecisionReceipt
	if json.Unmarshal([]byte(result), &receipt) != nil || len(receipt.EditSteps) != 3 ||
		receipt.EditSteps[2].Tool != "write_file" || receipt.EditSteps[2].Path != "tests/new_test.py" {
		t.Fatalf("edit decision receipt 非法: %s", result)
	}
	edit.Arguments["edit_steps"] = []any{map[string]any{"tool": "write_file", "path": "../escape.py"}}
	if _, err := registry.Dispatch(context.Background(), edit); err == nil || !strings.Contains(err.Error(), "不得逃逸项目根") {
		t.Fatalf("越出 ProjectRoot 的 edit step 必须拒绝: %v", err)
	}
}

func TestSubmitChangeDecisionHypothesisRejectedFinalizesBlocked(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "reject-work", Description: "修复", EventType: "code",
		GraphNodeKind: string(graph.KindAgent), GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV4,
		ContextInputs: []model.TaskContextInput{{Kind: model.TaskContextUpstreamResult,
			SourceRef: "graph-result:recovery@1", Content: recoveryV4InputForToolTest([]string{"src/a.py"})}},
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	notifier := &fakeFinalizationNotifier{}
	state := agent.NewSubmitState()
	registry := agent.NewToolRegistry()
	PlanControlGroup{Store: tasks, Holder: &fakeHolder{id: task.ID}, AgentID: "worker",
		FinalizationNotifier: notifier, SubmitState: state}.Register(registry)
	_, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "submit_change_decision", Arguments: map[string]any{
		"decision": "hypothesis_rejected", "reason": "目标文件不包含失败调用链", "summary": "拒绝当前假设",
	}})
	if err != nil || !notifier.marked {
		t.Fatalf("hypothesis_rejected 必须进入结构化 blocked finalizing: marked=%t err=%v", notifier.marked, err)
	}
	submission, ok := state.Take(task.ID)
	if !ok || submission.Status != agent.SubmitStatusBlocked || !strings.Contains(submission.ResultJSON, "hypothesis_rejected") {
		t.Fatalf("blocked submission 未保留 change decision: %+v ok=%t", submission, ok)
	}
}

func TestSubmitChangeDecisionV5ResumeCandidateRequiresDirtyAuthority(t *testing.T) {
	for _, test := range []struct {
		name       string
		dirtyPaths []string
		wantError  bool
	}{
		{name: "非空候选", dirtyPaths: []string{"src/a.py"}},
		{name: "空候选", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := &model.Task{ID: "resume-work", Description: "恢复", EventType: "code",
				GraphNodeKind: string(graph.KindAgent), GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV5,
				ContextInputs: []model.TaskContextInput{{Kind: model.TaskContextUpstreamResult,
					SourceRef: "graph-result:recovery@1", Content: recoveryV5InputForToolTest([]string{"src/a.py"}, test.dirtyPaths)}},
			}
			if err := tasks.PublishTask(task); err != nil {
				t.Fatal(err)
			}
			if err := tasks.ClaimTask("worker", task.ID); err != nil {
				t.Fatal(err)
			}
			registry := agent.NewToolRegistry()
			PlanControlGroup{Store: tasks, Holder: &fakeHolder{id: task.ID}, AgentID: "worker",
				FinalizationNotifier: &fakeFinalizationNotifier{}, SubmitState: agent.NewSubmitState()}.Register(registry)
			result, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "submit_change_decision", Arguments: map[string]any{
				"decision": "resume_candidate", "summary": "继续验证已有候选",
			}})
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "非空 dirty candidate") {
					t.Fatalf("空 candidate 必须拒绝 resume: result=%s err=%v", result, err)
				}
				return
			}
			if err != nil || !strings.Contains(result, `"schema":"agentgo.change-decision/v2"`) ||
				!strings.Contains(result, `"decision":"resume_candidate"`) {
				t.Fatalf("v5 resume receipt 非法: result=%s err=%v", result, err)
			}
		})
	}
}

func TestSubmitChangeDecisionV5RejectsUnreadEditTarget(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "focus-work", Description: "恢复", EventType: "code",
		GraphNodeKind: string(graph.KindAgent), GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV5,
		ContextInputs: []model.TaskContextInput{{Kind: model.TaskContextUpstreamResult,
			SourceRef: "graph-result:recovery@1", Content: recoveryV5InputForToolTest([]string{"src/a.py"}, nil)}},
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	registry := agent.NewToolRegistry()
	PlanControlGroup{Store: tasks, Holder: &fakeHolder{id: task.ID}, AgentID: "worker",
		FinalizationNotifier: &fakeFinalizationNotifier{}, SubmitState: agent.NewSubmitState()}.Register(registry)
	_, err := registry.Dispatch(context.Background(), llm.ToolCall{Name: "submit_change_decision", Arguments: map[string]any{
		"decision": "edit", "summary": "修改两个文件", "edit_steps": []any{
			map[string]any{"tool": "edit_file", "path": "src/a.py"},
			map[string]any{"tool": "edit_file", "path": "src/b.py"},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "decision=need_context") || !strings.Contains(err.Error(), "src/b.py") {
		t.Fatalf("v5 未读 edit target 必须在冻结 plan 前拒绝: %v", err)
	}
}

func recoveryV4InputForToolTest(files []string) string {
	payload, _ := json.Marshal(map[string]any{
		"target_input": "recovery_directive",
		"result": map[string]any{
			"schema":            graph.RecoveryDeltaSchemaV4,
			"first_action":      map[string]any{"tool": "read_file", "path": files[0]},
			"evidence_contract": map[string]any{"files": files},
		},
	})
	return `<upstream-result authority="graph-dataflow">` + string(payload) + `</upstream-result>`
}

func recoveryV5InputForToolTest(files, dirtyPaths []string) string {
	payload, _ := json.Marshal(map[string]any{
		"target_input": "recovery_directive",
		"result": map[string]any{
			"schema":            graph.RecoveryDeltaSchemaV5,
			"first_action":      map[string]any{"tool": "read_file", "path": files[0]},
			"evidence_contract": map[string]any{"files": files},
			"candidate_state": map[string]any{
				"schema": graph.RecoveryCandidateStateSchemaV1, "source_activation_id": "work@1",
				"delivery_id": "delivery:test", "dirty_paths": dirtyPaths,
			},
		},
	})
	return `<upstream-result authority="graph-dataflow">` + string(payload) + `</upstream-result>`
}
