package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		Protocol: ProtocolResponses, ForcedToolName: "list_dir",
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
	choice, _ := body["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "list_dir" {
		t.Fatalf("tool_choice=%#v", body["tool_choice"])
	}
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["strict"] != false {
		t.Fatalf("tool strict=%#v, want false", tool["strict"])
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
		json.RawMessage(`{"type":"reasoning","id":"rs-1","summary":[],"encrypted_content":"opaque","status":"completed"}`),
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
	want := []string{"message", "reasoning", "function_call", "function_call_output"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("input types=%v, want %v", types, want)
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
