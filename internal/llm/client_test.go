package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

	client := NewSDKClient(server.URL, "test-key", "gpt-4o", "", "", 30*time.Second)
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

	client := NewSDKClient(server.URL, "test-key", "gpt-4o", "", "", 30*time.Second)
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

	client := NewSDKClient(server.URL, "key", "gpt-4o", "你是任务调度器", "", 30*time.Second)
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

	client := NewSDKClient(server.URL, "bad-key", "gpt-4o", "", "", 30*time.Second)
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

	client := NewSDKClient(server.URL+"/v1", "key", "gpt-4o", "", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var recoverable *ErrRecoverable
	if !errors.As(err, &recoverable) {
		t.Errorf("expected ErrRecoverable, got %T: %v", err, err)
	}
}

func TestSDKClient_Chat_500_Unrecoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Internal server error",
				"type":    "internal_error",
				"code":    "internal_error",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL+"/v1", "key", "gpt-4o", "", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var unrecoverable *ErrUnrecoverable
	if !errors.As(err, &unrecoverable) {
		t.Fatalf("expected ErrUnrecoverable, got %T: %v", err, err)
	}
	if unrecoverable.Code != "internal_error" {
		t.Errorf("expected Code='internal_error', got %q", unrecoverable.Code)
	}
	if unrecoverable.Message != "Internal server error" {
		t.Errorf("expected Message='Internal server error', got %q", unrecoverable.Message)
	}
}

func TestSDKClient_Chat_400_ModelNotFound_Unrecoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "The model `gpt-5` does not exist or you do not have access to it.",
				"type":    "invalid_request_error",
				"code":    "model_not_found",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL+"/v1", "key", "gpt-5", "", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var unrecoverable *ErrUnrecoverable
	if !errors.As(err, &unrecoverable) {
		t.Fatalf("expected ErrUnrecoverable, got %T: %v", err, err)
	}
	if unrecoverable.Code != "model_not_found" {
		t.Errorf("expected Code='model_not_found', got %q", unrecoverable.Code)
	}
	if unrecoverable.Message == "" {
		t.Error("expected non-empty Message")
	}
}

func TestSDKClient_Chat_502_Recoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Bad gateway",
				"type":    "gateway_error",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL+"/v1", "key", "gpt-4o", "", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var recoverable *ErrRecoverable
	if !errors.As(err, &recoverable) {
		t.Fatalf("expected ErrRecoverable, got %T: %v", err, err)
	}
}

func TestSDKClient_Chat_503_Recoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Service unavailable",
				"type":    "service_unavailable",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL+"/v1", "key", "gpt-4o", "", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var recoverable *ErrRecoverable
	if !errors.As(err, &recoverable) {
		t.Fatalf("expected ErrRecoverable, got %T: %v", err, err)
	}
}

// TestSDKClient_Chat_EndpointPropagation 守的是 §9.4 一个非平凡的装配点：
// classifySDKError 把 apiErr.Request.URL.String() 写入 ErrUnrecoverable.Endpoint
// （client.go:313-316），diagnoseLLMError 中 404Endpoint / 404Host 两条路径需要
// 这个字段才能给出具体诊断。
//
// 既有测试都手填 Endpoint，没有验证从 SDK apiErr → ErrUnrecoverable.Endpoint
// 这条透传链路；本测试通过 httptest 模拟真实 401 响应，断言 Endpoint 字段
// 至少含我们配置的 server URL host:port。
//
// 失败模式：openai-go SDK 升级时可能改 apiErr.Request 字段名/结构，那时本测试
// 会精确暴露空 Endpoint 而非等到 diagnoseLLMError 输出"端点错误"诊断里的"空"
// 才被发现。
func TestSDKClient_Chat_EndpointPropagation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Incorrect API key provided",
				"type":    "invalid_request_error",
				"code":    "invalid_api_key",
			},
		})
	}))
	defer server.Close()

	client := NewSDKClient(server.URL+"/v1", "bad-key", "gpt-4o", "", "", 30*time.Second)
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "test"}}, nil)

	var unrecoverable *ErrUnrecoverable
	if !errors.As(err, &unrecoverable) {
		t.Fatalf("expected ErrUnrecoverable, got %T: %v", err, err)
	}
	if unrecoverable.Endpoint == "" {
		t.Fatalf("Endpoint 为空——apiErr.Request.URL → ErrUnrecoverable.Endpoint 透传链路断了")
	}
	// httptest 的 server.URL 形如 http://127.0.0.1:<port>，client 用的是 server.URL+"/v1"
	// SDK 实际请求路径是 server.URL+"/v1/chat/completions"，Endpoint 应至少含 server.URL
	if !strings.Contains(unrecoverable.Endpoint, server.URL) {
		t.Errorf("Endpoint 不含预期 server URL；\n  got      = %q\n  want sub = %q",
			unrecoverable.Endpoint, server.URL)
	}
}

