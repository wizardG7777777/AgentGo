package agent

import (
	"context"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/model"
)

func BenchmarkRecoveryFirstActionToolPolicy(b *testing.B) {
	registry, task := recoveryFirstActionBenchmarkFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		policy := deriveInvocationToolPolicyWithControl(task, nil, registry, registry)
		if policy.Phase != "agent:recovery-first-action" || len(policy.Registry.Names()) != 1 {
			b.Fatalf("recovery first action policy 未冻结: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
		}
		definitions := policy.Registry.Defs()
		properties := definitions[0].Parameters["properties"].(map[string]any)
		path := properties["path"].(map[string]any)
		if path["const"] != "src/flask/app.py" {
			b.Fatalf("recovery first action path 未冻结为 const: %#v", path)
		}
	}
}

func BenchmarkRecoveryFirstActionToolPolicyAttempted(b *testing.B) {
	registry, task := recoveryFirstActionBenchmarkFixture()
	history := []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "read", Name: "read_file"}},
		ToolResults: []ToolResult{{ToolCallID: "read", Content: "ok"}},
	}}
	b.ReportAllocs()
	for b.Loop() {
		policy := deriveInvocationToolPolicyWithControl(task, history, registry, registry)
		if policy.Phase == "agent:recovery-first-action" {
			b.Fatal("已结算首动作后不应继续冻结")
		}
	}
}

func BenchmarkRecoveryHandoffV3MutationPolicy(b *testing.B) {
	registry, task := recoveryFirstActionBenchmarkFixture()
	task.ContextInputs = []model.TaskContextInput{
		recoveryDirectiveContextInput("recovery@1", graph.RecoveryDeltaSchemaV3, "read_file", "src/flask/app.py"),
	}
	history := []HistoryEntry{{
		ToolCalls: []llm.ToolCall{{ID: "read", Name: "read_file", Arguments: map[string]any{
			"path": "src/flask/app.py",
		}}},
		ToolResults: []ToolResult{{ToolCallID: "read", Content: "file content"}},
	}}
	b.ReportAllocs()
	for b.Loop() {
		policy := deriveInvocationToolPolicyWithControl(task, history, registry, registry)
		if policy.Phase != "agent:recovery-mutation" ||
			!sameExactToolSet(policy.Registry.Names(), []string{"edit_file"}) {
			b.Fatalf("v3 mutation gate 未冻结: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
		}
	}
}

func recoveryFirstActionBenchmarkFixture() (*ToolRegistry, *model.Task) {
	registry := NewToolRegistry()
	registry.Register("read_file", "读取文件", map[string]any{
		"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string"},
		}, "required": []any{"path"},
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	registry.Register("grep_search", "搜索", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (string, error) { return "ok", nil })
	registry.Register("edit_file", "编辑文件", map[string]any{
		"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string"},
		}, "required": []any{"path"},
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	task := &model.Task{ID: "benchmark-recovery-first-action", ContextInputs: []model.TaskContextInput{{
		Kind: model.TaskContextUpstreamResult,
		Content: `<upstream-result authority="graph-dataflow">
{"target_input": "recovery_directive", "result": {"schema": "` + graph.RecoveryDeltaSchemaV2 + `", "first_action": {"tool": "read_file", "path": "src/flask/app.py"}}}
</upstream-result>`,
	}}}
	return registry, task
}
