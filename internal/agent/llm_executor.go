package agent

import (
	"agentgo/internal/contextruntime"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentgo/internal/contextcontract"
	"agentgo/internal/controlcapability"
	"agentgo/internal/gate"
	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// executorContextKey 是注入到 context 中的键类型，用于传递执行上下文信息供日志和 trace 使用。
type executorContextKey int

const (
	ctxAgentID executorContextKey = iota
	ctxLoopNum
	ctxTaskID
	ctxCancelSource
	ctxActivity
	ctxToolDispatchGuard
	ctxToolName
	ctxRunID
	ctxAttemptID
	ctxTurnID
	ctxToolActionBoundary
)

func withToolActionBoundary(ctx context.Context, boundary toolActionBoundary) context.Context {
	if boundary == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxToolActionBoundary, boundary)
}

func toolActionBoundaryFromContext(ctx context.Context) toolActionBoundary {
	boundary, _ := ctx.Value(ctxToolActionBoundary).(toolActionBoundary)
	return boundary
}

// ToolDispatchGuard runs immediately before each concrete tool dispatch. The
// runner uses it to re-check task liveness (dispatch ctx 未取消 + 任务经 Store
// 重读仍 processing) after the LLM response and after every earlier tool in
// the same response.
type ToolDispatchGuard func(context.Context, *model.Task) error

// WithToolDispatchGuard installs a per-dispatch execution boundary. Tool calls
// are executed in model order, so a call that cancels/finalizes the task is
// visible to the guard before any later call can produce a side effect.
func WithToolDispatchGuard(ctx context.Context, guard ToolDispatchGuard) context.Context {
	return context.WithValue(ctx, ctxToolDispatchGuard, guard)
}

func toolDispatchGuardFromContext(ctx context.Context) ToolDispatchGuard {
	guard, _ := ctx.Value(ctxToolDispatchGuard).(ToolDispatchGuard)
	return guard
}

// ToolNameFromContext returns the concrete tool currently being considered by
// a per-dispatch guard. It is intentionally set only at the immediate dispatch
// boundary, after the model response has already been generated.
func ToolNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(ctxToolName).(string)
	return name
}

// WithAgentContext 将 agentID + taskID + loopNum 注入 context，
// 供 llm_executor 和工具调用层（local_write 等）记录日志和 trace 事件使用。
// 在 agent.processTask 的循环中每轮调用一次，更新 loopNum。
func WithAgentContext(ctx context.Context, agentID, taskID string, loopNum int) context.Context {
	ctx = context.WithValue(ctx, ctxAgentID, agentID)
	ctx = context.WithValue(ctx, ctxTaskID, taskID)
	ctx = context.WithValue(ctx, ctxLoopNum, loopNum)
	return ctx
}

// WithExecutionIdentity 注入本轮稳定 Run/Attempt/Turn identity。与
// WithAgentContext 分离，保持工具测试和 legacy 调用方兼容。
func WithExecutionIdentity(ctx context.Context, runID, attemptID, turnID string) context.Context {
	ctx = context.WithValue(ctx, ctxRunID, runID)
	ctx = context.WithValue(ctx, ctxAttemptID, attemptID)
	ctx = context.WithValue(ctx, ctxTurnID, turnID)
	return ctx
}

func executionIdentityFromContext(ctx context.Context) (runID, attemptID, turnID string) {
	runID, _ = ctx.Value(ctxRunID).(string)
	attemptID, _ = ctx.Value(ctxAttemptID).(string)
	turnID, _ = ctx.Value(ctxTurnID).(string)
	return
}

// WithActivityContext injects the best-effort live activity tracker used by the
// TUI. It is optional; executor behavior is unchanged when absent.
func WithActivityContext(ctx context.Context, tracker *ActivityTracker) context.Context {
	return context.WithValue(ctx, ctxActivity, tracker)
}

// WithCancelSource 标记当前 context 的取消来源，供 processTask 在
// KindTaskCancelled trace 事件中填充 Transition.CancelSource。
func WithCancelSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, ctxCancelSource, source)
}

// TaskIDFromContext 从 context 中提取当前任务 ID。
// 工具实现可调用此函数来 emit 包含 task_id 的 trace 事件。
// 不在 agent 包外也能使用——通过 trace 事件的 TaskID 字段实现解耦。
func TaskIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxTaskID).(string)
	return id
}

// AgentIDFromContext 从 context 中提取当前代理 ID。
func AgentIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxAgentID).(string)
	return id
}

// CancelSourceFromContext 从 context 中提取取消来源。
func CancelSourceFromContext(ctx context.Context) string {
	source, _ := ctx.Value(ctxCancelSource).(string)
	return source
}

func activityFromContext(ctx context.Context) *ActivityTracker {
	tracker, _ := ctx.Value(ctxActivity).(*ActivityTracker)
	return tracker
}

