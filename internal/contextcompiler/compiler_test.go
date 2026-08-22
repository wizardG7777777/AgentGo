package contextcompiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentgo/internal/contextcontract"
)

func compilerPolicy() contextcontract.ContextBudgetPolicy {
	fragments := make(map[contextcontract.FragmentKind]contextcontract.FragmentBudgetRule)
	for _, kind := range contextcontract.KnownFragmentKinds() {
		fragments[kind] = contextcontract.FragmentBudgetRule{
			MaxSerializedBytes: 4096,
			MaxEstimatedTokens: 1024,
			AllowedDispositions: []contextcontract.Disposition{
				contextcontract.DispositionInline,
				contextcontract.DispositionRejected,
			},
			RetentionClass: contextcontract.RetentionTaskLifetime,
			Priority:       10,
		}
	}
	groups := make(map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupBudgetRule)
	for _, kind := range contextcontract.KnownAtomicGroupKinds() {
		groups[kind] = contextcontract.AtomicGroupBudgetRule{
			MaxSerializedBytes: 8192,
			MaxEstimatedTokens: 2048,
		}
	}
	sections := make(map[contextcontract.ContextSection]contextcontract.Budget)
	for _, section := range contextcontract.KnownContextSections() {
		sections[section] = contextcontract.Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4096}
	}
	return contextcontract.ContextBudgetPolicy{
		Schema: contextcontract.PolicySchemaV1, PolicyID: "compiler-test/v1", Version: 1,
		ModelClass: "test-model", FragmentRules: fragments,
		AtomicGroupRules: groups, SectionBudgets: sections,
		SnapshotInputBudget:   contextcontract.Budget{SerializedBytes: 64 << 10, EstimatedTokens: 16 << 10},
		CompletionReserve:     contextcontract.Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4096},
		AbsoluteWireByteLimit: 96 << 10,
	}
}

func deterministicEncoder(counter *int) WireEncoder {
	return WireEncoderFunc(func(_ context.Context, items []contextcontract.WireItem) ([]byte, error) {
		if counter != nil {
			*counter++
		}
		var out bytes.Buffer
		out.WriteByte('[')
		for i, item := range items {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, err := json.Marshal(struct {
				Kind    contextcontract.WireItemKind `json:"kind"`
				Payload json.RawMessage              `json:"payload"`
			}{Kind: item.Kind, Payload: item.Payload})
			if err != nil {
				return nil, err
			}
			out.Write(encoded)
		}
		out.WriteByte(']')
		return out.Bytes(), nil
	})
}

func baseCompileInput() CompileInput {
	content := []byte(`{"task":"修复问题"}`)
	return CompileInput{
		AttemptID: "attempt-1", InvocationID: "invocation-1",
		PromptBuildRef: "prompt-build:1", ExecutionLeaseRef: "lease:1",
		ToolRouterSnapshotID: "tool-router:1",
		Fragments: []PreparedFragment{{
			Fragment: contextcontract.ContextFragment{
				FragmentID: "task", Kind: contextcontract.FragmentUserTask,
				Section: contextcontract.SectionTaskContract, SourceRef: "task:1",
				Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityAuthoritative,
				Freshness: contextcontract.FreshnessSnapshot,
				Digest:    contextcontract.DigestBytes(content), SerializedBytes: int64(len(content)),
				EstimatedTokens: 8, RetentionClass: contextcontract.RetentionTaskLifetime,
				Content: content, Disposition: contextcontract.DispositionInline,
			},
			WireKind: contextcontract.WireUserMessage, Payload: content,
		}},
		BudgetPolicy: compilerPolicy(),
		ReplayPolicy: contextcontract.ProviderReplayPolicy{
			Schema:   contextcontract.ProviderReplaySchemaV1,
			PolicyID: "replay-test/v1", Version: 1,
			Fields: map[string]contextcontract.ReplayRequirement{},
		},
		Encoder: deterministicEncoder(nil),
	}
}

