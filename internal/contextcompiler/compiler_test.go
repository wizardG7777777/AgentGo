package contextcompiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"agentgo/internal/contextcontract"
)

func compilerPolicy() contextcontract.ContextBudgetPolicy {
	fragments := make(map[contextcontract.FragmentKind]contextcontract.FragmentRuleSpec)
	for _, kind := range contextcontract.KnownFragmentKinds() {
		fragments[kind] = contextcontract.FragmentRuleSpec{
			AllowedDispositions: []contextcontract.Disposition{
				contextcontract.DispositionInline,
				contextcontract.DispositionRejected,
			},
			RetentionClass: contextcontract.RetentionTaskLifetime,
			Priority:       10,
		}
	}
	groups := make(map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupRuleSpec)
	for _, kind := range contextcontract.KnownAtomicGroupKinds() {
		groups[kind] = contextcontract.AtomicGroupRuleSpec{}
	}
	return contextcontract.ContextBudgetPolicy{
		Schema: contextcontract.PolicySchemaV2, PolicyID: "compiler-test/v1", Version: 12,
		ModelClass: "test-model", FragmentRules: fragments,
		AtomicGroupRules:      groups,
		SnapshotInputBudget:   contextcontract.Budget{SerializedBytes: 64 << 10, EstimatedTokens: 16 << 10},
		CompletionReserve:     contextcontract.Budget{SerializedBytes: 16 << 10, EstimatedTokens: 4096},
		AbsoluteWireByteLimit: 96 << 10, ModelContextWindow: &contextcontract.Budget{
			SerializedBytes: 16 <<
				20, EstimatedTokens: 4 << 20}, ProtocolOverheadReserve: &contextcontract.Budget{
			SerializedBytes: 4096,
			EstimatedTokens: 1024},
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
		InstructionRef: "prompt-build:1", ExecutionLeaseRef: "lease:1",
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
			PolicyID: "replay-test/v1", Version: 6,
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

func baseCompileInputUsage(t *testing.T, prepared PreparedFragment) contextcontract.BudgetUsage {
	t.Helper()
	return contextcontract.BudgetUsage{
		SerializedBytes: int64(len(prepared.Payload)), EstimatedTokens: prepared.Fragment.EstimatedTokens,
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