// truncateForLog 将参数截断为日志友好的短字符串。
func truncateForLog(args map[string]any, maxLen int) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	s := string(b)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// LLMExecutor 是基于 LLM 的 TaskExecutor 实现：每次 Execute 对应 ReAct 循环中
// 的一步（调用 LLM → 有 tool calls 则串行执行并返回 ToolCalled=true，否则
// ToolCalled=false 表示任务完成）。
//
// 它持有"当前生效"的工具注册表，支持按任务换入/恢复过滤视图——这是 per-node
// 能力（model.NodeCapability.Tools）在执行面的落点：processTask 在任务声明了
// 工具子集时经 SwapToolRegistry 换入 ToolRegistry.Filtered 视图，任务结束恢复。
// Agent 串行处理任务，正常路径无并发竞争；toolsMu 仅作防御性保护。
type LLMExecutor struct {
	client         llm.Invoker
	gateReg        *gate.Registry
	recordToolCall func(string, store.ToolCallRecord)
	// durableToolCallRecorder 是生产 L3 账本入口；非 nil 时优先于旧 void
	// callback，任何写失败都会终止剩余工具。旧 callback 仅供 legacy 测试。
	durableToolCallRecorder func(string, store.ToolCallRecord) error
	instructions            contextruntime.Instructions
	observationModel        string
	controlCapabilities     *controlcapability.Store
	toolsMu                 sync.RWMutex
	tools                   *ToolRegistry
	// frameworkTools 是启动装配期注册全集的只读 authority。任务级
	// ExecutionLease 只替换 tools 业务视图；Observation 等 framework-owned
	// Control Invocation 必须从这里按 exact phase 重新派生，不能依赖角色业务
	// Lease 是否暴露该工具，也不能把注册全集泄露回普通业务轮。
	frameworkTools *ToolRegistry
	// finalizationChecker 是 finalizing fence 的状态源（runner 装配注入与
	// submit_task_result 提交通道共享的 FinalizationHolder）。非 nil 时，
	// 每次具体工具 dispatch 前检查：已 finalized（submit_task_result 被接受）
	// 则跳过本次调用——不 dispatch、不产生副作用，返回「已跳过」提示文本并
	// emit tool_call_skipped。nil 时无 fence（兼容旧装配与 scheduler 路径）。
	finalizationChecker FinalizationChecker
	// sug / sugMu 是 V6 §4 H2a 的 per-task 建议状态（重复熔断计数 + 待判定
	// 建议），任务切换时整体重置；实现见 suggestions.go。
	sugMu sync.Mutex
	sug   *suggestionTracker
	// promptVersion 是交付 L2 的角色指令来源身份：system prompt 的
	// 来源版本（runner=system_prompt_file 内容 sha256 前 12，scheduler=
	// 内嵌常量版本，team 模板=模板 Version）。装配期经 SetPromptVersion
	// 设置一次，之后只读（与 instructions 同为构造期冻结材料）。
	promptVersion string
	// phasePromptResolver 为生产 Scheduler 把当前 ToolRouter phase 映射成
	// 本轮 L2 task_control_context。核心角色材料在启动期注入；阶段契约
	// 每轮随 ToolRouter snapshot 一起冻结，不允许形成第二条消息装配路径。
	phasePromptResolver func(string) string
	// invSeq 为每次 Execute 生成单调序号，拼入 <turnID>/invocation-<seq>。
	// Attempt/Turn 身份必须由调用方显式提供。
	invSeq atomic.Uint64
	// contextRuntime 是 L2 唯一编译、快照和调用记录路径；依赖缺失时拒绝调用。
	// lastSnapshotByAttempt 形成同 Attempt 的不可变父快照链。
	contextRuntime        contextruntime.Runtime
	contextMu             sync.Mutex
	lastSnapshotByAttempt map[string]string
}

func (e *LLMExecutor) SetDurableToolCallRecorder(recorder func(string, store.ToolCallRecord) error) {
	e.toolsMu.Lock()
	defer e.toolsMu.Unlock()
	e.durableToolCallRecorder = recorder
}

func (e *LLMExecutor) recordToolCallFact(taskID string, record store.ToolCallRecord) error {
	e.toolsMu.RLock()
	strict := e.durableToolCallRecorder
	legacy := e.recordToolCall
	e.toolsMu.RUnlock()
	if strict != nil {
		return strict(taskID, record)
	}
	if legacy != nil {
		legacy(taskID, record)
	}
	return nil
}

// SetContextRuntime 在启动装配期注入 L2 production authority。
func (e *LLMExecutor) SetContextRuntime(runtime contextruntime.Runtime) {
	e.contextMu.Lock()
	defer e.contextMu.Unlock()
	e.contextRuntime = runtime
	if e.lastSnapshotByAttempt == nil {
		e.lastSnapshotByAttempt = make(map[string]string)
	}
}

func (e *LLMExecutor) contextRuntimeForAttempt(attemptID string) (contextruntime.Runtime, string) {
	e.contextMu.Lock()
	defer e.contextMu.Unlock()
	return e.contextRuntime, e.lastSnapshotByAttempt[attemptID]
}

func (e *LLMExecutor) rememberContextSnapshot(attemptID, snapshotID string) {
	if attemptID == "" || snapshotID == "" {
		return
	}
	e.contextMu.Lock()
	defer e.contextMu.Unlock()
	if e.lastSnapshotByAttempt == nil {
		e.lastSnapshotByAttempt = make(map[string]string)
	}
	e.lastSnapshotByAttempt[attemptID] = snapshotID
}

// SetPromptVersion 注入交付 L2 的角色指令来源版本，装配方在构造后调用一次。
func (e *LLMExecutor) SetPromptVersion(version string) {
	e.promptVersion = version
}

func (e *LLMExecutor) SetPhasePromptResolver(resolver func(string) string) {
	e.phasePromptResolver = resolver
}

// SetFinalizationChecker 注入 finalizing fence 的状态源。runner 在构造
// executor 后调用一次；nil 表示关闭 fence（默认）。
func (e *LLMExecutor) SetFinalizationChecker(checker FinalizationChecker) {
	e.toolsMu.Lock()
	defer e.toolsMu.Unlock()
	e.finalizationChecker = checker
}

// finalizing 报告当前是否处于收尾态（finalization fence 判定）：checker 已
// 装配且已 finalized 时为 true。与 ToolRegistry 同一把锁读取，保证与换入的
// 工具视图一致的快照语义。
func (e *LLMExecutor) finalizing() bool {
	e.toolsMu.RLock()
	defer e.toolsMu.RUnlock()
	return e.finalizationChecker != nil && e.finalizationChecker.IsFinalized()
}

// ToolRegistrySwapper 是支持按任务替换工具注册表的 executor 能力接口
// （当前唯一实现是 *LLMExecutor）。processTask 只依赖本接口，不依赖具体类型。
type ToolRegistrySwapper interface {
	// SwapToolRegistry 原子替换当前生效的工具注册表，返回被替换的旧 registry，
	// 供调用方在任务边界恢复。
	SwapToolRegistry(reg *ToolRegistry) (old *ToolRegistry)
	// ToolRegistry 返回当前生效的工具注册表（可能是上一任务边界换入的过滤视图；
	// processTask 在任务入口调用时即为该 agent 的完整注册集）。
	ToolRegistry() *ToolRegistry
}

// 编译期断言：*LLMExecutor 必须实现 ToolRegistrySwapper。
var _ ToolRegistrySwapper = (*LLMExecutor)(nil)

// SwapToolRegistry 实现 ToolRegistrySwapper。reg 为 nil 是编程错误（会让后续
// Execute  nil 解引用），此处直接拒绝并保持原 registry。
func (e *LLMExecutor) SwapToolRegistry(reg *ToolRegistry) (old *ToolRegistry) {
	if reg == nil {
		return e.ToolRegistry()
	}
	e.toolsMu.Lock()
	defer e.toolsMu.Unlock()
	old = e.tools
	e.tools = reg
	return old
}

