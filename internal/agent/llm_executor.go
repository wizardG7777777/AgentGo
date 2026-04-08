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

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// executorContextKey 是注入到 context 中的键类型，用于传递执行上下文信息供日志和 trace 使用。
type executorContextKey int

const (
	ctxAgentID executorContextKey = iota
	ctxLoopNum
	ctxTaskID
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
// systemPrompt 为可选参数，非空时作为 system/developer 消息注入到对话开头。
func NewLLMExecutor(client llm.Client, tools *ToolRegistry, systemPrompt ...string) TaskExecutor {
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
		messages := buildMessages(effectivePrompt, task, depResults, history)

		agentIDForTrace, _ := ctx.Value(ctxAgentID).(string)
		loopForTrace, _ := ctx.Value(ctxLoopNum).(int)
		toolDefs := tools.Defs()

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

		// 无 tool calls → 任务完成
		if len(resp.ToolCalls) == 0 {
			return ExecuteResult{
				Output:           resp.Content,
				ToolCalled:       false,
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
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
				start := time.Now()
				result, toolErr := tools.Dispatch(ctx, c)
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
						CallID:     c.ID,
						DurationMS: dur.Milliseconds(),
						ResultLen:  len(content),
					})
				}
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
		}, nil
	}
}

// buildMessages 将任务信息和执行历史转换为 LLM 对话消息。
// systemPrompt 非空时作为 system 消息插入到对话开头。
func buildMessages(systemPrompt string, task *model.Task, depResults map[string]string, history []HistoryEntry) []llm.Message {
	var messages []llm.Message

	// 注入 system prompt（如果提供）
	if systemPrompt != "" {
		messages = append(messages, llm.Message{Role: "system", Content: systemPrompt})
	}

	// 构建用户消息：任务描述 + 依赖结果
	var prompt strings.Builder
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
				Role:      "assistant",
				Content:   entry.AssistantContent,
				ToolCalls: entry.ToolCalls,
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
				Role:    "assistant",
				Content: entry.Output,
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
