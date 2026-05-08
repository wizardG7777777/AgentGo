package userdef

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/spawn"
	"agentgo/internal/trace"
)

// fakeSpawnHost 记录 Spawn 调用，配合 SpawnRequest 断言。
type fakeSpawnHost struct {
	mu       sync.Mutex
	requests []spawn.SpawnRequest
	err      error
}

func (h *fakeSpawnHost) Spawn(ctx context.Context, req spawn.SpawnRequest) (string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return "", "", h.err
	}
	h.requests = append(h.requests, req)
	return "spawn-id", "task-id", nil
}

func (h *fakeSpawnHost) snapshot() []spawn.SpawnRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]spawn.SpawnRequest(nil), h.requests...)
}

// ── happy path ─────────────────────────────────────────────────────────

func TestSpawnAgent_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "post_mortem.md", "Investigate ${event.task.id}: ${event.task.reason}")

	yamlData := []byte(`
reactors:
  - name: post-mortem
    on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          file: ./post_mortem.md
      lifecycle: one_shot
`)
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{
		SpawnHost:      host,
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{
		Kind: trace.KindTaskFailed, TaskID: "T-1", Reason: "rate limit",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := host.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(reqs))
	}
	got := reqs[0]
	if got.BaseKind != "explorer" {
		t.Errorf("BaseKind=%q", got.BaseKind)
	}
	if got.Lifecycle != "one_shot" {
		t.Errorf("Lifecycle=%q", got.Lifecycle)
	}
	if got.SourceTaskID != "T-1" {
		t.Errorf("SourceTaskID=%q", got.SourceTaskID)
	}
	if got.Depth != 1 {
		t.Errorf("Depth=%d want 1", got.Depth)
	}
	want := "Investigate T-1: rate limit"
	if got.InitialTaskDescription != want {
		t.Errorf("description=%q want %q", got.InitialTaskDescription, want)
	}
}

func TestSpawnAgent_OverrideFieldsPropagate(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "stub")
	writePrompt(t, dir, "sys.md", "OVERRIDE: ${event.task.id}")

	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      override:
        system_prompt: { file: ./sys.md }
        model: gpt-x
        agent_max_loops: 5
        context_limit: 8000
      initial_task:
        description: { file: ./task.md }
`)
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-99"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := host.snapshot()[0]
	if got.Override.Model != "gpt-x" || got.Override.AgentMaxLoops != 5 || got.Override.ContextLimit != 8000 {
		t.Errorf("override numeric fields wrong: %+v", got.Override)
	}
	if !got.Override.SystemPromptSet {
		t.Error("SystemPromptSet should be true when override.system_prompt provided")
	}
	if got.Override.SystemPrompt != "OVERRIDE: T-99" {
		t.Errorf("system_prompt rendered=%q", got.Override.SystemPrompt)
	}
}

func TestSpawnAgent_When_Filters(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    when: ${event.task.retry_count} >= 3
    spawn_agent:
      base_kind: explorer
      initial_task:
        description: { file: ./task.md }
`)
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 不命中
	rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T", AttemptNo: 1})
	if got := len(host.snapshot()); got != 0 {
		t.Errorf("when=false should skip spawn, got %d", got)
	}
	// 命中
	rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T",
		Transition: &trace.Transition{RetryCount: 5}})
	if got := len(host.snapshot()); got != 1 {
		t.Errorf("when=true should spawn, got %d", got)
	}
}

func TestSpawnAgent_DepthIncrementsFromEvent(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description: { file: ./task.md }
`)
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T", Depth: 3}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := host.snapshot()[0]
	if got.Depth != 4 {
		t.Errorf("Depth=%d want 4", got.Depth)
	}
}

// ── 校验 / 错误路径 ──────────────────────────────────────────────────

func TestSpawnAgent_NoSpawnHost(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description: { file: ./task.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{})
	if err == nil || !strings.Contains(err.Error(), "SpawnHost") {
		t.Errorf("expected SpawnHost-required error, got %v", err)
	}
}

func TestSpawnAgent_RejectsUnknownBaseKind(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: ghost
      initial_task:
        description: { file: ./task.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{
		SpawnHost:      &fakeSpawnHost{},
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown base_kind") {
		t.Errorf("expected unknown base_kind error, got %v", err)
	}
}

func TestSpawnAgent_PersistentLifecycleLoadsButRuntimeFails(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      lifecycle: persistent
      initial_task:
        description: { file: ./task.md }
`)
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host})
	if err != nil {
		t.Fatalf("persistent is a placeholder and should load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed})
	if err == nil || !strings.Contains(err.Error(), "persistent") {
		t.Errorf("expected runtime persistent error, got %v", err)
	}
	if got := len(host.snapshot()); got != 0 {
		t.Errorf("persistent placeholder should fail before SpawnHost call, got %d calls", got)
	}
}

func TestSpawnAgent_RejectsUnknownLifecycleAtLoad(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      lifecycle: forever
      initial_task:
        description: { file: ./task.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}})
	if err == nil || !strings.Contains(err.Error(), "unknown lifecycle") {
		t.Errorf("expected unknown lifecycle error, got %v", err)
	}
}