// ToolRegistry 实现 ToolRegistrySwapper。
func (e *LLMExecutor) ToolRegistry() *ToolRegistry {
	e.toolsMu.RLock()
	defer e.toolsMu.RUnlock()
	return e.tools
}

// SetObservationModel freezes the optional control-lane model at runner setup.
func (e *LLMExecutor) SetObservationModel(model string) {
	e.observationModel = strings.TrimSpace(model)
}

func (e *LLMExecutor) SetControlCapabilityStore(store *controlcapability.Store) {
	e.controlCapabilities = store
}

// invocationToolRegistries 原子取得当前任务业务视图与启动期 framework
// authority。两者只供一次 Invocation 冻结 ToolRouter，返回后均按只读使用。
func (e *LLMExecutor) invocationToolRegistries() (business, framework *ToolRegistry) {
	e.toolsMu.RLock()
	defer e.toolsMu.RUnlock()
	return e.tools, e.frameworkTools
}

// NewTurnExecutor 注入 L3 工具执行依赖和完整的 L2 服务；不提供缺省上下文构造。
func NewTurnExecutor(client llm.Invoker, tools *ToolRegistry, gates *gate.Registry, recorder func(string, store.ToolCallRecord), runtime contextruntime.Runtime, instructions contextruntime.Instructions) *LLMExecutor {
	return &LLMExecutor{client: client, tools: tools, frameworkTools: tools, gateReg: gates, recordToolCall: recorder, contextRuntime: runtime, instructions: instructions}
}

