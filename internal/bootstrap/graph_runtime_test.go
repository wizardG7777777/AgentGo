package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/runcontract"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/trace"
)

// ============================================================
// graphBoard 单测
// ============================================================

// TestGraphBoardPublishFields 验证图任务发布的完整字段映射（含 Capability 三路）。
func TestGraphBoardPublishFields(t *testing.T) {
	// 公告板默认允许两个 Agent 协作同一 legacy Task；Graph activation 必须
	// 显式覆盖为 1，不能继承该默认值。
	s := store.NewMemoryTaskStore(nil, 100, 2, 300)
	b := newGraphBoard(s)
	spec := graph.TaskSpec{
		GraphID:      "g-1",
		NodeID:       "implement",
		ActivationID: "implement@1",
		NodeKind:     graph.KindAgent,
		Title:        "实施修改",
		Description:  "写代码",
		Route:        "agent",
		Tools:        []string{"read_file", "write_file"},
		Model:        "m-1",
		Isolation:    "workspace",
	}
	id, err := b.PublishGraphTask(spec)
	if err != nil {
		t.Fatalf("PublishGraphTask 应成功: %v", err)
	}
	if id == "" {
		t.Fatal("PublishGraphTask 应返回生成的 task.ID")
	}
	if id != graphTaskID(spec.GraphID, spec.ActivationID) {
		t.Fatalf("图任务应使用 activation 的确定性 ID: got=%s want=%s", id, graphTaskID(spec.GraphID, spec.ActivationID))
	}
	task, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Description != "实施修改\n\n写代码" {
		t.Errorf("Description = %q，应为标题+换行+描述", task.Description)
	}
	if task.EventType != "agent" {
		t.Errorf("EventType = %q，应为路由 agent", task.EventType)
	}
	if task.GraphID != "g-1" || task.NodeID != "implement" || task.ActivationID != "implement@1" {
		t.Errorf("图身份不符: graph=%q node=%q activation=%q", task.GraphID, task.NodeID, task.ActivationID)
	}
	if task.GraphNodeKind != string(graph.KindAgent) {
		t.Errorf("GraphNodeKind=%q，want %q", task.GraphNodeKind, graph.KindAgent)
	}
	if task.RouteScope != model.GraphRouteScope("g-1") {
		t.Errorf("图任务 RouteScope=%q，want %q", task.RouteScope, model.GraphRouteScope("g-1"))
	}
	if task.ParentTaskID != "" {
		t.Errorf("图任务是独立根任务，ParentTaskID 应为空，实际 %q", task.ParentTaskID)
	}
	if task.MaxConcurrency != 1 {
		t.Errorf("一次 Graph activation 只能有一个执行者，MaxConcurrency=%d，want 1", task.MaxConcurrency)
	}
	if task.Capability == nil {
		t.Fatal("声明了 tools/model/isolation 时应挂 NodeCapability")
	}
	if len(task.Capability.Tools) != 2 || task.Capability.Tools[0] != "read_file" {
		t.Errorf("Capability.Tools 不符: %v", task.Capability.Tools)
	}
	if task.Capability.Model != "m-1" {
		t.Errorf("Capability.Model = %q，应为 m-1", task.Capability.Model)
	}
	if task.Capability.Isolation == nil || task.Capability.Isolation.Mode != "workspace" {
		t.Errorf("Capability.Isolation 不符: %+v", task.Capability.Isolation)
	}
	if err := s.ClaimTask("runner-1", id); err != nil {
		t.Fatalf("首个 Runner 应能认领 Graph Task: %v", err)
	}
	if err := s.ClaimTask("runner-2", id); !errors.Is(err, store.ErrConcurrencyFull) {
		t.Fatalf("第二个 Runner 不得重复执行同一 activation，实际错误: %v", err)
	}
}

// TestGraphBoardPublishWithoutCapability 验证无能力声明时 Capability 为 nil、
// 描述不含追加段。
func TestGraphBoardPublishWithoutCapability(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	b := newGraphBoard(s)
	id, err := b.PublishGraphTask(graph.TaskSpec{
		GraphID:      "g-1",
		NodeID:       "root",
		ActivationID: "root@1",
		NodeKind:     graph.KindController,
		Title:        "完成请求",
		Route:        "controller",
	})
	if err != nil {
		t.Fatalf("PublishGraphTask 应成功: %v", err)
	}
	task, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Capability != nil {
		t.Errorf("无能力声明时 Capability 应为 nil，实际 %+v", task.Capability)
	}
	if task.Description != "完成请求" {
		t.Errorf("无节点描述时 Description 应仅为标题，实际 %q", task.Description)
	}
}

func TestGraphTaskResultExpandsStructuredCarrierWithJSONTypes(t *testing.T) {
	task := &model.Task{
		ID:     "task-structured",
		Status: model.TaskStatusCompleted,
		Results: map[string]string{
			agent.StructuredResultStorageKey: `{"coverage":"gap","retry_count":2,"ready":true,"metrics":{"score":0.75},"items":[1,"x"]}`,
			"event":                          "completed",
			"worker-1":                       "权威结果正文",
		},
	}
	got := graphTaskResult(task)
	if got["coverage"] != "gap" || got["retry_count"] != float64(2) || got["ready"] != true {
		t.Fatalf("结构化标量未类型保真展开: %#v", got)
	}
	metrics, ok := got["metrics"].(map[string]any)
	if !ok || metrics["score"] != float64(0.75) {
		t.Fatalf("嵌套 object 未类型保真展开: %#v", got["metrics"])
	}
	if _, leaked := got[agent.StructuredResultStorageKey]; leaked {
		t.Fatal("内部 carrier 不得泄漏进 Graph Result")
	}
	if got["event"] != "completed" || got["status"] != string(model.TaskStatusCompleted) {
		t.Fatalf("专用 event/status 应保持权威优先级: %#v", got)
	}
}

func TestGraphTaskResultFailedStripsSuccessProtocolAndStructuredCarrier(t *testing.T) {
	task := &model.Task{
		ID:     "task-corrupt-carrier",
		Status: model.TaskStatusFailed,
		Results: map[string]string{
			agent.StructuredResultStorageKey: `{"status":"completed","event":"pass","verdict":"pass","cited_evidence":"ev:fake:1","coverage":"ok"}`,
			"event":                          "pass",
			"verdict":                        "pass",
			"cited_evidence":                 "ev:fake:1",
			"worker-1":                       "失败前的诊断正文",
		},
	}
	got := graphTaskResult(task)
	if got["status"] != string(model.TaskStatusFailed) {
		t.Fatalf("failed 权威终态丢失: %#v", got)
	}
	for _, key := range []string{"event", "verdict", "cited_evidence", "coverage", agent.StructuredResultStorageKey} {
		if _, ok := got[key]; ok {
			t.Fatalf("failed Result 不得暴露可命中成功/custom path 的键 %q: %#v", key, got)
		}
	}
	if got["worker-1"] != "失败前的诊断正文" {
		t.Fatalf("普通诊断正文应保留: %#v", got)
	}
}

func TestGraphTaskResultCancelledStripsSuccessProtocolAndStructuredCarrier(t *testing.T) {
	task := &model.Task{
		ID:     "task-cancelled-carrier",
		Status: model.TaskStatusCancelled,
		Results: map[string]string{
			agent.StructuredResultStorageKey: `{"coverage":"gap","ready":true}`,
			"event":                          "ready",
			"verdict":                        "pass",
			"cited_evidence":                 "ev:fake:1",
		},
	}
	got := graphTaskResult(task)
	if got["status"] != string(model.TaskStatusCancelled) {
		t.Fatalf("cancelled 权威终态丢失: %#v", got)
	}
	for _, key := range []string{"event", "verdict", "cited_evidence", "coverage", "ready", agent.StructuredResultStorageKey} {
		if _, ok := got[key]; ok {
			t.Fatalf("cancelled Result 不得暴露可命中成功/custom path 的键 %q: %#v", key, got)
		}
	}
}

