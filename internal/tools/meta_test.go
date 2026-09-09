package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
func (f *fakeStore) RecordLastHistory(taskID string, history []byte) error {
	return nil
}

func (f *fakeStore) QueryAvailable(eventType, agentID string) ([]*model.Task, error) {
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
func (f *fakeStore) AppendSchedulerBatch(taskID, childTaskID string) error  { return nil }
func (f *fakeStore) ClearSchedulerBatch(taskID string) error                { return nil }
func (f *fakeStore) ScanAll() ([]*model.Task, error)                        { return nil, nil }
func (f *fakeStore) AppendToolCall(string, store.ToolCallRecord) error      { return nil }
func (f *fakeStore) QueryToolCalls(string, string) ([]store.ToolCallRecord, error) {
	return nil, nil
}

type fakeHolder struct{ id string }

func (f *fakeHolder) Get() string { return f.id }

// fakeRouteValidator 模拟运行时路由权威：routes 是 event_type → 保证工具的
// 映射；ownerScopes 是 event_type → 命名空间化归属 scope ID 的映射，空串表示
// 全局路由。CanRouteForPlan 第一参数是发布方的 task:/graph: scope；Worker
// 发布时传 ""。
type fakeRouteValidator struct {
	routes      map[string][]string
	envelopes   map[string][]string
	ownerScopes map[string]string
}

func (f fakeRouteValidator) CanRouteForPlan(ownerScopeID, eventType string, required ...string) bool {
	have, ok := f.routes[eventType]
	if !ok {
		return false
	}
	if owner := f.ownerScopes[eventType]; owner != "" && owner != ownerScopeID {
		return false
	}
	set := make(map[string]bool, len(have))
	for _, tool := range have {
		set[tool] = true
	}
	for _, tool := range required {
		if !set[tool] {
			return false
		}
	}
	return true
}

func (f fakeRouteValidator) RouteCapabilitiesForPlan(ownerScopeID, eventType string) ([]string, bool) {
	if !f.CanRouteForPlan(ownerScopeID, eventType) {
		return nil, false
	}
	return append([]string(nil), f.routes[eventType]...), true
}

func (f fakeRouteValidator) RouteCapabilityEnvelopeForPlan(ownerScopeID, eventType string) ([]string, bool) {
	if !f.CanRouteForPlan(ownerScopeID, eventType) {
		return nil, false
	}
	if tools, ok := f.envelopes[eventType]; ok {
		return append([]string(nil), tools...), true
	}
	return append([]string(nil), f.routes[eventType]...), true
}

// ---- Register counting tests ----

func TestCommunicationGroup_Register_BothTools(t *testing.T) {
	reg := agent.NewToolRegistry()
	CommunicationGroup{
		Store:      newFakeStore(),
		MBRegistry: mailbox.NewRegistry(4),
		AgentID:    "a1",
	}.Register(reg)
	if got := len(reg.Defs()); got != 1 {
		t.Fatalf("只应注册信息工具, got %d", got)
	}
}

func TestCommunicationGroup_Register_OnlyMailbox(t *testing.T) {
	reg := agent.NewToolRegistry()
	CommunicationGroup{
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

func TestCommunicationGroup_Register_NeitherDep(t *testing.T) {
	reg := agent.NewToolRegistry()
	CommunicationGroup{}.Register(reg)
	if got := len(reg.Defs()); got != 0 {
		t.Fatalf("expected 0 tools, got %d", got)
	}
}

// ---- publish_task behavior ----

// 动态 Team 路由按命名空间化归属 scope 绑定：别的 scope 拥有的 team 路由
// 对当前请求不可见；本 scope 拥有的路由与全局静态路由照常可用。

// ---- S1: BatchTracker integration ----

// recordingBatchTracker captures every AppendBatch call.
type recordingBatchTracker struct {
	mu      sync.Mutex
	calls   []string
	failNth int // 0 = never fail; n>0 = fail on n-th call (1-indexed)
}

func (r *recordingBatchTracker) AppendBatch(childTaskID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, childTaskID)
	if r.failNth > 0 && len(r.calls) == r.failNth {
		return fmt.Errorf("simulated tracker failure")
	}
	return nil
}

// ---- send_message behavior ----

func TestSendMessage_Basic(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	senderBox := mbReg.Register("sender", "")
	receiverBox := mbReg.Register("receiver", "")
	_ = senderBox

	g := CommunicationGroup{MBRegistry: mbReg, AgentID: "sender"}
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

	g := CommunicationGroup{MBRegistry: mbReg, AgentID: "sender"}
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

// ---- B5: send_message 邮件链跳数继承 ----

func TestSendMessage_PropagatesChainDepth(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// 当前任务 MailChainDepth=2，期望 outgoing message ChainDepth=3
	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 2, Status: model.TaskStatusProcessing}
	s.tasks[parent.ID] = parent

	g := CommunicationGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "继承链深度",
	}))
	if err != nil {
		t.Fatalf("send 失败: %v", err)
	}

	msgs := recvBox.Drain()
	if len(msgs) != 1 {
		t.Fatalf("期望 1 条消息，实际: %d", len(msgs))
	}
	if msgs[0].ChainDepth != 3 {
		t.Errorf("期望 ChainDepth=3 (parent.MailChainDepth=2 + 1)，实际: %d", msgs[0].ChainDepth)
	}
}

