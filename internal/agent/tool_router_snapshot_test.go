package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/graph"
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

func TestAutoSingletonPhaseRejectsMismatchedToolRouter(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("read_file", "读取", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) { return "ok", nil })
	if _, err := FreezeToolRouterSnapshotWithPolicy(
		registry, "scheduler:draft-create", defaultToolCallsPerResponse,
	); err == nil || !strings.Contains(err.Error(), "auto-singleton") {
		t.Fatalf("错误工具面不得借 required 扩大行动权: %v", err)
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
	if initial.Phase != "scheduler:draft-create" || initial.MaxCalls != defaultToolCallsPerResponse ||
		!sameExactToolSet(initial.Registry.Names(), []string{"create_graph_draft"}) {
		t.Fatalf("初始 Scheduler phase 工具面错误: phase=%s tools=%v", initial.Phase, initial.Registry.Names())
	}
	initialRouter, err := FreezeToolRouterSnapshotWithPolicy(initial.Registry, initial.Phase, initial.MaxCalls)
	if err != nil {
		t.Fatal(err)
	}
	if choice := invocationToolChoice(initialRouter); choice.Mode != invocation.ToolChoiceAuto || choice.Name != "" {
		t.Fatalf("draft-create 未冻结 auto-singleton ToolChoice: %+v", choice)
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
	if choice := invocationToolChoice(editedRouter); choice.Mode != invocation.ToolChoiceAuto || choice.Name != "" {
		t.Fatalf("draft-configure 未冻结 auto-singleton ToolChoice: %+v", choice)
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
	if final.Phase != "scheduler:final-report" || final.MaxCalls != defaultToolCallsPerResponse ||
		final.Registry.Missing([]string{"read_graph"}) != nil ||
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
	if policy.Phase != "agent:deliverable-submit" || policy.MaxCalls != defaultToolCallsPerResponse ||
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

func TestDeliverableHistoryProjectionDropsHistoricalToolsButKeepsControlNotices(t *testing.T) {
	history := []HistoryEntry{
		{AssistantContent: "调查", ToolCalls: []llm.ToolCall{{ID: "read", Name: "read_file"}},
			ToolResults: []ToolResult{{ToolCallID: "read", Content: "source"}}},
		{SystemNotice: progressDeliverableRequiredMarker + " 请提交"},
		{SystemNotice: "<loop-reminder>stop exploring</loop-reminder>",
			ToolCalls: []llm.ToolCall{{ID: "grep", Name: "grep_search"}}},
	}
	projected := deliverableHistoryProjection(history)
	if len(projected) != 2 || !strings.Contains(projected[0].SystemNotice, progressDeliverableRequiredMarker) ||
		len(projected[0].ToolCalls) != 0 || len(projected[1].ToolCalls) != 0 ||
		projected[0].AssistantContent != "" || projected[1].AssistantContent != "" {
		t.Fatalf("强制交付投影不得携带历史工具偏好: %+v", projected)
	}
}

func TestDeliverablePhasePromptRevokesHistoricalToolGuidance(t *testing.T) {
	for _, required := range []string{
		"only legal action", "submit_task_result", "unavailable now", "Do not answer with text",
	} {
		if !strings.Contains(agentDeliverablePhasePrompt, required) {
			t.Fatalf("强制交付 phase contract 缺少 %q", required)
		}
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

func TestSchedulerPhaseIgnoresProviderDuplicateSkippedResult(t *testing.T) {
	history := []HistoryEntry{
		{
			ToolCalls: []llm.ToolCall{{ID: "create-1", Name: "create_graph_draft"}, {ID: "create-2", Name: "create_graph_draft"}},
			ToolResults: []ToolResult{
				{ToolCallID: "create-1", Content: `{"proposal_id":"p1"}`},
				{ToolCallID: "create-2", Content: "已跳过：auto-singleton 阶段只执行首个工具调用"},
			},
		},
		{
			ToolCalls: []llm.ToolCall{{ID: "configure-1", Name: "configure_simple_graph_draft"}, {ID: "configure-2", Name: "configure_simple_graph_draft"}},
			ToolResults: []ToolResult{
				{ToolCallID: "configure-1", Content: `{"draft_revision":2}`},
				{ToolCallID: "configure-2", Content: "已跳过: auto-singleton duplicate"},
			},
		},
		{
			ToolCalls: []llm.ToolCall{{ID: "validate-1", Name: "validate_current_graph_draft"}, {ID: "validate-2", Name: "validate_current_graph_draft"}},
			ToolResults: []ToolResult{
				{ToolCallID: "validate-1", Content: `{"accepted":true}`},
				{ToolCallID: "validate-2", Content: "已跳过：auto-singleton duplicate"},
			},
		},
	}
	phase, tools := graphAuthoringPolicy(history)
	if phase != "scheduler:draft-commit" || !sameExactToolSet(tools, []string{"commit_current_graph_draft"}) {
		t.Fatalf("provider duplicate skipped result 不得把 accepted Draft 退回 configure: phase=%s tools=%v", phase, tools)
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

func TestGraphRecoveryControllerUsesDedicatedSingleActionPhase(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{
		"commit_graph_change", "get_task_result", "propose_graph_change", "read_content_ref",
		"read_graph", "read_graph_change", "submit_task_result", "validate_graph_change",
		"run_shell", "write_file", "report_done", "patch_graph",
	} {
		full.Register(name, name, map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("graph-recovery-controller", nil)
	task.EventType = "__scheduler__"
	task.GraphID, task.NodeID, task.ActivationID = "g-1", "recovery", "recovery@1"
	task.GraphNodeKind = string(graph.KindController)
	task.GraphControllerRole = string(graph.ControllerRoleLoopRecovery)
	task.RecoverySourceTaskID = "work-task"
	task.RunPhase = runcontract.PhaseRecovery

	policy := deriveInvocationToolPolicy(task, nil, full)
	want := []string{
		"commit_graph_change", "get_task_result", "propose_graph_change", "read_content_ref",
		"read_graph", "read_graph_change", "submit_task_result", "validate_graph_change",
	}
	if policy.Phase != "scheduler:graph-recovery" || !sameExactToolSet(policy.Registry.Names(), want) ||
		!phaseRequiresToolCall(policy.Phase) || !phaseDispatchesOnlyFirstTool(policy.Phase) {
		t.Fatalf("Graph recovery phase/ToolRouter 不符: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
}

func TestSchedulerAutoSingletonBatchDispatchesOnlyFirstCall(t *testing.T) {
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
	result, err := executor.Execute(ctx, task, nil, nil)
	if err != nil {
		t.Fatalf("auto-singleton provider fan-out 不应被拒绝: %v", err)
	}
	if runs != 1 || len(result.ToolResults) != 2 ||
		!strings.Contains(result.ToolResults[1].Content, "首个工具调用") {
		t.Fatalf("应只 dispatch 首个并为重复 call_id 返回 skipped result: runs=%d result=%+v", runs, result)
	}
}

func TestSchedulerAutoSingletonRejectsTextOnlyPhaseResponse(t *testing.T) {
	router := ToolRouterSnapshot{Phase: "scheduler:draft-create", MaxCalls: defaultToolCallsPerResponse}
	if err := validateToolCallBatch(router, nil); err == nil || !strings.Contains(err.Error(), "未返回必需") {
		t.Fatalf("auto wire 不得让正文越过机械阶段: %v", err)
	}
}

func TestSchedulerRecoveryAllowsProviderFanoutButKeepsSingleDispatchPolicy(t *testing.T) {
	if !phaseDispatchesOnlyFirstTool("scheduler:recovery") || !phaseRequiresToolCall("scheduler:recovery") {
		t.Fatal("recovery 必须要求工具调用且只 dispatch 首个")
	}
	if phaseDispatchesOnlyFirstTool("scheduler:final-report") {
		t.Fatal("final-report 的纯读批次应全部串行执行")
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