func TestGraphTaskResultBlockedKeepsCustomCarrierButStripsSuccessProtocol(t *testing.T) {
	task := &model.Task{
		ID:     "task-blocked-carrier",
		Status: model.TaskStatusBlocked,
		Results: map[string]string{
			agent.StructuredResultStorageKey: `{"missing_dependency":"catalog","retryable":true}`,
			"event":                          "ready",
			"verdict":                        "pass",
			"cited_evidence":                 "ev:fake:1",
		},
	}
	got := graphTaskResult(task)
	if got["status"] != string(model.TaskStatusBlocked) || got["missing_dependency"] != "catalog" || got["retryable"] != true {
		t.Fatalf("blocked 应保留合法自定义诊断字段: %#v", got)
	}
	for _, key := range []string{"event", "verdict", "cited_evidence", agent.StructuredResultStorageKey} {
		if _, ok := got[key]; ok {
			t.Fatalf("blocked 不得暴露成功协议键 %q: %#v", key, got)
		}
	}
}

// TestGraphBoardIdempotentRepublish 验证 (graph_id, activation_id) 幂等键：
// 同一 activation 补发返回原 task.ID 而不制造重复任务——含「进程内索引为空」
// 的重启恢复路径（新 board 扫公告板去重）。
func TestGraphBoardIdempotentRepublish(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	spec := graph.TaskSpec{
		GraphID:      "g-1",
		NodeID:       "root",
		ActivationID: "root@1",
		NodeKind:     graph.KindController,
		Title:        "完成请求",
		Route:        "controller",
	}
	b := newGraphBoard(s)
	id1, err := b.PublishGraphTask(spec)
	if err != nil {
		t.Fatalf("首次发布应成功: %v", err)
	}
	// 同 board 快路径补发。
	id2, err := b.PublishGraphTask(spec)
	if err != nil {
		t.Fatalf("补发应成功: %v", err)
	}
	if id1 != id2 {
		t.Errorf("同一 activation 补发应返回原 task.ID：%q vs %q", id1, id2)
	}
	// 模拟重启：公告板经快照保留了任务图身份，但进程内索引为空。
	b2 := newGraphBoard(s)
	id3, err := b2.PublishGraphTask(spec)
	if err != nil {
		t.Fatalf("恢复路径补发应成功: %v", err)
	}
	if id3 != id1 {
		t.Errorf("恢复路径应扫公告板去重返回原 task.ID：%q vs %q", id3, id1)
	}
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("幂等补发不应制造重复任务，公告板任务数 = %d，应为 1", len(tasks))
	}

	wrongKind := spec
	wrongKind.NodeKind = graph.KindAcceptance
	if _, err := b2.PublishGraphTask(wrongKind); err == nil || !strings.Contains(err.Error(), "node_kind=controller") || !strings.Contains(err.Error(), "冻结定义=acceptance") {
		t.Fatalf("同 activation 的非空 node kind 不一致必须 fail-closed，实际 err=%v", err)
	}
}

func TestGraphBoardPublishRequiresFrozenNodeKind(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	b := newGraphBoard(s)
	if _, err := b.PublishGraphTask(graph.TaskSpec{
		GraphID: "g-missing-kind", NodeID: "work", ActivationID: "work@1", Title: "执行",
	}); err == nil || !strings.Contains(err.Error(), "缺少冻结 node_kind") {
		t.Fatalf("新 Graph Task 缺少 NodeKind 应拒绝发布，实际 err=%v", err)
	}
}

