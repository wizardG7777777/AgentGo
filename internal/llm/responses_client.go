package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// responses 执行 OpenAI Responses typed-item 主链。工具身份只由
// response.output_item.done.item.type=function_call 产生；正文中的任何标记
// 都只保留为 message，不参与工具识别。
func (c *protocolCall) responses(ctx context.Context, messages []Message, tools []ToolDef) (responseData, error) {
	params, err := c.responsesParams(ctx, messages, tools)
	if err != nil {
		return responseData{}, err
	}
	return c.responsesStreaming(ctx, params)
}

func (c *protocolCall) responsesParams(ctx context.Context, messages []Message, tools []ToolDef) (responses.ResponseNewParams, error) {
	return responsesParams(c.request, messages, tools)
}
func responsesParams(options Options, messages []Message, tools []ToolDef) (responses.ResponseNewParams, error) {
	model := options.Model
	budget := options.OutputBudget
	params := responses.ResponseNewParams{
		Model:             shared.ResponsesModel(model),
		MaxOutputTokens:   openai.Int(budget.MaxCompletionTokens),
		ParallelToolCalls: openai.Bool(options.ParallelToolCalls),
		Store:             openai.Bool(false),
		Include:           []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Truncation:        responses.ResponseNewParamsTruncationDisabled,
	}
	if reasoningEffort := options.ReasoningEffort; reasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(reasoningEffort)}
	}
	input, err := convertResponsesMessages(messages)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}
	for _, tool := range tools {
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: tool.Name, Description: openai.String(tool.Description), Parameters: tool.Parameters,
			// AgentGo 的现有 schema 允许可选字段和 L3 默认参数；关闭服务端 strict
			// 不等于放松 Harness，最终参数仍由 typed decoder、ToolRouter 与 Gate 校验。
			Strict: openai.Bool(tool.Strict),
		}})
	}
	choice, err := responseToolChoice(options.ToolChoice, tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params.ToolChoice = choice
	return params, nil
}

func responseToolChoice(choice ToolChoice, tools []ToolDef) (responses.ResponseNewParamsToolChoiceUnion, error) {
	if err := choice.Validate(); err != nil {
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("tool_choice 无效: %w", err)
	}
	if choice.Mode == ToolChoiceAuto {
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto)}, nil
	}
	if len(tools) == 0 {
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("tool_choice=%s 但本次 ToolRouter 为空", choice.Mode)
	}
	switch choice.Mode {
	case ToolChoiceRequired:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsRequired)}, nil
	case ToolChoiceFunction:
		if !hasToolDefinition(tools, choice.Name) {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("forced tool_choice=%q 不在本次 ToolRouter 定义中", choice.Name)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice.Name}}, nil
	default:
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("未知 tool_choice mode=%q", choice.Mode)
	}
}

func convertResponsesMessages(messages []Message) (responses.ResponseInputParam, error) {
	input := make(responses.ResponseInputParam, 0, len(messages)+1)
	appendText := func(role, content string) {
		input = append(input, responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
			Role:    responses.EasyInputMessageRole(role),
			Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(content)},
		}})
	}
	for _, message := range messages {
		if len(message.Parts) > 0 {
			item, err := responseParts(message)
			if err != nil {
				return nil, err
			}
			input = append(input, item)
			continue
		}
		switch message.Role {
		case "system", "developer", "user":
			appendText(message.Role, message.Content)
		case "assistant":
			if message.Replay != nil && len(message.Replay.Items) > 0 {
				rawItems := message.Replay.Items
				for index, rawItem := range rawItems {
					item, err := exactResponsesReplayItem(rawItem, index)
					if err != nil {
						return nil, err
					}
					input = append(input, item)
				}
				continue
			}
			if message.Replay != nil && len(message.Replay.Fields) > 0 {
				return nil, responsesProtocolError("Responses 请求遇到未类型化的 assistant extra fields", nil)
			}
			if message.Content != "" {
				appendText("assistant", message.Content)
			}
			for _, call := range message.ToolCalls {
				arguments, err := json.Marshal(call.Arguments)
				if err != nil {
					return nil, responsesProtocolError("序列化历史 function_call arguments 失败", err)
				}
				input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID: call.ID, Name: call.Name, Arguments: string(arguments),
				}})
			}
		case "tool":
			if strings.TrimSpace(message.ToolCallID) == "" {
				return nil, responsesProtocolError("function_call_output 缺少 call_id", nil)
			}
			input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: message.ToolCallID,
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: openai.String(message.Content)},
			}})
		default:
			return nil, &ErrUnknownRole{Role: message.Role}
		}
	}
	return input, nil
}

