package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agentgo/internal/invocation"
)

func responsesFixture(output []map[string]any) map[string]any {
	return map[string]any{
		"id": "resp-test", "object": "response", "status": "completed", "output": output,
		"usage": map[string]any{
			"input_tokens": 11, "output_tokens": 7, "total_tokens": 18,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	}
}

func TestResponsesClientFunctionCallUsesTypedEnvelope(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path=%q, want /responses", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesFixture([]map[string]any{{
			"type": "function_call", "id": "fc-1", "call_id": "call-1", "status": "completed",
			"name": "list_dir", "arguments": `{"path":"."}`,
		}}))
	}))
	defer server.Close()

	client := NewSDKClientWithConfig(server.URL, "key", "gpt-test", "", 30*time.Second, ClientConfig{
		Protocol: ProtocolResponses, ToolChoice: invocation.ToolChoice{Mode: invocation.ToolChoiceAuto},
	})
	response, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "list"}}, []ToolDef{{
		Name: "list_dir", Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.FinishReason != FinishReasonToolCalls || len(response.ToolCalls) != 1 ||
		response.ToolCalls[0].Name != "list_dir" || response.ToolCalls[0].Arguments["path"] != "." {
		t.Fatalf("response=%+v", response)
	}
	if len(response.Items) != 1 || response.Items[0].Kind != OutputItemFunctionCall {
		t.Fatalf("typed items=%+v", response.Items)
	}
	if _, ok := response.ExtraFields[ResponsesOutputItemsExtraField()]; !ok {
		t.Fatal("Responses typed output item 未进入 L2 replay carrier")
	}
	if got := body["parallel_tool_calls"]; got != false {
		t.Fatalf("parallel_tool_calls=%#v, want false", got)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice=%#v", body["tool_choice"])
	}
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["strict"] != false {
		t.Fatalf("tool strict=%#v, want false", tool["strict"])
	}
}

func TestResponsesContextBindingOverridesReasoningForExactSubmit(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesFixture([]map[string]any{{
			"type": "function_call", "id": "fc-submit", "call_id": "call-submit", "status": "completed",
			"name": "submit_task_result", "arguments": `{"status":"completed","summary":"done"}`,
		}}))
	}))
	defer server.Close()
	binding := invocation.ContextBinding{
		Schema: invocation.ContextBindingSchemaV1, InvocationID: "invocation-submit",
		ContextSnapshotID: "snapshot-submit", ContextPolicyID: "context:default/v8",
		ToolRouterSnapshotID: "router-submit", EncodedRequestDigest: "sha256:submit",
		OutputBudget: DefaultOutputBudget(), ReasoningEffort: "none",
		ToolChoice: invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: "submit_task_result"},
	}
	ctx, err := invocation.WithContextBinding(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	client := NewSDKClientWithConfig(server.URL, "key", "deepseek-v4-flash", "", 30*time.Second, ClientConfig{
		Protocol: ProtocolResponses, ReasoningEffort: "low",
	})
	if _, err := client.Chat(ctx, []Message{{Role: "user", Content: "submit"}}, []ToolDef{{
		Name: "submit_task_result", Parameters: map[string]any{"type": "object"},
	}}); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	choice, _ := body["tool_choice"].(map[string]any)
	if reasoning["effort"] != "none" || choice["type"] != "function" || choice["name"] != "submit_task_result" {
		t.Fatalf("终态提交 wire 未冻结 reasoning=none + exact submit: reasoning=%#v choice=%#v", body["reasoning"], body["tool_choice"])
	}
}

