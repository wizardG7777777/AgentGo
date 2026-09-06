// Package testhttp 为协议测试提供标准 SSE 帧，生产代码不得导入。
package testhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// WriteSSE 把测试结果描述发布成流式协议事件；错误信封仍使用 HTTP JSON 错误格式。
func WriteSSE(w http.ResponseWriter, r *http.Request, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if _, ok := body["error"]; ok {
		return json.NewEncoder(w).Encode(body)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return err
	}
	if r.URL.Path == "/responses" || r.URL.Path == "/v1/responses" {
		items, _ := body["output"].([]any)
		for index, value := range items {
			item, _ := value.(map[string]any)
			switch item["type"] {
			case "function_call":
				if args, ok := item["arguments"].(string); ok {
					if err := emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": item["id"], "delta": args}); err != nil {
						return err
					}
				}
			case "message":
				if parts, ok := item["content"].([]any); ok {
					for _, part := range parts {
						p, _ := part.(map[string]any)
						if text, ok := p["text"].(string); ok {
							if err := emit(map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": item["id"], "delta": text}); err != nil {
								return err
							}
						}
					}
				}
			}
			if err := emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}); err != nil {
				return err
			}
		}
		event := "response.completed"
		if body["status"] == "incomplete" {
			event = "response.incomplete"
		}
		if body["status"] == "failed" {
			event = "response.failed"
		}
		return emit(map[string]any{"type": event, "response": body})
	}
	choices, _ := body["choices"].([]any)
	for _, value := range choices {
		choice, _ := value.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if calls, ok := message["tool_calls"].([]any); ok {
			for i, c := range calls {
				c.(map[string]any)["index"] = i
			}
		}
		if err := emit(map[string]any{"id": body["id"], "object": "chat.completion.chunk", "model": body["model"], "choices": []any{map[string]any{"index": choice["index"], "delta": message, "finish_reason": choice["finish_reason"]}}, "usage": body["usage"]}); err != nil {
			return err
		}
	}
	_, err = fmt.Fprint(w, "data: [DONE]\n\n")
	return err
}