// exactResponsesReplayItem 先按 Responses 强类型信封校验服务端 output item，
// 再通过 SDK 的 raw override 原样放回下一轮 input。不能把 output struct 普通
// 反序列化后再序列化：SDK 的 omitzero 会删除服务端明确返回的空 required 字段
// （例如 message.content=[]），使无状态 provider 在第二轮拒绝 replay。
func exactResponsesReplayItem(rawItem json.RawMessage, index int) (responses.ResponseInputItemUnionParam, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawItem, &header); err != nil {
		return responses.ResponseInputItemUnionParam{}, responsesProtocolError(
			fmt.Sprintf("Responses replay item[%d] 信封无效", index), err)
	}
	var typed responses.ResponseInputItemUnionParam
	if err := json.Unmarshal(rawItem, &typed); err != nil {
		return responses.ResponseInputItemUnionParam{}, responsesProtocolError(
			fmt.Sprintf("Responses replay item[%d] 无法反序列化", index), err)
	}
	switch header.Type {
	case "message":
		if typed.OfMessage == nil && typed.OfInputMessage == nil && typed.OfOutputMessage == nil {
			return responses.ResponseInputItemUnionParam{}, responsesProtocolError(
				fmt.Sprintf("Responses replay item[%d] message 未形成强类型变体", index), nil)
		}
	case "reasoning":
		if typed.OfReasoning == nil {
			return responses.ResponseInputItemUnionParam{}, responsesProtocolError(
				fmt.Sprintf("Responses replay item[%d] reasoning 未形成强类型变体", index), nil)
		}
	case "function_call":
		if typed.OfFunctionCall == nil {
			return responses.ResponseInputItemUnionParam{}, responsesProtocolError(
				fmt.Sprintf("Responses replay item[%d] function_call 未形成强类型变体", index), nil)
		}
	default:
		return responses.ResponseInputItemUnionParam{}, responsesProtocolError(
			fmt.Sprintf("Responses replay item[%d] type=%q 不在 AgentGo profile", index, header.Type), nil)
	}
	exact := append(json.RawMessage(nil), rawItem...)
	return param.Override[responses.ResponseInputItemUnionParam](exact), nil
}

