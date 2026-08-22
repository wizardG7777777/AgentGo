package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentgo/internal/invocation"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// responses 执行 OpenAI Responses typed-item 主链。工具身份只由
// response.output_item.done.item.type=function_call 产生；正文中的任何标记
// 都只保留为 message，不参与工具识别。
func (c *SDKClient) responses(ctx context.Context, messages []Message, tools []ToolDef) (Response, error) {
	params, err := c.responsesParams(ctx, messages, tools)
	if err != nil {
		return Response{}, err
	}
	if c.request.Stream {
		return c.responsesStreaming(ctx, params)
	}
	result, err := c.client.Responses.New(ctx, params)
	if err != nil {
		return Response{}, classifySDKError(ctx, err)
	}
	return responsesResult(result, outputBudgetFromContext(ctx, c.request.OutputBudget))
}

func (c *SDKClient) responsesParams(ctx context.Context, messages []Message, tools []ToolDef) (responses.ResponseNewParams, error) {
	model := c.model
	if override := modelOverrideFromContext(ctx); override != "" {
		model = override
	}
	budget := outputBudgetFromContext(ctx, c.request.OutputBudget)
	params := responses.ResponseNewParams{
		Model:             shared.ResponsesModel(model),
		MaxOutputTokens:   openai.Int(budget.MaxCompletionTokens),
		ParallelToolCalls: openai.Bool(false),
		Store:             openai.Bool(false),
		Include:           []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Truncation:        responses.ResponseNewParamsTruncationDisabled,
	}
	if c.request.ReasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(c.request.ReasoningEffort)}
	}
	input, err := convertResponsesMessages(c.systemPrompt, messages)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}
	for _, tool := range tools {
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: tool.Name, Description: openai.String(tool.Description), Parameters: tool.Parameters,
			// AgentGo 的现有 schema 允许可选字段和 L3 默认参数；关闭服务端 strict
			// 不等于放松 Harness，最终参数仍由 typed decoder、ToolRouter 与 Gate 校验。
			Strict: openai.Bool(false),
		}})
	}
	choice, err := responseToolChoice(ctx, c.request.ForcedToolName, tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params.ToolChoice = choice
	return params, nil
}

func responseToolChoice(ctx context.Context, forced string, tools []ToolDef) (responses.ResponseNewParamsToolChoiceUnion, error) {
	find := func(name string) bool {
		for _, tool := range tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	if forced != "" {
		if !find(forced) {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("forced tool_choice=%q 不在本次 ToolRouter 定义中", forced)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: forced}}, nil
	}
	binding, ok := invocation.ContextBindingFrom(ctx)
	if !ok || binding.ToolChoice.Mode == "" || binding.ToolChoice.Mode == invocation.ToolChoiceAuto {
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto)}, nil
	}
	if len(tools) == 0 {
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("tool_choice=%s 但本次 ToolRouter 为空", binding.ToolChoice.Mode)
	}
	switch binding.ToolChoice.Mode {
	case invocation.ToolChoiceRequired:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsRequired)}, nil
	case invocation.ToolChoiceFunction:
		if !find(binding.ToolChoice.Name) {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("ContextBinding forced tool_choice=%q 不在本次 ToolRouter 定义中", binding.ToolChoice.Name)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: binding.ToolChoice.Name}}, nil
	default:
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("未知 tool_choice mode=%q", binding.ToolChoice.Mode)
	}
}

