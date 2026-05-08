package userdef

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/trace"
)

// ── 测试桩 ─────────────────────────────────────────────────────────────

type fakeLLM struct {
	mu       sync.Mutex
	prompts  []string
	response string
	err      error
}

type recordingLLMFactory struct {
	mu     sync.Mutex
	models []string
	llm    LLMCompleter
}

func (f *recordingLLMFactory) build(model string) LLMCompleter {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append(f.models, model)
	return f.llm
}

func (f *recordingLLMFactory) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models...)
}

func (f *fakeLLM) Complete(ctx context.Context, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func (f *fakeLLM) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

type fakeMailbox struct {
	mu   sync.Mutex
	sent []mailbox.Message
	err  error
}

func (m *fakeMailbox) Send(msg mailbox.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *fakeMailbox) snapshot() []mailbox.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mailbox.Message(nil), m.sent...)
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []trace.Event
}

func (e *fakeEmitter) Emit(ev trace.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *fakeEmitter) snapshot() []trace.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]trace.Event(nil), e.events...)
}

// ── write_file sink ───────────────────────────────────────────────────

func TestInvokeLLM_WriteFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "summarize.md", "Summarize task ${event.task.id}")

	yamlData := []byte(`
reactors:
  - name: write-summary
    on: task_failed
    invoke_llm:
      prompt:
        file: ./summarize.md
      output:
        write_file:
          path: ./logs/summary-${event.task.id}.md
`)
	llm := &fakeLLM{response: "the summary text"}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-42"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(llm.lastPrompt(), "T-42") {
		t.Errorf("LLM prompt should contain rendered task id, got %q", llm.lastPrompt())
	}

	written, err := os.ReadFile(filepath.Join(dir, "logs", "summary-T-42.md"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(written) != "the summary text" {
		t.Errorf("file content=%q want %q", string(written), "the summary text")
	}
}

func TestInvokeLLM_WriteFile_ShortFormString(t *testing.T) {
	// 验证 write_file 短形式字符串：write_file: ./logs/x.md
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        write_file: ./out.md
`)
	llm := &fakeLLM{response: "y"}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.md")); err != nil {
		t.Errorf("expected out.md to exist: %v", err)
	}
}

func TestInvokeLLM_WriteFile_RejectsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        write_file: /tmp/agentgo-evil-${event.task.id}.md
`)
	llm := &fakeLLM{response: "y"}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Errorf("expected outside-root error, got %v", err)
	}
}

func TestInvokeLLM_WriteFile_RelativePathUsesProjectRoot(t *testing.T) {
	dir := t.TempDir()
	otherCWD := t.TempDir()
	writePrompt(t, dir, "p.md", "x")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        write_file: ./logs/out.md
`)
	rs, err := Load(yamlData, dir, dir, Deps{LLM: &fakeLLM{response: "content"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(otherCWD); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "out.md")); err != nil {
		t.Fatalf("expected file under project root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherCWD, "logs", "out.md")); !os.IsNotExist(err) {
		t.Fatalf("relative write_file should not use cwd, stat err=%v", err)
	}
}

func TestInvokeLLM_WriteFile_RejectsOutsideRootBeforeMkdir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writePrompt(t, root, "p.md", "x")
	outsideChild := filepath.Join(outside, "should-not-exist", "out.md")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        write_file: ` + outsideChild + `
`)
	rs, err := Load(yamlData, root, root, Deps{LLM: &fakeLLM{response: "y"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed})
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Fatalf("expected outside-root error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("outside directory should not be created before confinement check, stat err=%v", err)
	}
}

// ── send_message sink ─────────────────────────────────────────────────

func TestInvokeLLM_SendMessage_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "ask about ${event.task.id}")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        send_message:
          to: admin
          type: info
          priority: high
`)
	llm := &fakeLLM{response: "please look at this failure"}
	mbox := &fakeMailbox{}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm, Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-9"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sent := mbox.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if sent[0].To != "admin" {
		t.Errorf("To=%q want admin", sent[0].To)
	}
	if sent[0].Content != "please look at this failure" {
		t.Errorf("Content=%q want LLM output", sent[0].Content)
	}
	if sent[0].Type != "info" || sent[0].Priority != "high" {
		t.Errorf("type/priority wrong: %+v", sent[0])
	}
}

func TestInvokeLLM_SendMessage_ToTemplated(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        send_message:
          to: ${event.agent.id}
`)
	llm := &fakeLLM{response: "hi"}
	mbox := &fakeMailbox{}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm, Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T", AgentID: "worker-3"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := mbox.snapshot()[0].To; got != "worker-3" {
		t.Errorf("To=%q want worker-3", got)
	}
}

func TestInvokeLLM_SendMessage_DefaultsTypeAndPriority(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")

	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        send_message: { to: admin }
`)
	llm := &fakeLLM{response: "hi"}
	mbox := &fakeMailbox{}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm, Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	got := mbox.snapshot()[0]
	if got.Type != "info" || got.Priority != "normal" {
		t.Errorf("defaults wrong: type=%q priority=%q", got.Type, got.Priority)
	}
}

func TestInvokeLLM_SendMessage_NoMailboxDep(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        send_message: { to: admin }
`)
	_, err := Load(yamlData, dir, dir, Deps{LLM: &fakeLLM{response: "x"}})
	if err == nil || !strings.Contains(err.Error(), "Mailbox") {
		t.Errorf("expected Mailbox-required error, got %v", err)
	}
}

