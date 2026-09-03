package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/observationcontract"
	"agentgo/internal/policycatalog"
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
Use the authoritative task objective, upstream input, output contract, TaskMemory, and any settled investigation-evidence notices present to submit completed, failed, or blocked. Do not answer with text.
</agent-phase>`

const observationCheckpointPhasePrompt = `<agent-phase name="observation-checkpoint">
This is a mechanical L2 checkpoint before Context projection, Attempt rollover, or intervention.
The only legal action is record_observation_delta. Phase must be investigate, implement, verify, finalize, or blocked. Facts are evidence-bound model claims with text and evidence_refs, never bare strings; the framework stores them as inferred, not confirmed semantic truth. Use an empty facts array when the current evidence enum is empty. Every evidence_ref must be copied literally from the relevant enum in this invocation's tool schema and the system catalog below. Never use ev:, content:, file:, grep:, a bare call_id, a composed reference, upstream evidence, or a prior Task/Attempt reference. If you cannot copy a listed literal exactly, use empty facts and resolved_candidates arrays instead of inventing a reference. Facts are the complete currently valid claim projection, not an append-only recap. Close predecessor candidates only with the candidate_ref and post-predecessor evidence enums in this invocation; merely rewording or adding candidates is not progress. Keep next candidates within the schema-bounded list. next_action is your typed commitment for the next ordinary business stage: choose mutate only when you are ready to perform the declared edit_file/write_file path; otherwise choose continue, need_context, verify, or blocked without mutation. Never include chain-of-thought or raw tool bodies. Do not edit, test, message, submit the task result, or answer with text.
</agent-phase>`

const observationCommitmentPhasePrompt = `<agent-phase name="observation-commitment">
The latest durable Observation records a model-chosen mutation commitment. The only legal action is the mutation tool and frozen project-relative path exposed by this invocation's tool schema. Generate only the edit content arguments needed for that exact mutation. Do not read, grep, test, submit, or answer with text. A rejected argument keeps the commitment pending.
</agent-phase>`

func recoveryActionPhasePrompt(gate recoveryActionGate) string {
	target := gate.Tool
	if gate.Path != "" {
		target += " with frozen path " + gate.Path
	}
	if gate.CheckID != "" {
		target += " with frozen check_id " + gate.CheckID
	}
	if gate.RefID != "" {
		target += fmt.Sprintf(" with frozen ref_id %s offset %d limit %d", gate.RefID, gate.Offset, gate.Limit)
	}
	guidance := "Generate the remaining arguments from the task and frozen recovery directive."
	switch gate.Stage {
	case recoveryStageEvidence:
		guidance = "Read exactly the frozen evidence segment. This closes one mechanical coverage gap; it is not permission to browse another path."
	case recoveryStageEvidenceUnavailable:
		guidance = "The frozen evidence read failed, so mutation and further context expansion are unsafe. Submit hypothesis_rejected or blocked with the concrete read failure as reason; do not edit or claim coverage."
	case recoveryStageDecision:
		guidance = "The current EvidenceContract is fully covered and fresh. Submit edit with explicit ordered {tool,path} edit_steps, need_context with one new project-relative evidence file and reason, or hypothesis_rejected/blocked. Evidence files justify the decision but do not restrict mutation targets; write_file may declare a new path. Do not edit in this invocation."
		if gate.Schema == graph.RecoveryDeltaSchemaV5 {
			guidance = "The current bounded focus context is available. Submit edit with ordered {tool,path} steps, resume_candidate for a valid dirty candidate, need_context for one additional focus page, or hypothesis_rejected/blocked. For the same file, need_context must provide a new 1-based offset and the schema-constant limit; use upstream symbol/line evidence to jump near the target instead of paging the whole file. Every edit_file target must first enter focus context; write_file may declare a new path. Do not edit in this invocation."
		}
	case recoveryStageMutation:
		guidance = "You previously chose edit and declared this exact edit step. Apply that decision now; do not return to investigation or merely describe the patch."
	case recoveryStageCheck:
		guidance = "Run the frozen CheckContract against the mutation already present. Copy kind and command from the current tool schema and RunContract."
	}
	return `<agent-phase name="` + gate.Phase + `">
This invocation is a frozen L3 recovery handoff. The only legal action is ` + target + `.
Do not choose another tool or answer with text. ` + guidance + ` A mechanical argument error keeps this stage active so it can be corrected.
</agent-phase>`
}

func observationCheckpointCatalogPrompt(defs []llm.ToolDef) string {
	var facts, candidates, resolved []string
	for _, def := range defs {
		if def.Name != "record_observation_delta" {
			continue
		}
		facts = nestedStringEnum(def.Parameters, "properties", "facts", "items", "properties", "evidence_refs", "items", "enum")
		candidates = nestedStringEnum(def.Parameters, "properties", "resolved_candidates", "items", "properties", "candidate_ref", "enum")
		resolved = nestedStringEnum(def.Parameters, "properties", "resolved_candidates", "items", "properties", "evidence_refs", "items", "enum")
		break
	}
	factsJSON, _ := json.Marshal(facts)
	candidatesJSON, _ := json.Marshal(candidates)
	resolvedJSON, _ := json.Marshal(resolved)
	return fmt.Sprintf(`<observation-evidence-catalog>
