package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentgo/internal/llm"
	"agentgo/internal/model"
)

// NewLLMExecutor 创建一个基于 LLM 的 TaskExecutor。
// 每次调用对应 ReAct 循环中的一步：调用 LLM → 如果有 tool calls 则执行并返回 ToolCalled=true，
// 否则返回 ToolCalled=false 表示任务完成。
func NewLLMExecutor(client llm.Client, tools *ToolRegistry) TaskExecutor {
	return func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		messages := buildMessages(task, depResults, history)

		resp, err := client.Chat(ctx, messages, tools.Defs())
		if err != nil {
			return ExecuteResult{}, classifyError(err)
		}

		// 无 tool calls → 任务完成
		if len(resp.ToolCalls) == 0 {
			return ExecuteResult{Output: resp.Content, ToolCalled: false}, nil
		}

		// 有 tool calls → 逐一执行，记录每个 tool call 的结果
		var output strings.Builder
		var toolResults []ToolResult
		for _, call := range resp.ToolCalls {
			result, toolErr := tools.Dispatch(ctx, call)
			var content string
			if toolErr != nil {
				content = fmt.Sprintf("错误: %v", toolErr)
				output.WriteString(fmt.Sprintf("[%s] %s\n", call.Name, content))
			} else {
				content = result
				output.WriteString(fmt.Sprintf("[%s] %s\n", call.Name, result))
			}
			toolResults = append(toolResults, ToolResult{
				ToolCallID: call.ID,
				Content:    content,
			})
		}

		return ExecuteResult{
			Output:           output.String(),
			ToolCalled:       true,
			AssistantContent: resp.Content,
			ToolCalls:        resp.ToolCalls,
			ToolResults:      toolResults,
		}, nil
	}
}

// buildMessages 将任务信息和执行历史转换为 LLM 对话消息。
func buildMessages(task *model.Task, depResults map[string]string, history []HistoryEntry) []llm.Message {
	var messages []llm.Message

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
		if entry.ToolCalled && len(entry.ToolCalls) > 0 {
			// assistant 消息：携带 ToolCalls（LLM 请求调用工具）
			messages = append(messages, llm.Message{
				Role:      "assistant",
				Content:   entry.AssistantContent,
				ToolCalls: entry.ToolCalls,
			})
			// 每个 tool call 对应一条 tool role 消息（执行结果）
			for _, tr := range entry.ToolResults {
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    tr.Content,
					ToolCallID: tr.ToolCallID,
				})
			}
		} else {
			// 无 tool call 的历史步骤（兼容旧数据），作为纯 assistant 消息
			messages = append(messages, llm.Message{
				Role:    "assistant",
				Content: entry.Output,
			})
		}
	}

	return messages
}

// classifyError 将 llm 包的错误类型桥接为 agent 包的错误类型。
// ErrRecoverable 和 ErrBadResponse 均视为可恢复，触发重试。
func classifyError(err error) error {
	var llmRecov *llm.ErrRecoverable
	if errors.As(err, &llmRecov) {
		return &ErrRecoverable{Err: err}
	}
	var llmBad *llm.ErrBadResponse
	if errors.As(err, &llmBad) {
		return &ErrRecoverable{Err: err}
	}
	// llm.ErrUnrecoverable 和其他错误 → 不可恢复
	return err
}
