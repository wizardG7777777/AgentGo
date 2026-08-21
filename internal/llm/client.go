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
	Content string
	// Reasoning is the provider's plaintext reasoning exactly as returned by the
	// API. It is normalized from reasoning, reasoning_content, or readable
	// reasoning_details blocks; ExtraFields remains the protocol authority.
	Reasoning    string
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

// StreamEvent is a transport-level snapshot emitted while a streaming Chat
// Completions response is being accumulated. Content and provider-supplied raw
// reasoning are independent accumulated streams so UIs can label them clearly.
type StreamEvent struct {
	ContentDelta         string
	AccumulatedContent   string
	ReasoningDelta       string
	AccumulatedReasoning string
	Done                 bool
	Error                string
}

type streamHandlerKey struct{}

// modelOverrideKey 是 per-call 模型覆盖的 context 键。
// 用于 per-node 能力（model.NodeCapability.Model）：Agent 在任务入口把节点
// 指定的模型写入 ctx，SDKClient.Chat 读取后替换请求模型——wire 层模型不再
// 绑定在客户端构造期。未设置时行为与之前完全一致（用 c.model）。
type modelOverrideKey struct{}

// WithModelOverride 把本次调用链的 LLM 请求模型覆盖为 model。
// 空串时原样返回（不覆盖）。仅 SDKClient 生产路径消费该值；
// 测试 fake Client 忽略 ctx 时自然退回各自构造期模型。
func WithModelOverride(ctx context.Context, model string) context.Context {
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, modelOverrideKey{}, model)
}

// modelOverrideFromContext 读取 per-call 模型覆盖；未设置返回空串。
func modelOverrideFromContext(ctx context.Context) string {
	m, _ := ctx.Value(modelOverrideKey{}).(string)
	return m
}

// WithStreamHandler installs an optional synchronous observer for streamed
// reasoning and answer text. The handler must return quickly; callers that need
// throttling or fan-out should coalesce snapshots before publishing them to a UI.
func WithStreamHandler(ctx context.Context, handler func(StreamEvent)) context.Context {
	if handler == nil {
		return ctx
	}
	return context.WithValue(ctx, streamHandlerKey{}, handler)
}

func streamHandlerFromContext(ctx context.Context) func(StreamEvent) {
	handler, _ := ctx.Value(streamHandlerKey{}).(func(StreamEvent))
	return handler
}

// ClientConfig controls standard request behavior shared by every AgentGo LLM
// client created from the global llm block.
type ClientConfig struct {
	ReasoningEffort string
	Stream          bool
}

// SDKClient 通过 openai-go 官方 SDK 实现 Client 接口。
// 请求路径统一为 OpenAI-compatible Chat Completions（V6 起不再按 provider 分支）。
type SDKClient struct {
	client       openai.Client
	model        openai.ChatModel
	systemPrompt string
	request      ClientConfig
}

const defaultLLMTimeout = 120 * time.Second

// NewSDKClient 创建基于 openai-go SDK 的客户端。
// baseURL 为空时使用 OpenAI 官方端点。
// HTTP 层重试由 SDK 内部处理（429/5xx），此处不再额外设置 MaxRetries，
// 避免与调用方的业务重试语义重叠。
func NewSDKClient(baseURL, apiKey, model, systemPrompt string, timeout time.Duration) *SDKClient {
	return NewSDKClientWithConfig(baseURL, apiKey, model, systemPrompt, timeout, ClientConfig{})
}

// NewSDKClientWithConfig is the production constructor. NewSDKClient remains
// as a compatibility wrapper for focused tests and external package users.
func NewSDKClientWithConfig(baseURL, apiKey, model, systemPrompt string, timeout time.Duration, request ClientConfig) *SDKClient {
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
		request:      request,
	}
}

