package agent

import (
	"errors"
	"sync"
	"testing"
)

func TestActivityTracker_TaskAndToolLifecycle(t *testing.T) {
	tracker := NewActivityTracker()
	tracker.RegisterAgent("worker-1", "worker")
	tracker.TaskClaimed("worker-1", "worker", "task-1", "inspect files")
	tracker.LoopStarted("worker-1", "task-1", 2)
	tracker.LLMStart("worker-1", "task-1", 2, 6)
	tracker.LLMEnd("worker-1", "task-1", 2, "I will read the file", 1, nil)
	tracker.ToolStarted("worker-1", "task-1", 2, "call-1", "read_file")

	snap, ok := tracker.Snapshot("worker-1")
	if !ok {
		t.Fatal("snapshot not found")
	}
	if snap.Phase != "tooling" {
		t.Fatalf("phase = %q, want tooling", snap.Phase)
	}
	if snap.LastTool != "read_file" {
		t.Fatalf("LastTool = %q", snap.LastTool)
	}
	if snap.ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d, want 1", snap.ToolCallCount)
	}
	if len(snap.ActiveTools) != 1 {
		t.Fatalf("ActiveTools = %d, want 1", len(snap.ActiveTools))
	}

	tracker.ToolFinished("worker-1", "task-1", 2, "call-1", "read_file", nil)
	tracker.TaskFinished("worker-1", "task-1", true, "react_loop_exit:natural")

	snap, _ = tracker.Snapshot("worker-1")
	if snap.Phase != "completed" {
		t.Fatalf("phase after finish = %q, want completed", snap.Phase)
	}
	if snap.TaskID != "" {
		t.Fatalf("TaskID after finish = %q, want empty", snap.TaskID)
	}
	if len(snap.ActiveTools) != 0 {
		t.Fatalf("ActiveTools after finish = %d, want 0", len(snap.ActiveTools))
	}
}

func TestActivityTracker_RecordsErrors(t *testing.T) {
	tracker := NewActivityTracker()
	tracker.TaskClaimed("worker-1", "worker", "task-1", "do work")
	tracker.ToolStarted("worker-1", "task-1", 0, "call-1", "run_shell")
	tracker.ToolFinished("worker-1", "task-1", 0, "call-1", "run_shell", errors.New("exit status 1"))

	snap, _ := tracker.Snapshot("worker-1")
	if snap.Phase != "tool_error" {
		t.Fatalf("phase = %q, want tool_error", snap.Phase)
	}
	if snap.LastError != "exit status 1" {
		t.Fatalf("LastError = %q", snap.LastError)
	}
}

func TestActivityTracker_ConcurrentUpdates(t *testing.T) {
	tracker := NewActivityTracker()
	tracker.TaskClaimed("worker-1", "worker", "task-1", "parallel work")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.ToolStarted("worker-1", "task-1", 0, "call", "read_file")
			tracker.ToolFinished("worker-1", "task-1", 0, "call", "read_file", nil)
		}()
	}
	wg.Wait()

	snap, _ := tracker.Snapshot("worker-1")
	if snap.ToolCallCount != 50 {
		t.Fatalf("ToolCallCount = %d, want 50", snap.ToolCallCount)
	}
}