func TestInvokeLLM_SendMessage_RejectsBadContentVar(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        send_message:
          to: admin
          content_var: prompt
`)
	_, err := Load(yamlData, dir, dir, Deps{LLM: &fakeLLM{response: "x"}, Mailbox: &fakeMailbox{}})
	if err == nil || !strings.Contains(err.Error(), "content_var") {
		t.Errorf("expected content_var error, got %v", err)
	}
}

// ── emit_trace sink ───────────────────────────────────────────────────

func TestInvokeLLM_EmitTrace_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        emit_trace:
          kind: failure_summary
`)
	llm := &fakeLLM{response: "summary text"}
	emitter := &fakeEmitter{}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm, Emitter: emitter})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1", AgentID: "worker-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := emitter.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(events))
	}
	got := events[0]
	if got.Kind != trace.EventKind("failure_summary") {
		t.Errorf("Kind=%q want failure_summary", got.Kind)
	}
	if got.TaskID != "T-1" || got.AgentID != "worker-1" {
		t.Errorf("TaskID/AgentID wrong: %+v", got)
	}
	if got.Description != "summary text" {
		t.Errorf("Description=%q want LLM output", got.Description)
	}
}

// ── 校验 / 错误路径 ──────────────────────────────────────────────────

func TestInvokeLLM_NoLLMDep(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        emit_trace: { kind: x }
`)
	_, err := Load(yamlData, dir, dir, Deps{})
	if err == nil || !strings.Contains(err.Error(), "LLM") {
		t.Errorf("expected LLM-required error, got %v", err)
	}
}

func TestInvokeLLM_ModelUsesFactory(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      model: qwen3.6-flash
      prompt: { file: ./p.md }
      output:
        emit_trace: { kind: x }
`)
	factory := &recordingLLMFactory{llm: &fakeLLM{response: "ok"}}
	rs, err := Load(yamlData, dir, dir, Deps{
		LLMFactory: factory.build,
		Emitter:    &fakeEmitter{},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := factory.snapshot(); len(got) != 1 || got[0] != "qwen3.6-flash" {
		t.Fatalf("factory models=%v, want [qwen3.6-flash]", got)
	}
	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestInvokeLLM_ModelWithoutFactoryRejected(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      model: qwen3.6-flash
      prompt: { file: ./p.md }
      output:
        emit_trace: { kind: x }
`)
	_, err := Load(yamlData, dir, dir, Deps{LLM: &fakeLLM{response: "ok"}})
	if err == nil || !strings.Contains(err.Error(), "LLMFactory") {
		t.Fatalf("expected LLMFactory-required error, got %v", err)
	}
}

func TestInvokeLLM_RejectsZeroSinks(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output: {}
`)
	_, err := Load(yamlData, dir, dir, Deps{LLM: &fakeLLM{response: "x"}})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected exactly-one-sink error, got %v", err)
	}
}

func TestInvokeLLM_RejectsMultipleSinks(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        write_file: ./out.md
        emit_trace: { kind: x }
`)
	_, err := Load(yamlData, dir, dir, Deps{LLM: &fakeLLM{response: "x"}})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected exactly-one-sink error, got %v", err)
	}
}

func TestInvokeLLM_LLMErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        emit_trace: { kind: x }
`)
	llm := &fakeLLM{err: errors.New("simulated LLM down")}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	if err == nil || !strings.Contains(err.Error(), "simulated LLM down") {
		t.Errorf("expected LLM error to propagate, got %v", err)
	}
}

func TestInvokeLLM_When_Filters(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    when: ${event.task.retry_count} >= 3
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        emit_trace: { kind: failure_summary }
`)
	llm := &fakeLLM{response: "summary"}
	emitter := &fakeEmitter{}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm, Emitter: emitter})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 不命中 when：不应调 LLM
	rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T", AttemptNo: 1})
	if got := llm.lastPrompt(); got != "" {
		t.Errorf("when=false should skip LLM, got prompt %q", got)
	}
	// 命中
	rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T",
		Transition: &trace.Transition{RetryCount: 5}})
	if got := llm.lastPrompt(); got == "" {
		t.Error("when=true should invoke LLM")
	}
	if len(emitter.snapshot()) != 1 {
		t.Errorf("expected 1 emit, got %d", len(emitter.snapshot()))
	}
}

func TestInvokeLLM_LLMTimeout_PropagatesContextDeadline(t *testing.T) {
	// LLMTimeout 走到 LLMCompleter，验证 context 超时被传递
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    invoke_llm:
      prompt: { file: ./p.md }
      output:
        emit_trace: { kind: x }
`)
	slowLLM := &slowFakeLLM{delay: 200 * time.Millisecond}
	rs, err := Load(yamlData, dir, dir, Deps{LLM: slowLLM, LLMTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("expected context deadline error, got %v", err)
	}
}

type slowFakeLLM struct {
	delay time.Duration
}

func (s *slowFakeLLM) Complete(ctx context.Context, prompt string) (string, error) {
	select {
	case <-time.After(s.delay):
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
