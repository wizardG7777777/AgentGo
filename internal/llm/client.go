package llm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
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
	ID        string
	Name      string
	Arguments map[string]string
}

// Response 是解析后的 LLM 响应。
type Response struct {
	Content   string
	ToolCalls []ToolCall
	Usage     struct {
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

// NewSDKClient 创建基于 openai-go SDK 的客户端。
// baseURL 为空时使用 OpenAI 官方端点。
func NewSDKClient(baseURL, apiKey, model, systemPrompt string, timeout time.Duration) *SDKClient {
	opts := []option.RequestOption{
		option.WithMaxRetries(2),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(timeout))
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
		params.Messages = append(params.Messages, convertMessage(m))
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

	// 调用 SDK
	completion, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, classifySDKError(err)
	}

	if len(completion.Choices) == 0 {
		return Response{}, &ErrUnrecoverable{Err: errors.New("LLM 返回空 choices")}
	}

	choice := completion.Choices[0]

	// 转换 tool calls
	var toolCalls []ToolCall
	for _, tc := range choice.Message.ToolCalls {
		args := make(map[string]string)
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	result := Response{
		Content:   choice.Message.Content,
		ToolCalls: toolCalls,
	}
	result.Usage.PromptTokens = int(completion.Usage.PromptTokens)
	result.Usage.CompletionTokens = int(completion.Usage.CompletionTokens)

	return result, nil
}

// convertMessage 将内部 Message 转换为 SDK 的消息类型。
func convertMessage(m Message) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case "system":
		return openai.DeveloperMessage(m.Content)
	case "user":
		return openai.UserMessage(m.Content)
	case "assistant":
		if len(m.ToolCalls) > 0 {
			var sdkCalls []openai.ChatCompletionMessageToolCallUnionParam
			for _, tc := range m.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Arguments)
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
			}
		}
		return openai.AssistantMessage(m.Content)
	case "tool":
		return openai.ToolMessage(m.Content, m.ToolCallID)
	default:
		return openai.UserMessage(m.Content)
	}
}

// classifySDKError 将 SDK 错误分类为可恢复/不可恢复。
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