func compileFailure(t *testing.T, compiler *Compiler, input CompileInput) *contextcontract.ContextAssemblyFailure {
	t.Helper()
	_, err := compiler.Compile(context.Background(), input)
	if err == nil {
		t.Fatal("预期 ContextCompiler 失败，实际成功")
	}
	var failure *contextcontract.ContextAssemblyFailure
	if !errors.As(err, &failure) {
		t.Fatalf("错误类型=%T，不是 ContextAssemblyFailure: %v", err, err)
	}
	if err := failure.Validate(); err != nil {
		t.Fatalf("失败 DTO 自身无效: %v", err)
	}
	return failure
}

func TestCompileInlineProducesSealedSnapshotAndRuntimePayload(t *testing.T) {
	input := baseCompileInput()
	encoderCalls := 0
	input.Encoder = deterministicEncoder(&encoderCalls)
	compiler := &Compiler{Now: func() time.Time {
		return time.Date(2026, 8, 22, 2, 3, 4, 0, time.UTC)
	}}

	result, err := compiler.Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if encoderCalls != 2 {
		t.Fatalf("encoder 调用次数=%d，应为两次确定性对账", encoderCalls)
	}
	if result.Snapshot == nil || len(result.Snapshot.Fragments) != 1 ||
		len(result.Snapshot.WireItems) != 1 || len(result.Runtime.WireItems) != 1 {
		t.Fatalf("编译产物不完整: %+v", result)
	}
	if err := result.Snapshot.Validate(); err != nil {
		t.Fatalf("Snapshot.Validate: %v", err)
	}
	if got := contextcontract.DigestBytes(result.Runtime.EncodedRequest); got != result.Snapshot.EncodedRequestDigest {
		t.Fatalf("encoded request digest=%s，snapshot=%s", got, result.Snapshot.EncodedRequestDigest)
	}
	if result.Snapshot.CompletionReserve != input.BudgetPolicy.CompletionReserve {
		t.Fatal("completion reserve 未按冻结 policy 写入 Snapshot")
	}

	durable, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(durable, []byte("修复问题")) {
		t.Fatalf("durable CompileResult 泄露运行时正文: %s", durable)
	}
}

