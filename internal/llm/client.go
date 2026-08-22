package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"agentgo/internal/invocation"

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
	Reasoning string
	ToolCalls []ToolCall
	// Items 是服务端结构化信封经反序列化后的有序输出。它是行动身份的唯一
	// Model Invocation 权威；Content/Reasoning/ToolCalls 是下游兼容投影。
	Items        []OutputItem
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
	Protocol        Protocol
	ReasoningEffort string
	Stream          bool
	// ForcedToolName 只用于能力探针等机械协议调用；普通 Agent 留空并由模型
	// 按冻结 ToolRouter 自主选择。非空时 wire 使用 exact function tool_choice。
	ForcedToolName string
	// OutputBudget 是单次响应的冻结硬上限。零值使用 Model Invocation 的版本化
	// 安全默认值，不能解释为无限。
	OutputBudget invocation.OutputBudget
}

// SDKClient 通过 openai-go 官方 SDK 实现 Client 接口。生产请求 protocol 在
// 构造时冻结；Responses 为新主链，Chat Completions 只作显式兼容。
type SDKClient struct {
	client       openai.Client
	model        string
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
		model:        model,
		systemPrompt: systemPrompt,
		request:      request,
	}
}

func (c *SDKClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	if c.request.Protocol == ProtocolResponses {
		return c.responses(ctx, messages, tools)
	}
	// 空 protocol 只为直接构造客户端的旧测试/API 保留 Chat Completions
	// 兼容；生产 Runtime 必须由配置传入已冻结 protocol。
	if c.request.Protocol != "" && c.request.Protocol != ProtocolChatCompletions {
		return Response{}, fmt.Errorf("SDKClient 未知 protocol=%q", c.request.Protocol)
	}
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(c.model),
	}
	outputBudget := outputBudgetFromContext(ctx, c.request.OutputBudget)
	params.MaxCompletionTokens = openai.Int(outputBudget.MaxCompletionTokens)
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
	if c.request.ForcedToolName != "" {
		found := false
		for _, tool := range tools {
			if tool.Name == c.request.ForcedToolName {
				found = true
				break
			}
		}
		if !found {
			return Response{}, fmt.Errorf("forced tool_choice=%q 不在本次 ToolRouter 定义中", c.request.ForcedToolName)
		}
		params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(
			openai.ChatCompletionNamedToolChoiceFunctionParam{Name: c.request.ForcedToolName})
	}
	if binding, ok := invocation.ContextBindingFrom(ctx); ok && binding.ToolChoice.Mode != "" &&
		binding.ToolChoice.Mode != invocation.ToolChoiceAuto {
		if len(tools) == 0 {
			return Response{}, fmt.Errorf("tool_choice=%s 但本次 ToolRouter 为空", binding.ToolChoice.Mode)
		}
		switch binding.ToolChoice.Mode {
		case invocation.ToolChoiceRequired:
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoRequired)),
			}
		case invocation.ToolChoiceFunction:
			found := false
			for _, tool := range tools {
				if tool.Name == binding.ToolChoice.Name {
					found = true
					break
				}
			}
			if !found {
				return Response{}, fmt.Errorf("ContextBinding forced tool_choice=%q 不在本次 ToolRouter 定义中", binding.ToolChoice.Name)
			}
			params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(
				openai.ChatCompletionNamedToolChoiceFunctionParam{Name: binding.ToolChoice.Name})
		}
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
		return Response{}, classifySDKError(ctx, err)
	}

	if len(completion.Choices) == 0 {
		err := errors.New("LLM 返回空 choices")
		return Response{}, &ErrBadResponse{Err: err, Failure: invocation.NewFailure(
			invocation.FailureMalformedResponse, invocation.PhaseResponseValidate, invocation.OriginProtocol, err)}
	}

	choice := completion.Choices[0]
	budgetCounter := newOutputBudgetCounter(outputBudget, invocation.PhaseResponseValidate)
	if err := budgetCounter.addContent(choice.Message.Content); err != nil {
		return Response{}, err
	}
	for key, field := range choice.Message.JSON.ExtraFields {
		raw := field.Raw()
		if raw == "" {
			continue
		}
		extra := map[string]json.RawMessage{key: json.RawMessage(raw)}
		if err := budgetCounter.addExtra(key, raw, ReasoningText(extra)); err != nil {
			return Response{}, err
		}
	}
	for i, tc := range choice.Message.ToolCalls {
		if err := budgetCounter.addTool(int64(i), tc.Function.Name, tc.Function.Arguments); err != nil {
			return Response{}, err
		}
	}

	// 解析 FinishReason
	finishReason := parseFinishReason(string(choice.FinishReason))

	// 检查异常终止
	switch finishReason {
	case FinishReasonLength:
		log.Printf("[llm] 警告: 响应因 token 上限被截断 (finish_reason=length)")
		err := fmt.Errorf("响应被截断 (finish_reason=length)")
		failure := invocation.NewFailure(invocation.FailureOutputTruncated,
			invocation.PhaseResponseValidate, invocation.OriginProvider, err)
		failure.FinishReason = string(finishReason)
		return Response{FinishReason: finishReason}, &ErrBadResponse{
			Err: err, Failure: failure,
		}
	case FinishReasonContentFilter:
		log.Printf("[llm] 警告: 响应被内容过滤器拦截 (finish_reason=content_filter)")
		err := fmt.Errorf("响应被内容过滤器拦截 (finish_reason=content_filter)")
		failure := invocation.NewFailure(invocation.FailureContentFiltered,
			invocation.PhaseResponseValidate, invocation.OriginProvider, err)
		failure.FinishReason = string(finishReason)
		return Response{FinishReason: finishReason}, &ErrUnrecoverable{
			Err: err, Failure: failure,
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
				wrapped := fmt.Errorf("tool call %q 参数解析失败（载荷 %d 字符）: %w",
					tc.Function.Name, len([]rune(tc.Function.Arguments)), err)
				return Response{}, &ErrBadResponse{Err: wrapped, Failure: invocation.NewFailure(
					invocation.FailureMalformedResponse, invocation.PhaseToolCallValidate,
					invocation.OriginProtocol, wrapped)}
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
		Items:        chatCompletionOutputItems(choice.Message.Content, "", toolCalls),
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
	result.Items = chatCompletionOutputItems(result.Content, result.Reasoning, result.ToolCalls)

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
	budgetCounter := newOutputBudgetCounter(outputBudgetFromContext(ctx, c.request.OutputBudget), invocation.PhaseStreamAccumulate)
	// The SDK accumulator intentionally ignores JSON metadata. Keep string
	// deltas by concatenation, append array-valued extension chunks in order,
	// and retain the last raw value for other extension-field shapes.
	extraStrings := make(map[string]string)
	extraArrays := make(map[string][]json.RawMessage)
	extraRaw := make(map[string]json.RawMessage)
	for stream.Next() {
		chunk := stream.Current()
		if field, ok := chunk.JSON.ExtraFields["error"]; ok && field.Raw() != "" {
			return emitFailure(classifyStreamProviderError(field.Raw()))
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if err := budgetCounter.addContent(delta.Content); err != nil {
				return emitFailure(err)
			}
			for key, field := range delta.JSON.ExtraFields {
				raw := field.Raw()
				if raw == "" {
					continue
				}
				extra := map[string]json.RawMessage{key: json.RawMessage(raw)}
				if err := budgetCounter.addExtra(key, raw, ReasoningText(extra)); err != nil {
					return emitFailure(err)
				}
			}
			for _, tc := range delta.ToolCalls {
				if err := budgetCounter.addTool(tc.Index, tc.Function.Name, tc.Function.Arguments); err != nil {
					return emitFailure(err)
				}
			}
		}
		if !acc.AddChunk(chunk) {
			err := errors.New("流式响应 chunk 无法按序聚合")
			return emitFailure(&ErrBadResponse{Err: err, Failure: invocation.NewFailure(
				invocation.FailureMalformedResponse, invocation.PhaseStreamAccumulate,
				invocation.OriginProtocol, err)})
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
		return emitFailure(classifySDKError(ctx, err))
	}
	if len(acc.Choices) == 0 {
		err := errors.New("LLM 流式响应返回空 choices")
		return emitFailure(&ErrBadResponse{Err: err, Failure: invocation.NewFailure(
			invocation.FailureMalformedResponse, invocation.PhaseResponseValidate,
			invocation.OriginProtocol, err)})
	}

	choice := acc.Choices[0]
	finishReason := parseFinishReason(string(choice.FinishReason))
	switch finishReason {
	case FinishReasonLength:
		err := errors.New("响应被截断 (finish_reason=length)")
		failure := invocation.NewFailure(invocation.FailureOutputTruncated,
			invocation.PhaseResponseValidate, invocation.OriginProvider, err)
		failure.FinishReason = string(finishReason)
		return emitFailure(&ErrBadResponse{Err: err, Failure: failure})
	case FinishReasonContentFilter:
		err := errors.New("响应被内容过滤器拦截 (finish_reason=content_filter)")
		failure := invocation.NewFailure(invocation.FailureContentFiltered,
			invocation.PhaseResponseValidate, invocation.OriginProvider, err)
		failure.FinishReason = string(finishReason)
		return emitFailure(&ErrUnrecoverable{Err: err, Failure: failure})
	case FinishReasonUnknown:
		log.Printf("[llm] 警告: 流式响应未知 finish_reason=%q", choice.FinishReason)
	}

	var toolCalls []ToolCall
	for _, tc := range choice.Message.ToolCalls {
		args := make(map[string]any)
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				// 与非流式同口径：载荷尺寸入错（2026-08-20 SWE-001 预防 1）。
				wrapped := fmt.Errorf("流式 tool call %q 参数解析失败（载荷 %d 字符）: %w",
					tc.Function.Name, len([]rune(tc.Function.Arguments)), err)
				return emitFailure(&ErrBadResponse{Err: wrapped, Failure: invocation.NewFailure(
					invocation.FailureMalformedResponse, invocation.PhaseToolCallValidate,
					invocation.OriginProtocol, wrapped)})
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
				wrapped := fmt.Errorf("流式扩展数组字段 %q 聚合失败: %w", key, err)
				return emitFailure(&ErrBadResponse{Err: wrapped, Failure: invocation.NewFailure(
					invocation.FailureMalformedResponse, invocation.PhaseStreamAccumulate,
					invocation.OriginProtocol, wrapped)})
			}
			result.ExtraFields[key] = encoded
		}
		for key, value := range extraStrings {
			encoded, err := json.Marshal(value)
			if err != nil {
				wrapped := fmt.Errorf("流式扩展字段 %q 聚合失败: %w", key, err)
				return emitFailure(&ErrBadResponse{Err: wrapped, Failure: invocation.NewFailure(
					invocation.FailureMalformedResponse, invocation.PhaseStreamAccumulate,
					invocation.OriginProtocol, wrapped)})
			}
			result.ExtraFields[key] = encoded
		}
	}
	result.Reasoning = ReasoningText(result.ExtraFields)
	if result.Reasoning == "" {
		result.Reasoning = accumulatedReasoning
	}
	result.Items = chatCompletionOutputItems(result.Content, result.Reasoning, result.ToolCalls)
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
					wrapped := fmt.Errorf("序列化 tool call %q 参数失败: %w", tc.Name, err)
					return openai.ChatCompletionMessageParamUnion{}, &ErrBadResponse{
						Err: wrapped,
						Failure: invocation.NewFailure(invocation.FailureInvalidRequest,
							invocation.PhaseRequestEncode, invocation.OriginRuntime, wrapped),
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
				if k == responsesOutputItemsExtraField {
					continue
				}
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

func chatCompletionOutputItems(content, reasoning string, toolCalls []ToolCall) []OutputItem {
	items := make([]OutputItem, 0, 2+len(toolCalls))
	if reasoning != "" {
		items = append(items, OutputItem{Kind: OutputItemReasoning, Reasoning: reasoning})
	}
	if content != "" {
		items = append(items, OutputItem{Kind: OutputItemMessage, Text: content})
	}
	for i := range toolCalls {
		call := toolCalls[i]
		items = append(items, OutputItem{Kind: OutputItemFunctionCall, ID: call.ID, ToolCall: &call})
	}
	return items
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

// classifySDKError 将 SDK/transport 错误规范化为 InvocationFailure。
//
// ErrRecoverable/ErrUnrecoverable 仅保留为迁移期兼容包装；Failure.Kind 才是跨层
// 权威事实。L4 决定是否重试，不能再从 error 文本猜测。
func classifySDKError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			failure := invocation.NewFailure(invocation.FailureCallerCancelled,
				invocation.PhaseRequestSend, invocation.OriginCaller, cause)
			failure.TimeoutScope = invocation.TimeoutCaller
			failure.Partial = true
			failure.UsageState = invocation.UsagePartial
			return &ErrUnrecoverable{Err: err, Failure: failure}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind := invocation.FailureActivationDeadline
			scope := invocation.TimeoutActivation
			switch {
			case errors.Is(cause, invocation.ErrAttemptDeadline):
				kind = invocation.FailureAttemptDeadline
				scope = invocation.TimeoutAttempt
			case errors.Is(cause, invocation.ErrGraphDeadline):
				scope = invocation.TimeoutGraph
			case errors.Is(cause, invocation.ErrRunDeadline):
				scope = invocation.TimeoutRun
			case errors.Is(cause, invocation.ErrActivationDeadline):
				scope = invocation.TimeoutActivation
			}
			failure := invocation.NewFailure(kind,
				invocation.PhaseRequestSend, invocation.OriginRuntime, cause)
			failure.TimeoutScope = scope
			failure.Partial = true
			failure.UsageState = invocation.UsagePartial
			return &ErrUnrecoverable{Err: err, Failure: failure}
		}
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		code := apiErr.Code
		message := apiErr.Message
		statusCode := apiErr.StatusCode
		endpoint := ""
		if apiErr.Request != nil && apiErr.Request.URL != nil {
			endpoint = apiErr.Request.URL.String()
		}
		failure := invocation.NewFailure(invocation.FailureUnknown,
			invocation.PhaseResponseHeaders, invocation.OriginProvider, err)
		failure.ProviderCode = code
		failure.HTTPStatus = statusCode

		switch normalized := strings.ToLower(strings.TrimSpace(code)); {
		case normalized == "context_length_exceeded",
			normalized == "maximum_context_length_exceeded",
			normalized == "context_window_exceeded":
			failure.Kind = invocation.FailureContextWindowExceeded
			return &ErrUnrecoverable{Err: err, StatusCode: statusCode, Code: code,
				Message: message, Endpoint: endpoint, Failure: failure}
		case statusCode == 408:
			failure.Kind = invocation.FailureRequestTimeout
			failure.TimeoutScope = invocation.TimeoutInvocation
			return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
		case statusCode == 429:
			failure.Kind = invocation.FailureRateLimited
			return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
		case statusCode == 502 || statusCode == 503 || statusCode == 504:
			failure.Kind = invocation.FailureProviderUnavailable
			return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
		case statusCode == 500:
			// 外壳类型只保留 API 兼容；internal L4 依据 canonical kind
			// provider_unavailable 做唯一恢复决策。
			failure.Kind = invocation.FailureProviderUnavailable
		case statusCode == 401:
			failure.Kind = invocation.FailureAuth
		case statusCode == 403:
			failure.Kind = invocation.FailurePermissionDenied
		case statusCode == 404:
			if normalized == "model_not_found" {
				failure.Kind = invocation.FailureModelUnavailable
			} else {
				failure.Kind = invocation.FailureInvalidRequest
			}
		case statusCode == 400 || statusCode == 405:
			failure.Kind = invocation.FailureInvalidRequest
		default:
			failure.Kind = invocation.FailureUnknown
		}
		return &ErrUnrecoverable{Err: err, StatusCode: statusCode, Code: code,
			Message: message, Endpoint: endpoint, Failure: failure}
	}

	if errors.Is(err, context.Canceled) {
		failure := invocation.NewFailure(invocation.FailureCallerCancelled,
			invocation.PhaseRequestSend, invocation.OriginCaller, err)
		failure.TimeoutScope = invocation.TimeoutCaller
		failure.Partial = true
		failure.UsageState = invocation.UsagePartial
		return &ErrUnrecoverable{Err: err, Failure: failure}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		failure := invocation.NewFailure(invocation.FailureRequestTimeout,
			invocation.PhaseRequestSend, invocation.OriginTransport, err)
		failure.TimeoutScope = invocation.TimeoutInvocation
		failure.Partial = true
		failure.UsageState = invocation.UsagePartial
		return &ErrRecoverable{Err: err, Failure: failure}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		failure := invocation.NewFailure(invocation.FailureTransport,
			invocation.PhaseConnect, invocation.OriginTransport, err)
		return &ErrRecoverable{Err: err, Failure: failure}
	}

	// SDK 未暴露足够结构化信息时保持 unknown。外壳保留兼容，但 internal
	// L4 不得据此覆盖 canonical RecoveryRequestIntervene。
	failure := invocation.NewFailure(invocation.FailureUnknown,
		invocation.PhaseRequestSend, invocation.OriginRuntime, err)
	return &ErrRecoverable{Err: err, Failure: failure}
}