facts.evidence_refs literals: %s
resolved_candidates.candidate_ref literals: %s
resolved_candidates.evidence_refs post-predecessor literals: %s
Copy only these complete strings. Empty list means submit the corresponding array empty.
</observation-evidence-catalog>`, factsJSON, candidatesJSON, resolvedJSON)
}

func nestedStringEnum(root map[string]any, path ...string) []string {
	var current any = root
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	values, ok := current.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

const finalReportSubmitPhasePrompt = `<scheduler-phase name="final-report-submit">
This is the terminal user delivery. The only legal action is one report_done call using the frozen GraphTerminalSummary and any already-read TaskOutcome facts. Do not read more data, create or change a Graph, rerun business work, or answer with text.
</scheduler-phase>`

type invocationToolPolicy struct {
	Registry     *ToolRegistry
	Phase        string
	MaxCalls     int
	RecoveryGate *recoveryActionGate
}

type recoveryActionStage string

const (
	recoveryStageFirstAction         recoveryActionStage = "first_action"
	recoveryStageEvidence            recoveryActionStage = "evidence"
	recoveryStageEvidenceUnavailable recoveryActionStage = "evidence_unavailable"
	recoveryStageDecision            recoveryActionStage = "decision"
	recoveryStageMutation            recoveryActionStage = "mutation"
	recoveryStageCheck               recoveryActionStage = "check"
)

type recoveryDirective struct {
	Schema         string
	FirstAction    graph.RecoveryFirstAction
	EvidenceFiles  []string
	DirectiveCount int
}

type recoveryActionGate struct {
	Schema         string
	Stage          recoveryActionStage
	Phase          string
	Tool           string
	Path           string
	CheckID        string
	RefID          string
	Offset         int64
	Limit          int64
	ForceFull      bool
	DirectiveCount int
}

func isObservationPhase(phase string) bool {
	return phase == "agent:observation-checkpoint" || isAutoObservationPhase(phase)
}

func isAutoObservationPhase(phase string) bool {
	return phase == "agent:observation-checkpoint-v8" || phase == "agent:observation-checkpoint-v9" ||
		phase == "agent:observation-checkpoint-v10" || phase == "agent:observation-checkpoint-v11" || phase == "agent:observation-checkpoint-v12"
}

// observationOutputLimits 按冻结 Progress 版本返回控制调用预算。v7/v8 的
// 2048/32KiB 保持历史语义；v9 只扩大 completion/response 容器，不改变
// Observation schema、业务 history 或 L3 required-action gate。
func observationOutputLimits(phase string) (completionTokens, responseBytes int64) {
	if phase == "agent:observation-checkpoint-v9" || phase == "agent:observation-checkpoint-v10" || phase == "agent:observation-checkpoint-v11" || phase == "agent:observation-checkpoint-v12" {
		return 4096, 48 << 10
	}
	return 2048, 32 << 10
}

func recoveryV5CompletionLimit(stage recoveryActionStage, history []HistoryEntry) int64 {
	escalated := len(history) > 0 && strings.Contains(history[len(history)-1].SystemNotice, "[same-snapshot-retry")
	switch stage {
	case recoveryStageDecision:
		if escalated {
			return 8192
		}
		return 4096
	case recoveryStageMutation:
		if escalated {
			return 8192
		}
		return 4096
	default:
		return 2048
	}
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
	case "agent:observation-checkpoint-v8", "agent:observation-checkpoint-v9", "agent:observation-checkpoint-v10", "agent:observation-checkpoint-v11", "agent:observation-checkpoint-v12":
		// v8+ 改为 auto + singleton ToolRouter。required-action gate 仍在 L3
		// 强制唯一合法调用；v9 只调整冻结输出预算。
		return invocation.ToolChoice{Mode: invocation.ToolChoiceAuto}
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
	case "agent:observation-checkpoint-v8", "agent:observation-checkpoint-v9", "agent:observation-checkpoint-v10", "agent:observation-checkpoint-v11", "agent:observation-checkpoint-v12":
		return "low", true
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
	case "agent:observation-checkpoint-v8", "agent:observation-checkpoint-v9", "agent:observation-checkpoint-v10", "agent:observation-checkpoint-v11", "agent:observation-checkpoint-v12":
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
		phase == "scheduler:final-report" || phase == "agent:observation-commitment" ||
		strings.HasPrefix(phase, "agent:recovery-")
}

func phaseDispatchesOnlyFirstTool(phase string) bool {
	return mechanicalSingletonPhase(phase) || phase == "scheduler:draft-edit" ||
		phase == "scheduler:recovery" || phase == "scheduler:graph-recovery" ||
		phase == "agent:observation-commitment" || strings.HasPrefix(phase, "agent:recovery-")
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
	full := businessRegistryWithoutFrameworkControl(business)
	full = registryWithTaskCheckContract(full, task)
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
		phase := "agent:observation-checkpoint"
		if task.ProgressContract != nil {
			switch task.ProgressContract.Ref.ContractID {
			case policycatalog.ProgressCodeChangeV8:
				phase = "agent:observation-checkpoint-v8"
			case policycatalog.ProgressCodeChangeV9:
				phase = "agent:observation-checkpoint-v9"
			case policycatalog.ProgressCodeChangeV10:
				phase = "agent:observation-checkpoint-v10"
			case policycatalog.ProgressCodeChangeV11:
				phase = "agent:observation-checkpoint-v11"
			case policycatalog.ProgressCodeChangeV12:
				phase = "agent:observation-checkpoint-v12"
			}
		}
		return invocationToolPolicy{Registry: view, Phase: phase, MaxCalls: defaultToolCallsPerResponse}
	}
	if view, gate, required := recoveryActionRegistry(full, frameworkControl, task, history); required {
		return invocationToolPolicy{Registry: view, Phase: gate.Phase,
			MaxCalls: defaultToolCallsPerResponse, RecoveryGate: &gate}
	}
	if view, required := observationCommitmentRegistry(full, task, history); required {
		return invocationToolPolicy{Registry: view, Phase: "agent:observation-commitment",
			MaxCalls: defaultToolCallsPerResponse}
	}
	if task != nil && full != nil && task.GraphID != "" &&
		task.GraphNodeKind == string(graph.KindController) &&
		task.GraphControllerRole == string(graph.ControllerRoleLoopRecovery) {
		allow := []string{
			"commit_graph_change", "get_task_result", "propose_graph_change",
			"read_content_ref", "read_graph", "read_graph_change",
			"submit_recovery_decision", "validate_graph_change",
		}
		view := full.Filtered(allow)
		view = recoveryDecisionRegistry(view, task)
		return invocationToolPolicy{
			Registry: view, Phase: "scheduler:graph-recovery",
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
	case controlScope == model.ControlScopeGraphChange:
		phase = "scheduler:recovery"
		allow = []string{
			"read_graph", "get_task_result", "read_content_ref",
			"propose_graph_change", "read_graph_change", "validate_graph_change", "commit_graph_change",
		}
		if graphChangeReadObserved(history) {
			allow = append(allow, "submit_graph_change_decision")
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

func recoveryDecisionRegistry(registry *ToolRegistry, task *model.Task) *ToolRegistry {
	if registry == nil || task == nil ||
		(task.GraphRecoveryDeltaSchema != graph.RecoveryDeltaSchemaV3 && task.GraphRecoveryDeltaSchema != graph.RecoveryDeltaSchemaV4 &&
			task.GraphRecoveryDeltaSchema != graph.RecoveryDeltaSchemaV5) {
		return registry
	}
	return registry.WithDefinitionParameters("submit_recovery_decision", func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		firstAction, _ := properties["first_action"].(map[string]any)
		actionProperties, _ := firstAction["properties"].(map[string]any)
		tool, _ := actionProperties["tool"].(map[string]any)
		if tool != nil {
			tool["enum"] = []any{"read_file"}
			tool["description"] = "RecoveryDelta v3+ 必须先为新 Task 建立目标业务文件读集"
		}
		if firstAction != nil {
			firstAction["required"] = []any{"tool", "path"}
			firstAction["description"] = "handoff 首读；具体后续由冻结 recovery schema 决定"
		}
		if task.GraphRecoveryDeltaSchema != graph.RecoveryDeltaSchemaV4 &&
			task.GraphRecoveryDeltaSchema != graph.RecoveryDeltaSchemaV5 {
			return
		}
		properties["evidence_contract"] = map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"files": map[string]any{
					"type": "array", "minItems": 1, "maxItems": graph.MaxRecoveryEvidenceFiles,
					"items":       map[string]any{"type": "string"},
					"description": "下一 work Activation 在提交修改决策前必须完整覆盖的项目相对文件；首项必须等于 first_action.path",
				},
			},
			"required":    []any{"files"},
			"description": "RecoveryDelta v4 EvidenceContract；只声明最小必要文件，不替 Worker 编造修改内容",
		}
		if task.GraphRecoveryDeltaSchema == graph.RecoveryDeltaSchemaV5 {
			contract := properties["evidence_contract"].(map[string]any)
			contractProperties := contract["properties"].(map[string]any)
			files := contractProperties["files"].(map[string]any)
			files["maxItems"] = 1
			files["description"] = "v5 单一 focus file；首项必须等于 first_action.path，后续上下文只能经 typed need_context 逐个增加"
			contract["description"] = "RecoveryDelta v5 bounded focus handoff；Runtime 另行绑定 candidate_state"
		}
	})
}

// graphChangeReadObserved 只在当前 raw History 中存在成功 read_graph receipt 后
// 开放 no_change 结构化收口。read_graph handler 已按 InterventionGraphID
// fail-closed，因此这里无需从模型参数再次推断 scope。
func recoveryActionRegistry(registry, frameworkControl *ToolRegistry, task *model.Task,
	history []HistoryEntry) (*ToolRegistry, recoveryActionGate, bool) {
	if registry == nil {
		return nil, recoveryActionGate{}, false
	}
	directive, ok := frozenRecoveryDirective(task)
	if !ok {
		return registry, recoveryActionGate{}, false
	}
	if directive.Schema == graph.RecoveryDeltaSchemaV4 {
		return recoveryV4ActionRegistry(registry, frameworkControl, task, directive, history)
	}
	if directive.Schema == graph.RecoveryDeltaSchemaV5 {
		return recoveryV5ActionRegistry(registry, frameworkControl, task, directive, history)
	}
	firstSettled, firstSuccessful := recoveryToolSettlement(history,
		directive.FirstAction.Tool, directive.FirstAction.Path)
	if directive.Schema == graph.RecoveryDeltaSchemaV2 {
		if firstSettled {
			return registry, recoveryActionGate{}, false
		}
		return recoveryToolGate(registry, directive, recoveryStageFirstAction,
			"agent:recovery-first-action", directive.FirstAction.Tool, directive.FirstAction.Path, "", "", "")
	}
	// v3 的首读、同路径 mutation 与 typed check 都是机械阶段。只有真实成功
	// 的读/编辑或形成 CheckRecord 的检查才能推进；参数错误不会把 gate 消耗掉。
	if !firstSuccessful {
		return recoveryToolGate(registry, directive, recoveryStageFirstAction,
			"agent:recovery-first-action", directive.FirstAction.Tool, directive.FirstAction.Path, "", "", "")
	}
	mutationSettled, mutationSuccessful := recoveryToolSettlement(history, "edit_file", directive.FirstAction.Path)
	if !mutationSuccessful {
		_ = mutationSettled
		return recoveryToolGate(registry, directive, recoveryStageMutation,
			"agent:recovery-mutation", "edit_file", directive.FirstAction.Path, "", "", "")
	}
	check := recoveryRequiredCheckContract(task)
	checkID := strings.TrimSpace(check.CheckID)
	if checkID == "" || recoveryCheckRecorded(history, checkID) {
		return registry, recoveryActionGate{}, false
	}
	return recoveryToolGate(registry, directive, recoveryStageCheck,
		"agent:recovery-check", "run_check", "", checkID, check.Kind, check.ExactCommand)
}

func recoveryToolGate(registry *ToolRegistry, directive recoveryDirective, stage recoveryActionStage,
	phase, toolName, path, checkID, checkKind, checkCommand string) (*ToolRegistry, recoveryActionGate, bool) {
	view := registry.Filtered([]string{toolName})
	view = view.WithDefinitionParameters(toolName, func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		if properties == nil {
			return
		}
		if path != "" {
			field, _ := properties["path"].(map[string]any)
			if field != nil {
				field["type"] = "string"
				field["const"] = path
				field["description"] = "L5 recovery 冻结的目标路径，必须逐字使用"
			}
		}
		if checkID != "" {
			field, _ := properties["check_id"].(map[string]any)
			if field != nil {
				field["type"] = "string"
				field["const"] = checkID
				field["description"] = "Recovery handoff 冻结的 CheckContract ID，必须逐字使用"
			}
			if checkKind != "" {
				kind, _ := properties["kind"].(map[string]any)
				if kind != nil {
					kind["type"] = "string"
					kind["const"] = checkKind
					kind["description"] = "Recovery handoff 冻结的 CheckContract kind，必须逐字使用"
				}
			}
			if checkCommand != "" {
				command, _ := properties["command"].(map[string]any)
				if command != nil {
					command["type"] = "string"
					command["const"] = checkCommand
					command["description"] = "Recovery handoff 冻结的 exact_command，必须逐字使用"
				}
			}
		}
	})
	gate := recoveryActionGate{Schema: directive.Schema, Stage: stage, Phase: phase,
		Tool: toolName, Path: path, CheckID: checkID, DirectiveCount: directive.DirectiveCount}
	return view, gate, true
}

func frozenRecoveryDirective(task *model.Task) (recoveryDirective, bool) {
	if task == nil {
		return recoveryDirective{}, false
	}
	var latest recoveryDirective
	for _, input := range task.ContextInputs {
		if input.Kind != model.TaskContextUpstreamResult {
			continue
		}
		start, end := strings.IndexByte(input.Content, '{'), strings.LastIndexByte(input.Content, '}')
		if start < 0 || end < start {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(input.Content[start:end+1]), &payload) != nil {
			continue
		}
		if target, _ := payload["target_input"].(string); target != "recovery_directive" {
			continue
		}
		result, _ := payload["result"].(map[string]any)
		schema, _ := result["schema"].(string)
		if result == nil || (schema != graph.RecoveryDeltaSchemaV2 && schema != graph.RecoveryDeltaSchemaV3 && schema != graph.RecoveryDeltaSchemaV4 && schema != graph.RecoveryDeltaSchemaV5) {
			continue
		}
		raw, _ := result["first_action"].(map[string]any)
		if raw == nil {
			continue
		}
		action := graph.RecoveryFirstAction{}
		action.Tool, _ = raw["tool"].(string)
		action.Path, _ = raw["path"].(string)
		if strings.TrimSpace(action.Tool) != "" {
			var evidenceFiles []string
			if contract, _ := result["evidence_contract"].(map[string]any); contract != nil {
				evidenceFiles = recoveryStringValues(contract["files"])
			}
			latest = recoveryDirective{Schema: schema, FirstAction: action, EvidenceFiles: evidenceFiles,
				DirectiveCount: latest.DirectiveCount + 1}
		}
	}
	return latest, latest.DirectiveCount > 0
}

func recoveryStringValues(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, _ := item.(string)
		if text = strings.TrimSpace(text); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func frozenRecoveryFirstAction(task *model.Task) (graph.RecoveryFirstAction, bool) {
	directive, ok := frozenRecoveryDirective(task)
	return directive.FirstAction, ok
}

func recoveryToolSettlement(history []HistoryEntry, toolName, path string) (bool, bool) {
	settled, successful := false, false
	for _, entry := range history {
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for _, call := range entry.ToolCalls {
			if call.Name != toolName {
				continue
			}
			if path != "" && strings.TrimSpace(fmt.Sprint(call.Arguments["path"])) != path {
				continue
			}
			content, ok := results[call.ID]
			if ok {
				settled = true
				successful = successful || !unsuccessfulToolResult(content)
			}
		}
	}
	return settled, successful
}

func recoveryRequiredCheckContract(task *model.Task) runcontract.CheckContract {
	if task == nil {
		return runcontract.CheckContract{}
	}
	if task.RunContract != nil {
		for _, contract := range task.RunContract.CheckContracts {
			if strings.TrimSpace(contract.CheckID) == "targeted" {
				return contract
			}
		}
	}
	if task.FulfillmentContract != nil {
		for _, raw := range task.FulfillmentContract.RequiredCheckIDs {
			if id := strings.TrimSpace(raw); id != "" {
				return runcontract.CheckContract{CheckID: id}
			}
		}
	}
	return runcontract.CheckContract{}
}

func recoveryCheckRecorded(history []HistoryEntry, checkID string) bool {
	for _, entry := range history {
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for _, call := range entry.ToolCalls {
			if call.Name != "run_check" || strings.TrimSpace(fmt.Sprint(call.Arguments["check_id"])) != checkID {
				continue
			}
			var receipt struct {
				CheckID string `json:"check_id"`
				Status  string `json:"status"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil && receipt.CheckID == checkID && receipt.Status != "" {
				return true
			}
		}
	}
	return false
}

