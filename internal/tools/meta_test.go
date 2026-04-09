package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func mkCall(name string, args map[string]any) llm.ToolCall {
	return llm.ToolCall{ID: "t", Name: name, Arguments: args}
}

// ---- fakes ----

type fakeStore struct {
	tasks       map[string]*model.Task
	createCalls []*model.Task
	nextID      int
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: make(map[string]*model.Task)}
}

func (f *fakeStore) PublishTask(task *model.Task) error {
	f.nextID++
	task.ID = fmt.Sprintf("task-%d", f.nextID)
	task.Status = model.TaskStatusPending
	f.tasks[task.ID] = task
	f.createCalls = append(f.createCalls, task)
	return nil
}

func (f *fakeStore) ClaimTask(agentID string, taskID string) error     { return nil }
func (f *fakeStore) SubmitResult(agentID, taskID, result string) error { return nil }
func (f *fakeStore) TransitionState(taskID string, from, to model.TaskStatus) error {
	return nil
}
func (f *fakeStore) FailTask(agentID, taskID, reason string) error { return nil }
func (f *fakeStore) FailTaskBySystem(taskID, reason string) error  { return nil }
func (f *fakeStore) RetryRollback(agentID, taskID, reason string) error {
	return nil
}
func (f *fakeStore) AppendOutput(agentID, taskID, chunk string) error { return nil }

func (f *fakeStore) QueryAvailable(eventType string) ([]*model.Task, error) {
	return nil, nil
}
func (f *fakeStore) GetTask(taskID string) (*model.Task, error) {
	t, ok := f.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("not found: %s", taskID)
	}
	return t, nil
}
func (f *fakeStore) GetDependencyResults(taskID string) (map[string]string, error) {
	return nil, nil
}
func (f *fakeStore) GetDependencyArtifacts(taskID string) (map[string][]string, error) {
	return nil, nil
}
func (f *fakeStore) AppendArtifact(taskID string, path string) error        { return nil }
func (f *fakeStore) RecordLastResponse(taskID string, content string) error { return nil }
func (f *fakeStore) ScanAll() ([]*model.Task, error)                        { return nil, nil }
func (f *fakeStore) AppendToolCall(string, store.ToolCallRecord) error      { return nil }
func (f *fakeStore) QueryToolCalls(string, string) ([]store.ToolCallRecord, error) {
	return nil, nil
}

type fakeHolder struct{ id string }

func (f *fakeHolder) Get() string { return f.id }

// ---- Register counting tests ----

func TestMetaGroup_Register_BothTools(t *testing.T) {
	reg := agent.NewToolRegistry()
	MetaGroup{
		Store:      newFakeStore(),
		MBRegistry: mailbox.NewRegistry(4),
		AgentID:    "a1",
	}.Register(reg)
	if got := len(reg.Defs()); got != 2 {
		t.Fatalf("expected 2 tools, got %d", got)
	}
}

func TestMetaGroup_Register_OnlyMailbox(t *testing.T) {
	reg := agent.NewToolRegistry()
	MetaGroup{
		MBRegistry: mailbox.NewRegistry(4),
		AgentID:    "a1",
	}.Register(reg)
	if got := len(reg.Defs()); got != 1 {
		t.Fatalf("expected 1 tool, got %d", got)
	}
	if reg.Defs()[0].Name != "send_message" {
		t.Fatalf("expected send_message, got %s", reg.Defs()[0].Name)
	}
}

func TestMetaGroup_Register_OnlyStore(t *testing.T) {
	reg := agent.NewToolRegistry()
	MetaGroup{Store: newFakeStore()}.Register(reg)
	if got := len(reg.Defs()); got != 1 {
		t.Fatalf("expected 1 tool, got %d", got)
	}
	if reg.Defs()[0].Name != "publish_task" {
		t.Fatalf("expected publish_task, got %s", reg.Defs()[0].Name)
	}
}

func TestMetaGroup_Register_NeitherDep(t *testing.T) {
	reg := agent.NewToolRegistry()
	MetaGroup{}.Register(reg)
	if got := len(reg.Defs()); got != 0 {
		t.Fatalf("expected 0 tools, got %d", got)
	}
}

// ---- publish_task behavior ----

