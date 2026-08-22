package agent

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
)

func TestFreezeToolRouterSnapshotBindsVisibleAndRuntimeView(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("read_file", "读取", map[string]any{
		"type": "object",
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })

	snapshot, err := FreezeToolRouterSnapshot(registry)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID == "" || snapshot.Registry != registry || len(snapshot.Defs) != 1 {
		t.Fatalf("ToolRouterSnapshot 不完整: %+v", snapshot)
	}

	// Defs 是值拷贝；调用方修改切片不能污染 Registry 的 model-visible authority。
	snapshot.Defs[0].Name = "tampered"
	if got := registry.Defs()[0].Name; got != "read_file" {
		t.Fatalf("修改 snapshot defs 污染 Registry: %q", got)
	}
}

func TestSchedulerInvocationToolPolicyMovesThroughAuthoringPhases(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{
		"create_graph_draft", "configure_simple_graph_draft", "read_graph_draft", "patch_graph_draft", "validate_graph_draft",
		"validate_current_graph_draft", "commit_graph_draft", "commit_current_graph_draft", "start_graph", "start_current_graph",
		"read_graph", "get_task_result", "read_content_ref", "propose_graph_change", "read_graph_change",
		"validate_graph_change", "commit_graph_change",
	} {
		full.Register(name, name, map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("task-phase-policy", nil)
	task.EventType, task.EventSource, task.Description = "__scheduler__", "user", "请求"
	initial := deriveInvocationToolPolicy(task, nil, full)
	if initial.Phase != "scheduler:draft-create" || initial.MaxCalls != 1 ||
		!sameExactToolSet(initial.Registry.Names(), []string{"create_graph_draft"}) {
		t.Fatalf("初始 Scheduler phase 工具面错误: phase=%s tools=%v", initial.Phase, initial.Registry.Names())
	}
	initialRouter, err := FreezeToolRouterSnapshotWithPolicy(initial.Registry, initial.Phase, initial.MaxCalls)
	if err != nil {
		t.Fatal(err)
	}
	if choice := invocationToolChoice(initialRouter); choice.Mode != invocation.ToolChoiceFunction || choice.Name != "create_graph_draft" {
		t.Fatalf("draft-create 未冻结 exact ToolChoice: %+v", choice)
	}
	edited := deriveInvocationToolPolicy(task, []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "c1", Name: "create_graph_draft"}},
		ToolResults: []ToolResult{{ToolCallID: "c1", Content: `{"proposal_id":"p1"}`}},
	}}, full)
	if edited.Phase != "scheduler:draft-configure" ||
		!sameExactToolSet(edited.Registry.Names(), []string{"configure_simple_graph_draft"}) {
		t.Fatalf("Draft configure phase 工具面错误: phase=%s tools=%v", edited.Phase, edited.Registry.Names())
	}
	editedRouter, err := FreezeToolRouterSnapshotWithPolicy(edited.Registry, edited.Phase, edited.MaxCalls)
	if err != nil {
		t.Fatal(err)
	}
	if choice := invocationToolChoice(editedRouter); choice.Mode != invocation.ToolChoiceFunction || choice.Name != "configure_simple_graph_draft" {
		t.Fatalf("draft-configure 未冻结 exact ToolChoice: %+v", choice)
	}
	validate := deriveInvocationToolPolicy(task, []HistoryEntry{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "create_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c1", Content: `{"proposal_id":"p1"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "c2", Name: "configure_simple_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c2", Content: `{"draft_revision":2}`}}},
	}, full)
	if validate.Phase != "scheduler:draft-validate" || !sameExactToolSet(validate.Registry.Names(), []string{"validate_current_graph_draft"}) {
		t.Fatalf("Draft validate phase 工具面错误: phase=%s tools=%v", validate.Phase, validate.Registry.Names())
	}
	commit := deriveInvocationToolPolicy(task, append([]HistoryEntry{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "create_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c1", Content: `{"proposal_id":"p1"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "c2", Name: "configure_simple_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c2", Content: `{"draft_revision":2}`}}},
	}, HistoryEntry{ToolCalls: []llm.ToolCall{{ID: "c3", Name: "validate_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c3", Content: `{"accepted":true}`}}}), full)
	if commit.Phase != "scheduler:draft-commit" || !sameExactToolSet(commit.Registry.Names(), []string{"commit_current_graph_draft"}) {
		t.Fatalf("Draft commit phase 工具面错误: phase=%s tools=%v", commit.Phase, commit.Registry.Names())
	}
	reconfigure := deriveInvocationToolPolicy(task, []HistoryEntry{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "create_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c1", Content: `{"proposal_id":"p1"}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "c2", Name: "configure_simple_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c2", Content: `{"draft_revision":2}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "c3", Name: "validate_current_graph_draft"}}, ToolResults: []ToolResult{{ToolCallID: "c3", Content: `{"accepted":false,"errors":[{"code":"EXECUTION_CLASS_MISMATCH"}]}`}}},
	}, full)
	if reconfigure.Phase != "scheduler:draft-configure" || !sameExactToolSet(reconfigure.Registry.Names(), []string{"configure_simple_graph_draft"}) {
		t.Fatalf("simple Validation rejection 应回到高层 configure: phase=%s tools=%v", reconfigure.Phase, reconfigure.Registry.Names())
	}
	task.EventSource = "graph-ended"
	final := deriveInvocationToolPolicy(task, nil, full)
	if final.Phase != "scheduler:final-report" || final.Registry.Missing([]string{"read_graph"}) != nil ||
		containsToolName(final.Registry.Names(), "create_graph_draft") {
		t.Fatalf("Final report phase 工具面错误: phase=%s tools=%v", final.Phase, final.Registry.Names())
	}
}

