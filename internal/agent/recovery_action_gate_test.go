package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
)

func TestFrozenRecoveryDirectiveSelectsLatestGeneration(t *testing.T) {
	task := &model.Task{ContextInputs: []model.TaskContextInput{
		recoveryDirectiveContextInput("recovery@1", graph.RecoveryDeltaSchemaV2, "read_file", "src/old.py"),
		recoveryDirectiveContextInput("recovery@2", graph.RecoveryDeltaSchemaV2, "edit_file", "src/new.py"),
	}}
	directive, ok := frozenRecoveryDirective(task)
	if !ok || directive.DirectiveCount != 2 || directive.FirstAction.Tool != "edit_file" ||
		directive.FirstAction.Path != "src/new.py" {
		t.Fatalf("多代历史 directive 必须选择最后一代并保留歧义计数: %+v ok=%t", directive, ok)
	}
}

func TestRecoveryDeltaV3ForcesReadMutationAndCheckStages(t *testing.T) {
	registry := NewToolRegistry()
	register := func(name string, parameters map[string]any) {
		registry.Register(name, name, parameters,
			func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	pathParams := map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]any{"type": "string"},
	}}
	register("read_file", pathParams)
	register("edit_file", pathParams)
	register("grep_search", map[string]any{"type": "object"})
	register("run_check", map[string]any{"type": "object", "properties": map[string]any{
		"check_id": map[string]any{"type": "string"},
		"kind":     map[string]any{"type": "string"},
		"command":  map[string]any{"type": "string"},
	}})
	task := &model.Task{ContextInputs: []model.TaskContextInput{
		recoveryDirectiveContextInput("recovery@1", graph.RecoveryDeltaSchemaV3, "read_file", "src/flask/ctx.py"),
	}, RunContract: &runcontract.RunContract{CheckContracts: []runcontract.CheckContract{
		{CheckID: "targeted", Kind: "test", ExactCommand: "uv run --no-sync python -m pytest -q 'tests/test_reqctx.py' 'tests/test_subclassing.py'"},
		{CheckID: "verification", Kind: "test", ExactCommand: "uv run --no-sync python -m pytest -q"},
	}}}

	assertRecoveryGate(t, deriveInvocationToolPolicyWithControl(task, nil, registry, registry),
		"agent:recovery-first-action", "read_file", "path", "src/flask/ctx.py")

	history := []HistoryEntry{{
		ToolCalls:   []llm.ToolCall{{ID: "read", Name: "read_file", Arguments: map[string]any{"path": "src/flask/ctx.py"}}},
		ToolResults: []ToolResult{{ToolCallID: "read", Content: "file content"}},
	}}
	assertRecoveryGate(t, deriveInvocationToolPolicyWithControl(task, history, registry, registry),
		"agent:recovery-mutation", "edit_file", "path", "src/flask/ctx.py")

	history = append(history, HistoryEntry{
		ToolCalls: []llm.ToolCall{{ID: "edit", Name: "edit_file", Arguments: map[string]any{
			"path": "src/flask/ctx.py", "old_str": "old", "new_str": "new",
		}}},
		ToolResults: []ToolResult{{ToolCallID: "edit", Content: "编辑成功"}},
	})
	assertRecoveryGate(t, deriveInvocationToolPolicyWithControl(task, history, registry, registry),
		"agent:recovery-check", "run_check", "check_id", "targeted")
	checkPolicy := deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, checkPolicy, "agent:recovery-check", "run_check", "kind", "test")
	assertRecoveryGate(t, checkPolicy, "agent:recovery-check", "run_check", "command",
		"uv run --no-sync python -m pytest -q 'tests/test_reqctx.py' 'tests/test_subclassing.py'")

	history = append(history, HistoryEntry{
		ToolCalls: []llm.ToolCall{{ID: "check", Name: "run_check", Arguments: map[string]any{
			"check_id": "targeted", "kind": "test", "command": "uv run --no-sync python -m pytest -q 'tests/test_reqctx.py' 'tests/test_subclassing.py'",
		}}},
		ToolResults: []ToolResult{{ToolCallID: "check", Content: `{"check_id":"targeted","status":"failed"}`}},
	})
	policy := deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	if stringsHasRecoveryPhase(policy.Phase) || !containsToolName(policy.Registry.Names(), "grep_search") {
		t.Fatalf("形成真实 CheckRecord 后应恢复普通业务工具面: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
}

func TestRecoveryDeltaV4CompletesEvidenceBeforeTypedEditDecision(t *testing.T) {
	registry := NewToolRegistry()
	register := func(name string, parameters map[string]any) {
		registry.Register(name, name, parameters,
			func(context.Context, map[string]any) (string, error) { return "ok", nil })
	}
	register("read_file", map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"},
		"limit": map[string]any{"type": "integer"}, "force_full": map[string]any{"type": "boolean"},
	}})
	register("read_content_ref", map[string]any{"type": "object", "properties": map[string]any{
		"ref_id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"},
		"limit": map[string]any{"type": "integer"},
	}})
	register("edit_file", map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]any{"type": "string"},
	}})
	register("write_file", map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]any{"type": "string"},
	}})
	register("run_check", map[string]any{"type": "object", "properties": map[string]any{
		"check_id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string"},
		"command": map[string]any{"type": "string"},
	}})
	register("submit_change_decision", map[string]any{"type": "object", "properties": map[string]any{
		"decision": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
		"edit_steps": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "properties": map[string]any{
				"tool": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
			},
		}},
	}})
	task := &model.Task{GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV4,
		ContextInputs: []model.TaskContextInput{recoveryDirectiveContextInputV4(
			"recovery@1", []string{"src/a.py", "src/b.py"})},
		RunContract: &runcontract.RunContract{CheckContracts: []runcontract.CheckContract{{
			CheckID: "targeted", Kind: "test", ExactCommand: "pytest -q tests/test_a.py",
		}}},
	}
	policy := deriveInvocationToolPolicyWithControl(task, nil, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_file", "path", "src/a.py")
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_file", "offset", 1)

	history := []HistoryEntry{toolHistory("read-a", "read_file", map[string]any{
		"path": "src/a.py", "offset": 1, "limit": recoveryEvidenceReadLines,
	}, "[file] src/a.py (2 lines, full)\n[hash] a\n---\na\nb")}
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_file", "path", "src/b.py")

	envelope := `{"schema":"agentgo.tool-result-ref/v1","ref_id":"content:b","sha256":"digest-b","preview_head":"[file] src/b.py (lines 1-80 of 100)\n[hash] b\n---\nhead","preview_tail":"tail"}`
	history = append(history, toolHistory("read-b-1", "read_file", map[string]any{
		"path": "src/b.py", "offset": 1, "limit": recoveryEvidenceReadLines,
	}, envelope))
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_content_ref", "ref_id", "content:b")
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_content_ref", "offset", int64(0))

	history = append(history, toolHistory("page-b", "read_content_ref", map[string]any{
		"ref_id": "content:b", "offset": 0, "limit": recoveryEvidenceContentPageByte,
	}, `{"next_offset":12000,"eof":true,"digest":"digest-b","encoding":"utf-8","content":"full"}`))
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_file", "offset", 81)

	history = append(history, toolHistory("read-b-2", "read_file", map[string]any{
		"path": "src/b.py", "offset": 81, "limit": recoveryEvidenceReadLines,
	}, "[file] src/b.py (lines 81-100 of 100)\n[hash] b\n---\nrest"))
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	if policy.Phase != "agent:recovery-change-decision" ||
		!sameExactToolSet(policy.Registry.Names(), []string{"submit_change_decision"}) {
		t.Fatalf("完整覆盖后必须进入 typed change decision: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}

	history = append(history, toolHistory("need-c", "submit_change_decision", map[string]any{
		"decision": "need_context", "path": "src/c.py",
	}, `{"schema":"agentgo.change-decision/v1","decision":"need_context","path":"src/c.py","summary":"need c"}`))
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_file", "path", "src/c.py")
	history = append(history, toolHistory("read-c", "read_file", map[string]any{
		"path": "src/c.py", "offset": 1, "limit": recoveryEvidenceReadLines,
	}, "[file] src/c.py (1 lines, full)\n[hash] c\n---\nc"))
	history = append(history, toolHistory("edit-decision", "submit_change_decision", map[string]any{
		"decision": "edit", "edit_steps": []any{
			map[string]any{"tool": "edit_file", "path": "src/a.py"},
			map[string]any{"tool": "edit_file", "path": "src/a.py"},
			map[string]any{"tool": "write_file", "path": "tests/new_test.py"},
		},
	}, `{"schema":"agentgo.change-decision/v1","decision":"edit","edit_steps":[{"tool":"edit_file","path":"src/a.py"},{"tool":"edit_file","path":"src/a.py"},{"tool":"write_file","path":"tests/new_test.py"}],"summary":"edit"}`))
	steps := []graph.RecoveryEditStep{
		{Tool: "edit_file", Path: "src/a.py"},
		{Tool: "edit_file", Path: "src/a.py"},
		{Tool: "write_file", Path: "tests/new_test.py"},
	}
	for index, step := range steps {
		policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
		assertRecoveryGate(t, policy, "agent:recovery-mutation", step.Tool, "path", step.Path)
		history = append(history, toolHistory(fmt.Sprintf("edit-%d", index), step.Tool,
			map[string]any{"path": step.Path}, "修改成功"))
	}
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-check", "run_check", "check_id", "targeted")
	history = append(history, toolHistory("check", "run_check", map[string]any{
		"check_id": "targeted", "kind": "test", "command": "pytest -q tests/test_a.py",
	}, `{"check_id":"targeted","status":"failed"}`))
	policy = deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	assertRecoveryGate(t, policy, "agent:recovery-evidence", "read_file", "path", "src/a.py")
}

func TestRecoveryDeltaV4FailedEvidenceReadCanOnlyExitSafely(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("read_file", "读取", map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"},
		"limit": map[string]any{"type": "integer"}, "force_full": map[string]any{"type": "boolean"},
	}}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	registry.Register("read_content_ref", "解引用", map[string]any{"type": "object", "properties": map[string]any{
		"ref_id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"},
		"limit": map[string]any{"type": "integer"},
	}}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	registry.Register("submit_change_decision", "决策", map[string]any{
		"type": "object", "properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []any{
				"edit", "need_context", "hypothesis_rejected", "blocked",
			}},
		},
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	task := &model.Task{GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV4,
		ContextInputs: []model.TaskContextInput{recoveryDirectiveContextInputV4(
			"recovery@1", []string{"src/missing.py"})},
	}
	history := []HistoryEntry{toolHistory("read-missing", "read_file", map[string]any{
		"path": "src/missing.py", "offset": 1, "limit": recoveryEvidenceReadLines,
	}, "错误: 读取文件失败: file does not exist")}
	policy := deriveInvocationToolPolicyWithControl(task, history, registry, registry)
	if policy.Phase != "agent:recovery-evidence-unavailable" ||
		!sameExactToolSet(policy.Registry.Names(), []string{"submit_change_decision"}) {
		t.Fatalf("证据读取失败必须进入安全退出 phase: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
	properties := policy.Registry.Defs()[0].Parameters["properties"].(map[string]any)
	decision := properties["decision"].(map[string]any)
	if !sameExactToolSet(anyStrings(decision["enum"]), []string{"hypothesis_rejected", "blocked"}) {
		t.Fatalf("证据读取失败不得开放 edit/need_context: %#v", decision)
	}

	envelope := `{"schema":"agentgo.tool-result-ref/v1","ref_id":"content:missing","sha256":"digest-missing","preview_head":"[file] src/missing.py (lines 1-80 of 100)\n---\nhead","preview_tail":"tail"}`
	contentHistory := []HistoryEntry{
		toolHistory("read-ref", "read_file", map[string]any{
			"path": "src/missing.py", "offset": 1, "limit": recoveryEvidenceReadLines,
		}, envelope),
		toolHistory("page-failed", "read_content_ref", map[string]any{
			"ref_id": "content:missing", "offset": 0, "limit": recoveryEvidenceContentPageByte,
		}, "错误: ContentRef 不存在"),
	}
	policy = deriveInvocationToolPolicyWithControl(task, contentHistory, registry, registry)
	if policy.Phase != "agent:recovery-evidence-unavailable" ||
		!sameExactToolSet(policy.Registry.Names(), []string{"submit_change_decision"}) {
		t.Fatalf("ContentRef 读取失败也必须安全退出: phase=%s tools=%v", policy.Phase, policy.Registry.Names())
	}
}

func TestRecoveryDeltaV3NarrowsControllerDecisionSchema(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("submit_recovery_decision", "恢复裁决", map[string]any{
		"type": "object", "properties": map[string]any{
			"first_action": map[string]any{"type": "object", "properties": map[string]any{
				"tool": map[string]any{"type": "string", "enum": []any{"read_file", "edit_file"}},
				"path": map[string]any{"type": "string"},
			}, "required": []any{"tool"}},
		}}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	task := &model.Task{GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV3}
	view := recoveryDecisionRegistry(registry, task)
	properties := view.Defs()[0].Parameters["properties"].(map[string]any)
	firstAction := properties["first_action"].(map[string]any)
	actionProperties := firstAction["properties"].(map[string]any)
	tool := actionProperties["tool"].(map[string]any)
	if !sameExactToolSet(anyStrings(tool["enum"]), []string{"read_file"}) ||
		!sameExactToolSet(anyStrings(firstAction["required"]), []string{"path", "tool"}) {
		t.Fatalf("v3 controller schema 未冻结为带 path 的 read_file: %#v", firstAction)
	}
}

func TestRecoveryDeltaV4ExposesEvidenceContractInControllerSchema(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register("submit_recovery_decision", "恢复裁决", map[string]any{
		"type": "object", "properties": map[string]any{
			"first_action": map[string]any{"type": "object", "properties": map[string]any{
				"tool": map[string]any{"type": "string", "enum": []any{"read_file", "edit_file"}},
				"path": map[string]any{"type": "string"},
			}, "required": []any{"tool"}},
		},
		"allOf": []any{map[string]any{"then": map[string]any{"required": []any{"first_action"}}}},
	}, func(context.Context, map[string]any) (string, error) { return "ok", nil })
	task := &model.Task{GraphRecoveryDeltaSchema: graph.RecoveryDeltaSchemaV4}
	view := recoveryDecisionRegistry(registry, task)
	parameters := view.Defs()[0].Parameters
	properties := parameters["properties"].(map[string]any)
	contract, ok := properties["evidence_contract"].(map[string]any)
	if !ok {
		t.Fatalf("v4 controller schema 缺少 EvidenceContract: %#v", parameters)
	}
	contractProperties := contract["properties"].(map[string]any)
	files := contractProperties["files"].(map[string]any)
	if files["maxItems"] != graph.MaxRecoveryEvidenceFiles {
		t.Fatalf("EvidenceContract files 上限未与 Runtime 同源: %#v", files)
	}
}

func recoveryDirectiveContextInput(sourceActivation, schema, tool, path string) model.TaskContextInput {
	return model.TaskContextInput{Kind: model.TaskContextUpstreamResult, Content: `<upstream-result authority="graph-dataflow">
{"source_activation_id":"` + sourceActivation + `","target_input":"recovery_directive","result":{"schema":"` + schema + `","first_action":{"tool":"` + tool + `","path":"` + path + `"}}}
</upstream-result>`}
}

func assertRecoveryGate(t *testing.T, policy invocationToolPolicy, phase, toolName, field string, value any) {
	t.Helper()
	if policy.Phase != phase || policy.RecoveryGate == nil ||
		!sameExactToolSet(policy.Registry.Names(), []string{toolName}) {
		t.Fatalf("Recovery gate 不符: phase=%s tools=%v gate=%+v", policy.Phase, policy.Registry.Names(), policy.RecoveryGate)
	}
	definitions := policy.Registry.Defs()
	properties := definitions[0].Parameters["properties"].(map[string]any)
	property := properties[field].(map[string]any)
	if property["const"] != value {
		t.Fatalf("Recovery gate 未冻结 %s=%v: %#v", field, value, property)
	}
}

func recoveryDirectiveContextInputV4(sourceActivation string, files []string) model.TaskContextInput {
	encoded, _ := json.Marshal(map[string]any{
		"source_activation_id": sourceActivation, "target_input": "recovery_directive",
		"result": map[string]any{
			"schema":            graph.RecoveryDeltaSchemaV4,
			"first_action":      map[string]any{"tool": "read_file", "path": files[0]},
			"evidence_contract": map[string]any{"files": files},
		},
	})
	return model.TaskContextInput{Kind: model.TaskContextUpstreamResult,
		Content: `<upstream-result authority="graph-dataflow">` + string(encoded) + `</upstream-result>`}
}

func toolHistory(id, name string, arguments map[string]any, result string) HistoryEntry {
	return HistoryEntry{ToolCalled: true,
		ToolCalls:   []llm.ToolCall{{ID: id, Name: name, Arguments: arguments}},
		ToolResults: []ToolResult{{ToolCallID: id, Content: result}},
	}
}

func stringsHasRecoveryPhase(phase string) bool {
	return len(phase) >= len("agent:recovery-") && phase[:len("agent:recovery-")] == "agent:recovery-"
}

func anyStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}
