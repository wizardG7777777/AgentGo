package userdef

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// ── template.go ──────────────────────────────────────────────────────

func TestRenderTemplate_BasicSubst(t *testing.T) {
	got := renderTemplate("task=${event.task.id} retry=${event.task.retry_count}",
		trace.Event{TaskID: "T-1", AttemptNo: 3})
	want := "task=T-1 retry=3"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestRenderTemplate_TransitionFields(t *testing.T) {
	got := renderTemplate("retry=${event.task.retry_count} from=${event.task.prev_status} to=${event.task.new_status} cause=${event.cause}",
		trace.Event{
			AttemptNo: 1,
			Transition: &trace.Transition{
				PrevStatus: "processing",
				NewStatus:  "failed",
				RetryCount: 4,
				Cause:      "non_recoverable_error",
			},
		})
	want := "retry=4 from=processing to=failed cause=non_recoverable_error"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestRenderTemplate_UnknownPath_EmptyString(t *testing.T) {
	// 运行期遇到未知路径返回空串（防御性兜底）
	got := renderTemplate("x=${event.bogus}", trace.Event{})
	if got != "x=" {
		t.Errorf("unknown path should resolve to empty, got %q", got)
	}
}

func TestValidatePaths_Reject(t *testing.T) {
	if err := validatePaths(`hello ${event.bogus}`); err == nil {
		t.Error("expected error for unknown path")
	}
	if err := validatePaths(`hello ${event.task.id}`); err != nil {
		t.Errorf("known path should validate, got %v", err)
	}
}

func TestPromptTemplate_RenderWithArgs(t *testing.T) {
	tpl := &promptTemplate{
		content: "Task: ${task_id}\nReason: ${reason}\nKind: ${event.kind}",
		args: map[string]string{
			"task_id": "${event.task.id}",
			"reason":  "${event.task.reason}",
		},
	}
	got := tpl.render(trace.Event{TaskID: "T-9", Reason: "timeout", Kind: trace.KindTaskFailed})
	want := "Task: T-9\nReason: timeout\nKind: task_failed"
	if got != want {
		t.Errorf("got=%q\nwant=%q", got, want)
	}
}

func TestValidatePromptTemplate_UnknownArgRef(t *testing.T) {
	err := validatePromptTemplate("${task_id}", map[string]string{
		"task_id": "${event.bogus}",
	})
	if err == nil {
		t.Error("expected error for unknown event field in arg")
	}
}

func TestValidatePromptTemplate_ContentRefMissingArg(t *testing.T) {
	err := validatePromptTemplate("${not_in_args_or_event}", map[string]string{
		"task_id": "${event.task.id}",
	})
	if err == nil {
		t.Error("content ref must be in args or event fields")
	}
}

// ── loader.go ─────────────────────────────────────────────────────────

type fakeStore struct {
	mu    sync.Mutex
	tasks []*model.Task
	err   error
}

func (s *fakeStore) PublishTask(t *model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.tasks = append(s.tasks, t)
	return nil
}

func (s *fakeStore) snapshot() []*model.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*model.Task(nil), s.tasks...)
}

func writePrompt(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	return path
}

