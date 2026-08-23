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

const progressDeliverableRequiredMarker = "[progress-deliverable-required]"

const agentDeliverablePhasePrompt = `<agent-phase name="deliverable-submit">
This is a mechanical terminal handoff. The only legal action in this invocation is one typed submit_task_result call.
All read, grep, list, shell, edit, web, messaging, and replan tools from earlier role instructions are unavailable now; do not emit their names even if they appeared earlier.
Use the authoritative task objective, upstream input, output contract, and TaskMemory already present to submit completed, failed, or blocked. Do not answer with text.
</agent-phase>`

type invocationToolPolicy struct {
	Registry *ToolRegistry
	Phase    string
	MaxCalls int
}

func invocationToolChoice(router ToolRouterSnapshot) invocation.ToolChoice {
	switch router.Phase {
	case "scheduler:draft-create", "scheduler:draft-configure", "scheduler:draft-validate",
		"scheduler:draft-commit", "scheduler:start":
		// DeepSeek thinking 同时拒绝 exact/required，但支持 auto + tools。
		// ToolRouter 已机械收窄为唯一工具，L3 response gate 另外
		// 强制本轮必须至少返回一个该工具调用，不允许正文逃逸。
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
	case "agent:deliverable-submit":
		// 真实 DeepSeek 会在 auto 下继续返回历史 grep/read 工具，
		// 而 exact + thinking 又被 API 400。该轮仅做结构化终态提交，
		// LLMExecutor 同时冻结 reasoning=none，因此可安全使用 exact submit。
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "submit_task_result"}
	case "scheduler:draft-edit", "scheduler:recovery":
		// 多工具阶段同样使用 auto wire，由 L3 response gate 强制
		// 至少一个授权工具调用，避免 thinking + required 被 provider 400。
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
	default:
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
	}
}

// mechanicalSingletonPhase 标识“逻辑上必须且只允许一个工具动作”的机械阶段。
// 这些阶段在 wire 上使用 auto + singleton ToolRouter。Provider 即使
// 忽略 parallel_tool_calls=false 并返回多个调用，L3 也只 dispatch 第一个，
// 后续 call_id 获得可重放的 skipped result，不会形成第二个副作用。
func mechanicalSingletonPhase(phase string) bool {
	_, ok := mechanicalSingletonTool(phase)
	return ok
}

func mechanicalSingletonTool(phase string) (string, bool) {
	switch phase {
	case "scheduler:draft-create":
		return "create_graph_draft", true
	case "scheduler:draft-configure":
		return "configure_simple_graph_draft", true
	case "scheduler:draft-validate":
		return "validate_current_graph_draft", true
	case "scheduler:draft-commit":
		return "commit_current_graph_draft", true
	case "scheduler:start":
		return "start_current_graph", true
	case "agent:deliverable-submit":
		return "submit_task_result", true
	default:
		return "", false
	}
}

func phaseRequiresToolCall(phase string) bool {
	return mechanicalSingletonPhase(phase) || phase == "scheduler:draft-edit" || phase == "scheduler:recovery"
}

func phaseDispatchesOnlyFirstTool(phase string) bool {
	return mechanicalSingletonPhase(phase) || phase == "scheduler:draft-edit" || phase == "scheduler:recovery"
}

func deriveInvocationToolPolicy(task *model.Task, history []HistoryEntry, full *ToolRegistry) invocationToolPolicy {
	policy := invocationToolPolicy{Registry: full, Phase: "default", MaxCalls: defaultToolCallsPerResponse}
	if task != nil && full != nil && task.GraphID != "" && historyRequiresDeliverableSubmit(history) {
		return invocationToolPolicy{
			Registry: full.Filtered([]string{"submit_task_result"}),
			Phase:    "agent:deliverable-submit",
			MaxCalls: defaultToolCallsPerResponse,
		}
	}
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
	// Authoring/recovery 逻辑上只 dispatch 一个控制面动作。
	// singleton 阶段允许 provider wire 返回有界 fan-out，由 L3 duplicate
	// fence 仅执行首个；final-report 只含读工具，允许有界串行批次；
	// 多工具 edit/recovery 含状态变更，同样只 dispatch 首个并为其余 call_id
	// 生成 skipped output，而不在 SSE 第二个 item 时拒绝整个响应。
	policy.MaxCalls = 1
	if phaseDispatchesOnlyFirstTool(phase) || phase == "scheduler:final-report" {
		policy.MaxCalls = defaultToolCallsPerResponse
	}
	return policy
}

func historyRequiresDeliverableSubmit(history []HistoryEntry) bool {
	for index := len(history) - 1; index >= 0; index-- {
		if strings.Contains(history[index].SystemNotice, progressDeliverableRequiredMarker) {
			return true
		}
	}
	return false
}

// deliverableHistoryProjection 只用于终态提交 Invocation。Raw History 始终
// 保持不变；本次 L2 投影剔除旧 assistant tool calls/results，避免 DeepSeek
// 在 exact submit 下仍继续产生已撤下的 read/grep 工具。TaskMemory、任务目标、
// 上游输入与 output contract 由各自的 L2 fragment 独立注入，不依赖这些旧轮次。
func deliverableHistoryProjection(history []HistoryEntry) []HistoryEntry {
	projected := make([]HistoryEntry, 0, len(history))
	for _, entry := range history {
		if notice := strings.TrimSpace(entry.SystemNotice); notice != "" {
			projected = append(projected, HistoryEntry{SystemNotice: notice})
		}
	}
	return projected
}

// unsuccessfulToolResult 兼容历史已持久的半角/全角中文冒号。
// skipped call 只是为 provider call_id 补齐 replay output，不得驱动阶段迁移。
func unsuccessfulToolResult(content string) bool {
	return content == "" || strings.HasPrefix(content, "错误:") ||
		strings.HasPrefix(content, "已跳过:") || strings.HasPrefix(content, "已跳过：")
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
			if !ok || unsuccessfulToolResult(content) {
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
	if phaseRequiresToolCall(router.Phase) && len(calls) == 0 {
		return fmt.Errorf("auto tool phase=%s 未返回必需的工具调用", router.Phase)
	}
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