func TestCompilePreparedReferenceAndTombstone(t *testing.T) {
	input := baseCompileInput()
	refPayload := []byte(`{"result_ref":"graph-result:g1:a@1","summary":"有界摘要"}`)
	refRule := input.BudgetPolicy.FragmentRules[contextcontract.FragmentUpstreamResult]
	refRule.AllowedDispositions = []contextcontract.Disposition{
		contextcontract.DispositionReferenced,
		contextcontract.DispositionRejected,
	}
	refRule.TransformID = "upstream_result_ref/v1"
	input.BudgetPolicy.FragmentRules[contextcontract.FragmentUpstreamResult] = refRule
	input.Fragments = append(input.Fragments, PreparedFragment{
		Fragment: contextcontract.ContextFragment{
			FragmentID: "upstream", Kind: contextcontract.FragmentUpstreamResult,
			Section: contextcontract.SectionUpstreamInputs, SourceRef: "graph-result:g1:a@1",
			Scope: contextcontract.ScopeActivation, Authority: contextcontract.AuthorityInformational,
			Freshness:       contextcontract.FreshnessSnapshot,
			Digest:          contextcontract.DigestBytes([]byte(`{"large":"raw"}`)),
			SerializedBytes: int64(len(refPayload)), EstimatedTokens: 20,
			RetentionClass: contextcontract.RetentionTaskLifetime,
			ContentRef:     "graph-result:g1:a@1", Disposition: contextcontract.DispositionReferenced,
			TransformRef: "upstream_result_ref/v1",
		},
		WireKind: contextcontract.WireUserMessage, Payload: refPayload,
	})

	tombstonePayload := []byte(`{"tool_call_id":"call-1","content_ref":"content:tool-1","tombstone":true}`)
	toolRule := input.BudgetPolicy.FragmentRules[contextcontract.FragmentToolResult]
	toolRule.AllowedDispositions = []contextcontract.Disposition{
		contextcontract.DispositionTombstoned,
		contextcontract.DispositionRejected,
	}
	toolRule.TransformID = "tool_result_ref/v1"
	input.BudgetPolicy.FragmentRules[contextcontract.FragmentToolResult] = toolRule
	groupRule := input.BudgetPolicy.AtomicGroupRules[contextcontract.AtomicAssistantToolExchange]
	groupRule.TransformIDs = []string{"tool_result_ref/v1"}
	input.BudgetPolicy.AtomicGroupRules[contextcontract.AtomicAssistantToolExchange] = groupRule
	input.ReplayPolicy.GroupTransforms = []contextcontract.ReplayTransform{{
		GroupKind: contextcontract.AtomicAssistantToolExchange, TransformID: "tool_result_ref/v1",
	}}
	input.Fragments = append(input.Fragments, PreparedFragment{
		Fragment: contextcontract.ContextFragment{
			FragmentID: "tool-result", Kind: contextcontract.FragmentToolResult,
			Section: contextcontract.SectionToolResults, SourceRef: "tool-result:call-1",
			Scope: contextcontract.ScopeTurn, Authority: contextcontract.AuthorityInformational,
			Freshness:       contextcontract.FreshnessLive,
			Digest:          contextcontract.DigestBytes([]byte("原始工具结果")),
			SerializedBytes: int64(len(tombstonePayload)), EstimatedTokens: 24,
			RetentionClass: contextcontract.RetentionTaskLifetime,
			ReplayGroupID:  "tool-exchange-1", ContentRef: "content:tool-1",
			Disposition:  contextcontract.DispositionTombstoned,
			TransformRef: "tool_result_ref/v1",
		},
		WireKind: contextcontract.WireToolMessage, Payload: tombstonePayload,
	})
	input.AtomicGroups = []contextcontract.ProtocolAtomicGroup{{
		GroupID: "tool-exchange-1", GroupKind: contextcontract.AtomicAssistantToolExchange,
		FragmentIDs: []string{"tool-result"}, ReplayPolicy: contextcontract.ReplayOptional,
		TransformID: "tool_result_ref/v1",
	}}

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile reference/tombstone: %v", err)
	}
	if len(result.Snapshot.Fragments) != 3 || len(result.Snapshot.AtomicGroups) != 1 {
		t.Fatalf("Snapshot records 不完整: %+v", result.Snapshot)
	}
	if result.Snapshot.Fragments[1].ContentRef != "graph-result:g1:a@1" ||
		result.Snapshot.Fragments[2].Disposition != contextcontract.DispositionTombstoned {
		t.Fatalf("ref/tombstone 元数据不正确: %+v", result.Snapshot.Fragments)
	}
}

