package tools

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// fakeGraphBoard 实现 graph.TaskBoard，记录收到的发布 spec（含幂等去重）。
type fakeGraphBoard struct {
	mu    sync.Mutex
	specs []graph.TaskSpec
	seq   int
}

func (b *fakeGraphBoard) PublishGraphTask(spec graph.TaskSpec) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.specs = append(b.specs, spec)
	return "task-" + spec.ActivationID, nil
}

func (b *fakeGraphBoard) LookupGraphTask(graphID, activationID, _ string) (graph.GraphTaskSnapshot, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, spec := range b.specs {
		if spec.GraphID == graphID && spec.ActivationID == activationID {
			return graph.GraphTaskSnapshot{TaskID: "task-" + spec.ActivationID, NodeKind: spec.NodeKind}, true, nil
		}
	}
	return graph.GraphTaskSnapshot{}, false, nil
}

func (b *fakeGraphBoard) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.specs)
}

func (b *fakeGraphBoard) last() graph.TaskSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.specs[len(b.specs)-1]
}

// newGraphControlEnv 构造带真实 graph.Store/Runtime 的 GraphControlGroup。
// Windows 纪律：t.Cleanup 先 Close Store（释放 journal 句柄）再让 TempDir 清理。
func newGraphControlEnv(t *testing.T) (GraphControlGroup, *graph.Store, *fakeGraphBoard) {
	t.Helper()
	gs, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 graph Store: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	board := &fakeGraphBoard{}
	rt := graph.NewRuntime(gs, board)
	return GraphControlGroup{
		Runtime: rt, Store: gs,
		RouteValidator: fakeRouteValidator{routes: map[string][]string{"": {}}},
	}, gs, board
}

// graphToolGraphJSON 最小合法图：root(controller) → implement(agent) → finish(end)。
const graphToolGraphJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g-tool-basic",
  "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"controller","task":{"title":"分解与裁决"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"implement"}]},
    "implement": {"kind":"agent","task":{"title":"实施修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// submit_graph 成功路径：校验 → 提交 → root 激活并发布任务（controller 路由
// 到 __scheduler__），返回值携带 graph_id 与 root activation。
func TestSubmitGraphSuccess(t *testing.T) {
	g, gs, board := newGraphControlEnv(t)
	finalizer := &graphFinalizationRecorder{}
	g.FinalizationNotifier = finalizer
	reply, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON})
	if err != nil {
		t.Fatalf("submit_graph 应成功: %v", err)
	}
	if !strings.Contains(reply, "graph_id=g-tool-basic") || !strings.Contains(reply, "root_activation=root@1") {
		t.Errorf("返回值应含 graph_id 与 root activation，实际：%s", reply)
	}
	if board.count() != 1 {
		t.Fatalf("root 激活应发布 1 个任务，实际 %d", board.count())
	}
	spec := board.last()
	if spec.NodeID != "root" || spec.Route != graph.RouteScheduler {
		t.Errorf("root（controller）任务路由 = %q，应为 %q: %+v", spec.Route, graph.RouteScheduler, spec)
	}
	doc, ok := gs.Get("g-tool-basic")
	if !ok || doc.Revision != 1 || doc.Status != graph.GraphRunning {
		t.Errorf("图应已 durable 为 revision=1 running: ok=%v doc=%+v", ok, doc)
	}
	if !finalizer.marked {
		t.Fatal("submit_graph 完整成功后必须标记 origin Scheduler task finalizing")
	}
}

type graphFinalizationRecorder struct{ marked bool }

func (r *graphFinalizationRecorder) MarkTaskFinalized() { r.marked = true }

