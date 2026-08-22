package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
)

const defaultToolCallsPerResponse = 16

type invocationToolPolicy struct {
	Registry *ToolRegistry
	Phase    string
	MaxCalls int
}

func invocationToolChoice(router ToolRouterSnapshot) invocation.ToolChoice {
	switch router.Phase {
	case "scheduler:draft-create":
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "create_graph_draft"}
	case "scheduler:draft-configure":
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "configure_simple_graph_draft"}
	case "scheduler:draft-validate":
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "validate_current_graph_draft"}
	case "scheduler:draft-commit":
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "commit_current_graph_draft"}
	case "scheduler:start":
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "start_current_graph"}
	case "scheduler:draft-edit", "scheduler:recovery":
		return invocation.ToolChoice{Mode: invocation.ToolChoiceRequired}
	default:
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
	}
}

func deriveInvocationToolPolicy(task *model.Task, history []HistoryEntry, full *ToolRegistry) invocationToolPolicy {
	policy := invocationToolPolicy{Registry: full, Phase: "default", MaxCalls: defaultToolCallsPerResponse}
	if task == nil || full == nil || task.GraphID != "" || task.EventType != "__scheduler__" ||
		task.RunContract == nil || strings.TrimSpace(task.ContextPolicyRef) == "" {
		return policy
	}
	allow := []string{"create_graph_draft"}
	phase := "scheduler:draft-create"
	switch {
	case task.RunPhase == runcontract.PhaseFinalization || task.EventSource == "graph-ended" ||
		strings.HasPrefix(strings.TrimSpace(task.Description), "[graph-ended:"):
		phase = "scheduler:final-report"
		allow = []string{"read_graph", "get_task_result", "read_content_ref"}
	case task.RunPhase == runcontract.PhaseRecovery && task.EventSource == model.TaskEventSourceLoopIntervention && task.ParentTaskID != "":
		if strings.TrimSpace(task.InterventionGraphID) != "" {
			phase = "scheduler:recovery"
			allow = []string{
				"read_graph", "get_task_result", "read_content_ref",
				"propose_graph_change", "read_graph_change", "validate_graph_change", "commit_graph_change",
			}
		} else {
			phase, allow = graphAuthoringPolicy(history)
		}
	case task.RunPhase == runcontract.PhaseRecovery:
		phase = "scheduler:recovery"
		allow = []string{
			"read_graph", "get_task_result", "read_content_ref",
			"propose_graph_change", "read_graph_change", "validate_graph_change", "commit_graph_change",
		}
	default:
		phase, allow = graphAuthoringPolicy(history)
	}
	policy.Registry = full.Filtered(allow)
	policy.Phase = phase
	// Authoring/recovery/final-report 均采用一次响应一个控制面动作；模型根据
	// durable tool result 进入下一 phase，避免同批副作用与 revision CAS 竞态。
	policy.MaxCalls = 1
	return policy
}

type graphAuthoringStage string

const (
	graphAuthoringCreate    graphAuthoringStage = "create"
	graphAuthoringConfigure graphAuthoringStage = "configure"
	graphAuthoringValidate  graphAuthoringStage = "validate"
	graphAuthoringEdit      graphAuthoringStage = "edit"
	graphAuthoringCommit    graphAuthoringStage = "commit"
	graphAuthoringStart     graphAuthoringStage = "start"
)

// graphAuthoringPolicy 只按按序成功的 authoring receipt 推进状态机。read 不改变
// 阶段；失败 tool result 不生效；validate 只有 accepted=true 才能进入 commit。
// 这避免旧实现用“历史上调用过某工具”猜测当前 Draft authority。
func graphAuthoringPolicy(history []HistoryEntry) (string, []string) {
	stage := graphAuthoringCreate
	simpleTemplate := false
	for _, entry := range history {
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for _, call := range entry.ToolCalls {
			content, ok := results[call.ID]
			if !ok || content == "" || strings.HasPrefix(content, "错误:") || strings.HasPrefix(content, "已跳过:") {
				continue
			}
			switch call.Name {
			case "create_graph_draft":
				stage = graphAuthoringConfigure
			case "configure_simple_graph_draft":
				simpleTemplate = true
				stage = graphAuthoringValidate
			case "patch_graph_draft":
				simpleTemplate = false
				stage = graphAuthoringValidate
			case "validate_graph_draft", "validate_current_graph_draft":
				var report struct {
					Accepted bool `json:"accepted"`
				}
				if json.Unmarshal([]byte(content), &report) == nil && report.Accepted {
					stage = graphAuthoringCommit
				} else if simpleTemplate {
					stage = graphAuthoringConfigure
				} else {
					stage = graphAuthoringEdit
				}
			case "commit_graph_draft", "commit_current_graph_draft":
				stage = graphAuthoringStart
			}
		}
	}
	switch stage {
	case graphAuthoringConfigure:
		return "scheduler:draft-configure", []string{"configure_simple_graph_draft"}
	case graphAuthoringValidate:
		return "scheduler:draft-validate", []string{"validate_current_graph_draft"}
	case graphAuthoringEdit:
		return "scheduler:draft-edit", []string{"read_graph_draft", "patch_graph_draft", "validate_graph_draft"}
	case graphAuthoringCommit:
		return "scheduler:draft-commit", []string{"commit_current_graph_draft"}
	case graphAuthoringStart:
		return "scheduler:start", []string{"start_current_graph"}
	default:
		return "scheduler:draft-create", []string{"create_graph_draft"}
	}
}

func validateToolCallBatch(router ToolRouterSnapshot, calls []llm.ToolCall) error {
	if len(calls) > router.MaxCalls {
		return fmt.Errorf("tool call batch 数量 %d 超过 phase=%s 上限 %d", len(calls), router.Phase, router.MaxCalls)
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
		names = append(names, call.Name)
	}
	if missing := router.Registry.Missing(names); len(missing) > 0 {
		return fmt.Errorf("tool call batch 含 phase=%s 未授权工具 %v", router.Phase, missing)
	}
	return nil
}
