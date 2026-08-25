package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
)

const defaultToolCallsPerResponse = 16

const progressDeliverableRequiredMarker = "[progress-deliverable-required]"
const observationCheckpointRequiredMarker = "[observation-checkpoint-required"
const observationCheckpointAbandonedMarker = "[observation-checkpoint-abandoned]"
const observationCheckpointFailureMarker = "[observation-checkpoint-failure]"

const agentDeliverablePhasePrompt = `<agent-phase name="deliverable-submit">
This is a mechanical terminal handoff. The only legal action in this invocation is one typed submit_task_result call.
All read, grep, list, shell, edit, web, messaging, and replan tools from earlier role instructions are unavailable now; do not emit their names even if they appeared earlier.
Use the authoritative task objective, upstream input, output contract, and TaskMemory already present to submit completed, failed, or blocked. Do not answer with text.
</agent-phase>`

const observationCheckpointPhasePrompt = `<agent-phase name="observation-checkpoint">
This is a mechanical L2 checkpoint before Context projection, Attempt rollover, or intervention.
The only legal action is one record_observation_delta call. Phase must be investigate, implement, verify, finalize, or blocked. Facts are the complete currently valid fact projection, not an append-only recap. Close predecessor candidates with their candidate_ref and settled evidence; merely rewording or adding candidates is not progress. Record next candidates as open questions/actions. Never include chain-of-thought or raw tool bodies. Do not edit, test, message, submit the task result, or answer with text.
</agent-phase>`

const finalReportSubmitPhasePrompt = `<scheduler-phase name="final-report-submit">
This is the terminal user delivery. The only legal action is one report_done call using the frozen GraphTerminalSummary and any already-read TaskOutcome facts. Do not read more data, create or change a Graph, rerun business work, or answer with text.
</scheduler-phase>`

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
	case "agent:deliverable-submit", "scheduler:final-report-submit":
		// 终态交付后不会再回到 thinking 业务轮，因此可以使用
		// reasoning=none + exact 的狭义例外。
		name, _ := mechanicalSingletonTool(router.Phase)
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: name}
	case "agent:observation-checkpoint":
		// Observation 使用独立 Control Invocation lane：本轮只消费冻结
		// TaskMemory/evidence catalog，不进入业务 reasoning replay。因而可以
		// 与终态交付一样使用 reasoning=none + exact typed action，而不关闭
		// 下一业务轮的 thinking。
		return invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "record_observation_delta"}
	case "scheduler:draft-edit", "scheduler:recovery":
		// 多工具阶段同样使用 auto wire，由 L3 response gate 强制
		// 至少一个授权工具调用，避免 thinking + required 被 provider 400。
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
	default:
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
	}
}

func phaseReasoningEffortOverride(phase string) (string, bool) {
	switch phase {
	case "agent:deliverable-submit", "agent:observation-checkpoint", "scheduler:final-report-submit":
		return "none", true
	default:
		return "", false
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
	case "agent:observation-checkpoint":
		return "record_observation_delta", true
	case "scheduler:final-report-submit":
		return "report_done", true
	default:
		return "", false
	}
}

func phaseRequiresToolCall(phase string) bool {
	return mechanicalSingletonPhase(phase) || phase == "scheduler:draft-edit" ||
		phase == "scheduler:recovery" || phase == "scheduler:graph-recovery" ||
		phase == "scheduler:final-report"
}

func phaseDispatchesOnlyFirstTool(phase string) bool {
	return mechanicalSingletonPhase(phase) || phase == "scheduler:draft-edit" ||
		phase == "scheduler:recovery" || phase == "scheduler:graph-recovery"
}

func deriveInvocationToolPolicy(task *model.Task, history []HistoryEntry, full *ToolRegistry) invocationToolPolicy {
	return deriveInvocationToolPolicyWithControl(task, history, full, full)
}

