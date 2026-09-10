package tools

import (
	"agentgo/internal/model"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

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

func graphNodeNativeSchema() map[string]any {
	source := nativeObject(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"node_result", "node_outcome", "graph_input"}}, "node_id": nativeString("node_result 来源实例"), "port": nativeString("graph_input 输入端口"), "version": nativeInteger("图输入的明确版本"), "pointer": nativeString("可选 JSON Pointer，例如 /summary")}, "kind")
	return nativeObject(map[string]any{
		"node_id": nativeString("新的任务实例 ID；返工追加新 ID"), "kind": map[string]any{"type": "string", "enum": []string{"agentTask"}},
		"title": nativeString("简短任务标题"), "objective": nativeString("确定任务和应交付的结果"),
		"inputs":          map[string]any{"type": "object", "additionalProperties": source},
		"result_schema":   map[string]any{"type": "object", "description": "JSON Schema 子集(type/properties/required/additionalProperties/items/enum)，根类型必须 object；通常要求 summary string"},
		"execution":       nativeObject(map[string]any{"route_ref": nativeString("能力目录中的 route_ref，例如 default；不要填写 Agent 名称"), "tools": nativeArray(nativeString("目录明确授予的工具")), "model": nativeString("可选显式模型")}, "route_ref", "tools"),
		"workspace_input": nativeString("提供工作候选基线的输入槽；多候选时必须指定"),
		"labels":          map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	}, "node_id", "kind", "title", "objective", "result_schema", "execution")
}

func nativeObject(properties map[string]any, required ...string) map[string]any {
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		result["required"] = required
	}
	return result
}
func nativeString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func nativeInteger(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
func nativeArray(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}
