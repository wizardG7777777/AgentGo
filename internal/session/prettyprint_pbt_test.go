package session

import (
	"testing"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 3: Pretty Printer round-trip
// **Validates: Requirements 6.8**
//
// For any valid HistoryEvent, PrettyPrint → ParsePrettyPrint SHALL produce
// an equivalent event (same timestamp, event_type, and payload values).
func TestProperty_PrettyPrintRoundTrip(t *testing.T) {
	allEventTypes := []string{
		HistEventTaskPublished,
		HistEventTaskClaimed,
		HistEventTaskSubmitted,
		HistEventTaskCompleted,
		HistEventTaskFailed,
		HistEventTaskRetry,
		HistEventRosterClaim,
		HistEventRosterRelease,
		HistEventMailSent,
		HistEventMailDelivered,
	}

	rapid.Check(t, func(t *rapid.T) {
		eventType := rapid.SampledFrom(allEventTypes).Draw(t, "eventType")
		timestamp := genTimestamp(t, "ts")

		// Generate payload with safe values (no special chars that break parsing)
		payloadSize := rapid.IntRange(0, 5).Draw(t, "payloadSize")
		payload := make(map[string]any, payloadSize)
		for i := 0; i < payloadSize; i++ {
			key := rapid.StringMatching(`[a-z_]{2,12}`).Draw(t, labelIdx("key", i))
			// Generate values that round-trip cleanly:
			// - simple strings (no spaces → unquoted)
			// - strings with spaces (→ quoted)
			// - integers
			valType := rapid.IntRange(0, 2).Draw(t, labelIdx("valType", i))
			switch valType {
			case 0: // simple string (no special chars)
				payload[key] = rapid.StringMatching(`[a-zA-Z0-9\-]{1,20}`).Draw(t, labelIdx("strVal", i))
			case 1: // integer
				payload[key] = rapid.IntRange(0, 10000).Draw(t, labelIdx("intVal", i))
			case 2: // string with spaces (will be quoted)
				payload[key] = rapid.StringMatching(`[a-zA-Z]{1,8} [a-zA-Z]{1,8}`).Draw(t, labelIdx("spacedVal", i))
			}
		}

		original := HistoryEvent{
			Timestamp: timestamp,
			EventType: eventType,
			Payload:   payload,
		}

		// PrettyPrint → ParsePrettyPrint
		text := PrettyPrint(original)
		parsed, err := ParsePrettyPrint(text)
		if err != nil {
			t.Fatalf("ParsePrettyPrint(%q) failed: %v", text, err)
		}

		// Verify timestamp
		if parsed.Timestamp != original.Timestamp {
			t.Fatalf("Timestamp: got %q, want %q", parsed.Timestamp, original.Timestamp)
		}

		// Verify event_type
		if parsed.EventType != original.EventType {
			t.Fatalf("EventType: got %q, want %q", parsed.EventType, original.EventType)
		}

		// Verify payload keys and values
		if len(parsed.Payload) != len(original.Payload) {
			t.Fatalf("Payload size: got %d, want %d\ntext: %q\nparsed: %+v\noriginal: %+v",
				len(parsed.Payload), len(original.Payload), text, parsed.Payload, original.Payload)
		}

		for k, origVal := range original.Payload {
			parsedVal, ok := parsed.Payload[k]
			if !ok {
				t.Fatalf("Payload key %q missing after round-trip\ntext: %q", k, text)
			}

			// Normalize types for comparison:
			// PrettyPrint outputs int as decimal string, ParsePrettyPrint parses back as int64
			origNorm := normalizePayloadValue(origVal)
			parsedNorm := normalizePayloadValue(parsedVal)

			if origNorm != parsedNorm {
				t.Fatalf("Payload[%q]: got %v (%T), want %v (%T)\ntext: %q",
					k, parsedVal, parsedVal, origVal, origVal, text)
			}
		}
	})
}

// normalizePayloadValue normalizes a payload value for comparison.
// Converts all numeric types to int64 for consistent comparison.
func normalizePayloadValue(v any) any {
	switch val := v.(type) {
	case int:
		return int64(val)
	case int64:
		return val
	case float64:
		if val == float64(int64(val)) {
			return int64(val)
		}
		return val
	default:
		return v
	}
}
