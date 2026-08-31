package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentgo/internal/fulfillment"
	"agentgo/internal/graph"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
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
		"validate_graph_change", "commit_graph_change", "submit_graph_change_decision", "report_done",
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
	task.RunPhase, task.FinalReportGraphID = runcontract.PhaseFinalization, "g-final"
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
	reopened := deriveInvocationToolPolicy(task, []HistoryEntry{
		{SystemNotice: progressDeliverableRequiredMarker},
		{ToolCalls: []llm.ToolCall{{ID: "submit", Name: "submit_task_result"}},
			ToolResults: []ToolResult{{ToolCallID: "submit", Content: "错误: reason_code=contract_fulfillment_missing：缺少 required check verification"}}},
	}, full)
	if reopened.Phase == "agent:deliverable-submit" || !containsToolName(reopened.Registry.Names(), "read_file") {
		t.Fatalf("可修复的 fulfillment 拒绝必须重新开放业务工具: phase=%s tools=%v",
			reopened.Phase, reopened.Registry.Names())
	}
	forcedAgain := deriveInvocationToolPolicy(task, append([]HistoryEntry{
		{SystemNotice: progressDeliverableRequiredMarker},
		{ToolCalls: []llm.ToolCall{{ID: "submit", Name: "submit_task_result"}},
			ToolResults: []ToolResult{{ToolCallID: "submit", Content: "错误: reason_code=contract_fulfillment_missing"}}},
	}, HistoryEntry{SystemNotice: progressDeliverableRequiredMarker}), full)
	if forcedAgain.Phase != "agent:deliverable-submit" {
		t.Fatalf("后续新 verification progress 应可重新进入 exact submit: %+v", forcedAgain)
	}
}

func TestMechanicalControlHistoryProjectionDropsHistoricalToolsButKeepsControlNotices(t *testing.T) {
	history := []HistoryEntry{
		{AssistantContent: "调查", ToolCalls: []llm.ToolCall{{ID: "read", Name: "read_file"}},
			ToolResults: []ToolResult{{ToolCallID: "read", Content: "source"}}},
		{SystemNotice: progressDeliverableRequiredMarker + " 请提交"},
		{SystemNotice: "<loop-reminder>stop exploring</loop-reminder>",
			ToolCalls: []llm.ToolCall{{ID: "grep", Name: "grep_search"}}},
	}
	projected := mechanicalControlHistoryProjection(history)
	if len(projected) != 2 || !strings.Contains(projected[0].SystemNotice, progressDeliverableRequiredMarker) ||
		len(projected[0].ToolCalls) != 0 || len(projected[1].ToolCalls) != 0 ||
		projected[0].AssistantContent != "" || projected[1].AssistantContent != "" {
		t.Fatalf("强制交付投影不得携带历史工具偏好: %+v", projected)
	}
}

