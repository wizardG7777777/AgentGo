package session

import (
	"regexp"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: session-logging, Property 4: Metadata 創建完整性
// **Validates: Requirements 1.2, 3.1**
func TestProperty_MetadataCreation(t *testing.T) {
	// UUID v4 regex: 8-4-4-4-12 hex digits, version nibble = 4, variant nibble = [89ab]
	uuidV4Re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	rapid.Check(t, func(t *rapid.T) {
		// Generate random SessionConfig values (not used in NewMetadata directly,
		// but we generate them to satisfy the property spec requirement)
		_ = SessionConfig{
			RetentionDays: rapid.IntRange(1, 365).Draw(t, "retentionDays"),
			ArchiveMax:    rapid.IntRange(1, 1000).Draw(t, "archiveMax"),
			Enabled:       rapid.Bool().Draw(t, "enabled"),
		}

		m := NewMetadata()

		// Verify session_id is valid UUID v4 format
		if !uuidV4Re.MatchString(m.SessionID) {
			t.Fatalf("session_id %q is not a valid UUID v4", m.SessionID)
		}

		// Verify created_at is valid UTC ISO 8601 (RFC3339Nano)
		parsed, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
		if err != nil {
			t.Fatalf("created_at %q is not valid RFC3339Nano: %v", m.CreatedAt, err)
		}
		if parsed.Location() != time.UTC {
			t.Fatalf("created_at %q is not in UTC", m.CreatedAt)
		}

		// Verify status is "active"
		if m.Status != "active" {
			t.Fatalf("status = %q, want %q", m.Status, "active")
		}

		// Verify first_user_input is empty string
		if m.FirstUserInput != "" {
			t.Fatalf("first_user_input = %q, want empty string", m.FirstUserInput)
		}

		// Verify task_count is 0
		if m.TaskCount != 0 {
			t.Fatalf("task_count = %d, want 0", m.TaskCount)
		}
	})
}
