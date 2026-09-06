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
	Parts      []ContentPart   `json:"parts,omitempty"`
	Replay     *ProtocolReplay `json:"replay,omitempty"`
	Role       string          // "system" | "user" | "assistant" | "tool"
	Content    string          // 消息内容
	Name       string          // 工具名称，仅 role="tool" 时使用
	ToolCallID string          // 对应的 tool_call ID，仅 role="tool" 时使用
	ToolCalls  []ToolCall      // LLM 返回的工具调用，仅 role="assistant" 时使用

}

// ToolDef 描述一个可供 LLM 调用的工具。
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema
	// Strict 只在 schema 已经满足 provider strict 子集时开启。普通
	// AgentGo 工具保持 false；启动 nonce capability probe 使用 true。
	Strict bool
}

// ToolCall 是 LLM 返回的结构化工具调用请求。
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// responseData 是解析后的 LLM 响应。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
}

type responseData struct {
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
	Usage        Usage
	// ExtraFields 是 assistant 消息里的非标字段（如 DeepSeek V4 的 reasoning_content）。
	// 调用方应把这份 map 挂到随后追加进历史的 Message 上，确保下一轮请求能原样回传。
	ExtraFields map[string]json.RawMessage
}

func hasToolDefinition(tools []ToolDef, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// Transport 仅持有连接；每次调用的业务参数来自封存 Request。
type Transport struct{ client openai.Client }
type TransportConfig struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

func NewTransport(config TransportConfig) *Transport {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	opts := []option.RequestOption{option.WithRequestTimeout(timeout), option.WithMaxRetries(0)}
	if config.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(config.BaseURL))
	}
	if config.APIKey != "" {
		opts = append(opts, option.WithAPIKey(config.APIKey))
	}
	return &Transport{client: openai.NewClient(opts...)}
}

// protocolCall 的生命周期仅限一次请求，不共享或补充业务默认值。
type protocolCall struct {
	client   openai.Client
	request  Options
	sink     EventSink
	identity Identity
}

func (t *Transport) Invoke(ctx context.Context, request Request, sink EventSink) (Result, error) {
	if err := request.Validate(); err != nil {
		return Result{}, NewFailure(FailureInvalidRequest, PhaseRequestBuild, OriginRuntime, err)
	}
	spec := request.Spec()
	call := protocolCall{client: t.client, request: spec.Options, sink: sink, identity: spec.Identity}
	var response responseData
	var err error
	switch spec.Options.Protocol {
	case ProtocolResponses:
		response, err = call.responses(ctx, spec.Messages, spec.Tools)
	case ProtocolChatCompletions:
		response, err = call.chat(ctx, spec.Messages, spec.Tools)
	}
	if err != nil {
		if f, ok := FromError(err); ok {
			f.InvocationID = spec.Identity.InvocationID
			f.SnapshotID = spec.Identity.SnapshotID
			f.ProviderPolicy = spec.Identity.ContextPolicyID
		}
		return Result{}, err
	}
	replay := ProtocolReplay{Protocol: spec.Options.Protocol}
	if spec.Options.Protocol == ProtocolResponses {
		for _, item := range response.Items {
			if len(item.Raw) > 0 {
				replay.Items = append(replay.Items, item.Raw)
			}
		}
	} else {
		replay.Fields = response.ExtraFields
	}
	return NewResult(ResultData{Schema: ResultSchema, Items: response.Items, FinishReason: response.FinishReason, Usage: response.Usage, Replay: replay})
}
func (c *protocolCall) emit(index int64, id, kind, delta string) {
	if c.sink != nil && delta != "" {
		c.sink(Event{InvocationID: c.identity.InvocationID, ItemIndex: index, ItemID: id, Kind: kind, Delta: delta})
	}
}