// TestGraphBoardLookupGraphTask 验证恢复核对面能区分缺失、在途与终态，
// cancelled 按 Graph 契约映射为 failed 且保留原任务状态。
func TestGraphBoardLookupGraphTask(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	b := newGraphBoard(s)
	if _, ok, err := b.LookupGraphTask("missing", "root@1", ""); err != nil || ok {
		t.Fatalf("缺失 activation 应返回 found=false: ok=%v err=%v", ok, err)
	}
	id, err := b.PublishGraphTask(graph.TaskSpec{
		GraphID: "g-lookup", NodeID: "root", ActivationID: "root@1", NodeKind: graph.KindAgent, Title: "执行", Route: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ok, err := b.LookupGraphTask("g-lookup", "root@1", id)
	if err != nil || !ok || snap.TaskID != id || snap.NodeKind != graph.KindAgent || snap.TerminalStatus != "" {
		t.Fatalf("pending 快照错误: found=%v snapshot=%+v err=%v", ok, snap, err)
	}
	if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}
	snap, ok, err = b.LookupGraphTask("g-lookup", "root@1", id)
	if err != nil || !ok || snap.TerminalStatus != graph.NodeFailed {
		t.Fatalf("cancelled 应映射为 Graph failed: found=%v snapshot=%+v err=%v", ok, snap, err)
	}
	if got, _ := snap.Result["status"].(string); got != "cancelled" {
		t.Fatalf("终态结果应保留 cancelled，实际 %q", got)
	}
}

// TestGraphBoardLookupQuarantinesMissingUnknownEffect 验证 Graph journal 有
// 旧 task_id、公告板快照却缺失时，unknown Effect 优先合成为 blocked，
// Runtime 不会把该 activation 误判为可安全补发。
func TestGraphBoardLookupQuarantinesMissingUnknownEffect(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	b := newGraphBoard(s, map[string]string{"old-task": "effect old-task-1 仍 unknown"})
	snap, ok, err := b.LookupGraphTask("g-recover", "write@1", "old-task")
	if err != nil || !ok {
		t.Fatalf("unknown effect 应返回 synthetic terminal: ok=%v err=%v", ok, err)
	}
	if snap.TaskID != "old-task" || snap.TerminalStatus != graph.NodeBlocked {
		t.Fatalf("synthetic terminal 形状错误: %+v", snap)
	}
	if got, _ := snap.Result["error"].(string); !strings.Contains(got, "unknown") {
		t.Fatalf("blocked 结果应保留裁决原因: %+v", snap.Result)
	}
}

// 即使 Graph journal 尚未写回 task_id（expectedTaskID 为空），bridge 也能
// 从 graph/activation 推导确定性 ID，命中 Effect quarantine 而不补发。
func TestGraphBoardLookupQuarantinesReadyActivationWithoutDurableTaskID(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	taskID := graphTaskID("g-ready-crash", "write@1")
	b := newGraphBoard(s, map[string]string{taskID: "shell effect 仍 unknown"})
	snap, ok, err := b.LookupGraphTask("g-ready-crash", "write@1", "")
	if err != nil || !ok {
		t.Fatalf("ready activation 应以确定性 task_id 命中 quarantine: ok=%v err=%v", ok, err)
	}
	if snap.TaskID != taskID || snap.TerminalStatus != graph.NodeBlocked {
		t.Fatalf("synthetic ready quarantine 形状错误: %+v", snap)
	}
	if _, err := b.PublishGraphTask(graph.TaskSpec{
		GraphID: "g-ready-crash", NodeID: "write", ActivationID: "write@1", NodeKind: graph.KindAgent, Title: "write",
	}); err == nil || !strings.Contains(err.Error(), "拒绝整任务重放") {
		t.Fatalf("quarantine activation 不得经 publish 旁路补发: %v", err)
	}
}

// TaskStore/Session 都缺失时，任意 durable Effect 历史（即使已 settled）只
// 证明副作用可能已发生，不证明整任务完成；恢复必须合成 blocked 而非重发。
func TestGraphBoardMissingTaskWithSettledEffectFailsClosed(t *testing.T) {
	taskID := graphTaskID("g-settled-crash", "write@1")
	j, err := effect.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	e := effect.Effect{
		TaskID: taskID, Kind: effect.KindShell, Target: "cmd:abc", ArgsDigest: "123456789abc",
		Policy: effect.PolicyManualOnly,
	}
	if err := j.Prepare(&e); err != nil {
		t.Fatal(err)
	}
	if err := j.Settle(e.ID, "exit_code=0"); err != nil {
		t.Fatal(err)
	}

	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	b := newGraphBoardWithEffects(s, j)
	snap, found, err := b.LookupGraphTask("g-settled-crash", "write@1", "")
	if err != nil || !found || snap.TaskID != taskID || snap.TerminalStatus != graph.NodeBlocked {
		t.Fatalf("settled Effect + missing Task 必须合成 blocked: found=%v snapshot=%+v err=%v", found, snap, err)
	}
	if got, _ := snap.Result["error"].(string); !strings.Contains(got, "effect_history_recovery_quarantine") {
		t.Fatalf("blocked 原因应明确来自 Effect 历史: %+v", snap.Result)
	}
	if _, err := b.PublishGraphTask(graph.TaskSpec{
		GraphID: "g-settled-crash", NodeID: "write", ActivationID: "write@1", NodeKind: graph.KindAgent, Title: "write",
	}); err == nil || !strings.Contains(err.Error(), "拒绝整任务重放") {
		t.Fatalf("PublishGraphTask 不得绕过 settled Effect fence: %v", err)
	}
	if tasks, _ := s.ScanAll(); len(tasks) != 0 {
		t.Fatalf("fence 后不得发布任务，实际 %d", len(tasks))
	}

	// missing-only：若 Session 已恢复真实 Task，则它优先于 Effect 历史。
	if err := s.PublishTask(&model.Task{
		ID: taskID, GraphID: "g-settled-crash", NodeID: "write", ActivationID: "write@1", EventType: "agent",
	}); err != nil {
		t.Fatal(err)
	}
	snap, found, err = b.LookupGraphTask("g-settled-crash", "write@1", "")
	if err != nil || !found || snap.TaskID != taskID || snap.TerminalStatus != "" {
		t.Fatalf("真实 Task 应优先于 missing-task Effect fence: found=%v snapshot=%+v err=%v", found, snap, err)
	}
}

func TestGraphBoardMissingTaskEffectPoliciesAllFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		kind   effect.Kind
		policy effect.ReplayPolicy
		status effect.Status
	}{
		{name: "manual-only-settled", kind: effect.KindShell, policy: effect.PolicyManualOnly, status: effect.StatusSettled},
		{name: "never-replay-settled", kind: effect.KindWorkspaceMerge, policy: effect.PolicyNeverReplay, status: effect.StatusSettled},
		{name: "verify-first-settled", kind: effect.KindFileWrite, policy: effect.PolicyVerifyFirst, status: effect.StatusSettled},
		{name: "manual-only-unknown", kind: effect.KindMessage, policy: effect.PolicyManualOnly, status: effect.StatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graphID := "g-effect-" + tc.name
			taskID := graphTaskID(graphID, "work@1")
			j, err := effect.OpenJournal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = j.Close() })
			e := effect.Effect{
				TaskID: taskID, Kind: tc.kind, Target: "target", ArgsDigest: "123456789abc", Policy: tc.policy,
			}
			if err := j.Prepare(&e); err != nil {
				t.Fatal(err)
			}
			if tc.status == effect.StatusSettled {
				err = j.Settle(e.ID, "done")
			} else {
				err = j.MarkUnknown(e.ID, "crash window")
			}
			if err != nil {
				t.Fatal(err)
			}

			b := newGraphBoardWithEffects(store.NewMemoryTaskStore(nil, 100, 1, 300), j)
			snap, found, err := b.LookupGraphTask(graphID, "work@1", "")
			if err != nil || !found || snap.TerminalStatus != graph.NodeBlocked || snap.TaskID != taskID {
				t.Fatalf("%s history 必须阻断 missing Task 重放: found=%v snapshot=%+v err=%v", tc.name, found, snap, err)
			}
		})
	}
}

// ============================================================
// graphFeedReactor 单测
// ============================================================

// fakeTerminalSink 记录收到的 TerminalFact（实现 graphTerminalSink）。
type fakeTerminalSink struct {
	mu       sync.Mutex
	facts    []graph.TerminalFact
	err      error
	fbErr    error
	fbFacts  []graph.TerminalFact
	fbCauses []error
}

type taskCancelledCapture struct {
	mu     sync.Mutex
	events []trace.Event
}

type graphTerminalOrderingCapture struct {
	mu          sync.Mutex
	tasks       store.TaskStore
	slowTaskID  string
	events      []trace.EventKind
	statusAtEnd model.TaskStatus
}

func (c *graphTerminalOrderingCapture) Name() string { return "graph-terminal-ordering-capture" }
func (c *graphTerminalOrderingCapture) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindTaskCancelled, trace.KindGraphEnded}
}
func (c *graphTerminalOrderingCapture) IsSync() bool  { return true }
func (c *graphTerminalOrderingCapture) Priority() int { return 0 }
func (c *graphTerminalOrderingCapture) Run(ev trace.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev.Kind)
	if ev.Kind == trace.KindTaskCancelled && c.slowTaskID == "" {
		c.slowTaskID = ev.TaskID
	}
	if ev.Kind == trace.KindGraphEnded {
		task, err := c.tasks.GetTask(c.slowTaskID)
		if err != nil {
			return err
		}
		c.statusAtEnd = task.Status
	}
	return nil
}

func (c *taskCancelledCapture) Name() string { return "task-cancelled-capture" }
func (c *taskCancelledCapture) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindTaskCancelled}
}
func (c *taskCancelledCapture) IsSync() bool  { return true }
func (c *taskCancelledCapture) Priority() int { return 900 }
func (c *taskCancelledCapture) Run(ev trace.Event) error {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	return nil
}
func (c *taskCancelledCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (f *fakeTerminalSink) OnTaskTerminal(fact graph.TerminalFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = append(f.facts, fact)
	return f.err
}

// FailTerminalWriteback 记录回落调用（SWE-002 第三层防线的 fake 实现）。
func (f *fakeTerminalSink) FailTerminalWriteback(fact graph.TerminalFact, cause error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fbFacts = append(f.fbFacts, fact)
	f.fbCauses = append(f.fbCauses, cause)
	return f.fbErr
}

func (f *fakeTerminalSink) fbCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fbFacts)
}

func (f *fakeTerminalSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.facts)
}

func (f *fakeTerminalSink) last() graph.TerminalFact {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.facts[len(f.facts)-1]
}

// publishGraphTask 是 feed 测试的快捷发布辅助。
func publishGraphTask(t *testing.T, s *store.MemoryTaskStore, graphID, nodeID, activationID string) string {
	t.Helper()
	b := newGraphBoard(s)
	id, err := b.PublishGraphTask(graph.TaskSpec{
		GraphID: graphID, NodeID: nodeID, ActivationID: activationID,
		NodeKind: graph.KindAgent, Title: "任务", Route: "agent",
	})
	if err != nil {
		t.Fatalf("发布图任务: %v", err)
	}
	return id
}

