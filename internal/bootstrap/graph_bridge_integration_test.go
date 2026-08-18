package bootstrap

// 本文件是 V6 Graph 运行桥接（C5a）的离线端到端集成冒烟：
// 真实 MemoryTaskStore + graph.Runtime + graphBoard + graphFeedReactor
// （注册进真实 reactor.Registry 并经 trace.SetDefaultDispatcher 挂载），
// 模拟 runner 走「认领 → 提交结果 → trace.Emit 终态事件」的活系统路径，
// 断言引擎经 feed 回填后自动推进图（发布下游任务 / 回边新 activation /
// 图收官 + graph_ended 事件落 graph_ 分片）。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/graph"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// graphEndedCapture 是观测档 sync Reactor，记录 graph_ended 事件。
// sync 保证 Emit 返回即可断言（事件由引擎在 feed 的 async goroutine 内发出，
// capture 在同一调用栈同步执行）。
type graphEndedCapture struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *graphEndedCapture) Name() string  { return "graph-ended-capture" }
func (c *graphEndedCapture) IsSync() bool  { return true }
func (c *graphEndedCapture) Priority() int { return 1000 }
func (c *graphEndedCapture) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindGraphEnded}
}
func (c *graphEndedCapture) Run(ev trace.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *graphEndedCapture) sawGraphEnded(graphID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		if ev.GraphID == graphID {
			return true
		}
	}
	return false
}

// graphBridgeEnv 是集成测试环境：真实公告板 + 真实图持久化（TempDir）+
// 真实 reactor Registry（经 SetDefaultDispatcher 挂载）+ 真实 trace writer。
type graphBridgeEnv struct {
	tasks    *store.MemoryTaskStore
	graphs   *graph.Store
	runtime  *graph.Runtime
	capture  *graphEndedCapture
	traceDir string
}

// newGraphBridgeEnv 装配集成环境。Windows 纪律：trace writer 与 graph Store
// 都在 t.Cleanup 先 Close（在 TempDir 清理之前）；先 Quiesce 排空在途
// async reactor，避免写已关闭的 journal。
func newGraphBridgeEnv(t *testing.T) *graphBridgeEnv {
	t.Helper()
	traceDir := t.TempDir()
	w, err := trace.NewWriter(traceDir, 0)
	if err != nil {
		t.Fatalf("创建 trace writer: %v", err)
	}
	trace.SetDefault(w)
	prevDispatcher := trace.DefaultDispatcher()

	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 300)
	graphStore, err := graph.NewStore(filepath.Join(t.TempDir(), "graphs"))
	if err != nil {
		t.Fatalf("创建 graph Store: %v", err)
	}
	rt := graph.NewRuntime(graphStore, newGraphBoard(taskStore))
	reg := reactor.NewRegistry()
	if err := reg.Register(newGraphFeedReactor(taskStore, rt)); err != nil {
		t.Fatalf("注册 graph-terminal-feed: %v", err)
	}
	capture := &graphEndedCapture{}
	if err := reg.Register(capture); err != nil {
		t.Fatalf("注册 capture: %v", err)
	}
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() {
		trace.SetDefaultDispatcher(prevDispatcher)
		reg.Quiesce(0)
		trace.SetDefault(nil)
		_ = w.Close()
		_ = graphStore.Close()
	})
	return &graphBridgeEnv{tasks: taskStore, graphs: graphStore, runtime: rt, capture: capture, traceDir: traceDir}
}

// eventually 轮询等待条件满足（异步事件分发），禁止裸 sleep 大时长。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（3s）：%s", what)
}

// findGraphTask 按图身份在公告板中找任务（找不到返回 nil）。
func findGraphTask(s *store.MemoryTaskStore, graphID, nodeID, activationID string) *model.Task {
	tasks, err := s.ScanAll()
	if err != nil {
		return nil
	}
	for _, task := range tasks {
		if task != nil && task.GraphID == graphID && task.NodeID == nodeID && task.ActivationID == activationID {
			return task
		}
	}
	return nil
}

