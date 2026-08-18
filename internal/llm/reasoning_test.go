package llm

import (
	"encoding/json"
	"testing"
)

func TestReasoningTextSupportsOpenRouterShapes(t *testing.T) {
	tests := []struct {
		name   string
		extras map[string]json.RawMessage
		want   string
	}{
		{
			name:   "reasoning string",
			extras: map[string]json.RawMessage{"reasoning": json.RawMessage(`"raw reasoning"`)},
			want:   "raw reasoning",
		},
		{
			name:   "reasoning content alias",
			extras: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"alias reasoning"`)},
			want:   "alias reasoning",
		},
		{
			name: "structured details",
			extras: map[string]json.RawMessage{"reasoning_details": json.RawMessage(`[
				{"type":"reasoning.summary","summary":"summary"},
				{"type":"reasoning.text","text":" raw text"}
			]`)},
			want: "summary raw text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReasoningText(tt.extras); got != tt.want {
				t.Fatalf("ReasoningText() = %q, want %q", got, tt.want)
			}
		})
	}
}