func graphChangeReadObserved(history []HistoryEntry) bool {
	for _, entry := range history {
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for _, call := range entry.ToolCalls {
			if call.Name == "read_graph" && !unsuccessfulToolResult(results[call.ID]) {
				return true
			}
		}
	}
	return false
}

func registryWithTaskCheckContract(registry *ToolRegistry, task *model.Task) *ToolRegistry {
	if registry == nil || task == nil {
		return registry
	}
	seen := make(map[string]struct{})
	allowed := make([]string, 0)
	if task.FulfillmentContract != nil {
		for _, raw := range task.FulfillmentContract.RequiredCheckIDs {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			allowed = append(allowed, id)
		}
	}
	if task.RunContract != nil {
		for _, contract := range task.RunContract.CheckContracts {
			id := strings.TrimSpace(contract.CheckID)
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			allowed = append(allowed, id)
		}
	}
	if len(allowed) == 0 {
		return registry
	}
	sort.Strings(allowed)
	return registry.WithDefinitionParameters("run_check", func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		if properties == nil {
			properties = make(map[string]any)
			parameters["properties"] = properties
		}
		checkID, _ := properties["check_id"].(map[string]any)
		if checkID == nil {
			checkID = make(map[string]any)
			properties["check_id"] = checkID
		}
		checkID["type"] = "string"
		checkID["enum"] = append([]string(nil), allowed...)
		details := make([]string, 0)
		if task.RunContract != nil {
			for _, contract := range task.RunContract.CheckContracts {
				if contract.ExactCommand != "" {
					details = append(details, fmt.Sprintf("%s exact_command=%q", contract.CheckID, contract.ExactCommand))
				}
			}
		}
		checkID["description"] = "当前冻结契约允许的 check_id；必须从 enum 逐字复制。同一 ID 的最新 CheckRecord 是 fulfillment 权威。" + strings.Join(details, "；")
	})
}