func TestLoad_HappyPath_PublishTask(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "investigate.md", "Investigate task ${event.task.id} failed: ${event.task.reason}")

	yamlData := []byte(`
reactors:
  - name: investigate-failure
    on: task_failed
    publish_task:
      kind: explorer
      event_type: investigation
      priority: 5
      description:
        file: ./investigate.md
`)
	store := &fakeStore{}
	rs, err := Load(yamlData, dir, dir, Deps{Store: store})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("len=%d want 1", len(rs))
	}

	r := rs[0]
	if r.Name() != "investigate-failure" {
		t.Errorf("name=%q", r.Name())
	}
	subs := r.Subscribe()
	if len(subs) != 1 || subs[0] != trace.KindTaskFailed {
		t.Errorf("subscribe wrong: %+v", subs)
	}
	if r.IsSync() {
		t.Error("user reactor should be async")
	}

	if err := r.Run(trace.Event{
		Kind: trace.KindTaskFailed, TaskID: "T-77", AgentID: "worker-7",
		BatchID: "batch-3", Reason: "rate limit",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tasks := store.snapshot()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	want := "Investigate task T-77 failed: rate limit"
	if tasks[0].Description != want {
		t.Errorf("desc=%q want=%q", tasks[0].Description, want)
	}
	if tasks[0].ParentTaskID != "T-77" || tasks[0].ReplyToAgentID != "worker-7" || tasks[0].BatchID != "batch-3" {
		t.Fatalf("reactor task routing metadata = %+v", tasks[0])
	}
	if tasks[0].EventType != "investigation" || tasks[0].Priority != 5 {
		t.Errorf("event_type/priority wrong: %+v", tasks[0])
	}
}

func TestLoad_When_Filters(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub ${event.task.id}")

	yamlData := []byte(`
reactors:
  - on: task_failed
    when: ${event.task.retry_count} >= 3
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
	store := &fakeStore{}
	rs, err := Load(yamlData, dir, dir, Deps{Store: store})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// 不命中 when：不投递
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1", AttemptNo: 1}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(store.snapshot()); got != 0 {
		t.Errorf("when=false should skip publish, got %d tasks", got)
	}

	// 命中 when：task_failed 的 retry_count 来自结构化 Transition.RetryCount
	if err := rs[0].Run(trace.Event{
		Kind:       trace.KindTaskFailed,
		TaskID:     "T-2",
		Transition: &trace.Transition{RetryCount: 5},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(store.snapshot()); got != 1 {
		t.Errorf("when=true should publish, got %d tasks", got)
	}
}

func TestLoadWithKindEventTypes_FillsEventTypeFromKind(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")

	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
	store := &fakeStore{}
	rs, err := Load(yamlData, dir, dir, Deps{
		Store: store,
		KindEventTypes: map[string]string{
			"worker":   "",
			"explorer": "explore",
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tasks := store.snapshot()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].EventType != "explore" {
		t.Fatalf("EventType=%q, want explore", tasks[0].EventType)
	}
}

func TestProgramVerifyReactors_RequestPlanReevaluation(t *testing.T) {
	repoRoot := testRepoRoot(t)
	st := newReplanWakeStore()
	rs, err := LoadFromFile(filepath.Join(repoRoot, "reactors.program-verify.yaml"), repoRoot, Deps{
		Store: st,
		KindEventTypes: map[string]string{
			"worker":   "",
			"verifier": "verify",
		},
		AgentKindOf: func(agentID string) string {
			if agentID == "worker-1" {
				return "worker"
			}
			if agentID == "verifier-1" {
				return "verifier"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("len=%d want 1", len(rs))
	}

	if err := rs[0].Run(trace.Event{
		Kind:       trace.KindTaskRetry,
		TaskID:     "worker-task-1",
		AgentID:    "worker-1",
		Transition: &trace.Transition{RetryCount: 2},
	}); err != nil {
		t.Fatalf("Run worker event: %v", err)
	}
	wakes := wakeTasks(t, st)
	if len(wakes) != 1 {
		t.Fatalf("worker 重试压力应发布 1 个 replan 唤醒任务，got %d", len(wakes))
	}
	if !strings.Contains(wakes[0].Description, "reason_code=worker_retry_pressure") ||
		!strings.Contains(wakes[0].Description, "urgency=high") {
		t.Fatalf("唤醒任务描述应含 reason_code/urgency: %q", wakes[0].Description)
	}
	if wakes[0].ParentTaskID != "worker-task-1" {
		t.Fatalf("ParentTaskID = %q, want worker-task-1", wakes[0].ParentTaskID)
	}

	if err := rs[0].Run(trace.Event{
		Kind:       trace.KindTaskRetry,
		TaskID:     "verifier-task-1",
		AgentID:    "verifier-1",
		Transition: &trace.Transition{RetryCount: 2},
	}); err != nil {
		t.Fatalf("Run verifier event: %v", err)
	}
	if got := len(wakeTasks(t, st)); got != 1 {
		t.Fatalf("verifier retry 不应触发 per-kind 过滤外的发布，got %d 个唤醒任务", got)
	}
}

func testRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func TestLoadWithKindEventTypes_RejectsUnknownKind(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: ghost
      description: { file: ./p.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{
		Store:          &fakeStore{},
		KindEventTypes: map[string]string{"worker": ""},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("expected unknown kind error, got %v", err)
	}
}

func TestLoadWithKindEventTypes_RejectsMismatchedEventType(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: explorer
      event_type: wrong
      description: { file: ./p.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{
		Store:          &fakeStore{},
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err == nil || !strings.Contains(err.Error(), "routes to event_type") {
		t.Fatalf("expected event_type mismatch error, got %v", err)
	}
}

func TestLoad_RejectsUnknownEventKind(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")
	yamlData := []byte(`
reactors:
  - on: bogus_event
    publish_task: { kind: x, description: { file: ./p.md } }
`)
	if _, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}}); err == nil {
		t.Error("expected error for unknown event kind")
	}
}

func TestLoad_RejectsMultipleActions(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task: { kind: x, description: { file: ./p.md } }
    invoke_llm: { prompt: { file: ./p.md }, output: {} }
`)
	if _, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}}); err == nil {
		t.Error("expected error for multiple action verbs")
	}
}

