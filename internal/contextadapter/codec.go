package contextadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

const (
	envelopeMessageBase = "message_base"
	envelopeToolCall    = "assistant_tool_call"
	envelopeExtraField  = "assistant_extra_field"
	envelopeToolDef     = "tool_definition"
)

type wireEnvelope struct {
	Type         string            `json:"type"`
	MessageIndex int               `json:"message_index,omitempty"`
	PartIndex    int               `json:"part_index,omitempty"`
	ToolIndex    int               `json:"tool_index,omitempty"`
	Message      *canonicalMessage `json:"message,omitempty"`
	ToolCall     *llm.ToolCall     `json:"tool_call,omitempty"`
	ExtraName    string            `json:"extra_name,omitempty"`
	ExtraValue   json.RawMessage   `json:"extra_value,omitempty"`
	Tool         *canonicalToolDef `json:"tool,omitempty"`
}

type semanticWireEncoder struct{ toolRouterSnapshotID string }

func (e semanticWireEncoder) Encode(ctx context.Context, items []contextcontract.WireItem) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, err := decodeWireRequest(e.toolRouterSnapshotID, items)
	if err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func encodeEnvelope(envelope wireEnvelope) ([]byte, error) {
	return json.Marshal(envelope)
}

func decodeWireRequest(toolRouterSnapshotID string, items []contextcontract.WireItem) (canonicalRequest, error) {
	bases := make(map[int]canonicalMessage)
	calls := make(map[int]map[int]llm.ToolCall)
	extras := make(map[int]map[string]json.RawMessage)
	tools := make(map[int]canonicalToolDef)
	maxMessage, maxTool := -1, -1
	for _, item := range items {
		var envelope wireEnvelope
		if err := json.Unmarshal(item.Payload, &envelope); err != nil {
			return canonicalRequest{}, fmt.Errorf("context adapter: wire=%s envelope 解码失败: %w", item.WireID, err)
		}
		switch envelope.Type {
		case envelopeMessageBase:
			if envelope.Message == nil || envelope.MessageIndex < 0 {
				return canonicalRequest{}, fmt.Errorf("context adapter: wire=%s message base 无效", item.WireID)
			}
			if _, duplicate := bases[envelope.MessageIndex]; duplicate {
				return canonicalRequest{}, fmt.Errorf("context adapter: message_index=%d 重复 base", envelope.MessageIndex)
			}
			bases[envelope.MessageIndex] = cloneCanonicalMessage(*envelope.Message)
			if envelope.MessageIndex > maxMessage {
				maxMessage = envelope.MessageIndex
			}
		case envelopeToolCall:
			if envelope.ToolCall == nil || envelope.MessageIndex < 0 || envelope.PartIndex < 0 {
				return canonicalRequest{}, fmt.Errorf("context adapter: wire=%s tool call part 无效", item.WireID)
			}
			if calls[envelope.MessageIndex] == nil {
				calls[envelope.MessageIndex] = make(map[int]llm.ToolCall)
			}
			if _, duplicate := calls[envelope.MessageIndex][envelope.PartIndex]; duplicate {
				return canonicalRequest{}, fmt.Errorf("context adapter: message=%d tool part=%d 重复", envelope.MessageIndex, envelope.PartIndex)
			}
			calls[envelope.MessageIndex][envelope.PartIndex] = cloneToolCall(*envelope.ToolCall)
			if envelope.MessageIndex > maxMessage {
				maxMessage = envelope.MessageIndex
			}
		case envelopeExtraField:
			if envelope.MessageIndex < 0 || envelope.ExtraName == "" || len(envelope.ExtraValue) == 0 || !json.Valid(envelope.ExtraValue) {
				return canonicalRequest{}, fmt.Errorf("context adapter: wire=%s provider extra part 无效", item.WireID)
			}
			if extras[envelope.MessageIndex] == nil {
				extras[envelope.MessageIndex] = make(map[string]json.RawMessage)
			}
			if _, duplicate := extras[envelope.MessageIndex][envelope.ExtraName]; duplicate {
				return canonicalRequest{}, fmt.Errorf("context adapter: message=%d extra=%s 重复", envelope.MessageIndex, envelope.ExtraName)
			}
			extras[envelope.MessageIndex][envelope.ExtraName] = append(json.RawMessage(nil), envelope.ExtraValue...)
			if envelope.MessageIndex > maxMessage {
				maxMessage = envelope.MessageIndex
			}
		case envelopeToolDef:
			if envelope.Tool == nil || envelope.ToolIndex < 0 {
				return canonicalRequest{}, fmt.Errorf("context adapter: wire=%s tool definition 无效", item.WireID)
			}
			if _, duplicate := tools[envelope.ToolIndex]; duplicate {
				return canonicalRequest{}, fmt.Errorf("context adapter: tool_index=%d 重复", envelope.ToolIndex)
			}
			tools[envelope.ToolIndex] = cloneCanonicalToolDef(*envelope.Tool)
			if envelope.ToolIndex > maxTool {
				maxTool = envelope.ToolIndex
			}
		default:
			return canonicalRequest{}, fmt.Errorf("context adapter: wire=%s envelope type=%q 未知", item.WireID, envelope.Type)
		}
	}

	request := canonicalRequest{ToolRouterSnapshotID: toolRouterSnapshotID}
	for index := 0; index <= maxMessage; index++ {
		message, ok := bases[index]
		if !ok {
			return canonicalRequest{}, fmt.Errorf("context adapter: message_index=%d 缺少 base", index)
		}
		if parts := calls[index]; len(parts) > 0 {
			if message.Role != "assistant" {
				return canonicalRequest{}, fmt.Errorf("context adapter: 非 assistant message=%d 含 tool call", index)
			}
			message.ToolCalls = make([]llm.ToolCall, len(parts))
			for part := 0; part < len(parts); part++ {
				call, ok := parts[part]
				if !ok {
					return canonicalRequest{}, fmt.Errorf("context adapter: message=%d tool call part=%d 缺失", index, part)
				}
				message.ToolCalls[part] = cloneToolCall(call)
			}
		}
		if fields := extras[index]; len(fields) > 0 {
			if message.Role != "assistant" {
				return canonicalRequest{}, fmt.Errorf("context adapter: 非 assistant message=%d 含 provider extra", index)
			}
			message.ExtraFields = make(map[string]json.RawMessage, len(fields))
			for key, raw := range fields {
				message.ExtraFields[key] = append(json.RawMessage(nil), raw...)
			}
		}
		request.Messages = append(request.Messages, message)
	}
	for index := 0; index <= maxTool; index++ {
		tool, ok := tools[index]
		if !ok {
			return canonicalRequest{}, fmt.Errorf("context adapter: tool_index=%d 缺失", index)
		}
		request.Tools = append(request.Tools, tool)
	}
	return request, nil
}

