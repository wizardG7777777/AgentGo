package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextadapter"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/prompt"
	"agentgo/internal/runcontract"
)

type contextRuntimeLLM struct {
	messages []llm.Message
	tools    []llm.ToolDef
	binding  invocation.ContextBinding
}

func newAgentTestContextRuntime(t *testing.T) ContextRuntime {
	t.Helper()
	root := t.TempDir()
	snapshots, err := contextstore.New(root + "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Close() })
	contents, err := contentstore.Open(root+"/content", contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contents.Close() })
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	return ContextRuntime{
		Adapter: contextadapter.New(), Policies: catalog, Snapshots: snapshots,
		Content: contents, SessionID: func() string { return "session-test" },
	}
}

func (f *contextRuntimeLLM) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	f.messages = append([]llm.Message(nil), messages...)
	f.tools = append([]llm.ToolDef(nil), tools...)
	f.binding, _ = invocation.ContextBindingFrom(ctx)
	return llm.Response{Content: "完成", FinishReason: llm.FinishReasonStop}, nil
}

func TestLLMExecutorUsesDurableContextCompilerAndParentChain(t *testing.T) {
	root := t.TempDir()
	snapshots, err := contextstore.New(root + "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Close() })
	contents, err := contentstore.Open(root+"/content", contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contents.Close() })
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	client := &contextRuntimeLLM{}
	registry := NewToolRegistry()
	registry.Register("read_file", "读取", map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) {
		return "", nil
	})
	executor := NewSwappableLLMExecutor(client, registry, nil, nil, nil, "", "系统")
	executor.SetContextRuntime(ContextRuntime{
		Adapter: contextadapter.New(), Policies: catalog, Snapshots: snapshots,
		Content: contents, SessionID: func() string { return "session-1" },
	})
	now := time.Now().UTC()
	task := &model.Task{
		ID: "task-context", RunID: "run-context",
		RunContract: &runcontract.RunContract{
			Schema: runcontract.SchemaV1, RunID: "run-context", CreatedAt: now,
			DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
			RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
		},
		ContextPolicyRef: policycatalog.ContextDefaultV1,
		Description:      "检查上下文", AttemptID: "task-context/attempt-1", AttemptNo: 1,
		Lease: &model.ExecutionLease{
			TaskID: "task-context", Attempt: 1, FrozenAt: now,
			BusinessTools: []string{"read_file"}, Digest: "lease-digest",
		},
	}
	ctx := WithExecutionIdentity(context.Background(), "run-context", task.AttemptID, task.AttemptID+"/turn-1")
	build := prompt.Compile([]prompt.Component{
		{ID: prompt.ComponentAgentRole, Version: "test/v1", Text: "冻结角色", InMessage: true},
		{ID: prompt.ComponentControlProtocol, Version: "test/v1", Text: "冻结控制", InMessage: true},
		{ID: prompt.ComponentTaskObjective, Version: "test/v1", Text: "冻结目标", InMessage: true},
		{ID: prompt.ComponentOutputContract, Version: "test/v1", Text: "冻结输出契约", InMessage: false},
		{ID: prompt.ComponentToolGuidance, Version: "test/v1", Text: "read_file", InMessage: false},
	})
	ctx = withPromptBuild(ctx, build)
	history := []HistoryEntry{
		{TurnID: task.AttemptID + "/history-1", Output: strings.Repeat("x", 80<<10)},
		{SystemNotice: "<loop-reminder>改用交付动作</loop-reminder>"},
	}
	first, err := executor.Execute(ctx, task, nil, history)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContextSnapshotID == "" || len(client.messages) < 6 || client.messages[len(client.messages)-1].Role != "system" {
		t.Fatalf("ContextCompiler/顺序未生效: result=%+v messages=%+v", first, client.messages)
	}
	joined := ""
	for _, message := range client.messages {
		joined += message.Content
	}
	if !strings.Contains(joined, "冻结角色") || !strings.Contains(joined, "冻结输出契约") ||
		strings.Contains(joined, task.Description) {
		t.Fatalf("L2 未以冻结 PromptBuild 为唯一 L1 来源: %q", joined)
	}
	stored, ok, err := snapshots.Get(first.ContextSnapshotID)
	if err != nil || !ok || stored.Snapshot.ToolRouterSnapshotID == "" {
		t.Fatalf("Snapshot 未 durable: ok=%v err=%v record=%+v", ok, err, stored)
	}
	if client.binding.ContextSnapshotID != stored.Snapshot.SnapshotID ||
		client.binding.InvocationID != stored.Snapshot.InvocationID ||
		client.binding.EncodedRequestDigest != stored.Snapshot.EncodedRequestDigest {
		t.Fatalf("provider request 未绑定已持久化 Snapshot: binding=%+v snapshot=%+v", client.binding, stored.Snapshot)
	}

	ctx2 := WithExecutionIdentity(context.Background(), "run-context", task.AttemptID, task.AttemptID+"/turn-2")
	ctx2 = withPromptBuild(ctx2, build)
	second, err := executor.Execute(ctx2, task, nil, history)
	if err != nil {
		t.Fatal(err)
	}
	stored2, ok, err := snapshots.Get(second.ContextSnapshotID)
	if err != nil || !ok || stored2.Snapshot.ParentSnapshotRef != first.ContextSnapshotID {
		t.Fatalf("Snapshot parent chain 错误: ok=%v err=%v parent=%q want=%q",
			ok, err, stored2.Snapshot.ParentSnapshotRef, first.ContextSnapshotID)
	}
}

