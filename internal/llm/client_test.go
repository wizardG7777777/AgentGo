package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// openaiResponse 构造 OpenAI 格式的响应 JSON。
func openaiResponse(content string, toolCalls []map[string]any) map[string]any {
	return openaiResponseWithFinish(content, toolCalls, "stop")
}

func openaiResponseWithFinish(content string, toolCalls []map[string]any, finishReason string) map[string]any {
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if toolCalls != nil {
		msg["tool_calls"] = toolCalls
	}
	return map[string]any{
		"id":     "chatcmpl-test",
		"object": "chat.completion",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}
}

func TestSDKClient_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponse("你好", nil))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "test-key", "gpt-4o", "", 30*time.Second)
	resp, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "你好" {
		t.Errorf("content = %q, want %q", resp.Content, "你好")
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("tool calls = %d, want 0", len(resp.ToolCalls))
	}
	if resp.FinishReason != FinishReasonStop {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, FinishReasonStop)
	}
}

func TestSDKClient_Chat_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openaiResponseWithFinish("", []map[string]any{
			{
				"id":   "call_abc",
				"type": "function",
				"function": map[string]any{
					"name":      "read_file",
					"arguments": `{"path":"/tmp/a.txt"}`,
				},
			},
		}, "tool_calls")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "test-key", "gpt-4o", "", 30*time.Second)
	resp, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "read file"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_abc" {
		t.Errorf("tool call ID = %q, want %q", tc.ID, "call_abc")
	}
	if tc.Name != "read_file" {
		t.Errorf("tool call name = %q, want %q", tc.Name, "read_file")
	}
	if tc.Arguments["path"] != "/tmp/a.txt" {
		t.Errorf("tool call args[path] = %v, want %q", tc.Arguments["path"], "/tmp/a.txt")
	}
	if resp.FinishReason != FinishReasonToolCalls {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, FinishReasonToolCalls)
	}
}

func TestSDKClient_Chat_SystemPrompt(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponse("ok", nil))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "你是任务调度器", 30*time.Second)
	client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	messages, ok := capturedBody["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("messages count insufficient: %v", capturedBody["messages"])
	}
	firstMsg := messages[0].(map[string]any)
	role := firstMsg["role"].(string)
	if role != "developer" && role != "system" {
		t.Errorf("first message role = %q, want 'developer' or 'system'", role)
	}
}

func TestSDKClient_Chat_401_Unrecoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "unauthorized",
				"type":    "invalid_request_error",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "bad-key", "gpt-4o", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var unrecoverable *ErrUnrecoverable
	if !errors.As(err, &unrecoverable) {
		t.Errorf("expected ErrUnrecoverable, got %T: %v", err, err)
	}
}

func TestSDKClient_Chat_429_Recoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "rate limited",
				"type":    "rate_limit_error",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL+"/v1", "key", "gpt-4o", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var recoverable *ErrRecoverable
	if !errors.As(err, &recoverable) {
		t.Errorf("expected ErrRecoverable, got %T: %v", err, err)
	}
}

func TestSDKClient_Chat_ContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := client.Chat(ctx, []Message{{Role: "user", Content: "test"}}, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestSDKClient_Chat_FinishReasonLength_ReturnsBadResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponseWithFinish("partial...", nil, "length"))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var badResp *ErrBadResponse
	if !errors.As(err, &badResp) {
		t.Errorf("expected ErrBadResponse for length truncation, got %T: %v", err, err)
	}
}

func TestSDKClient_Chat_FinishReasonContentFilter_ReturnsUnrecoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponseWithFinish("", nil, "content_filter"))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var unrecov *ErrUnrecoverable
	if !errors.As(err, &unrecov) {
		t.Errorf("expected ErrUnrecoverable for content_filter, got %T: %v", err, err)
	}
}

func TestSDKClient_Chat_BadToolCallJSON_ReturnsBadResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openaiResponseWithFinish("", []map[string]any{
			{
				"id":   "call_bad",
				"type": "function",
				"function": map[string]any{
					"name":      "some_tool",
					"arguments": `{invalid json`,
				},
			},
		}, "tool_calls")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var badResp *ErrBadResponse
	if !errors.As(err, &badResp) {
		t.Errorf("expected ErrBadResponse for bad JSON, got %T: %v", err, err)
	}
}

func TestConvertMessage_UnknownRole_ReturnsError(t *testing.T) {
	_, err := convertMessage(Message{Role: "bogus", Content: "test"})
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
	var unknownRole *ErrUnknownRole
	if !errors.As(err, &unknownRole) {
		t.Errorf("expected ErrUnknownRole, got %T: %v", err, err)
	}
	if unknownRole.Role != "bogus" {
		t.Errorf("role = %q, want %q", unknownRole.Role, "bogus")
	}
}

func TestDefaultTimeout_Applied(t *testing.T) {
	// timeout=0 应使用默认值，不应 panic
	client := NewSDKClient("http://localhost:1", "key", "gpt-4o", "", 0)
	if client == nil {
		t.Fatal("expected non-nil client with default timeout")
	}
}
