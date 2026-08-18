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

	"agentgo/internal/gate"
	"agentgo/internal/llm"
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
)

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
	teamAwareness  string
	sysPrompt      string
	toolsMu        sync.RWMutex
	tools          *ToolRegistry
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
	// invSeq 是 V6 §7.2 InvocationID 的 executor 级单调序号：每次 Execute
	// （= 一次 LLM 调用）取一次，拼成 <taskID前8>-<loop>-<seq>。重试产生的
	// 同 (task,loop) 重复调用借 seq 区分。
	invSeq atomic.Uint64
}

// SetPromptVersion 注入 system prompt 的来源版本（V6 §2 P1a）。装配方在
// 构造 executor 后调用一次；空串表示来源版本未知（组件 Version 缺省）。
func (e *LLMExecutor) SetPromptVersion(version string) {
	e.promptVersion = version
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
	{
		// Task-level system prompt 优先于默认值
		effectivePrompt := e.sysPrompt
		if task.SystemPrompt != "" {
			effectivePrompt = task.SystemPrompt
		}
		messages := buildMessages(effectivePrompt, task, depResults, history, e.teamAwareness)

		// CM2（V6 §3）：Task Memory 注入——user 首条之后插入有界渲染。
		// 载体降级（dropped）时不注入正文，Manifest 仍会记 dropped:<原因>。
		if carrier := taskMemCarrierFromContext(ctx); carrier != nil && carrier.dropped == "" && carrier.text != "" {
			messages = insertTaskMemMessage(messages, carrier.text)
		}

		agentIDForTrace, _ := ctx.Value(ctxAgentID).(string)
		loopForTrace, _ := ctx.Value(ctxLoopNum).(int)
		// V6 §7.2：每次 Execute（= 一次 LLM 调用）生成统一调用身份
		// <taskID前8>-<loop>-<seq>，挂到本轮 llm_call_start / llm_call_end /
		// context_manifest_built 三处事件，同一轮三事件可经 invocation_id
		// 精确关联。Attempt/Turn 身份本轮不加——Loop 序号已可关联同任务
		// 各轮，重试靠 seq 区分。
		shortTaskID := task.ID
		if len(shortTaskID) > 8 {
			shortTaskID = shortTaskID[:8]
		}
		invocationID := fmt.Sprintf("%s-%d-%d", shortTaskID, loopForTrace, e.invSeq.Add(1))
		activity := activityFromContext(ctx)
		toolDefs := tools.Defs()
		// CM1（V6 §3）：每次 LLM 调用前生成 Context Manifest 影子账本——逐段
		// 记录来源/作用域/权威等级/新鲜度/digest/token 估算/处置，纯观测，
		// 不改变 messages 与 toolDefs 的任何字节。每轮恰好一条
		// context_manifest_built 事件（落任务分片）；实测 prompt tokens 由
		// 同 (task_id, loop) 的 llm_call_end 事件对账。
		manifest := buildContextManifest(ctx, effectivePrompt, task, depResults, history, e.teamAwareness, toolDefs)
		manifestEv := trace.Event{
			Kind:           trace.KindContextManifestBuilt,
			TaskID:         task.ID,
			AgentID:        agentIDForTrace,
			Loop:           loopForTrace,
			InvocationID:   invocationID,
			PromptTokens:   manifest.TotalEstimatedTokens,
			HistoryEntries: len(history),
			Description:    manifest.SummaryJSON(),
		}
		// V6 §2 P1a：prompt_bound 不独立成事件——context_manifest_built 每轮
		// 恰好一条、与 LLM 调用同域同频，prompt_build_id 作为本轮上下文的
		// 身份字段并入，避免同频双账本。Build 由 processTask 在 attempt
		// 开始编译冻结，同 attempt 各轮复用同一 ID。
		if build, ok := promptBuildFromContext(ctx); ok {
			manifestEv.PromptBuildID = build.ID
		}
		trace.Emit(manifestEv)
		activity.LLMStart(agentIDForTrace, task.ID, loopForTrace, len(toolDefs))

		// Trace：LLM 调用开始
		trace.Emit(trace.Event{
			Kind:           trace.KindLLMCallStart,
			TaskID:         task.ID,
			AgentID:        agentIDForTrace,
			Loop:           loopForTrace,
			InvocationID:   invocationID,
			HistoryEntries: len(history),
			ToolCallsCount: len(toolDefs),
		})
		// Prompt dump（仅在 --dump-prompts 启用时写入）
		trace.DumpRequest(task.ID, loopForTrace, messages, len(toolDefs))

		llmStart := time.Now()
		resp, err := e.client.Chat(ctx, messages, toolDefs)
		llmDuration := time.Since(llmStart)

		if err != nil {
			activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, "", 0, err)
			trace.Emit(trace.Event{
				Kind:         trace.KindLLMCallEnd,
				TaskID:       task.ID,
				AgentID:      agentIDForTrace,
				Loop:         loopForTrace,
				InvocationID: invocationID,
				DurationMS:   llmDuration.Milliseconds(),
				Error:        err.Error(),
			})
			return ExecuteResult{}, classifyError(err)
		}

		// Trace：LLM 调用成功结束
		trace.Emit(trace.Event{
			Kind:             trace.KindLLMCallEnd,
			TaskID:           task.ID,
			AgentID:          agentIDForTrace,
			Loop:             loopForTrace,
			InvocationID:     invocationID,
			DurationMS:       llmDuration.Milliseconds(),
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			ToolCallsCount:   len(resp.ToolCalls),
		})
		// CM1 对账：Manifest 估算 tokens 与实测值对照，只记录不告警
		//（估算口径 rune/3，偏差供后续校准估算系数参考）。
		log.Printf("[agent %s] task=%s loop=%d manifest 估算 prompt tokens=%d，实测=%d，偏差=%+d",
			agentIDForTrace, task.ID, loopForTrace, manifest.TotalEstimatedTokens,
			resp.Usage.PromptTokens, resp.Usage.PromptTokens-manifest.TotalEstimatedTokens)
		trace.DumpResponse(task.ID, loopForTrace, resp.Content, resp.ToolCalls, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, resp.Content, len(resp.ToolCalls), nil)

		// 无 tool calls → 任务完成
		if len(resp.ToolCalls) == 0 {
			return ExecuteResult{
				Output:           resp.Content,
				AssistantContent: resp.Content,
				Reasoning:        resp.Reasoning,
				ToolCalled:       false,
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				ExtraFields:      resp.ExtraFields,
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
		for i, call := range resp.ToolCalls {
			func(idx int, c llm.ToolCall) {
				// finalizing fence：submit_task_result 被接受（MarkTaskFinalized）
				// 后，同一响应中排在其后的工具调用一律跳过——不 dispatch、不产生
				// 副作用、不写 ToolCallRecord（调用从未发生），只返回结构化提示
				// 文本并 emit tool_call_skipped 审计事件。工具按序串行执行，
				// 前一个调用置上的 finalized 标志对后续调用即时可见。
				if e.finalizing() {
					content := "已跳过：任务已进入收尾（finalizing），本次调用未执行"
					log.Printf("[agent %s] task=%s loop=%d tool=%s 被 finalizing fence 跳过（call_id=%s）", agentID, task.ID, loopNum, c.Name, c.ID)
					trace.Emit(trace.Event{
						Kind:    trace.KindToolCallSkipped,
						TaskID:  task.ID,
						AgentID: agentID,
						Loop:    loopNum,
						Tool:    c.Name,
						CallID:  c.ID,
						Reason:  "task_finalizing",
					})
					results[idx] = indexedResult{
						toolResult: ToolResult{
							ToolCallID: c.ID,
							Content:    content,
						},
						output: fmt.Sprintf("[%s] %s\n", c.Name, content),
					}
					return
				}

				argsLog := truncateForLog(c.Arguments, 120)
				log.Printf("[agent %s] task=%s loop=%d tool=%s args=%s", agentID, task.ID, loopNum, c.Name, argsLog)
				activity.ToolStarted(agentID, task.ID, loopNum, c.ID, c.Name)
				// Trace：工具调用开始。V6 §7.4：Args 过默认脱敏（结构字段保留、
				// 自由内容替换为 <redacted> 占位；AGENTGO_TRACE_FULL_ARGS=1 可旁路），
				// 原 c.Arguments 不受影响，继续参与 Gate / dispatch / ToolCallRecord。
				trace.Emit(trace.Event{
					Kind:    trace.KindToolCall,
					TaskID:  task.ID,
					AgentID: agentID,
					Loop:    loopNum,
					Tool:    c.Name,
					Args:    trace.RedactArgs(c.Name, c.Arguments),
					CallID:  c.ID,
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
							result, toolErr = tools.Dispatch(ctx, c)
						}
					} else {
						result, toolErr = tools.Dispatch(ctx, c)
					}
				}
				dur := time.Since(start)

				var content string
				if toolErr != nil {
					content = fmt.Sprintf("错误: %v", toolErr)
					log.Printf("[agent %s] task=%s loop=%d tool=%s duration=%s error=%v", agentID, task.ID, loopNum, c.Name, dur.Round(time.Millisecond), toolErr)
					trace.Emit(trace.Event{
						Kind:       trace.KindToolResult,
						TaskID:     task.ID,
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
				if e.recordToolCall != nil {
					var exitCode *int
					if c.Name == "run_shell" && toolErr == nil {
						exitCode = parseRunShellExitCode(result)
					}
					e.recordToolCall(task.ID, store.ToolCallRecord{
						Timestamp: time.Now(),
						CallID:    c.ID,
						AgentID:   agentID,
						ToolName:  c.Name,
						Args:      c.Arguments,
						Success:   toolErr == nil,
						ExitCode:  exitCode,
					})
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
			}(i, call)
		}

		// 按原始顺序组装输出和 toolResults
		var output strings.Builder
		toolResults := make([]ToolResult, len(results))
		for i, r := range results {
			output.WriteString(r.output)
			toolResults[i] = r.toolResult
		}

		return ExecuteResult{
			Output:           output.String(),
			ToolCalled:       true,
			AssistantContent: resp.Content,
			Reasoning:        resp.Reasoning,
			ToolCalls:        resp.ToolCalls,
			ToolResults:      toolResults,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			ExtraFields:      resp.ExtraFields,
		}, nil
	}
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

// buildMessages 将任务信息和执行历史转换为 LLM 对话消息。
// systemPrompt 非空时作为 system 消息插入到对话开头。
// teamAwareness 非空时注入到 user prompt 的 task description 之前。
func buildMessages(systemPrompt string, task *model.Task, depResults map[string]string, history []HistoryEntry, teamAwareness string) []llm.Message {
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

// classifyError 将 llm 包的错误类型桥接为 agent 包的错误类型。
func classifyError(err error) error {
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
