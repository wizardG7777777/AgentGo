package userdef

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

type planAwareFakeStore struct {
	fakeStore
	tasksByID map[string]*model.Task
}

func (s *planAwareFakeStore) GetTask(taskID string) (*model.Task, error) {
	task := s.tasksByID[taskID]
	if task == nil {
		return nil, errors.New("task not found")
	}
	cp := *task
	return &cp, nil
}

func TestPublishTask_PlannedSourceRedirectsToReplan(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "publish.md", "Investigate ${event.task.id}: ${event.task.reason}")
	store := &planAwareFakeStore{tasksByID: map[string]*model.Task{
		"task-1": {ID: "task-1", PlanID: "plan-1"},
	}}
	requester := &fakeReplanRequester{}
	reactors, err := Load([]byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: explorer
      description: { file: ./publish.md }
`), dir, dir, Deps{
		Store: store, ReplanRequester: requester,
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reactors[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1", Reason: "tests failed"}); err != nil {
		t.Fatal(err)
	}
	if got := len(store.snapshot()); got != 0 {
		t.Fatalf("planned Reactor published %d tasks directly", got)
	}
	calls := requester.snapshot()
	if len(calls) != 1 || calls[0].reasonCode != "reactor_publish_task_intent" ||
		calls[0].detail != "Investigate task-1: tests failed" {
		t.Fatalf("redirected request = %+v", calls)
	}
}

func TestPlannedSourceGatesSpawnAndLLMWriteSideEffects(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "action.md", "Handle ${event.task.id}")
	store := &planAwareFakeStore{tasksByID: map[string]*model.Task{
		"task-1": {ID: "task-1", PlanID: "plan-1"},
	}}

	t.Run("spawn_agent", func(t *testing.T) {
		requester := &fakeReplanRequester{}
		host := &fakeSpawnHost{}
		reactors, err := Load([]byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description: { file: ./action.md }
      lifecycle: one_shot
`), dir, dir, Deps{
			Store: store, ReplanRequester: requester, SpawnHost: host,
			KindEventTypes: map[string]string{"explorer": "explore"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := reactors[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1"}); err != nil {
			t.Fatal(err)
		}
		if got := len(host.snapshot()); got != 0 {
			t.Fatalf("planned Reactor spawned %d agents directly", got)
		}
		calls := requester.snapshot()
		if len(calls) != 1 || calls[0].reasonCode != "reactor_spawn_agent_intent" {
			t.Fatalf("redirected request = %+v", calls)
		}
	})

	t.Run("invoke_llm_write_file", func(t *testing.T) {
		requester := &fakeReplanRequester{}
		llm := &fakeLLM{response: "must not be written"}
		outputPath := filepath.Join(dir, "planned-output.md")
		yamlData := []byte(strings.ReplaceAll(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./action.md }
      output:
        write_file: OUTPUT_PATH
`, "OUTPUT_PATH", outputPath))
		reactors, err := Load(yamlData, dir, dir, Deps{Store: store, ReplanRequester: requester, LLM: llm})
		if err != nil {
			t.Fatal(err)
		}
		if err := reactors[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1"}); err != nil {
			t.Fatal(err)
		}
		if llm.lastPrompt() != "" {
			t.Fatal("planned write intent reached the Reactor LLM")
		}
		if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("planned write side effect exists: %v", err)
		}
		calls := requester.snapshot()
		if len(calls) != 1 || calls[0].reasonCode != "reactor_invoke_llm_intent" {
			t.Fatalf("redirected request = %+v", calls)
		}
	})

	t.Run("invoke_llm_send_message", func(t *testing.T) {
		requester := &fakeReplanRequester{}
		llm := &fakeLLM{response: "must not be sent"}
		mailbox := &fakeMailbox{}
		reactors, err := Load([]byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./action.md }
      output:
        send_message: { to: scheduler-1 }
`), dir, dir, Deps{Store: store, ReplanRequester: requester, LLM: llm, Mailbox: mailbox})
		if err != nil {
			t.Fatal(err)
		}
		if err := reactors[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1"}); err != nil {
			t.Fatal(err)
		}
		if llm.lastPrompt() != "" || len(mailbox.snapshot()) != 0 {
			t.Fatal("planned invoke_llm reached an unbudgeted LLM or mailbox side effect")
		}
		calls := requester.snapshot()
		if len(calls) != 1 || calls[0].reasonCode != "reactor_invoke_llm_intent" {
			t.Fatalf("redirected request = %+v", calls)
		}
	})
}

func TestPublishTask_PlannedSourceFailsClosedWithoutReplanRequester(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "publish.md", "Investigate ${event.task.id}")
	store := &planAwareFakeStore{tasksByID: map[string]*model.Task{
		"task-1": {ID: "task-1", PlanID: "plan-1"},
	}}
	reactors, err := Load([]byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: explorer
      description: { file: ./publish.md }
`), dir, dir, Deps{Store: store, KindEventTypes: map[string]string{"explorer": "explore"}})
	if err != nil {
		t.Fatal(err)
	}
	err = reactors[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1"})
	if err == nil || !strings.Contains(err.Error(), "planned source requires request_replan") {
		t.Fatalf("Run error = %v", err)
	}
	if got := len(store.snapshot()); got != 0 {
		t.Fatalf("fail-closed Reactor published %d tasks", got)
	}
}
