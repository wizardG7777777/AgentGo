package bootstrap

import (
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/outcome"
	"fmt"
	"sort"
	"strings"
)

func taskOutcomeStatus(status model.TaskStatus) (outcome.Status, error) {
	switch status {
	case model.TaskStatusCompleted:
		return outcome.StatusCompleted, nil
	case model.TaskStatusFailed:
		return outcome.StatusFailed, nil
	case model.TaskStatusBlocked:
		return outcome.StatusBlocked, nil
	case model.TaskStatusCancelled:
		return outcome.StatusCancelled, nil
	default:
		return "", fmt.Errorf("Task status=%s 不是终态", status)
	}
}

func modelStatusFromOutcome(status outcome.Status) model.TaskStatus {
	return model.TaskStatus(status)
}

func taskOutcomeSummary(task *model.Task) string {
	if task == nil {
		return "Task terminal"
	}
	if text := strings.TrimSpace(task.LastResponse); text != "" {
		return text
	}
	if text := strings.TrimSpace(task.Error); text != "" {
		return text
	}
	keys := make([]string, 0, len(task.Results))
	for key := range task.Results {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if text := strings.TrimSpace(task.Results[key]); text != "" {
			return text
		}
	}
	if text := strings.TrimSpace(task.Description); text != "" {
		return text
	}
	return "Task terminal status=" + string(task.Status)
}

func outcomeEvidenceFact(entry graph.EvidenceEntry) outcome.EvidenceFact {
	fact := outcome.EvidenceFact{
		Ref: entry.Ref, Kind: entry.Kind, Summary: entry.Summary,
		CallID: entry.CallID, ToolName: entry.ToolName,
		Command: entry.Command, CommandTruncated: entry.CommandTruncated,
		Path: entry.Path, PathTruncated: entry.PathTruncated,
		WorkspaceRevisionRef: entry.WorkspaceRevisionRef,
		OutputRef:            entry.OutputRef,
	}
	if entry.Success != nil {
		value := *entry.Success
		fact.Success = &value
	}
	if entry.ExitCode != nil {
		value := *entry.ExitCode
		fact.ExitCode = &value
	}
	fact.ExitCodeScope = entry.ExitCodeScope
	return fact
}

func cloneTaskResults(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// replayPendingTaskOutcomes 在 dispatcher/Runner 激活前消费 durable delivery
// outbox。Session 模式的历史图按“进入会话不自动续跑”保持 pending；无
// Session 模式正常重放。非 Graph outcome 在 Task projection 成功后直接 ack。
