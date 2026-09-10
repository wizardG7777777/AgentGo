// Package terminaladapter 把已持久化 TaskOutcome 转为 agentTask 终态事实。
package terminaladapter

import (
	"agentgo/internal/graph"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type ResultResolver interface {
	ResolveTaskResult(context.Context, string) (json.RawMessage, error)
}
type EvidenceResolver interface {
	ResolveTaskEvidence(context.Context, string, []string) ([]graph.EvidenceEntry, error)
}
type Dependencies struct {
	Results  ResultResolver
	Evidence EvidenceResolver
}

func ToAgentTaskTerminal(ctx context.Context, record outcomestore.Record, deps Dependencies) (graph.AgentTaskTerminal, error) {
	if err := ctx.Err(); err != nil {
		return graph.AgentTaskTerminal{}, err
	}
	if err := outcomestore.ValidateRecord(record); err != nil {
		return graph.AgentTaskTerminal{}, err
	}
	v := record.Outcome
	if v.GraphID == "" || v.NodeID == "" || v.ActivationID == "" {
		return graph.AgentTaskTerminal{}, fmt.Errorf("非图结果不能提交 agentTask 终态")
	}
	result, err := resolveResult(ctx, v, deps.Results)
	if err != nil {
		return graph.AgentTaskTerminal{}, err
	}
	if deps.Evidence != nil && len(v.EvidenceRefs) > 0 {
		e, err := deps.Evidence.ResolveTaskEvidence(ctx, v.TaskID, v.EvidenceRefs)
		if err != nil {
			return graph.AgentTaskTerminal{}, err
		}
		if !evidenceExact(e, durableEvidence(v.EvidenceFacts)) {
			return graph.AgentTaskTerminal{}, fmt.Errorf("证据与终态权威不符")
		}
	}
	return graph.AgentTaskTerminal{TaskID: v.TaskID, AttemptID: v.AttemptID, OutcomeRef: record.OutcomeRef, Status: string(v.Status), Value: result, CandidateRef: v.CandidateRef, Evidence: durableEvidence(v.EvidenceFacts), EvidenceRefs: append([]string(nil), v.EvidenceRefs...), Error: strings.TrimSpace(v.Reason)}, nil
}

func durableEvidence(values []outcome.EvidenceFact) []graph.EvidenceEntry {
	out := make([]graph.EvidenceEntry, len(values))
	for i, value := range values {
		out[i] = graph.EvidenceEntry{
			Ref: value.Ref, Kind: value.Kind, Summary: value.Summary,
			CallID: value.CallID, ToolName: value.ToolName,
			Command: value.Command, CommandTruncated: value.CommandTruncated,
			Path: value.Path, PathTruncated: value.PathTruncated,
			ExitCodeScope:        value.ExitCodeScope,
			WorkspaceRevisionRef: value.WorkspaceRevisionRef,
			OutputRef:            value.OutputRef,
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