// submit_graph 校验失败：返回「图校验失败」+ 校验阶段/路径的中文错误。
func TestSubmitGraphValidationFailure(t *testing.T) {
	g, _, board := newGraphControlEnv(t)
	finalizer := &graphFinalizationRecorder{}
	g.FinalizationNotifier = finalizer
	bad := `{"schema":"agentgo.graph/v1","graph_id":"g-bad","revision":1,"state_version":0,"root":"ghost","status":"pending","nodes":{}}`
	_, err := g.submitGraph(context.Background(), map[string]any{"graph": bad})
	if err == nil {
		t.Fatal("root 指向不存在节点应拒绝")
	}
	if !strings.Contains(err.Error(), "图校验失败") || !strings.Contains(err.Error(), "校验[") {
		t.Errorf("错误应含「图校验失败」与校验阶段，实际：%v", err)
	}
	if board.count() != 0 {
		t.Error("校验失败不应发布任何任务")
	}
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": "  "}); err == nil {
		t.Error("空 graph 参数应报错")
	}
	if finalizer.marked {
		t.Fatal("submit_graph 失败时不得标记 finalizing，Scheduler 必须仍可修正重试")
	}
}

func TestSubmitGraphValidatesGraphScopedRoutesBeforePersistence(t *testing.T) {
	g, gs, board := newGraphControlEnv(t)
	teamGraph := strings.Replace(graphToolGraphJSON,
		`"implement": {"kind":"agent","task":{"title":"实施修改"},`,
		`"implement": {"kind":"agent","task":{"title":"实施修改"},"metadata":{"route":"team:impl"},"capability":{"tools":["read_file"]},`, 1)
	g.RouteValidator = fakeRouteValidator{
		routes:      map[string][]string{"team:impl": {"read_file", "submit_task_result"}},
		ownerScopes: map[string]string{"team:impl": model.GraphRouteScope("g-tool-basic")},
	}
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": teamGraph}); err != nil {
		t.Fatalf("same-Graph Team route should pass: %v", err)
	}
	if board.count() != 1 {
		t.Fatalf("root should activate after route validation, count=%d", board.count())
	}

	other, otherStore, otherBoard := newGraphControlEnv(t)
	other.RouteValidator = fakeRouteValidator{
		routes:      map[string][]string{"team:impl": {"read_file"}},
		ownerScopes: map[string]string{"team:impl": model.GraphRouteScope("g-other")},
	}
	if _, err := other.submitGraph(context.Background(), map[string]any{"graph": teamGraph}); err == nil || !strings.Contains(err.Error(), "图路由校验失败") {
		t.Fatalf("cross-Graph route err=%v", err)
	}
	if _, ok := otherStore.Get("g-tool-basic"); ok || otherBoard.count() != 0 {
		t.Fatal("route rejection persisted or activated the Graph")
	}

	_ = gs // keep the successful Store authority explicit in this test.
}

func TestSubmitGraphFailsClosedWithoutRouteAuthority(t *testing.T) {
	g, gs, board := newGraphControlEnv(t)
	g.RouteValidator = nil
	_, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON})
	if err == nil || !strings.Contains(err.Error(), "fail-closed") || !strings.Contains(err.Error(), "runtime route") {
		t.Fatalf("missing route authority err=%v", err)
	}
	if _, ok := gs.Get("g-tool-basic"); ok || board.count() != 0 {
		t.Fatal("fail-closed route rejection persisted or activated graph")
	}
}