func (c *protocolCall) chat(ctx context.Context, messages []Message, tools []ToolDef) (responseData, error) {
	params, err := chatParams(c.request, messages, tools)
	if err != nil {
		return responseData{}, err
	}
	return c.chatStreaming(ctx, params)
}
func chatParams(options Options, messages []Message, tools []ToolDef) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(options.Model),
	}
	outputBudget := options.OutputBudget
	params.MaxCompletionTokens = openai.Int(outputBudget.MaxCompletionTokens)
	if reasoningEffort := options.ReasoningEffort; reasoningEffort != "" {
		// ReasoningEffort is a string-backed SDK type. Casting keeps AgentGo
		// aligned with newly documented OpenAI values (for example "max") even
		// when the generated SDK constants lag the API specification.
		params.ReasoningEffort = shared.ReasoningEffort(reasoningEffort)
	}

	// 转换消息
	for _, m := range messages {
		msg, err := convertMessage(m)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		params.Messages = append(params.Messages, msg)
	}

	// 转换工具定义
	for _, t := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Name,
					Strict:      openai.Bool(t.Strict),
					Description: openai.String(t.Description),
					Parameters:  shared.FunctionParameters(t.Parameters),
				},
			},
		})
	}
	toolChoice := options.ToolChoice
	params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")}
	if err := toolChoice.Validate(); err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("tool_choice 无效: %w", err)
	}
	if toolChoice.Mode != ToolChoiceAuto {
		if len(tools) == 0 {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("tool_choice=%s 但本次 ToolRouter 为空", toolChoice.Mode)
		}
		switch toolChoice.Mode {
		case ToolChoiceRequired:
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoRequired)),
			}
		case ToolChoiceFunction:
			if !hasToolDefinition(tools, toolChoice.Name) {
				return openai.ChatCompletionNewParams{}, fmt.Errorf("forced tool_choice=%q 不在本次 ToolRouter 定义中", toolChoice.Name)
			}
			params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(
				openai.ChatCompletionNamedToolChoiceFunctionParam{Name: toolChoice.Name})
		}
	}

	params.ParallelToolCalls = openai.Bool(options.ParallelToolCalls)
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	return params, nil
}

// chatStreaming 通过 SSE 执行 Chat Completions 请求并聚合协议输出。
// 增量仅供观察；完整结果交回 L2 检查与记录后，L3 才能执行工具。
func (c *protocolCall) chatStreaming(ctx context.Context, params openai.ChatCompletionNewParams) (responseData, error) {
	witness, streamOption := streamWitness(c.request.OutputBudget)
	stream := c.client.Chat.Completions.NewStreaming(ctx, params, streamOption)
	defer stream.Close()

	emitFailure := func(err error) (responseData, error) { return responseData{}, err }
	var acc openai.ChatCompletionAccumulator
	var accumulatedContent string
	var accumulatedReasoning string
	budgetCounter := newOutputBudgetCounter(c.request.OutputBudget, PhaseStreamAccumulate)
	// The SDK accumulator intentionally ignores JSON metadata. Keep string
	// deltas by concatenation, append array-valued extension chunks in order,
	// and retain the last raw value for other extension-field shapes.
	extraStrings := make(map[string]string)
	extraArrays := make(map[string][]json.RawMessage)
	extraRaw := make(map[string]json.RawMessage)
	for stream.Next() {
		if ctx.Err() != nil {
			return emitFailure(classifySDKError(ctx, ctx.Err()))
		}
		observeStreamEvent(ctx, "chat.chunk")
		chunk := stream.Current()
		if field, ok := chunk.JSON.ExtraFields["error"]; ok && field.Raw() != "" {
			return emitFailure(classifyStreamProviderError(field.Raw()))
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return emitFailure(responsesProtocolError("Chat SSE 返回未请求的候选", nil))
			}
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
				observeStreamDelta(ctx, "tool", tc.Function.Name != "" || tc.Function.Arguments != "")
				c.emit(tc.Index, tc.ID, "tool_arguments_delta", tc.Function.Arguments)
				if err := budgetCounter.addTool(tc.Index, tc.Function.Name, tc.Function.Arguments); err != nil {
					return emitFailure(err)
				}
			}
		}
		if !acc.AddChunk(chunk) {
			err := errors.New("流式响应 chunk 无法按序聚合")
			return emitFailure(&ErrBadResponse{Err: err, Failure: NewFailure(
				FailureMalformedResponse, PhaseStreamAccumulate,
				OriginProtocol, err)})
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
			observeStreamDelta(ctx, "text", delta.Content != "")
			observeStreamDelta(ctx, "reasoning", reasoningDelta != "")
			accumulatedContent += delta.Content
			accumulatedReasoning += reasoningDelta
			c.emit(choice.Index, "", "text_delta", delta.Content)
			c.emit(choice.Index, "", "reasoning_delta", reasoningDelta)
		}
	}
	if err := stream.Err(); err != nil {
		return emitFailure(classifySDKError(ctx, err))
	}
	if !witness.done {
		return emitFailure(responsesProtocolError("Chat SSE 缺少 [DONE] 终止事件", nil))
	}
	if len(acc.Choices) == 0 {
		err := errors.New("LLM 流式响应返回空 choices")
		return emitFailure(&ErrBadResponse{Err: err, Failure: NewFailure(
			FailureMalformedResponse, PhaseResponseValidate,
			OriginProtocol, err)})
	}

	choice := acc.Choices[0]
	finishReason := parseFinishReason(string(choice.FinishReason))
	switch finishReason {
	case FinishReasonLength:
		err := errors.New("响应被截断 (finish_reason=length)")
		failure := NewFailure(FailureOutputTruncated,
			PhaseResponseValidate, OriginProvider, err)
		failure.FinishReason = string(finishReason)
		return emitFailure(&ErrBadResponse{Err: err, Failure: failure})
	case FinishReasonContentFilter:
		err := errors.New("响应被内容过滤器拦截 (finish_reason=content_filter)")
		failure := NewFailure(FailureContentFiltered,
			PhaseResponseValidate, OriginProvider, err)
		failure.FinishReason = string(finishReason)
		return emitFailure(&ErrUnrecoverable{Err: err, Failure: failure})
	case FinishReasonUnknown:
		return emitFailure(responsesProtocolError("Chat SSE 缺少合法 finish_reason", nil))
	}

	var toolCalls []ToolCall
	for _, tc := range choice.Message.ToolCalls {
		args := make(map[string]any)
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				// 错误中仅记录参数载荷尺寸，便于诊断截断或非法 JSON。
				wrapped := fmt.Errorf("流式 tool call %q 参数解析失败（载荷 %d 字符）: %w",
					tc.Function.Name, len([]rune(tc.Function.Arguments)), err)
				return emitFailure(&ErrBadResponse{Err: wrapped, Failure: NewFailure(
					FailureMalformedResponse, PhaseToolCallValidate,
					OriginProtocol, wrapped)})
			}
		}
		toolCalls = append(toolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}

	result := responseData{
		Content:      choice.Message.Content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	result.Usage.PromptTokens = int(acc.Usage.PromptTokens)
	result.Usage.CompletionTokens = int(acc.Usage.CompletionTokens)
	result.Usage.ReasoningTokens = int(acc.Usage.CompletionTokensDetails.ReasoningTokens)
	if len(extraStrings)+len(extraArrays)+len(extraRaw) > 0 {
		result.ExtraFields = make(map[string]json.RawMessage, len(extraStrings)+len(extraArrays)+len(extraRaw))
		for key, value := range extraRaw {
			result.ExtraFields[key] = value
		}
		for key, value := range extraArrays {
			encoded, err := json.Marshal(value)
			if err != nil {
				wrapped := fmt.Errorf("流式扩展数组字段 %q 聚合失败: %w", key, err)
				return emitFailure(&ErrBadResponse{Err: wrapped, Failure: NewFailure(
					FailureMalformedResponse, PhaseStreamAccumulate,
					OriginProtocol, wrapped)})
			}
			result.ExtraFields[key] = encoded
		}
		for key, value := range extraStrings {
			encoded, err := json.Marshal(value)
			if err != nil {
				wrapped := fmt.Errorf("流式扩展字段 %q 聚合失败: %w", key, err)
				return emitFailure(&ErrBadResponse{Err: wrapped, Failure: NewFailure(
					FailureMalformedResponse, PhaseStreamAccumulate,
					OriginProtocol, wrapped)})
			}
			result.ExtraFields[key] = encoded
		}
	}
	result.Reasoning = ReasoningText(result.ExtraFields)
	if result.Reasoning == "" {
		result.Reasoning = accumulatedReasoning
	}
	result.Items = chatCompletionOutputItems(result.Content, result.Reasoning, result.ToolCalls)
	markStreamCompleted(ctx)
	return result, nil
}

