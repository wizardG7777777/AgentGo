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
	Offset    int                      `json:"offset,omitempty"`
	Limit     int                      `json:"limit,omitempty"`
	EditSteps []graph.RecoveryEditStep `json:"edit_steps,omitempty"`
	Reason    string                   `json:"reason,omitempty"`
	Summary   string                   `json:"summary"`
}

func (g PlanControlGroup) submitChangeDecision(ctx context.Context, args map[string]any) (string, error) {
	taskID := strings.TrimSpace(g.Holder.Get())
	task, err := g.Store.GetTask(taskID)
	if err != nil || task == nil || task.Status != model.TaskStatusProcessing ||
		task.GraphNodeKind != string(graph.KindAgent) {
		return "", fmt.Errorf("submit_change_decision 仅允许 RecoveryDelta v4/v5 的 processing work Task: %v", err)
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
	recoverySchema := recoverySchemaForTask(task)
	receiptSchema := graph.ChangeDecisionSchemaV1
	if recoverySchema == graph.RecoveryDeltaSchemaV5 {
		receiptSchema = graph.ChangeDecisionSchemaV2
	}
	receipt := changeDecisionReceipt{Schema: receiptSchema, Decision: decision, Summary: summary}
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
		if recoverySchema == graph.RecoveryDeltaSchemaV5 {
			offset, limit := intArg(args, "offset"), intArg(args, "limit")
			if offset <= 0 {
				offset = 1
			}
			if limit <= 0 {
				limit = 240
			}
			if limit != 240 {
				return "", fmt.Errorf("RecoveryDelta v5 focus limit 必须等于冻结页大小 240")
			}
			pages, pageErr := recoveryFocusPagesForTask(g.Store, task)
			if pageErr != nil {
				return "", pageErr
			}
			key := fmt.Sprintf("%s:%d:%d", path, offset, limit)
			if _, exists := pages[key]; exists {
				return "", fmt.Errorf("decision=need_context focus page=%q 已声明；同一路径继续读取必须填写新的 offset", key)
			}
			if len(pages) >= graph.MaxRecoveryEvidenceFiles {
				return "", fmt.Errorf("v5 focus page 数已达上限 %d，不能继续扩展", graph.MaxRecoveryEvidenceFiles)
			}
			receipt.Path, receipt.Offset, receipt.Limit, receipt.Reason = path, offset, limit, reason
			break
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
			if recoverySchema == graph.RecoveryDeltaSchemaV5 && steps[index].Tool == "edit_file" {
				if _, visible := files[path]; !visible {
					return "", fmt.Errorf("edit_steps[%d] path=%q 尚未进入 v5 focus context；先用 decision=need_context 声明并读取该文件", index, path)
				}
			}
		}
		receipt.EditSteps = steps
	case "resume_candidate":
		if recoverySchema != graph.RecoveryDeltaSchemaV5 {
			return "", fmt.Errorf("decision=resume_candidate 只允许 RecoveryDelta v5")
		}
		state, stateErr := recoveryCandidateStateForTask(task)
		if stateErr != nil || state == nil || len(state.DirtyPaths) == 0 {
			return "", fmt.Errorf("decision=resume_candidate 缺少 Runtime 绑定的非空 dirty candidate: %v", stateErr)
		}
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
		return "", fmt.Errorf("change decision 只接受 edit/resume_candidate/need_context/hypothesis_rejected/blocked")
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
			wrapper.TargetInput != "recovery_directive" ||
			(wrapper.Result.Schema != graph.RecoveryDeltaSchemaV4 && wrapper.Result.Schema != graph.RecoveryDeltaSchemaV5) ||
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

func recoveryFocusPagesForTask(taskStore interface {
	QueryToolCalls(string, string) ([]store.ToolCallRecord, error)
}, task *model.Task) (map[string]struct{}, error) {
	pages := make(map[string]struct{})
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
			wrapper.TargetInput != "recovery_directive" || wrapper.Result.Schema != graph.RecoveryDeltaSchemaV5 ||
			wrapper.Result.FirstAction == nil {
			continue
		}
		path, err := graph.CanonicalRecoveryEvidencePath(wrapper.Result.FirstAction.Path)
		if err != nil {
			return nil, err
		}
		pages[fmt.Sprintf("%s:%d:%d", path, 1, 240)] = struct{}{}
	}
	records, err := taskStore.QueryToolCalls(task.ID, "")
	if err != nil {
		return nil, fmt.Errorf("读取 v5 focus page 账本: %w", err)
	}
	for _, record := range records {
		if record.AttemptID != task.AttemptID || !record.Success || record.ToolName != "submit_change_decision" ||
			strings.TrimSpace(fmt.Sprint(record.Args["decision"])) != "need_context" {
			continue
		}
		path, pathErr := graph.CanonicalRecoveryEvidencePath(fmt.Sprint(record.Args["path"]))
		if pathErr != nil {
			continue
		}
		offset, limit := intArg(record.Args, "offset"), intArg(record.Args, "limit")
		if offset <= 0 {
			offset = 1
		}
		if limit <= 0 {
			limit = 240
		}
		pages[fmt.Sprintf("%s:%d:%d", path, offset, limit)] = struct{}{}
	}
	return pages, nil
}

func recoveryCandidateStateForTask(task *model.Task) (*graph.RecoveryCandidateState, error) {
	if task == nil {
		return nil, fmt.Errorf("task 为空")
	}
	var found *graph.RecoveryCandidateState
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
			wrapper.TargetInput != "recovery_directive" || wrapper.Result.Schema != graph.RecoveryDeltaSchemaV5 ||
			wrapper.Result.CandidateState == nil {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("存在多份 recovery candidate state")
		}
		copy := *wrapper.Result.CandidateState
		copy.DirtyPaths = append([]string(nil), wrapper.Result.CandidateState.DirtyPaths...)
		if wrapper.Result.CandidateState.LatestCheck != nil {
			check := *wrapper.Result.CandidateState.LatestCheck
			copy.LatestCheck = &check
		}
		found = &copy
	}
	if found == nil {
		return nil, fmt.Errorf("未找到 RecoveryDelta v5 candidate_state")
	}
	return found, nil
}

func recoverySchemaForTask(task *model.Task) string {
	if task == nil {
		return ""
	}
	if schema := strings.TrimSpace(task.GraphRecoveryDeltaSchema); schema != "" {
		return schema
	}
	for _, input := range task.ContextInputs {
		start, end := strings.IndexByte(input.Content, '{'), strings.LastIndexByte(input.Content, '}')
		if start < 0 || end < start {
			continue
		}
		var wrapper struct {
			TargetInput string              `json:"target_input"`
			Result      graph.RecoveryDelta `json:"result"`
		}
		if json.Unmarshal([]byte(input.Content[start:end+1]), &wrapper) == nil &&
			wrapper.TargetInput == "recovery_directive" {
			return strings.TrimSpace(wrapper.Result.Schema)
		}
	}
	return ""
}