// TestGraphFeedIgnoresNonGraphTask 验证非图任务（GraphID 为空）的终态事件
// 不回填引擎。
func TestGraphFeedIgnoresNonGraphTask(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	sink := &fakeTerminalSink{}
	feed := newGraphFeedReactor(s, sink)

	plain := &model.Task{Description: "普通任务", EventType: "agent"}
	if err := s.PublishTask(plain); err != nil {
		t.Fatalf("发布普通任务: %v", err)
	}
	for _, kind := range []trace.EventKind{
		trace.KindTaskCompleted, trace.KindTaskFailed, trace.KindTaskBlocked, trace.KindTaskCancelled,
	} {
		if err := feed.Run(trace.Event{Kind: kind, TaskID: plain.ID}); err != nil {
			t.Errorf("非图任务 %s 事件应静默忽略（nil error）: %v", kind, err)
		}
	}
	if sink.count() != 0 {
		t.Errorf("非图任务不应回填引擎，实际收到 %d 条", sink.count())
	}
	// 不存在的任务同样静默忽略。
	if err := feed.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "不存在"}); err != nil {
		t.Errorf("未知任务应静默忽略（nil error）: %v", err)
	}
}

// TestGraphFeedTerminalStatusMapping 表驱动验证四种任务终态到节点终态的映射，
// 以及 Result 合并（status 键 + Results 全量键值）。
func TestGraphFeedTerminalStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		drive      func(s *store.MemoryTaskStore, taskID string) // 把任务驱动到目标终态
		wantStatus graph.NodeStatus
		wantResult string // Result["status"] 应保留的任务原状态
	}{
		{
			name: "completed→completed",
			drive: func(s *store.MemoryTaskStore, id string) {
				if err := s.ClaimTask("worker-1", id); err != nil {
					t.Errorf("认领: %v", err)
				}
				if err := s.SubmitResult("worker-1", id, "ok"); err != nil {
					t.Errorf("提交结果: %v", err)
				}
			},
			wantStatus: graph.NodeCompleted,
			wantResult: "completed",
		},
		{
			name: "failed→failed",
			drive: func(s *store.MemoryTaskStore, id string) {
				if err := s.ClaimTask("worker-1", id); err != nil {
					t.Errorf("认领: %v", err)
				}
				if err := s.FailTask("worker-1", id, "炸了"); err != nil {
					t.Errorf("失败任务: %v", err)
				}
			},
			wantStatus: graph.NodeFailed,
			wantResult: "failed",
		},
		{
			name: "blocked→blocked",
			drive: func(s *store.MemoryTaskStore, id string) {
				if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusBlocked); err != nil {
					t.Errorf("置 blocked: %v", err)
				}
			},
			wantStatus: graph.NodeBlocked,
			wantResult: "blocked",
		},
		{
			// V6 §5：agent 经 submit_task_result status=blocked 自报的路径
			// （processing → blocked，cause=agent_reported_blocked）同样映射
			// NodeBlocked——Graph 只消费已提交的终态事实，不区分阻塞来源。
			name: "agent_reported_blocked→blocked",
			drive: func(s *store.MemoryTaskStore, id string) {
				if err := s.ClaimTask("worker-1", id); err != nil {
					t.Errorf("认领: %v", err)
				}
				if err := s.BlockProcessingTaskBySystem(id, "无生产库写权限", "agent_reported_blocked"); err != nil {
					t.Errorf("agent 自报 blocked: %v", err)
				}
			},
			wantStatus: graph.NodeBlocked,
			wantResult: "blocked",
		},
		{
			name: "cancelled→failed（原状态保留在 Result）",
			drive: func(s *store.MemoryTaskStore, id string) {
				if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
					t.Errorf("置 cancelled: %v", err)
				}
			},
			wantStatus: graph.NodeFailed,
			wantResult: "cancelled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 100, 1, 300)
			sink := &fakeTerminalSink{}
			feed := newGraphFeedReactor(s, sink)
			id := publishGraphTask(t, s, "g-1", "implement", "implement@1")
			tc.drive(s, id)
			if err := feed.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: id}); err != nil {
				t.Fatalf("Run 应成功: %v", err)
			}
			if sink.count() != 1 {
				t.Fatalf("应回填 1 条终态事实，实际 %d", sink.count())
			}
			fact := sink.last()
			if fact.GraphID != "g-1" || fact.NodeID != "implement" || fact.ActivationID != "implement@1" || fact.TaskID != id {
				t.Errorf("终态事实身份不符: %+v", fact)
			}
			if fact.Status != tc.wantStatus {
				t.Errorf("节点终态 = %q，应为 %q", fact.Status, tc.wantStatus)
			}
			if got, _ := fact.Result["status"].(string); got != tc.wantResult {
				t.Errorf("Result[status] = %q，应为 %q", got, tc.wantResult)
			}
		})
	}
}

// TestGraphFeedResultMergesTaskResults 验证 Results 全量键值合并进 Result——
// 含 "event" 键时引擎的事件形态转移条件会优先采用（fixable→pass 回边的
// 语义衔接点）。
func TestGraphFeedResultMergesTaskResults(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	sink := &fakeTerminalSink{}
	feed := newGraphFeedReactor(s, sink)
	id := publishGraphTask(t, s, "g-1", "verify", "verify@1")
	// SubmitResult 以认领者 ID 为 Results 键：以 agentID="event" 提交等价模拟
	// C5b 结构化结果通道（submit_task_result event 参数）写入 Results["event"]。
	if err := s.ClaimTask("event", id); err != nil {
		t.Fatalf("认领: %v", err)
	}
	if err := s.SubmitResult("event", id, "fixable"); err != nil {
		t.Fatalf("提交结果: %v", err)
	}
	if err := feed.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: id}); err != nil {
		t.Fatalf("Run 应成功: %v", err)
	}
	fact := sink.last()
	if got, _ := fact.Result["event"].(string); got != "fixable" {
		t.Errorf("Result[event] = %q，应为 fixable", got)
	}
	if got, _ := fact.Result["status"].(string); got != "completed" {
		t.Errorf("Result[status] = %q，应为 completed", got)
	}
}

// TestGraphFeedSinkError 验证 OnTaskTerminal 报错时 feed 走 SWE-002 回落：
// 以同一终态事实与原始 cause 调 FailTerminalWriteback；回落成功即终态事实
// 已有处置（节点 failed + 唤醒），Run 返回 nil。
func TestGraphFeedSinkError(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	sinkErr := errors.New("图不存在")
	sink := &fakeTerminalSink{err: sinkErr}
	feed := newGraphFeedReactor(s, sink)
	id := publishGraphTask(t, s, "g-1", "root", "root@1")
	if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatalf("置 cancelled: %v", err)
	}
	if err := feed.Run(trace.Event{Kind: trace.KindTaskCancelled, TaskID: id}); err != nil {
		t.Fatalf("回落成功时 Run 应返回 nil（终态事实已处置）: %v", err)
	}
	if sink.fbCount() != 1 {
		t.Fatalf("回填失败应触发 1 次回落，实际 %d", sink.fbCount())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.fbFacts[0].GraphID != "g-1" || sink.fbFacts[0].NodeID != "root" || sink.fbFacts[0].ActivationID != "root@1" || sink.fbFacts[0].TaskID != id {
		t.Errorf("回落收到的终态事实身份不符: %+v", sink.fbFacts[0])
	}
	if !errors.Is(sink.fbCauses[0], sinkErr) {
		t.Errorf("回落收到的 cause 应为 OnTaskTerminal 原始错误 %v，实际 %v", sinkErr, sink.fbCauses[0])
	}
}