// Execute 实现 TaskExecutor 签名（方法值可直接赋给 Agent.Execute）。
func (e *LLMExecutor) Execute(ctx context.Context, task *model.Task, depResults map[string]string, history []contextcontract.HistoryEntry, actionBudget llm.OutputBudget) (ExecuteResult, error) {
	// 整个 Execute 使用同一份 registry 快照——任务边界换入的过滤视图对本次
	// 调用自洽，不会在模型调用与工具分发之间被换走。
	tools, frameworkTools := e.invocationToolRegistries()
	toolPolicy := deriveInvocationToolPolicyWithControl(task, history, tools, frameworkTools)
	toolRouter, err := FreezeToolRouterSnapshotWithPolicy(toolPolicy.Registry, toolPolicy.Phase, toolPolicy.MaxCalls)
	if err != nil {
		return ExecuteResult{}, err
	}
	{
		agentIDForTrace, _ := ctx.Value(ctxAgentID).(string)
		loopForTrace, _ := ctx.Value(ctxLoopNum).(int)
		runIDForTrace, attemptIDForTrace, turnIDForTrace := executionIdentityFromContext(ctx)
		// 每次 Execute（= 一次 LLM 调用）使用 Attempt/Turn lineage + executor
		// 单调序号生成身份；不能只用 task 前缀/loop，否则进程重启或新 Attempt
		// 会撞 ContextSnapshotStore 的 Invocation 唯一键。
		if attemptIDForTrace == "" || turnIDForTrace == "" {
			return ExecuteResult{}, fmt.Errorf("L3 模型调用缺少 Attempt/Turn 身份")
		}
		invocationBase := turnIDForTrace
		invocationID := fmt.Sprintf("%s/invocation-%d", invocationBase, e.invSeq.Add(1))
		if toolPolicy.RecoveryGate != nil {
			gate := toolPolicy.RecoveryGate
			trace.Emit(trace.Event{
				Kind: trace.KindRecoveryActionGated, TaskID: task.ID,
				RunID: string(task.RunID), AttemptID: attemptIDForTrace, TurnID: turnIDForTrace,
				InvocationID: invocationID, AgentID: agentIDForTrace,
				RecoveryGate: &trace.RecoveryActionPayload{
					Schema: gate.Schema, Stage: string(gate.Stage), Tool: gate.Tool,
					Path: gate.Path, CheckID: gate.CheckID, RefID: gate.RefID,
					Offset: gate.Offset, Limit: gate.Limit, ForceFull: gate.ForceFull,
					DirectiveCount: gate.DirectiveCount,
				},
			})
		}
		activity := activityFromContext(ctx)
		contextRuntime, parentSnapshotRef := e.contextRuntimeForAttempt(attemptIDForTrace)
		phasePrompt := ""
		if e.phasePromptResolver != nil {
			phasePrompt = e.phasePromptResolver(toolRouter.Phase)
		}
		if toolRouter.Phase == "agent:deliverable-submit" {
			phasePrompt = agentDeliverablePhasePrompt
		} else if isObservationCheckpointPhase(toolRouter.Phase) {
			phasePrompt = observationCheckpointPhasePrompt + "\n" + observationCheckpointCatalogPrompt(toolRouter.Defs)
		} else if toolRouter.Phase == "agent:observation-commitment" {
			phasePrompt = observationCommitmentPhasePrompt
		} else if toolPolicy.RecoveryGate != nil {
			phasePrompt = recoveryActionPhasePrompt(*toolPolicy.RecoveryGate)
		} else if toolRouter.Phase == "scheduler:final-report-submit" {
			phasePrompt = finalReportSubmitPhasePrompt
		}
		// 正常业务链不重放 reasoning=none 的 Observation Control Invocation；
		// 其 durable 结果由 TaskMemory 独立注入。
		historyMode := "business"
		if toolRouter.Phase == "agent:deliverable-submit" && task.ProgressContract != nil && task.ProgressContract.WorkClass == loopcontract.WorkInvestigation {
			historyMode = "investigation-evidence"
		} else if toolRouter.Phase == "agent:deliverable-submit" || isObservationCheckpointPhase(toolRouter.Phase) || toolRouter.Phase == "scheduler:final-report-submit" {
			historyMode = "control"
		}
		if err := actionBudget.Validate(); err != nil {
			return ExecuteResult{}, err
		}
		limit := actionBudget.Clone()
		if int64(toolRouter.MaxCalls) < limit.MaxToolCalls {
			limit.MaxToolCalls = int64(toolRouter.MaxCalls)
		}
		if isObservationPhase(toolRouter.Phase) {
			completion, responseBytes := observationOutputLimits(toolRouter.Phase)
			limit.MaxCompletionTokens = minPositiveInt64(limit.MaxCompletionTokens, completion)
			limit.MaxContentBytes = minPositiveInt64(limit.MaxContentBytes, responseBytes)
			limit.MaxReasoningBytes = minPositiveInt64(limit.MaxReasoningBytes, responseBytes)
			limit.MaxExtraFieldBytes = minPositiveInt64(limit.MaxExtraFieldBytes, responseBytes)
			limit.MaxToolArgumentsBytes = minPositiveInt64(limit.MaxToolArgumentsBytes, 16<<10)
			limit.MaxToolArgumentsTotalBytes = minPositiveInt64(limit.MaxToolArgumentsTotalBytes, 16<<10)
			limit.MaxResponseBytes = minPositiveInt64(limit.MaxResponseBytes, responseBytes)
			if toolRouter.Phase == "agent:observation-checkpoint" {
				limit.MaxToolCalls = 1
			}
			for name, n := range limit.MaxExtraFieldBytesByName {
				limit.MaxExtraFieldBytesByName[name] = minPositiveInt64(n, responseBytes)
			}
		}
		if toolPolicy.RecoveryGate != nil && toolPolicy.RecoveryGate.Schema == graph.RecoveryDeltaSchemaV5 {
			limit.MaxCompletionTokens = minPositiveInt64(limit.MaxCompletionTokens, recoveryV5CompletionLimit(toolPolicy.RecoveryGate.Stage, history))
		}
		input := executionContextInput(contextRuntime, task, depResults, history, toolRouter, limit)
		input.Identity.InvocationID = invocationID
		input.Identity.AttemptID = attemptIDForTrace
		input.Identity.TurnID = turnIDForTrace
		input.Identity.AgentID = agentIDForTrace
		input.Identity.Loop = loopForTrace
		input.ParentSnapshotRef = parentSnapshotRef
		input.HistoryMode = historyMode
		input.Instructions = contextruntime.Instructions{ProfileID: e.promptVersion, System: e.instructions.System, Override: task.SystemPrompt, Team: e.instructions.Team, Objective: task.Description, Control: renderTaskContextBlock(task), Output: renderOutputContract(task, taskControlTools(task)), Phase: phasePrompt}
		compiled, compileErr := contextRuntime.Compile(ctx, input)
		if compileErr != nil {
			return ExecuteResult{InvocationID: invocationID}, contextAssemblyFailure(ctx, invocationID, task.ContextPolicyRef, compileErr)
		}
		snapshot := compiled.Snapshot()
		spec := compiled.Request().Spec()
		messages, toolDefs := spec.Messages, spec.Tools
		invocationBinding := &spec.Options
		contextSnapshotID, contextPolicyRef := snapshot.SnapshotID, snapshot.ContextPolicyID
		manifestTokens := int(snapshot.Manifest.Usage.EstimatedTokens)
		manifestRaw, _ := json.Marshal(snapshot.Manifest.Items)
		manifestDescription := string(manifestRaw)
		e.rememberContextSnapshot(attemptIDForTrace, contextSnapshotID)
		manifestEv := trace.Event{
			Kind:                 trace.KindContextManifestBuilt,
			TaskID:               task.ID,
			RunID:                runIDForTrace,
			AttemptID:            attemptIDForTrace,
			TurnID:               turnIDForTrace,
			AgentID:              agentIDForTrace,
			Loop:                 loopForTrace,
			InvocationID:         invocationID,
			ToolRouterSnapshotID: toolRouter.ID,
			ContextSnapshotID:    contextSnapshotID,
			ContextPolicyRef:     contextPolicyRef,
			PromptTokens:         manifestTokens,
			HistoryEntries:       len(history),
			Description:          manifestDescription,
		}
		// 按调用身份发布已持久化的 L2 快照摘要，不包含提示词正文。
		trace.Emit(manifestEv)
		activity.LLMStart(agentIDForTrace, task.ID, loopForTrace, len(toolDefs))

		// Trace：LLM 调用开始
		toolChoiceMode, toolChoiceName, reasoningEffort := "", "", ""
		modelCapabilityDigest, invocationProfileRef := "", ""
		effectiveModel := ""
		if task.Lease != nil {
			effectiveModel = task.Lease.Model
		}
		if isAutoObservationPhase(toolRouter.Phase) && e.observationModel != "" {
			effectiveModel = e.observationModel
		}
		if invocationBinding != nil {
			toolChoiceMode = string(invocationBinding.ToolChoice.Mode)
			toolChoiceName = invocationBinding.ToolChoice.Name
			reasoningEffort = invocationBinding.ReasoningEffort
			if invocationBinding.Model != "" {
				effectiveModel = invocationBinding.Model
			}
			modelCapabilityDigest = invocationBinding.CapabilityDigest
			invocationProfileRef = invocationBinding.ProfileRef
		}
		var controlCapabilityKey controlcapability.Key
		if isAutoObservationPhase(toolRouter.Phase) && invocationBinding != nil {
			controlCapabilityKey = controlcapability.Key{
				RunID: runIDForTrace, EffectiveModel: effectiveModel,
				InvocationProfile: toolRouter.Phase, ToolSchemaDigest: toolRouter.ID,
			}
			if record, incompatible := e.controlCapabilities.Incompatible(controlCapabilityKey); incompatible {
				return ExecuteResult{InvocationID: invocationID, ContextSnapshotID: contextSnapshotID},
					&controlcapability.IncompatibleError{Record: record}
			}
		}
		trace.Emit(trace.Event{
			Kind:                  trace.KindLLMCallStart,
			TaskID:                task.ID,
			RunID:                 runIDForTrace,
			AttemptID:             attemptIDForTrace,
			TurnID:                turnIDForTrace,
			AgentID:               agentIDForTrace,
			Loop:                  loopForTrace,
			InvocationID:          invocationID,
			ToolRouterSnapshotID:  toolRouter.ID,
			ContextSnapshotID:     contextSnapshotID,
			ContextPolicyRef:      contextPolicyRef,
			HistoryEntries:        len(history),
			ToolCallsCount:        len(toolDefs),
			ToolChoiceMode:        toolChoiceMode,
			ToolChoiceName:        toolChoiceName,
			ReasoningEffort:       reasoningEffort,
			EffectiveModel:        effectiveModel,
			ModelCapabilityDigest: modelCapabilityDigest,
			InvocationProfileRef:  invocationProfileRef,
		})
		// Prompt dump（仅在 --dump-prompts 启用时写入）
		trace.DumpRequest(task.ID, loopForTrace, messages, len(toolDefs))

		llmStart := time.Now()
		invocationTiming := llm.NewInvocationTiming(llmStart)
		invokeCtx := llm.WithInvocationTiming(ctx, invocationTiming)
		resp, err := contextRuntime.InvokeCompiled(invokeCtx, compiled, e.client)
		llmDuration := time.Since(llmStart)
		traceTiming := traceInvocationTiming(invocationTiming.Snapshot())

		if err != nil {
			if failure, ok := llm.FromError(err); ok && controlCapabilityKey.RunID != "" {
				if _, storeErr := e.controlCapabilities.Mark(controlCapabilityKey, failure); storeErr != nil {
					return ExecuteResult{InvocationID: invocationID, ContextSnapshotID: contextSnapshotID, ContextProjected: compiled.Projected(),
							InvocationDuration: llmDuration, ProviderCallStarted: true},
						contextAssemblyFailure(ctx, invocationID, contextPolicyRef,
							fmt.Errorf("持久化 ControlCapability 失败: %w", storeErr))
				}
			}
			activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, "", 0, err)
			event := trace.Event{
				Kind:                  trace.KindLLMCallEnd,
				TaskID:                task.ID,
				RunID:                 runIDForTrace,
				AttemptID:             attemptIDForTrace,
				TurnID:                turnIDForTrace,
				AgentID:               agentIDForTrace,
				Loop:                  loopForTrace,
				InvocationID:          invocationID,
				ToolRouterSnapshotID:  toolRouter.ID,
				ContextSnapshotID:     contextSnapshotID,
				ContextPolicyRef:      contextPolicyRef,
				DurationMS:            llmDuration.Milliseconds(),
				Error:                 err.Error(),
				EffectiveModel:        effectiveModel,
				ModelCapabilityDigest: modelCapabilityDigest,
				InvocationProfileRef:  invocationProfileRef,
				LLMTiming:             traceTiming,
			}
			if failure, ok := llm.FromError(err); ok {
				failure.InvocationID = invocationID
				failure.SnapshotID = contextSnapshotID
				failure.ProviderPolicy = contextPolicyRef
				event.FailureKind = string(failure.Kind)
				event.FailurePhase = string(failure.Phase)
				event.FailureOrigin = string(failure.Origin)
				event.TimeoutScope = string(failure.TimeoutScope)
				event.ProviderCode = failure.ProviderCode
				event.HTTPStatus = failure.HTTPStatus
				event.UsageState = string(failure.UsageState)
				event.Partial = failure.Partial
				event.FinishReason = failure.FinishReason
			}
			trace.Emit(event)
			return ExecuteResult{InvocationID: invocationID, ContextSnapshotID: contextSnapshotID, ContextProjected: compiled.Projected(),
				InvocationDuration: llmDuration, ProviderCallStarted: true}, classifyError(err)
		}

		batchErr := validateToolCallBatch(toolRouter, resp.ToolCalls())
		if batchErr == nil && toolPolicy.RecoveryGate != nil {
			batchErr = validateRecoveryActionCall(*toolPolicy.RecoveryGate, resp.ToolCalls())
		}
		if batchErr != nil {
			failureKind := llm.FailureMalformedResponse
			origin := llm.OriginProtocol
			if isActionContractViolation(batchErr) {
				failureKind = llm.FailureActionContractRejected
				origin = llm.OriginRuntime
			}
			failure := llm.NewFailure(failureKind,
				llm.PhaseToolCallValidate, origin, batchErr)
			failure.UsageState = llm.UsageSettled
			failure.InvocationID = invocationID
			failure.SnapshotID = contextSnapshotID
			failure.ProviderPolicy = contextPolicyRef
			activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, "", 0, failure)
			trace.Emit(trace.Event{
				Kind: trace.KindLLMCallEnd, TaskID: task.ID, RunID: runIDForTrace,
				AttemptID: attemptIDForTrace, TurnID: turnIDForTrace, AgentID: agentIDForTrace,
				Loop: loopForTrace, InvocationID: invocationID,
				ToolRouterSnapshotID: toolRouter.ID, ContextSnapshotID: contextSnapshotID,
				ContextPolicyRef: contextPolicyRef, DurationMS: llmDuration.Milliseconds(),
				PromptTokens: resp.Data().Usage.PromptTokens, CompletionTokens: resp.Data().Usage.CompletionTokens,
				ReasoningTokens: resp.Data().Usage.ReasoningTokens,
				ToolCallsCount:  len(resp.ToolCalls()), Error: failure.Error(),
				FailureKind: string(failure.Kind), FailurePhase: string(failure.Phase),
				FailureOrigin: string(failure.Origin), UsageState: string(failure.UsageState),
				EffectiveModel:        effectiveModel,
				ModelCapabilityDigest: modelCapabilityDigest, InvocationProfileRef: invocationProfileRef,
				LLMTiming: traceTiming,
			})
			return ExecuteResult{
				InvocationID: invocationID, ContextSnapshotID: contextSnapshotID, ContextProjected: compiled.Projected(),
				InvocationDuration: llmDuration, ProviderCallStarted: true,
				PromptTokens:     resp.Data().Usage.PromptTokens,
				CompletionTokens: resp.Data().Usage.CompletionTokens,
			}, failure
		}

		// Trace：LLM 调用成功结束
		trace.Emit(trace.Event{
			Kind:                  trace.KindLLMCallEnd,
			TaskID:                task.ID,
			RunID:                 runIDForTrace,
			AttemptID:             attemptIDForTrace,
			TurnID:                turnIDForTrace,
			AgentID:               agentIDForTrace,
			Loop:                  loopForTrace,
			InvocationID:          invocationID,
			ToolRouterSnapshotID:  toolRouter.ID,
			ContextSnapshotID:     contextSnapshotID,
			ContextPolicyRef:      contextPolicyRef,
			DurationMS:            llmDuration.Milliseconds(),
			PromptTokens:          resp.Data().Usage.PromptTokens,
			CompletionTokens:      resp.Data().Usage.CompletionTokens,
			ReasoningTokens:       resp.Data().Usage.ReasoningTokens,
			ToolCallsCount:        len(resp.ToolCalls()),
			EffectiveModel:        effectiveModel,
			ModelCapabilityDigest: modelCapabilityDigest,
			InvocationProfileRef:  invocationProfileRef,
			LLMTiming:             traceTiming,
		})
		// CM1 对账：Manifest 估算 tokens 与实测值对照，只记录不告警
		//（估算口径 rune/3，偏差供后续校准估算系数参考）。
		log.Printf("[agent %s] task=%s loop=%d manifest 估算 prompt tokens=%d，实测=%d，偏差=%+d",
			agentIDForTrace, task.ID, loopForTrace, manifestTokens,
			resp.Data().Usage.PromptTokens, resp.Data().Usage.PromptTokens-manifestTokens)
		trace.DumpResponse(task.ID, loopForTrace, resp.Content(), resp.ToolCalls(), resp.Data().Usage.PromptTokens, resp.Data().Usage.CompletionTokens)
		activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, resp.Content(), len(resp.ToolCalls()), nil)

		// 无 tool calls → 任务完成
		if len(resp.ToolCalls()) == 0 {
			return ExecuteResult{
				InvocationID:      invocationID,
				ContextSnapshotID: contextSnapshotID, ContextProjected: compiled.Projected(),
				InvocationDuration:  llmDuration,
				ProviderCallStarted: true,
				Output:              resp.Content(),
				AssistantContent:    resp.Content(),
				Reasoning:           resp.Reasoning(),
				ToolCalled:          false,
				PromptTokens:        resp.Data().Usage.PromptTokens,
				CompletionTokens:    resp.Data().Usage.CompletionTokens,
				Replay:              resultReplay(resp),
			}, nil
		}

		// Tool calls execute in the model-provided order. Agent tools include
		// stateful and side-effecting operations, so parallel dispatch would let
		// report_done/finalize/mark_blocked race with writes or shell commands.
		// Serial dispatch also makes each prior ToolCallRecord visible to the next
		// call and gives a Plan guard an actual boundary between calls.
		type indexedResult struct {
			toolResult contextcontract.ToolResult
			output     string
		}

		agentID, _ := ctx.Value(ctxAgentID).(string)
		loopNum, _ := ctx.Value(ctxLoopNum).(int)
		// H2a：本任务的建议跟踪器（重复熔断计数 + 待判定建议）；任务切换时
		// 由 suggestionsForTask 整体重置，per-task 计数任务结束即弃。
		sugTrack := e.suggestionsForTask(task.ID)

		results := make([]indexedResult, len(resp.ToolCalls()))
		completedResults := 0
		var controlErr error
		for i, call := range resp.ToolCalls() {
			func(idx int, c llm.ToolCall) {
				actionID := turnIDForTrace + "/tool-" + c.ID
				if turnIDForTrace == "" {
					actionID = task.ID + "/legacy-tool-" + c.ID
				}
				// auto + singleton 是 DeepSeek thinking 可消费的 provider wire
				// 表达。某些 thinking provider 会忽略 parallel_tool_calls=false
				// 并返回重复调用。阶段权威仍只允许一个动作：执行首个，
				// 为后续 call_id 生成 skipped result 以保持 Responses 无状态重放完整。
				if idx > 0 && phaseDispatchesOnlyFirstTool(toolRouter.Phase) {
					content := "已跳过：当前机械阶段只执行 provider 顺序中的首个工具调用"
					trace.Emit(trace.Event{
						Kind: trace.KindToolCallSkipped, TaskID: task.ID, RunID: runIDForTrace,
						AttemptID: attemptIDForTrace, TurnID: turnIDForTrace, ActionID: actionID,
						AgentID: agentID, Loop: loopNum, Tool: c.Name, CallID: c.ID,
						Reason: "phase_single_action_fanout",
					})
					results[idx] = indexedResult{toolResult: contextcontract.ToolResult{
						ToolCallID: c.ID, Content: content,
					}, output: fmt.Sprintf("[%s] %s\n", c.Name, content)}
					completedResults = idx + 1
					return
				}
				// finalizing fence：submit_task_result 被接受（MarkTaskFinalized）
				// 后，同一响应中排在其后的工具调用一律跳过——不 dispatch、不产生
				// 副作用、不写 ToolCallRecord（调用从未发生），只返回结构化提示
				// 文本并 emit tool_call_skipped 审计事件。工具按序串行执行，
				// 前一个调用置上的 finalized 标志对后续调用即时可见。
				if e.finalizing() {
					content := "已跳过：任务已进入收尾（finalizing），本次调用未执行"
					log.Printf("[agent %s] task=%s loop=%d tool=%s 被 finalizing fence 跳过（call_id=%s）", agentID, task.ID, loopNum, c.Name, c.ID)
					trace.Emit(trace.Event{
						Kind:      trace.KindToolCallSkipped,
						TaskID:    task.ID,
						RunID:     runIDForTrace,
						AttemptID: attemptIDForTrace,
						TurnID:    turnIDForTrace,
						ActionID:  actionID,
						AgentID:   agentID,
						Loop:      loopNum,
						Tool:      c.Name,
						CallID:    c.ID,
						Reason:    "task_finalizing",
					})
					results[idx] = indexedResult{
						toolResult: contextcontract.ToolResult{
							ToolCallID: c.ID,
							Content:    content,
						},
						output: fmt.Sprintf("[%s] %s\n", c.Name, content),
					}
					completedResults = idx + 1
					return
				}

				// L3 参数规范化必须先于 Trace/Gate/Effect/账本。只有 Tool Registry
				// 显式声明的安全默认值会被填入；不得从正文或别名猜参数。
				c = toolRouter.Registry.NormalizeCall(c)

				argsLog := truncateForLog(c.Arguments, 120)
				log.Printf("[agent %s] task=%s loop=%d tool=%s args=%s", agentID, task.ID, loopNum, c.Name, argsLog)
				activity.ToolStarted(agentID, task.ID, loopNum, c.ID, c.Name)
				// Trace：工具调用开始。V6 §7.4：Args 过默认脱敏（结构字段保留、
				// 自由内容替换为 <redacted> 占位；AGENTGO_TRACE_FULL_ARGS=1 可旁路），
				// 原 c.Arguments 不受影响，继续参与 Gate / dispatch / ToolCallRecord。
				trace.Emit(trace.Event{
					Kind:      trace.KindToolCall,
					TaskID:    task.ID,
					RunID:     runIDForTrace,
					AttemptID: attemptIDForTrace,
					TurnID:    turnIDForTrace,
					ActionID:  actionID,
					AgentID:   agentID,
					Loop:      loopNum,
					Tool:      c.Name,
					Args:      trace.RedactArgs(c.Name, c.Arguments),
					CallID:    c.ID,
				})

				// Gate pre-call：允许注册的 Gate 拒绝本次调用。
				// gateReg 为 nil 时 Dispatch 直接返回 Continue（nil receiver 安全）。
				preDecision := e.gateReg.Dispatch(&gate.ToolContext{
					CtxField:     ctx,
					PhaseField:   gate.PhaseToolPreCall,
					AgentIDField: agentID,
					TaskIDField:  task.ID,
					ToolName:     c.Name,
					Args:         c.Arguments,
				})

				start := time.Now()
				var result string
				var toolErr error
				var actionBoundary toolActionBoundary
				var actionHandle toolActionHandle
				dispatched := false
				if preDecision.Action == gate.Abort {
					// Pre hook 拒绝 — 跳过实际工具调用，合成错误返回值。
					// 错误消息同时注入到 content 和 toolErr，让 LLM 和后续记录都看到。
					// H2a：结构化拒绝在此完成 pending 判定、过滤、熔断与文本构建
					// （无结构化字段的 Gate 走旧 [hook 拒绝] 文本路径）。
					result = ""
					toolErr = e.handleGateAbort(sugTrack, task.ID, agentID, loopNum, preDecision)
				} else {
					// 调用通过 Gate：判定上一轮建议的 disposition（adopted /
					// abandoned），随后照常 dispatch。
					e.resolvePendingOnPass(sugTrack, task.ID, agentID, loopNum, c)
					if guard := toolDispatchGuardFromContext(ctx); guard != nil {
						dispatchCtx := context.WithValue(ctx, ctxToolName, c.Name)
						if guardErr := guard(dispatchCtx, task); guardErr != nil {
							toolErr = fmt.Errorf("tool dispatch suspended: %w", guardErr)
						} else {
							actionBoundary = toolActionBoundaryFromContext(ctx)
							if actionBoundary != nil {
								actionHandle, toolErr = actionBoundary.ReserveTool(ctx, task, c)
								if toolErr != nil {
									controlErr = &loopAuthorityError{Err: toolErr}
									return
								}
							}
							result, toolErr = toolRouter.Registry.Dispatch(ctx, c)
							dispatched = true
						}
					} else {
						actionBoundary = toolActionBoundaryFromContext(ctx)
						if actionBoundary != nil {
							actionHandle, toolErr = actionBoundary.ReserveTool(ctx, task, c)
							if toolErr != nil {
								controlErr = &loopAuthorityError{Err: toolErr}
								return
							}
						}
						result, toolErr = toolRouter.Registry.Dispatch(ctx, c)
						dispatched = true
					}
				}
				if dispatched && actionBoundary != nil {
					if settleErr := actionBoundary.SettleTool(ctx, task, c, actionHandle, result, toolErr); settleErr != nil {
						controlErr = &loopAuthorityError{Err: settleErr}
					}
				}
				dur := time.Since(start)
				if toolErr == nil {
					boundedResult, persistErr := externalizeToolResult(contextRuntime, ctx, task, c, result)
					if persistErr != nil {
						controlErr = &loopAuthorityError{Err: persistErr}
						return
					}
					result = boundedResult
				}

				var content string
				if toolErr != nil {
					content = fmt.Sprintf("错误: %v", toolErr)
					log.Printf("[agent %s] task=%s loop=%d tool=%s duration=%s error=%v", agentID, task.ID, loopNum, c.Name, dur.Round(time.Millisecond), toolErr)
					trace.Emit(trace.Event{
						Kind:       trace.KindToolResult,
						TaskID:     task.ID,
						RunID:      runIDForTrace,
						AttemptID:  attemptIDForTrace,
						TurnID:     turnIDForTrace,
						ActionID:   actionID,
						AgentID:    agentID,
						Loop:       loopNum,
						Tool:       c.Name,
						Args:       trace.RedactArgs(c.Name, c.Arguments), // v5 Phase 6：与 KindToolCall 对称，让 Reactor 能读 args.path；V6 §7.4 起过默认脱敏（path 属保留字段）
						CallID:     c.ID,
						DurationMS: dur.Milliseconds(),
						Error:      toolErr.Error(),
					})
				} else {
					content = result
					log.Printf("[agent %s] task=%s loop=%d tool=%s duration=%s result_len=%d", agentID, task.ID, loopNum, c.Name, dur.Round(time.Millisecond), len(content))
					trace.Emit(trace.Event{
						Kind:       trace.KindToolResult,
						TaskID:     task.ID,
						RunID:      runIDForTrace,
						AttemptID:  attemptIDForTrace,
						TurnID:     turnIDForTrace,
						ActionID:   actionID,
						AgentID:    agentID,
						Loop:       loopNum,
						Tool:       c.Name,
						Args:       trace.RedactArgs(c.Name, c.Arguments), // v5 Phase 6：read-set-write Reactor 据此 filter 并拿 path；V6 §7.4 起过默认脱敏（path 属保留字段）
						CallID:     c.ID,
						DurationMS: dur.Milliseconds(),
						ResultLen:  len(content),
					})
				}
				activity.ToolFinished(agentID, task.ID, loopNum, c.ID, c.Name, toolErr)

				// 写入 ToolCallRecord（hookSystem.md §11.1.3）：
				//   - 时机：Dispatch 之后、RunPost 之前 —— 让 post hook 能通过
				//     GetToolCallHistory 看到刚刚结束的调用；pre hook 在 Dispatch
				//     之前看，避免"自己引用自己"
				//   - 写入范围：无论 pre hook Abort 还是真正执行都写，Success
				//     由 toolErr == nil 决定
				//   - Scheduler 工具不经过本路径，不被记录（hookSystem.md §11.1.3）
				var exitCode *int
				var exitCodeScope store.ShellExitCodeScope
				if c.Name == "run_shell" && toolErr == nil {
					exitCode = parseRunShellExitCode(result)
					exitCodeScope = parseRunShellExitCodeScope(result)
				}
				if recordErr := e.recordToolCallFact(task.ID, store.ToolCallRecord{
					Timestamp:     time.Now(),
					RunID:         runIDForTrace,
					AttemptID:     attemptIDForTrace,
					TurnID:        turnIDForTrace,
					ActionID:      actionID,
					CallID:        c.ID,
					AgentID:       agentID,
					ToolName:      c.Name,
					Args:          c.Arguments,
					Success:       toolErr == nil,
					ExitCode:      exitCode,
					ExitCodeScope: exitCodeScope,
				}); recordErr != nil {
					controlErr = &loopAuthorityError{Err: fmt.Errorf("ToolCallRecord durable 写失败: %w", recordErr)}
				}

				// Gate post-call：纯观察，Dispatch 返回值忽略。gateReg 为 nil 时无操作。
				_ = e.gateReg.Dispatch(&gate.ToolContext{
					CtxField:     ctx,
					PhaseField:   gate.PhaseToolPostCall,
					AgentIDField: agentID,
					TaskIDField:  task.ID,
					ToolName:     c.Name,
					Args:         c.Arguments,
					Result:       content,
					Err:          toolErr,
				})

				results[idx] = indexedResult{
					toolResult: contextcontract.ToolResult{
						ToolCallID: c.ID,
						Content:    content,
					},
					output: fmt.Sprintf("[%s] %s\n", c.Name, content),
				}
				completedResults = idx + 1
			}(i, call)
			if controlErr != nil {
				break
			}
		}

		// 按原始顺序组装输出和 toolResults
		var output strings.Builder
		toolResults := make([]contextcontract.ToolResult, completedResults)
		for i, r := range results[:completedResults] {
			output.WriteString(r.output)
			toolResults[i] = r.toolResult
		}

		completedCalls := append([]llm.ToolCall(nil), resp.ToolCalls()[:completedResults]...)
		executeResult := ExecuteResult{
			InvocationID:      invocationID,
			ContextSnapshotID: contextSnapshotID, ContextProjected: compiled.Projected(),
			InvocationDuration:  llmDuration,
			ProviderCallStarted: true,
			Output:              output.String(),
			ToolCalled:          completedResults > 0,
			AssistantContent:    resp.Content(),
			Reasoning:           resp.Reasoning(),
			ToolCalls:           completedCalls,
			ToolResults:         toolResults,
			PromptTokens:        resp.Data().Usage.PromptTokens,
			CompletionTokens:    resp.Data().Usage.CompletionTokens,
			Replay:              resultReplay(resp),
		}
		return executeResult, controlErr
	}
}