func TestCompileRecordsDroppedOptionalProviderFieldWithoutWireUsage(t *testing.T) {
	input := baseCompileInput()
	rule := input.BudgetPolicy.FragmentRules[contextcontract.FragmentAssistantReasoning]
	rule.AllowedDispositions = append(rule.AllowedDispositions, contextcontract.DispositionDropped)
	rule.RetentionClass = contextcontract.RetentionEphemeralRequest
	input.BudgetPolicy.FragmentRules[contextcontract.FragmentAssistantReasoning] = rule
	input.ReplayPolicy.Fields["reasoning"] = contextcontract.ReplayOptional
	raw := bytes.Repeat([]byte("x"), int(rule.MaxSerializedBytes)+1)
	input.Fragments = append(input.Fragments, PreparedFragment{
		Fragment: contextcontract.ContextFragment{
			FragmentID: "reasoning-dropped", Kind: contextcontract.FragmentAssistantReasoning,
			Section: contextcontract.SectionConversationHistory, SourceRef: "turn:1/provider-extra:reasoning",
			Scope: contextcontract.ScopeTurn, Authority: contextcontract.AuthorityInformational,
			Freshness: contextcontract.FreshnessSnapshot, Digest: contextcontract.DigestBytes(raw),
			SerializedBytes: int64(len(raw)), EstimatedTokens: rule.MaxEstimatedTokens + 1,
			RetentionClass: contextcontract.RetentionEphemeralRequest,
			Disposition:    contextcontract.DispositionDropped,
		},
		ProviderField: "reasoning",
	})

	result, err := New().Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile dropped optional provider field: %v", err)
	}
	if len(result.Snapshot.Fragments) != 2 || len(result.Snapshot.WireItems) != 1 {
		t.Fatalf("dropped fragment 不应生成 wire: fragments=%d wires=%d",
			len(result.Snapshot.Fragments), len(result.Snapshot.WireItems))
	}
	dropped := result.Snapshot.Fragments[1]
	if dropped.Disposition != contextcontract.DispositionDropped || dropped.WireID != "" ||
		dropped.OutputDigest != "" || dropped.SerializedBytes != int64(len(raw)) {
		t.Fatalf("dropped provider record 错误: %+v", dropped)
	}
	if result.Snapshot.InputBudgetUsed != baseCompileInputUsage(t, input.Fragments[0]) {
		t.Fatalf("dropped fragment 不得计入 wire usage: %+v", result.Snapshot.InputBudgetUsed)
	}
}

func baseCompileInputUsage(t *testing.T, prepared PreparedFragment) contextcontract.BudgetUsage {
	t.Helper()
	return contextcontract.BudgetUsage{
		SerializedBytes: int64(len(prepared.Payload)), EstimatedTokens: prepared.Fragment.EstimatedTokens,
	}
}

func TestCompileBudgetFailuresAreLayered(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CompileInput)
		want   contextcontract.AssemblyFailureReason
	}{
		{
			name: "单项 hard cap",
			mutate: func(input *CompileInput) {
				rule := input.BudgetPolicy.FragmentRules[contextcontract.FragmentUserTask]
				rule.MaxSerializedBytes = 1
				input.BudgetPolicy.FragmentRules[contextcontract.FragmentUserTask] = rule
			},
			want: contextcontract.AssemblyFragmentLimitExceeded,
		},
		{
			name: "原子组 hard cap",
			mutate: func(input *CompileInput) {
				input.Fragments[0].Fragment.ReplayGroupID = "group-1"
				input.AtomicGroups = []contextcontract.ProtocolAtomicGroup{{
					GroupID: "group-1", GroupKind: contextcontract.AtomicUserTaskContract,
					FragmentIDs: []string{"task"}, ReplayPolicy: contextcontract.ReplayRequiredExact,
				}}
				rule := input.BudgetPolicy.AtomicGroupRules[contextcontract.AtomicUserTaskContract]
				rule.MaxSerializedBytes = 1
				input.BudgetPolicy.AtomicGroupRules[contextcontract.AtomicUserTaskContract] = rule
			},
			want: contextcontract.AssemblyAtomicGroupLimitExceeded,
		},
		{
			name: "section budget",
			mutate: func(input *CompileInput) {
				input.BudgetPolicy.SectionBudgets[contextcontract.SectionTaskContract] =
					contextcontract.Budget{SerializedBytes: 1, EstimatedTokens: 4096}
			},
			want: contextcontract.AssemblySectionBudgetExceeded,
		},
		{
			name: "snapshot total budget",
			mutate: func(input *CompileInput) {
				input.BudgetPolicy.SnapshotInputBudget =
					contextcontract.Budget{SerializedBytes: 1, EstimatedTokens: 4096}
			},
			want: contextcontract.AssemblySnapshotBudgetExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := baseCompileInput()
			test.mutate(&input)
			failure := compileFailure(t, New(), input)
			if failure.Reason != test.want {
				t.Fatalf("failure reason=%s，want=%s，detail=%s", failure.Reason, test.want, failure.Detail)
			}
		})
	}
}

