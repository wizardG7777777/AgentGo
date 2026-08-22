package store

import (
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

func TestPublishTaskMigratesLegacyTimeoutToExpectedDuration(t *testing.T) {
	s := NewMemoryTaskStore(nil, 8, 1, 30)
	legacy := &model.Task{Description: "旧发布方", TimeoutSeconds: 7}
	if err := s.PublishTask(legacy); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != 7*time.Second {
		t.Fatalf("ExpectedDuration=%v，期望 7s", got.ExpectedDuration)
	}
	if got.TimeoutSeconds != 7 {
		t.Fatalf("legacy alias 被意外抹除: %d", got.TimeoutSeconds)
	}

	canonical := &model.Task{
		Description: "新发布方", ExpectedDuration: 11 * time.Second, TimeoutSeconds: 99,
	}
	if err := s.PublishTask(canonical); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetTask(canonical.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != 11*time.Second {
		t.Fatalf("canonical ExpectedDuration 被 legacy alias 覆盖: %v", got.ExpectedDuration)
	}
}

func TestPublishTaskDefaultOnlyPopulatesExpectedDuration(t *testing.T) {
	s := NewMemoryTaskStore(nil, 8, 1, 30)
	task := &model.Task{Description: "默认 SLO"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != 30*time.Second {
		t.Fatalf("ExpectedDuration=%v，期望 30s", got.ExpectedDuration)
	}
	if got.TimeoutSeconds != 0 {
		t.Fatalf("新任务不应反向生成 TimeoutSeconds，得到 %d", got.TimeoutSeconds)
	}
}

func TestExpectedDurationSnapshotRoundTripAndLegacyImport(t *testing.T) {
	s := NewMemoryTaskStore(nil, 8, 1, 30)
	task := &model.Task{Description: "roundtrip", ExpectedDuration: 13 * time.Second}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	snaps := s.ExportSnapshot()
	if len(snaps) != 1 || snaps[0].ExpectedDuration != 13*time.Second {
		t.Fatalf("导出 ExpectedDuration 丢失: %+v", snaps)
	}

	restored := NewMemoryTaskStore(nil, 8, 1, 30)
	if err := restored.ImportSnapshot(snaps); err != nil {
		t.Fatal(err)
	}
	got, err := restored.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != 13*time.Second {
		t.Fatalf("恢复 ExpectedDuration=%v，期望 13s", got.ExpectedDuration)
	}

	legacy := session.TaskSnapshot{
		ID: "legacy-snapshot", Status: string(model.TaskStatusPending), TimeoutSeconds: 17,
	}
	if err := restored.ImportSnapshot([]session.TaskSnapshot{legacy}); err != nil {
		t.Fatal(err)
	}
	got, err = restored.GetTask(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpectedDuration != 17*time.Second {
		t.Fatalf("旧快照未迁移: ExpectedDuration=%v", got.ExpectedDuration)
	}
}
