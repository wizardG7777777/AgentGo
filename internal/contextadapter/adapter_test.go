package contextadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
)

func adapterTestInput(t *testing.T) CompileInput {
	t.Helper()
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, ok := catalog.ContextPolicy(policycatalog.ContextDefaultV1)
	if !ok {
		t.Fatal("缺少默认 Context policy")
	}
	replayProfile, ok := catalog.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	if !ok {
		t.Fatal("缺少默认 Replay policy")
	}
	return CompileInput{
		AttemptID: "attempt-1", InvocationID: "invocation-1",
		PromptBuildRef: "prompt-build:1", ExecutionLeaseRef: "lease:1",
		Messages: []MessageBinding{
			{
				Message: llm.Message{Role: "system", Content: "你是代码代理"},
				Kind:    contextcontract.FragmentPromptComponent,
				Section: contextcontract.SectionSystem, SourceRef: "prompt:role",
				Scope: contextcontract.ScopeSystem, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
			},
			{
				Message: llm.Message{Role: "user", Content: "修复问题"},
				Kind:    contextcontract.FragmentUserTask,
				Section: contextcontract.SectionTaskContract, SourceRef: "task:1",
				Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
			},
		},
		ToolRouter: ToolRouterBinding{
			SnapshotID: "trs-test-1",
			Definitions: []llm.ToolDef{{
				Name: "read_file", Description: "读取文件",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
				},
			}},
		},
		BudgetPolicy: contextProfile.Policy,
		ReplayPolicy: replayProfile.Policy, ReplayPolicyRef: replayProfile.Ref,
	}
}

