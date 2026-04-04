package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// FinishReason 表示 LLM 响应的终止原因。
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonToolCalls     FinishReason = "tool_calls"
	FinishReasonLength        FinishReason = "length"
	FinishReasonContentFilter FinishReason = "content_filter"
	FinishReasonUnknown       FinishReason = "unknown"
)

// Message 是对话中的单条消息。
type Message struct {
	Role       string     // "system" | "user" | "assistant" | "tool"
	Content    string     // 消息内容
	Name       string     // 工具名称，仅 role="tool" 时使用
	ToolCallID string     // 对应的 tool_call ID，仅 role="tool" 时使用
	ToolCalls  []ToolCall // LLM 返回的工具调用，仅 role="assistant" 时使用
}

// ToolDef 描述一个可供 LLM 调用的工具。
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema
}

// ToolCall 是 LLM 返回的结构化工具调用请求。
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Response 是解析后的 LLM 响应。
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason FinishReason
	Usage        struct {
		PromptTokens     int
		CompletionTokens int
	}
}

// Client 是 LLM 调用接口。
type Client interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error)
}

// SDKClient 通过 openai-go 官方 SDK 实现 Client 接口。
type SDKClient struct {
	client       openai.Client
	model        openai.ChatModel
	systemPrompt string
}

const defaultLLMTimeout = 120 * time.Second

// NewSDKClient 创建基于 openai-go SDK 的客户端。
// baseURL 为空时使用 OpenAI 官方端点。
// HTTP 层重试由 SDK 内部处理（429/5xx），此处不再额外设置 MaxRetries，
// 避免与调用方的业务重试语义重叠。
func NewSDKClient(baseURL, apiKey, model, systemPrompt string, timeout time.Duration) *SDKClient {
	if timeout <= 0 {
		timeout = defaultLLMTimeout
		log.Printf("[llm] 未指定超时，使用默认值 %v", timeout)
	}

	opts := []option.RequestOption{
		option.WithRequestTimeout(timeout),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	} else {
		log.Println("[llm] 警告: apiKey 为空，SDK 将尝试从环境变量 OPENAI_API_KEY 读取")
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	client := openai.NewClient(opts...)

	return &SDKClient{
		client:       client,
		model:        openai.ChatModel(model),
		systemPrompt: systemPrompt,
	}
}

func (c *SDKClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	params := openai.ChatCompletionNewParams{
		Model: c.model,
	}

	// 插入 system prompt
	if c.systemPrompt != "" {
		params.Messages = append(params.Messages, openai.DeveloperMessage(c.systemPrompt))
	}

	// 转换消息
	for _, m := range messages {
		msg, err := convertMessage(m)
		if err != nil {
			return Response{}, err
		}
		params.Messages = append(params.Messages, msg)
	}

	// 转换工具定义
	for _, t := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Name,
					Description: openai.String(t.Description),
					Parameters:  shared.FunctionParameters(t.Parameters),
				},
			},
		})
	}

	// 调用 SDK — HTTP 层错误（429/5xx）由 SDK 内部重试处理
	completion, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, classifySDKError(err)
	}

	if len(completion.Choices) == 0 {
		return Response{}, &ErrUnrecoverable{Err: errors.New("LLM 返回空 choices")}
	}

	choice := completion.Choices[0]

	// 解析 FinishReason
	finishReason := parseFinishReason(string(choice.FinishReason))

	// 检查异常终止
	switch finishReason {
	case FinishReasonLength:
		log.Printf("[llm] 警告: 响应因 token 上限被截断 (finish_reason=length)")
		return Response{FinishReason: finishReason}, &ErrBadResponse{
			Err: fmt.Errorf("响应被截断 (finish_reason=length)"),
		}
	case FinishReasonContentFilter:
		log.Printf("[llm] 警告: 响应被内容过滤器拦截 (finish_reason=content_filter)")
		return Response{FinishReason: finishReason}, &ErrUnrecoverable{
			Err: fmt.Errorf("响应被内容过滤器拦截 (finish_reason=content_filter)"),
		}
	case FinishReasonUnknown:
		log.Printf("[llm] 警告: 未知的 finish_reason=%q", choice.FinishReason)
	}

	// 转换 tool calls
	var toolCalls []ToolCall
	for _, tc := range choice.Message.ToolCalls {
		args := make(map[string]any)
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				log.Printf("[llm] tool call %q 参数 JSON 解析失败: %v (raw: %s)",
					tc.Function.Name, err, tc.Function.Arguments)
				return Response{}, &ErrBadResponse{
					Err: fmt.Errorf("tool call %q 参数解析失败: %w", tc.Function.Name, err),
				}
			}
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	result := Response{
		Content:      choice.Message.Content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	result.Usage.PromptTokens = int(completion.Usage.PromptTokens)
	result.Usage.CompletionTokens = int(completion.Usage.CompletionTokens)

	return result, nil
}

// convertMessage 将内部 Message 转换为 SDK 的消息类型。
// 遇到未知 role 时返回 ErrUnknownRole 而非静默降级。
func convertMessage(m Message) (openai.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case "system":
		return openai.DeveloperMessage(m.Content), nil
	case "user":
		return openai.UserMessage(m.Content), nil
	case "assistant":
		if len(m.ToolCalls) > 0 {
			var sdkCalls []openai.ChatCompletionMessageToolCallUnionParam
			for _, tc := range m.ToolCalls {
				argsJSON, err := json.Marshal(tc.Arguments)
				if err != nil {
					log.Printf("[llm] 序列化 tool call %q 参数失败: %v", tc.Name, err)
					return openai.ChatCompletionMessageParamUnion{}, &ErrBadResponse{
						Err: fmt.Errorf("序列化 tool call %q 参数失败: %w", tc.Name, err),
					}
				}
				sdkCalls = append(sdkCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: string(argsJSON),
						},
					},
				})
			}
			return openai.ChatCompletionMessageParamUnion{
				OfAssistant: &openai.ChatCompletionAssistantMessageParam{
					Content:   openai.ChatCompletionAssistantMessageParamContentUnion{OfString: openai.String(m.Content)},
					ToolCalls: sdkCalls,
				},
			}, nil
		}
		return openai.AssistantMessage(m.Content), nil
	case "tool":
		return openai.ToolMessage(m.Content, m.ToolCallID), nil
	default:
		log.Printf("[llm] 错误: 遇到未知消息 role=%q", m.Role)
		return openai.ChatCompletionMessageParamUnion{}, &ErrUnknownRole{Role: m.Role}
	}
}

// parseFinishReason 将 API 返回的 finish_reason 字符串映射为枚举值。
func parseFinishReason(raw string) FinishReason {
	switch raw {
	case "stop":
		return FinishReasonStop
	case "tool_calls":
		return FinishReasonToolCalls
	case "length":
		return FinishReasonLength
	case "content_filter":
		return FinishReasonContentFilter
	default:
		return FinishReasonUnknown
	}
}

// classifySDKError 将 SDK 错误分类。
// HTTP 层重试已由 SDK 处理，这里只做最终分类。
func classifySDKError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 429 || apiErr.StatusCode >= 500:
			return &ErrRecoverable{Err: err}
		case apiErr.StatusCode == 401 || apiErr.StatusCode == 403 || apiErr.StatusCode == 404:
			return &ErrUnrecoverable{Err: err}
		default:
			return &ErrUnrecoverable{Err: err}
		}
	}
	// 网络错误等非 API 错误视为可恢复
	return &ErrRecoverable{Err: err}
}
