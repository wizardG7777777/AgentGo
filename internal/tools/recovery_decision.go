package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/graph"
)

func (g PlanControlGroup) submitRecoveryDecision(ctx context.Context, args map[string]any) (string, error) {
	taskID := g.Holder.Get()
	task, err := g.Store.GetTask(taskID)
	if err != nil || task == nil || task.GraphControllerRole != string(graph.ControllerRoleLoopRecovery) {
		return "", fmt.Errorf("submit_recovery_decision 仅允许 loop_recovery controller: %v", err)
	}
	decision, _ := args["decision"].(string)
	summary, _ := args["summary"].(string)
	result := map[string]any{"decision": decision}
	switch decision {
	case "blocked":
		reason, _ := args["blocked_reason"].(string)
		if strings.TrimSpace(reason) == "" {
			return "", fmt.Errorf("decision=blocked 必须填写 blocked_reason")
		}
		result["blocked_reason"] = reason
	case "retry":
		if startErr := g.RecoveryAuthority.ValidateRecoveryRetryStart(task.GraphID, task.NodeID,
			task.ActivationID, time.Now().UTC()); startErr != nil {
			return "", startErr
		}
		dimensions, ok := stringSlice(args["changed_dimensions"])
		if !ok || len(dimensions) == 0 {
			return "", fmt.Errorf("decision=retry 必须填写 changed_dimensions 数组")
		}
		partial := graph.RecoveryDelta{ChangedDimensions: dimensions}
		partial.Strategy, _ = args["strategy"].(string)
		firstAction, ok := args["first_action"].(map[string]any)
		if !ok {
			return "", fmt.Errorf("decision=retry 必须填写 first_action object")
		}
		partial.FirstAction = &graph.RecoveryFirstAction{}
		partial.FirstAction.Tool, _ = firstAction["tool"].(string)
		partial.FirstAction.Path, _ = firstAction["path"].(string)
		if task.GraphRecoveryDeltaSchema == graph.RecoveryDeltaSchemaV4 {
			files := []string{partial.FirstAction.Path}
			if rawContract, supplied := args["evidence_contract"].(map[string]any); supplied {
				var ok bool
				files, ok = stringSlice(rawContract["files"])
				if !ok || len(files) == 0 {
					return "", fmt.Errorf("evidence_contract.files 必须是非空 string 数组")
				}
			}
			partial.EvidenceContract = &graph.RecoveryEvidenceContract{Files: files}
		}
		partial.ExpectedMilestone, _ = args["expected_milestone"].(string)
		bound, bindErr := g.RecoveryAuthority.BindRecoveryDeltaAuthority(task.GraphID, task.NodeID, task.ActivationID, partial)
		if bindErr != nil {
			return "", bindErr
		}
		encoded, _ := json.Marshal(bound)
		var value map[string]any
		if json.Unmarshal(encoded, &value) != nil {
			return "", fmt.Errorf("编码 recovery_delta 失败")
		}
		result["recovery_delta"] = value
	default:
		return "", fmt.Errorf("decision 只接受 retry/blocked")
	}
	return g.submitTaskResult(ctx, map[string]any{"summary": summary, "result": result})
}

func stringSlice(value any) ([]string, bool) {
	if values, ok := value.([]string); ok {
		out := make([]string, 0, len(values))
		for _, text := range values {
			if strings.TrimSpace(text) == "" {
				return nil, false
			}
			out = append(out, strings.TrimSpace(text))
		}
		return out, true
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, false
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out, true
}