func TestSDKClient_Chat_ContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
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

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
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

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
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

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
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
	client := NewSDKClient("http://localhost:1", "key", "gpt-4o", "", "", 0)
	if client == nil {
		t.Fatal("expected non-nil client with default timeout")
	}
}

// ============================================================================
// 层 1 — ExtraFields 响应抽取 + 请求回写
// ============================================================================

// openaiResponseWithExtras 返回带非标 message 字段的响应（模拟 DeepSeek V4）。
func openaiResponseWithExtras(content string, extras map[string]any) map[string]any {
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	for k, v := range extras {
		msg[k] = v
	}
	return map[string]any{
		"id":     "chatcmpl-test",
		"object": "chat.completion",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}
}

func TestSDKClient_Chat_ExtractExtraFields(t *testing.T) {
	// Server 响应里带 reasoning_content（DeepSeek V4 风格）
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponseWithExtras("hi there", map[string]any{
			"reasoning_content": "We should greet the user.",
		}))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
	resp, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "hi there" {
		t.Errorf("Content = %q, want %q", resp.Content, "hi there")
	}
	if resp.ExtraFields == nil {
		t.Fatal("ExtraFields 为 nil，期望包含 reasoning_content")
	}
	raw, ok := resp.ExtraFields["reasoning_content"]
	if !ok {
		t.Fatalf("ExtraFields 缺少 reasoning_content，实际 keys = %v", keysOf(resp.ExtraFields))
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("raw JSON 解析失败: %v (raw=%s)", err, string(raw))
	}
	if got != "We should greet the user." {
		t.Errorf("reasoning_content = %q, want %q", got, "We should greet the user.")
	}
}

func TestSDKClient_Chat_NoExtraFieldsInResponse_NilMap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponse("hi", nil))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
	resp, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ExtraFields != nil {
		t.Errorf("无 extras 时 ExtraFields 应为 nil，实际 = %v", resp.ExtraFields)
	}
}

func TestSDKClient_Chat_RoundtripExtraFields_WrittenBackOnAssistant(t *testing.T) {
	// 捕获 request body，断言 assistant 消息里带 reasoning_content
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponse("ok", nil))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "gpt-4o", "", "", 30*time.Second)
	history := []Message{
		{Role: "user", Content: "q1"},
		{
			Role:    "assistant",
			Content: "a1",
			ExtraFields: map[string]json.RawMessage{
				"reasoning_content": json.RawMessage(`"prior chain of thought"`),
			},
		},
		{Role: "user", Content: "q2"},
	}
	_, err := client.Chat(context.Background(), history, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	messages, ok := capturedBody["messages"].([]any)
	if !ok {
		t.Fatalf("captured body messages not a slice: %v", capturedBody["messages"])
	}
	// 找 assistant 消息（系统 prompt 为空，所以 0=user, 1=assistant, 2=user）
	var asst map[string]any
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			asst = mm
			break
		}
	}
	if asst == nil {
		t.Fatalf("请求体里找不到 assistant 消息: %v", messages)
	}
	rc, ok := asst["reasoning_content"]
	if !ok {
		t.Fatalf("assistant 消息缺少 reasoning_content，实际 keys = %v", keysOfAny(asst))
	}
	if rc != "prior chain of thought" {
		t.Errorf("reasoning_content = %v, want %q", rc, "prior chain of thought")
	}
}