func TestObservationCheckpointEvidenceEnumMatchesCurrentAttemptAuthority(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("record_observation_delta", "obs", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"facts": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "properties": map[string]any{
					"evidence_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			}},
			"resolved_candidates": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "properties": map[string]any{
					"candidate_ref": map[string]any{"type": "string"},
					"evidence_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			}},
		},
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	task := replayGateTask("task-observation-attempt", nil)
	task.AttemptID = task.ID + "/attempt-2"
	task.Artifacts = []string{"old-attempt.txt"}
	history := []HistoryEntry{
		{TurnID: task.ID + "/attempt-1/turn-6",
			ToolCalls:   []llm.ToolCall{{ID: "old-call", Name: "read_file"}},
			ToolResults: []ToolResult{{ToolCallID: "old-call", Content: "ok"}}},
		{TurnID: task.AttemptID + "/turn-1",
			ToolCalls:   []llm.ToolCall{{ID: "current-before", Name: "run_shell"}},
			ToolResults: []ToolResult{{ToolCallID: "current-before", Content: "ok"}}},
		{TurnID: task.AttemptID + "/turn-2",
			ToolCalls: []llm.ToolCall{{ID: "observation", Name: "record_observation_delta"}},
			ToolResults: []ToolResult{{ToolCallID: "observation",
				Content: `{"open_candidate_refs":["candidate:open"]}`}}},
		{TurnID: task.AttemptID + "/turn-3",
			ToolCalls:   []llm.ToolCall{{ID: "current-after", Name: "grep_search"}},
			ToolResults: []ToolResult{{ToolCallID: "current-after", Content: "ok"}}},
	}
	view := observationCheckpointRegistry(registry, task, history)
	defs := view.Defs()
	if len(defs) != 1 {
		t.Fatalf("Observation registry 定义数=%d，期望 1", len(defs))
	}
	properties := defs[0].Parameters["properties"].(map[string]any)
	facts := properties["facts"].(map[string]any)
	items := facts["items"].(map[string]any)
	factProperties := items["properties"].(map[string]any)
	evidence := factProperties["evidence_refs"].(map[string]any)
	values := evidence["items"].(map[string]any)["enum"].([]any)
	if len(values) != 2 || values[0] != "tool-call:current-after" || values[1] != "tool-call:current-before" {
		t.Fatalf("evidence enum 泄露前一 Attempt/累计 artifact: %v", values)
	}
	resolved := properties["resolved_candidates"].(map[string]any)
	resolvedItems := resolved["items"].(map[string]any)
	resolvedProperties := resolvedItems["properties"].(map[string]any)
	resolvedEvidence := resolvedProperties["evidence_refs"].(map[string]any)
	resolvedValues := resolvedEvidence["items"].(map[string]any)["enum"].([]any)
	if len(resolvedValues) != 1 || resolvedValues[0] != "tool-call:current-after" {
		t.Fatalf("resolved evidence enum 含 predecessor 之前的证据: %v", resolvedValues)
	}
	candidateValues := resolvedProperties["candidate_ref"].(map[string]any)["enum"].([]any)
	if len(candidateValues) != 1 || candidateValues[0] != "candidate:open" {
		t.Fatalf("predecessor open candidate 未冻结: %v", candidateValues)
	}
	catalog := observationCheckpointCatalogPrompt(defs)
	for _, required := range []string{
		`facts.evidence_refs literals: ["tool-call:current-after","tool-call:current-before"]`,
		`resolved_candidates.candidate_ref literals: ["candidate:open"]`,
		`post-predecessor literals: ["tool-call:current-after"]`,
	} {
		if !strings.Contains(catalog, required) {
			t.Fatalf("Observation system catalog 缺少 %q: %s", required, catalog)
		}
	}
	if strings.Contains(catalog, "old-call") || strings.Contains(catalog, "old-attempt.txt") {
		t.Fatalf("Observation system catalog 泄露无效 authority: %s", catalog)
	}
}

func TestObservationCheckpointFailureDetailOnlyReturnsBoundedControlError(t *testing.T) {
	result := ExecuteResult{
		ToolCalls: []llm.ToolCall{
			{ID: "business", Name: "read_file"},
			{ID: "observation", Name: "record_observation_delta"},
		},
		ToolResults: []ToolResult{
			{ToolCallID: "business", Content: "错误: 不应回显业务错误"},
			{ToolCallID: "observation", Content: "错误: facts[0] 缺少合法 evidence_refs"},
		},
	}
	if got := observationCheckpointFailureDetail(result); got != "错误: facts[0] 缺少合法 evidence_refs" {
		t.Fatalf("Observation retry 机械错误回执=%q", got)
	}
	result.ToolCalls = result.ToolCalls[:1]
	if got := observationCheckpointFailureDetail(result); got != "" {
		t.Fatalf("不得回显业务工具错误: %q", got)
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
		"read_graph", "read_graph_change", "submit_recovery_decision", "validate_graph_change",
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
		"read_graph", "read_graph_change", "submit_recovery_decision", "validate_graph_change",
	}
	if policy.Phase != "scheduler:graph-recovery" || !sameExactToolSet(policy.Registry.Names(), want) ||
		!phaseRequiresToolCall(policy.Phase) || !phaseDispatchesOnlyFirstTool(policy.Phase) {
		t.Fatalf("Graph recovery phase/ToolRouter 不符: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
}

func TestGraphChangeNoChangeDecisionRequiresSuccessfulGraphRead(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{
		"read_graph", "get_task_result", "read_content_ref", "propose_graph_change",
		"read_graph_change", "validate_graph_change", "commit_graph_change",
		"submit_graph_change_decision",
	} {
		full.Register(name, name, map[string]any{"type": "object"},
			func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("graph-change-no-change-policy", nil)
	task.EventType = "__scheduler__"
	task.EventSource = model.TaskEventSourceGraphChange
	task.InterventionGraphID = "g-1"
	task.RunPhase = runcontract.PhaseRecovery

	initial := deriveInvocationToolPolicy(task, nil, full)
	if initial.Phase != "scheduler:recovery" || containsToolName(initial.Registry.Names(), "submit_graph_change_decision") {
		t.Fatalf("读取 Graph 前不得开放 no_change 收口: phase=%s tools=%v", initial.Phase, initial.Registry.Names())
	}
	read := []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "read-1", Name: "read_graph", Arguments: map[string]any{"graph_id": "g-1"}}},
		ToolResults: []ToolResult{{ToolCallID: "read-1", Content: `{"graph_id":"g-1","status":"failed"}`}},
	}}
	afterRead := deriveInvocationToolPolicy(task, read, full)
	if !containsToolName(afterRead.Registry.Names(), "submit_graph_change_decision") {
		t.Fatalf("成功 read_graph 后应开放结构化 no_change 收口: %v", afterRead.Registry.Names())
	}
	failedRead := []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "read-2", Name: "read_graph"}},
		ToolResults: []ToolResult{{ToolCallID: "read-2", Content: "错误: scope mismatch"}},
	}}
	if policy := deriveInvocationToolPolicy(task, failedRead, full); containsToolName(policy.Registry.Names(), "submit_graph_change_decision") {
		t.Fatalf("失败 read_graph 不得开放 no_change 收口: %v", policy.Registry.Names())
	}
}