// convertMessage 将内部 Message 转换为 SDK 的消息类型。
// 遇到未知 role 时返回 ErrUnknownRole 而非静默降级。
func convertMessage(m Message) (openai.ChatCompletionMessageParamUnion, error) {
	if len(m.Parts) > 0 {
		return chatParts(m)
	}
	switch m.Role {
	case "system":
		return openai.SystemMessage(m.Content), nil
	case "developer":
		return openai.DeveloperMessage(m.Content), nil
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
						Failure: NewFailure(FailureInvalidRequest,
							PhaseRequestEncode, OriginRuntime, wrapped),
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
		if m.Replay != nil && len(m.Replay.Fields) > 0 {
			extras := make(map[string]any, len(m.Replay.Fields))
			for k, v := range m.Replay.Fields {
				if k == ReplayItemsBudgetKey {
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
			failure := NewFailure(FailureCallerCancelled,
				PhaseRequestSend, OriginCaller, cause)
			failure.TimeoutScope = TimeoutCaller
			failure.Partial = true
			failure.UsageState = UsagePartial
			return &ErrUnrecoverable{Err: err, Failure: failure}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind := FailureActivationDeadline
			scope := TimeoutActivation
			switch {
			case errors.Is(cause, ErrAttemptDeadline):
				kind = FailureAttemptDeadline
				scope = TimeoutAttempt
			case errors.Is(cause, ErrGraphDeadline):
				scope = TimeoutGraph
			case errors.Is(cause, ErrRunDeadline):
				scope = TimeoutRun
			case errors.Is(cause, ErrActivationDeadline):
				scope = TimeoutActivation
			}
			failure := NewFailure(kind,
				PhaseRequestSend, OriginRuntime, cause)
			failure.TimeoutScope = scope
			failure.Partial = true
			failure.UsageState = UsagePartial
			return &ErrUnrecoverable{Err: err, Failure: failure}
		}
	}

	if _, ok := FromError(err); ok {
		return err
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
		failure := NewFailure(FailureUnknown,
			PhaseResponseHeaders, OriginProvider, err)
		failure.ProviderCode = code
		failure.HTTPStatus = statusCode

		switch normalized := strings.ToLower(strings.TrimSpace(code)); {
		case normalized == "context_length_exceeded",
			normalized == "maximum_context_length_exceeded",
			normalized == "context_window_exceeded":
			failure.Kind = FailureContextWindowExceeded
			return &ErrUnrecoverable{Err: err, StatusCode: statusCode, Code: code,
				Message: message, Endpoint: endpoint, Failure: failure}
		case statusCode == 408:
			failure.Kind = FailureRequestTimeout
			failure.TimeoutScope = TimeoutInvocation
			return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
		case statusCode == 429:
			failure.Kind = FailureRateLimited
			return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
		case statusCode == 402,
			normalized == "insufficient_quota",
			normalized == "insufficient_balance",
			normalized == "billing_hard_limit_reached",
			normalized == "billing_not_active":
			failure.Kind = FailureProviderQuotaExhausted
		case statusCode == 502 || statusCode == 503 || statusCode == 504:
			failure.Kind = FailureProviderUnavailable
			return &ErrRecoverable{Err: err, Code: code, Message: message, Failure: failure}
		case statusCode == 500:
			// 外壳类型只保留 API 兼容；internal L4 依据 canonical kind
			// provider_unavailable 做唯一恢复决策。
			failure.Kind = FailureProviderUnavailable
		case statusCode == 401:
			failure.Kind = FailureAuth
		case statusCode == 403:
			failure.Kind = FailurePermissionDenied
		case statusCode == 404:
			if normalized == "model_not_found" {
				failure.Kind = FailureModelUnavailable
			} else {
				failure.Kind = FailureInvalidRequest
			}
		case statusCode == 400 || statusCode == 405:
			failure.Kind = FailureInvalidRequest
		default:
			failure.Kind = FailureUnknown
		}
		return &ErrUnrecoverable{Err: err, StatusCode: statusCode, Code: code,
			Message: message, Endpoint: endpoint, Failure: failure}
	}

	if errors.Is(err, context.Canceled) {
		failure := NewFailure(FailureCallerCancelled,
			PhaseRequestSend, OriginCaller, err)
		failure.TimeoutScope = TimeoutCaller
		failure.Partial = true
		failure.UsageState = UsagePartial
		return &ErrUnrecoverable{Err: err, Failure: failure}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		failure := NewFailure(FailureRequestTimeout,
			PhaseRequestSend, OriginTransport, err)
		failure.TimeoutScope = TimeoutInvocation
		failure.Partial = true
		failure.UsageState = UsagePartial
		return &ErrRecoverable{Err: err, Failure: failure}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		failure := NewFailure(FailureTransport,
			PhaseConnect, OriginTransport, err)
		return &ErrRecoverable{Err: err, Failure: failure}
	}

	// SDK 未暴露足够结构化信息时保持 unknown。外壳保留兼容，但 internal
	// L4 不得据此覆盖 canonical RecoveryRequestIntervene。
	failure := NewFailure(FailureUnknown,
		PhaseRequestSend, OriginRuntime, err)
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
	failure := NewFailure(FailureUnknown,
		PhaseStreamReceive, OriginProvider, err)
	failure.ProviderCode = code

	switch strings.ToLower(code) {
	case "context_length_exceeded", "maximum_context_length_exceeded", "context_window_exceeded":
		failure.Kind = FailureContextWindowExceeded
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "rate_limit_exceeded":
		failure.Kind = FailureRateLimited
	case "insufficient_quota", "insufficient_balance", "billing_hard_limit_reached", "billing_not_active":
		failure.Kind = FailureProviderQuotaExhausted
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "invalid_api_key", "authentication_error":
		failure.Kind = FailureAuth
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "model_not_found":
		failure.Kind = FailureModelUnavailable
		return &ErrUnrecoverable{Err: err, Code: code, Message: message, Failure: failure}
	case "server_error", "service_unavailable":
		failure.Kind = FailureProviderUnavailable
	default:
		failure.Kind = FailureUnknown
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