func TestResponsesStreamingSeparatesReasoningAndFunctionCall(t *testing.T) {
	reasoningItem := map[string]any{
		"type": "reasoning", "id": "rs-1", "status": "completed",
		"summary":           []map[string]any{{"type": "summary_text", "text": "先检查"}},
		"encrypted_content": "opaque",
	}
	callItem := map[string]any{
		"type": "function_call", "id": "fc-1", "call_id": "call-1", "status": "completed",
		"name": "list_dir", "arguments": `{"path":"."}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-test", "status": "in_progress"}},
			{"type": "response.reasoning_summary_text.delta", "item_id": "rs-1", "output_index": 0, "summary_index": 0, "delta": "先检查"},
			{"type": "response.output_item.done", "output_index": 0, "item": reasoningItem},
			{"type": "response.function_call_arguments.delta", "item_id": "fc-1", "output_index": 1, "delta": `{"path":"."}`},
			{"type": "response.function_call_arguments.done", "item_id": "fc-1", "output_index": 1, "name": "list_dir", "arguments": `{"path":"."}`},
			{"type": "response.output_item.done", "output_index": 1, "item": callItem},
			{"type": "response.completed", "response": responsesFixture([]map[string]any{reasoningItem, callItem})},
		}
		for _, frame := range frames {
			encoded, _ := json.Marshal(frame)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewSDKClientWithConfig(server.URL, "key", "gpt-test", "", 30*time.Second, ClientConfig{
		Protocol: ProtocolResponses, Stream: true,
	})
	var streamed []StreamEvent
	ctx := WithStreamHandler(context.Background(), func(event StreamEvent) { streamed = append(streamed, event) })
	response, err := client.Chat(ctx, []Message{{Role: "user", Content: "list"}}, []ToolDef{{Name: "list_dir"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "" || response.Reasoning != "先检查" || len(response.ToolCalls) != 1 {
		t.Fatalf("response=%+v", response)
	}
	if len(response.Items) != 2 || response.Items[0].Kind != OutputItemReasoning || response.Items[1].Kind != OutputItemFunctionCall {
		t.Fatalf("items=%+v", response.Items)
	}
	if len(streamed) != 2 || streamed[0].ReasoningDelta != "先检查" || !streamed[1].Done {
		t.Fatalf("streamed=%+v", streamed)
	}
}

func TestResponsesMessageContainingDSMLNeverBecomesToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesFixture([]map[string]any{{
			"type": "message", "id": "msg-1", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": `<｜DSML｜invoke name="run_shell">`}},
		}}))
	}))
	defer server.Close()

	client := NewSDKClientWithConfig(server.URL, "key", "gpt-test", "", 30*time.Second, ClientConfig{Protocol: ProtocolResponses})
	response, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "answer"}}, []ToolDef{{Name: "run_shell"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 0 || response.FinishReason != FinishReasonStop || !strings.Contains(response.Content, "DSML") {
		t.Fatalf("DSML message 被错误提升为行动: %+v", response)
	}
}

func TestResponsesReplayPreservesTypedItemsAndCallOutput(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesFixture([]map[string]any{{
			"type": "message", "id": "msg-2", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": "done"}},
		}}))
	}))
	defer server.Close()

	rawItems, _ := json.Marshal([]json.RawMessage{
		json.RawMessage(`{"type":"message","id":"msg-empty","role":"assistant","content":[],"status":"completed"}`),
		json.RawMessage(`{"type":"reasoning","id":"rs-1","summary":[],"content":[],"encrypted_content":"opaque","status":"completed"}`),
		json.RawMessage(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"list_dir","arguments":"{}","status":"completed"}`),
	})
	client := NewSDKClientWithConfig(server.URL, "key", "gpt-test", "", 30*time.Second, ClientConfig{Protocol: ProtocolResponses})
	_, err := client.Chat(context.Background(), []Message{
		{Role: "user", Content: "list"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "list_dir", Arguments: map[string]any{}}},
			ExtraFields: map[string]json.RawMessage{ResponsesOutputItemsExtraField(): rawItems}},
		{Role: "tool", ToolCallID: "call-1", Content: "ok"},
	}, []ToolDef{{Name: "list_dir"}})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	var types []string
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		itemType := fmt.Sprint(item["type"])
		if item["type"] == nil && item["role"] != nil {
			itemType = "message"
		}
		types = append(types, itemType)
	}
	want := []string{"message", "message", "reasoning", "function_call", "function_call_output"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("input types=%v, want %v", types, want)
	}
	for index, field := range []string{"content", "summary", "content"} {
		itemIndex := 1
		if index > 0 {
			itemIndex = 2
		}
		item, _ := input[itemIndex].(map[string]any)
		value, present := item[field]
		list, isList := value.([]any)
		if !present || !isList || len(list) != 0 {
			t.Fatalf("input[%d].%s=%#v present=%v，应原样保留空 required 集合", itemIndex, field, value, present)
		}
	}
}

