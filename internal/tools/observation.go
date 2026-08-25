package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// ObservationGroup 提供 L2 ObservationDelta 的唯一模型写入口。它只接受
// 当前 Task/Attempt 已 settled 的证据引用，不读取或保存模型 reasoning。
type ObservationGroup struct {
	Store   store.TaskStore
	TaskMem *taskmem.Store
	Holder  TaskHolder
	AgentID string
	Checks  *checkstore.Store
}

func (g ObservationGroup) Register(r *agent.ToolRegistry) {
	if r == nil || g.Store == nil || g.TaskMem == nil || g.Holder == nil {
		return
	}
	factSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "有界的已确认工作事实，不得写 reasoning"},
			"evidence_refs": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "当前 Task/Attempt 的 tool-call:<call_id> 或 artifact:<path> 引用",
			},
		},
		"required": []any{"text", "evidence_refs"},
	}
	resolvedSchema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"candidate_ref": map[string]any{"type": "string", "description": "上一 Observation receipt 中仍开放的 candidate ref"},
			"evidence_refs": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "证明该候选已经解决的 settled evidence refs",
			},
		},
		"required": []any{"candidate_ref", "evidence_refs"},
	}
	params := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"phase": map[string]any{"type": "string", "enum": []any{
				taskmem.ObservationPhaseInvestigate, taskmem.ObservationPhaseImplement,
				taskmem.ObservationPhaseVerify, taskmem.ObservationPhaseFinalize,
				taskmem.ObservationPhaseBlocked,
			}, "description": "当前状态阶段；只有阶段前进、关闭旧候选、workspace revision 或 typed check 前进才算语义进展"},
			"facts": map[string]any{
				"type": "array", "items": factSchema,
				"description": "当前状态仍成立的 confirmed facts，最多 12 条；旧 Observation facts 会被整体替换",
			},
			"resolved_candidates": map[string]any{
				"type": "array", "items": resolvedSchema,
				"description": "本轮由 settled evidence 关闭的上一状态候选；不得用换措辞冒充进展",
			},
			"next_candidates": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "下一步候选，最多 5 条；候选不是已确认事实",
			},
		},
		"required": []any{"phase", "facts", "resolved_candidates", "next_candidates"},
	}
	r.Register("record_observation_delta",
		"冻结一次有 predecessor 的当前工作状态，供 Context 投影、Attempt rollover 与 Graph recovery 使用；关闭旧候选必须引用 settled evidence，只新增不同措辞不算语义进展；不得提交 chain-of-thought、工具参数或原始大正文",
		params, g.record)
}