func TestSpawnAgent_RejectsNegativeOverride(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      override:
        agent_max_loops: -1
      initial_task:
        description: { file: ./task.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}})
	if err == nil || !strings.Contains(err.Error(), "agent_max_loops") {
		t.Errorf("expected negative override error, got %v", err)
	}
}

// ── S7 via_translator ────────────────────────────────────────────────

func TestSpawnAgent_ViaTranslator_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "Reword for sub-agent: task=${event.task.id} reason=${event.task.reason}")

	yamlData := []byte(`
reactors:
  - name: post-mortem-via-tx
    on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt:
              file: ./tx.md
      lifecycle: one_shot
`)
	llm := &fakeLLM{response: "Translated: please investigate task T-7."}
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host, LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-7", Reason: "rate limit"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 1) translator prompt 经过模板替换被喂给 LLM
	want := "Reword for sub-agent: task=T-7 reason=rate limit"
	if got := llm.lastPrompt(); got != want {
		t.Errorf("translator prompt to LLM=%q\nwant=%q", got, want)
	}
	// 2) Spawn 收到的 description 是 LLM 输出
	reqs := host.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(reqs))
	}
	if reqs[0].InitialTaskDescription != "Translated: please investigate task T-7." {
		t.Errorf("description=%q", reqs[0].InitialTaskDescription)
	}
}

func TestSpawnAgent_ViaTranslator_LLMFactoryPath(t *testing.T) {
	// 与 invoke_llm 一致：Deps.LLMFactory("") 也能解析出 LLMCompleter
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt: { file: ./tx.md }
`)
	llm := &fakeLLM{response: "ok"}
	factory := &recordingLLMFactory{llm: llm}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}, LLMFactory: factory.build})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// factory 应该被以空 model 调用一次（via_translator 不暴露 model 字段）
	got := factory.snapshot()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("factory called with %v, want exactly one [\"\"]", got)
	}
}

func TestSpawnAgent_ViaTranslator_NoLLMRejected(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt: { file: ./tx.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}})
	if err == nil || !strings.Contains(err.Error(), "LLM") {
		t.Errorf("expected LLM-required error, got %v", err)
	}
}

func TestSpawnAgent_ViaTranslator_RejectsConflictingFile(t *testing.T) {
	// description 同时给 file + via_translator 应启动期报错
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          file: ./task.md
          via_translator:
            translator_prompt: { file: ./tx.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}, LLM: &fakeLLM{}})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got %v", err)
	}
}

func TestSpawnAgent_ViaTranslator_RejectsOuterArgs(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          args:
            task_id: ${event.task.id}
          via_translator:
            translator_prompt: { file: ./tx.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}, LLM: &fakeLLM{}})
	if err == nil || !strings.Contains(err.Error(), "outer args") {
		t.Errorf("expected outer-args error, got %v", err)
	}
}

func TestSpawnAgent_ViaTranslator_RejectsNesting(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt:
              via_translator:
                translator_prompt: { file: ./tx.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}, LLM: &fakeLLM{}})
	if err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Errorf("expected nesting-not-supported error, got %v", err)
	}
}

func TestSpawnAgent_ViaTranslator_LLMErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt: { file: ./tx.md }
`)
	llm := &fakeLLM{err: errors.New("simulated translator failure")}
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host, LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed})
	if err == nil || !strings.Contains(err.Error(), "simulated translator failure") {
		t.Errorf("expected translator failure to propagate, got %v", err)
	}
	if got := len(host.snapshot()); got != 0 {
		t.Errorf("LLM failure should abort spawn, got %d hosts called", got)
	}
}

func TestSpawnAgent_ViaTranslator_EmptyLLMOutputRejected(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt: { file: ./tx.md }
`)
	llm := &fakeLLM{response: ""}
	host := &fakeSpawnHost{}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host, LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-output error, got %v", err)
	}
	if got := len(host.snapshot()); got != 0 {
		t.Errorf("empty translator output should abort spawn, got %d hosts called", got)
	}
}

func TestSpawnAgent_HostErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description: { file: ./task.md }
`)
	host := &fakeSpawnHost{err: errors.New("simulated host failure")}
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: host})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	if err == nil || !strings.Contains(err.Error(), "simulated host failure") {
		t.Errorf("expected host error to propagate, got %v", err)
	}
}

func TestSpawnAgent_BasicSubscribe(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "task.md", "x")
	yamlData := []byte(`
reactors:
  - name: my-spawn
    on: task_completed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description: { file: ./task.md }
`)
	rs, err := Load(yamlData, dir, dir, Deps{SpawnHost: &fakeSpawnHost{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := rs[0]
	if r.Name() != "my-spawn" {
		t.Errorf("Name=%q", r.Name())
	}
	if r.IsSync() {
		t.Error("user spawn_agent reactor should be async")
	}
	subs := r.Subscribe()
	if len(subs) != 1 || subs[0] != trace.KindTaskCompleted {
		t.Errorf("subscribe wrong: %v", subs)
	}
}