func TestAdapterCompilesMessagesExtrasToolsFromOneWireAuthority(t *testing.T) {
	input := adapterTestInput(t)
	input.History = []SettledTurn{{
		TurnID: "turn-1",
		Assistant: llm.Message{
			Role: "assistant", Content: "已完成检查",
			ExtraFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"先读取再判断"`)},
		},
	}}

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if result.Snapshot == nil || result.Snapshot.ToolRouterSnapshotID != input.ToolRouter.SnapshotID {
		t.Fatalf("ToolRouter snapshot 未绑定: %+v", result.Snapshot)
	}
	wantMessages := []llm.Message{
		input.Messages[0].Message,
		input.Messages[1].Message,
		input.History[0].Assistant,
	}
	assertJSONEqual(t, result.Messages, wantMessages)
	assertJSONEqual(t, result.Tools, input.ToolRouter.Definitions)
	if len(result.Snapshot.Fragments) != 5 || len(result.Snapshot.Manifest.Items) != 5 {
		t.Fatalf("Fragment/Manifest 未一一对应: fragments=%d manifest=%d",
			len(result.Snapshot.Fragments), len(result.Snapshot.Manifest.Items))
	}
	if got := contextcontract.DigestBytes(result.Runtime.EncodedRequest); got != result.Snapshot.EncodedRequestDigest {
		t.Fatalf("encoded request digest=%s snapshot=%s", got, result.Snapshot.EncodedRequestDigest)
	}
	if !hasAtomicGroup(result.Snapshot, contextcontract.AtomicAssistantProviderReplay) ||
		!hasAtomicGroup(result.Snapshot, contextcontract.AtomicToolDefinition) {
		t.Fatalf("provider replay/tool definition 原子组缺失: %+v", result.Snapshot.AtomicGroups)
	}
	providerGroup := atomicGroup(result.Snapshot, contextcontract.AtomicAssistantProviderReplay)
	if providerGroup.ReplayPolicy != contextcontract.ReplayRequiredExact {
		t.Fatalf("provider extra 未绑定 exact replay: %+v", providerGroup)
	}

	durable, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(durable, []byte("先读取再判断")) || bytes.Contains(durable, []byte("修复问题")) {
		t.Fatalf("Snapshot metadata 泄露正文: %s", durable)
	}
}

func TestAdapterConversationPreservesInterleavedControlOrder(t *testing.T) {
	input := adapterTestInput(t)
	call := llm.ToolCall{ID: "call-ordered", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}
	input.Conversation = []ConversationItem{
		{Message: &input.Messages[0]},
		{Message: &input.Messages[1]},
		{Turn: &SettledTurn{
			TurnID: "turn-ordered", Assistant: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
			ToolResults: []llm.Message{{Role: "tool", ToolCallID: call.ID, Content: "package a"}},
		}},
		{Message: &MessageBinding{
			Message: llm.Message{Role: "system", Content: "<loop-reminder>停止重复读取</loop-reminder>"},
			Kind:    contextcontract.FragmentTaskControlContext, Section: contextcontract.SectionRuntimeControl,
			SourceRef: "loop:checkpoint-2", Scope: contextcontract.ScopeTask,
			Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessLive,
		}},
	}
	input.Messages, input.History = nil, nil
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 5 || result.Messages[2].Role != "assistant" ||
		result.Messages[3].Role != "tool" || result.Messages[4].Role != "system" ||
		!strings.Contains(result.Messages[4].Content, "loop-reminder") {
		t.Fatalf("Conversation wire 顺序漂移: %+v", result.Messages)
	}
}

func TestAdapterBuildsSettledAssistantToolExchange(t *testing.T) {
	input := adapterTestInput(t)
	call := llm.ToolCall{ID: "call-1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}
	input.History = []SettledTurn{{
		TurnID:      "turn-tool",
		Assistant:   llm.Message{Role: "assistant", Content: "读取文件", ToolCalls: []llm.ToolCall{call}},
		ToolResults: []llm.Message{{Role: "tool", ToolCallID: "call-1", Content: "package a"}},
	}}

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	wantMessages := []llm.Message{
		input.Messages[0].Message, input.Messages[1].Message,
		input.History[0].Assistant, input.History[0].ToolResults[0],
	}
	assertJSONEqual(t, result.Messages, wantMessages)
	group := atomicGroup(result.Snapshot, contextcontract.AtomicAssistantToolExchange)
	if group == nil || len(group.FragmentIDs) != 3 || group.ReplayPolicy != contextcontract.ReplayOptional {
		t.Fatalf("assistant/tool exchange 原子组错误: %+v", group)
	}
}

func TestAdapterExternalizesOversizedToolResult(t *testing.T) {
	input := adapterTestInput(t)
	store, err := contentstore.Open(filepath.Join(t.TempDir(), "content"), contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	input.ContentRepository = store
	input.ContentScope = contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: "session-1", GraphID: "graph-1", TaskID: "task-1",
	}
	large := strings.Repeat("x", 60<<10)
	input.History = []SettledTurn{{
		TurnID: "turn-large",
		Assistant: llm.Message{
			Role: "assistant", Content: "读取大结果",
			ToolCalls: []llm.ToolCall{{ID: "call-large", Name: "read_file", Arguments: map[string]any{"path": "large.txt"}}},
		},
		ToolResults: []llm.Message{{Role: "tool", ToolCallID: "call-large", Content: large}},
	}}

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile oversized ToolResult: %v", err)
	}
	if len(result.ExternalizedRefs) != 1 {
		t.Fatalf("externalized refs=%d，want=1", len(result.ExternalizedRefs))
	}
	if strings.Contains(result.Messages[len(result.Messages)-1].Content, large[:1024]) {
		t.Fatal("大 ToolResult 仍原样进入 model-visible message")
	}
	if bytes.Contains(result.Runtime.EncodedRequest, []byte(large[:1024])) {
		t.Fatal("大 ToolResult 仍原样进入 encoded request")
	}
	var record *contextcontract.ContextFragmentRecord
	for i := range result.Snapshot.Fragments {
		if result.Snapshot.Fragments[i].Kind == contextcontract.FragmentToolResult {
			record = &result.Snapshot.Fragments[i]
			break
		}
	}
	if record == nil || record.Disposition != contextcontract.DispositionTombstoned || record.ContentRef == "" {
		t.Fatalf("ToolResult 未形成 tombstone/ref: %+v", record)
	}
	group := atomicGroup(result.Snapshot, contextcontract.AtomicAssistantToolExchange)
	if group == nil || group.ReplayPolicy != contextcontract.ReplayRequiredTransformable ||
		group.TransformID != "tool_result_ref/v1" {
		t.Fatalf("外置 exchange 原子组错误: %+v", group)
	}

	ref := result.ExternalizedRefs[0]
	resolved, err := store.Resolve(context.Background(), contentstore.ResolveRequest{
		Ref: ref, LeaseRef: "lease:1", RequesterScope: input.ContentScope, MaxBytes: int64(len(large) + 1),
	}, func(context.Context, contentstore.AuthorizationRequest) error { return nil })
	if err != nil || string(resolved.Content) != large {
		t.Fatalf("ContentRef 无法解回原始 ToolResult: len=%d err=%v", len(resolved.Content), err)
	}
}

func TestAdapterRecognizesL3ToolResultReferenceWithoutReexternalizing(t *testing.T) {
	input := adapterTestInput(t)
	refID := "content:" + strings.Repeat("a", 64)
	digest := contextcontract.DigestBytes([]byte("完整工具结果"))
	envelope, _ := json.Marshal(map[string]any{
		"schema": "agentgo.tool-result-ref/v1", "ref_id": refID,
		"sha256": digest, "original_bytes": 123456, "preview_head": "head",
	})
	call := llm.ToolCall{ID: "call-ref", Name: "run_shell", Arguments: map[string]any{"command": "test"}}
	input.History = []SettledTurn{{
		TurnID: "turn-tool-ref", Assistant: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
		ToolResults: []llm.Message{{Role: "tool", ToolCallID: call.ID, Content: string(envelope)}},
	}}
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ExternalizedRefs) != 0 {
		t.Fatal("已由 L3 持久化的 ToolResult 不得重复 Put ContentStore")
	}
	var record *contextcontract.ContextFragmentRecord
	for i := range result.Snapshot.Fragments {
		if result.Snapshot.Fragments[i].Kind == contextcontract.FragmentToolResult {
			record = &result.Snapshot.Fragments[i]
			break
		}
	}
	if record == nil || record.Disposition != contextcontract.DispositionTombstoned ||
		record.ContentRef != refID || record.InputDigest != digest {
		t.Fatalf("L3 ToolResult ref 未投影成 L2 tombstone: %+v", record)
	}
}

func TestAdapterExternalizesOversizedSettledAssistant(t *testing.T) {
	input := adapterTestInput(t)
	store, err := contentstore.Open(filepath.Join(t.TempDir(), "content"), contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	input.ContentRepository = store
	input.ContentScope = contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: "session-1", GraphID: "graph-1", TaskID: "task-1",
	}
	large := strings.Repeat("assistant-output-", 6<<10)
	input.History = []SettledTurn{{
		TurnID:    "turn-large-assistant",
		Assistant: llm.Message{Role: "assistant", Content: large},
	}}

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile oversized assistant: %v", err)
	}
	if len(result.ExternalizedRefs) != 1 {
		t.Fatalf("externalized refs=%d，want=1", len(result.ExternalizedRefs))
	}
	assistant := result.Messages[len(result.Messages)-1]
	if assistant.Role != "assistant" || strings.Contains(assistant.Content, large[:1024]) {
		t.Fatalf("巨大 assistant 未替换为有界 Ref: role=%s len=%d", assistant.Role, len(assistant.Content))
	}
	if bytes.Contains(result.Runtime.EncodedRequest, []byte(large[:1024])) {
		t.Fatal("巨大 assistant 仍原样进入 encoded request")
	}
	var record *contextcontract.ContextFragmentRecord
	for i := range result.Snapshot.Fragments {
		if result.Snapshot.Fragments[i].Kind == contextcontract.FragmentAssistantContent {
			record = &result.Snapshot.Fragments[i]
			break
		}
	}
	if record == nil || record.Disposition != contextcontract.DispositionReferenced ||
		record.ContentRef == "" || record.TransformRef != "assistant_content_ref/v1" {
		t.Fatalf("assistant content 未形成 referenced fragment: %+v", record)
	}
	ref := result.ExternalizedRefs[0]
	resolved, err := store.Resolve(context.Background(), contentstore.ResolveRequest{
		Ref: ref, LeaseRef: "lease:1", RequesterScope: input.ContentScope, MaxBytes: int64(len(large) + 1),
	}, func(context.Context, contentstore.AuthorizationRequest) error { return nil })
	if err != nil || string(resolved.Content) != large {
		t.Fatalf("assistant ContentRef 无法解回原文: len=%d err=%v", len(resolved.Content), err)
	}
}

func TestAdapterRejectsMalformedToolExchangeBeforeCompiler(t *testing.T) {
	input := adapterTestInput(t)
	input.History = []SettledTurn{{
		TurnID: "turn-bad",
		Assistant: llm.Message{
			Role:      "assistant",
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read_file", Arguments: map[string]any{"path": "a"}}},
		},
	}}
	_, err := New().Compile(context.Background(), input)
	var failure *contextcontract.ContextAssemblyFailure
	if !errors.As(err, &failure) || failure.Reason != contextcontract.AssemblyInvalidContract {
		t.Fatalf("坏 tool exchange 未被 typed fail-closed: %v", err)
	}
}

func TestAdapterRejectsUnknownProviderExtra(t *testing.T) {
	input := adapterTestInput(t)
	input.History = []SettledTurn{{
		TurnID: "turn-extra",
		Assistant: llm.Message{
			Role: "assistant", Content: "answer",
			ExtraFields: map[string]json.RawMessage{"vendor_unknown": json.RawMessage(`"opaque"`)},
		},
	}}
	_, err := New().Compile(context.Background(), input)
	var failure *contextcontract.ContextAssemblyFailure
	if !errors.As(err, &failure) || failure.Reason != contextcontract.AssemblyProviderReplayUnknown {
		t.Fatalf("未知 provider extra 未 fail-closed: %v", err)
	}
}

func TestAdapterV3DropsOversizedOptionalReasoningButKeepsRawHistoryUntouched(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV3)
	replayProfile, _ := catalog.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	input.BudgetPolicy = contextProfile.Policy
	input.ReplayPolicy, input.ReplayPolicyRef = replayProfile.Policy, replayProfile.Ref
	raw := json.RawMessage(`"` + strings.Repeat("r", 40<<10) + `"`)
	input.History = []SettledTurn{{
		TurnID: "turn-optional-reasoning",
		Assistant: llm.Message{Role: "assistant", Content: "继续工作",
			ExtraFields: map[string]json.RawMessage{"reasoning": raw}},
	}}

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Optional reasoning 超限不应阻断下一轮: %v", err)
	}
	if _, stillVisible := result.Messages[len(result.Messages)-1].ExtraFields["reasoning"]; stillVisible {
		t.Fatal("超限 Optional reasoning 仍进入下一轮 model-visible wire")
	}
	if !bytes.Equal(input.History[0].Assistant.ExtraFields["reasoning"], raw) {
		t.Fatal("Context projection 修改了 raw History")
	}
	var dropped *contextcontract.ContextFragmentRecord
	for i := range result.Snapshot.Fragments {
		record := &result.Snapshot.Fragments[i]
		if record.SourceRef == "turn:turn-optional-reasoning/provider-extra:reasoning" {
			dropped = record
			break
		}
	}
	if dropped == nil || dropped.Kind != contextcontract.FragmentAssistantReasoning ||
		dropped.Disposition != contextcontract.DispositionDropped || dropped.WireID != "" {
		t.Fatalf("Optional reasoning 未形成 dropped 审计记录: %+v", dropped)
	}
}

func TestAdapterV3DropsOversizedInformationalRuntimeSnapshot(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV3)
	replayProfile, _ := catalog.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	input.BudgetPolicy = contextProfile.Policy
	input.ReplayPolicy, input.ReplayPolicyRef = replayProfile.Policy, replayProfile.Ref
	raw := `{"scheduler_board":"` + strings.Repeat("状态", 10<<10) + `"}`
	input.Messages = append(input.Messages, MessageBinding{
		Message: llm.Message{Role: "user", Content: raw},
		Kind:    contextcontract.FragmentRuntimeSnapshot, Section: contextcontract.SectionRuntimeControl,
		SourceRef: "scheduler-board:turn-1", Scope: contextcontract.ScopeTask,
		Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot,
	})

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("超限 informational runtime snapshot 不应阻断 Invocation: %v", err)
	}
	if len(result.Messages) != len(input.Messages)-1 {
		t.Fatalf("dropped runtime snapshot 仍进入 wire: messages=%d want=%d", len(result.Messages), len(input.Messages)-1)
	}
	var dropped *contextcontract.ContextFragmentRecord
	for i := range result.Snapshot.Fragments {
		if result.Snapshot.Fragments[i].SourceRef == "scheduler-board:turn-1" {
			dropped = &result.Snapshot.Fragments[i]
			break
		}
	}
	if dropped == nil || dropped.Kind != contextcontract.FragmentRuntimeSnapshot ||
		dropped.Disposition != contextcontract.DispositionDropped || dropped.WireID != "" ||
		dropped.SerializedBytes <= contextProfile.Policy.FragmentRules[contextcontract.FragmentRuntimeSnapshot].MaxSerializedBytes {
		t.Fatalf("超限 runtime snapshot 未形成有界 dropped 审计记录: %+v", dropped)
	}
	encoded, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("scheduler_board")) || bytes.Contains(encoded, []byte("状态状态")) {
		t.Fatal("dropped runtime snapshot 泄露原始正文")
	}
}

func TestAdapterV3RejectsOversizedRequiredExactReasoningBeforeReplay(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV3)
	replayProfile, _ := catalog.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	input.BudgetPolicy = contextProfile.Policy
	input.ReplayPolicy, input.ReplayPolicyRef = replayProfile.Policy, replayProfile.Ref
	input.History = []SettledTurn{{
		TurnID: "turn-required-reasoning",
		Assistant: llm.Message{Role: "assistant", Content: "继续工作",
			ExtraFields: map[string]json.RawMessage{
				"reasoning_content": json.RawMessage(`"` + strings.Repeat("r", 40<<10) + `"`),
			}},
	}}

	_, err = New().Compile(context.Background(), input)
	var failure *contextcontract.ContextAssemblyFailure
	if !errors.As(err, &failure) || failure.Reason != contextcontract.AssemblyFragmentLimitExceeded {
		t.Fatalf("RequiredExact reasoning 超限未在 replay gate fail-closed: %v", err)
	}
	for _, want := range []string{"provider field=reasoning_content", "requirement=required_exact", "actual=", "limit="} {
		if !strings.Contains(failure.Detail, want) {
			t.Fatalf("RequiredExact replay 诊断缺少 %q: %+v", want, failure)
		}
	}
}

func TestAdapterV4AcceptsReplayableASCIIReasoningRejectedByFrozenV3Estimator(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	replayRaw := json.RawMessage(`"` + strings.Repeat("r", 20<<10) + `"`)
	input.History = []SettledTurn{{
		TurnID: "turn-v4-estimator-regression",
		Assistant: llm.Message{Role: "assistant", Content: "继续工作", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": replayRaw,
		}},
	}}

	v3, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV3)
	replay, _ := catalog.ProviderReplayPolicy(v3.ReplayPolicyRef)
	input.BudgetPolicy, input.ReplayPolicy, input.ReplayPolicyRef = v3.Policy, replay.Policy, replay.Ref
	if _, err := New().Compile(context.Background(), input); err == nil {
		t.Fatal("冻结 v3 all-runes estimator 应复现 ASCII reasoning 假超限")
	}

	v4, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV4)
	input.BudgetPolicy = v4.Policy
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("v4 mixed estimator 不应拒绝可表示的 ASCII reasoning: %v", err)
	}
	if !bytes.Equal(result.Messages[len(result.Messages)-1].ExtraFields["reasoning_content"], replayRaw) {
		t.Fatal("v4 接受 RequiredExact 后未逐字节保留 reasoning_content")
	}
}

func TestAdapterV5AcceptsRequiredExactReasoningWithinCompletionReserve(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	replayRaw := json.RawMessage(`"` + strings.Repeat("r", 40<<10) + `"`)
	input.History = []SettledTurn{{
		TurnID: "turn-v5-required-reasoning",
		Assistant: llm.Message{Role: "assistant", Content: "继续工作", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": replayRaw,
		}},
	}}
	v4, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV4)
	replay, _ := catalog.ProviderReplayPolicy(v4.ReplayPolicyRef)
	input.BudgetPolicy, input.ReplayPolicy, input.ReplayPolicyRef = v4.Policy, replay.Policy, replay.Ref
	if _, err := New().Compile(context.Background(), input); err == nil {
		t.Fatal("冻结 v4 32KiB RequiredExact cap 应拒绝事故形状")
	}

	v5, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV5)
	input.BudgetPolicy = v5.Policy
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("v5 不应拒绝 completion reserve 内的 RequiredExact reasoning: %v", err)
	}
	if !bytes.Equal(result.Messages[len(result.Messages)-1].ExtraFields["reasoning_content"], replayRaw) {
		t.Fatal("v5 未逐字节保留 RequiredExact reasoning_content")
	}
	if result.OutputBudget.MaxExtraFieldBytesByName["reasoning_content"] != 63<<10 ||
		result.OutputBudget.MaxResponseBytes != 128<<10 {
		t.Fatalf("v5 Invocation/Replay 预算没有同源闭合: %+v", result.OutputBudget)
	}
}

func TestAdapterV6FreezesThirtyTwoKCompletionAndRequiredReasoningBudget(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	replayRaw := json.RawMessage(`"` + strings.Repeat("r", 90<<10) + `"`)
	input.History = []SettledTurn{{
		TurnID: "turn-v6-required-reasoning",
		Assistant: llm.Message{Role: "assistant", Content: "继续工作", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": replayRaw,
		}},
	}}
	v5, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV5)
	replay, _ := catalog.ProviderReplayPolicy(v5.ReplayPolicyRef)
	input.BudgetPolicy, input.ReplayPolicy, input.ReplayPolicyRef = v5.Policy, replay.Policy, replay.Ref
	if _, err := New().Compile(context.Background(), input); err == nil {
		t.Fatal("冻结 v5 64KiB RequiredExact cap 应拒绝 v6 事故形状")
	}
	v6, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV6)
	input.BudgetPolicy = v6.Policy
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("v6 不应拒绝 32K completion reserve 内的 RequiredExact reasoning: %v", err)
	}
	if result.OutputBudget.MaxCompletionTokens != 32<<10 || result.OutputBudget.MaxResponseBytes != 256<<10 ||
		result.OutputBudget.MaxExtraFieldBytesByName["reasoning_content"] != 127<<10 {
		t.Fatalf("v6 Invocation/Replay 预算没有同源闭合: %+v", result.OutputBudget)
	}
}

func TestAdapterV7WidensOptionalReasoningBytesWithoutChangingWindowSplit(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	v7, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV7)
	replay, _ := catalog.ProviderReplayPolicy(v7.ReplayPolicyRef)
	input.BudgetPolicy, input.ReplayPolicy, input.ReplayPolicyRef = v7.Policy, replay.Policy, replay.Ref
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputBudget.MaxReasoningBytes != 256<<10 || result.OutputBudget.MaxCompletionTokens != 32<<10 ||
		result.OutputBudget.MaxResponseBytes != 256<<10 || v7.Policy.SnapshotInputBudget.EstimatedTokens != 92<<10 {
		t.Fatalf("v7 仅应放宽 optional reasoning bytes: budget=%+v policy=%+v", result.OutputBudget, v7.Policy)
	}
}

func TestAdapterV8TypesResponsesOutputItemsWithoutMutatingV7(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	v8, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV8)
	replay, _ := catalog.ProviderReplayPolicy(v8.ReplayPolicyRef)
	input.BudgetPolicy, input.ReplayPolicy, input.ReplayPolicyRef = v8.Policy, replay.Policy, replay.Ref
	input.History = []SettledTurn{{
		TurnID: "turn-responses",
		Assistant: llm.Message{Role: "assistant", ExtraFields: map[string]json.RawMessage{
			llm.ResponsesOutputItemsExtraField(): json.RawMessage(`[{"type":"function_call","call_id":"c1","name":"read_file","arguments":"{}"}]`),
		}},
	}}
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Snapshot.Manifest.Items {
		if item.Kind == contextcontract.FragmentAssistantResponseItems {
			found = true
			if item.Disposition != contextcontract.DispositionInline {
				t.Fatalf("Responses carrier disposition=%s", item.Disposition)
			}
		}
	}
	if !found || result.OutputBudget.MaxExtraFieldBytesByName[llm.ResponsesOutputItemsExtraField()] <= 0 {
		t.Fatalf("v8 未形成 typed Responses carrier/budget: manifest=%+v budget=%+v",
			result.Snapshot.Manifest.Items, result.OutputBudget)
	}
	v7, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV7)
	if _, leaked := v7.Policy.FragmentRules[contextcontract.FragmentAssistantResponseItems]; leaked {
		t.Fatal("Responses fragment rule 污染历史 v7")
	}
}

func TestAdapterV3FreezesInvocationBudgetFromCompletionReserveAndReplayPolicy(t *testing.T) {
	input := adapterTestInput(t)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	contextProfile, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV3)
	replayProfile, _ := catalog.ProviderReplayPolicy(contextProfile.ReplayPolicyRef)
	input.BudgetPolicy = contextProfile.Policy
	input.ReplayPolicy, input.ReplayPolicyRef = replayProfile.Policy, replayProfile.Ref
	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	budget := result.OutputBudget
	if budget.MaxResponseBytes != contextProfile.Policy.CompletionReserve.SerializedBytes ||
		budget.MaxCompletionTokens != contextProfile.Policy.CompletionReserve.EstimatedTokens {
		t.Fatalf("Invocation budget 未消费 L2 completion reserve: %+v", budget)
	}
	if budget.MaxExtraFieldBytesByName["reasoning_content"] <= 0 ||
		budget.MaxExtraFieldBytesByName["reasoning_content"] >= contextProfile.Policy.FragmentRules[contextcontract.FragmentAssistantReasoning].MaxSerializedBytes {
		t.Fatalf("RequiredExact reasoning 未提前收紧: %+v", budget.MaxExtraFieldBytesByName)
	}
	if _, cappedOptional := budget.MaxExtraFieldBytesByName["reasoning"]; cappedOptional {
		t.Fatal("Optional reasoning 不应被 RequiredExact replay cap 误伤")
	}
	binding, err := result.InvocationBinding()
	if err != nil || binding.OutputBudget.MaxCompletionTokens != budget.MaxCompletionTokens {
		t.Fatalf("ContextBinding 未冻结同源 OutputBudget: %+v err=%v", binding, err)
	}
}

func TestContextV4TokenEstimatorFixesASCIIWithoutMutatingV2V3(t *testing.T) {
	input := adapterTestInput(t)
	payload := []byte(strings.Repeat("界", 300))
	if got := estimateTokens(input, payload); got != 100 {
		t.Fatalf("历史 v1/v2 estimator 行为被改写: got=%d want=100", got)
	}
	input.BudgetPolicy.Version = 3
	if got := estimateTokens(input, payload); got < 300 {
		t.Fatalf("v3 CJK estimator 仍低估: got=%d want>=300", got)
	}
	ascii := []byte(strings.Repeat("x", 300))
	if got := estimateTokens(input, ascii); got != 300 {
		t.Fatalf("历史 v3 all-runes estimator 被改写: got=%d want=300", got)
	}
	input.BudgetPolicy.Version = 4
	if got := estimateTokens(input, ascii); got != 100 {
		t.Fatalf("v4 ASCII/code estimator 未修正为 bytes/3: got=%d want=100", got)
	}
	if got := estimateTokens(input, payload); got != 300 {
		t.Fatalf("v4 CJK 保守估算被削弱: got=%d want=300", got)
	}
	mixed := []byte(strings.Repeat("abc界", 100))
	if got := estimateTokens(input, mixed); got != 200 {
		t.Fatalf("v4 mixed estimator=%d want=200", got)
	}
}

func TestAdapterRejectsDuplicateToolDefinition(t *testing.T) {
	input := adapterTestInput(t)
	input.ToolRouter.Definitions = append(input.ToolRouter.Definitions, input.ToolRouter.Definitions[0])
	_, err := New().Compile(context.Background(), input)
	var failure *contextcontract.ContextAssemblyFailure
	if !errors.As(err, &failure) || failure.Reason != contextcontract.AssemblyInvalidContract {
		t.Fatalf("重复 ToolDef 未 fail-closed: %v", err)
	}
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("JSON 不一致:\n got=%s\nwant=%s", gotJSON, wantJSON)
	}
}

func hasAtomicGroup(snapshot *contextcontract.ContextSnapshot, kind contextcontract.AtomicGroupKind) bool {
	return atomicGroup(snapshot, kind) != nil
}

func atomicGroup(snapshot *contextcontract.ContextSnapshot, kind contextcontract.AtomicGroupKind) *contextcontract.ProtocolAtomicGroupRecord {
	if snapshot == nil {
		return nil
	}
	for i := range snapshot.AtomicGroups {
		if snapshot.AtomicGroups[i].GroupKind == kind {
			return &snapshot.AtomicGroups[i]
		}
	}
	return nil
}
