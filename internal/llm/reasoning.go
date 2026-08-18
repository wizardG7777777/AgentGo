package llm

import "encoding/json"

// ReasoningText extracts provider-supplied plaintext reasoning without
// rewriting or summarizing it. OpenRouter may return a direct reasoning string,
// the reasoning_content alias, or structured reasoning_details blocks.
func ReasoningText(extraFields map[string]json.RawMessage) string {
	for _, key := range []string{"reasoning", "reasoning_content"} {
		if text := rawJSONString(extraFields[key]); text != "" {
			return text
		}
	}
	return reasoningDetailsText(extraFields["reasoning_details"])
}

func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return text
}

func reasoningDetailsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var details []json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return ""
	}
	var text string
	for _, detail := range details {
		if direct := rawJSONString(detail); direct != "" {
			text += direct
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(detail, &fields); err != nil {
			continue
		}
		for _, key := range []string{"text", "summary", "content"} {
			if fragment := rawJSONString(fields[key]); fragment != "" {
				text += fragment
				break
			}
		}
	}
	return text
}
