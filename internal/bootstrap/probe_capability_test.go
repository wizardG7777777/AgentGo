package bootstrap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/config"
)

func TestStartupToolProbeExercisesRealFunctionCalling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("probe 请求路径=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("probe 未携带真实 tool schema: %+v", body)
		}
		choice, _ := body["tool_choice"].(map[string]any)
		function, _ := choice["function"].(map[string]any)
		probeName, _ := function["name"].(string)
		tool, _ := tools[0].(map[string]any)
		functionDef, _ := tool["function"].(map[string]any)
		parameters, _ := functionDef["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		nonceDef, _ := properties["nonce"].(map[string]any)
		probeNonce, _ := nonceDef["const"].(string)
		if choice["type"] != "function" || !strings.HasPrefix(probeName, "agentgo_capability_probe_") {
			t.Fatalf("probe 未强制 exact function tool_choice: %+v", body["tool_choice"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "probe", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant", "content": "",
					"tool_calls": []any{map[string]any{
						"id": "call-probe", "type": "function",
						"function": map[string]any{"name": probeName, "arguments": fmt.Sprintf(`{"nonce":%q}`, probeNonce)},
					}},
				},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer server.Close()
	cfg := &config.Config{
		StartupProbe: "tool", StartupProbeTimeoutSec: 5,
		LLM: config.LLMConfig{BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "chat_completions"},
	}
	var output bytes.Buffer
	if err := startupProbe(&output, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "typed function-call/required-arguments") {
		t.Fatalf("probe 输出未证明 tool capability: %s", output.String())
	}
}

func TestStartupToolProbeUsesResponsesTypedItemMainline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("Responses probe path=%q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		choice, _ := body["tool_choice"].(map[string]any)
		name, _ := choice["name"].(string)
		tools, _ := body["tools"].([]any)
		tool, _ := tools[0].(map[string]any)
		parameters, _ := tool["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		nonceDef, _ := properties["nonce"].(map[string]any)
		nonce, _ := nonceDef["const"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-probe", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "function_call", "id": "fc-1", "call_id": "call-1", "status": "completed",
				"name": name, "arguments": fmt.Sprintf(`{"nonce":%q}`, nonce),
			}},
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		})
	}))
	defer server.Close()
	cfg := &config.Config{
		StartupProbe: "tool", StartupProbeTimeoutSec: 5,
		LLM: config.LLMConfig{
			BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "responses",
		},
	}
	var output bytes.Buffer
	if err := startupProbe(&output, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "protocol=responses") {
		t.Fatalf("未报告 Responses probe 证据: %s", output.String())
	}
}

func TestStartupToolProbeRetriesInconclusiveTruncationWithinFrozenDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		choice, _ := body["tool_choice"].(map[string]any)
		function, _ := choice["function"].(map[string]any)
		probeName, _ := function["name"].(string)
		tools, _ := body["tools"].([]any)
		tool, _ := tools[0].(map[string]any)
		functionDef, _ := tool["function"].(map[string]any)
		parameters, _ := functionDef["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		nonceDef, _ := properties["nonce"].(map[string]any)
		probeNonce, _ := nonceDef["const"].(string)
		w.Header().Set("Content-Type", "application/json")
		finishReason := "tool_calls"
		message := map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{
				"id": "call-probe", "type": "function",
				"function": map[string]any{"name": probeName, "arguments": fmt.Sprintf(`{"nonce":%q}`, probeNonce)},
			}},
		}
		if calls.Add(1) == 1 {
			finishReason = "length"
			message = map[string]any{"role": "assistant", "content": ""}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "probe", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": finishReason, "message": message,
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer server.Close()
	cfg := &config.Config{
		StartupProbe: "tool", StartupProbeTimeoutSec: 5,
		LLM: config.LLMConfig{BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "chat_completions"},
	}
	var output bytes.Buffer
	if err := startupToolCapabilityProbe(&output, cfg, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !strings.Contains(output.String(), "[RETRY]") ||
		!strings.Contains(output.String(), "attempts=2") {
		t.Fatalf("截断后应在同一总 deadline 内重采样并记录次数: calls=%d output=%s", calls.Load(), output.String())
	}
}

func TestStartupToolProbeRejectsTextOnlyProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "probe", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "nonce=agentgo-capability-probe-v1"},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer server.Close()
	cfg := &config.Config{
		StartupProbe: "tool", StartupProbeTimeoutSec: 5,
		LLM: config.LLMConfig{BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "chat_completions"},
	}
	if err := startupProbe(&bytes.Buffer{}, cfg); err == nil || !strings.Contains(err.Error(), "function-call capability 不兼容") {
		t.Fatalf("text-only provider 应被 capability gate 拒绝: %v", err)
	}
}

func TestStartupToolProbeRejectsWrongFunctionIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		toolName  string
		arguments string
	}{
		{name: "wrong name", toolName: "other_probe", arguments: `{"nonce":"wrong"}`},
		{name: "unexpected arguments", toolName: "agentgo_capability_probe_wrong", arguments: `{"unexpected":"x"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				choice, _ := body["tool_choice"].(map[string]any)
				function, _ := choice["function"].(map[string]any)
				requestedName, _ := function["name"].(string)
				toolName := test.toolName
				if test.name == "unexpected arguments" {
					toolName = requestedName
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "probe", "object": "chat.completion", "created": 1, "model": "test-model",
					"choices": []any{map[string]any{
						"index": 0, "finish_reason": "tool_calls",
						"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
							"id": "call-probe", "type": "function",
							"function": map[string]any{"name": toolName, "arguments": test.arguments},
						}}},
					}},
				})
			}))
			defer server.Close()
			cfg := &config.Config{
				StartupProbe: "tool", StartupProbeTimeoutSec: 5,
				LLM: config.LLMConfig{BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "chat_completions"},
			}
			if err := startupProbe(&bytes.Buffer{}, cfg); err == nil || !strings.Contains(err.Error(), "function-call capability 不兼容") {
				t.Fatalf("错误 function identity 应被拒绝: %v", err)
			}
		})
	}
}

func TestStartupToolProbeRejectsProviderErrorAndTimeout(t *testing.T) {
	t.Run("provider error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"code":"unavailable"}}`, http.StatusServiceUnavailable)
		}))
		defer server.Close()
		cfg := &config.Config{
			StartupProbe: "tool", LLM: config.LLMConfig{
				BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "chat_completions",
			},
		}
		if err := startupToolCapabilityProbe(&bytes.Buffer{}, cfg, time.Second); err == nil {
			t.Fatal("provider 503 不得通过 capability probe")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[]}`))
		}))
		defer server.Close()
		cfg := &config.Config{
			StartupProbe: "tool", LLM: config.LLMConfig{
				BaseURL: server.URL, APIKey: "test-key", DefaultModel: "test-model", Protocol: "chat_completions",
			},
		}
		if err := startupToolCapabilityProbe(&bytes.Buffer{}, cfg, 20*time.Millisecond); err == nil {
			t.Fatal("超时不得通过 capability probe")
		}
	})
}
