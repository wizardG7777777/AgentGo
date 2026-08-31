package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

type changeDecisionReceipt struct {
	Schema    string                   `json:"schema"`
	Decision  string                   `json:"decision"`
	Path      string                   `json:"path,omitempty"`
	EditSteps []graph.RecoveryEditStep `json:"edit_steps,omitempty"`
	Reason    string                   `json:"reason,omitempty"`
	Summary   string                   `json:"summary"`
}

func (g PlanControlGroup) submitChangeDecision(ctx context.Context, args map[string]any) (string, error) {
	taskID := strings.TrimSpace(g.Holder.Get())
	task, err := g.Store.GetTask(taskID)
	if err != nil || task == nil || task.Status != model.TaskStatusProcessing ||
		task.GraphNodeKind != string(graph.KindAgent) {
		return "", fmt.Errorf("submit_change_decision 仅允许 RecoveryDelta v4 的 processing work Task: %v", err)
	}
	files, err := recoveryEvidenceFilesForTask(g.Store, task)
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("submit_change_decision 缺少 RecoveryDelta v4 EvidenceContract: %v", err)
	}
	decision, _ := args["decision"].(string)
	summary, _ := args["summary"].(string)
	decision, summary = strings.TrimSpace(decision), strings.TrimSpace(summary)
	if summary == "" {
		return "", fmt.Errorf("submit_change_decision 缺少 summary")
	}
	receipt := changeDecisionReceipt{Schema: graph.ChangeDecisionSchemaV1, Decision: decision, Summary: summary}
	switch decision {
	case "need_context":
		path, pathErr := graph.CanonicalRecoveryEvidencePath(fmt.Sprint(args["path"]))
		if pathErr != nil {
			return "", fmt.Errorf("need_context path: %w", pathErr)
		}
		reason := strings.TrimSpace(fmt.Sprint(args["reason"]))
		if reason == "" {
			return "", fmt.Errorf("decision=need_context 必须填写 reason")
		}
		if _, exists := files[path]; exists {
			return "", fmt.Errorf("decision=need_context path=%q 已在 EvidenceContract 中，不得重复扩展", path)
		}
		if len(files) >= graph.MaxRecoveryEvidenceFiles {
			return "", fmt.Errorf("EvidenceContract 文件数已达上限 %d，不能继续扩展", graph.MaxRecoveryEvidenceFiles)
		}
		receipt.Path, receipt.Reason = path, reason
	case "edit":
		steps, ok := recoveryEditSteps(args["edit_steps"])
		if !ok || len(steps) == 0 || len(steps) > graph.MaxRecoveryEditSteps {
			return "", fmt.Errorf("decision=edit 的 edit_steps 必须有 1..%d 项", graph.MaxRecoveryEditSteps)
		}
		for index := range steps {
			if steps[index].Tool != "edit_file" && steps[index].Tool != "write_file" {
				return "", fmt.Errorf("edit_steps[%d].tool=%q 只允许 edit_file/write_file", index, steps[index].Tool)
			}
			path, pathErr := graph.CanonicalRecoveryEvidencePath(steps[index].Path)
			if pathErr != nil {
				return "", fmt.Errorf("edit_steps[%d]: %w", index, pathErr)
			}
			steps[index].Path = path
		}
		receipt.EditSteps = steps
	case "hypothesis_rejected", "blocked":
		reason := strings.TrimSpace(fmt.Sprint(args["reason"]))
		if reason == "" {
			return "", fmt.Errorf("decision=%s 必须填写 reason", decision)
		}
		result := map[string]any{"change_decision": decision}
		return g.submitTaskResult(ctx, map[string]any{
			"summary": summary, "status": "blocked", "blocked_reason": reason,
			"result": result,
		})
	default:
		return "", fmt.Errorf("change decision 只接受 edit/need_context/hypothesis_rejected/blocked")
	}
	encoded, _ := json.Marshal(receipt)
	return string(encoded), nil
}

func recoveryEditSteps(value any) ([]graph.RecoveryEditStep, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	steps := make([]graph.RecoveryEditStep, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		tool, toolOK := object["tool"].(string)
		path, pathOK := object["path"].(string)
		if !toolOK || !pathOK {
			return nil, false
		}
		steps = append(steps, graph.RecoveryEditStep{Tool: strings.TrimSpace(tool), Path: path})
	}
	return steps, true
}

func recoveryEvidenceFilesForTask(taskStore interface {
	QueryToolCalls(string, string) ([]store.ToolCallRecord, error)
}, task *model.Task) (map[string]struct{}, error) {
	files := make(map[string]struct{})
	for _, input := range task.ContextInputs {
		start, end := strings.IndexByte(input.Content, '{'), strings.LastIndexByte(input.Content, '}')
		if start < 0 || end < start {
			continue
		}
		var wrapper struct {
			TargetInput string              `json:"target_input"`
			Result      graph.RecoveryDelta `json:"result"`
		}
		if json.Unmarshal([]byte(input.Content[start:end+1]), &wrapper) != nil ||
			wrapper.TargetInput != "recovery_directive" || wrapper.Result.Schema != graph.RecoveryDeltaSchemaV4 ||
			wrapper.Result.EvidenceContract == nil {
			continue
		}
		for _, raw := range wrapper.Result.EvidenceContract.Files {
			path, err := graph.CanonicalRecoveryEvidencePath(raw)
			if err != nil {
				return nil, err
			}
			files[path] = struct{}{}
		}
	}
	records, err := taskStore.QueryToolCalls(task.ID, "")
	if err != nil {
		return nil, fmt.Errorf("读取 change decision 账本: %w", err)
	}
	for _, record := range records {
		if record.AttemptID != task.AttemptID || !record.Success || record.ToolName != "submit_change_decision" ||
			strings.TrimSpace(fmt.Sprint(record.Args["decision"])) != "need_context" {
			continue
		}
		path, pathErr := graph.CanonicalRecoveryEvidencePath(fmt.Sprint(record.Args["path"]))
		if pathErr == nil {
			files[path] = struct{}{}
		}
	}
	return files, nil
}