func TestResponsesRejectsUnknownOutputItemBeforeDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesFixture([]map[string]any{{"type": "future_unknown", "id": "x"}}))
	}))
	defer server.Close()

	client := NewSDKClientWithConfig(server.URL, "key", "gpt-test", "", 30*time.Second, ClientConfig{Protocol: ProtocolResponses})
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if err == nil {
		t.Fatal("未知 Responses item 应 fail-closed")
	}
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureMalformedResponse {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
}

// 该测试只在显式授权的外部回归中运行，用 DeepSeek 官方 Responses 端点钉住
// “空 message content + function_call + function_call_output”的真实无状态重放。
func TestDeepSeekLiveResponsesReplayPreservesEmptyMessageContent(t *testing.T) {
	if os.Getenv("AGENTGO_LIVE_DEEPSEEK") != "1" {
		t.Skip("仅在显式外部 DeepSeek 协议回归中运行")
	}
	baseURL, apiKey, model := os.Getenv("SWE_BASE_URL"), os.Getenv("SWE_API_KEY"), os.Getenv("SWE_MODEL")
	if baseURL == "" || apiKey == "" {
		t.Fatal("SWE_BASE_URL/SWE_API_KEY 未设置")
	}
	if model != "deepseek-v4-flash" {
		t.Fatalf("外部协议回归只允许 deepseek-v4-flash，当前=%q", model)
	}
	tool := ToolDef{
		Name: "agentgo_live_replay_probe", Description: "Return the required nonce.",
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"nonce": map[string]any{"type": "string", "const": "live-replay"}},
			"required":   []any{"nonce"},
		},
	}
	first := NewSDKClientWithConfig(baseURL, apiKey, model, "", 60*time.Second, ClientConfig{
		Protocol: ProtocolResponses, Stream: true, ReasoningEffort: "low",
		ToolChoice: invocation.ToolChoice{Mode: invocation.ToolChoiceAuto},
	})
	firstResponse, err := first.Chat(context.Background(), []Message{{
		Role: "user", Content: "Call the required function exactly once with nonce live-replay.",
	}}, []ToolDef{tool})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstResponse.ToolCalls) != 1 || firstResponse.ToolCalls[0].Name != tool.Name {
		t.Fatalf("首轮未形成唯一目标工具调用: %+v", firstResponse.ToolCalls)
	}
	carrier := firstResponse.ExtraFields[ResponsesOutputItemsExtraField()]
	var rawItems []json.RawMessage
	if err := json.Unmarshal(carrier, &rawItems); err != nil || len(rawItems) == 0 {
		t.Fatalf("首轮 typed carrier 无效: items=%d err=%v", len(rawItems), err)
	}
	rawItems = append([]json.RawMessage{json.RawMessage(
		`{"type":"message","id":"msg-agentgo-empty","role":"assistant","content":[],"status":"completed"}`,
	)}, rawItems...)
	carrier, err = json.Marshal(rawItems)
	if err != nil {
		t.Fatal(err)
	}
	replay := NewSDKClientWithConfig(baseURL, apiKey, model, "", 60*time.Second, ClientConfig{
		Protocol: ProtocolResponses, Stream: true, ReasoningEffort: "low",
	})
	secondResponse, err := replay.Chat(context.Background(), []Message{
		{Role: "user", Content: "Call the required function exactly once with nonce live-replay."},
		{Role: "assistant", Content: firstResponse.Content, ToolCalls: firstResponse.ToolCalls,
			ExtraFields: map[string]json.RawMessage{ResponsesOutputItemsExtraField(): carrier}},
		{Role: "tool", ToolCallID: firstResponse.ToolCalls[0].ID, Content: "live-replay-ok"},
	}, []ToolDef{tool})
	if err != nil {
		t.Fatal(err)
	}
	if secondResponse.FinishReason != FinishReasonStop || len(secondResponse.ToolCalls) != 0 {
		t.Fatalf("第二轮未形成文本终态: finish=%s calls=%d", secondResponse.FinishReason, len(secondResponse.ToolCalls))
	}
}

