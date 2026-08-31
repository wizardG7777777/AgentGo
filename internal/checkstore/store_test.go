package checkstore

import (
	"testing"
	"time"
)

func TestListTaskReturnsTypedChecksInSettlementOrder(t *testing.T) {
	store := New(t.TempDir())
	now := time.Now().UTC()
	for _, record := range []Record{
		{Schema: SchemaV1, RunID: "run-1", TaskID: "task-1", AttemptID: "attempt-2",
			CheckID: "verification", Kind: "test", CommandDigest: "sha256:2", Status: StatusPass,
			ExitCode: 0, WorkspaceRevisionRef: "workspace:2", StartedAt: now.Add(time.Second), SettledAt: now.Add(2 * time.Second)},
		{Schema: SchemaV1, RunID: "run-1", TaskID: "task-1", AttemptID: "attempt-1",
			CheckID: "verification", Kind: "test", CommandDigest: "sha256:1", Status: StatusFailed,
			ExitCode: 1, WorkspaceRevisionRef: "workspace:1", StartedAt: now, SettledAt: now.Add(time.Second)},
	} {
		if _, err := store.Put(record); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.ListTask("task-1")
	if err != nil || len(records) != 2 || records[0].Status != StatusFailed || records[1].Status != StatusPass {
		t.Fatalf("ListTask 顺序/内容错误: records=%+v err=%v", records, err)
	}
}