func (c *protocolCall) responsesStreaming(ctx context.Context, params responses.ResponseNewParams) (responseData, error) {
	_, streamOption := streamWitness(c.request.OutputBudget)
	stream := c.client.Responses.NewStreaming(ctx, params, streamOption)
	defer stream.Close()
	emitFailure := func(err error) (responseData, error) { return responseData{}, err }

	budget := newOutputBudgetCounter(c.request.OutputBudget, PhaseStreamAccumulate)
	itemsByIndex := make(map[int64]responses.ResponseOutputItemUnion)
	var completed *responses.Response
	argumentDeltas := make(map[int64]string)
	seenDone := make(map[int64]struct{})
	var content, reasoning strings.Builder

	for stream.Next() {
		if ctx.Err() != nil {
			return emitFailure(classifySDKError(ctx, ctx.Err()))
		}
		event := stream.Current()
		observeStreamEvent(ctx, event.Type)
		switch event.Type {
		case "response.output_text.delta":
			observeStreamDelta(ctx, "text", event.Delta != "")
			if err := budget.addContent(event.Delta); err != nil {
				return emitFailure(err)
			}
			content.WriteString(event.Delta)
			c.emit(event.OutputIndex, event.ItemID, "text_delta", event.Delta)
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			observeStreamDelta(ctx, "reasoning", event.Delta != "")
			if err := budget.addReasoning(event.Delta); err != nil {
				return emitFailure(err)
			}
			reasoning.WriteString(event.Delta)
			c.emit(event.OutputIndex, event.ItemID, "reasoning_delta", event.Delta)
		case "response.function_call_arguments.delta":
			observeStreamDelta(ctx, "tool", event.Delta != "")
			argumentDeltas[event.OutputIndex] += event.Delta
			c.emit(event.OutputIndex, event.ItemID, "tool_arguments_delta", event.Delta)
			if err := budget.addTool(event.OutputIndex, "", event.Delta); err != nil {
				return emitFailure(err)
			}
		case "response.output_item.done":
			if _, duplicate := seenDone[event.OutputIndex]; duplicate {
				return emitFailure(responsesProtocolError(fmt.Sprintf("重复 output_item.done index=%d", event.OutputIndex), nil))
			}
			seenDone[event.OutputIndex] = struct{}{}
			item := event.AsResponseOutputItemDone().Item
			if item.Type == "function_call" {
				call := item.AsFunctionCall()
				if partial, ok := argumentDeltas[event.OutputIndex]; ok {
					if partial != call.Arguments {
						return emitFailure(responsesProtocolError("function_call arguments delta 与 done item 不一致", nil))
					}
					if err := budget.addTool(event.OutputIndex, call.Name, ""); err != nil {
						return emitFailure(err)
					}
				} else if err := budget.addTool(event.OutputIndex, call.Name, call.Arguments); err != nil {
					return emitFailure(err)
				}
			}
			itemsByIndex[event.OutputIndex] = item
		case "response.completed":
			value := event.AsResponseCompleted().Response
			completed = &value
		case "response.incomplete":
			return emitFailure(responsesIncompleteError(event.AsResponseIncomplete().Response.IncompleteDetails.Reason))
		case "response.failed":
			failed := event.AsResponseFailed().Response
			return emitFailure(responsesFailedError(string(failed.Error.Code), failed.Error.Message))
		case "error":
			failure := event.AsError()
			return emitFailure(responsesFailedError(failure.Code, failure.Message))
		case "response.created", "response.in_progress", "response.output_item.added",
			"response.content_part.added", "response.content_part.done", "response.output_text.done",
			"response.function_call_arguments.done", "response.reasoning_summary_part.added",
			"response.reasoning_summary_part.done", "response.reasoning_summary_text.done",
			"response.reasoning_text.done":
			// 这些事件只描述生命周期或已由 delta/done item 覆盖的视图。
		default:
			// 未申请任何服务端内置工具，因此其它事件代表当前协议 profile
			// 无法解释的响应，不能静默忽略后继续 dispatch。
			return emitFailure(responsesProtocolError(fmt.Sprintf("未知 Responses SSE event=%q", event.Type), nil))
		}
	}
	if err := stream.Err(); err != nil {
		return emitFailure(classifySDKError(ctx, err))
	}
	if completed == nil {
		return emitFailure(responsesProtocolError("Responses SSE 缺少 response.completed", nil))
	}
	if completed.Status != responses.ResponseStatusCompleted {
		return emitFailure(responsesProtocolError(fmt.Sprintf("Responses 终态 status=%q", completed.Status), nil))
	}
	items := make([]responses.ResponseOutputItemUnion, len(itemsByIndex))
	for index := range items {
		item, ok := itemsByIndex[int64(index)]
		if !ok {
			return emitFailure(responsesProtocolError("Responses 输出项序号不连续", nil))
		}
		items[index] = item
	}
	if len(items) == 0 {
		return emitFailure(responsesProtocolError("Responses SSE 没有完成的 output item", nil))
	}
	result, err := responsesItemsResult(items, completed.Usage, c.request.OutputBudget)
	if err != nil {
		return emitFailure(err)
	}
	markStreamCompleted(ctx)
	return result, nil
}

