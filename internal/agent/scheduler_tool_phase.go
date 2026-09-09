package agent

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"errors"
	"fmt"
	"strings"
)

const defaultToolCallsPerResponse = 16

// 工具目录按角色/权限提供，不按经验轮数或模型报告切换机械阶段。
type invocationToolPolicy struct {
	Registry *ToolRegistry
	Phase    string
	MaxCalls int
}

func deriveInvocationToolPolicy(task *model.Task, _ []contextcontract.HistoryEntry, full *ToolRegistry) invocationToolPolicy {
	policy := invocationToolPolicy{Registry: full, Phase: "default", MaxCalls: defaultToolCallsPerResponse}
	if task != nil && task.EventType == "__scheduler__" && task.GraphID != "" && task.GraphNodeKind == "agent" {
		policy.Phase = "agent:execution"
		return policy
	}
	if task == nil || task.EventType != "__scheduler__" || task.GraphID != "" {
		return policy
	}
	names := []string{"read_graph_definition", "apply_graph_change", "control_graph", "request_replan", "inspect_board", "inspect_node", "read_evidence", "send_message", "request_user_input", "list_agent_templates", "provision_agent_team"}
	policy.Phase = "scheduler:authoring"
	if task.FinalReportGraphID != "" {
		policy.Phase = "scheduler:final-report"
		names = []string{"inspect_board", "inspect_node", "read_evidence", "read_graph_definition", "submit_task_result"}
	}
	if task.InterventionGraphID != "" {
		policy.Phase = "scheduler:coordination"
		names = append(names, "submit_task_result")
	}
	if full != nil {
		policy.Registry = full.Filtered(names)
	}
	return policy
}
func phaseRequiresToolCall(phase string) bool { return strings.HasPrefix(phase, "scheduler:") }
func skippedToolResult(content string) bool {
	return strings.HasPrefix(content, "已跳过:") || strings.HasPrefix(content, "已跳过：")
}
func unsuccessfulToolResult(content string) bool {
	return content == "" || strings.HasPrefix(content, "错误:") || strings.HasPrefix(content, "错误：") || skippedToolResult(content)
}

func validateToolCallBatch(router ToolRouterSnapshot, calls []llm.ToolCall) error {
	if phaseRequiresToolCall(router.Phase) && len(calls) == 0 {
		return &actionContractViolation{detail: fmt.Sprintf("auto tool phase=%s 未返回必需的工具调用", router.Phase)}
	}
	if len(calls) > router.MaxCalls {
		return &actionContractViolation{detail: fmt.Sprintf("tool call batch 数量 %d 超过 phase=%s 上限 %d", len(calls), router.Phase, router.MaxCalls)}
	}
	if len(calls) == 0 {
		return nil
	}
	names := make([]string, 0, len(calls))
	seenIDs := make(map[string]struct{}, len(calls))
	for i, call := range calls {
		if !store.IsWellFormedToolName(call.Name) {
			return fmt.Errorf("tool_calls[%d].name 不是合法注册工具名", i)
		}
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("tool_calls[%d] 缺少 call_id", i)
		}
		if _, duplicate := seenIDs[call.ID]; duplicate {
			return fmt.Errorf("tool_calls[%d] 重复 call_id", i)
		}
		seenIDs[call.ID] = struct{}{}
		// 每个实际调用都必须属于同一个冻结工具视图。
		names = append(names, call.Name)
	}
	if missing := router.Registry.Missing(names); len(missing) > 0 {
		return &actionContractViolation{detail: fmt.Sprintf("tool call batch 含 phase=%s 未授权工具 %v", router.Phase, missing)}
	}
	return nil
}

type actionContractViolation struct{ detail string }

func (e *actionContractViolation) Error() string { return e.detail }

func isActionContractViolation(err error) bool {
	var target *actionContractViolation
	return errors.As(err, &target)
}
