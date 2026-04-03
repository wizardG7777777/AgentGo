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

		// 有 tool calls → 逐一执行，拼接结果
		var output strings.Builder
		for _, call := range resp.ToolCalls {
			result, toolErr := tools.Dispatch(ctx, call)
			if toolErr != nil {
				output.WriteString(fmt.Sprintf("[%s] 错误: %v\n", call.Name, toolErr))
			} else {
				output.WriteString(fmt.Sprintf("[%s] %s\n", call.Name, result))
			}
		}

		return ExecuteResult{Output: output.String(), ToolCalled: true}, nil
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

	// 将历史步骤转换为 assistant + tool 消息对
	for _, entry := range history {
		// 每个历史步骤是一次 assistant 返回（含 tool 调用结果）
		messages = append(messages, llm.Message{
			Role:    "assistant",
			Content: entry.Output,
		})
	}

	return messages
}

// classifyError 将 llm 包的错误类型桥接为 agent 包的错误类型。
func classifyError(err error) error {
	var llmRecov *llm.ErrRecoverable
	if errors.As(err, &llmRecov) {
		return &ErrRecoverable{Err: err}
	}
	// llm.ErrUnrecoverable 和其他错误 → 不可恢复
	return err
}
