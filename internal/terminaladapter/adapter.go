// Package terminaladapter 是 L4 TaskOutcome → L5 Graph TerminalFact 的唯一转换边界。
// 自由文本 Task.Error/Results 不能绕过本包直接推进新 Graph。
package terminaladapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
)

// ResultResolver 解引用 TaskOutcome.ResultRef。实现必须返回 JSON object bytes。
type ResultResolver interface {
	ResolveTaskResult(ctx context.Context, resultRef string) (json.RawMessage, error)
}

// EvidenceResolver 把稳定 EvidenceRef 转为 Graph Runtime 可验证的结构化证据。
type EvidenceResolver interface {
	ResolveTaskEvidence(ctx context.Context, taskID string, refs []string) ([]graph.EvidenceEntry, error)
}

type Dependencies struct {
	Results  ResultResolver
	Evidence EvidenceResolver
}

// ToTerminalFact 校验并转换 durable Record。OutcomeRef 会写入 Result 的保留字段
// `_task_outcome_ref`，让 Graph Result/Trace 可追溯原始终态事实；业务字段不能覆盖。
func ToTerminalFact(ctx context.Context, record outcomestore.Record, deps Dependencies) (graph.TerminalFact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.TerminalFact{}, err
	}
	if strings.TrimSpace(record.OutcomeRef) == "" {
		return graph.TerminalFact{}, fmt.Errorf("Terminal adapter 缺少 outcome_ref")
	}
	if err := outcomestore.ValidateRecord(record); err != nil {
		return graph.TerminalFact{}, fmt.Errorf("Terminal adapter 收到伪造或损坏的 outcome record: %w", err)
	}
	value := record.Outcome
	if err := value.Validate(); err != nil {
		return graph.TerminalFact{}, fmt.Errorf("Terminal adapter 收到无效 TaskOutcome: %w", err)
	}
	if value.GraphID == "" || value.NodeID == "" || value.ActivationID == "" {
		return graph.TerminalFact{}, fmt.Errorf("非 Graph TaskOutcome 不能转换为 TerminalFact")
	}
	status, err := graphStatus(value.Status)
	if err != nil {
		return graph.TerminalFact{}, err
	}
	result, err := resolveResult(ctx, value, deps.Results)
	if err != nil {
		return graph.TerminalFact{}, err
	}
	// 保留键先无条件删除，再按权威值写回；真实字段为空时也不能允许业务
	// Result 伪造 reason/ref/identity。
	for _, key := range []string{
		"status", "summary", "reason_code", "reason", "_task_outcome_ref",
		"_run_id", "_attempt_id", "_attempt_no", "_result_ref",
		"_checkpoint_ref", "_artifact_refs",
		"_checkpoint_state",
	} {
		delete(result, key)
	}
	// 这些键是 L4→L5 边界的权威终态镜像，必须覆盖业务 result 同名字段。
	result["status"] = string(value.Status)
	result["summary"] = value.Summary
	result["_task_outcome_ref"] = record.OutcomeRef
	result["_run_id"] = string(value.RunID)
	result["_attempt_id"] = value.AttemptID
	result["_attempt_no"] = value.AttemptNo
	if value.ReasonCode != "" {
		result["reason_code"] = value.ReasonCode
	}
	if value.Reason != "" {
		result["reason"] = value.Reason
	}
	if value.ResultRef != "" {
		result["_result_ref"] = value.ResultRef
	}
	if value.CheckpointRef != "" {
		result["_checkpoint_ref"] = value.CheckpointRef
	}
	if value.CheckpointState != "" {
		result["_checkpoint_state"] = value.CheckpointState
	}
	if len(value.ArtifactRefs) > 0 {
		result["_artifact_refs"] = append([]string(nil), value.ArtifactRefs...)
	}

	evidence := durableEvidence(value.EvidenceFacts)
	if deps.Evidence != nil && len(value.EvidenceRefs) > 0 {
		resolved, resolveErr := deps.Evidence.ResolveTaskEvidence(ctx, value.TaskID, append([]string(nil), value.EvidenceRefs...))
		if resolveErr != nil {
			return graph.TerminalFact{}, fmt.Errorf("核验 TaskOutcome evidence: %w", resolveErr)
		}
		if !evidenceExact(resolved, evidence) {
			return graph.TerminalFact{}, fmt.Errorf("EvidenceResolver 返回事实与 durable evidence_facts 不一致")
		}
	}
	return graph.TerminalFact{
		GraphID: value.GraphID, NodeID: value.NodeID, ActivationID: value.ActivationID,
		TaskID: value.TaskID, Status: status, Result: result,
		Evidence: append([]graph.EvidenceEntry(nil), evidence...),
	}, nil
}

func durableEvidence(values []outcome.EvidenceFact) []graph.EvidenceEntry {
	out := make([]graph.EvidenceEntry, len(values))
	for i, value := range values {
		out[i] = graph.EvidenceEntry{
			Ref: value.Ref, Kind: value.Kind, Summary: value.Summary,
			CallID: value.CallID, ToolName: value.ToolName,
			Command: value.Command, CommandTruncated: value.CommandTruncated,
			Path: value.Path, PathTruncated: value.PathTruncated,
		}
		if value.Success != nil {
			copy := *value.Success
			out[i].Success = &copy
		}
		if value.ExitCode != nil {
			copy := *value.ExitCode
			out[i].ExitCode = &copy
		}
	}
	return out
}

func evidenceExact(left, right []graph.EvidenceEntry) bool {
	if len(left) != len(right) {
		return false
	}
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftRaw) == string(rightRaw)
}

func resolveResult(ctx context.Context, value outcome.TaskOutcome, resolver ResultResolver) (map[string]any, error) {
	raw := append(json.RawMessage(nil), value.Result...)
	if len(raw) == 0 && value.ResultRef != "" {
		if resolver == nil {
			return nil, fmt.Errorf("TaskOutcome 只有 result_ref，但 ResultResolver 未注入")
		}
		resolved, err := resolver.ResolveTaskResult(ctx, value.ResultRef)
		if err != nil {
			return nil, fmt.Errorf("解引用 TaskOutcome result: %w", err)
		}
		raw = append(json.RawMessage(nil), resolved...)
	}
	result := make(map[string]any)
	if len(raw) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, fmt.Errorf("TaskOutcome result 不是 JSON object: %v", err)
	}
	return result, nil
}

func graphStatus(status outcome.Status) (graph.NodeStatus, error) {
	switch status {
	case outcome.StatusCompleted:
		return graph.NodeCompleted, nil
	case outcome.StatusFailed, outcome.StatusCancelled:
		return graph.NodeFailed, nil
	case outcome.StatusBlocked:
		return graph.NodeBlocked, nil
	default:
		return "", fmt.Errorf("TaskOutcome status=%q 无法转换为 Graph status", status)
	}
}