func (g ObservationGroup) record(_ context.Context, args map[string]any) (string, error) {
	taskID := strings.TrimSpace(g.Holder.Get())
	if taskID == "" {
		return "", fmt.Errorf("record_observation_delta 缺少当前 Task")
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("读取 Observation Task: %w", err)
	}
	if task == nil || task.Status != model.TaskStatusProcessing || strings.TrimSpace(task.AttemptID) == "" {
		return "", fmt.Errorf("record_observation_delta 仅允许 processing Task/Attempt")
	}
	mem, err := g.TaskMem.Load(task.ID)
	if err != nil {
		return "", fmt.Errorf("读取 predecessor Observation: %w", err)
	}
	previousRef := ""
	var predecessorCreatedAt time.Time
	if mem != nil {
		previousRef = mem.LatestObservationDeltaRef
	}
	if previousRef != "" {
		previous, resolveErr := g.TaskMem.ResolveObservation(task.ID, previousRef)
		if resolveErr != nil {
			return "", fmt.Errorf("解析 predecessor Observation: %w", resolveErr)
		}
		predecessorCreatedAt = previous.CreatedAt
	}
	evidence, evidenceSettledAt, latestCheckRef, err := g.evidenceAuthority(task)
	if err != nil {
		return "", err
	}
	facts, err := parseObservationFacts(args["facts"], evidence)
	if err != nil {
		return "", err
	}
	next, err := parseBoundedStringArray(args["next_candidates"], taskmem.MaxObservationNext, "next_candidates")
	if err != nil {
		return "", err
	}
	phase, _ := args["phase"].(string)
	resolvedCandidates, err := parseResolvedObservationCandidates(args["resolved_candidates"], evidence,
		evidenceSettledAt, predecessorCreatedAt)
	if err != nil {
		return "", err
	}
	workspaceRef, _, err := checkstore.WorkspaceRevision(task, g.Store)
	if err != nil {
		return "", fmt.Errorf("冻结 Observation workspace revision: %w", err)
	}
	candidates := make([]taskmem.ObservationCandidate, 0, len(next))
	for _, text := range next {
		candidates = append(candidates, taskmem.NewObservationCandidate(task.ID, task.AttemptID, text))
	}
	delta := taskmem.ObservationDelta{
		Schema: taskmem.ObservationDeltaSchemaV2, TaskID: task.ID, AttemptID: task.AttemptID,
		PreviousRef: previousRef, Phase: strings.TrimSpace(phase), Facts: facts,
		ResolvedCandidates: resolvedCandidates, NextCandidates: candidates,
		WorkspaceRevisionRef: workspaceRef, LatestCheckRef: latestCheckRef, CreatedAt: time.Now().UTC(),
	}
	ref, err := g.TaskMem.RecordObservation(delta)
	if err != nil {
		return "", err
	}
	stored, err := g.TaskMem.ResolveObservation(task.ID, ref)
	if err != nil {
		return "", fmt.Errorf("回读 ObservationDelta receipt: %w", err)
	}
	trace.Emit(trace.Event{Kind: trace.KindObservationDeltaRecorded, TaskID: task.ID,
		RunID: string(task.RunID), AgentID: g.AgentID, AttemptID: task.AttemptID,
		Description: fmt.Sprintf(`{"observation_delta_ref":%q,"facts":%d,"resolved":%d,"next":%d,"semantic_advance":%t}`,
			ref, len(facts), len(resolvedCandidates), len(candidates), stored.SemanticAdvance)})
	openRefs := make([]string, 0, len(stored.NextCandidates))
	for _, candidate := range stored.NextCandidates {
		openRefs = append(openRefs, candidate.Ref)
	}
	payload, _ := json.Marshal(map[string]any{
		"schema": taskmem.ObservationDeltaSchemaV2, "observation_delta_ref": ref,
		"previous_ref": stored.PreviousRef, "phase": stored.Phase,
		"facts": len(facts), "resolved_candidates": len(resolvedCandidates),
		"open_candidate_refs": openRefs, "semantic_advance": stored.SemanticAdvance,
		"workspace_revision_ref": stored.WorkspaceRevisionRef, "latest_check_ref": stored.LatestCheckRef,
	})
	return string(payload), nil
}

func (g ObservationGroup) evidenceAuthority(task *model.Task) (map[string]taskmem.EvidenceRef,
	map[string]time.Time, string, error,
) {
	authority := make(map[string]taskmem.EvidenceRef)
	settledAt := make(map[string]time.Time)
	currentArtifacts := make(map[string]struct{})
	artifactSettledAt := make(map[string]time.Time)
	records, err := g.Store.QueryToolCalls(task.ID, "")
	if err != nil {
		return nil, nil, "", fmt.Errorf("读取 Observation 工具证据: %w", err)
	}
	latestCheckRef := ""
	var latestCheckAt time.Time
	for _, record := range records {
		if record.AttemptID != task.AttemptID {
			continue
		}
		callID := strings.TrimSpace(record.CallID)
		if callID == "" {
			continue
		}
		digestInput := record.ToolName + "\x00" + callID + "\x00" + fmt.Sprint(record.Success)
		sum := sha256.Sum256([]byte(digestInput))
		toolRef := "tool-call:" + callID
		authority[toolRef] = taskmem.EvidenceRef{
			Kind: taskmem.EvidenceToolResult, Ref: "tool-call:" + callID,
			Digest: hex.EncodeToString(sum[:]),
		}
		settledAt[toolRef] = record.Timestamp
		if record.Success && (record.ToolName == "write_file" || record.ToolName == "edit_file") {
			if path, _ := record.Args["path"].(string); strings.TrimSpace(path) != "" {
				path = strings.TrimSpace(path)
				currentArtifacts[path] = struct{}{}
				artifactSettledAt[path] = record.Timestamp
			}
		}
		if record.Success && record.ToolName == "run_check" && g.Checks != nil {
			checkID, _ := record.Args["check_id"].(string)
			if check, ok, checkErr := g.Checks.Latest(task.ID, task.AttemptID, checkID); checkErr == nil && ok {
				authority[check.CheckRef] = taskmem.EvidenceRef{Kind: taskmem.EvidenceCheck, Ref: check.CheckRef, Digest: check.CommandDigest}
				settledAt[check.CheckRef] = check.SettledAt
				if latestCheckAt.IsZero() || check.SettledAt.After(latestCheckAt) {
					latestCheckAt, latestCheckRef = check.SettledAt, check.CheckRef
				}
			}
		}
	}
	for _, path := range task.Artifacts {
		path = strings.TrimSpace(path)
		if _, writtenThisAttempt := currentArtifacts[path]; path != "" && writtenThisAttempt {
			ref := "artifact:" + path
			authority[ref] = taskmem.EvidenceRef{Kind: taskmem.EvidenceArtifact, Ref: ref}
			settledAt[ref] = artifactSettledAt[path]
		}
	}
	return authority, settledAt, latestCheckRef, nil
}

