package graph

import "testing"

func TestValidateNodeOutput(t *testing.T) {
	contract := &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{
		{Path: "$.changed", Type: "boolean", Required: true},
		{Path: "$.stats.count", Type: "integer", Required: true},
		{Path: "$.notes", Type: "array"},
	}}
	valid := map[string]any{"changed": true, "stats": map[string]any{"count": float64(2)}, "notes": []any{"ok"}}
	if err := ValidateNodeOutput(contract, "完成", valid); err != nil {
		t.Fatalf("合法 typed output 被拒绝: %v", err)
	}
	for name, test := range map[string]struct {
		summary string
		result  map[string]any
	}{
		"empty-summary": {summary: "", result: valid},
		"missing":       {summary: "完成", result: map[string]any{"changed": true}},
		"wrong-type":    {summary: "完成", result: map[string]any{"changed": "yes", "stats": map[string]any{"count": 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateNodeOutput(contract, test.summary, test.result); err == nil {
				t.Fatal("非法 typed output 必须拒绝")
			}
		})
	}
}