// TestGraphFeedSinkFallbackError 验证回落自身再失败（persistence-degraded
// 情形）：Run 返回合并错误交 Registry 记日志，终态事实只剩告警。
func TestGraphFeedSinkFallbackError(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	sink := &fakeTerminalSink{err: errors.New("证据越界拒写"), fbErr: errors.New("图持久化降级")}
	feed := newGraphFeedReactor(s, sink)
	id := publishGraphTask(t, s, "g-1", "root", "root@1")
	if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatalf("置 cancelled: %v", err)
	}
	err := feed.Run(trace.Event{Kind: trace.KindTaskCancelled, TaskID: id})
	if err == nil {
		t.Fatal("回落也失败时 Run 应返回合并错误")
	}
	if !strings.Contains(err.Error(), "证据越界拒写") || !strings.Contains(err.Error(), "图持久化降级") {
		t.Errorf("合并错误应同时含回填失败与回落失败原因: %v", err)
	}
}

func TestGraphFeedIgnoresGraphTerminalCleanupCancellation(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	sink := &fakeTerminalSink{err: errors.New("must not be called")}
	feed := newGraphFeedReactor(s, sink)
	id := publishGraphTask(t, s, "g-ended", "sibling", "sibling@1")
	if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}
	err := feed.Run(trace.Event{
		Kind: trace.KindTaskCancelled, TaskID: id,
		Transition: &trace.Transition{CancelSource: "graph_terminal", Cause: "graph_terminal_cleanup"},
	})
	if err != nil || sink.count() != 0 {
		t.Fatalf("terminal cleanup must not re-enter Graph Runtime: sink=%d err=%v", sink.count(), err)
	}
}

// TestGraphFeedReactorMeta 验证 Reactor 元信息（异步可靠、订阅四种终态事件、
// 优先级档位），防装配期手滑。
func TestGraphFeedReactorMeta(t *testing.T) {
	feed := newGraphFeedReactor(store.NewMemoryTaskStore(nil, 100, 1, 300), &fakeTerminalSink{})
	if feed.Name() != "graph-terminal-feed" {
		t.Errorf("Name = %q", feed.Name())
	}
	if feed.IsSync() || !feed.ReliableAsync() {
		t.Error("feed 必须走 reliable async 通道，既不阻塞 trace.Emit 也不能被背压丢弃")
	}
	if feed.Priority() != 100 {
		t.Errorf("Priority = %d，应为 100（与 task-end-callback 同档）", feed.Priority())
	}
	subs := feed.Subscribe()
	if len(subs) != 4 {
		t.Fatalf("应订阅 4 种任务终态事件，实际 %d", len(subs))
	}
}

func TestGraphBoardTerminateGraphTasksCancelsOnlyLiveTasksInEndedGraph(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	pending := &model.Task{ID: "pending", Description: "pending", EventType: "team:x", GraphID: "g-ended", NodeID: "b", ActivationID: "b@1"}
	processing := &model.Task{ID: "processing", Description: "processing", EventType: "team:x", GraphID: "g-ended", NodeID: "c", ActivationID: "c@1"}
	completed := &model.Task{ID: "completed", Description: "completed", EventType: "team:x", GraphID: "g-ended", NodeID: "a", ActivationID: "a@1"}
	other := &model.Task{ID: "other", Description: "other", EventType: "team:x", GraphID: "g-other", NodeID: "x", ActivationID: "x@1"}
	for _, task := range []*model.Task{pending, processing, completed, other} {
		if err := tasks.PublishTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := tasks.ClaimTask("worker", processing.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", completed.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SubmitResult("worker", completed.ID, "done"); err != nil {
		t.Fatal(err)
	}

	reg := reactor.NewRegistry()
	capture := &taskCancelledCapture{}
	if err := reg.Register(capture); err != nil {
		t.Fatal(err)
	}
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	defer func() {
		trace.SetDefaultDispatcher(previousDispatcher)
		reg.Quiesce(0)
	}()

	board := newGraphBoard(tasks)
	if err := board.TerminateGraphTasks("g-ended"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pending.ID, processing.ID} {
		got, err := tasks.GetTask(id)
		if err != nil || got.Status != model.TaskStatusCancelled {
			t.Fatalf("live Graph task %s = %+v err=%v, want cancelled", id, got, err)
		}
	}
	if got, _ := tasks.GetTask(completed.ID); got.Status != model.TaskStatusCompleted {
		t.Fatalf("completed task changed: %+v", got)
	}
	if got, _ := tasks.GetTask(other.ID); got.Status != model.TaskStatusPending {
		t.Fatalf("other Graph task changed: %+v", got)
	}
	if capture.count() != 1 {
		t.Fatalf("pending cancellation must remain visible to non-feed Reactors, events=%d", capture.count())
	}
	// Startup replay of the same terminal Graph must not change settled state.
	if err := board.TerminateGraphTasks("g-ended"); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCancelsPendingSiblingBeforeGraphEndedDispatch(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	graphs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = graphs.Close() })
	runtime := graph.NewRuntime(graphs, newGraphBoard(tasks))
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: "g-terminal-order", Revision: 1,
		Root: "root", Status: graph.GraphPending,
		Nodes: map[string]graph.Node{
			"root": {Kind: graph.KindAgent, Task: &graph.NodeTask{Title: "root"}, Status: graph.NodeInactive,
				Next: []graph.Transition{{To: "slow"}, {To: "done"}}},
			"slow": {Kind: graph.KindAgent, Task: &graph.NodeTask{Title: "slow"}, Status: graph.NodeInactive,
				Next: []graph.Transition{{To: "slow_done"}}},
			"slow_done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "slow done"}, Status: graph.NodeInactive, Next: []graph.Transition{}},
			"done":      {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "done"}, Status: graph.NodeInactive, Next: []graph.Transition{}},
		},
	}
	if err := runtime.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	root := findGraphTask(tasks, doc.GraphID, "root", "root@1")
	if root == nil {
		t.Fatal("root Graph task not published")
	}
	if err := tasks.ClaimTask("worker", root.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SubmitResult("worker", root.ID, "done"); err != nil {
		t.Fatal(err)
	}

	capture := &graphTerminalOrderingCapture{tasks: tasks}
	reg := reactor.NewRegistry()
	if err := reg.Register(capture); err != nil {
		t.Fatal(err)
	}
	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() {
		trace.SetDefaultDispatcher(previousDispatcher)
		reg.Quiesce(0)
	})

	if err := runtime.OnTaskTerminal(graph.TerminalFact{
		GraphID: doc.GraphID, NodeID: "root", ActivationID: "root@1", TaskID: root.ID,
		Status: graph.NodeCompleted,
	}); err != nil {
		t.Fatal(err)
	}
	slow := findGraphTask(tasks, doc.GraphID, "slow", "slow@1")
	if slow == nil {
		t.Fatal("slow sibling task not published")
	}
	capture.mu.Lock()
	events := append([]trace.EventKind(nil), capture.events...)
	statusAtEnd := capture.statusAtEnd
	capturedSlowID := capture.slowTaskID
	capture.mu.Unlock()
	if capturedSlowID != slow.ID {
		t.Fatalf("cancelled task=%q, want slow sibling %q", capturedSlowID, slow.ID)
	}
	if len(events) < 2 || events[len(events)-2] != trace.KindTaskCancelled || events[len(events)-1] != trace.KindGraphEnded {
		t.Fatalf("terminal event order=%v, want task_cancelled before graph_ended", events)
	}
	if statusAtEnd != model.TaskStatusCancelled {
		t.Fatalf("earliest graph_ended observer saw sibling status=%s, want cancelled", statusAtEnd)
	}
}

