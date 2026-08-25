package agent

import (
	"context"
	"strings"
	"testing"

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
		"validate_graph_change", "commit_graph_change", "report_done",
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

func TestBusinessHistoryProjectionDropsObservationControlLane(t *testing.T) {
	history := []HistoryEntry{
		{TurnID: "business-1", AssistantContent: "调查", ToolCalls: []llm.ToolCall{{ID: "r", Name: "read_file"}}},
		{TurnID: "control-1", ToolCalled: true,
			ToolCalls:   []llm.ToolCall{{ID: "o", Name: "record_observation_delta"}},
			ToolResults: []ToolResult{{ToolCallID: "o", Content: `{"observation_delta_ref":"observation:sha256:x"}`}}},
		{TurnID: "business-2", AssistantContent: "继续实现"},
	}
	projected := businessHistoryProjection(history)
	if len(projected) != 2 || projected[0].TurnID != "business-1" || projected[1].TurnID != "business-2" {
		t.Fatalf("业务 replay 不得混入 Observation Control lane: %+v", projected)
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