func isObservationCheckpointPhase(phase string) bool {
	return phase == "agent:observation-checkpoint" ||
		strings.HasPrefix(phase, "agent:observation-checkpoint-v")
}

func traceInvocationTiming(value llm.InvocationTimingSnapshot) *trace.LLMInvocationTiming {
	if !value.HasAny() {
		return nil
	}
	return &trace.LLMInvocationTiming{
		Schema: trace.LLMInvocationTimingSchemaV1,
		DNSMS:  value.DNSMS, ConnectMS: value.ConnectMS, TLSMS: value.TLSMS,
		FirstResponseByteMS: value.FirstResponseByteMS, FirstSSEEventMS: value.FirstSSEEventMS,
		FirstReasoningDeltaMS: value.FirstReasoningDeltaMS, FirstTextDeltaMS: value.FirstTextDeltaMS,
		FirstToolDeltaMS: value.FirstToolDeltaMS, FirstModelDeltaMS: value.FirstModelDeltaMS,
		CompletedMS: value.CompletedMS, MaxInterEventGapMS: value.MaxInterEventGapMS,
		StreamEventCount: value.StreamEventCount, ConnectAttempts: value.ConnectAttempts,
		ConnectFailures: value.ConnectFailures, NetworkFamily: value.NetworkFamily,
		ConnectionReused: value.ConnectionReused,
	}
}

