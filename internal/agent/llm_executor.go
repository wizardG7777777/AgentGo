package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"agentgo/internal/llm"
	"agentgo/internal/model"
)

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

		resp, err := client.Chat(ctx, messages, tools.Defs())
		if err != nil {
			return ExecuteResult{}, classifyError(err)
		}

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

		results := make([]indexedResult, len(resp.ToolCalls))
		var wg sync.WaitGroup
		for i, call := range resp.ToolCalls {
			wg.Add(1)
			go func(idx int, c llm.ToolCall) {
				defer wg.Done()
				result, toolErr := tools.Dispatch(ctx, c)
				var content string
				if toolErr != nil {
					content = fmt.Sprintf("错误: %v", toolErr)
				} else {
					content = result
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
