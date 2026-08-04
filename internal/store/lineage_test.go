package store

import (
	"testing"

	"agentgo/internal/model"
)

func TestPublishTaskPrefersExplicitParentAndMigratesLegacyParent(t *testing.T) {
	s, _ := newTestStore(16, 100)
	parentA := &model.Task{ID: "parent-a", Description: "A", EventType: "__scheduler__"}
	parentB := &model.Task{ID: "parent-b", Description: "B", EventType: "__scheduler__"}
	if err := s.PublishTask(parentA); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishTask(parentB); err != nil {
		t.Fatal(err)
	}

	explicit := &model.Task{
		ID: "explicit-child", Description: "child", ParentTaskID: parentA.ID,
		EventSource: parentB.ID, ReplyToAgentID: "scheduler-1", BatchID: "batch-1",
	}
	if err := s.PublishTask(explicit); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(explicit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentTaskID != parentA.ID {
		t.Fatalf("explicit parent was not authoritative: %+v", got)
	}

	legacy := &model.Task{ID: "legacy-child", Description: "legacy", EventSource: parentB.ID}
	if err := s.PublishTask(legacy); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTask(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentTaskID != parentB.ID {
		t.Fatalf("legacy EventSource parent was not migrated: %+v", got)
	}
}

func TestLineageReplyAndBatchSurviveSnapshotRoundTrip(t *testing.T) {
	s1, _ := newTestStore(16, 100)
	parent := &model.Task{ID: "snapshot-parent", Description: "parent"}
	if err := s1.PublishTask(parent); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{
		ID: "snapshot-child", Description: "child", EventSource: parent.ID,
		ParentTaskID: parent.ID, ReplyToAgentID: "scheduler-7", BatchID: "batch-7",
	}
	if err := s1.PublishTask(child); err != nil {
		t.Fatal(err)
	}

	snaps := s1.ExportSnapshot()
	s2, _ := newTestStore(16, 100)
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetTask(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentTaskID != parent.ID || got.ReplyToAgentID != "scheduler-7" || got.BatchID != "batch-7" {
		t.Fatalf("snapshot round trip lost routing metadata: %+v", got)
	}

	// A pre-field snapshot still recovers its parent edge when EventSource names
	// another restored Task, without treating external source labels as parents.
	for i := range snaps {
		if snaps[i].ID == child.ID {
			snaps[i].ParentTaskID = ""
			snaps[i].ReplyToAgentID = ""
			snaps[i].BatchID = ""
		}
	}
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatal(err)
	}
	got, err = s2.GetTask(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentTaskID != parent.ID {
		t.Fatalf("legacy snapshot parent = %q, want %q", got.ParentTaskID, parent.ID)
	}
}