func responsesItemsResult(items []responses.ResponseOutputItemUnion, usage responses.ResponseUsage, outputBudget OutputBudget) (responseData, error) {
	if len(items) == 0 {
		return responseData{}, responsesProtocolError("Responses 返回空 output items", nil)
	}
	counter := newOutputBudgetCounter(outputBudget, PhaseResponseValidate)
	result := responseData{Items: make([]OutputItem, 0, len(items))}
	for index, item := range items {
		raw := strings.TrimSpace(item.RawJSON())
		if raw == "" {
			return responseData{}, responsesProtocolError(fmt.Sprintf("output item[%d] 缺少原始 JSON", index), nil)
		}
		switch item.Type {
		case "message":
			message := item.AsMessage()
			if message.Status == responses.ResponseOutputMessageStatusIncomplete {
				return responseData{}, responsesProtocolError(fmt.Sprintf("message item[%d] incomplete", index), nil)
			}
			var text strings.Builder
			for _, part := range message.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					text.WriteString(part.Refusal)
				default:
					return responseData{}, responsesProtocolError(fmt.Sprintf("message item[%d] 未知 content type=%q", index, part.Type), nil)
				}
			}
			value := text.String()
			if err := counter.addContent(value); err != nil {
				return responseData{}, err
			}
			result.Content += value
			result.Items = append(result.Items, OutputItem{Kind: OutputItemMessage, ID: message.ID, Text: value, Raw: json.RawMessage(raw)})
		case "reasoning":
			reasoningItem := item.AsReasoning()
			var text strings.Builder
			for _, summary := range reasoningItem.Summary {
				text.WriteString(summary.Text)
			}
			for _, content := range reasoningItem.Content {
				text.WriteString(content.Text)
			}
			value := text.String()
			if err := counter.addReasoning(value); err != nil {
				return responseData{}, err
			}
			result.Reasoning += value
			result.Items = append(result.Items, OutputItem{Kind: OutputItemReasoning, ID: reasoningItem.ID, Reasoning: value, Raw: json.RawMessage(raw)})
		case "function_call":
			callItem := item.AsFunctionCall()
			if strings.TrimSpace(callItem.CallID) == "" || strings.TrimSpace(callItem.Name) == "" {
				return responseData{}, responsesProtocolError(fmt.Sprintf("function_call item[%d] 缺少 call_id/name", index), nil)
			}
			if callItem.Status == responses.ResponseFunctionToolCallStatusIncomplete {
				return responseData{}, responsesProtocolError(fmt.Sprintf("function_call item[%d] incomplete", index), nil)
			}
			if err := counter.addTool(int64(index), callItem.Name, callItem.Arguments); err != nil {
				return responseData{}, err
			}
			arguments := make(map[string]any)
			if err := json.Unmarshal([]byte(callItem.Arguments), &arguments); err != nil || arguments == nil {
				return responseData{}, responsesProtocolError(fmt.Sprintf("function_call %q arguments 不是 JSON object", callItem.Name), err)
			}
			call := ToolCall{ID: callItem.CallID, Name: callItem.Name, Arguments: arguments}
			result.ToolCalls = append(result.ToolCalls, call)
			result.Items = append(result.Items, OutputItem{Kind: OutputItemFunctionCall, ID: callItem.ID, ToolCall: &call, Raw: json.RawMessage(raw)})
		default:
			return responseData{}, responsesProtocolError(fmt.Sprintf("Responses output item[%d] type=%q 不在 AgentGo profile", index, item.Type), nil)
		}
	}
	result.Usage.PromptTokens = int(usage.InputTokens)
	result.Usage.CompletionTokens = int(usage.OutputTokens)
	result.Usage.ReasoningTokens = int(usage.OutputTokensDetails.ReasoningTokens)
	if len(result.ToolCalls) > 0 {
		result.FinishReason = FinishReasonToolCalls
	} else {
		result.FinishReason = FinishReasonStop
	}
	return result, nil
}

func responsesProtocolError(message string, cause error) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	failure := NewFailure(FailureMalformedResponse,
		PhaseResponseValidate, OriginProtocol, cause)
	failure.UsageState = UsageSettled
	return &ErrBadResponse{Err: cause, Failure: failure}
}

func responsesIncompleteError(reason string) error {
	cause := fmt.Errorf("Responses 返回 incomplete: reason=%s", strings.TrimSpace(reason))
	failure := NewFailure(FailureOutputTruncated,
		PhaseResponseValidate, OriginProvider, cause)
	failure.FinishReason = strings.TrimSpace(reason)
	failure.UsageState = UsageSettled
	return &ErrBadResponse{Err: cause, Failure: failure}
}

func responsesFailedError(code, message string) error {
	cause := fmt.Errorf("Responses provider failure: code=%s message=%s", strings.TrimSpace(code), strings.TrimSpace(message))
	failure := NewFailure(FailureProviderUnavailable,
		PhaseResponseValidate, OriginProvider, cause)
	failure.ProviderCode = strings.TrimSpace(code)
	failure.UsageState = UsageSettled
	normalized := strings.ToLower(strings.TrimSpace(code))
	if normalized == "invalidparameter" || normalized == "invalid_parameter" ||
		normalized == "invalid_request_error" || normalized == "invalid_request" {
		failure.Kind = FailureInvalidRequest
		return &ErrUnrecoverable{Err: cause, Code: code, Message: message, Failure: failure}
	}
	if normalized == "model_not_found" || normalized == "model_unavailable" {
		failure.Kind = FailureModelUnavailable
		return &ErrUnrecoverable{Err: cause, Code: code, Message: message, Failure: failure}
	}
	if normalized == "invalid_api_key" || normalized == "authentication_error" || normalized == "unauthorized" {
		failure.Kind = FailureAuth
		return &ErrUnrecoverable{Err: cause, Code: code, Message: message, Failure: failure}
	}
	if normalized == "permission_denied" || normalized == "forbidden" {
		failure.Kind = FailurePermissionDenied
		return &ErrUnrecoverable{Err: cause, Code: code, Message: message, Failure: failure}
	}
	if normalized == "insufficient_quota" || normalized == "insufficient_balance" ||
		normalized == "billing_hard_limit_reached" {
		failure.Kind = FailureProviderQuotaExhausted
		return &ErrUnrecoverable{Err: cause, Code: code, Message: message, Failure: failure}
	}
	return &ErrRecoverable{Err: cause, Code: code, Message: message, Failure: failure}
}