func TestLLMExecutorRejectsPartiallyConfiguredContextRuntime(t *testing.T) {
	client := &contextRuntimeLLM{}
	executor := NewSwappableLLMExecutor(client, NewToolRegistry(), nil, nil, nil, "")
	executor.SetContextRuntime(ContextRuntime{Adapter: contextadapter.New()})
	_, err := executor.Execute(context.Background(), &model.Task{ID: "legacy-task", Description: "test"}, nil, nil)
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureContextAssembly {
		t.Fatalf("部分 L2 装配必须 fail-closed: err=%v failure=%+v", err, failure)
	}
	if len(client.messages) != 0 {
		t.Fatal("部分 L2 装配不得调用 provider")
	}
}

func TestValidateStaticPromptUsesVersionedContextBudget(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	runtime := ContextRuntime{Adapter: contextadapter.New(), Policies: catalog}
	// 事故形状：中文 Scheduler Prompt 的 serialized bytes 大于 v1 的 48 KiB，
	// 但 rune-based token estimate 与 bytes 都落在 v2 内。
	promptText := strings.Repeat("策", 18<<10)
	v1 := StaticPromptProfile{
		ProfileID: "scheduler-v1-regression", ContextPolicyRef: policycatalog.ContextDefaultV1,
		SystemPrompt: promptText,
	}
	err = runtime.ValidateStaticPrompt(context.Background(), v1)
	var assemblyFailure *contextcontract.ContextAssemblyFailure
	if !errors.As(err, &assemblyFailure) || assemblyFailure.Reason != contextcontract.AssemblyFragmentLimitExceeded {
		t.Fatalf("v1 应按稳定 hard cap 拒绝事故形状: err=%v failure=%+v", err, assemblyFailure)
	}
	v2 := v1
	v2.ProfileID = "scheduler-v2-historical"
	v2.ContextPolicyRef = policycatalog.ContextDefaultV2
	if err := runtime.ValidateStaticPrompt(context.Background(), v2); err != nil {
		t.Fatalf("历史 v2 应保持可容纳事故形状: %v", err)
	}
	v2.SystemPrompt = strings.Repeat("策", 22<<10)
	if err := runtime.ValidateStaticPrompt(context.Background(), v2); err == nil {
		t.Fatal("v2 仍必须拒绝超过 64 KiB 的单项，不得把预检变成无界放行")
	}
	v3 := v1
	v3.ProfileID = "scheduler-v4-current"
	v3.ContextPolicyRef = policycatalog.ContextDefaultCurrent
	v3.SystemPrompt = strings.Repeat("策", 15<<10)
	if err := runtime.ValidateStaticPrompt(context.Background(), v3); err != nil {
		t.Fatalf("当前 v4 应容纳修正混合估算内的合法 Prompt: %v", err)
	}
}