func parseResolvedObservationCandidates(raw any, authority map[string]taskmem.EvidenceRef,
	settledAt map[string]time.Time, predecessorCreatedAt time.Time,
) ([]taskmem.ResolvedObservationCandidate, error) {
	items, ok := raw.([]any)
	if !ok || len(items) > taskmem.MaxObservationNext {
		return nil, fmt.Errorf("resolved_candidates 必须是最多 %d 条的数组", taskmem.MaxObservationNext)
	}
	out := make([]taskmem.ResolvedObservationCandidate, 0, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resolved_candidates[%d] 必须是 object", i)
		}
		candidateRef, _ := object["candidate_ref"].(string)
		candidateRef = strings.TrimSpace(candidateRef)
		refs, err := parseBoundedStringArray(object["evidence_refs"], 8,
			fmt.Sprintf("resolved_candidates[%d].evidence_refs", i))
		if err != nil || candidateRef == "" || len(refs) == 0 {
			return nil, fmt.Errorf("resolved_candidates[%d] 缺少 candidate_ref/evidence_refs", i)
		}
		resolved := taskmem.ResolvedObservationCandidate{Ref: candidateRef}
		for _, ref := range refs {
			evidence, exists := authority[ref]
			if !exists {
				return nil, fmt.Errorf("resolved_candidates[%d] evidence_ref=%q 不属于当前 Task/Attempt settled authority", i, ref)
			}
			if predecessorCreatedAt.IsZero() || !settledAt[ref].After(predecessorCreatedAt) {
				return nil, fmt.Errorf("resolved_candidates[%d] evidence_ref=%q 不晚于 predecessor，不能证明候选已关闭", i, ref)
			}
			resolved.Evidence = append(resolved.Evidence, evidence)
		}
		out = append(out, resolved)
	}
	return out, nil
}

func parseObservationFacts(raw any, authority map[string]taskmem.EvidenceRef) ([]taskmem.ObservationFact, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) > taskmem.MaxObservationFacts {
		return nil, fmt.Errorf("record_observation_delta facts 必须是最多 %d 条的数组", taskmem.MaxObservationFacts)
	}
	out := make([]taskmem.ObservationFact, 0, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("facts[%d] 必须是 object", i)
		}
		text, _ := object["text"].(string)
		text = strings.TrimSpace(text)
		if text == "" || len([]rune(text)) > taskmem.MaxObservationTextRunes {
			return nil, fmt.Errorf("facts[%d].text 为空或过长", i)
		}
		refs, err := parseBoundedStringArray(object["evidence_refs"], 8, fmt.Sprintf("facts[%d].evidence_refs", i))
		if err != nil || len(refs) == 0 {
			return nil, fmt.Errorf("facts[%d] 缺少合法 evidence_refs", i)
		}
		fact := taskmem.ObservationFact{Text: text}
		for _, ref := range refs {
			resolved, ok := authority[ref]
			if !ok {
				return nil, fmt.Errorf("facts[%d] evidence_ref=%q 不属于当前 Task/Attempt settled authority", i, ref)
			}
			fact.Evidence = append(fact.Evidence, resolved)
		}
		out = append(out, fact)
	}
	return out, nil
}

func parseBoundedStringArray(raw any, limit int, field string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) > limit {
		return nil, fmt.Errorf("%s 必须是最多 %d 条的 string 数组", field, limit)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		value, ok := item.(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" || len([]rune(value)) > taskmem.MaxObservationTextRunes {
			return nil, fmt.Errorf("%s[%d] 为空或过长", field, i)
		}
		out = append(out, value)
	}
	return out, nil
}
