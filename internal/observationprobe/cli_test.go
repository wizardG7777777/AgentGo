package observationprobe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentgo/internal/config"
)

func TestRunV8UsesSharedSchemaAutoLow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["tool_choice"] != "auto" {
			t.Errorf("tool_choice=%#v", request["tool_choice"])
		}
		reasoning, _ := request["reasoning"].(map[string]any)
		if reasoning["effort"] != "low" {
			t.Errorf("reasoning=%#v", reasoning)
		}
		tools, _ := request["tools"].([]any)
		encoded, _ := json.Marshal(tools)
		if string(encoded) == "" || containsEmptyEnum(encoded) {
			t.Errorf("probe 使用了非法/漂移 schema: %s", encoded)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp", "object": "response", "status": "completed",
			"output": []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "call",
				"name": "record_observation_delta", "status": "completed",
				"arguments": `{"phase":"investigate","facts":[],"resolved_candidates":[],"next_candidates":[],"next_action":{"decision":"continue"}}`}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
				"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}},
		})
	}))
	defer server.Close()
	cfg := &config.Config{LLM: config.LLMConfig{BaseURL: server.URL, APIKey: "test", DefaultModel: "m",
		Protocol: "responses", DefaultContextWindowTokens: 131072, DefaultMaxCompletionTokens: 16384}}
	report := run(cfg, "m", "v8", "empty", 1)
	if report.Successes != 1 || len(report.Failures) != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func containsEmptyEnum(data []byte) bool {
	for i := 0; i+9 <= len(data); i++ {
		if string(data[i:i+9]) == `"enum":[]` {
			return true
		}
	}
	return false
}
