package agent

import (
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
	"agentgo/internal/gate"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/prompt"
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
	// ctxManifestSideInfo 携带 processTask 每 attempt 一份的 Context Manifest
	// 侧信息（Memory 段 UpdatedAt、压缩处置回填），executor 构建 Manifest 时只读。
	ctxManifestSideInfo
	// ctxTaskMemCarrier 携带当前 Task Memory 的有界渲染（V6 §3 CM2），
	// executor 把它注入 messages 并在 Manifest 登记 task_memory 段。
	ctxTaskMemCarrier
	// ctxPromptBuild 携带 processTask 每个 attempt 编译并冻结的 Prompt
	// Build（V6 §2 P1a，值语义不可变），executor 把 Build.ID 并入每轮
	// context_manifest_built 事件的 prompt_build_id 字段。
	ctxPromptBuild
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
	client         llm.Client
	gateReg        *gate.Registry
	recordToolCall func(string, store.ToolCallRecord)
	// durableToolCallRecorder 是生产 L3 账本入口；非 nil 时优先于旧 void
	// callback，任何写失败都会终止剩余工具。旧 callback 仅供 legacy 测试。
	durableToolCallRecorder func(string, store.ToolCallRecord) error
	teamAwareness           string
	sysPrompt               string
	toolsMu                 sync.RWMutex
	tools                   *ToolRegistry
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
	// promptVersion 是 V6 §2 P1a prompt 编译的身份维度：system prompt 的
	// 来源版本（runner=system_prompt_file 内容 sha256 前 12，scheduler=
	// 内嵌常量版本，team 模板=模板 Version）。装配期经 SetPromptVersion
	// 设置一次，之后只读（与 sysPrompt 同为构造期冻结事实）。
	promptVersion string
	// phasePromptResolver 为生产 Scheduler 把当前 ToolRouter phase 映射成
	// 本轮 L2 task_control_context。核心 sysPrompt 仍按 Attempt 冻结；阶段契约
	// 每轮随 ToolRouter snapshot 一起冻结，不允许形成第二条消息装配路径。
	phasePromptResolver func(string) string
	// invSeq 是 V6 §7.2 InvocationID 的 executor 级单调序号：每次 Execute
	// （= 一次 LLM 调用）取一次，拼成 <taskID前8>-<loop>-<seq>。重试产生的
	// 同 (task,loop) 重复调用借 seq 区分。
	invSeq atomic.Uint64
	// contextRuntime 是 L2 唯一编译/持久化路径。生产必须注入；nil 仅用于
	// legacy/隔离测试。lastSnapshotByAttempt 形成同 Attempt 的 immutable parent 链。
	contextRuntime        ContextRuntime
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
func (e *LLMExecutor) SetContextRuntime(runtime ContextRuntime) {
	e.contextMu.Lock()
	defer e.contextMu.Unlock()
	e.contextRuntime = runtime
	if e.lastSnapshotByAttempt == nil {
		e.lastSnapshotByAttempt = make(map[string]string)
	}
}

func (e *LLMExecutor) contextRuntimeForAttempt(attemptID string) (ContextRuntime, string) {
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

// SetPromptVersion 注入 system prompt 的来源版本（V6 §2 P1a）。装配方在
// 构造 executor 后调用一次；空串表示来源版本未知（组件 Version 缺省）。
func (e *LLMExecutor) SetPromptVersion(version string) {
	e.promptVersion = version
}

func (e *LLMExecutor) SetPhasePromptResolver(resolver func(string) string) {
	e.phasePromptResolver = resolver
}

// SystemPrompt 实现 PromptIdentityProvider：返回启动期装配的静态 system
// prompt 全文（任务级 task.SystemPrompt 覆盖判定在 processTask 侧）。
func (e *LLMExecutor) SystemPrompt() string { return e.sysPrompt }

// PromptVersion 实现 PromptIdentityProvider。
func (e *LLMExecutor) PromptVersion() string { return e.promptVersion }

// TeamAwareness 实现 PromptIdentityProvider：返回静态团队感知文本。
func (e *LLMExecutor) TeamAwareness() string { return e.teamAwareness }

// 编译期断言：*LLMExecutor 必须实现 PromptIdentityProvider。
var _ PromptIdentityProvider = (*LLMExecutor)(nil)

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

// newLLMExecutor 是 LLMExecutor 的统一构造入口。storeView 当前未在 executor
// 内部使用，仅透传以便未来扩展（如未来需要在 executor 内直接查询任务状态再启用）。
func newLLMExecutor(
	client llm.Client,
	tools *ToolRegistry,
	gateReg *gate.Registry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	teamAwareness string,
	systemPrompt ...string,
) *LLMExecutor {
	_ = storeView
	var sysPrompt string
	if len(systemPrompt) > 0 {
		sysPrompt = systemPrompt[0]
	}
	return &LLMExecutor{
		client:         client,
		tools:          tools,
		gateReg:        gateReg,
		recordToolCall: recordToolCall,
		teamAwareness:  teamAwareness,
		sysPrompt:      sysPrompt,
	}
}

// NewLLMExecutor 创建一个基于 LLM 的 TaskExecutor。
// 每次调用对应 ReAct 循环中的一步：调用 LLM → 如果有 tool calls 则执行并返回 ToolCalled=true，
// 否则返回 ToolCalled=false 表示任务完成。
//
// 新增的 3 个 hook 系统参数（v5 Phase 1 起改名为 gateReg，承载统一 Gate 子系统）：
//   - gateReg：工具调用 Gate 注册表（gate.Registry，跨 Tool / Mailbox 域）；
//     nil 时 Dispatch 路径短路为 Continue（gate.Registry 支持 nil receiver）
//   - storeView：当前未在 executor 内部使用，仅透传以便未来扩展
//   - recordToolCall：把每次工具调用（含被 Gate Abort 的调用）自动写入任务
//     历史的闭包。bootstrap 用 `func(id, rec) { taskStore.AppendToolCall(id, rec) }`
//     注入。nil 时跳过历史记录
//
// 三个参数均允许 nil，nil 时整段 Gate + 历史记录路径与改动前字节级一致。
//
// systemPrompt 为可选参数，非空时作为 system/developer 消息注入到对话开头。
// teamAwareness 为可选参数，描述系统中其他 Agent 类型的能力边界，
// 非空时注入到每条 user prompt 的 task description 之前。
//
// 本函数保持返回 TaskExecutor 函数形态（兼容全部既有调用方）。需要按任务
// 替换工具注册表（per-node 能力）的装配方请改用 NewSwappableLLMExecutor
// 拿到 *LLMExecutor 句柄，并把 Agent.ToolSwapper 一并接线。
func NewLLMExecutor(
	client llm.Client,
	tools *ToolRegistry,
	gateReg *gate.Registry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	teamAwareness string,
	systemPrompt ...string,
) TaskExecutor {
	return newLLMExecutor(client, tools, gateReg, storeView, recordToolCall, teamAwareness, systemPrompt...).Execute
}

// NewSwappableLLMExecutor 与 NewLLMExecutor 参数完全相同，但返回 *LLMExecutor
// 结构形态：调用方用 executor.Execute 作为 TaskExecutor，同时可把 executor
// 本身赋给 Agent.ToolSwapper，让 processTask 在 per-node 能力任务上换入
// 过滤视图。
func NewSwappableLLMExecutor(
	client llm.Client,
	tools *ToolRegistry,
	gateReg *gate.Registry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	teamAwareness string,
	systemPrompt ...string,
) *LLMExecutor {
	return newLLMExecutor(client, tools, gateReg, storeView, recordToolCall, teamAwareness, systemPrompt...)
}

// Execute 实现 TaskExecutor 签名（方法值可直接赋给 Agent.Execute）。
func (e *LLMExecutor) Execute(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
	// 整个 Execute 使用同一份 registry 快照——任务边界换入的过滤视图对本次
	// 调用自洽，不会在 Chat 与 Dispatch 之间被换走。
	tools := e.ToolRegistry()
	toolPolicy := deriveInvocationToolPolicy(task, history, tools)
	toolRouter, err := FreezeToolRouterSnapshotWithPolicy(toolPolicy.Registry, toolPolicy.Phase, toolPolicy.MaxCalls)
	if err != nil {
		return ExecuteResult{}, err
	}
	{
		// Task-level system prompt 优先于默认值
		effectivePrompt := e.sysPrompt
		if task.SystemPrompt != "" {
			effectivePrompt = task.SystemPrompt
		}
		agentIDForTrace, _ := ctx.Value(ctxAgentID).(string)
		loopForTrace, _ := ctx.Value(ctxLoopNum).(int)
		runIDForTrace, attemptIDForTrace, turnIDForTrace := executionIdentityFromContext(ctx)
		// 每次 Execute（= 一次 LLM 调用）使用 Attempt/Turn lineage + executor
		// 单调序号生成身份；不能只用 task 前缀/loop，否则进程重启或新 Attempt
		// 会撞 ContextSnapshotStore 的 Invocation 唯一键。
		shortTaskID := task.ID
		if len(shortTaskID) > 8 {
			shortTaskID = shortTaskID[:8]
		}
		invocationBase := turnIDForTrace
		if invocationBase == "" {
			invocationBase = fmt.Sprintf("%s/legacy-loop-%d", shortTaskID, loopForTrace)
		}
		invocationID := fmt.Sprintf("%s/invocation-%d", invocationBase, e.invSeq.Add(1))
		activity := activityFromContext(ctx)
		promptBuildRef := "prompt-build:legacy/unknown"
		var frozenPromptBuild *prompt.Build
		if build, ok := promptBuildFromContext(ctx); ok {
			promptBuildRef = build.ID
			buildCopy := build
			frozenPromptBuild = &buildCopy
		}
		var messages []llm.Message
		var toolDefs []llm.ToolDef
		var invocationBinding *invocation.ContextBinding
		contextSnapshotID := ""
		contextPolicyRef := ""
		manifestTokens := 0
		manifestDescription := "[]"
		contextRuntime, parentSnapshotRef := e.contextRuntimeForAttempt(attemptIDForTrace)
		phasePrompt := ""
		if e.phasePromptResolver != nil {
			phasePrompt = e.phasePromptResolver(toolRouter.Phase)
		}
		if contextRuntime.ready() {
			leaseRef := ""
			if task.Lease != nil && task.Lease.Digest != "" {
				leaseRef = "execution-lease:" + task.Lease.Digest
			}
			compiled, compileErr := contextRuntime.compileAndPersist(ctx, contextCompileRequest{
				Task: task, EffectivePrompt: effectivePrompt, TeamAwareness: e.teamAwareness,
				DependencyResult: depResults, History: history, TaskMemory: taskMemCarrierFromContext(ctx),
				ToolRouter: toolRouter, AttemptID: attemptIDForTrace, InvocationID: invocationID,
				PhasePrompt: phasePrompt, PhasePromptRef: toolRouter.Phase,
				PromptBuildRef: promptBuildRef, PromptBuild: frozenPromptBuild, ExecutionLeaseRef: leaseRef,
				ParentSnapshotRef: parentSnapshotRef,
			})
			if compileErr != nil {
				trace.Emit(trace.Event{
					Kind: trace.KindContextManifestBuilt, TaskID: task.ID, RunID: runIDForTrace,
					AttemptID: attemptIDForTrace, TurnID: turnIDForTrace, AgentID: agentIDForTrace,
					Loop: loopForTrace, InvocationID: invocationID,
					ToolRouterSnapshotID: toolRouter.ID, PromptBuildID: promptBuildRef,
					ContextPolicyRef: task.ContextPolicyRef, Error: compileErr.Error(),
				})
				failure := contextAssemblyFailure(ctx, invocationID, task.ContextPolicyRef, compileErr)
				return ExecuteResult{InvocationID: invocationID}, failure
			}
			messages, toolDefs = compiled.Messages, compiled.Tools
			contextSnapshotID = compiled.Snapshot.SnapshotID
			contextPolicyRef = compiled.Snapshot.ContextPolicyID
			binding, bindErr := compiled.InvocationBinding()
			if bindErr != nil {
				return ExecuteResult{InvocationID: invocationID},
					contextAssemblyFailure(ctx, invocationID, task.ContextPolicyRef, bindErr)
			}
			if int64(toolRouter.MaxCalls) < binding.OutputBudget.MaxToolCalls {
				binding.OutputBudget.MaxToolCalls = int64(toolRouter.MaxCalls)
				if bindErr = binding.Validate(); bindErr != nil {
					return ExecuteResult{InvocationID: invocationID},
						contextAssemblyFailure(ctx, invocationID, task.ContextPolicyRef, bindErr)
				}
			}
			binding.ToolChoice = invocationToolChoice(toolRouter)
			if bindErr = binding.Validate(); bindErr != nil {
				return ExecuteResult{InvocationID: invocationID},
					contextAssemblyFailure(ctx, invocationID, task.ContextPolicyRef, bindErr)
			}
			invocationBinding = &binding
			manifestTokens = int(compiled.Snapshot.Manifest.Usage.EstimatedTokens)
			if raw, marshalErr := json.Marshal(compiled.Snapshot.Manifest.Items); marshalErr == nil {
				manifestDescription = string(raw)
			}
			e.rememberContextSnapshot(attemptIDForTrace, contextSnapshotID)
		} else {
			// 仅无 Run/Graph identity 的 legacy/隔离测试允许旧 builder。生产新任务
			// 缺 L2 authority 时必须 fail-closed，不能静默形成双轨。
			if contextRuntime.configured() || task.RunContract != nil || task.RunID != "" || task.ContextPolicyRef != "" {
				cause := fmt.Errorf("任务缺少完整 L2 ContextRuntime 装配")
				return ExecuteResult{InvocationID: invocationID}, contextAssemblyFailure(ctx, invocationID, task.ContextPolicyRef, cause)
			}
			messages = buildLegacyMessages(effectivePrompt, task, depResults, history, e.teamAwareness)
			if carrier := taskMemCarrierFromContext(ctx); carrier != nil && carrier.dropped == "" && carrier.text != "" {
				messages = insertTaskMemMessage(messages, carrier.text)
			}
			toolDefs = toolRouter.Defs
			manifest := buildLegacyContextManifest(ctx, effectivePrompt, task, depResults, history, e.teamAwareness, toolDefs)
			manifestTokens = manifest.TotalEstimatedTokens
			manifestDescription = manifest.SummaryJSON()
		}
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
			PromptBuildID:        promptBuildRef,
		}
		// V6 §2 P1a：prompt_bound 不独立成事件——context_manifest_built 每轮
		// 恰好一条、与 LLM 调用同域同频，prompt_build_id 作为本轮上下文的
		// 身份字段并入，避免同频双账本。Build 由 processTask 在 attempt
		// 开始编译冻结，同 attempt 各轮复用同一 ID。
		trace.Emit(manifestEv)
		activity.LLMStart(agentIDForTrace, task.ID, loopForTrace, len(toolDefs))

		// Trace：LLM 调用开始
		trace.Emit(trace.Event{
			Kind:                 trace.KindLLMCallStart,
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
			HistoryEntries:       len(history),
			ToolCallsCount:       len(toolDefs),
		})
		// Prompt dump（仅在 --dump-prompts 启用时写入）
		trace.DumpRequest(task.ID, loopForTrace, messages, len(toolDefs))

		llmStart := time.Now()
		var resp llm.Response
		if invocationBinding != nil {
			resp, err = llm.Invoke(ctx, e.client, llm.InvocationRequest{
				Binding: *invocationBinding, Messages: messages, Tools: toolDefs,
			})
		} else {
			resp, err = llm.InvokeLegacy(ctx, e.client, messages, toolDefs)
		}
		llmDuration := time.Since(llmStart)

		if err != nil {
			activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, "", 0, err)
			event := trace.Event{
				Kind:                 trace.KindLLMCallEnd,
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
				DurationMS:           llmDuration.Milliseconds(),
				Error:                err.Error(),
			}
			if failure, ok := invocation.FromError(err); ok {
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
			return ExecuteResult{InvocationID: invocationID, ContextSnapshotID: contextSnapshotID, InvocationDuration: llmDuration}, classifyError(err)
		}

		// Response commit gate：在任何 Tool dispatch 和 History commit 之前证明
		// provider RequiredExact 字段能由下一轮 Context replay。Optional 大
		// reasoning 由 Replay v2 投影为 dropped，不进入此失败路径。
		if contextRuntime.ready() && len(resp.ExtraFields) > 0 {
			if _, replayErr := contextRuntime.validateResponseReplay(task, turnIDForTrace, len(messages), resp.ExtraFields); replayErr != nil {
				failure := responseReplayFailure(replayErr)
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
					PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens,
					ToolCallsCount: len(resp.ToolCalls), Error: failure.Error(),
					FailureKind: string(failure.Kind), FailurePhase: string(failure.Phase),
					FailureOrigin: string(failure.Origin), UsageState: string(failure.UsageState),
				})
				return ExecuteResult{
					InvocationID: invocationID, ContextSnapshotID: contextSnapshotID,
					InvocationDuration: llmDuration, PromptTokens: resp.Usage.PromptTokens,
					CompletionTokens: resp.Usage.CompletionTokens,
				}, failure
			}
		}
		if batchErr := validateToolCallBatch(toolRouter, resp.ToolCalls); batchErr != nil {
			failure := invocation.NewFailure(invocation.FailureMalformedResponse,
				invocation.PhaseToolCallValidate, invocation.OriginProtocol, batchErr)
			failure.UsageState = invocation.UsageSettled
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
				PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens,
				ToolCallsCount: len(resp.ToolCalls), Error: failure.Error(),
				FailureKind: string(failure.Kind), FailurePhase: string(failure.Phase),
				FailureOrigin: string(failure.Origin), UsageState: string(failure.UsageState),
			})
			return ExecuteResult{
				InvocationID: invocationID, ContextSnapshotID: contextSnapshotID,
				InvocationDuration: llmDuration, PromptTokens: resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
			}, failure
		}

		// Trace：LLM 调用成功结束
		trace.Emit(trace.Event{
			Kind:                 trace.KindLLMCallEnd,
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
			DurationMS:           llmDuration.Milliseconds(),
			PromptTokens:         resp.Usage.PromptTokens,
			CompletionTokens:     resp.Usage.CompletionTokens,
			ToolCallsCount:       len(resp.ToolCalls),
		})
		// CM1 对账：Manifest 估算 tokens 与实测值对照，只记录不告警
		//（估算口径 rune/3，偏差供后续校准估算系数参考）。
		log.Printf("[agent %s] task=%s loop=%d manifest 估算 prompt tokens=%d，实测=%d，偏差=%+d",
			agentIDForTrace, task.ID, loopForTrace, manifestTokens,
			resp.Usage.PromptTokens, resp.Usage.PromptTokens-manifestTokens)
		trace.DumpResponse(task.ID, loopForTrace, resp.Content, resp.ToolCalls, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, resp.Content, len(resp.ToolCalls), nil)

		// 无 tool calls → 任务完成
		if len(resp.ToolCalls) == 0 {
			return ExecuteResult{
				InvocationID:       invocationID,
				ContextSnapshotID:  contextSnapshotID,
				InvocationDuration: llmDuration,
				Output:             resp.Content,
				AssistantContent:   resp.Content,
				Reasoning:          resp.Reasoning,
				ToolCalled:         false,
				PromptTokens:       resp.Usage.PromptTokens,
				CompletionTokens:   resp.Usage.CompletionTokens,
				ExtraFields:        resp.ExtraFields,
			}, nil
		}

		// Tool calls execute in the model-provided order. Agent tools include
		// stateful and side-effecting operations, so parallel dispatch would let
		// report_done/finalize/mark_blocked race with writes or shell commands.
		// Serial dispatch also makes each prior ToolCallRecord visible to the next
		// call and gives a Plan guard an actual boundary between calls.
		type indexedResult struct {
			toolResult ToolResult
			output     string
		}

		agentID, _ := ctx.Value(ctxAgentID).(string)
		loopNum, _ := ctx.Value(ctxLoopNum).(int)
		// H2a：本任务的建议跟踪器（重复熔断计数 + 待判定建议）；任务切换时
		// 由 suggestionsForTask 整体重置，per-task 计数任务结束即弃。
		sugTrack := e.suggestionsForTask(task.ID)

		results := make([]indexedResult, len(resp.ToolCalls))
		completedResults := 0
		var controlErr error
		for i, call := range resp.ToolCalls {
			func(idx int, c llm.ToolCall) {
				actionID := turnIDForTrace + "/tool-" + c.ID
				if turnIDForTrace == "" {
					actionID = task.ID + "/legacy-tool-" + c.ID
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
						toolResult: ToolResult{
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
					boundedResult, persistErr := contextRuntime.externalizeToolResult(ctx, task, c, result)
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
				if c.Name == "run_shell" && toolErr == nil {
					exitCode = parseRunShellExitCode(result)
				}
				if recordErr := e.recordToolCallFact(task.ID, store.ToolCallRecord{
					Timestamp: time.Now(),
					RunID:     runIDForTrace,
					AttemptID: attemptIDForTrace,
					TurnID:    turnIDForTrace,
					ActionID:  actionID,
					CallID:    c.ID,
					AgentID:   agentID,
					ToolName:  c.Name,
					Args:      c.Arguments,
					Success:   toolErr == nil,
					ExitCode:  exitCode,
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
					toolResult: ToolResult{
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
		toolResults := make([]ToolResult, completedResults)
		for i, r := range results[:completedResults] {
			output.WriteString(r.output)
			toolResults[i] = r.toolResult
		}

		completedCalls := append([]llm.ToolCall(nil), resp.ToolCalls[:completedResults]...)
		executeResult := ExecuteResult{
			InvocationID:       invocationID,
			ContextSnapshotID:  contextSnapshotID,
			InvocationDuration: llmDuration,
			Output:             output.String(),
			ToolCalled:         completedResults > 0,
			AssistantContent:   resp.Content,
			Reasoning:          resp.Reasoning,
			ToolCalls:          completedCalls,
			ToolResults:        toolResults,
			PromptTokens:       resp.Usage.PromptTokens,
			CompletionTokens:   resp.Usage.CompletionTokens,
			ExtraFields:        resp.ExtraFields,
		}
		return executeResult, controlErr
	}
}

func responseReplayFailure(cause error) *invocation.Failure {
	kind := invocation.FailureOutputLimitExceeded
	var assembly *contextcontract.ContextAssemblyFailure
	if errors.As(cause, &assembly) && assembly.Reason == contextcontract.AssemblyProviderReplayUnknown {
		kind = invocation.FailureProtocolIncompatible
	}
	failure := invocation.NewFailure(kind, invocation.PhaseResponseValidate, invocation.OriginRuntime, cause)
	failure.UsageState = invocation.UsageSettled
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

// buildLegacyMessages 仅供无 Run identity 的旧快照与隔离测试兼容。
// 新生产任务的消息必须来自 ContextCompiler，禁止调用本函数形成第二条装配路径。
// systemPrompt 非空时作为 system 消息插入到对话开头。
// teamAwareness 非空时注入到 user prompt 的 task description 之前。
func buildLegacyMessages(systemPrompt string, task *model.Task, depResults map[string]string, history []HistoryEntry, teamAwareness string) []llm.Message {
	var messages []llm.Message

	// 注入 system prompt（如果提供）
	if systemPrompt != "" {
		messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	}

	// 构建用户消息：团队能力感知 + 受信任任务上下文 + 任务描述 + 依赖结果
	var prompt strings.Builder
	if teamAwareness != "" {
		prompt.WriteString(teamAwareness)
		prompt.WriteString("\n")
	}
	// <task-context>：task_id 必含；图任务（V6 Graph）追加 graph_id /
	// node_id / activation_id 三字段（source 属性仍 "control-plane"），
	// 供 LLM 识别自身所处的图路由语境；非图任务仅 task_id。
	// 渲染实现与 Context Manifest（CM1）共用 renderTaskContextBlock，
	// 保证 manifest digest 与消息内容字节级一致。
	prompt.WriteString(renderTaskContextBlock(task))
	prompt.WriteString(task.Description)

	if len(depResults) > 0 {
		prompt.WriteString("\n\n--- 前置任务结果 ---\n")
		for depID, result := range depResults {
			prompt.WriteString(fmt.Sprintf("[%s] %s\n", depID, result))
		}
	}

	messages = append(messages, llm.Message{Role: "user", Content: prompt.String()})

	// 将历史步骤按 OpenAI tool calling 协议重建为 assistant + tool 消息序列
	for _, entry := range history {
		if entry.SystemNotice != "" {
			messages = append(messages, llm.Message{Role: "system", Content: entry.SystemNotice})
			continue
		}
		// 代理间邮件注入为 user 角色消息（外部信息，非 assistant 自己说的）
		if entry.IncomingMail != "" {
			messages = append(messages, llm.Message{Role: "user", Content: entry.IncomingMail})
			continue
		}
		if entry.ToolCalled && len(entry.ToolCalls) > 0 {
			messages = append(messages, llm.Message{
				Role:        "assistant",
				Content:     entry.AssistantContent,
				ToolCalls:   entry.ToolCalls,
				ExtraFields: entry.ExtraFields,
			})
			for _, tr := range entry.ToolResults {
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    tr.Content,
					ToolCallID: tr.ToolCallID,
				})
			}
		} else {
			messages = append(messages, llm.Message{
				Role:        "assistant",
				Content:     entry.Output,
				ExtraFields: entry.ExtraFields,
			})
		}
	}

	return messages
}

// classifyError 只为尚未携带 InvocationFailure 的外部兼容错误保留旧
// ErrRecoverable 桥。已有 canonical Failure 时原样返回，禁止再叠一层可恢复
// 标签与 FailureKind 形成两个互相冲突的决策事实。
func classifyError(err error) error {
	if _, ok := invocation.FromError(err); ok {
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

func contextAssemblyFailure(ctx context.Context, invocationID, policyRef string, cause error) *invocation.Failure {
	kind := invocation.FailureContextAssembly
	scope := invocation.TimeoutNone
	origin := invocation.OriginRuntime
	contextCause := context.Cause(ctx)
	switch {
	case errors.Is(contextCause, invocation.ErrAttemptDeadline):
		kind, scope = invocation.FailureAttemptDeadline, invocation.TimeoutAttempt
	case errors.Is(contextCause, invocation.ErrActivationDeadline):
		kind, scope = invocation.FailureActivationDeadline, invocation.TimeoutActivation
	case errors.Is(contextCause, invocation.ErrGraphDeadline):
		kind, scope = invocation.FailureActivationDeadline, invocation.TimeoutGraph
	case errors.Is(contextCause, invocation.ErrRunDeadline):
		kind, scope = invocation.FailureActivationDeadline, invocation.TimeoutRun
	case errors.Is(contextCause, context.Canceled):
		kind, scope, origin = invocation.FailureCallerCancelled, invocation.TimeoutCaller, invocation.OriginCaller
	}
	failure := invocation.NewFailure(kind, invocation.PhaseRequestBuild, origin, cause)
	failure.TimeoutScope = scope
	failure.InvocationID = invocationID
	failure.ProviderPolicy = policyRef
	return failure
}
