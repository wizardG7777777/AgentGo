package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// ============================================================
// graphBoard 单测
// ============================================================

// TestGraphBoardPublishFields 验证图任务发布的完整字段映射（含 Capability 三路）。
func TestGraphBoardPublishFields(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	b := newGraphBoard(s)
	spec := graph.TaskSpec{
		GraphID:      "g-1",
		NodeID:       "implement",
		ActivationID: "implement@1",
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
	if task.ParentTaskID != "" {
		t.Errorf("图任务是独立根任务，ParentTaskID 应为空，实际 %q", task.ParentTaskID)
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

// TestGraphBoardIdempotentRepublish 验证 (graph_id, activation_id) 幂等键：
// 同一 activation 补发返回原 task.ID 而不制造重复任务——含「进程内索引为空」
// 的重启恢复路径（新 board 扫公告板去重）。
func TestGraphBoardIdempotentRepublish(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	spec := graph.TaskSpec{
		GraphID:      "g-1",
		NodeID:       "root",
		ActivationID: "root@1",
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
		GraphID: "g-lookup", NodeID: "root", ActivationID: "root@1", Title: "执行", Route: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ok, err := b.LookupGraphTask("g-lookup", "root@1", id)
	if err != nil || !ok || snap.TaskID != id || snap.TerminalStatus != "" {
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
		GraphID: "g-ready-crash", NodeID: "write", ActivationID: "write@1", Title: "write",
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
		GraphID: "g-settled-crash", NodeID: "write", ActivationID: "write@1", Title: "write",
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
	mu    sync.Mutex
	facts []graph.TerminalFact
	err   error
}

func (f *fakeTerminalSink) OnTaskTerminal(fact graph.TerminalFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = append(f.facts, fact)
	return f.err
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
		Title: "任务", Route: "agent",
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

// TestGraphFeedSinkError 验证引擎错误原样返回（由 Registry 的 async 路径记
// 日志，feed 自身不吞咽也不放大）。
func TestGraphFeedSinkError(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	sink := &fakeTerminalSink{err: errors.New("图不存在")}
	feed := newGraphFeedReactor(s, sink)
	id := publishGraphTask(t, s, "g-1", "root", "root@1")
	if err := s.TransitionState(id, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatalf("置 cancelled: %v", err)
	}
	if err := feed.Run(trace.Event{Kind: trace.KindTaskCancelled, TaskID: id}); err == nil {
		t.Error("引擎返回错误时 Run 应原样返回该错误")
	}
}

// TestGraphFeedReactorMeta 验证 Reactor 元信息（异步、订阅四种终态事件、
// 优先级档位），防装配期手滑。
func TestGraphFeedReactorMeta(t *testing.T) {
	feed := newGraphFeedReactor(store.NewMemoryTaskStore(nil, 100, 1, 300), &fakeTerminalSink{})
	if feed.Name() != "graph-terminal-feed" {
		t.Errorf("Name = %q", feed.Name())
	}
	if feed.IsSync() {
		t.Error("feed 应为 Async（不阻塞 trace.Emit 调用方）")
	}
	if feed.Priority() != 100 {
		t.Errorf("Priority = %d，应为 100（与 task-end-callback 同档）", feed.Priority())
	}
	subs := feed.Subscribe()
	if len(subs) != 4 {
		t.Fatalf("应订阅 4 种任务终态事件，实际 %d", len(subs))
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
	doc := &graph.GraphDocument{
		Schema: graph.SchemaV1, GraphID: "g-finished", Root: "done", Status: graph.GraphPending,
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
	for _, want := range []string{"[graph-ended: g-finished/", "completed", "read_graph", "明确"} {
		if !strings.Contains(wake.Description, want) {
			t.Errorf("唤醒描述缺少 %q: %s", want, wake.Description)
		}
	}
}

// TestReconcileTerminalGraphWakes 验证启动恢复会为已 durable 的终态图补发
// 丢失的通知，且公告板已有 marker 时不会重复。
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
	reconcileTerminalGraphWakes(gs, tasks)
	reconcileTerminalGraphWakes(gs, tasks)
	all, _ := tasks.ScanAll()
	if len(all) != 1 || !strings.Contains(all[0].Description, "g-recovered") {
		t.Fatalf("恢复补发应幂等生成一条通知，实际 %+v", all)
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

	gs, rt, err := wireGraphRuntime(cfg, s, reg, nil)
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