func businessRegistryWithoutFrameworkControl(registry *ToolRegistry) *ToolRegistry {
	if registry == nil {
		return nil
	}
	names := registry.Names()
	filtered := names[:0]
	for _, name := range names {
		if name != "record_observation_delta" && name != "submit_change_decision" {
			filtered = append(filtered, name)
		}
	}
	return registry.Filtered(filtered)
}

type observationMutationCommitment struct {
	Tool  string
	Path  string
	Entry int
}

func observationCommitmentRegistry(registry *ToolRegistry, task *model.Task,
	history []HistoryEntry) (*ToolRegistry, bool) {
	commitment, ok := pendingObservationMutation(task, history)
	if !ok || registry == nil {
		return registry, false
	}
	view := registry.Filtered([]string{commitment.Tool})
	view = view.WithDefinitionParameters(commitment.Tool, func(parameters map[string]any) {
		properties, _ := parameters["properties"].(map[string]any)
		setRecoverySchemaConst(properties, "path", commitment.Path,
			"Observation v4 冻结的模型自选 mutation 路径")
		requireRecoverySchemaFields(parameters, "path")
	})
	if fn := view.tools[commitment.Tool]; fn != nil {
		view.tools[commitment.Tool] = func(ctx context.Context, args map[string]any) (string, error) {
			path, err := graph.CanonicalRecoveryEvidencePath(fmt.Sprint(args["path"]))
			if err != nil || path != commitment.Path {
				return "", fmt.Errorf("Observation mutation commitment 要求 path=%q", commitment.Path)
			}
			return fn(ctx, args)
		}
	}
	return view, true
}