func convertResponsesMessages(systemPrompt string, messages []Message) (responses.ResponseInputParam, error) {
	input := make(responses.ResponseInputParam, 0, len(messages)+1)
	appendText := func(role, content string) {
		input = append(input, responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
			Role:    responses.EasyInputMessageRole(role),
			Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(content)},
		}})
	}
	if systemPrompt != "" {
		appendText("system", systemPrompt)
	}
	for _, message := range messages {
		switch message.Role {
		case "system", "user":
			appendText(message.Role, message.Content)
		case "assistant":
			if raw, ok := message.ExtraFields[responsesOutputItemsExtraField]; ok {
				var rawItems []json.RawMessage
				if err := json.Unmarshal(raw, &rawItems); err != nil || len(rawItems) == 0 {
					return nil, responsesProtocolError("L2 replay 的 Responses output items 无效", err)
				}
				for index, rawItem := range rawItems {
					var item responses.ResponseInputItemUnionParam
					if err := json.Unmarshal(rawItem, &item); err != nil {
						return nil, responsesProtocolError(fmt.Sprintf("Responses replay item[%d] 无法反序列化", index), err)
					}
					input = append(input, item)
				}
				continue
			}
			if len(message.ExtraFields) > 0 {
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

func (c *SDKClient) responsesStreaming(ctx context.Context, params responses.ResponseNewParams) (Response, error) {
	stream := c.client.Responses.NewStreaming(ctx, params)
	defer stream.Close()
	handler := streamHandlerFromContext(ctx)
	emitFailure := func(err error) (Response, error) {
		if handler != nil {
			handler(StreamEvent{Done: true, Error: err.Error()})
		}
		return Response{}, err
	}

	budget := newOutputBudgetCounter(outputBudgetFromContext(ctx, c.request.OutputBudget), invocation.PhaseStreamAccumulate)
	var items []responses.ResponseOutputItemUnion
	var completed *responses.Response
	argumentDeltas := make(map[int64]string)
	seenDone := make(map[int64]struct{})
	var content, reasoning strings.Builder

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			if err := budget.addContent(event.Delta); err != nil {
				return emitFailure(err)
			}
			content.WriteString(event.Delta)
			if handler != nil {
				handler(StreamEvent{ContentDelta: event.Delta, AccumulatedContent: content.String(), AccumulatedReasoning: reasoning.String()})
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if err := budget.addReasoning(event.Delta); err != nil {
				return emitFailure(err)
			}
			reasoning.WriteString(event.Delta)
			if handler != nil {
				handler(StreamEvent{ReasoningDelta: event.Delta, AccumulatedContent: content.String(), AccumulatedReasoning: reasoning.String()})
			}
		case "response.function_call_arguments.delta":
			argumentDeltas[event.OutputIndex] += event.Delta
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
			items = append(items, item)
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
	if len(items) == 0 {
		return emitFailure(responsesProtocolError("Responses SSE 没有完成的 output item", nil))
	}
	result, err := responsesItemsResult(items, completed.Usage, outputBudgetFromContext(ctx, c.request.OutputBudget))
	if err != nil {
		return emitFailure(err)
	}
	if handler != nil {
		handler(StreamEvent{AccumulatedContent: result.Content, AccumulatedReasoning: result.Reasoning, Done: true})
	}
	return result, nil
}

func responsesResult(response *responses.Response, budget invocation.OutputBudget) (Response, error) {
	if response == nil {
		return Response{}, responsesProtocolError("Responses 返回 nil response", nil)
	}
	switch response.Status {
	case responses.ResponseStatusCompleted:
		return responsesItemsResult(response.Output, response.Usage, budget)
	case responses.ResponseStatusIncomplete:
		return Response{}, responsesIncompleteError(response.IncompleteDetails.Reason)
	case responses.ResponseStatusFailed:
		return Response{}, responsesFailedError(string(response.Error.Code), response.Error.Message)
	default:
		return Response{}, responsesProtocolError(fmt.Sprintf("Responses 非终态 status=%q", response.Status), nil)
	}
}

func responsesItemsResult(items []responses.ResponseOutputItemUnion, usage responses.ResponseUsage, outputBudget invocation.OutputBudget) (Response, error) {
	if len(items) == 0 {
		return Response{}, responsesProtocolError("Responses 返回空 output items", nil)
	}
	counter := newOutputBudgetCounter(outputBudget, invocation.PhaseResponseValidate)
	result := Response{Items: make([]OutputItem, 0, len(items))}
	rawItems := make([]json.RawMessage, 0, len(items))
	for index, item := range items {
		raw := strings.TrimSpace(item.RawJSON())
		if raw == "" {
			return Response{}, responsesProtocolError(fmt.Sprintf("output item[%d] 缺少原始 JSON", index), nil)
		}
		rawItems = append(rawItems, json.RawMessage(raw))
		switch item.Type {
		case "message":
			message := item.AsMessage()
			if message.Status == responses.ResponseOutputMessageStatusIncomplete {
				return Response{}, responsesProtocolError(fmt.Sprintf("message item[%d] incomplete", index), nil)
			}
			var text strings.Builder
			for _, part := range message.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					text.WriteString(part.Refusal)
				default:
					return Response{}, responsesProtocolError(fmt.Sprintf("message item[%d] 未知 content type=%q", index, part.Type), nil)
				}
			}
			value := text.String()
			if err := counter.addContent(value); err != nil {
				return Response{}, err
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
				return Response{}, err
			}
			result.Reasoning += value
			result.Items = append(result.Items, OutputItem{Kind: OutputItemReasoning, ID: reasoningItem.ID, Reasoning: value, Raw: json.RawMessage(raw)})
		case "function_call":
			callItem := item.AsFunctionCall()
			if strings.TrimSpace(callItem.CallID) == "" || strings.TrimSpace(callItem.Name) == "" {
				return Response{}, responsesProtocolError(fmt.Sprintf("function_call item[%d] 缺少 call_id/name", index), nil)
			}
			if callItem.Status == responses.ResponseFunctionToolCallStatusIncomplete {
				return Response{}, responsesProtocolError(fmt.Sprintf("function_call item[%d] incomplete", index), nil)
			}
			if err := counter.addTool(int64(index), callItem.Name, callItem.Arguments); err != nil {
				return Response{}, err
			}
			arguments := make(map[string]any)
			if err := json.Unmarshal([]byte(callItem.Arguments), &arguments); err != nil || arguments == nil {
				return Response{}, responsesProtocolError(fmt.Sprintf("function_call %q arguments 不是 JSON object", callItem.Name), err)
			}
			call := ToolCall{ID: callItem.CallID, Name: callItem.Name, Arguments: arguments}
			result.ToolCalls = append(result.ToolCalls, call)
			result.Items = append(result.Items, OutputItem{Kind: OutputItemFunctionCall, ID: callItem.ID, ToolCall: &call, Raw: json.RawMessage(raw)})
		default:
			return Response{}, responsesProtocolError(fmt.Sprintf("Responses output item[%d] type=%q 不在 AgentGo profile", index, item.Type), nil)
		}
	}
	carrier, err := json.Marshal(rawItems)
	if err != nil {
		return Response{}, responsesProtocolError("序列化 Responses output item carrier 失败", err)
	}
	result.ExtraFields = map[string]json.RawMessage{responsesOutputItemsExtraField: carrier}
	result.Usage.PromptTokens = int(usage.InputTokens)
	result.Usage.CompletionTokens = int(usage.OutputTokens)
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
	failure := invocation.NewFailure(invocation.FailureMalformedResponse,
		invocation.PhaseResponseValidate, invocation.OriginProtocol, cause)
	failure.UsageState = invocation.UsageSettled
	return &ErrBadResponse{Err: cause, Failure: failure}
}

func responsesIncompleteError(reason string) error {
	cause := fmt.Errorf("Responses 返回 incomplete: reason=%s", strings.TrimSpace(reason))
	failure := invocation.NewFailure(invocation.FailureOutputTruncated,
		invocation.PhaseResponseValidate, invocation.OriginProvider, cause)
	failure.FinishReason = strings.TrimSpace(reason)
	failure.UsageState = invocation.UsageSettled
	return &ErrBadResponse{Err: cause, Failure: failure}
}

func responsesFailedError(code, message string) error {
	cause := fmt.Errorf("Responses provider failure: code=%s message=%s", strings.TrimSpace(code), strings.TrimSpace(message))
	failure := invocation.NewFailure(invocation.FailureProviderUnavailable,
		invocation.PhaseResponseValidate, invocation.OriginProvider, cause)
	failure.ProviderCode = strings.TrimSpace(code)
	failure.UsageState = invocation.UsageSettled
	return &ErrRecoverable{Err: cause, Code: code, Message: message, Failure: failure}
}
