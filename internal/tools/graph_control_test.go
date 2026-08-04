package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
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
			return graph.GraphTaskSnapshot{TaskID: "task-" + spec.ActivationID}, true, nil
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
	return GraphControlGroup{Runtime: rt, Store: gs}, gs, board
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
}

// submit_graph 校验失败：返回「图校验失败」+ 校验阶段/路径的中文错误。
func TestSubmitGraphValidationFailure(t *testing.T) {
	g, _, board := newGraphControlEnv(t)
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