func pendingObservationMutation(task *model.Task, history []HistoryEntry) (observationMutationCommitment, bool) {
	for entryIndex := len(history) - 1; entryIndex >= 0; entryIndex-- {
		if task != nil && strings.TrimSpace(task.AttemptID) != "" &&
			!strings.HasPrefix(history[entryIndex].TurnID, task.AttemptID+"/") {
			continue
		}
		results := recoveryResultsByCall(history[entryIndex])
		for callIndex := len(history[entryIndex].ToolCalls) - 1; callIndex >= 0; callIndex-- {
			call := history[entryIndex].ToolCalls[callIndex]
			if call.Name != "record_observation_delta" || unsuccessfulToolResult(results[call.ID]) {
				continue
			}
			var receipt struct {
				Schema     string                         `json:"schema"`
				NextAction *taskmem.ObservationNextAction `json:"next_action"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) != nil ||
				receipt.Schema != taskmem.ObservationDeltaSchemaV4 || receipt.NextAction == nil ||
				receipt.NextAction.Decision != taskmem.ObservationNextMutate ||
				receipt.NextAction.Mutation == nil {
				return observationMutationCommitment{}, false
			}
			commitment := observationMutationCommitment{Tool: receipt.NextAction.Mutation.Tool,
				Path: receipt.NextAction.Mutation.Path, Entry: entryIndex}
			for later := entryIndex + 1; later < len(history); later++ {
				laterResults := recoveryResultsByCall(history[later])
				for _, laterCall := range history[later].ToolCalls {
					if laterCall.Name == commitment.Tool && canonicalCallPath(laterCall) == commitment.Path &&
						!unsuccessfulToolResult(laterResults[laterCall.ID]) {
						return observationMutationCommitment{}, false
					}
				}
			}
			return commitment, true
		}
	}
	return observationMutationCommitment{}, false
}

func observationCheckpointRegistry(registry *ToolRegistry, task *model.Task, history []HistoryEntry) *ToolRegistry {
	refs := make([]string, 0, 64)
	resolvedRefs := make([]string, 0, 64)
	openCandidates := make([]string, 0, taskmem.MaxObservationNext)
	seen := make(map[string]struct{})
	seenResolved := make(map[string]struct{})
	addTo := func(target *[]string, targetSeen map[string]struct{}, value string) {
		if value == "" || len(*target) >= 64 {
			return
		}
		if _, exists := targetSeen[value]; exists {
			return
		}
		targetSeen[value] = struct{}{}
		*target = append(*target, value)
	}
	add := func(value string, afterPredecessor bool) {
		addTo(&refs, seen, value)
		if afterPredecessor {
			addTo(&resolvedRefs, seenResolved, value)
		}
	}
	// predecessor receipt 可以来自前一 Attempt；它只提供 open candidate
	// 身份，不授权复用前一 Attempt 的工具证据。
	latestObservationIndex := -1
	for i := len(history) - 1; i >= 0 && latestObservationIndex < 0; i-- {
		results := make(map[string]string, len(history[i].ToolResults))
		for _, result := range history[i].ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for j := len(history[i].ToolCalls) - 1; j >= 0; j-- {
			call := history[i].ToolCalls[j]
			if call.Name != "record_observation_delta" || call.ID == "" || unsuccessfulToolResult(results[call.ID]) {
				continue
			}
			var receipt struct {
				OpenCandidates []string `json:"open_candidate_refs"`
			}
			if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil {
				openCandidates = append(openCandidates, receipt.OpenCandidates...)
			}
			latestObservationIndex = i
			break
		}
	}
	for i := len(history) - 1; i >= 0 && len(refs) < 64; i-- {
		// Observation handler 只接受当前 Task/Attempt 的 settled authority；
		// schema enum 必须使用同一边界。Attempt rollover 后的 Raw History 仍含
		// 前一 Attempt 工具记录，若把它们暴露给模型，会诱导 exact control
		// Invocation 选择一个机械上必然被拒绝的 evidence_ref。
		if task != nil && strings.TrimSpace(task.AttemptID) != "" &&
			!strings.HasPrefix(history[i].TurnID, task.AttemptID+"/") {
			continue
		}
		results := make(map[string]string, len(history[i].ToolResults))
		for _, result := range history[i].ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for j := len(history[i].ToolCalls) - 1; j >= 0; j-- {
			call := history[i].ToolCalls[j]
			if call.ID != "" && !unsuccessfulToolResult(results[call.ID]) {
				if call.Name == "record_observation_delta" {
					continue
				}
				afterPredecessor := latestObservationIndex < 0 || i > latestObservationIndex
				if call.Name == "run_check" {
					var receipt struct {
						CheckRef string `json:"check_ref"`
					}
					if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil {
						add(receipt.CheckRef, afterPredecessor)
					}
				}
				add("tool-call:"+call.ID, afterPredecessor)
			}
		}
	}
	// task.Artifacts 是跨 Attempt 的累计展示值；当前 Attempt 是否真的写入由
	// Observation handler 的 artifact ledger 判定。这里没有该 authority 端口，
	// 因此不把累计路径伪装成合法 enum。当前 Attempt 的 write/edit ToolCall 仍可
	// 作为事实证据，artifact digest 则由后续业务进展机械读取。
	sort.Strings(refs)
	sort.Strings(resolvedRefs)
	return registry.WithDefinitionParameters("record_observation_delta", func(parameters map[string]any) {
		fresh := observationcontract.Parameters(observationcontract.SchemaProfile{
			EvidenceRefs: refs, OpenCandidateRefs: openCandidates,
			PostPredecessorEvidence: resolvedRefs,
		})
		for key := range parameters {
			delete(parameters, key)
		}
		for key, value := range fresh {
			parameters[key] = value
		}
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
	pending := false
	for _, entry := range history {
		if strings.Contains(entry.SystemNotice, progressDeliverableRequiredMarker) {
			pending = true
		}
		if pending && deliverableSubmissionNeedsMoreWork(entry) {
			pending = false
		}
	}
	return pending
}

func deliverableSubmissionNeedsMoreWork(entry HistoryEntry) bool {
	results := make(map[string]string, len(entry.ToolResults))
	for _, result := range entry.ToolResults {
		results[result.ToolCallID] = result.Content
	}
	for _, call := range entry.ToolCalls {
		if call.Name != "submit_task_result" {
			continue
		}
		content := results[call.ID]
		if unsuccessfulToolResult(content) && (strings.Contains(content, "contract_fulfillment_missing") ||
			strings.Contains(content, "output contract") || strings.Contains(content, "输出契约")) {
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

// observationCheckpointFailureDetail 只回显 framework control tool 的有界机械
// 校验错误，供唯一一次 fresh-projection retry 修正。它不读取 assistant 正文、
// 不修补 JSON，也不回灌其它业务工具输出。
func observationCheckpointFailureDetail(result ExecuteResult) string {
	callNames := make(map[string]string, len(result.ToolCalls))
	for _, call := range result.ToolCalls {
		callNames[call.ID] = call.Name
	}
	for _, item := range result.ToolResults {
		if callNames[item.ToolCallID] != "record_observation_delta" || !unsuccessfulToolResult(item.Content) {
			continue
		}
		return truncateTaskMemRunes(strings.TrimSpace(item.Content), 400)
	}
	return ""
}

// mechanicalControlHistoryProjection 用于机械交付和 Observation
// checkpoint Invocation。Raw History 始终保持不变；本轮 L2 只投影控制
// notice，避免 provider 在 singleton ToolRouter 下仍延续旧 read/grep/edit
// tool call。TaskMemory、任务目标、上游输入、output contract 与 Observation
// evidence enum 由独立 L2/L3 authority 注入，不依赖旧轮次正文。
func mechanicalControlHistoryProjection(history []HistoryEntry) []HistoryEntry {
	start := 0
	for index := len(history) - 1; index >= 0; index-- {
		if strings.Contains(history[index].SystemNotice, observationCheckpointRequiredMarker) {
			start = index
			break
		}
	}
	projected := make([]HistoryEntry, 0, len(history)-start)
	for _, entry := range history[start:] {
		if notice := strings.TrimSpace(entry.SystemNotice); notice != "" {
			projected = append(projected, HistoryEntry{SystemNotice: notice})
		}
	}
	return projected
}

// investigationDeliverableHistoryProjection 为 investigation/v3 的 exact
// submit 轮保留已结算源码证据，同时去掉 provider-visible 历史 ToolCall。直接用
// mechanicalControlHistoryProjection 会让 Explorer 在交付时只记得“读过”，却
// 看不到内容；直接保留 Raw exchange 又会诱发 singleton ToolRouter 粘滞重放。
func investigationDeliverableHistoryProjection(history []HistoryEntry) []HistoryEntry {
	const (
		maxEvidenceBytes          = 8 << 10
		maxEvidenceRunesPerResult = 1200
	)
	var evidence []map[string]any
	for entryIndex := len(history) - 1; entryIndex >= 0; entryIndex-- {
		entry := history[entryIndex]
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for callIndex := len(entry.ToolCalls) - 1; callIndex >= 0; callIndex-- {
			call := entry.ToolCalls[callIndex]
			content, settled := results[call.ID]
			if !settled || call.Name == "record_observation_delta" || call.Name == "submit_task_result" {
				continue
			}
			candidate := map[string]any{
				"tool": call.Name, "arguments": call.Arguments,
				"result": truncateTaskMemRunes(content, maxEvidenceRunesPerResult),
			}
			probe, _ := json.Marshal(append(append([]map[string]any(nil), evidence...), candidate))
			if len(probe) > maxEvidenceBytes {
				continue
			}
			evidence = append(evidence, candidate)
		}
	}
	encoded, _ := json.Marshal(evidence)
	projected := []HistoryEntry{{SystemNotice: "<investigation-evidence authority=\"settled-current-task\" order=\"newest-first\">\n" +
		string(encoded) + "\n</investigation-evidence>"}}
	projected = append(projected, HistoryEntry{SystemNotice: progressDeliverableRequiredMarker +
		" 使用上述 settled evidence 填写结构化调查结果；不得继续调用读取工具。"})
	return projected
}

// investigationDeliverableHistoryProjectionV4 在相同 8KiB 上限内优先保留源码
// read/read_content_ref，再保留 grep，最后才是目录/其它结果；每一优先级内部仍按
// newest-first。真实十轮 investigation 证明纯时间倒序会让尾部 grep 挤掉较早的
// 失败测试和状态所有者正文。v3 继续使用上面的历史投影，禁止静默迁移。
func investigationDeliverableHistoryProjectionV4(history []HistoryEntry) []HistoryEntry {
	const (
		maxEvidenceBytes          = 8 << 10
		maxEvidenceRunesPerResult = 1200
	)
	type candidate struct {
		priority int
		recency  int
		value    map[string]any
	}
	candidates := make([]candidate, 0, len(history))
	recency := 0
	for entryIndex := len(history) - 1; entryIndex >= 0; entryIndex-- {
		entry := history[entryIndex]
		results := make(map[string]string, len(entry.ToolResults))
		for _, result := range entry.ToolResults {
			results[result.ToolCallID] = result.Content
		}
		for callIndex := len(entry.ToolCalls) - 1; callIndex >= 0; callIndex-- {
			call := entry.ToolCalls[callIndex]
			content, settled := results[call.ID]
			if !settled || call.Name == "record_observation_delta" || call.Name == "submit_task_result" {
				continue
			}
			priority := 3
			switch call.Name {
			case "read_file", "read_content_ref":
				priority = 0
			case "grep_search":
				priority = 1
			case "glob_search", "list_dir":
				priority = 2
			}
			candidates = append(candidates, candidate{
				priority: priority, recency: recency,
				value: map[string]any{
					"tool": call.Name, "arguments": call.Arguments,
					"result": truncateTaskMemRunes(content, maxEvidenceRunesPerResult),
				},
			})
			recency++
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].recency < candidates[j].recency
	})
	evidence := make([]map[string]any, 0, len(candidates))
	for _, item := range candidates {
		probe, _ := json.Marshal(append(append([]map[string]any(nil), evidence...), item.value))
		if len(probe) > maxEvidenceBytes {
			continue
		}
		evidence = append(evidence, item.value)
	}
	encoded, _ := json.Marshal(evidence)
	projected := []HistoryEntry{{SystemNotice: "<investigation-evidence authority=\"settled-current-task\" priority=\"read-before-search\" order=\"newest-first-within-priority\">\n" +
		string(encoded) + "\n</investigation-evidence>"}}
	projected = append(projected, HistoryEntry{SystemNotice: progressDeliverableRequiredMarker +
		" 使用上述 settled evidence 填写结构化调查结果；不得继续调用读取工具。"})
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
				if ref := successfulObservationRef(entry); ref != "" {
					projected = append(projected, HistoryEntry{
						TurnID: entry.TurnID, ContextProjection: observationProjectionPrefix + ref,
					})
				}
				continue
			}
		}
		projected = append(projected, entry)
	}
	return projected
}

func successfulObservationRef(entry HistoryEntry) string {
	results := make(map[string]string, len(entry.ToolResults))
	for _, result := range entry.ToolResults {
		results[result.ToolCallID] = result.Content
	}
	for _, call := range entry.ToolCalls {
		if call.Name != "record_observation_delta" || unsuccessfulToolResult(results[call.ID]) {
			continue
		}
		var receipt struct {
			Ref string `json:"observation_delta_ref"`
		}
		if json.Unmarshal([]byte(results[call.ID]), &receipt) == nil && strings.TrimSpace(receipt.Ref) != "" {
			return strings.TrimSpace(receipt.Ref)
		}
	}
	return ""
}

// unsuccessfulToolResult 兼容历史已持久的半角/全角中文冒号。
// skipped call 只是为 provider call_id 补齐 replay output，不得驱动阶段迁移。
func skippedToolResult(content string) bool {
	return strings.HasPrefix(content, "已跳过:") || strings.HasPrefix(content, "已跳过：")
}

func unsuccessfulToolResult(content string) bool {
	return content == "" || strings.HasPrefix(content, "错误:") ||
		strings.HasPrefix(content, "错误：") || skippedToolResult(content)
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
		// 单动作阶段只会 dispatch provider 顺序中的首个调用；尾部调用只需
		// 保证协议身份可重放，不得因其使用了过滤前的合法工具名拒绝整个响应。
		if i == 0 || !phaseDispatchesOnlyFirstTool(router.Phase) {
			names = append(names, call.Name)
		}
	}
	if missing := router.Registry.Missing(names); len(missing) > 0 {
		return &actionContractViolation{detail: fmt.Sprintf("tool call batch 含 phase=%s 未授权工具 %v", router.Phase, missing)}
	}
	return nil
}

// validateRecoveryActionCall 对 provider 顺序中的首个、实际待 dispatch 调用
// 执行冻结参数校验。model-visible JSON schema 只用于引导，provider 可能忽略
// const；L3 必须在工具产生读取/修改副作用前独立 fail-closed。
func validateRecoveryActionCall(gate recoveryActionGate, calls []llm.ToolCall) error {
	if len(calls) == 0 {
		return nil
	}
	call := calls[0]
	if call.Name != gate.Tool {
		return &actionContractViolation{detail: fmt.Sprintf(
			"Recovery gate stage=%s 要求 tool=%s，实际=%s", gate.Stage, gate.Tool, call.Name)}
	}
	if gate.Path != "" && canonicalCallPath(call) != gate.Path {
		return &actionContractViolation{detail: fmt.Sprintf(
			"Recovery gate stage=%s 要求 path=%q", gate.Stage, gate.Path)}
	}
	if gate.CheckID != "" && strings.TrimSpace(fmt.Sprint(call.Arguments["check_id"])) != gate.CheckID {
		return &actionContractViolation{detail: fmt.Sprintf(
			"Recovery gate stage=%s 要求 check_id=%q", gate.Stage, gate.CheckID)}
	}
	if gate.RefID != "" && strings.TrimSpace(fmt.Sprint(call.Arguments["ref_id"])) != gate.RefID {
		return &actionContractViolation{detail: fmt.Sprintf(
			"Recovery gate stage=%s 要求 ref_id=%q", gate.Stage, gate.RefID)}
	}
	if gate.Limit > 0 {
		if int64(recoveryIntArgument(call.Arguments["offset"], -1)) != gate.Offset ||
			int64(recoveryIntArgument(call.Arguments["limit"], -1)) != gate.Limit {
			return &actionContractViolation{detail: fmt.Sprintf(
				"Recovery gate stage=%s 要求 offset=%d limit=%d", gate.Stage, gate.Offset, gate.Limit)}
		}
	}
	if gate.ForceFull {
		value, ok := call.Arguments["force_full"].(bool)
		if !ok || !value {
			return &actionContractViolation{detail: fmt.Sprintf(
				"Recovery gate stage=%s 要求 force_full=true", gate.Stage)}
		}
	}
	return nil
}

type actionContractViolation struct{ detail string }

func (e *actionContractViolation) Error() string { return e.detail }

func isActionContractViolation(err error) bool {
	var target *actionContractViolation
	return errors.As(err, &target)
}