// deriveInvocationToolPolicyWithControl 把任务级业务视图与 framework-owned
// Control authority 分开。普通阶段只使用 business；Observation exact
// Invocation 只从 frameworkControl 取 record_observation_delta。这样
// Acceptance/readonly 的普通 Lease 不会看到控制工具，同时周期性 checkpoint
// 不会因角色闭集裁剪而变成不可达状态。
func deriveInvocationToolPolicyWithControl(task *model.Task, history []HistoryEntry,
	business, frameworkControl *ToolRegistry) invocationToolPolicy {
	full := business
	policy := invocationToolPolicy{Registry: full, Phase: "default", MaxCalls: defaultToolCallsPerResponse}
	if task != nil && taskUsesObservationCheckpoint(task) && historyRequiresObservationCheckpoint(history) {
		control := frameworkControl
		if control == nil {
			control = business
		}
		var view *ToolRegistry
		if control != nil {
			view = observationCheckpointRegistry(control.Filtered([]string{"record_observation_delta"}), task, history)
		}
		return invocationToolPolicy{Registry: view,
			Phase: "agent:observation-checkpoint", MaxCalls: defaultToolCallsPerResponse}
	}
	if task != nil && full != nil && task.GraphID != "" &&
		task.GraphNodeKind == string(graph.KindController) &&
		task.GraphControllerRole == string(graph.ControllerRoleLoopRecovery) {
		allow := []string{
			"commit_graph_change", "get_task_result", "propose_graph_change",
			"read_content_ref", "read_graph", "read_graph_change",
			"submit_recovery_decision", "validate_graph_change",
		}
		return invocationToolPolicy{
			Registry: full.Filtered(allow), Phase: "scheduler:graph-recovery",
			MaxCalls: defaultToolCallsPerResponse,
		}
	}
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
	controlScope, _ := model.ClassifyControlScope(task)
	switch {
	case controlScope == model.ControlScopeFinalReport:
		if historyRequiresDeliverableSubmit(history) || finalReportReadTurns(history) >= 2 {
			phase = "scheduler:final-report-submit"
			allow = []string{"report_done"}
		} else {
			phase = "scheduler:final-report"
			allow = []string{"read_graph", "get_task_result", "read_content_ref", "report_done"}
		}
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

func observationCheckpointRegistry(registry *ToolRegistry, task *model.Task, history []HistoryEntry) *ToolRegistry {
	refs := make([]string, 0, 64)
	openCandidates := make([]string, 0, taskmem.MaxObservationNext)
	foundObservationReceipt := false
	seen := make(map[string]struct{})
	add := func(value string) {
		if value == "" || len(refs) >= 64 {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		refs = append(refs, value)
	}
	for i := len(history) - 1; i >= 0 && len(refs) < 64; i-- {
		results := make(map[string]string, len(history[i].ToolResults))
		for _, result := range history[i].ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for j := len(history[i].ToolCalls) - 1; j >= 0; j-- {
			call := history[i].ToolCalls[j]
			if call.ID != "" && !unsuccessfulToolResult(results[call.ID]) {
				if call.Name == "record_observation_delta" && !foundObservationReceipt {
					var receipt struct {
						OpenCandidates []string `json:"open_candidate_refs"`
					}
					if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil {
						foundObservationReceipt = true
						openCandidates = append(openCandidates, receipt.OpenCandidates...)
					}
				}
				if call.Name == "run_check" {
					var receipt struct {
						CheckRef string `json:"check_ref"`
					}
					if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil {
						add(receipt.CheckRef)
					}
				}
				add("tool-call:" + call.ID)
			}
		}
	}
	if task != nil {
		artifacts := append([]string(nil), task.Artifacts...)
		sort.Strings(artifacts)
		for _, artifact := range artifacts {
			add("artifact:" + artifact)
		}
	}
	sort.Strings(refs)
	return registry.WithDefinitionParameters("record_observation_delta", func(parameters map[string]any) {
		properties, ok := parameters["properties"].(map[string]any)
		if !ok {
			return
		}
		facts, ok := properties["facts"].(map[string]any)
		if !ok {
			return
		}
		items, ok := facts["items"].(map[string]any)
		if !ok {
			return
		}
		factProperties, ok := items["properties"].(map[string]any)
		if !ok {
			return
		}
		evidence, ok := factProperties["evidence_refs"].(map[string]any)
		if !ok {
			return
		}
		evidenceItems, ok := evidence["items"].(map[string]any)
		if !ok {
			return
		}
		values := make([]any, len(refs))
		for i, ref := range refs {
			values[i] = ref
		}
		evidenceItems["enum"] = values
		evidence["description"] = "只能从本 Invocation schema 提供的 settled evidence enum 中选择"
		resolved, ok := properties["resolved_candidates"].(map[string]any)
		if !ok {
			return
		}
		resolvedItems, ok := resolved["items"].(map[string]any)
		if !ok {
			return
		}
		resolvedProperties, ok := resolvedItems["properties"].(map[string]any)
		if !ok {
			return
		}
		candidate, ok := resolvedProperties["candidate_ref"].(map[string]any)
		if !ok {
			return
		}
		candidateValues := make([]any, len(openCandidates))
		for i, ref := range openCandidates {
			candidateValues[i] = ref
		}
		candidate["enum"] = candidateValues
		candidate["description"] = "只能关闭上一份 Observation receipt 给出的 open candidate ref；首份状态必须提交空数组"
		resolvedEvidence, ok := resolvedProperties["evidence_refs"].(map[string]any)
		if !ok {
			return
		}
		resolvedEvidenceItems, ok := resolvedEvidence["items"].(map[string]any)
		if !ok {
			return
		}
		resolvedEvidenceItems["enum"] = values
		resolvedEvidence["description"] = "只能选择本 Invocation schema 提供、且晚于 predecessor 的 settled evidence；时间顺序由 L3 再校验"
	})
}

func finalReportReadTurns(history []HistoryEntry) int {
	turns := 0
	for _, entry := range history {
		read := false
		for _, call := range entry.ToolCalls {
			switch call.Name {
			case "read_graph", "get_task_result", "read_content_ref":
				read = true
			}
		}
		if read {
			turns++
		}
	}
	return turns
}

func historyRequiresDeliverableSubmit(history []HistoryEntry) bool {
	for index := len(history) - 1; index >= 0; index-- {
		if strings.Contains(history[index].SystemNotice, progressDeliverableRequiredMarker) {
			return true
		}
	}
	return false
}

func historyRequiresObservationCheckpoint(history []HistoryEntry) bool {
	return pendingObservationCheckpointAction(history) != ""
}

func pendingObservationCheckpointAction(history []HistoryEntry) string {
	pending := false
	action := ""
	for _, entry := range history {
		if strings.Contains(entry.SystemNotice, observationCheckpointAbandonedMarker) {
			pending = false
			action = ""
			continue
		}
		if strings.Contains(entry.SystemNotice, observationCheckpointRequiredMarker) {
			pending = true
			action = observationActionFromNotice(entry.SystemNotice)
		}
		for _, result := range entry.ToolResults {
			if strings.Contains(result.Content, "reason_code=observation_checkpoint_required") &&
				strings.Contains(result.Content, observationCheckpointRequiredMarker) {
				pending = true
				action = observationActionFromNotice(result.Content)
			}
		}
		if !pending {
			continue
		}
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for _, call := range entry.ToolCalls {
			if call.Name == "record_observation_delta" && !unsuccessfulToolResult(results[call.ID]) {
				pending = false
				action = ""
			}
		}
	}
	if !pending {
		return ""
	}
	return action
}

func observationCheckpointFailureCount(history []HistoryEntry) int {
	count := 0
	for _, entry := range history {
		if strings.Contains(entry.SystemNotice, observationCheckpointRequiredMarker) {
			count = 0
		}
		if strings.Contains(entry.SystemNotice, observationCheckpointFailureMarker) {
			count++
		}
		for _, call := range entry.ToolCalls {
			if call.Name != "record_observation_delta" {
				continue
			}
			result := ""
			for _, candidate := range entry.ToolResults {
				if candidate.ToolCallID == call.ID {
					result = candidate.Content
					break
				}
			}
			if unsuccessfulToolResult(result) {
				count++
			}
		}
	}
	return count
}

func observationActionFromNotice(notice string) string {
	const prefix = "action="
	index := strings.Index(notice, prefix)
	if index < 0 {
		return "continue"
	}
	value := notice[index+len(prefix):]
	if end := strings.IndexAny(value, "] \n\t"); end >= 0 {
		value = value[:end]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "continue"
	}
	return value
}

func observationCheckpointNotice(action, reminder string) string {
	if strings.TrimSpace(action) == "" {
		action = "continue"
	}
	return fmt.Sprintf("%s action=%s] %s", observationCheckpointRequiredMarker, action, reminder)
}

func observationCheckpointSucceeded(result ExecuteResult) bool {
	results := make(map[string]string, len(result.ToolResults))
	for _, item := range result.ToolResults {
		results[item.ToolCallID] = item.Content
	}
	for _, call := range result.ToolCalls {
		if call.Name == "record_observation_delta" && !unsuccessfulToolResult(results[call.ID]) {
			return true
		}
	}
	return false
}

// mechanicalControlHistoryProjection 用于机械交付和 Observation
// checkpoint Invocation。Raw History 始终保持不变；本轮 L2 只投影控制
// notice，避免 provider 在 singleton ToolRouter 下仍延续旧 read/grep/edit
// tool call。TaskMemory、任务目标、上游输入、output contract 与 Observation
// evidence enum 由独立 L2/L3 authority 注入，不依赖旧轮次正文。
func mechanicalControlHistoryProjection(history []HistoryEntry) []HistoryEntry {
	projected := make([]HistoryEntry, 0, len(history))
	for _, entry := range history {
		if notice := strings.TrimSpace(entry.SystemNotice); notice != "" {
			projected = append(projected, HistoryEntry{SystemNotice: notice})
		}
	}
	return projected
}

// businessHistoryProjection 把 L3 Control Invocation 从正常业务 Responses
// replay 中移除。Observation 已通过 durable TaskMemory/ObservationRef 注入；
// 再重放 reasoning=none 的 exact tool item 会把业务 thinking 链与控制链混接。
func businessHistoryProjection(history []HistoryEntry) []HistoryEntry {
	projected := make([]HistoryEntry, 0, len(history))
	for _, entry := range history {
		if len(entry.ToolCalls) > 0 {
			controlOnly := true
			for _, call := range entry.ToolCalls {
				if call.Name != "record_observation_delta" {
					controlOnly = false
					break
				}
			}
			if controlOnly {
				continue
			}
		}
		projected = append(projected, entry)
	}
	return projected
}

// unsuccessfulToolResult 兼容历史已持久的半角/全角中文冒号。
// skipped call 只是为 provider call_id 补齐 replay output，不得驱动阶段迁移。
func unsuccessfulToolResult(content string) bool {
	return content == "" || strings.HasPrefix(content, "错误:") ||
		strings.HasPrefix(content, "错误：") ||
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