func classifyStreamProviderError(raw string) error {
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	code := strings.TrimSpace(envelope.Code)
	message := strings.TrimSpace(envelope.Message)
	if code == "" {
		code = strings.TrimSpace(envelope.Error.Code)
	}
	if message == "" {
		message = strings.TrimSpace(envelope.Error.Message)
	}
	message = boundedDiagnostic(message, 256)
	diagnostic := "流式响应返回 provider error"
	if code != "" {
		diagnostic += " code=" + code
	}
	if message != "" {
		diagnostic += ": " + message
	} else {
		diagnostic += fmt.Sprintf("（载荷 %d 字节，正文已脱敏）", len([]byte(raw)))
	}
	err := errors.New(diagnostic)
	failure := invocation.NewFailure(invocation.FailureUnknown,
		invocation.PhaseStreamReceive, invocation.OriginProvider, err)
	failure.ProviderCode = code

	switch strings.ToLower(code) {
	case "context_length_exceeded", "maximum_context_length_exceeded", "context_window_exceeded":
		failure.Kind = invocation.FailureContextWindowExceeded
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "rate_limit_exceeded":
		failure.Kind = invocation.FailureRateLimited
	case "invalid_api_key", "authentication_error":
		failure.Kind = invocation.FailureAuth
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "model_not_found":
		failure.Kind = invocation.FailureModelUnavailable
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "server_error", "service_unavailable":
		failure.Kind = invocation.FailureProviderUnavailable
	default:
		failure.Kind = invocation.FailureUnknown
	}
	// 外壳仅为兼容；internal L4 只看 FailureKind。
	return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
}

func boundedDiagnostic(s string, maxRunes int) string {
	if maxRunes <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