func runtimeView(request canonicalRequest) ([]llm.Message, []llm.ToolDef) {
	messages := make([]llm.Message, len(request.Messages))
	for i, input := range request.Messages {
		messages[i] = llm.Message{
			Role: input.Role, Content: input.Content, Name: input.Name,
			ToolCallID: input.ToolCallID,
			ToolCalls:  append([]llm.ToolCall(nil), input.ToolCalls...),
		}
		if len(input.ExtraFields) > 0 {
			messages[i].ExtraFields = make(map[string]json.RawMessage, len(input.ExtraFields))
			for key, raw := range input.ExtraFields {
				messages[i].ExtraFields[key] = append(json.RawMessage(nil), raw...)
			}
		}
	}
	tools := make([]llm.ToolDef, len(request.Tools))
	for i, input := range request.Tools {
		tools[i] = llm.ToolDef{
			Name: input.Name, Description: input.Description,
			Parameters: cloneJSONMap(input.Parameters),
		}
	}
	return messages, tools
}

func canonicalFromMessage(message llm.Message) canonicalMessage {
	output := canonicalMessage{
		Role: message.Role, Content: message.Content, Name: message.Name,
		ToolCallID: message.ToolCallID,
	}
	return output
}

func canonicalFromToolDef(tool llm.ToolDef) canonicalToolDef {
	return canonicalToolDef{
		Name: tool.Name, Description: tool.Description,
		Parameters: cloneJSONMap(tool.Parameters),
	}
}

func cloneCanonicalMessage(input canonicalMessage) canonicalMessage {
	output := input
	output.ToolCalls = make([]llm.ToolCall, len(input.ToolCalls))
	for i, call := range input.ToolCalls {
		output.ToolCalls[i] = cloneToolCall(call)
	}
	if input.ExtraFields != nil {
		output.ExtraFields = make(map[string]json.RawMessage, len(input.ExtraFields))
		for key, raw := range input.ExtraFields {
			output.ExtraFields[key] = append(json.RawMessage(nil), raw...)
		}
	}
	return output
}

func cloneCanonicalToolDef(input canonicalToolDef) canonicalToolDef {
	output := input
	output.Parameters = cloneJSONMap(input.Parameters)
	return output
}

func cloneToolCall(input llm.ToolCall) llm.ToolCall {
	output := input
	output.Arguments = cloneJSONMap(input.Arguments)
	return output
}

func cloneJSONMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var output map[string]any
	if json.Unmarshal(data, &output) != nil {
		return nil
	}
	return output
}

func sortedExtraKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