func TestValidateStaticPromptZeroRuntimeKeepsExplicitLegacyTests(t *testing.T) {
	if err := (ContextRuntime{}).ValidateStaticPrompt(context.Background(), StaticPromptProfile{
		ProfileID: "legacy-test", SystemPrompt: "只用于未装配 L2 的隔离测试",
	}); err != nil {
		t.Fatalf("完全零值 runtime 应保留显式 legacy 测试兼容: %v", err)
	}
	if err := (ContextRuntime{Adapter: contextadapter.New()}).ValidateStaticPrompt(
		context.Background(), StaticPromptProfile{ProfileID: "partial", SystemPrompt: "x"}); err == nil {
		t.Fatal("部分 L2 装配必须 fail-closed")
	}
}

func TestResponseReplayGateDropsOptionalReasoningOnNextTurnWithoutBlockingTools(t *testing.T) {
	runtime := newAgentTestContextRuntime(t)
	raw := json.RawMessage(`"` + strings.Repeat("r", 40<<10) + `"`)
	mock := &mockLLMClient{responses: []llm.Response{
		{
			ToolCalls:   []llm.ToolCall{{ID: "call-optional", Name: "observe", Arguments: map[string]any{}}},
			ExtraFields: map[string]json.RawMessage{"reasoning": raw},
		},
		{Content: "完成", FinishReason: llm.FinishReasonStop},
	}}
	run := false
	registry := NewToolRegistry()
	registry.Register("observe", "观察", map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) {
		run = true
		return "ok", nil
	})
	executor := NewSwappableLLMExecutor(mock, registry, nil, nil, nil, "", "系统")
	executor.SetContextRuntime(runtime)
	task := replayGateTask("task-replay-optional", []string{"observe"})

	ctx := WithExecutionIdentity(context.Background(), string(task.RunID), task.AttemptID, task.AttemptID+"/turn-1")
	first, err := executor.Execute(ctx, task, nil, nil)
	if err != nil || !run || !first.ToolCalled {
		t.Fatalf("Optional reasoning 不应阻断工具执行: run=%v result=%+v err=%v", run, first, err)
	}
	if !json.Valid(first.ExtraFields["reasoning"]) || !strings.Contains(string(first.ExtraFields["reasoning"]), strings.Repeat("r", 128)) {
		t.Fatal("raw provider reasoning 未保留在 settled Turn 候选")
	}

	history := []HistoryEntry{historyEntryFromResult(first, "test-model", task.AttemptID+"/turn-1")}
	ctx = WithExecutionIdentity(context.Background(), string(task.RunID), task.AttemptID, task.AttemptID+"/turn-2")
	second, err := executor.Execute(ctx, task, nil, history)
	if err != nil || second.Output != "完成" {
		t.Fatalf("下一轮应通过 dropped replay 正常执行: result=%+v err=%v", second, err)
	}
}