func TestSubmitGraphAcceptanceCapabilityIsStructurallyReadOnly(t *testing.T) {
	const base = `{
  "schema":"agentgo.graph/v1","graph_id":"g-acceptance-cap","revision":1,"state_version":0,
  "root":"verify","status":"pending","nodes":{
    "verify":{"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"pass"}}]},
    "done":{"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }}`

	for _, denied := range []string{
		"run_shell", "write_file", "edit_file", "publish_task",
		"send_message", "request_user_input", "request_replan",
	} {
		denied := denied
		t.Run("route 含闭集外工具 "+denied+" 时拒绝", func(t *testing.T) {
			g, gs, board := newGraphControlEnv(t)
			g.RouteValidator = fakeRouteValidator{routes: map[string][]string{
				graph.RouteAcceptance: {"read_file", denied, "submit_task_result"},
			}}
			_, err := g.submitGraph(context.Background(), map[string]any{"graph": base})
			if err == nil || !strings.Contains(err.Error(), denied) || !strings.Contains(err.Error(), "只读闭集外") {
				t.Fatalf("acceptance 含 %s 应 fail-closed，实际 err=%v", denied, err)
			}
			if _, ok := gs.Get("g-acceptance-cap"); ok || board.count() != 0 {
				t.Fatal("能力拒绝后不应持久化或激活 Graph")
			}
		})
	}

	t.Run("route 交集安全但任一 listener 可能越权时按并集拒绝", func(t *testing.T) {
		g, gs, board := newGraphControlEnv(t)
		g.RouteValidator = fakeRouteValidator{
			routes: map[string][]string{
				graph.RouteAcceptance: {"read_file", "submit_task_result"},
			},
			envelopes: map[string][]string{
				graph.RouteAcceptance: {"read_file", "send_message", "submit_task_result"},
			},
		}
		_, err := g.submitGraph(context.Background(), map[string]any{"graph": base})
		if err == nil || !strings.Contains(err.Error(), "send_message") || !strings.Contains(err.Error(), "只读闭集外") {
			t.Fatalf("acceptance 应按 listener 能力并集拒绝隐藏的副作用工具，实际 err=%v", err)
		}
		if _, ok := gs.Get("g-acceptance-cap"); ok || board.count() != 0 {
			t.Fatal("能力并集拒绝后不应持久化或激活 Graph")
		}
	})

	t.Run("per-node capability 排除 Shell 后允许", func(t *testing.T) {
		g, _, board := newGraphControlEnv(t)
		g.RouteValidator = fakeRouteValidator{routes: map[string][]string{
			graph.RouteAcceptance: {"read_file", "run_shell", "submit_task_result"},
		}}
		narrowed := strings.Replace(base,
			`"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"}`,
			`"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"},"capability":{"tools":["read_file","submit_task_result"]}`,
			1)
		if _, err := g.submitGraph(context.Background(), map[string]any{"graph": narrowed}); err != nil {
			t.Fatalf("节点工具面已收窄为只读时应允许：%v", err)
		}
		if board.count() != 1 {
			t.Fatalf("root acceptance 应发布一项任务，实际 %d", board.count())
		}
	})

	t.Run("per-node capability 不得裁掉 verdict 提交工具", func(t *testing.T) {
		g, gs, board := newGraphControlEnv(t)
		g.RouteValidator = fakeRouteValidator{routes: map[string][]string{
			graph.RouteAcceptance: {"read_file", "submit_task_result"},
		}}
		withoutSubmit := strings.Replace(base,
			`"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"}`,
			`"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"},"capability":{"tools":["read_file"]}`,
			1)
		_, err := g.submitGraph(context.Background(), map[string]any{"graph": withoutSubmit})
		if err == nil || !strings.Contains(err.Error(), "实际工具面缺少 submit_task_result") {
			t.Fatalf("节点裁掉 verdict 提交工具应 fail-closed，实际 err=%v", err)
		}
		if _, ok := gs.Get("g-acceptance-cap"); ok || board.count() != 0 {
			t.Fatal("能力拒绝后不应持久化或激活 Graph")
		}
	})

	t.Run("Scheduler 不能兼任 acceptance", func(t *testing.T) {
		g, _, _ := newGraphControlEnv(t)
		g.RouteValidator = fakeRouteValidator{routes: map[string][]string{
			graph.RouteAcceptance: {"read_file", "submit_task_result"},
		}}
		schedulerRoute := strings.Replace(base,
			`"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"}`,
			`"kind":"acceptance","task":{"title":"独立验收","description":"判据：读取交付物并确认内容满足任务要求"},"metadata":{"route":"__scheduler__"}`,
			1)
		_, err := g.submitGraph(context.Background(), map[string]any{"graph": schedulerRoute})
		if err == nil || !strings.Contains(err.Error(), "不得路由到 Scheduler") {
			t.Fatalf("Scheduler acceptance 应拒绝，实际 err=%v", err)
		}
	})
}