// mustFindGraphTask 断言图任务存在并返回。
func mustFindGraphTask(t *testing.T, s *store.MemoryTaskStore, graphID, nodeID, activationID string) *model.Task {
	t.Helper()
	task := findGraphTask(s, graphID, nodeID, activationID)
	if task == nil {
		t.Fatalf("公告板应存在图任务（图 %s 节点 %s activation %s）", graphID, nodeID, activationID)
	}
	return task
}

// runTaskToCompleted 模拟 runner：认领任务、提交结果、发射终态事件
// （活系统中 KindTaskCompleted 由 agent 在 SubmitResult 成功后发出）。
// claimAs 即认领者 ID——SubmitResult 以它为 Results 键。
func runTaskToCompleted(t *testing.T, s *store.MemoryTaskStore, claimAs, taskID, result string) {
	t.Helper()
	if err := s.ClaimTask(claimAs, taskID); err != nil {
		t.Fatalf("认领任务 %s: %v", taskID, err)
	}
	if err := s.SubmitResult(claimAs, taskID, result); err != nil {
		t.Fatalf("提交任务 %s 结果: %v", taskID, err)
	}
	trace.Emit(trace.Event{Kind: trace.KindTaskCompleted, TaskID: taskID})
}

// graphShardContains 断言 graph_<graph_id前8位>.jsonl 分片落盘且含目标内容
// （trace writer 写穿无缓冲，运行中即可读）。
func graphShardContains(t *testing.T, traceDir, graphID, want string) {
	t.Helper()
	if len(graphID) > 8 {
		graphID = graphID[:8]
	}
	data, err := os.ReadFile(filepath.Join(traceDir, "graph_"+graphID+".jsonl"))
	if err != nil {
		t.Fatalf("读取 graph 分片: %v", err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("graph 分片应含 %q，实际内容:\n%s", want, string(data))
	}
}

// bridgeLinearGraphJSON 线性图：root(agent) → implement(agent) → finish(end)。
const bridgeLinearGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-linear",
  "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"理解需求","description":"分解目标"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"implement"}]},
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "capability":{"tools":["read_file","write_file"],"model":"m-1"},
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestGraphBridgeLinearGraphEndToEnd 线性图全链路：
// SubmitGraph → root 任务自动发布（带图身份）→ runner 完成 root → feed
// 回填 → implement 任务自动发布（新 activation）→ 完成 → 图 completed +
// graph_ended 事件（Registry 捕获 + graph_ 分片落盘）。
func TestGraphBridgeLinearGraphEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeLinearGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	// root 任务已自动发布到真实公告板，携带图身份。
	rootTask := mustFindGraphTask(t, env.tasks, "g-bridge-linear", "root", "root@1")
	if rootTask.EventType != "" {
		t.Errorf("root 任务 EventType = %q，C5b 路由映射后 agent 节点应进默认队列（空串）", rootTask.EventType)
	}
	if rootTask.GraphID != "g-bridge-linear" || rootTask.NodeID != "root" || rootTask.ActivationID != "root@1" {
		t.Errorf("root 任务应携带完整图身份，实际 GraphID=%q NodeID=%q ActivationID=%q",
			rootTask.GraphID, rootTask.NodeID, rootTask.ActivationID)
	}
	if rootTask.Description != "理解需求\n\n分解目标" {
		t.Errorf("root 任务描述 = %q", rootTask.Description)
	}

	// 模拟 runner 完成 root → feed 回填 → implement 应自动发布（新 activation）。
	runTaskToCompleted(t, env.tasks, "runner-1", rootTask.ID, "需求已理解")
	var implTask *model.Task
	eventually(t, "root 终态后 implement 任务应被自动发布", func() bool {
		implTask = findGraphTask(env.tasks, "g-bridge-linear", "implement", "implement@1")
		return implTask != nil
	})
	if implTask.ID == rootTask.ID {
		t.Error("implement 应是新任务，不应复用 root 的 task.ID")
	}
	if implTask.Capability == nil || len(implTask.Capability.Tools) != 2 || implTask.Capability.Model != "m-1" {
		t.Errorf("implement 任务应透传节点 capability: %+v", implTask.Capability)
	}

	// 完成 implement → end 激活 → 图 completed + graph_ended。
	runTaskToCompleted(t, env.tasks, "runner-1", implTask.ID, "修改完成")
	eventually(t, "图应到达 completed", func() bool {
		g, ok := env.graphs.Get("g-bridge-linear")
		return ok && g.Status == graph.GraphCompleted
	})
	eventually(t, "应发出 graph_ended 事件", func() bool {
		return env.capture.sawGraphEnded("g-bridge-linear")
	})
	graphShardContains(t, env.traceDir, "g-bridge-linear", "graph_ended")
}