// ============================================================================
// 集成 — 模拟 DeepSeek V4 / R1 后端的两轮对话
// ============================================================================

// TestSDKClient_DeepSeekV4_SimulatedRoundTrip 模拟 V4 的严格契约：
// 第一轮返回 reasoning_content；第二轮 server 校验 assistant 消息里必须有
// reasoning_content，缺失则 400。client 用 providerName="deepseek-v4"。
func TestSDKClient_DeepSeekV4_SimulatedRoundTrip(t *testing.T) {
	var round int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		round++

		if round == 2 {
			// 第二轮：检查 assistant 消息是否带 reasoning_content
			messages, _ := body["messages"].([]any)
			var asstFound bool
			for _, m := range messages {
				mm := m.(map[string]any)
				if mm["role"] != "assistant" {
					continue
				}
				asstFound = true
				if _, has := mm["reasoning_content"]; !has {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]any{
							"message": "reasoning_content in thinking mode must be passed back",
							"type":    "invalid_request_error",
						},
					})
					return
				}
			}
			if !asstFound {
				t.Errorf("第二轮请求里没有 assistant 消息")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponseWithExtras(
			"round-"+itoa(round),
			map[string]any{"reasoning_content": "thinking on round " + itoa(round)},
		))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "deepseek-v4-flash", "", "deepseek-v4", 30*time.Second)

	// 第一轮
	r1, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "q1"}}, nil)
	if err != nil {
		t.Fatalf("round 1 失败: %v", err)
	}
	if r1.ExtraFields == nil {
		t.Fatal("round 1 ExtraFields 为 nil")
	}

	// 第二轮：携带第一轮的 assistant ExtraFields
	history := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: r1.Content, ExtraFields: r1.ExtraFields},
		{Role: "user", Content: "q2"},
	}
	r2, err := client.Chat(context.Background(), history, nil)
	if err != nil {
		t.Fatalf("round 2 失败（400 说明 reasoning_content 没被回写）: %v", err)
	}
	if r2.Content != "round-2" {
		t.Errorf("round 2 content = %q, want %q", r2.Content, "round-2")
	}
}

// TestSDKClient_DeepSeekR1_StripsOnSecondRound 模拟 R1 的反向契约：
// 第二轮 server 校验 assistant 消息里**不能**带 reasoning_content，否则 400。
// client 用 providerName="deepseek-r1"。
func TestSDKClient_DeepSeekR1_StripsOnSecondRound(t *testing.T) {
	var round int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		round++

		if round == 2 {
			messages, _ := body["messages"].([]any)
			for _, m := range messages {
				mm := m.(map[string]any)
				if mm["role"] != "assistant" {
					continue
				}
				if _, has := mm["reasoning_content"]; has {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]any{
							"message": "reasoning_content must be removed from previous messages",
							"type":    "invalid_request_error",
						},
					})
					return
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponseWithExtras(
			"round-"+itoa(round),
			map[string]any{"reasoning_content": "R1 thinking on round " + itoa(round)},
		))
	}))
	defer server.Close()

	client := NewSDKClient(server.URL, "key", "deepseek-reasoner", "", "deepseek-r1", 30*time.Second)

	r1, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "q1"}}, nil)
	if err != nil {
		t.Fatalf("round 1 失败: %v", err)
	}

	history := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: r1.Content, ExtraFields: r1.ExtraFields},
		{Role: "user", Content: "q2"},
	}
	r2, err := client.Chat(context.Background(), history, nil)
	if err != nil {
		t.Fatalf("round 2 失败（400 说明 R1 provider 没剥离 reasoning_content）: %v", err)
	}
	if r2.Content != "round-2" {
		t.Errorf("round 2 content = %q", r2.Content)
	}
}

// ============================================================================
// helpers
// ============================================================================

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(i int) string {
	switch i {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return "N"
}
