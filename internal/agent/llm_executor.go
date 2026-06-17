package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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
	// ctxNoTools 用于在 buildTransferNote / 其它"应当只输出文字"的 LLM 调用路径上
	// 指示 executor 不暴露工具集。这样 LLM 想调工具也无工具可调，副作用泄漏（如
	// 多写一遍 APPROVED.md / 多发一条 send_message）被结构性地阻断。
	// 详见 transfer_note.go L1 注释 + agent.go MaxLoops 兜底路径。
	ctxNoTools
	ctxActivity
)

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

// WithNoTools 标记当前 LLM 调用不应暴露任何工具。executor 看到该标记后会
// 把传给 client.Chat 的 toolDefs 替换为空切片——LLM 物理上无工具可调，
// 强制走"只输出文字"的路径。用于 buildTransferNote 等期望纯文本输出的场景。
func WithNoTools(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxNoTools, true)
}

func noToolsFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxNoTools).(bool)
	return v
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
func NewLLMExecutor(
	client llm.Client,
	tools *ToolRegistry,
	gateReg *gate.Registry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	teamAwareness string,
	systemPrompt ...string,
) TaskExecutor {
	// storeView 当前仅用作未来扩展位。编译器会抱怨未使用，先用 _ 绑定一下。
	// 如未来需要在 executor 内直接查询任务状态（例如 hook 间共享），再启用。
	_ = storeView
	var sysPrompt string
	if len(systemPrompt) > 0 {
		sysPrompt = systemPrompt[0]
	}
	return func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		// Task-level system prompt 优先于默认值
		effectivePrompt := sysPrompt
		if task.SystemPrompt != "" {
			effectivePrompt = task.SystemPrompt
		}
		messages := buildMessages(effectivePrompt, task, depResults, history, teamAwareness)

		agentIDForTrace, _ := ctx.Value(ctxAgentID).(string)
		loopForTrace, _ := ctx.Value(ctxLoopNum).(int)
		activity := activityFromContext(ctx)
		var toolDefs []llm.ToolDef
		if !noToolsFromContext(ctx) {
			toolDefs = tools.Defs()
		}
		activity.LLMStart(agentIDForTrace, task.ID, loopForTrace, len(toolDefs))

		// Trace：LLM 调用开始
		trace.Emit(trace.Event{
			Kind:           trace.KindLLMCallStart,
			TaskID:         task.ID,
			AgentID:        agentIDForTrace,
			Loop:           loopForTrace,
			HistoryEntries: len(history),
			ToolCallsCount: len(toolDefs),
		})
		// Prompt dump（仅在 --dump-prompts 启用时写入）
		trace.DumpRequest(task.ID, loopForTrace, messages, len(toolDefs))

		llmStart := time.Now()
		resp, err := client.Chat(ctx, messages, toolDefs)
		llmDuration := time.Since(llmStart)

		if err != nil {
			activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, "", 0, err)
			trace.Emit(trace.Event{
				Kind:       trace.KindLLMCallEnd,
				TaskID:     task.ID,
				AgentID:    agentIDForTrace,
				Loop:       loopForTrace,
				DurationMS: llmDuration.Milliseconds(),
				Error:      err.Error(),
			})
			return ExecuteResult{}, classifyError(err)
		}

		// Trace：LLM 调用成功结束
		trace.Emit(trace.Event{
			Kind:             trace.KindLLMCallEnd,
			TaskID:           task.ID,
			AgentID:          agentIDForTrace,
			Loop:             loopForTrace,
			DurationMS:       llmDuration.Milliseconds(),
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			ToolCallsCount:   len(resp.ToolCalls),
		})
		trace.DumpResponse(task.ID, loopForTrace, resp.Content, resp.ToolCalls, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		activity.LLMEnd(agentIDForTrace, task.ID, loopForTrace, resp.Content, len(resp.ToolCalls), nil)

		// 无 tool calls → 任务完成
		if len(resp.ToolCalls) == 0 {
			return ExecuteResult{
				Output:           resp.Content,
				ToolCalled:       false,
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				ExtraFields:      resp.ExtraFields,
			}, nil
		}

		// 有 tool calls → 并行执行，记录每个 tool call 的结果
		type indexedResult struct {
			toolResult ToolResult
			output     string
		}

		agentID, _ := ctx.Value(ctxAgentID).(string)
		loopNum, _ := ctx.Value(ctxLoopNum).(int)

		results := make([]indexedResult, len(resp.ToolCalls))
		var wg sync.WaitGroup
		for i, call := range resp.ToolCalls {
			wg.Add(1)
			go func(idx int, c llm.ToolCall) {
				defer wg.Done()
				argsLog := truncateForLog(c.Arguments, 120)
				log.Printf("[agent %s] task=%s loop=%d tool=%s args=%s", agentID, task.ID, loopNum, c.Name, argsLog)
				activity.ToolStarted(agentID, task.ID, loopNum, c.ID, c.Name)
				// Trace：工具调用开始（含完整 args，不做截断）
				trace.Emit(trace.Event{
					Kind:    trace.KindToolCall,
					TaskID:  task.ID,
					AgentID: agentID,
					Loop:    loopNum,
					Tool:    c.Name,
					Args:    c.Arguments,
					CallID:  c.ID,
				})

				// Gate pre-call：允许注册的 Gate 拒绝本次调用。
				// gateReg 为 nil 时 Dispatch 直接返回 Continue（nil receiver 安全）。
				preDecision := gateReg.Dispatch(&gate.ToolContext{
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
					result = ""
					toolErr = fmt.Errorf("[hook 拒绝] %s: %s", preDecision.HookName, preDecision.AbortReason)
				} else {
					result, toolErr = tools.Dispatch(ctx, c)
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
						Args:       c.Arguments, // v5 Phase 6：与 KindToolCall 对称，让 Reactor 能读 args.path
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
						Args:       c.Arguments, // v5 Phase 6：read-set-write Reactor 据此 filter 并拿 path
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
				if recordToolCall != nil {
					recordToolCall(task.ID, store.ToolCallRecord{
						Timestamp: time.Now(),
						AgentID:   agentID,
						ToolName:  c.Name,
						Args:      c.Arguments,
						Success:   toolErr == nil,
					})
				}

				// Gate post-call：纯观察，Dispatch 返回值忽略。gateReg 为 nil 时无操作。
				_ = gateReg.Dispatch(&gate.ToolContext{
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
		wg.Wait()

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
			ToolCalls:        resp.ToolCalls,
			ToolResults:      toolResults,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			ExtraFields:      resp.ExtraFields,
		}, nil
	}
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

	// 构建用户消息：团队能力感知 + 任务描述 + 依赖结果
	var prompt strings.Builder
	if teamAwareness != "" {
		prompt.WriteString(teamAwareness)
		prompt.WriteString("\n")
	}
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