func TestDeepSeekLiveDeliverableOverrideEscapesHistoricalTools(t *testing.T) {
	if os.Getenv("AGENTGO_LIVE_DEEPSEEK") != "1" {
		t.Skip("仅在显式外部 DeepSeek 协议回归中运行")
	}
	baseURL, apiKey, model := os.Getenv("SWE_BASE_URL"), os.Getenv("SWE_API_KEY"), os.Getenv("SWE_MODEL")
	if baseURL == "" || apiKey == "" || model != "deepseek-v4-flash" {
		t.Fatal("需要真实 deepseek-v4-flash 的 SWE_BASE_URL/SWE_API_KEY/SWE_MODEL")
	}
	readTool := ToolDef{
		Name: "read_file", Description: "Read one file.",
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"path": map[string]any{"type": "string", "const": "README.md"}},
			"required":   []any{"path"},
		},
	}
	first := NewSDKClientWithConfig(baseURL, apiKey, model, "", 60*time.Second, ClientConfig{
		Protocol: ProtocolResponses, Stream: true, ReasoningEffort: "low",
		ToolChoice: invocation.ToolChoice{Mode: invocation.ToolChoiceAuto},
	})
	firstResponse, err := first.Chat(context.Background(), []Message{{
		Role: "user", Content: "Call read_file once for README.md. Do not answer with text.",
	}}, []ToolDef{readTool})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstResponse.ToolCalls) == 0 {
		t.Fatalf("首轮未形成 read_file: %+v", firstResponse)
	}
	messages := []Message{
		{Role: "user", Content: "Call read_file once for README.md. Do not answer with text."},
		{Role: "assistant", Content: firstResponse.Content, ToolCalls: firstResponse.ToolCalls,
			ExtraFields: firstResponse.ExtraFields},
	}
	for _, call := range firstResponse.ToolCalls {
		messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: "README content observed"})
	}
	messages = append(messages, Message{Role: "user", Content: "Now submit the final task result."})
	submitTool := ToolDef{
		Name: "submit_task_result", Description: "Submit the terminal task result.",
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"status":  map[string]any{"type": "string", "const": "completed"},
				"summary": map[string]any{"type": "string"},
			},
			"required": []any{"status", "summary"},
		},
	}
	deliver := NewSDKClientWithConfig(baseURL, apiKey, model, "", 60*time.Second, ClientConfig{
		Protocol: ProtocolResponses, Stream: true, ReasoningEffort: "none",
		ToolChoice: invocation.ToolChoice{Mode: invocation.ToolChoiceFunction, Name: submitTool.Name},
	})
	response, err := deliver.Chat(context.Background(), messages, []ToolDef{submitTool})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) == 0 {
		t.Fatalf("终态轮未返回 submit_task_result: %+v", response)
	}
	for _, call := range response.ToolCalls {
		if call.Name != submitTool.Name {
			t.Fatalf("none+exact 未逃离历史工具偏好: %+v", response.ToolCalls)
		}
	}
}