func responseReplayFailure(cause error) *llm.Failure {
	kind := llm.FailureOutputLimitExceeded
	var assembly *contextcontract.ContextAssemblyFailure
	if errors.As(cause, &assembly) && assembly.Reason == contextcontract.AssemblyProviderReplayUnknown {
		kind = llm.FailureProtocolIncompatible
	}
	failure := llm.NewFailure(kind, llm.PhaseResponseValidate, llm.OriginRuntime, cause)
	failure.UsageState = llm.UsageSettled
	return failure
}

func parseRunShellExitCode(result string) *int {
	line, _, _ := strings.Cut(result, "\n")
	value, ok := strings.CutPrefix(strings.TrimSpace(line), "exit_code:")
	if !ok {
		return nil
	}
	code, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	return &code
}

func parseRunShellExitCodeScope(result string) store.ShellExitCodeScope {
	lines := strings.Split(result, "\n")
	for _, line := range lines[:min(len(lines), 3)] {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "exit_code_scope:")
		if !ok {
			continue
		}
		scope := store.ShellExitCodeScope(strings.TrimSpace(value))
		switch scope {
		case store.ShellExitCodeScopeWholeCommand, store.ShellExitCodeScopeLastPipelineCommand:
			return scope
		default:
			return ""
		}
	}
	return ""
}