func TestGraphDeliverablePhaseForcesSubmitTaskResult(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{"read_file", "grep_search", "submit_task_result"} {
		full.Register(name, name, map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("task-deliverable", nil)
	task.GraphID = "graph-1"
	policy := deriveInvocationToolPolicy(task, []HistoryEntry{{SystemNotice: progressDeliverableRequiredMarker}}, full)
	if policy.Phase != "agent:deliverable-submit" || policy.MaxCalls != 1 ||
		!sameExactToolSet(policy.Registry.Names(), []string{"submit_task_result"}) {
		t.Fatalf("deliverable phase 工具面错误: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
	router, err := FreezeToolRouterSnapshotWithPolicy(policy.Registry, policy.Phase, policy.MaxCalls)
	if err != nil {
		t.Fatal(err)
	}
	choice := invocationToolChoice(router)
	if choice.Mode != invocation.ToolChoiceFunction || choice.Name != "submit_task_result" {
		t.Fatalf("deliverable phase 未冻结 exact submit: %+v", choice)
	}
}

func TestSchedulerPhaseDoesNotAdvanceOnFailedAuthoringTool(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{"create_graph_draft", "configure_simple_graph_draft", "read_graph_draft", "patch_graph_draft", "validate_graph_draft"} {
		full.Register(name, name, map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("task-failed-create-phase", nil)
	task.EventType, task.EventSource = "__scheduler__", "user"
	policy := deriveInvocationToolPolicy(task, []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "failed-create", Name: "create_graph_draft"}},
		ToolResults: []ToolResult{{ToolCallID: "failed-create", Content: "错误: execution_class 无效"}},
	}}, full)
	if policy.Phase != "scheduler:draft-create" || !sameExactToolSet(policy.Registry.Names(), []string{"create_graph_draft"}) {
		t.Fatalf("失败 create 错误推进了 phase: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
}

func TestLoopInterventionWakeStartsFreshAuthoringTransaction(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{
		"create_graph_draft", "configure_simple_graph_draft", "read_graph_draft", "patch_graph_draft", "validate_graph_draft",
		"validate_current_graph_draft", "commit_graph_draft", "commit_current_graph_draft", "start_graph", "start_current_graph",
		"read_graph", "get_task_result", "read_content_ref", "propose_graph_change", "read_graph_change",
		"validate_graph_change", "commit_graph_change",
	} {
		full.Register(name, name, map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("loop-intervention-wake-test", nil)
	task.EventType = "__scheduler__"
	task.EventSource = model.TaskEventSourceLoopIntervention
	task.ParentTaskID = "source-root"
	task.RunPhase = runcontract.PhaseRecovery

	initial := deriveInvocationToolPolicy(task, nil, full)
	if initial.Phase != "scheduler:draft-create" || !sameExactToolSet(initial.Registry.Names(), []string{"create_graph_draft"}) {
		t.Fatalf("intervention wake 未从新 Draft transaction 开始: phase=%s tools=%v", initial.Phase, initial.Registry.Names())
	}
	edited := deriveInvocationToolPolicy(task, []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "create", Name: "create_graph_draft"}},
		ToolResults: []ToolResult{{ToolCallID: "create", Content: `{"proposal_id":"p1"}`}},
	}}, full)
	if edited.Phase != "scheduler:draft-configure" || !sameExactToolSet(edited.Registry.Names(), []string{"configure_simple_graph_draft"}) {
		t.Fatalf("intervention authoring phase 未按历史推进: phase=%s tools=%v", edited.Phase, edited.Registry.Names())
	}
	graphWake := *task
	graphWake.ID = "loop-intervention-graph-wake-test"
	graphWake.InterventionGraphID = "graph-existing"
	graphWake.InterventionNodeID = "work"
	graphWake.InterventionActivationID = "work@1"
	recovery := deriveInvocationToolPolicy(&graphWake, nil, full)
	if recovery.Phase != "scheduler:recovery" || recovery.Registry.Missing([]string{"read_graph", "propose_graph_change"}) != nil ||
		containsToolName(recovery.Registry.Names(), "create_graph_draft") {
		t.Fatalf("Graph intervention 被错误路由到新 Draft: phase=%s tools=%v", recovery.Phase, recovery.Registry.Names())
	}
}

func TestSchedulerToolBatchRejectedBeforeAnyDispatch(t *testing.T) {
	runtime := newAgentTestContextRuntime(t)
	mock := &mockLLMClient{responses: []llm.Response{{ToolCalls: []llm.ToolCall{
		{ID: "c1", Name: "create_graph_draft", Arguments: map[string]any{}},
		{ID: "c2", Name: "create_graph_draft", Arguments: map[string]any{}},
	}}}}
	runs := 0
	registry := NewToolRegistry()
	registry.Register("create_graph_draft", "创建 Draft", map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) {
		runs++
		return "created", nil
	})
	executor := NewSwappableLLMExecutor(mock, registry, nil, nil, nil, "", "Scheduler")
	executor.SetContextRuntime(runtime)
	task := replayGateTask("task-scheduler-batch", []string{"create_graph_draft"})
	task.EventType, task.EventSource = "__scheduler__", "user"
	ctx := WithExecutionIdentity(context.Background(), string(task.RunID), task.AttemptID, task.AttemptID+"/turn-1")
	_, err := executor.Execute(ctx, task, nil, nil)
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureMalformedResponse || !errors.Is(failure, failure.Cause) {
		t.Fatalf("Scheduler 多动作批次未形成 malformed_response: err=%v failure=%+v", err, failure)
	}
	if runs != 0 {
		t.Fatalf("批次校验失败后仍执行了 %d 个工具", runs)
	}
}

func containsToolName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