// bridgeBackEdgeGraphJSON 回边图：implement(agent) → verify(agent)
//
//	--event pass--> finish(end)
//	--event fixable, activation:new--> implement（回边，新 activation）
const bridgeBackEdgeGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-loop",
  "revision": 1, "state_version": 0,
  "root": "implement", "status": "pending",
  "nodes": {
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"verify"}]},
    "verify": {"kind":"agent","task":{"title":"验证修改"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"finish","when":{"event":"pass"}},
        {"to":"implement","activation":"new","when":{"event":"fixable"}}
      ]},
    "finish": {"kind":"end","task":{"title":"形成结果"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestGraphBridgeBackEdgeGraphEndToEnd 回边图全链路（fixable→pass）：
// verify 判定 fixable 时经回边以新 activation 重进 implement（新任务），
// 第二轮判定 pass 后图收官。verify 的结果经 Results["event"] 键驱动事件
// 形态转移条件——此处以 agentID="event" 认领模拟写入 Results["event"]
// （C5b 的真实写路径是 submit_task_result 的 event 参数经
// store.RecordResultField 落库，端到端覆盖见 C5b 集成测试）。
func TestGraphBridgeBackEdgeGraphEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeBackEdgeGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	// 第一轮：implement@1 → verify@1 判定 fixable → 回边重进 implement@2。
	impl1 := mustFindGraphTask(t, env.tasks, "g-bridge-loop", "implement", "implement@1")
	runTaskToCompleted(t, env.tasks, "runner-1", impl1.ID, "第一版修改")
	var verify1 *model.Task
	eventually(t, "implement@1 终态后 verify@1 任务应被自动发布", func() bool {
		verify1 = findGraphTask(env.tasks, "g-bridge-loop", "verify", "verify@1")
		return verify1 != nil
	})
	runTaskToCompleted(t, env.tasks, "event", verify1.ID, "fixable")
	var impl2 *model.Task
	eventually(t, "verify@1 判定 fixable 后 implement 应经回边获得 @2 新 activation", func() bool {
		impl2 = findGraphTask(env.tasks, "g-bridge-loop", "implement", "implement@2")
		return impl2 != nil
	})
	if impl2.ID == impl1.ID {
		t.Error("回边重进应发布新任务（新 activation），不应复用旧 task.ID")
	}

	// 第二轮：implement@2 → verify@2 判定 pass → 图 completed。
	runTaskToCompleted(t, env.tasks, "runner-1", impl2.ID, "修复后的修改")
	var verify2 *model.Task
	eventually(t, "implement@2 终态后 verify@2 任务应被自动发布", func() bool {
		verify2 = findGraphTask(env.tasks, "g-bridge-loop", "verify", "verify@2")
		return verify2 != nil
	})
	runTaskToCompleted(t, env.tasks, "event", verify2.ID, "pass")
	eventually(t, "图应到达 completed", func() bool {
		g, ok := env.graphs.Get("g-bridge-loop")
		return ok && g.Status == graph.GraphCompleted
	})
	eventually(t, "应发出 graph_ended 事件", func() bool {
		return env.capture.sawGraphEnded("g-bridge-loop")
	})
	graphShardContains(t, env.traceDir, "g-bridge-loop", "graph_ended")

	// 节点终态佐证：verify@2 completed、implement 两次 activation 都 completed。
	g, ok := env.graphs.Get("g-bridge-loop")
	if !ok {
		t.Fatal("图应存在")
	}
	if n := g.Nodes["verify"]; n.Status != graph.NodeCompleted || n.Execution == nil || n.Execution.ActivationID != "verify@2" {
		t.Errorf("verify 当前 activation 应为 verify@2 且 completed: status=%s execution=%+v", n.Status, n.Execution)
	}
	if n := g.Nodes["implement"]; n.Status != graph.NodeCompleted || n.Execution == nil || n.Execution.ActivationID != "implement@2" {
		t.Errorf("implement 当前 activation 应为 implement@2 且 completed: status=%s execution=%+v", n.Status, n.Execution)
	}
}

// ============================================================
// C5b：submit_task_result event 参数 → 事件形态转移条件 端到端
// ============================================================

// bridgeEventGraphJSON 事件路由图：collect(agent)
//
//	--event ready--> report(agent) --> finish(end)。
//
// collect 只有 {event: ready} 一条出路：完成时带 event=ready 才推进，
// 不带事件键则「无任何匹配的出路」图置 failed。
const bridgeEventGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-event",
  "revision": 1, "state_version": 0,
  "root": "collect", "status": "pending",
  "nodes": {
    "collect": {"kind":"agent","task":{"title":"收集材料"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"report","when":{"event":"ready"}}]},
    "report": {"kind":"agent","task":{"title":"形成报告"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// runGraphNodeWithEvent 模拟 runner 走 C5b 真实收尾路径（与 agent finalization
// 短路分支同序）：认领 → store.RecordResultField 写 Results["event"] →
// SubmitResult → trace.Emit 终态事件（feed 回填事件在 Emit 后异步触发）。
func runGraphNodeWithEvent(t *testing.T, s *store.MemoryTaskStore, claimAs, taskID, result, event string) {
	t.Helper()
	if err := s.ClaimTask(claimAs, taskID); err != nil {
		t.Fatalf("认领任务 %s: %v", taskID, err)
	}
	if event != "" {
		if err := store.RecordResultField(s, taskID, "event", event); err != nil {
			t.Fatalf("写入 Results[event]: %v", err)
		}
	}
	if err := s.SubmitResult(claimAs, taskID, result); err != nil {
		t.Fatalf("提交任务 %s 结果: %v", taskID, err)
	}
	trace.Emit(trace.Event{Kind: trace.KindTaskCompleted, TaskID: taskID})
}

// TestGraphBridgeEventEdgeEndToEnd（C5b 端到端）：agent 节点经 C5b 结构化结果
// 通道带 event=ready 完成 → feed 把 Results["event"] 并入 TerminalFact.Result
// → 下游 {event: ready} 边激活（report 任务自动发布）→ 图收官。
// 顺带回断路由映射：collect（agent）任务应进默认队列（EventType=""）。
func TestGraphBridgeEventEdgeEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeEventGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	collectTask := mustFindGraphTask(t, env.tasks, "g-bridge-event", "collect", "collect@1")
	if collectTask.EventType != "" {
		t.Errorf("collect（agent）任务 EventType = %q，应为默认队列（空串）", collectTask.EventType)
	}

	// collect 带 event=ready 完成 → {event: ready} 边激活 → report@1 发布。
	runGraphNodeWithEvent(t, env.tasks, "runner-1", collectTask.ID, "材料已就绪", "ready")
	var reportTask *model.Task
	eventually(t, "collect 带 event=ready 终态后 report 任务应被自动发布", func() bool {
		reportTask = findGraphTask(env.tasks, "g-bridge-event", "report", "report@1")
		return reportTask != nil
	})

	// report 不带事件键完成（无条件边）→ 图 completed + graph_ended。
	runGraphNodeWithEvent(t, env.tasks, "runner-1", reportTask.ID, "报告已成文", "")
	eventually(t, "图应到达 completed", func() bool {
		g, ok := env.graphs.Get("g-bridge-event")
		return ok && g.Status == graph.GraphCompleted
	})
	eventually(t, "应发出 graph_ended 事件", func() bool {
		return env.capture.sawGraphEnded("g-bridge-event")
	})
	graphShardContains(t, env.traceDir, "g-bridge-event", "graph_ended")
}

// TestGraphBridgeEventEdgeMismatchFails 负面对照：collect 完成时不带事件键，
// 唯一的 {event: ready} 出路不匹配 → 图置 failed（不激活 report）。
func TestGraphBridgeEventEdgeMismatchFails(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeEventGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	collectTask := mustFindGraphTask(t, env.tasks, "g-bridge-event", "collect", "collect@1")
	runGraphNodeWithEvent(t, env.tasks, "runner-1", collectTask.ID, "做完了但忘了报事件", "")
	eventually(t, "无匹配出路时图应到达 failed", func() bool {
		g, ok := env.graphs.Get("g-bridge-event")
		return ok && g.Status == graph.GraphFailed
	})
	if tsk := findGraphTask(env.tasks, "g-bridge-event", "report", "report@1"); tsk != nil {
		t.Error("事件不匹配时 report 任务不应被发布")
	}
}

// bridgeControllerEventGraphJSON 专门锁定 controller 的角色接缝：controller
// 路由为 __scheduler__，但仍须经 submit_task_result(event=ready) 推进条件边。
const bridgeControllerEventGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-controller-event",
  "revision": 1, "state_version": 0,
  "root": "summarize", "status": "pending",
  "nodes": {
    "summarize": {"kind":"controller","task":{"title":"裁决覆盖度","description":"完成后调用 submit_task_result 并上报 event=ready"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish","when":{"event":"ready"}}]},
    "finish": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestGraphControllerSubmitTaskResultEventEndToEnd 走完整真实链路：Graph Runtime
// 发布 controller task → Scheduler Agent 认领 → 真实 ToolRegistry dispatch
// submit_task_result → SubmitState/finalization 收尾写 Results[event] →
// graph-terminal-feed 推进 ready 边 → end。它防止 prompt 声称可提交、实际装配
// 却漏掉工具的契约再次分叉。
func TestGraphControllerSubmitTaskResultEventEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeControllerEventGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	controllerTask := mustFindGraphTask(t, env.tasks, "g-bridge-controller-event", "summarize", "summarize@1")
	if controllerTask.EventType != "__scheduler__" {
		t.Fatalf("controller task EventType=%q, want __scheduler__", controllerTask.EventType)
	}

	cfg := config.DefaultConfig()
	cfg.ProjectRoot = t.TempDir()
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}
	fake := &planGateScriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:   "controller-submit-1",
			Name: "submit_task_result",
			Arguments: map[string]any{
				"summary": "五路结果已到齐，覆盖度充分",
				"event":   "ready",
			},
		}},
	}}}
	bundle := scheduler.New(
		env.tasks, roster.NewMemoryRoster(), fake, nil, cfg,
		nil, mailbox.NewRegistry(8), nil, nil, nil, nil, nil, nil, nil, nil,
		io.Discard, io.Discard, nil, env.runtime, env.graphs, nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bundle.Agent.Run(ctx)
	}()
	eventually(t, "Graph controller 应经结构化 event 推进图到 completed", func() bool {
		g, ok := env.graphs.Get("g-bridge-controller-event")
		return ok && g.Status == graph.GraphCompleted
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler Agent 未在取消后退出")
	}

	completedTask, err := env.tasks.GetTask(controllerTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completedTask.Status != model.TaskStatusCompleted {
		t.Fatalf("controller task status=%s, want completed", completedTask.Status)
	}
	if got := completedTask.Results["event"]; got != "ready" {
		t.Fatalf("controller Results[event]=%q, want ready（results=%v）", got, completedTask.Results)
	}
	if fake.calls != 1 {
		t.Fatalf("结构化 finalization 应只需一次 LLM 调用，实际 calls=%d log=%v", fake.calls, fake.callLog)
	}
	graphShardContains(t, env.traceDir, "g-bridge-controller-event", "graph_ended")
}

const bridgeStructuredResultRouterGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-bridge-structured-router",
  "revision": 1, "state_version": 0,
  "root": "judge", "status": "pending",
  "nodes": {
    "judge": {"kind":"controller","task":{"title":"裁决覆盖度","description":"用 result 提交 coverage 等结构化字段"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"route_coverage"}]},
    "route_coverage": {"kind":"router","task":{"title":"按 coverage 分流"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"route_retry","when":{"path":"$.coverage","operator":"eq","value":"gap"}},
        {"to":"coverage_mismatch","when":{"path":"$.coverage","operator":"ne","value":"gap"}}
      ]},
    "route_retry": {"kind":"router","task":{"title":"校验数字字段"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"route_ready","when":{"path":"$.retry_count","operator":"eq","value":2}},
        {"to":"retry_mismatch","when":{"path":"$.retry_count","operator":"ne","value":2}}
      ]},
    "route_ready": {"kind":"router","task":{"title":"校验布尔字段"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"route_score","when":{"path":"$.ready","operator":"eq","value":true}},
        {"to":"ready_mismatch","when":{"path":"$.ready","operator":"ne","value":true}}
      ]},
    "route_score": {"kind":"router","task":{"title":"校验嵌套字段"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"gap_done","when":{"path":"$.metrics.score","operator":"eq","value":0.75}},
        {"to":"score_mismatch","when":{"path":"$.metrics.score","operator":"ne","value":0.75}}
      ]},
    "gap_done": {"kind":"end","task":{"title":"缺口分支"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "coverage_mismatch": {"kind":"end","task":{"title":"coverage 类型错误分支"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "retry_mismatch": {"kind":"end","task":{"title":"retry_count 类型错误分支"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "ready_mismatch": {"kind":"end","task":{"title":"ready 类型错误分支"},"status":"inactive","executor":null,"execution":null,"next":[]},
    "score_mismatch": {"kind":"end","task":{"title":"score 类型错误分支"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestGraphControllerStructuredResultRoutesEndToEnd 证明普通 controller 不靠
// white-box TerminalFact 注入，即可通过 submit_task_result.result 产生真正的
// JSON Result，驱动 router 的 $.coverage 条件。carrier 内同时放入 number、
// bool、嵌套 object，锁住类型保真接线而不把它们降格成字符串。
func TestGraphControllerStructuredResultRoutesEndToEnd(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeStructuredResultRouterGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}

	controllerTask := mustFindGraphTask(t, env.tasks, "g-bridge-structured-router", "judge", "judge@1")
	cfg := config.DefaultConfig()
	cfg.ProjectRoot = t.TempDir()
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}
	fake := &planGateScriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:   "controller-structured-1",
			Name: "submit_task_result",
			Arguments: map[string]any{
				"summary": "发现覆盖缺口",
				"result": map[string]any{
					"coverage":    "gap",
					"retry_count": 2,
					"ready":       true,
					"metrics":     map[string]any{"score": 0.75},
				},
			},
		}},
	}}}
	bundle := scheduler.New(
		env.tasks, roster.NewMemoryRoster(), fake, nil, cfg,
		nil, mailbox.NewRegistry(8), nil, nil, nil, nil, nil, nil, nil, nil,
		io.Discard, io.Discard, nil, env.runtime, env.graphs, nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bundle.Agent.Run(ctx)
	}()
	eventually(t, "自定义 coverage 应驱动 router 命中 gap_done", func() bool {
		g, ok := env.graphs.Get("g-bridge-structured-router")
		if !ok || g.Status != graph.GraphCompleted || g.Nodes["gap_done"].Status != graph.NodeCompleted {
			return false
		}
		for _, id := range []string{"coverage_mismatch", "retry_mismatch", "ready_mismatch", "score_mismatch"} {
			if g.Nodes[id].Status != graph.NodeCancelled {
				return false
			}
		}
		return true
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler Agent 未在取消后退出")
	}

	completedTask, err := env.tasks.GetTask(controllerTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := completedTask.Results[agent.StructuredResultStorageKey]
	structured, err := agent.DecodeStructuredResult(raw)
	if err != nil {
		t.Fatalf("结构化 carrier 未 durable: %v（raw=%q）", err, raw)
	}
	if structured["coverage"] != "gap" || structured["retry_count"] != float64(2) || structured["ready"] != true {
		t.Fatalf("结构化标量未类型保真: %#v", structured)
	}
	metrics, ok := structured["metrics"].(map[string]any)
	if !ok || metrics["score"] != float64(0.75) {
		t.Fatalf("嵌套结构未类型保真: %#v", structured)
	}
	if fake.calls != 1 {
		t.Fatalf("结构化 finalization 应只需一次 LLM 调用，实际 calls=%d log=%v", fake.calls, fake.callLog)
	}
	graphShardContains(t, env.traceDir, "g-bridge-structured-router", "graph_ended")
}