func TestSendMessage_ChainDepth_ZeroParent(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// parent.MailChainDepth=0 → outgoing.ChainDepth=1
	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 0}
	s.tasks[parent.ID] = parent

	g := CommunicationGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, _ = reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "x",
	}))
	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 1 {
		t.Errorf("期望 ChainDepth=1，实际: %+v", msgs)
	}
}

func TestSendMessage_ChainDepth_NilHolder_DefaultsToZero(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// Holder=nil → Scheduler 模式 → ChainDepth=0
	g := CommunicationGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		// Holder 故意留 nil
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "x",
	}))
	if err != nil {
		t.Fatalf("send 失败: %v", err)
	}
	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 0 {
		t.Errorf("nil Holder 时 ChainDepth 应为 0，实际: %+v", msgs)
	}
}

func TestSendMessage_ChainDepth_EmptyTaskID_DefaultsToZero(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// Holder.Get() 返回空字符串 → ChainDepth=0
	g := CommunicationGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: ""},
		Store:      newFakeStore(),
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "x",
	}))
	if err != nil {
		t.Fatalf("send 失败: %v", err)
	}
	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 0 {
		t.Errorf("空 taskID 时 ChainDepth 应为 0，实际: %+v", msgs)
	}
}

func TestSendMessage_ChainDepth_TaskNotFound_DefaultsToZero(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("receiver", "")

	// Holder 指向不存在的 task → GetTask 返回 error → ChainDepth=0
	g := CommunicationGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "nonexistent"},
		Store:      newFakeStore(),
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "receiver",
		"content": "x",
	}))
	if err != nil {
		t.Fatalf("send 不应因 GetTask 失败而中止: %v", err)
	}
	msgs := recvBox.Drain()
	if len(msgs) != 1 || msgs[0].ChainDepth != 0 {
		t.Errorf("task 不存在时 ChainDepth 应兜底为 0，实际: %+v", msgs)
	}
}

func TestSendMessage_ChainDepth_BroadcastInherits(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	boxA := mbReg.Register("a", "")
	boxB := mbReg.Register("b", "")

	s := newFakeStore()
	parent := &model.Task{ID: "current", MailChainDepth: 5}
	s.tasks[parent.ID] = parent

	g := CommunicationGroup{
		MBRegistry: mbReg,
		AgentID:    "sender",
		Holder:     &fakeHolder{id: "current"},
		Store:      s,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("send_message", map[string]any{
		"to":      "*",
		"content": "广播",
	}))
	if err != nil {
		t.Fatalf("broadcast 失败: %v", err)
	}

	msgsA := boxA.Drain()
	msgsB := boxB.Drain()
	if len(msgsA) != 1 || msgsA[0].ChainDepth != 6 {
		t.Errorf("广播收件人 a 应继承 ChainDepth=6，实际: %+v", msgsA)
	}
	if len(msgsB) != 1 || msgsB[0].ChainDepth != 6 {
		t.Errorf("广播收件人 b 应继承 ChainDepth=6，实际: %+v", msgsB)
	}
}

func TestSendMessage_DefaultMsgType(t *testing.T) {
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	recvBox := mbReg.Register("r", "")

	g := CommunicationGroup{MBRegistry: mbReg, AgentID: "sender"}
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

// publish_task 的并发语义：未指定时显式置 1（单交付物任务默认执行一次，
// 不落进 store default_concurrency 兜底，2026-07-22 排查）；显式指定时透传。