type claimRaceTaskStore struct {
	*store.MemoryTaskStore
	once sync.Once
}

func (s *claimRaceTaskStore) TransitionStateWithCancelSource(taskID string, from, to model.TaskStatus, source string) error {
	s.once.Do(func() {
		_ = s.MemoryTaskStore.ClaimTask("racing-worker", taskID)
	})
	return s.MemoryTaskStore.TransitionStateWithCancelSource(taskID, from, to, source)
}

func TestCancelLiveGraphTaskRetriesConcurrentClaim(t *testing.T) {
	base := store.NewMemoryTaskStore(nil, 100, 1, 300)
	task := &model.Task{ID: "claim-race", Description: "race", EventType: "agent", GraphID: "g-race"}
	if err := base.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	racing := &claimRaceTaskStore{MemoryTaskStore: base}
	from, changed, err := cancelLiveGraphTask(racing, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || from != model.TaskStatusProcessing {
		t.Fatalf("cancel result changed=%v from=%s, want true/processing after raced claim", changed, from)
	}
	if got, _ := base.GetTask(task.ID); got.Status != model.TaskStatusCancelled {
		t.Fatalf("raced task status=%s, want cancelled", got.Status)
	}
}

// TestGraphEndWakeReactorPublishesExplicitSchedulerReply 验证 graph_ended
// 不再停留在 trace：顶层图终态会发布一条可持久恢复、marker 幂等的
// __scheduler__ 任务，要求 Scheduler 给用户明确结果。
func TestGraphEndWakeReactorPublishesExplicitSchedulerReply(t *testing.T) {
	gs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	binding := &model.Task{}
	if err := taskcontract.Start(binding, loopcontract.WorkCoordination, "test-graph-finalization/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: "g-finished", Root: "done", Status: graph.GraphPending,
		RunID: binding.RunID, RunContract: binding.RunContract,
		Nodes: map[string]graph.Node{
			"done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "完成"}, Status: graph.NodeInactive, Next: []graph.Transition{}},
		},
	}
	if err := gs.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph: %v", err)
	}
	current, _ := gs.Get(doc.GraphID)
	if err := gs.SetGraphStatus(doc.GraphID, graph.GraphRunning, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(doc.GraphID)
	if err := gs.SetGraphStatus(doc.GraphID, graph.GraphCompleted, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	waker := newGraphEndWakeReactor(tasks, gs)
	for i := 0; i < 2; i++ {
		if err := waker.Run(trace.Event{Kind: trace.KindGraphEnded, GraphID: doc.GraphID}); err != nil {
			t.Fatalf("Run #%d: %v", i+1, err)
		}
	}
	all, err := tasks.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("重复 graph_ended 应只发布一条唤醒任务，实际 %d", len(all))
	}
	wake := all[0]
	if wake.EventType != "__scheduler__" || wake.EventSource != graphEndEventSource || wake.GraphID != "" {
		t.Fatalf("终态唤醒任务形状错误: EventType=%q EventSource=%q GraphID=%q", wake.EventType, wake.EventSource, wake.GraphID)
	}
	if wake.RunID != binding.RunID || wake.RunContract == nil || wake.ContextPolicyRef == "" ||
		wake.ProgressContract == nil || wake.RunPhase != runcontract.PhaseFinalization {
		t.Fatalf("graph-ended 唤醒必须继承完整 finalization binding: %+v", wake)
	}
	for _, want := range []string{"[graph-ended: g-finished/", "completed", "read_graph", "明确"} {
		if !strings.Contains(wake.Description, want) {
			t.Errorf("唤醒描述缺少 %q: %s", want, wake.Description)
		}
	}
}

// TestReconcileTerminalGraphWakes 验证启动恢复会为已 durable 的终态图补发
// 丢失的通知，且公告板已有 marker 时不会重复。owned=nil（无 Session 模式）
// 为全量补发；会话模式下集合外的图不补发（2026-08 二期：不唤醒新会话的
// Scheduler）。
func TestReconcileTerminalGraphWakes(t *testing.T) {
	gs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: "g-recovered", Root: "done", Status: graph.GraphPending,
		Nodes: map[string]graph.Node{
			"done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "完成"}, Status: graph.NodeInactive, Next: []graph.Transition{}},
		},
	}
	if err := gs.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	current, _ := gs.Get(doc.GraphID)
	if err := gs.SetGraphStatus(doc.GraphID, graph.GraphRunning, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(doc.GraphID)
	if err := gs.SetGraphStatus(doc.GraphID, graph.GraphFailed, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reconcileTerminalGraphWakes(gs, tasks, nil)
	reconcileTerminalGraphWakes(gs, tasks, nil)
	all, _ := tasks.ScanAll()
	if len(all) != 1 || !strings.Contains(all[0].Description, "g-recovered") {
		t.Fatalf("恢复补发应幂等生成一条通知，实际 %+v", all)
	}

	// 会话模式：owned 集合为空（fresh start 的新会话不拥有任何图）→ 零唤醒。
	tasks2 := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reconcileTerminalGraphWakes(gs, tasks2, map[string]struct{}{})
	if all2, _ := tasks2.ScanAll(); len(all2) != 0 {
		t.Fatalf("会话模式空 owned 集不应补发唤醒，实际 %+v", all2)
	}
	// 会话模式：owned 含该图 → 补发（--resume 进入的会话保留崩溃窗口通知）。
	tasks3 := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reconcileTerminalGraphWakes(gs, tasks3, map[string]struct{}{"g-recovered": {}})
	if all3, _ := tasks3.ScanAll(); len(all3) != 1 {
		t.Fatalf("owned 集合内的图应补发唤醒，实际 %+v", all3)
	}
}

// TestReconcileTerminalGraphTasksAfterSnapshotRestore covers the crash window
// where Graph status reached durable terminal state but graph_ended task
// cleanup did not run. ImportSnapshot requeues the old processing claim as
// pending; startup reconciliation must cancel it before any Runner can replay
// the activation.
func TestReconcileTerminalGraphTasksAfterSnapshotRestore(t *testing.T) {
	gs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: "g-terminal-crash", Root: "done", Status: graph.GraphPending,
		Nodes: map[string]graph.Node{
			"done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "完成"}, Status: graph.NodeInactive, Next: []graph.Transition{}},
		},
	}
	if err := gs.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	current, _ := gs.Get(doc.GraphID)
	if err := gs.SetGraphStatus(doc.GraphID, graph.GraphRunning, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(doc.GraphID)
	if err := gs.SetGraphStatus(doc.GraphID, graph.GraphCompleted, current.StateVersion); err != nil {
		t.Fatal(err)
	}

	beforeCrash := store.NewMemoryTaskStore(nil, 100, 1, 300)
	pending := &model.Task{ID: "pending-before-crash", Description: "pending", EventType: "agent", GraphID: doc.GraphID, NodeID: "p", ActivationID: "p@1"}
	processing := &model.Task{ID: "processing-before-crash", Description: "processing", EventType: "agent", GraphID: doc.GraphID, NodeID: "r", ActivationID: "r@1"}
	other := &model.Task{ID: "other-graph", Description: "other", EventType: "agent", GraphID: "g-still-running", NodeID: "x", ActivationID: "x@1"}
	for _, task := range []*model.Task{pending, processing, other} {
		if err := beforeCrash.PublishTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := beforeCrash.ClaimTask("worker", processing.ID); err != nil {
		t.Fatal(err)
	}

	restored := store.NewMemoryTaskStore(nil, 100, 1, 300)
	if err := restored.ImportSnapshot(beforeCrash.ExportSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got, _ := restored.GetTask(processing.ID); got.Status != model.TaskStatusPending {
		t.Fatalf("processing snapshot should requeue pending before reconciliation: %+v", got)
	}

	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })
	reconcileTerminalGraphTasks(gs, restored)
	reconcileTerminalGraphTasks(gs, restored) // idempotent replay
	for _, id := range []string{pending.ID, processing.ID} {
		got, getErr := restored.GetTask(id)
		if getErr != nil || got.Status != model.TaskStatusCancelled {
			t.Fatalf("terminal Graph task %s = %+v err=%v, want cancelled", id, got, getErr)
		}
	}
	if got, _ := restored.GetTask(other.ID); got.Status != model.TaskStatusPending {
		t.Fatalf("unrelated Graph task changed: %+v", got)
	}
}

func TestResumeReconcilesLiveDescendantOfTerminalGraphBeforeExecution(t *testing.T) {
	gs, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gs.Close() })

	childID := "g-terminal-parent/sub@1"
	parent := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: "g-terminal-parent", Root: "sub", Status: graph.GraphPending,
		Nodes: map[string]graph.Node{
			"sub": {
				Kind: graph.KindSubgraph, Task: &graph.NodeTask{Title: "child"}, Status: graph.NodeInactive,
				Subgraph: &graph.SubgraphSpec{Root: "inner_done", Nodes: map[string]graph.Node{
					"inner_done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "inner done"}, Status: graph.NodeInactive},
				}},
				Next: []graph.Transition{{To: "done"}},
			},
			"done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "done"}, Status: graph.NodeInactive},
		},
	}
	if err := gs.SubmitGraph(parent); err != nil {
		t.Fatal(err)
	}
	current, _ := gs.Get(parent.GraphID)
	if err := gs.SetGraphStatus(parent.GraphID, graph.GraphRunning, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	parentExec := graph.Execution{Phase: "waiting", ActivationID: "sub@1", ChildGraphID: childID}
	current, _ = gs.Get(parent.GraphID)
	if err := gs.SetExecutionAndStatus(parent.GraphID, "sub", parentExec, graph.NodeReady, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(parent.GraphID)
	if err := gs.SetExecutionAndStatus(parent.GraphID, "sub", parentExec, graph.NodeWaiting, current.StateVersion); err != nil {
		t.Fatal(err)
	}

	child := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: childID, Root: "work", Status: graph.GraphPending,
		Nodes: map[string]graph.Node{
			"work": {Kind: graph.KindAgent, Task: &graph.NodeTask{Title: "work"}, Status: graph.NodeInactive, Next: []graph.Transition{{To: "done"}}},
			"done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "done"}, Status: graph.NodeInactive},
		},
	}
	if err := gs.SubmitGraph(child); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(childID)
	if err := gs.SetGraphStatus(childID, graph.GraphRunning, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	childExec := graph.Execution{Phase: "executing", TaskID: "child-task", ActivationID: "work@1"}
	current, _ = gs.Get(childID)
	if err := gs.SetExecutionAndStatus(childID, "work", childExec, graph.NodeReady, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(childID)
	if err := gs.SetExecutionAndStatus(childID, "work", childExec, graph.NodeRunning, current.StateVersion); err != nil {
		t.Fatal(err)
	}

	// Recreate the old crash snapshot: parent outcome is durable terminal while
	// its materialized child remains running.
	current, _ = gs.Get(parent.GraphID)
	if err := gs.SetNodeStatus(parent.GraphID, "sub", graph.NodeCancelled, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	current, _ = gs.Get(parent.GraphID)
	if err := gs.SetGraphStatus(parent.GraphID, graph.GraphCompleted, current.StateVersion); err != nil {
		t.Fatal(err)
	}

	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	childTask := &model.Task{ID: "child-task", Description: "must not resume", EventType: "agent", GraphID: childID, NodeID: "work", ActivationID: "work@1"}
	if err := tasks.PublishTask(childTask); err != nil {
		t.Fatal(err)
	}
	runtime := graph.NewRuntime(gs, newGraphBoard(tasks))

	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })
	resumeNonTerminalGraphs(&System{Store: tasks, GraphStore: gs, GraphRuntime: runtime})

	repaired, _ := gs.Get(childID)
	if repaired.Status != graph.GraphCancelled || repaired.Nodes["work"].Status != graph.NodeCancelled {
		t.Fatalf("live descendant was resumed instead of cancelled: graph=%s node=%s", repaired.Status, repaired.Nodes["work"].Status)
	}
	if got, _ := tasks.GetTask(childTask.ID); got.Status != model.TaskStatusCancelled {
		t.Fatalf("descendant task status=%s, want cancelled", got.Status)
	}
}

// ============================================================
// wireGraphRuntime 装配单测
// ============================================================

// TestWireGraphRuntime 验证装配函数：持久化目录创建、feed 注册进真实
// Registry、返回的 Store/Runtime 可用。Windows 纪律：t.Cleanup 先 Close。
func TestWireGraphRuntime(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{ProjectRoot: root}
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reg := reactor.NewRegistry()
	t.Cleanup(func() { reg.Quiesce(0) })

	gs, rt, err := wireGraphRuntime(cfg, s, reg, nil, nil)
	if err != nil {
		t.Fatalf("wireGraphRuntime 应成功: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	if gs == nil || rt == nil {
		t.Fatal("wireGraphRuntime 应返回非空 Store/Runtime")
	}
	// 持久化目录与 artifacts 同基：<project_root>/.agentgo/state/graphs。
	wantDir := filepath.Join(root, ".agentgo", "state", "graphs")
	if !dirExists(wantDir) {
		t.Errorf("持久化目录 %s 应已创建", wantDir)
	}
	// feed 已订阅四种终态事件。
	for _, kind := range []trace.EventKind{
		trace.KindTaskCompleted, trace.KindTaskFailed, trace.KindTaskBlocked, trace.KindTaskCancelled,
	} {
		found := false
		for _, r := range reg.Subscribers(kind) {
			if r.Name() == "graph-terminal-feed" {
				found = true
			}
		}
		if !found {
			t.Errorf("graph-terminal-feed 应订阅 %s", kind)
		}
	}
	foundEndWake := false
	for _, r := range reg.Subscribers(trace.KindGraphEnded) {
		if r.Name() == "graph-ended-scheduler-wake" {
			foundEndWake = true
		}
	}
	if !foundEndWake {
		t.Error("graph-ended-scheduler-wake 应订阅 graph_ended")
	}
	// 空目录 Recover 不报错、无图。
	if len(gs.List()) != 0 {
		t.Errorf("空持久化根不应恢复出图，实际 %d 张", len(gs.List()))
	}
}

func dirExists(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// ============================================================
// Session 生命周期隔离：补集停驻与恢复过滤
// ============================================================

// submitReadyAgentGraph 提交一张归属 sessionID 的图（a(agent)→b(end)），并
// 把节点 a 置为 ready（activation a@1 已 durable、任务未发布），模拟进程
// 重启前的在途状态——恢复时应对账补发任务。
func submitReadyAgentGraph(t *testing.T, gs *graph.Store, graphID, sessionID string) {
	t.Helper()
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: graphID, Root: "a", Status: graph.GraphPending,
		SessionID: sessionID,
		Nodes: map[string]graph.Node{
			"a": {Kind: graph.KindAgent, Task: &graph.NodeTask{Title: "做 A"}, Status: graph.NodeInactive, Next: []graph.Transition{{To: "b"}}},
			"b": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "结束"}, Status: graph.NodeInactive},
		},
	}
	if err := gs.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph(%s) 应成功: %v", graphID, err)
	}
	current, _ := gs.Get(graphID)
	if err := gs.SetGraphStatus(graphID, graph.GraphRunning, current.StateVersion); err != nil {
		t.Fatalf("SetGraphStatus(%s) 应成功: %v", graphID, err)
	}
	exec := graph.Execution{Phase: "executing", ActivationID: "a@1"}
	current, _ = gs.Get(graphID)
	if err := gs.SetExecutionAndStatus(graphID, "a", exec, graph.NodeReady, current.StateVersion); err != nil {
		t.Fatalf("SetExecutionAndStatus(%s) 应成功: %v", graphID, err)
	}
}