func TestObservationCheckpointAndFinalReportUseClosedToolPhases(t *testing.T) {
	full := NewToolRegistry()
	for _, name := range []string{"record_observation_delta", "read_file", "submit_task_result", "read_graph", "get_task_result", "read_content_ref", "report_done"} {
		full.Register(name, name, map[string]any{"type": "object"},
			func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	worker := replayGateTask("worker-observation", nil)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	v4, ok := catalog.ProgressContract(policycatalog.ProgressCodeChangeV4)
	if !ok {
		t.Fatal("缺少 code-change/v4")
	}
	worker.ProgressContract = &v4.Contract
	observation := deriveInvocationToolPolicy(worker, []HistoryEntry{{SystemNotice: observationCheckpointNotice("rollover", "冻结观察")}}, full)
	if observation.Phase != "agent:observation-checkpoint" ||
		observation.MaxCalls != defaultToolCallsPerResponse ||
		!sameExactToolSet(observation.Registry.Names(), []string{"record_observation_delta"}) {
		t.Fatalf("Observation phase 未收窄: phase=%s tools=%v", observation.Phase, observation.Registry.Names())
	}
	router, err := FreezeToolRouterSnapshotWithPolicy(observation.Registry, observation.Phase, observation.MaxCalls)
	if err != nil {
		t.Fatal(err)
	}
	if choice := invocationToolChoice(router); choice.Mode != invocation.ToolChoiceFunction || choice.Name != "record_observation_delta" {
		t.Fatalf("Observation checkpoint Control lane 必须 exact typed action: %+v", choice)
	}
	if reasoningEffort, override := phaseReasoningEffortOverride(router.Phase); !override || reasoningEffort != "none" {
		t.Fatalf("Observation Control lane 必须使用 reasoning=none: effort=%q override=%v", reasoningEffort, override)
	}
	if reasoningEffort, override := phaseReasoningEffortOverride("agent:deliverable-submit"); !override || reasoningEffort != "none" {
		t.Fatalf("终态交付应保留 reasoning=none 狭义例外: effort=%q override=%v", reasoningEffort, override)
	}

	verification, ok := catalog.ProgressContract(policycatalog.ProgressVerificationV2)
	if !ok {
		t.Fatal("缺少 verification/v2")
	}
	acceptance := replayGateTask("acceptance-observation", nil)
	acceptance.GraphID, acceptance.NodeID, acceptance.ActivationID = "g-1", "acceptance", "acceptance@1"
	acceptance.GraphNodeKind = string(graph.KindAcceptance)
	acceptance.ProgressContract = &verification.Contract
	business := full.Filtered([]string{"read_file", "submit_task_result"})
	executor := NewSwappableLLMExecutor(nil, full, nil, nil, nil, "")
	executor.SwapToolRegistry(business)
	businessView, frameworkAuthority := executor.invocationToolRegistries()
	normal := deriveInvocationToolPolicyWithControl(acceptance, nil, businessView, frameworkAuthority)
	if normal.Phase != "default" || !sameExactToolSet(normal.Registry.Names(), []string{"read_file", "submit_task_result"}) {
		t.Fatalf("Acceptance 普通业务轮不得泄露 Observation control: phase=%s tools=%v", normal.Phase, normal.Registry.Names())
	}
	checkpoint := deriveInvocationToolPolicyWithControl(acceptance,
		[]HistoryEntry{{SystemNotice: observationCheckpointNotice("periodic", "冻结观察")}}, businessView, frameworkAuthority)
	if checkpoint.Phase != "agent:observation-checkpoint" ||
		!sameExactToolSet(checkpoint.Registry.Names(), []string{"record_observation_delta"}) {
		t.Fatalf("Acceptance checkpoint 必须从 framework authority 取得 exact control tool: phase=%s tools=%v",
			checkpoint.Phase, checkpoint.Registry.Names())
	}

	failures := []HistoryEntry{
		{SystemNotice: observationCheckpointNotice("periodic", "冻结观察")},
		{SystemNotice: observationCheckpointFailureMarker + " provider response gate 失败"},
		{SystemNotice: observationCheckpointFailureMarker + " 参数修正失败"},
	}
	if got := observationCheckpointFailureCount(failures); got != 2 {
		t.Fatalf("Observation Invocation 失败未进入有界计数: got=%d", got)
	}
	for _, content := range []string{
		"错误: invalid evidence", "错误：invalid evidence", "已跳过: duplicate", "已跳过：duplicate",
	} {
		if !unsuccessfulToolResult(content) {
			t.Fatalf("Observation 失败回执前缀未兼容: %q", content)
		}
	}

	report := replayGateTask("final-report", nil)
	report.EventType, report.EventSource = "__scheduler__", "graph-ended"
	report.RunPhase, report.FinalReportGraphID = runcontract.PhaseFinalization, "g-1"
	finalReportNormal := deriveInvocationToolPolicy(report, nil, full)
	wantNormal := []string{"get_task_result", "read_content_ref", "read_graph", "report_done"}
	if finalReportNormal.Phase != "scheduler:final-report" || !sameExactToolSet(finalReportNormal.Registry.Names(), wantNormal) ||
		!phaseRequiresToolCall(finalReportNormal.Phase) {
		t.Fatalf("final-report 普通工具面错误: phase=%s tools=%v", finalReportNormal.Phase, finalReportNormal.Registry.Names())
	}
	withForeignMarker := deriveInvocationToolPolicy(report,
		[]HistoryEntry{{SystemNotice: observationCheckpointNotice("continue", "不应作用于 final-report")}}, full)
	if withForeignMarker.Phase != "scheduler:final-report" ||
		!sameExactToolSet(withForeignMarker.Registry.Names(), wantNormal) {
		t.Fatalf("final-report 结构化 scope 必须压过无关 Observation marker: phase=%s tools=%v",
			withForeignMarker.Phase, withForeignMarker.Registry.Names())
	}
	forced := deriveInvocationToolPolicy(report, []HistoryEntry{{SystemNotice: progressDeliverableRequiredMarker}}, full)
	if forced.Phase != "scheduler:final-report-submit" ||
		!sameExactToolSet(forced.Registry.Names(), []string{"report_done"}) {
		t.Fatalf("final-report 强制交付未收窄: phase=%s tools=%v", forced.Phase, forced.Registry.Names())
	}
	forcedAfterReads := deriveInvocationToolPolicy(report, []HistoryEntry{
		{ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read_graph"}}},
		{ToolCalls: []llm.ToolCall{{ID: "read-2", Name: "get_task_result"}},
			ToolResults: []ToolResult{{ToolCallID: "read-2", Content: "[工具错误] 缺少 agent_id"}}},
	}, full)
	if forcedAfterReads.Phase != "scheduler:final-report-submit" ||
		!sameExactToolSet(forcedAfterReads.Registry.Names(), []string{"report_done"}) {
		t.Fatalf("final-report 两个补读 turn（含失败）后必须 exact report_done: phase=%s tools=%v",
			forcedAfterReads.Phase, forcedAfterReads.Registry.Names())
	}
}

func TestOrdinaryBusinessPhaseRemovesFrameworkObservationToolFromUnion(t *testing.T) {
	registry := NewToolRegistry()
	for _, name := range []string{"read_file", "write_file", "record_observation_delta", "submit_task_result"} {
		registry.Register(name, name, map[string]any{"type": "object"},
			func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	task := replayGateTask("worker-business", nil)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	progress, _ := catalog.ProgressContract(policycatalog.ProgressCodeChangeV6)
	task.ProgressContract = &progress.Contract
	ordinary := deriveInvocationToolPolicyWithControl(task, nil, registry, registry)
	if containsToolName(ordinary.Registry.Names(), "record_observation_delta") ||
		!containsToolName(ordinary.Registry.Names(), "read_file") ||
		!containsToolName(ordinary.Registry.Names(), "submit_task_result") {
		t.Fatalf("普通业务 ToolRouter 未剔除 framework Observation control: %v", ordinary.Registry.Names())
	}
	checkpoint := deriveInvocationToolPolicyWithControl(task,
		[]HistoryEntry{{SystemNotice: observationCheckpointNotice("periodic", "checkpoint")}}, registry, registry)
	if !sameExactToolSet(checkpoint.Registry.Names(), []string{"record_observation_delta"}) {
		t.Fatalf("checkpoint control lane 被 normal 过滤误伤: %v", checkpoint.Registry.Names())
	}
}

func TestRunCheckSchemaFreezesCurrentTaskRequiredCheckIDs(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("run_check", "检查", map[string]any{
		"type": "object", "properties": map[string]any{
			"check_id": map[string]any{"type": "string"},
		},
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	task := replayGateTask("typed-check", nil)
	task.FulfillmentContract = &fulfillment.Contract{
		RequiredCheckIDs: []string{" verification ", "build", "verification"},
	}
	task.RunContract.CheckContracts = []runcontract.CheckContract{
		{CheckID: "targeted", Kind: "test"},
		{CheckID: "verification", Kind: "test", ExactCommand: "go test ./..."},
	}
	policy := deriveInvocationToolPolicyWithControl(task, nil, registry, registry)
	defs := policy.Registry.Defs()
	if len(defs) != 1 {
		t.Fatalf("run_check 工具面异常: %+v", defs)
	}
	properties, _ := defs[0].Parameters["properties"].(map[string]any)
	checkID, _ := properties["check_id"].(map[string]any)
	if !reflect.DeepEqual(checkID["enum"], []string{"build", "targeted", "verification"}) ||
		!strings.Contains(fmt.Sprint(checkID["description"]), `verification exact_command="go test ./..."`) {
		t.Fatalf("check_id enum 必须与当前 GraphContract 同源: %#v", checkID)
	}
}

func TestBusinessHistoryProjectionDropsObservationControlLane(t *testing.T) {
	history := []HistoryEntry{
		{TurnID: "business-1", AssistantContent: "调查", ToolCalls: []llm.ToolCall{{ID: "r", Name: "read_file"}}},
		{TurnID: "control-1", ToolCalled: true,
			ToolCalls:   []llm.ToolCall{{ID: "o", Name: "record_observation_delta"}},
			ToolResults: []ToolResult{{ToolCallID: "o", Content: `{"observation_delta_ref":"observation:sha256:x"}`}}},
		{TurnID: "business-2", AssistantContent: "继续实现"},
	}
	projected := businessHistoryProjection(history)
	if len(projected) != 3 || projected[0].TurnID != "business-1" ||
		projected[1].ContextProjection != observationProjectionPrefix+"observation:sha256:x" ||
		projected[2].TurnID != "business-2" {
		t.Fatalf("业务 replay 必须用不可见锚点替代 Observation Control lane: %+v", projected)
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

func TestRecoverySingleActionAllowsAuthorizedFirstCallWithUnauthorizedTail(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("edit_file", "编辑", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) { return "ok", nil })
	router, err := FreezeToolRouterSnapshotWithPolicy(registry, "agent:recovery-mutation",
		defaultToolCallsPerResponse)
	if err != nil {
		t.Fatal(err)
	}
	calls := []llm.ToolCall{
		{ID: "edit", Name: "edit_file"},
		{ID: "read", Name: "read_content_ref"},
		{ID: "grep", Name: "grep_search"},
	}
	if err := validateToolCallBatch(router, calls); err != nil {
		t.Fatalf("Recovery 单动作阶段应只校验首个待 dispatch 调用，尾部合法 fan-out 由 skipped receipt 收口: %v", err)
	}
	if err := validateToolCallBatch(router, []llm.ToolCall{{ID: "read", Name: "read_file"}}); err == nil ||
		!isActionContractViolation(err) {
		t.Fatalf("首个调用仍必须受 Recovery allowlist 约束: %v", err)
	}
}

func TestSchedulerAutoSingletonRejectsTextOnlyPhaseResponse(t *testing.T) {
	router := ToolRouterSnapshot{Phase: "scheduler:draft-create", MaxCalls: defaultToolCallsPerResponse}
	if err := validateToolCallBatch(router, nil); err == nil || !strings.Contains(err.Error(), "未返回必需") ||
		!isActionContractViolation(err) {
		t.Fatalf("auto wire 不得让正文越过机械阶段: %v", err)
	}
}

func TestToolBatchSeparatesActionContractFromMalformedProtocol(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("record_observation_delta", "obs", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) { return "ok", nil })
	router, err := FreezeToolRouterSnapshotWithPolicy(registry, "agent:observation-checkpoint",
		defaultToolCallsPerResponse)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := validateToolCallBatch(router, []llm.ToolCall{{ID: "c1", Name: "read_file"}})
	if unauthorized == nil || !isActionContractViolation(unauthorized) {
		t.Fatalf("合法 response 中的错阶段工具应归 action contract: %v", unauthorized)
	}
	malformed := validateToolCallBatch(router, []llm.ToolCall{{ID: "c2", Name: "bad tool name"}})
	if malformed == nil || isActionContractViolation(malformed) {
		t.Fatalf("非法工具名仍应归 protocol malformed: %v", malformed)
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