func TestPublishTask_SchedulerMode_NoDepthLimit(t *testing.T) {
	s := newFakeStore()
	g := MetaGroup{Store: s, Holder: nil, MaxDepth: 3}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	for i := 0; i < 100; i++ {
		out, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
			"description": fmt.Sprintf("t%d", i),
		}))
		if err != nil {
			t.Fatalf("publish #%d failed: %v", i, err)
		}
		if !strings.Contains(out, "depth=0") {
			t.Fatalf("expected depth=0, got %q", out)
		}
	}
	for _, task := range s.createCalls {
		if task.Depth != 0 {
			t.Fatalf("scheduler mode should always produce depth=0, got %d", task.Depth)
		}
	}
}

func TestPublishTask_WorkerMode_DepthIncrement(t *testing.T) {
	s := newFakeStore()
	parent := &model.Task{ID: "parent", Depth: 1, Status: model.TaskStatusProcessing}
	s.tasks[parent.ID] = parent

	g := MetaGroup{Store: s, Holder: &fakeHolder{id: "parent"}, MaxDepth: 3}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "child",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(s.createCalls) != 1 || s.createCalls[0].Depth != 2 {
		t.Fatalf("expected child depth=2, got %+v", s.createCalls)
	}
}

func TestPublishTask_WorkerMode_DepthLimitExceeded(t *testing.T) {
	s := newFakeStore()
	parent := &model.Task{ID: "p", Depth: 3}
	s.tasks[parent.ID] = parent

	g := MetaGroup{Store: s, Holder: &fakeHolder{id: "p"}, MaxDepth: 3}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "too-deep",
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "已达到最大子任务深度") {
		t.Fatalf("error text mismatch: %v", err)
	}
}

func TestPublishTask_WorkerMode_AtBoundary(t *testing.T) {
	// parent depth=2, MaxDepth=3 → child depth=3. worker.go uses `childDepth > maxDepth`,
	// so depth=3 must be ALLOWED.
	s := newFakeStore()
	parent := &model.Task{ID: "p", Depth: 2}
	s.tasks[parent.ID] = parent

	g := MetaGroup{Store: s, Holder: &fakeHolder{id: "p"}, MaxDepth: 3}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "edge",
	}))
	if err != nil {
		t.Fatalf("boundary depth should be allowed, got err: %v", err)
	}
	if len(s.createCalls) != 1 || s.createCalls[0].Depth != 3 {
		t.Fatalf("expected child depth=3, got %+v", s.createCalls)
	}
}

func TestPublishTask_MissingDescription(t *testing.T) {
	s := newFakeStore()
	g := MetaGroup{Store: s}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("expected missing description error, got %v", err)
	}
}

func TestPublishTask_NoCurrentTask_WorkerMode(t *testing.T) {
	s := newFakeStore()
	g := MetaGroup{Store: s, Holder: &fakeHolder{id: ""}, MaxDepth: 3}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "无法获取当前任务上下文") {
		t.Fatalf("expected 'no current task' error, got %v", err)
	}
}

// ---- send_message behavior ----

func TestSendMessage_Basic(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	senderBox := mbReg.Register("sender", "")
	receiverBox := mbReg.Register("receiver", "")
	_ = senderBox

	g := MetaGroup{MBRegistry: mbReg, AgentID: "sender"}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":       "receiver",
		"content":  "hi",
		"msg_type": "info",
	}))
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}

	msgs := receiverBox.Drain()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "hi" || msgs[0].From != "sender" || msgs[0].Type != "info" {
		t.Fatalf("unexpected message: %+v", msgs[0])
	}
}

func TestSendMessage_Broadcast(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	boxA := mbReg.Register("a", "")
	boxB := mbReg.Register("b", "")

	g := MetaGroup{MBRegistry: mbReg, AgentID: "sender"}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	out, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "*",
		"content": "broadcast",
	}))
	if err != nil {
		t.Fatalf("broadcast failed: %v", err)
	}
	if !strings.Contains(out, "广播") {
		t.Fatalf("expected broadcast response, got %q", out)
	}

	ma := boxA.Drain()
	mb := boxB.Drain()
	if len(ma) != 1 || len(mb) != 1 {
		t.Fatalf("expected each peer to receive 1 message, got a=%d b=%d", len(ma), len(mb))
	}
}

func TestSendMessage_DefaultMsgType(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("r", "")

	g := MetaGroup{MBRegistry: mbReg, AgentID: "sender"}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "r",
		"content": "hello",
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := recvBox.Drain()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Type != mailbox.MsgTypeInfo {
		t.Fatalf("expected default type=info, got %q", msgs[0].Type)
	}
	if msgs[0].Priority != mailbox.PriorityNormal {
		t.Fatalf("expected default priority=normal, got %q", msgs[0].Priority)
	}
}