// taskForGraph 返回公告板上属于 graphID 的第一个任务（无则 nil）。
func taskForGraph(t *testing.T, tasks store.TaskStore, graphID string) *model.Task {
	t.Helper()
	all, err := tasks.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll 应成功: %v", err)
	}
	for _, task := range all {
		if task != nil && task.GraphID == graphID {
			return task
		}
	}
	return nil
}

// TestResumeNonTerminalGraphsSessionFilter 2026-08 二期：会话模式下启动不再
// 恢复任何非终态图（历史图已在 wireGraphRuntime 全量停驻，进入会话不自动
// 续跑）；仅无 Session 模式（owned==nil）保持全量恢复（行为同今）。
func TestResumeNonTerminalGraphsSessionFilter(t *testing.T) {
	gs, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() }) // Windows 纪律：先 Close 再让 TempDir 清理

	mgr, err := session.NewSessionManager(filepath.Join(t.TempDir(), "sessions"), session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager 应成功: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	curID := mgr.Current().ID

	submitReadyAgentGraph(t, gs, "g-cur", curID)
	submitReadyAgentGraph(t, gs, "g-other", "sess-other")

	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	rt := graph.NewRuntime(gs, newGraphBoard(tasks))

	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })
	resumeNonTerminalGraphs(&System{Store: tasks, GraphStore: gs, GraphRuntime: rt, SessionMgr: mgr})

	// 会话模式：当前 session 的图也不恢复——不补发任务、节点保持 ready。
	if task := taskForGraph(t, tasks, "g-cur"); task != nil {
		t.Errorf("会话模式启动不应恢复 g-cur，实际补发了任务 %s", task.ID)
	}
	if doc, _ := gs.Get("g-cur"); doc.Nodes["a"].Status != graph.NodeReady {
		t.Errorf("g-cur 的 a 应保持 ready（未恢复），实际 %s", doc.Nodes["a"].Status)
	}

	// 非当前 session 的图同样不恢复。
	if task := taskForGraph(t, tasks, "g-other"); task != nil {
		t.Errorf("非当前 session 的图 g-other 不应被恢复，实际补发了任务 %s", task.ID)
	}
	if doc, _ := gs.Get("g-other"); doc.Nodes["a"].Status != graph.NodeReady {
		t.Errorf("g-other 的 a 应保持 ready（未恢复），实际 %s", doc.Nodes["a"].Status)
	}

	// 无 Session 模式（System 无 SessionMgr）：全量恢复，行为同今。
	tasks2 := store.NewMemoryTaskStore(nil, 100, 1, 300)
	rt2 := graph.NewRuntime(gs, newGraphBoard(tasks2))
	resumeNonTerminalGraphs(&System{Store: tasks2, GraphStore: gs, GraphRuntime: rt2})
	if task := taskForGraph(t, tasks2, "g-cur"); task == nil {
		t.Error("无 Session 模式应恢复 g-cur 并补发任务，实际公告板无其任务")
	}
	if doc, _ := gs.Get("g-cur"); doc.Nodes["a"].Status != graph.NodeRunning {
		t.Errorf("无 Session 模式下 g-cur 的 a 应转 running，实际 %s", doc.Nodes["a"].Status)
	}
}