func TestCompileFragmentLimitFailureNamesSafeKindSectionAndBudgets(t *testing.T) {
	input := baseCompileInput()
	rule := input.BudgetPolicy.FragmentRules[contextcontract.FragmentUserTask]
	rule.MaxSerializedBytes = 1
	input.BudgetPolicy.FragmentRules[contextcontract.FragmentUserTask] = rule
	failure := compileFailure(t, New(), input)
	if failure.Reason != contextcontract.AssemblyFragmentLimitExceeded ||
		failure.Section != contextcontract.SectionTaskContract ||
		!strings.Contains(failure.Detail, "kind=user_task") ||
		!strings.Contains(failure.Detail, "section=task_contract") ||
		!strings.Contains(failure.Detail, "actual=") || !strings.Contains(failure.Detail, "limit=") {
		t.Fatalf("fragment limit 诊断缺少安全定位事实: %+v", failure)
	}
}

func TestCompileRejectsMissingCompletionReserve(t *testing.T) {
	input := baseCompileInput()
	input.BudgetPolicy.CompletionReserve = contextcontract.Budget{}
	failure := compileFailure(t, New(), input)
	if failure.Reason != contextcontract.AssemblyCompletionReserveUnavailable {
		t.Fatalf("failure reason=%s，want completion_reserve_unavailable", failure.Reason)
	}
}

func TestCompileRejectsUnknownProviderReplay(t *testing.T) {
	input := baseCompileInput()
	content := []byte(`"reasoning"`)
	input.Fragments = []PreparedFragment{{
		Fragment: contextcontract.ContextFragment{
			FragmentID: "reasoning", Kind: contextcontract.FragmentAssistantReasoning,
			Section:   contextcontract.SectionConversationHistory,
			SourceRef: "turn:1/reasoning", Scope: contextcontract.ScopeTurn,
			Authority: contextcontract.AuthorityInformational,
			Freshness: contextcontract.FreshnessSnapshot,
			Digest:    contextcontract.DigestBytes(content), SerializedBytes: int64(len(content)),
			EstimatedTokens: 4, RetentionClass: contextcontract.RetentionTaskLifetime,
			Content: content, Disposition: contextcontract.DispositionInline,
		},
		WireKind: contextcontract.WireProviderExtra, Payload: content,
		ProviderField: "reasoning_content",
	}}
	failure := compileFailure(t, New(), input)
	if failure.Reason != contextcontract.AssemblyProviderReplayUnknown {
		t.Fatalf("failure reason=%s，want provider_replay_unknown", failure.Reason)
	}
}

func TestCompileRejectsNonDeterministicEncoding(t *testing.T) {
	input := baseCompileInput()
	calls := 0
	input.Encoder = WireEncoderFunc(func(context.Context, []contextcontract.WireItem) ([]byte, error) {
		calls++
		return []byte(fmt.Sprintf("call-%d", calls)), nil
	})
	failure := compileFailure(t, New(), input)
	if failure.Reason != contextcontract.AssemblyNonDeterministicEncoding {
		t.Fatalf("failure reason=%s，want non_deterministic_encoding", failure.Reason)
	}
}

func TestCompileRejectsAbsoluteWireOverflow(t *testing.T) {
	input := baseCompileInput()
	input.BudgetPolicy.AbsoluteWireByteLimit = input.BudgetPolicy.SnapshotInputBudget.SerializedBytes
	input.Encoder = WireEncoderFunc(func(context.Context, []contextcontract.WireItem) ([]byte, error) {
		return bytes.Repeat([]byte{'x'}, int(input.BudgetPolicy.AbsoluteWireByteLimit)+1), nil
	})
	failure := compileFailure(t, New(), input)
	if failure.Reason != contextcontract.AssemblySnapshotBudgetExceeded {
		t.Fatalf("failure reason=%s，want snapshot_budget_exceeded", failure.Reason)
	}
}
