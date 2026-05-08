package userdef

import (
	"strings"
	"testing"

	"agentgo/internal/trace"
)

// ── §6.1 call: send_message ──────────────────────────────────────────

func TestCall_SendMessage_HappyPath(t *testing.T) {
	yamlData := []byte(`
reactors:
  - name: notify-on-failure
    on: task_failed
    when: ${event.task.retry_count} >= 3
    call: send_message
    args:
      to: admin
      content: "终态失败：${event.task.id} (retry=${event.task.retry_count})"
      type: info
      priority: high
`)
	mbox := &fakeMailbox{}
	rs, err := Load(yamlData, ".", "", Deps{Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("len=%d", len(rs))
	}
	r := rs[0]
	if r.Name() != "notify-on-failure" || r.IsSync() {
		t.Errorf("metadata wrong: name=%q sync=%v", r.Name(), r.IsSync())
	}

	// 不命中 when：不发
	if err := r.Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1", AttemptNo: 1}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(mbox.snapshot()); got != 0 {
		t.Errorf("when=false should not send, got %d", got)
	}

	// 命中 when：发出消息
	if err := r.Run(trace.Event{
		Kind: trace.KindTaskFailed, TaskID: "T-2",
		Transition: &trace.Transition{RetryCount: 5},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sent := mbox.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	got := sent[0]
	if got.To != "admin" {
		t.Errorf("To=%q", got.To)
	}
	if got.Content != "终态失败：T-2 (retry=5)" {
		t.Errorf("Content=%q", got.Content)
	}
	if got.Type != "info" || got.Priority != "high" {
		t.Errorf("type/priority wrong: %+v", got)
	}
}

func TestCall_SendMessage_DefaultsTypeAndPriority(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: send_message
    args: { to: admin, content: "x" }
`)
	mbox := &fakeMailbox{}
	rs, err := Load(yamlData, ".", "", Deps{Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	got := mbox.snapshot()[0]
	if got.Type != "info" || got.Priority != "normal" {
		t.Errorf("defaults wrong: type=%q priority=%q", got.Type, got.Priority)
	}
}

func TestCall_SendMessage_RendersTypeAndPriorityTemplates(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: send_message
    args:
      to: admin
      content: "x"
      type: "${event.task.event_type}"
      priority: "${event.task.priority}"
`)
	mbox := &fakeMailbox{}
	rs, err := Load(yamlData, ".", "", Deps{Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := rs[0].Run(trace.Event{
		Kind:      trace.KindTaskFailed,
		TaskID:    "T",
		EventType: "question",
		Priority:  "high",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := mbox.snapshot()[0]
	if got.Type != "question" || got.Priority != "high" {
		t.Errorf("type/priority templates not rendered: type=%q priority=%q", got.Type, got.Priority)
	}
}

func TestCall_RejectsUnknownTool(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: read_file
    args: { path: /etc/passwd }
`)
	_, err := Load(yamlData, ".", "", Deps{Mailbox: &fakeMailbox{}})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected unsupported-tool error, got %v", err)
	}
}

func TestCall_RejectsMissingRequiredArgs(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing to",
			yaml: `reactors: [{on: task_failed, call: send_message, args: {content: x}}]`,
			want: "args.to",
		},
		{
			name: "missing content",
			yaml: `reactors: [{on: task_failed, call: send_message, args: {to: a}}]`,
			want: "args.content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.yaml), ".", "", Deps{Mailbox: &fakeMailbox{}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestCall_RejectsMissingMailbox(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: send_message
    args: { to: a, content: b }
`)
	_, err := Load(yamlData, ".", "", Deps{})
	if err == nil || !strings.Contains(err.Error(), "Mailbox") {
		t.Errorf("expected Mailbox-required error, got %v", err)
	}
}

func TestCall_MultipleActionsRejected(t *testing.T) {
	// call 与其他动作互斥
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: send_message
    args: { to: a, content: b }
    publish_task: { kind: worker, description: { file: ./p.md } }
`)
	_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}, Mailbox: &fakeMailbox{}})
	if err == nil || !strings.Contains(err.Error(), "exactly one action") {
		t.Errorf("expected exactly-one-action error, got %v", err)
	}
}

func TestCall_TemplatePathValidation(t *testing.T) {
	// args.to / content 内 ${event.x} 必须命中已知字段
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: send_message
    args: { to: "${event.bogus.field}", content: x }
`)
	_, err := Load(yamlData, ".", "", Deps{Mailbox: &fakeMailbox{}})
	if err == nil || !strings.Contains(err.Error(), "unknown variable") {
		t.Errorf("expected unknown-variable error, got %v", err)
	}
}

func TestCall_TemplatePathValidationForOptionalArgs(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    call: send_message
    args: { to: a, content: b, priority: "${event.bogus.field}" }
`)
	_, err := Load(yamlData, ".", "", Deps{Mailbox: &fakeMailbox{}})
	if err == nil || !strings.Contains(err.Error(), "unknown variable") {
		t.Errorf("expected unknown-variable error, got %v", err)
	}
}

func TestCall_RejectsArgsWithoutCall(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "x")
	yamlData := []byte(`
reactors:
  - on: task_failed
    args: { to: admin, content: x }
    publish_task: { kind: worker, description: { file: ./p.md } }
`)
	_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}})
	if err == nil || !strings.Contains(err.Error(), "args is only valid with call") {
		t.Errorf("expected args-without-call error, got %v", err)
	}
}

// ── §6.2 per-kind 过滤 ─────────────────────────────────────────────────

func TestPerKind_FiltersBySourceAgentKind(t *testing.T) {
	yamlData := []byte(`
reactors:
  - name: worker-only-followup
    on: task_completed
    kind: worker
    call: send_message
    args: { to: admin, content: "worker done: ${event.task.id}" }
`)
	mbox := &fakeMailbox{}
	rs, err := Load([]byte(yamlData), ".", "", Deps{
		Mailbox:        mbox,
		KindEventTypes: map[string]string{"worker": "", "explorer": "explore"},
		AgentKindOf: func(agentID string) string {
			switch agentID {
			case "worker-1":
				return "worker"
			case "explorer-1":
				return "explorer"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := rs[0]

	// worker 触发 → 命中
	r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-1", AgentID: "worker-1"})
	if got := len(mbox.snapshot()); got != 1 {
		t.Errorf("worker event should match per-kind filter, got %d sent", got)
	}

	// explorer 触发 → 不命中
	r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-2", AgentID: "explorer-1"})
	if got := len(mbox.snapshot()); got != 1 {
		t.Errorf("explorer event should NOT match worker-only filter, got %d sent total", got)
	}

	// AgentID="" 触发 → 不命中
	r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-3"})
	if got := len(mbox.snapshot()); got != 1 {
		t.Errorf("event with empty AgentID should NOT match, got %d sent total", got)
	}
}

func TestPerKind_RejectsUnknownKind(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    kind: ghost
    call: send_message
    args: { to: a, content: b }
`)
	_, err := Load(yamlData, ".", "", Deps{
		Mailbox:        &fakeMailbox{},
		KindEventTypes: map[string]string{"worker": ""},
		AgentKindOf:    func(string) string { return "" },
	})
	if err == nil || !strings.Contains(err.Error(), "unknown reactor kind") {
		t.Errorf("expected unknown-kind error, got %v", err)
	}
}

func TestPerKind_RequiresAgentKindOf(t *testing.T) {
	yamlData := []byte(`
reactors:
  - on: task_failed
    kind: worker
    call: send_message
    args: { to: a, content: b }
`)
	_, err := Load(yamlData, ".", "", Deps{
		Mailbox:        &fakeMailbox{},
		KindEventTypes: map[string]string{"worker": ""},
		// AgentKindOf nil
	})
	if err == nil || !strings.Contains(err.Error(), "AgentKindOf") {
		t.Errorf("expected AgentKindOf-required error, got %v", err)
	}
}

func TestPerKind_GlobalReactorMatchesAllAgents(t *testing.T) {
	// 无 kind 字段的 reactor 是全局——任何 agent 触发都命中
	yamlData := []byte(`
reactors:
  - on: task_completed
    call: send_message
    args: { to: admin, content: "any: ${event.task.id}" }
`)
	mbox := &fakeMailbox{}
	rs, err := Load([]byte(yamlData), ".", "", Deps{Mailbox: mbox})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rs[0].Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-1", AgentID: "worker-1"})
	rs[0].Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-2", AgentID: "explorer-1"})
	rs[0].Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-3"}) // empty AgentID
	if got := len(mbox.snapshot()); got != 3 {
		t.Errorf("global reactor should match all 3, got %d", got)
	}
}

func TestPerKind_PreservesInnerMetadata(t *testing.T) {
	// kindFilteredReactor 应透传 inner 的 Subscribe / Name / Priority / IsSync
	yamlData := []byte(`
reactors:
  - name: my-call
    on: task_failed
    kind: worker
    call: send_message
    args: { to: a, content: b }
`)
	rs, err := Load(yamlData, ".", "", Deps{
		Mailbox:        &fakeMailbox{},
		KindEventTypes: map[string]string{"worker": ""},
		AgentKindOf:    func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := rs[0]
	if r.Name() != "my-call" {
		t.Errorf("Name=%q want my-call (inner name preserved)", r.Name())
	}
	if r.IsSync() {
		t.Error("should be async (inner is async)")
	}
	subs := r.Subscribe()
	if len(subs) != 1 || subs[0] != trace.KindTaskFailed {
		t.Errorf("Subscribe wrong: %v", subs)
	}
}
