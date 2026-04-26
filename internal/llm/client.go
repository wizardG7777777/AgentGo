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
	// ExtraFields 保存响应里 openai-go 未识别的 assistant 消息字段（如 DeepSeek V4 的
	// reasoning_content）。下一轮请求时会通过 SetExtraFields 原样回写，避免
	// 被 openai-go 强类型 struct 默默吞掉。
	ExtraFields map[string]json.RawMessage `json:"extra_fields,omitempty"`
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
	// ExtraFields 是 assistant 消息里的非标字段（如 DeepSeek V4 的 reasoning_content）。
	// 调用方应把这份 map 挂到随后追加进历史的 Message 上，确保下一轮请求能原样回传。
	ExtraFields map[string]json.RawMessage
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
	provider     Provider
}

const defaultLLMTimeout = 120 * time.Second

// NewSDKClient 创建基于 openai-go SDK 的客户端。
// baseURL 为空时使用 OpenAI 官方端点。
// providerName 指定 LLM provider 适配器（"openai"/"deepseek-v4"/"deepseek-r1"），
// 空串或未知名称时 fallback 到 OpenAIProvider（no-op，与旧版行为一致）。
// HTTP 层重试由 SDK 内部处理（429/5xx），此处不再额外设置 MaxRetries，
// 避免与调用方的业务重试语义重叠。
func NewSDKClient(baseURL, apiKey, model, systemPrompt, providerName string, timeout time.Duration) *SDKClient {
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
		provider:     GetProvider(providerName),
	}
}

func (c *SDKClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	// Provider 有机会在发请求前改造 history（例如 DeepSeek R1 剥离老轮次的 reasoning_content）。
	if c.provider != nil {
		messages = c.provider.PrepareMessages(messages)
	}

	params := openai.ChatCompletionNewParams{
		Model: c.model,
	}

	// 插入 system prompt（使用 system 角色以兼容 Dashscope 等非 OpenAI 后端）
	if c.systemPrompt != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(c.systemPrompt))
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

	// Provider 可追加请求 RequestOption（例如 WithJSONSet 注入 body 字段）
	var providerOpts []option.RequestOption
	if c.provider != nil {
		providerOpts = c.provider.RequestOptions()
	}

	// 调用 SDK — HTTP 层错误（429/5xx）由 SDK 内部重试处理
	completion, err := c.client.Chat.Completions.New(ctx, params, providerOpts...)
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

	// 层 1：把响应里 openai-go 未识别的字段原样抽到 ExtraFields。
	// DeepSeek V4 的 reasoning_content、其他 provider 的自定义元数据都走这条路。
	if len(choice.Message.JSON.ExtraFields) > 0 {
		result.ExtraFields = make(map[string]json.RawMessage, len(choice.Message.JSON.ExtraFields))
		for k, f := range choice.Message.JSON.ExtraFields {
			raw := f.Raw()
			if raw == "" {
				continue
			}
			result.ExtraFields[k] = json.RawMessage(raw)
		}
	}

	return result, nil
}

// convertMessage 将内部 Message 转换为 SDK 的消息类型。
// 遇到未知 role 时返回 ErrUnknownRole 而非静默降级。
func convertMessage(m Message) (openai.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case "system":
		return openai.SystemMessage(m.Content), nil
	case "user":
		return openai.UserMessage(m.Content), nil
	case "assistant":
		// 统一构造 AssistantMessageParam：无论有无 tool calls 或 ExtraFields，
		// 走同一路径以便在尾部挂 SetExtraFields（层 1 通用透传）。
		assistantParam := openai.ChatCompletionAssistantMessageParam{
			Content: openai.ChatCompletionAssistantMessageParamContentUnion{OfString: openai.String(m.Content)},
		}
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
			assistantParam.ToolCalls = sdkCalls
		}
		if len(m.ExtraFields) > 0 {
			extras := make(map[string]any, len(m.ExtraFields))
			for k, v := range m.ExtraFields {
				// json.RawMessage 实现了 json.Marshaler，openai-go 会原样写出
				extras[k] = v
			}
			assistantParam.SetExtraFields(extras)
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistantParam}, nil
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
// 响应体中的 code / message 被提取到 error 结构体，供上层打印诊断信息。
func classifySDKError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		code := apiErr.Code
		message := apiErr.Message
		statusCode := apiErr.StatusCode
		endpoint := ""
		if apiErr.Request != nil && apiErr.Request.URL != nil {
			endpoint = apiErr.Request.URL.String()
		}
		switch {
		// 可恢复：临时网络波动、限流、网关超时
		case apiErr.StatusCode == 408 || apiErr.StatusCode == 429,
			apiErr.StatusCode == 502 || apiErr.StatusCode == 503 || apiErr.StatusCode == 504:
			return &ErrRecoverable{Err: err, Code: code, Message: message}
		// 不可恢复：请求参数错误、鉴权失败、端点不存在、服务端内部错误
		case apiErr.StatusCode == 400 || apiErr.StatusCode == 401 || apiErr.StatusCode == 403,
			apiErr.StatusCode == 404 || apiErr.StatusCode == 405 || apiErr.StatusCode == 500:
			return &ErrUnrecoverable{Err: err, StatusCode: statusCode, Code: code, Message: message, Endpoint: endpoint}
		default:
			return &ErrUnrecoverable{Err: err, StatusCode: statusCode, Code: code, Message: message, Endpoint: endpoint}
		}
	}
	// 网络错误等非 API 错误视为可恢复
	return &ErrRecoverable{Err: err}
}
