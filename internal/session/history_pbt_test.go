package session

import (
	"encoding/json"
	"regexp"
	"testing"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 9: HistoryEvent 結構完整性
// **Validates: Requirements 6.2, 9.3, 9.4**
//
// For any valid HistoryEvent, its serialized JSON SHALL contain:
// - ts (non-empty string)
// - event_type (matches [a-z_]+ snake_case format)
// - payload (non-nil map)
// and each record is self-contained.
func TestProperty_HistoryEventStructure(t *testing.T) {
	snakeCaseRe := regexp.MustCompile(`^[a-z_]+$`)

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
		// Generate a random HistoryEvent
		eventType := rapid.SampledFrom(allEventTypes).Draw(t, "eventType")

		// Generate random payload with 0-5 keys
		payloadSize := rapid.IntRange(0, 5).Draw(t, "payloadSize")
		payload := make(map[string]any, payloadSize)
		for i := 0; i < payloadSize; i++ {
			key := rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "payloadKey")
			payload[key] = rapid.StringMatching(`[a-zA-Z0-9_ ]{0,50}`).Draw(t, "payloadVal")
		}

		ev := HistoryEvent{
			Timestamp: nowUTC(),
			EventType: eventType,
			Payload:   payload,
		}

		// Serialize to JSON
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}

		// Parse back as generic map to verify structure
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("json.Unmarshal to map failed: %v", err)
		}

		// Verify "ts" field exists and is non-empty
		tsVal, ok := raw["ts"]
		if !ok {
			t.Fatal("serialized JSON missing 'ts' field")
		}
		tsStr, ok := tsVal.(string)
		if !ok || tsStr == "" {
			t.Fatalf("'ts' field is empty or not a string: %v", tsVal)
		}

		// Verify "event_type" field exists and matches snake_case
		etVal, ok := raw["event_type"]
		if !ok {
			t.Fatal("serialized JSON missing 'event_type' field")
		}
		etStr, ok := etVal.(string)
		if !ok || etStr == "" {
			t.Fatalf("'event_type' field is empty or not a string: %v", etVal)
		}
		if !snakeCaseRe.MatchString(etStr) {
			t.Fatalf("'event_type' %q does not match [a-z_]+", etStr)
		}

		// Verify "payload" field exists and is non-nil (JSON object)
		plVal, ok := raw["payload"]
		if !ok {
			t.Fatal("serialized JSON missing 'payload' field")
		}
		if plVal == nil {
			t.Fatal("'payload' field is nil")
		}
		if _, ok := plVal.(map[string]any); !ok {
			t.Fatalf("'payload' field is not a JSON object: %T", plVal)
		}
	})
}