func classifyError(err error) error {
	if _, ok := llm.FromError(err); ok {
		return err
	}
	var llmRecov *llm.ErrRecoverable
	if errors.As(err, &llmRecov) {
		return &ErrRecoverable{Err: err}
	}
	var llmBad *llm.ErrBadResponse
	if errors.As(err, &llmBad) {
		return &ErrRecoverable{Err: err}
	}
	return err
}

func contextAssemblyFailure(ctx context.Context, invocationID, policyRef string, cause error) *llm.Failure {
	kind := llm.FailureContextAssembly
	scope := llm.TimeoutNone
	origin := llm.OriginRuntime
	contextCause := context.Cause(ctx)
	switch {
	case errors.Is(contextCause, llm.ErrAttemptDeadline):
		kind, scope = llm.FailureAttemptDeadline, llm.TimeoutAttempt
	case errors.Is(contextCause, llm.ErrActivationDeadline):
		kind, scope = llm.FailureActivationDeadline, llm.TimeoutActivation
	case errors.Is(contextCause, llm.ErrGraphDeadline):
		kind, scope = llm.FailureActivationDeadline, llm.TimeoutGraph
	case errors.Is(contextCause, llm.ErrRunDeadline):
		kind, scope = llm.FailureActivationDeadline, llm.TimeoutRun
	case errors.Is(contextCause, context.Canceled):
		kind, scope, origin = llm.FailureCallerCancelled, llm.TimeoutCaller, llm.OriginCaller
	}
	failure := llm.NewFailure(kind, llm.PhaseRequestBuild, origin, cause)
	failure.TimeoutScope = scope
	failure.InvocationID = invocationID
	failure.ProviderPolicy = policyRef
	return failure
}