func TestGraphControllerControlPlaneIsBoundToCurrentGraph(t *testing.T) {
	g, _, _ := newGraphControlEnv(t)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON}); err != nil {
		t.Fatal(err)
	}
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	current := &model.Task{ID: "graph-controller", EventType: graph.RouteScheduler, GraphID: "g-tool-basic"}
	if err := tasks.PublishTask(current); err != nil {
		t.Fatal(err)
	}
	g.TaskStore = tasks
	g.Holder = &fakeHolder{id: current.ID}

	if _, err := g.readGraph(context.Background(), map[string]any{"graph_id": current.GraphID}); err != nil {
		t.Fatalf("same-Graph read should pass: %v", err)
	}
	if _, err := g.readGraph(context.Background(), map[string]any{"graph_id": "g-other"}); err == nil || !strings.Contains(err.Error(), "target_graph_id=g-other") {
		t.Fatalf("cross-Graph read err=%v", err)
	}
	if _, err := g.patchGraph(context.Background(), map[string]any{
		"graph_id": "g-other", "base_revision": 1, "patch": `{"upsert_nodes":[]}`,
	}); err == nil || !strings.Contains(err.Error(), "target_graph_id=g-other") {
		t.Fatalf("cross-Graph patch err=%v", err)
	}
	otherGraph := strings.Replace(graphToolGraphJSON, "g-tool-basic", "g-other", 1)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": otherGraph}); err == nil || !strings.Contains(err.Error(), "Graph controller 禁止 submit_graph") {
		t.Fatalf("Graph controller new submit err=%v", err)
	}
}

func TestFinalReportReadGraphIsBoundToFrozenGraph(t *testing.T) {
	g, _, _ := newGraphControlEnv(t)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON}); err != nil {
		t.Fatal(err)
	}
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	current := newFinalReportTestTask(t, "final-report", "g-tool-basic")
	current.EventType = graph.RouteScheduler
	if err := tasks.PublishTask(current); err != nil {
		t.Fatal(err)
	}
	g.TaskStore, g.Holder = tasks, &fakeHolder{id: current.ID}
	if _, err := g.readGraph(context.Background(), map[string]any{"graph_id": current.FinalReportGraphID}); err != nil {
		t.Fatalf("final-report 应能读取冻结 Graph: %v", err)
	}
	if _, err := g.readGraph(context.Background(), map[string]any{"graph_id": "g-other"}); err == nil || !strings.Contains(err.Error(), "final_report_graph_id") {
		t.Fatalf("final-report 跨 Graph read 必须拒绝: %v", err)
	}
}

func TestSubmitGraphRejectsParentGraphTeamRouteInsideInlineSubgraph(t *testing.T) {
	g, gs, board := newGraphControlEnv(t)
	g.RouteValidator = fakeRouteValidator{
		routes:      map[string][]string{"team:child": {"read_file"}},
		ownerScopes: map[string]string{"team:child": model.GraphRouteScope("g-inline-team")},
	}
	raw := `{
  "schema":"agentgo.graph/v1","graph_id":"g-inline-team","revision":1,"state_version":0,
  "root":"nested","status":"pending","nodes":{
    "nested":{"kind":"subgraph","task":{"title":"nested"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{"root":"work","nodes":{
        "work":{"kind":"agent","task":{"title":"child work"},"status":"inactive","executor":null,"execution":null,
          "metadata":{"route":"team:child"},"capability":{"tools":["read_file"]},"next":[{"to":"end"}]},
        "end":{"kind":"end","task":{"title":"child end"},"status":"inactive","executor":null,"execution":null,"next":[]}
      }},"next":[{"to":"done"}]},
    "done":{"kind":"end","task":{"title":"done"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`
	_, err := g.submitGraph(context.Background(), map[string]any{"graph": raw})
	if err == nil || !strings.Contains(err.Error(), "内联 subgraph") || !strings.Contains(err.Error(), "不继承父 Graph") {
		t.Fatalf("parent-Graph Team route in inline subgraph err=%v", err)
	}
	if _, ok := gs.Get("g-inline-team"); ok || board.count() != 0 {
		t.Fatal("inline subgraph route rejection persisted or activated the Graph")
	}
}