// TestWireGraphRuntimeComplementSuspension 装配期停驻（2026-08 二期）：会话
// 模式下全部历史图——不属于当前 session 的、无归属的（不再归并）、以及当前
// session 自己的（--resume 进入不自动续跑）——在 wireGraphRuntime 后即处
// 停驻（ResumeGraph 空操作、不补发任务）。
func TestWireGraphRuntimeComplementSuspension(t *testing.T) {
	root := t.TempDir()
	graphsDir := filepath.Join(root, ".agentgo", "state", "graphs")
	gs0, err := graph.NewStore(graphsDir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	submitReadyAgentGraph(t, gs0, "g-old", "sess-old")
	submitReadyAgentGraph(t, gs0, "g-cur", "")
	submitReadyAgentGraph(t, gs0, "g-own", "sess-cur")
	if err := gs0.Close(); err != nil { // Windows 纪律：装配前关闭旧句柄
		t.Fatalf("Close 应成功: %v", err)
	}

	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reg := reactor.NewRegistry()
	t.Cleanup(func() { reg.Quiesce(0) })
	gs, rt, err := wireGraphRuntime(&config.Config{ProjectRoot: root}, tasks, reg, nil, func() string { return "sess-cur" })
	if err != nil {
		t.Fatalf("wireGraphRuntime 应成功: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })

	// 无归属历史图不再归并当前 session。
	if got := rt.GraphsForSession("sess-cur"); len(got) != 1 || got[0] != "g-own" {
		t.Fatalf("sess-cur 应只拥有 [g-own]（无归并），实际 %v", got)
	}

	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })

	// 三张历史图全部停驻：ResumeGraph 空操作，不补发任务。
	for _, id := range []string{"g-old", "g-cur", "g-own"} {
		if err := rt.ResumeGraph(id); err != nil {
			t.Fatalf("停驻图 %s 的 ResumeGraph 应返回 nil: %v", id, err)
		}
		if task := taskForGraph(t, tasks, id); task != nil {
			t.Errorf("%s 已停驻，不应补发任务，实际补发了 %s", id, task.ID)
		}
	}
}

// TestWireGraphRuntimeNoSessionMode 无 Session 模式（provider 为 nil）：
// 补集停驻空操作，任何归属的历史图都正常恢复（行为同今）。
func TestWireGraphRuntimeNoSessionMode(t *testing.T) {
	root := t.TempDir()
	graphsDir := filepath.Join(root, ".agentgo", "state", "graphs")
	gs0, err := graph.NewStore(graphsDir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	submitReadyAgentGraph(t, gs0, "g-old", "sess-old")
	if err := gs0.Close(); err != nil {
		t.Fatalf("Close 应成功: %v", err)
	}

	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reg := reactor.NewRegistry()
	t.Cleanup(func() { reg.Quiesce(0) })
	gs, rt, err := wireGraphRuntime(&config.Config{ProjectRoot: root}, tasks, reg, nil, nil)
	if err != nil {
		t.Fatalf("wireGraphRuntime 应成功: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })

	previousDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() { trace.SetDefaultDispatcher(previousDispatcher) })

	// 无 Session 模式不停驻任何图：g-old 正常恢复补发任务。
	if err := rt.ResumeGraph("g-old"); err != nil {
		t.Fatalf("g-old 的 ResumeGraph 应成功: %v", err)
	}
	if task := taskForGraph(t, tasks, "g-old"); task == nil {
		t.Error("无 Session 模式不应停驻任何图，g-old 恢复应补发任务")
	}
}