func TestResponseReplayGateRejectsRequiredExactBeforeToolDispatch(t *testing.T) {
	runtime := newAgentTestContextRuntime(t)
	mock := &mockLLMClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{ID: "call-required", Name: "effect", Arguments: map[string]any{}}},
		ExtraFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"` + strings.Repeat("r", 40<<10) + `"`),
		},
	}}}
	run := false
	registry := NewToolRegistry()
	registry.Register("effect", "副作用", map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) {
		run = true
		return "不应执行", nil
	})
	executor := NewSwappableLLMExecutor(mock, registry, nil, nil, nil, "", "系统")
	executor.SetContextRuntime(runtime)
	task := replayGateTask("task-replay-required", []string{"effect"})
	ctx := WithExecutionIdentity(context.Background(), string(task.RunID), task.AttemptID, task.AttemptID+"/turn-1")
	result, err := executor.Execute(ctx, task, nil, nil)
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureOutputLimitExceeded || failure.UsageState != invocation.UsageSettled {
		t.Fatalf("RequiredExact 超限未形成 settled output limit: result=%+v err=%v failure=%+v", result, err, failure)
	}
	if run {
		t.Fatal("Response replay gate 失败后仍 dispatch 了工具")
	}
}

func TestLLMExecutorMapsTaskContextInputsToTypedUpstreamMessages(t *testing.T) {
	runtime := newAgentTestContextRuntime(t)
	client := &contextRuntimeLLM{}
	executor := NewSwappableLLMExecutor(client, NewToolRegistry(), nil, nil, nil, "", "系统")
	executor.SetContextRuntime(runtime)
	task := replayGateTask("task-typed-input", nil)
	task.Description = "只包含局部目标"
	task.ContextInputs = []model.TaskContextInput{
		{Kind: model.TaskContextUpstreamResult, SourceRef: "graph:g/activation:a@1/result:default", Content: `<upstream-result>{"coverage":91}</upstream-result>`},
		{Kind: model.TaskContextUpstreamEvidence, SourceRef: "graph:g/activation:a@1/evidence:default", Content: `<upstream-evidence>{"ref":"ev:1"}</upstream-evidence>`},
	}
	ctx := WithExecutionIdentity(context.Background(), string(task.RunID), task.AttemptID, task.AttemptID+"/turn-1")
	if _, err := executor.Execute(ctx, task, nil, nil); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, message := range client.messages {
		joined += message.Content
	}
	if !strings.Contains(joined, `"coverage":91`) || !strings.Contains(joined, `"ref":"ev:1"`) {
		t.Fatalf("typed upstream inputs 未进入 L2 wire: %q", joined)
	}
	if strings.Contains(task.Description, "coverage") || strings.Contains(task.Description, "ev:1") {
		t.Fatalf("typed upstream inputs 污染了 Task Description: %q", task.Description)
	}
}

func TestLLMExecutorPersistsLargeToolResultBeforeHistoryProjection(t *testing.T) {
	runtime := newAgentTestContextRuntime(t)
	mock := &mockLLMClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{ID: "call-large-result", Name: "large_output", Arguments: map[string]any{}}},
	}}}
	raw := strings.Repeat("开始-", 5000) + strings.Repeat("界", 20<<10) + strings.Repeat("-结束", 5000)
	registry := NewToolRegistry()
	registry.Register("large_output", "大结果", map[string]any{"type": "object"}, func(context.Context, map[string]any) (string, error) {
		return raw, nil
	})
	executor := NewSwappableLLMExecutor(mock, registry, nil, nil, nil, "", "系统")
	executor.SetContextRuntime(runtime)
	task := replayGateTask("task-large-tool-result", []string{"large_output"})
	ctx := WithExecutionIdentity(context.Background(), string(task.RunID), task.AttemptID, task.AttemptID+"/turn-1")
	result, err := executor.Execute(ctx, task, nil, nil)
	if err != nil || len(result.ToolResults) != 1 {
		t.Fatalf("执行大 ToolResult: result=%+v err=%v", result, err)
	}
	var envelope toolResultReferenceEnvelope
	if err := json.Unmarshal([]byte(result.ToolResults[0].Content), &envelope); err != nil {
		t.Fatalf("ToolResult 未替换为 reference envelope: %v content=%q", err, result.ToolResults[0].Content[:128])
	}
	if envelope.RefID == "" || envelope.OriginalBytes != len([]byte(raw)) ||
		envelope.SHA256 != contextcontract.DigestBytes([]byte(raw)) || !strings.Contains(envelope.Instruction, "read_content_ref") {
		t.Fatalf("ToolResult reference metadata 错误: %+v", envelope)
	}
	status, err := runtime.Content.Inspect(envelope.RefID)
	if err != nil || status.Ref.SizeBytes != int64(len([]byte(raw))) {
		t.Fatalf("完整 ToolResult 未进入 ContentStore: status=%+v err=%v", status, err)
	}
	if strings.Contains(result.ToolResults[0].Content, raw) || !strings.Contains(result.ToolResults[0].Content, "preview_head") {
		t.Fatal("History 候选仍内联完整 ToolResult 或缺少有界预览")
	}
}

func replayGateTask(id string, tools []string) *model.Task {
	now := time.Now().UTC()
	return &model.Task{
		ID: id, RunID: runcontract.RunID("run-" + id), Description: "验证 replay gate",
		AttemptID: id + "/attempt-1", AttemptNo: 1,
		ContextPolicyRef: policycatalog.ContextDefaultV3,
		RunContract: &runcontract.RunContract{
			Schema: runcontract.SchemaV1, RunID: runcontract.RunID("run-" + id), CreatedAt: now,
			DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
			RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
		},
		Lease: &model.ExecutionLease{
			TaskID: id, Attempt: 1, FrozenAt: now, BusinessTools: tools, Digest: "lease-" + id,
		},
	}
}
