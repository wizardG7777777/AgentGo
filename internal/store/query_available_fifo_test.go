package store

import (
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestQueryAvailable_PriorityThenFIFOWithStableIDTieBreak(t *testing.T) {
	s, _ := newTestStore(16, 100)
	base := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)

	tasks := []*model.Task{
		{ID: "high-new", Description: "high newest", Priority: 10, EventType: "code"},
		{ID: "low-old", Description: "low oldest", Priority: 1, EventType: "code"},
		{ID: "high-same-b", Description: "high oldest b", Priority: 10, EventType: "code"},
		{ID: "high-same-a", Description: "high oldest a", Priority: 10, EventType: "code"},
	}
	for _, task := range tasks {
		if err := s.PublishTask(task); err != nil {
			t.Fatalf("PublishTask(%s): %v", task.ID, err)
		}
	}

	times := map[string]time.Time{
		"high-new":    base.Add(2 * time.Minute),
		"low-old":     base.Add(-time.Minute),
		"high-same-b": base,
		"high-same-a": base,
	}
	for id, createdAt := range times {
		if err := s.SetTaskTiming(id, createdAt, time.Time{}); err != nil {
			t.Fatalf("SetTaskTiming(%s): %v", id, err)
		}
	}

	want := []string{"high-same-a", "high-same-b", "high-new", "low-old"}
	for round := 0; round < 5; round++ {
		got, err := s.QueryAvailable("code")
		if err != nil {
			t.Fatalf("QueryAvailable: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("round %d: len=%d, want %d", round, len(got), len(want))
		}
		for i, wantID := range want {
			if got[i].ID != wantID {
				t.Fatalf("round %d: result[%d]=%s, want %s", round, i, got[i].ID, wantID)
			}
		}
	}
}
