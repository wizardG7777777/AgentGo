package store

import (
	"testing"

	"agentgo/internal/model"
)

func TestTaskContextInputsSurviveCloneAndSessionSnapshot(t *testing.T) {
	store := NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{
		ID: "task-context-input-snapshot", Description: "目标",
		ContextInputs: []model.TaskContextInput{{
			Kind: model.TaskContextUpstreamResult, SourceRef: "graph:g/activation:a@1/result:default",
			Content: `<upstream-result>{"ok":true}</upstream-result>`,
		}},
	}
	if err := store.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	clone, err := store.GetTask(task.ID)
	if err != nil || len(clone.ContextInputs) != 1 {
		t.Fatalf("Store clone 丢失 ContextInputs: %+v err=%v", clone, err)
	}
	clone.ContextInputs[0].Content = "mutated"
	fresh, _ := store.GetTask(task.ID)
	if fresh.ContextInputs[0].Content == "mutated" {
		t.Fatal("Store clone 暴露了 ContextInputs 内部切片")
	}
	snapshot := store.ExportSnapshot()
	restored := NewMemoryTaskStore(nil, 16, 1, 60)
	if err := restored.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := restored.GetTask(task.ID)
	if err != nil || len(got.ContextInputs) != 1 || got.ContextInputs[0] != task.ContextInputs[0] {
		t.Fatalf("Session snapshot 丢失 typed ContextInputs: %+v err=%v", got, err)
	}
}
