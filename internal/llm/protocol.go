package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Protocol 冻结一次客户端使用的模型调用 wire 契约。Responses 是新主链；
// Responses 与 Chat Completions 均为显式 SSE 协议，禁止自动切换。
type Protocol string

const (
	ProtocolResponses       Protocol = "responses"
	ProtocolChatCompletions Protocol = "chat_completions"
)

func ParseProtocol(raw string) (Protocol, error) {
	switch Protocol(strings.TrimSpace(raw)) {
	case ProtocolResponses:
		return ProtocolResponses, nil
	case ProtocolChatCompletions:
		return ProtocolChatCompletions, nil
	default:
		return "", fmt.Errorf("未知 LLM protocol=%q（仅允许 %q / %q）",
			raw, ProtocolResponses, ProtocolChatCompletions)
	}
}

// OutputItemKind 是 Model Invocation 从 provider 信封反序列化出的类型标签。
// Harness 只允许 function_call 进入工具分发；message/reasoning 永远不会从正文
// 再解释为行动。
type OutputItemKind string

const (
	OutputItemMessage      OutputItemKind = "message"
	OutputItemReasoning    OutputItemKind = "reasoning"
	OutputItemFunctionCall OutputItemKind = "function_call"
)

// OutputItem 是跨 Responses/Chat Completions 适配器的最小强类型输出事实。
// Raw 只在 Responses 路径保存服务端完整 item，用于 L2 下一轮逐项 replay；
// ToolCall 仅在 Kind=function_call 时非 nil。
type OutputItem struct {
	Kind      OutputItemKind  `json:"kind"`
	ID        string          `json:"id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Reasoning string          `json:"reasoning,omitempty"`
	ToolCall  *ToolCall       `json:"tool_call,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// ReplayItemsBudgetKey 仅用于完整协议重放项的预算，不是模型消息字段。
const ReplayItemsBudgetKey = "response_items"
