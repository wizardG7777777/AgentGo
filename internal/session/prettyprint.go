package session

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PrettyPrint formats a HistoryEvent as human-readable text.
// Format: [timestamp] event_type key1=value1 key2="value with spaces"
//
// Keys are sorted alphabetically for deterministic output.
// String values containing spaces or quotes are double-quoted with escaping.
func PrettyPrint(event HistoryEvent) string {
	var sb strings.Builder
	sb.WriteString("[")
	sb.WriteString(event.Timestamp)
	sb.WriteString("] ")
	sb.WriteString(event.EventType)

	if len(event.Payload) > 0 {
		// Sort keys for deterministic output
		keys := make([]string, 0, len(event.Payload))
		for k := range event.Payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			sb.WriteString(" ")
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(formatValue(event.Payload[k]))
		}
	}

	return sb.String()
}

// formatValue formats a payload value for pretty printing.
// Strings are always double-quoted to preserve type information during round-trip.
// Numbers are formatted as-is (unquoted).
func formatValue(v any) string {
	switch val := v.(type) {
	case string:
		return strconv.Quote(val)
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case bool:
		return strconv.FormatBool(val)
	case []any:
		parts := make([]string, len(val))
		for i, item := range val {
			parts[i] = formatValue(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", val)
	}
}

// (needsQuoting removed — all strings are now always quoted for round-trip safety)

// ParsePrettyPrint parses a PrettyPrint-formatted text back into a HistoryEvent.
// Expected format: [timestamp] event_type key1=value1 key2="quoted value"
func ParsePrettyPrint(text string) (HistoryEvent, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return HistoryEvent{}, fmt.Errorf("empty input")
	}

	// Parse timestamp: [...]
	if text[0] != '[' {
		return HistoryEvent{}, fmt.Errorf("expected '[' at start")
	}
	closeBracket := strings.Index(text, "]")
	if closeBracket < 0 {
		return HistoryEvent{}, fmt.Errorf("missing closing ']'")
	}
	timestamp := text[1:closeBracket]
	rest := strings.TrimSpace(text[closeBracket+1:])

	// Parse event_type (first token)
	eventType, rest := nextToken(rest)
	if eventType == "" {
		return HistoryEvent{}, fmt.Errorf("missing event_type")
	}

	// Parse key=value pairs
	payload := make(map[string]any)
	for rest != "" {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}

		// Find key=
		eqIdx := strings.Index(rest, "=")
		if eqIdx < 0 {
			break
		}
		key := rest[:eqIdx]
		rest = rest[eqIdx+1:]

		// Parse value
		var value string
		var err error
		value, rest, err = parseValue(rest)
		if err != nil {
			return HistoryEvent{}, fmt.Errorf("parse value for key %q: %w", key, err)
		}

		payload[key] = inferType(value)
	}

	return HistoryEvent{
		Timestamp: timestamp,
		EventType: eventType,
		Payload:   payload,
	}, nil
}

// nextToken extracts the next whitespace-delimited token from s.
func nextToken(s string) (token, rest string) {
	s = strings.TrimSpace(s)
	idx := strings.IndexByte(s, ' ')
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// parseValue parses a value from the rest of the string.
// Handles quoted strings (with escape sequences) and unquoted tokens.
// Also handles array values like [val1,val2].
// Since PrettyPrint always quotes strings, unquoted values are numbers/bools/null.
func parseValue(s string) (value, rest string, err error) {
	if len(s) == 0 {
		return "", "", nil
	}

	if s[0] == '"' {
		// Quoted string — use strconv.Unquote
		// Find the end of the quoted string
		i := 1
		for i < len(s) {
			if s[i] == '\\' {
				i += 2 // skip escaped char
				continue
			}
			if s[i] == '"' {
				unquoted, err := strconv.Unquote(s[:i+1])
				if err != nil {
					return "", "", fmt.Errorf("unquote: %w", err)
				}
				// Mark as string by wrapping in a sentinel — but actually
				// we return the unquoted string and let inferType handle it.
				// Since it was quoted, we know it's a string.
				return "\"" + unquoted + "\"", strings.TrimSpace(s[i+1:]), nil
			}
			i++
		}
		return "", "", fmt.Errorf("unterminated quoted string")
	}

	if s[0] == '[' {
		// Array value — find matching ]
		closeBracket := strings.Index(s, "]")
		if closeBracket < 0 {
			return "", "", fmt.Errorf("unterminated array")
		}
		return s[:closeBracket+1], strings.TrimSpace(s[closeBracket+1:]), nil
	}

	// Unquoted value — read until next space
	idx := strings.IndexByte(s, ' ')
	if idx < 0 {
		return s, "", nil
	}
	return s[:idx], s[idx+1:], nil
}

// inferType converts a raw parsed value to its appropriate Go type.
// Values wrapped in quotes (from parseValue) are strings.
// Unquoted values are tried as numbers, bools, arrays, or null.
func inferType(s string) any {
	// If the value was originally quoted, it's a string
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}

	if s == "null" {
		return nil
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}

	// Try array
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := s[1 : len(s)-1]
		if inner == "" {
			return []any{}
		}
		parts := strings.Split(inner, ",")
		result := make([]any, len(parts))
		for i, p := range parts {
			result[i] = inferType(strings.TrimSpace(p))
		}
		return result
	}

	// Try integer
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}

	// Try float
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}

	return s
}
