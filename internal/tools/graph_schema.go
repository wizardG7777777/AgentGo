package tools

import (
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func (g GraphAuthoringGroup) validateDefinitionRoutes(graphID string, body graph.GraphDefinitionBody) error {
	nodes := make(map[string]graph.Node, len(body.Nodes))
	for id, definition := range body.Nodes {
		nodes[id] = graph.Node{
			Kind: definition.Kind, Task: definition.Task, Capability: definition.Capability,
			Next: definition.Next, Wait: definition.Wait, Tool: definition.Tool,
			Subgraph: definition.Subgraph, Metadata: definition.Metadata, Extensions: definition.Extensions,
		}
	}
	return (graphRouteValidator{RouteValidator: g.RouteValidator}).validateRoutes(graphID, nodes, "nodes")
}

func decodeNativeGraphArgs(args map[string]any, target any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("参数含多余 JSON 内容")
	}
	return nil
}

func schedulerRequestDigest(task *model.Task) string {
	runID := ""
	if task != nil {
		runID = string(task.RunID)
	}
	payload := "agentgo.scheduler-request/v1\x00" + runID + "\x00" + task.Description
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func marshalGraphAuthoringResult(value any) (string, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func graphNodeNativeSchema(withID bool) map[string]any {
	properties := map[string]any{
		"kind": map[string]any{"type": "string", "enum": []string{"controller", "agent", "tool", "router", "join", "approval", "wait_event", "acceptance", "end"}},
		"task": nativeObject(map[string]any{
			"title": nativeString("任务标题"), "description": nativeString("任务与验收/输出说明"),
			"required_inputs": nativeArray(nativeString("输入端口")),
		}, "title"),
		"capability": nativeObject(map[string]any{
			"tools": nativeArray(nativeString("工具名")), "model": nativeString("模型覆盖"), "isolation": nativeString("workspace"),
		}),
		"next": nativeArray(nativeObject(map[string]any{
			"to": nativeString("目标节点"), "activation": nativeString("new"), "target_input": nativeString("目标输入端口"),
			"when": nativeObject(map[string]any{
				"event": nativeString("completed/failed/blocked/always"), "path": nativeString("$.field"),
				"operator": nativeString("eq/ne/in/exists"), "value": map[string]any{},
			}),
		}, "to")),
		"wait":        nativeObject(map[string]any{"event": nativeString("外部事件"), "timeout_sec": nativeInteger("超时秒")}, "event"),
		"tool":        nativeObject(map[string]any{"name": nativeString("工具名"), "args": map[string]any{"type": "object"}}, "name"),
		"metadata":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		"end_outcome": map[string]any{"type": "string", "enum": []string{"success", "failed", "blocked", "cancelled"}},
		"output_contract": nativeObject(map[string]any{
			"summary_required": map[string]any{"type": "boolean"},
			"fields": nativeArray(nativeObject(map[string]any{
				"path": nativeString("$.field"), "type": nativeString("字段类型"),
				"description": nativeString("说明"), "required": map[string]any{"type": "boolean"},
			}, "path", "type")),
		}),
		"progress_contract_ref": nativeString("framework ProgressContract ref"),
		"context_policy_ref":    nativeString("framework ContextPolicy ref"),
		"contract_bindings": nativeObject(map[string]any{
			"deliverables": nativeArray(nativeString("deliverable ID")), "effects": nativeArray(nativeString("effect kind")),
			"artifacts":        nativeArray(nativeString("artifact ID")),
			"success_evidence": nativeArray(nativeString("evidence ID")),
		}),
	}
	required := []string{"kind", "next"}
	if withID {
		properties["id"] = nativeString("节点 ID")
		required = append([]string{"id"}, required...)
	}
	return nativeObject(properties, required...)
}

func graphContractNativeSchema() map[string]any {
	requirement := nativeObject(map[string]any{
		"id": nativeString("稳定 requirement ID"), "kind": nativeString("framework kind"), "description": nativeString("说明"),
	}, "id", "kind")
	return nativeObject(map[string]any{
		"execution_class": map[string]any{"type": "string", "enum": []string{"answer", "read_only", "mutating", "interactive", "waiting"}},
		"deliverables":    nativeArray(requirement), "constraints": nativeArray(nativeString("约束")),
		"required_effects": nativeArray(nativeString("effect kind")), "required_artifacts": nativeArray(requirement),
		"requires_acceptance": map[string]any{"type": "boolean"},
		"success_evidence":    nativeArray(requirement),
	}, "execution_class", "deliverables")
}

func nativeObject(properties map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func nativeArray(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func nativeString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func nativeInteger(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