func (c *SDKClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	params := openai.ChatCompletionNewParams{
		Model: c.model,
	}
	// per-call 模型覆盖（per-node 能力）：ctx 携带时替换 wire 请求模型。
	if m := modelOverrideFromContext(ctx); m != "" {
		params.Model = openai.ChatModel(m)
	}
	if c.request.ReasoningEffort != "" {
		// ReasoningEffort is a string-backed SDK type. Casting keeps AgentGo
		// aligned with newly documented OpenAI values (for example "max") even
		// when the generated SDK constants lag the API specification.
		params.ReasoningEffort = shared.ReasoningEffort(c.request.ReasoningEffort)
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

	if c.request.Stream {
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
		return c.chatStreaming(ctx, params)
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
				// 载荷尺寸入错（2026-08-20 SWE-001 预防 1）：长 JSON 是损坏
				// 主因，重试交接时该事实帮助模型决定分批提交。
				return Response{}, &ErrBadResponse{
					Err: fmt.Errorf("tool call %q 参数解析失败（载荷 %d 字符）: %w",
						tc.Function.Name, len([]rune(tc.Function.Arguments)), err),
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
	result.Reasoning = ReasoningText(result.ExtraFields)

	return result, nil
}

// chatStreaming executes the same Chat Completions request over SSE and
// reconstructs the ordinary Response contract consumed by the ReAct loop.
// Tool calls are never dispatched from partial chunks: only the fully
// accumulated response is returned to the executor.
func (c *SDKClient) chatStreaming(ctx context.Context, params openai.ChatCompletionNewParams) (Response, error) {
	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	handler := streamHandlerFromContext(ctx)
	emitFailure := func(err error) (Response, error) {
		if handler != nil {
			handler(StreamEvent{Done: true, Error: err.Error()})
		}
		return Response{}, err
	}

	var acc openai.ChatCompletionAccumulator
	var accumulatedContent string
	var accumulatedReasoning string
	// The SDK accumulator intentionally ignores JSON metadata. Keep string
	// deltas by concatenation, append array-valued extension chunks in order,
	// and retain the last raw value for other extension-field shapes.
	extraStrings := make(map[string]string)
	extraArrays := make(map[string][]json.RawMessage)
	extraRaw := make(map[string]json.RawMessage)
	for stream.Next() {
		chunk := stream.Current()
		if field, ok := chunk.JSON.ExtraFields["error"]; ok && field.Raw() != "" {
			return emitFailure(&ErrRecoverable{Err: fmt.Errorf("流式响应返回 provider error: %s", field.Raw())})
		}
		if !acc.AddChunk(chunk) {
			return emitFailure(&ErrBadResponse{Err: errors.New("流式响应 chunk 无法按序聚合")})
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			chunkExtras := make(map[string]json.RawMessage, len(delta.JSON.ExtraFields))
			for key, field := range delta.JSON.ExtraFields {
				raw := field.Raw()
				if raw == "" {
					continue
				}
				chunkExtras[key] = json.RawMessage(raw)
				var fragment string
				if err := json.Unmarshal([]byte(raw), &fragment); err == nil {
					extraStrings[key] += fragment
					continue
				}
				var fragments []json.RawMessage
				if err := json.Unmarshal([]byte(raw), &fragments); err == nil {
					extraArrays[key] = append(extraArrays[key], fragments...)
					continue
				}
				extraRaw[key] = json.RawMessage(raw)
			}
			reasoningDelta := ReasoningText(chunkExtras)
			accumulatedContent += delta.Content
			accumulatedReasoning += reasoningDelta
			if handler != nil && (delta.Content != "" || reasoningDelta != "") {
				handler(StreamEvent{
					ContentDelta:         delta.Content,
					AccumulatedContent:   accumulatedContent,
					ReasoningDelta:       reasoningDelta,
					AccumulatedReasoning: accumulatedReasoning,
				})
			}
		}
	}
	if err := stream.Err(); err != nil {
		return emitFailure(classifySDKError(err))
	}
	if len(acc.Choices) == 0 {
		return emitFailure(&ErrUnrecoverable{Err: errors.New("LLM 流式响应返回空 choices")})
	}

	choice := acc.Choices[0]
	finishReason := parseFinishReason(string(choice.FinishReason))
	switch finishReason {
	case FinishReasonLength:
		return emitFailure(&ErrBadResponse{Err: errors.New("响应被截断 (finish_reason=length)")})
	case FinishReasonContentFilter:
		return emitFailure(&ErrUnrecoverable{Err: errors.New("响应被内容过滤器拦截 (finish_reason=content_filter)")})
	case FinishReasonUnknown:
		log.Printf("[llm] 警告: 流式响应未知 finish_reason=%q", choice.FinishReason)
	}

	var toolCalls []ToolCall
	for _, tc := range choice.Message.ToolCalls {
		args := make(map[string]any)
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				// 与非流式同口径：载荷尺寸入错（2026-08-20 SWE-001 预防 1）。
				return emitFailure(&ErrBadResponse{Err: fmt.Errorf("流式 tool call %q 参数解析失败（载荷 %d 字符）: %w",
					tc.Function.Name, len([]rune(tc.Function.Arguments)), err)})
			}
		}
		toolCalls = append(toolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}

	result := Response{
		Content:      choice.Message.Content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	result.Usage.PromptTokens = int(acc.Usage.PromptTokens)
	result.Usage.CompletionTokens = int(acc.Usage.CompletionTokens)
	if len(extraStrings)+len(extraArrays)+len(extraRaw) > 0 {
		result.ExtraFields = make(map[string]json.RawMessage, len(extraStrings)+len(extraArrays)+len(extraRaw))
		for key, value := range extraRaw {
			result.ExtraFields[key] = value
		}
		for key, value := range extraArrays {
			encoded, err := json.Marshal(value)
			if err != nil {
				return emitFailure(&ErrBadResponse{Err: fmt.Errorf("流式扩展数组字段 %q 聚合失败: %w", key, err)})
			}
			result.ExtraFields[key] = encoded
		}
		for key, value := range extraStrings {
			encoded, err := json.Marshal(value)
			if err != nil {
				return emitFailure(&ErrBadResponse{Err: fmt.Errorf("流式扩展字段 %q 聚合失败: %w", key, err)})
			}
			result.ExtraFields[key] = encoded
		}
	}
	result.Reasoning = ReasoningText(result.ExtraFields)
	if result.Reasoning == "" {
		result.Reasoning = accumulatedReasoning
	}
	if handler != nil {
		handler(StreamEvent{
			AccumulatedContent: result.Content, AccumulatedReasoning: result.Reasoning, Done: true,
		})
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
