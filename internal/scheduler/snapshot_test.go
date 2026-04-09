package scheduler

import (
	"strings"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// 这些测试从旧的 internal/scheduler/scheduler_test.go::TestScheduler_BuildBoardJSON_*
// 迁移而来。原测试调用 sched.buildBoardJSON 私有方法，现在直接测公开 helper
// BuildBoardJSON。

func TestBuildBoardJSON_Resources(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 4}

	// 一个 processing worker task，agent worker-1 持有
	t1 := &model.Task{Description: "in flight"}
	if err := s.PublishTask(t1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.ClaimTask("worker-1", t1.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTickerWakeup})

	// resources 应当反映：worker_count=4, busy=1, available=3
	if !strings.Contains(out, `"worker_count": 4`) {
		t.Errorf("expected worker_count=4 in output, got: %s", out)
	}
	if !strings.Contains(out, `"busy_workers": 1`) {
		t.Errorf("expected busy_workers=1, got: %s", out)
	}
	if !strings.Contains(out, `"available_workers": 3`) {
		t.Errorf("expected available_workers=3, got: %s", out)
	}
}

func TestBuildBoardJSON_ResourcesDefault(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{} // WorkerCount=0 → 默认 1

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput})

	if !strings.Contains(out, `"worker_count": 1`) {
		t.Errorf("expected worker_count default 1, got: %s", out)
	}
	if !strings.Contains(out, `"available_workers": 1`) {
		t.Errorf("expected available_workers=1, got: %s", out)
	}
}

func TestBuildBoardJSON_TriggerFields(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	out := BuildBoardJSON(s, cfg, "plan", model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "hello world"},
	})

	if !strings.Contains(out, `"mode": "plan"`) {
		t.Errorf("expected mode=plan, got: %s", out)
	}
	if !strings.Contains(out, `"type": "user_input"`) {
		t.Errorf("expected trigger.type=user_input, got: %s", out)
	}
	if !strings.Contains(out, `"text": "hello world"`) {
		t.Errorf("expected trigger.text=hello world, got: %s", out)
	}
}

func TestBuildBoardJSON_TaskWithArtifacts(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	task := &model.Task{Description: "writes a file"}
	s.PublishTask(task)
	s.ClaimTask("worker-1", task.ID)
	s.AppendArtifact(task.ID, "docs/result.md")
	s.SubmitResult("worker-1", task.ID, "done")

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTaskCompleted})

	if !strings.Contains(out, "docs/result.md") {
		t.Errorf("expected artifact in snapshot, got: %s", out)
	}
}

func TestBuildBoardJSON_EmptyStore(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 2}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput})

	if !strings.Contains(out, `"worker_count": 2`) {
		t.Errorf("expected worker_count=2 in empty store snapshot, got: %s", out)
	}
}