func TestSubmitGraphAllowsGlobalRouteInsideInlineSubgraph(t *testing.T) {
	g, _, board := newGraphControlEnv(t)
	g.RouteValidator = fakeRouteValidator{
		routes:      map[string][]string{"global:reader": {"read_file"}},
		ownerScopes: map[string]string{"global:reader": ""},
	}
	raw := `{
  "schema":"agentgo.graph/v1","graph_id":"g-inline-global","revision":1,"state_version":0,
  "root":"nested","status":"pending","nodes":{
    "nested":{"kind":"subgraph","task":{"title":"nested"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{"root":"work","nodes":{
        "work":{"kind":"agent","task":{"title":"child work"},"status":"inactive","executor":null,"execution":null,
          "metadata":{"route":"global:reader"},"capability":{"tools":["read_file"]},"next":[{"to":"end"}]},
        "end":{"kind":"end","task":{"title":"child end"},"status":"inactive","executor":null,"execution":null,"next":[]}
      }},"next":[{"to":"done"}]},
    "done":{"kind":"end","task":{"title":"done"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": raw}); err != nil {
		t.Fatalf("global inline-subgraph route should pass: %v", err)
	}
	if board.count() != 1 || board.last().GraphID != "g-inline-global/nested@1" {
		t.Fatalf("inline subgraph root task mismatch: count=%d last=%+v", board.count(), board.last())
	}
}

// submit_graph 非注入上下文（非 Scheduler 装配）：返回明确中文错误。
func TestSubmitGraphNotInjected(t *testing.T) {
	g := GraphControlGroup{}
	_, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON})
	if err == nil || !strings.Contains(err.Error(), "未注入") {
		t.Errorf("未注入 Runtime 应返回明确错误，实际：%v", err)
	}
	if _, err := (GraphControlGroup{}).patchGraph(context.Background(), map[string]any{
		"graph_id": "g-x", "base_revision": 1, "patch": `{"remove_nodes":["a"]}`,
	}); err == nil || !strings.Contains(err.Error(), "未注入") {
		t.Errorf("未注入 Runtime 应返回明确错误，实际：%v", err)
	}
	if _, err := (GraphControlGroup{}).readGraph(context.Background(), map[string]any{
		"graph_id": "g-x",
	}); err == nil || !strings.Contains(err.Error(), "未注入") {
		t.Errorf("未注入 Store 时 read_graph 应返回明确错误，实际：%v", err)
	}
}

// TestReadGraphReturnsCurrentAuthority 验证 Scheduler 有受支持的当前图读取面：
// 返回值包含 CAS 所需 revision 与 Runtime 写入的 activation/task_id，未知图
// 和空 graph_id 给出明确错误。
func TestReadGraphReturnsCurrentAuthority(t *testing.T) {
	g, _, _ := newGraphControlEnv(t)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON}); err != nil {
		t.Fatalf("先提交图: %v", err)
	}
	reply, err := g.readGraph(context.Background(), map[string]any{"graph_id": "g-tool-basic"})
	if err != nil {
		t.Fatalf("read_graph 应成功: %v", err)
	}
	for _, want := range []string{`"graph_id": "g-tool-basic"`, `"revision": 1`, `"status": "running"`, `"activation_id": "root@1"`, `"task_id": "task-root@1"`} {
		if !strings.Contains(reply, want) {
			t.Errorf("read_graph 返回缺少 %s：%s", want, reply)
		}
	}
	if _, err := g.readGraph(context.Background(), map[string]any{"graph_id": "missing"}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("未知图应报不存在，实际：%v", err)
	}
	if _, err := g.readGraph(context.Background(), map[string]any{"graph_id": " "}); err == nil {
		t.Error("空 graph_id 应报错")
	}
}

func TestReadGraphProjectsTypedTerminalOutcome(t *testing.T) {
	for _, outcome := range []string{"failed", "blocked", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			g, _, _ := newGraphControlEnv(t)
			graphID := "g-read-outcome-" + outcome
			raw := strings.Replace(graphToolGraphJSON, "g-tool-basic", graphID, 1)
			raw = strings.Replace(raw,
				`"kind":"end","task":{"title":"收官"}`,
				`"kind":"end","task":{"title":"收官"},"end_outcome":"`+outcome+`"`, 1)
			if _, err := g.submitGraph(context.Background(), map[string]any{"graph": raw}); err != nil {
				t.Fatalf("提交 typed outcome 图: %v", err)
			}
			for _, fact := range []graph.TerminalFact{
				{GraphID: graphID, NodeID: "root", ActivationID: "root@1", TaskID: "task-root@1", Status: graph.NodeCompleted},
				{GraphID: graphID, NodeID: "implement", ActivationID: "implement@1", TaskID: "task-implement@1", Status: graph.NodeCompleted},
			} {
				if err := g.Runtime.OnTaskTerminal(fact); err != nil {
					t.Fatalf("推进图到 typed end: %v", err)
				}
			}
			reply, err := g.readGraph(context.Background(), map[string]any{"graph_id": graphID})
			if err != nil {
				t.Fatalf("read_graph: %v", err)
			}
			for _, want := range []string{
				`"status": "` + outcome + `"`,
				`"outcome": {`, `"outcome": "` + outcome + `"`, `"source": "end"`,
			} {
				if !strings.Contains(reply, want) {
					t.Errorf("read_graph 缺少 %s：%s", want, reply)
				}
			}
		})
	}
}

// patch_graph 成功路径：upsert 节点定义 → new_revision=2，且只影响未来转移
// 求值（在途 root activation 不受影响）；成功后 emit graph_revision_committed
// 审计事件（C5d，载 new_revision 与 patch 摘要）。
func TestPatchGraphSuccess(t *testing.T) {
	g, gs, _ := newGraphControlEnv(t)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON}); err != nil {
		t.Fatalf("先提交图: %v", err)
	}
	d := installGraphTraceCapture(t)
	patch := `{"upsert_nodes":[{"id":"implement","kind":"agent","task":{"title":"实施修改 v2"},"next":[{"to":"finish"}]}]}`
	reply, err := g.patchGraph(context.Background(), map[string]any{
		"graph_id": "g-tool-basic", "base_revision": 1, "patch": patch,
	})
	if err != nil {
		t.Fatalf("patch_graph 应成功: %v", err)
	}
	if !strings.Contains(reply, "new_revision=2") {
		t.Errorf("返回值应含 new_revision=2，实际：%s", reply)
	}
	doc, _ := gs.Get("g-tool-basic")
	if doc.Revision != 2 || doc.Nodes["implement"].Task.Title != "实施修改 v2" {
		t.Errorf("定义面应已更新到 revision=2: %+v", doc.Nodes["implement"])
	}
	events := d.ofKind(trace.KindGraphRevisionCommitted)
	if len(events) != 1 {
		t.Fatalf("patch 成功应 emit 1 条 graph_revision_committed，实际 %d", len(events))
	}
	ev := events[0]
	if ev.GraphID != "g-tool-basic" || ev.TaskID != "" {
		t.Errorf("事件应归属图分片（GraphID 非空、TaskID 为空）: %+v", ev)
	}
	if !strings.Contains(ev.Description, "new_revision=2") || !strings.Contains(ev.Description, "upsert=[implement]") {
		t.Errorf("事件描述应载 new_revision 与 patch 摘要，实际：%q", ev.Description)
	}
}

// patch_graph 冲突路径：base_revision 过期时返回含当前 revision 的中文冲突
// 错误（提示重新读取最新图）。
func TestPatchGraphRevisionConflict(t *testing.T) {
	g, _, _ := newGraphControlEnv(t)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON}); err != nil {
		t.Fatalf("先提交图: %v", err)
	}
	patch := `{"upsert_nodes":[{"id":"implement","kind":"agent","task":{"title":"v2"},"next":[{"to":"finish"}]}]}`
	args := map[string]any{"graph_id": "g-tool-basic", "base_revision": 1, "patch": patch}
	if _, err := g.patchGraph(context.Background(), args); err != nil {
		t.Fatalf("首次 patch 应成功: %v", err)
	}
	_, err := g.patchGraph(context.Background(), args) // 同 base_revision 重放
	if err == nil {
		t.Fatal("过期 base_revision 应冲突")
	}
	if !strings.Contains(err.Error(), "冲突") || !strings.Contains(err.Error(), "当前 revision=2") ||
		!strings.Contains(err.Error(), "重新读取最新图") {
		t.Errorf("冲突错误应含当前 revision 与重读提示，实际：%v", err)
	}
	if _, err := g.patchGraph(context.Background(), map[string]any{
		"graph_id": "g-tool-basic", "base_revision": 0, "patch": patch,
	}); err == nil {
		t.Error("base_revision<=0 应报错")
	}
	if _, err := g.patchGraph(context.Background(), map[string]any{
		"graph_id": "g-tool-basic", "base_revision": 2, "patch": "{不是json",
	}); err == nil || !strings.Contains(err.Error(), "解码失败") {
		t.Errorf("非法 patch JSON 应报解码错误，实际：%v", err)
	}
}

func TestPatchGraphRejectsCrossGraphTeamRouteWithoutRevisionChange(t *testing.T) {
	g, gs, _ := newGraphControlEnv(t)
	if _, err := g.submitGraph(context.Background(), map[string]any{"graph": graphToolGraphJSON}); err != nil {
		t.Fatal(err)
	}
	g.RouteValidator = fakeRouteValidator{
		routes:      map[string][]string{"team:foreign": {"read_file"}},
		ownerScopes: map[string]string{"team:foreign": model.GraphRouteScope("g-other")},
	}
	patch := `{"upsert_nodes":[{"id":"implement","kind":"agent","task":{"title":"foreign"},"capability":{"tools":["read_file"]},"metadata":{"route":"team:foreign"},"next":[{"to":"finish"}]}]}`
	if _, err := g.patchGraph(context.Background(), map[string]any{
		"graph_id": "g-tool-basic", "base_revision": 1, "patch": patch,
	}); err == nil || !strings.Contains(err.Error(), "图路由校验失败") {
		t.Fatalf("cross-Graph patch err=%v", err)
	}
	if doc, _ := gs.Get("g-tool-basic"); doc.Revision != 1 {
		t.Fatalf("rejected route patch changed revision to %d", doc.Revision)
	}
}

// GraphControlGroup 注册守护：三个工具无条件注册（nil 依赖也在册）。
func TestGraphControlGroupRegistersAllTools(t *testing.T) {
	reg := agent.NewToolRegistry()
	GraphControlGroup{}.Register(reg)
	registered := map[string]bool{}
	for _, d := range reg.Defs() {
		registered[d.Name] = true
	}
	for _, name := range []string{"submit_graph", "read_graph", "patch_graph"} {
		if !registered[name] {
			t.Errorf("工具 %s 应注册", name)
		}
	}
}

// legacy submit_graph 的语法错误只返回事实，不再建议提交可立即执行的半成品
// root/end 图；新图分批构造由 GraphDraft 工具承担。
func TestSubmitGraphSyntaxFailureDoesNotSuggestExecutableSkeleton(t *testing.T) {
	g, _, _ := newGraphControlEnv(t)
	// 语法损坏的图 JSON（节点定义半截截断）
	broken := `{"schema":"agentgo.graph/v1","graph_id":"g-broken","revision":1,"state_version":0,"root":"work","status":"pending","nodes":{"work":{"kind":"agent","task":{"title":"执行`
	_, err := g.submitGraph(context.Background(), map[string]any{"graph": broken})
	if err == nil {
		t.Fatal("语法损坏应拒绝")
	}
	if !strings.Contains(err.Error(), "JSON语法") {
		t.Fatalf("应报 JSON 语法阶段: %v", err)
	}
	if strings.Contains(err.Error(), "骨架") || strings.Contains(err.Error(), "root+end") {
		t.Fatalf("legacy 错误不得再建议可执行半成品图: %v", err)
	}
}

func TestGraphControlGroupCanHideLegacySubmit(t *testing.T) {
	registry := agent.NewToolRegistry()
	GraphControlGroup{DisableSubmitGraph: true}.Register(registry)
	names := registry.Names()
	if slices.Contains(names, "submit_graph") || !slices.Contains(names, "read_graph") || !slices.Contains(names, "patch_graph") {
		t.Fatalf("新 root registry 应隐藏 submit、保留 runtime read/patch compatibility: %v", names)
	}
}