func TestLoad_RejectsZeroActions(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
`)
	if _, err := Load(yamlData, ".", "", Deps{Store: &fakeStore{}}); err == nil {
		t.Error("expected error for missing action")
	}
}

func TestLoad_RejectsPromptOutsideRoot(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outside := writePrompt(t, outsideDir, "evil.md", "x")

	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: x
      description: { file: ` + outside + ` }
`)
	_, err := Load(yamlData, rootDir, rootDir, Deps{Store: &fakeStore{}})
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Errorf("expected 'outside project root' error, got %v", err)
	}
}

func TestLoad_RejectsMissingPromptFile(t *testing.T) {
	dir := t.TempDir()
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: x
      description: { file: ./nonexistent.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}})
	if err == nil {
		t.Error("expected error for missing prompt file")
	}
}

func TestLoad_RejectsURLInline(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task:
      kind: x
      description: { url: "http://example.com" }
`)
	_, err := Load(yamlData, ".", "", Deps{Store: &fakeStore{}})
	if err == nil || !strings.Contains(err.Error(), "url not implemented") {
		t.Errorf("expected url unimplemented error, got %v", err)
	}
}

// ── publish_task.go ──────────────────────────────────────────────────

func TestPublishTask_StoreErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x ${event.task.id}")
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task: { kind: x, description: { file: ./p.md } }
`)
	store := &fakeStore{err: errors.New("simulated")}
	rs, err := Load(yamlData, dir, dir, Deps{Store: store})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	if err == nil || !strings.Contains(err.Error(), "simulated") {
		t.Errorf("store error should propagate, got %v", err)
	}
}

func TestPublishTask_NewRunRequiresResolvableSourceTask(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x ${event.task.id}")
	yamlData := []byte(`
reactors:
  - on: task_failed
    publish_task: { kind: x, description: { file: ./p.md } }
`)
	store := &fakeStore{}
	rs, err := Load(yamlData, dir, dir, Deps{Store: store})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T", RunID: "run-new"})
	if err == nil || !strings.Contains(err.Error(), "可解引用 source Task") {
		t.Fatalf("新 Run 缺少 source lookup authority 必须 fail-closed: %v", err)
	}
	if tasks := store.snapshot(); len(tasks) != 0 {
		t.Fatalf("source 丢失时不得发布 Task: %+v", tasks)
	}
}

// spawn_agent 已在 S6 实现，相关测试见 spawn_agent_test.go。
// via_translator 仅在 spawn_agent.initial_task.description 下生效，相关测试见
// spawn_agent_test.go 的 ViaTranslator 用例。
