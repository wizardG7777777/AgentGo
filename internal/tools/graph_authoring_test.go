package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

type simpleGraphAcceptancePass struct{}

func (simpleGraphAcceptancePass) EvaluateProposal(context.Context, graph.ProposalAcceptanceInput) (graph.ProposalAcceptanceDecision, error) {
	return graph.ProposalAcceptanceDecision{Verdict: graph.ProposalAcceptancePass, Ref: "proposal:test"}, nil
}

func newUnifiedGraphEnv(t *testing.T) (GraphAuthoringGroup, *graph.Store, *fakeGraphBoard) {
	t.Helper()
	tasks := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := &model.Task{ID: "scheduler-root", EventType: "__scheduler__", Description: "调查项目并报告结果"}
	if err := taskcontract.Start(task, loopcontract.WorkCoordination, "test/v1"); err != nil {
		t.Fatal(err)
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("scheduler", task.ID); err != nil {
		t.Fatal(err)
	}
	authoring, err := graph.NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	executions, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	board := &fakeGraphBoard{}
	runtime := graph.NewRuntime(executions, board)
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	return GraphAuthoringGroup{Store: authoring, TaskStore: tasks, Holder: &fakeHolder{id: task.ID}, SessionID: func() string { return "s1" }, Compiler: graph.DefinitionCompiler{Policies: catalog, Acceptance: simpleGraphAcceptancePass{}}, Runtime: &graph.AuthoringRuntime{Authoring: authoring, Runtime: runtime}, RouteValidator: fakeRouteValidator{routes: map[string][]string{"": {"read_file", "submit_task_result"}}}, Finalization: &fakeFinalizationNotifier{}}, executions, board
}

func graphCreateArgs() map[string]any {
	return map[string]any{"operation": "create", "request_id": "initial", "contract": map[string]any{"execution_class": "read_only", "deliverables": []any{map[string]any{"id": "report", "kind": "report"}}}, "definition": map[string]any{"root": "work", "nodes": map[string]any{
		"work": map[string]any{"kind": "agent", "task": map[string]any{"title": "调查", "description": "调查并提供结果"}, "contract_bindings": map[string]any{"deliverables": []string{"report"}}, "next": []any{
			map[string]any{"to": "done", "when": map[string]any{"event": "completed"}}, map[string]any{"to": "failed", "when": map[string]any{"event": "failed"}}, map[string]any{"to": "blocked", "when": map[string]any{"event": "blocked"}},
		}},
		"done":    map[string]any{"kind": "end", "end_outcome": "success", "next": []any{}},
		"failed":  map[string]any{"kind": "end", "end_outcome": "failed", "next": []any{}},
		"blocked": map[string]any{"kind": "end", "end_outcome": "blocked", "next": []any{}},
	}}}
}

func appliedGraphID(t *testing.T, g GraphAuthoringGroup, args map[string]any) string {
	t.Helper()
	out, err := g.applyGraphChange(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		GraphID string `json:"graph_id"`
		Status  string `json:"status"`
	}
	if err = json.Unmarshal([]byte(out), &receipt); err != nil || receipt.Status != "applied" || receipt.GraphID == "" {
		t.Fatalf("回执非法：%s %v", out, err)
	}
	return receipt.GraphID
}

func TestGraphToolsCreateValidateCommitInOneCall(t *testing.T) {
	g, executions, board := newUnifiedGraphEnv(t)
	id := appliedGraphID(t, g, graphCreateArgs())
	if _, exists := executions.Get(id); exists || board.count() != 0 {
		t.Fatal("创建不得自动启动")
	}
	if again := appliedGraphID(t, g, graphCreateArgs()); again != id {
		t.Fatal("重复请求创建了新图")
	}
	args := graphCreateArgs()
	args["definition"].(map[string]any)["root"] = "wrong"
	if _, err := g.applyGraphChange(context.Background(), args); err == nil {
		t.Fatal("同一请求标识不能换内容")
	}
	args["request_id"] = "invalid"
	if _, err := g.applyGraphChange(context.Background(), args); err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("非法图应返回原因：%v", err)
	}
	if len(g.Store.ListLatestDefinitions()) != 1 {
		t.Fatal("非法请求污染正式定义")
	}
	for i := 0; i < 2; i++ {
		if _, err := g.controlGraph(context.Background(), map[string]any{"graph_id": id, "action": "start", "expected_revision": 1}); err != nil {
			t.Fatal(err)
		}
	}
	if board.count() != 1 {
		t.Fatalf("重复启动产生了多个任务：%d", board.count())
	}
}

func TestGraphToolsConcurrentCreateIsIdempotent(t *testing.T) {
	g, _, _ := newUnifiedGraphEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.applyGraphChange(context.Background(), graphCreateArgs()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(g.Store.ListLatestDefinitions()) != 1 {
		t.Fatal("并发重试产生重复图")
	}
}

func TestGraphToolsDynamicChangePreservesActiveExecution(t *testing.T) {
	g, executions, board := newUnifiedGraphEnv(t)
	id := appliedGraphID(t, g, graphCreateArgs())
	if _, err := g.controlGraph(context.Background(), map[string]any{"graph_id": id, "action": "start", "expected_revision": 1}); err != nil {
		t.Fatal(err)
	}
	before := board.last()
	node := graphCreateArgs()["definition"].(map[string]any)["nodes"].(map[string]any)["work"].(map[string]any)
	node["id"] = "work"
	node["task"].(map[string]any)["description"] = "未来执行使用新的调查范围"
	args := map[string]any{"operation": "update", "request_id": "update1", "graph_id": id, "expected_revision": 1, "reason": "新增反馈", "in_flight": "preserve", "changes": map[string]any{"upsert_nodes": []any{node}}}
	appliedGraphID(t, g, args)
	current, ok := executions.Get(id)
	if !ok || current.Revision != 2 {
		t.Fatalf("执行图未更新：%+v", current)
	}
	if board.count() != 1 || before.Description == current.Nodes["work"].Task.Description {
		t.Fatal("变更不得重新发布或改写在途任务")
	}
	appliedGraphID(t, g, args)
	args["request_id"] = "conflict"
	if _, err := g.applyGraphChange(context.Background(), args); err == nil {
		t.Fatal("过期 revision 应拒绝")
	}
	if latest, _ := g.Store.LatestDefinition(id); latest.Revision != 2 {
		t.Fatal("冲突请求污染正式版本")
	}
	if _, err := g.controlGraph(context.Background(), map[string]any{"graph_id": id, "action": "cancel", "expected_revision": 2, "reason": "用户取消"}); err != nil {
		t.Fatal(err)
	}
	args["expected_revision"] = 2
	args["request_id"] = "after-cancel"
	if _, err := g.applyGraphChange(context.Background(), args); err == nil {
		t.Fatal("终态图不得复活")
	}
}

func TestGraphToolsReadScopeAndPagination(t *testing.T) {
	g, _, _ := newUnifiedGraphEnv(t)
	id := appliedGraphID(t, g, graphCreateArgs())
	if _, err := g.readGraphDefinition(context.Background(), map[string]any{"graph_id": id, "offset": 1}); err == nil {
		t.Fatal("后续页必须绑定版本")
	}
	out, err := g.readGraphDefinition(context.Background(), map[string]any{"graph_id": id, "revision": 1, "limit": 1})
	if err != nil || !strings.Contains(out, `"has_more": true`) {
		t.Fatalf("分页返回错误：%s %v", out, err)
	}
	g.SessionID = func() string { return "other" }
	if _, err := g.readGraphDefinition(context.Background(), map[string]any{"graph_id": id}); err == nil {
		t.Fatal("不得跨会话读取")
	}
}

func TestGraphToolSchemasExposeOnlyUnifiedEntries(t *testing.T) {
	r := agent.NewToolRegistry()
	GraphAuthoringGroup{}.Register(r)
	if len(r.Names()) != 3 {
		t.Fatalf("编排组只应注册三个入口（request_replan 独立装配）：%v", r.Names())
	}
	for _, d := range r.Defs() {
		if strings.Contains(d.Name, "draft") {
			t.Fatal("模型不应管理内部草案")
		}
	}
}

func TestGraphRouteAndLeaseShareAcceptanceInspectionAuthority(t *testing.T) {
	allowed := []string{"read_file", "inspect_board", "inspect_node", "read_graph_definition", "read_evidence", "submit_task_result"}
	validator := graphRouteValidator{RouteValidator: fakeRouteValidator{routes: map[string][]string{"acceptance.verify": allowed}}}
	node := graph.Node{Kind: graph.KindAcceptance, Capability: &graph.Capability{Tools: allowed}}
	if err := validator.validateRoutes("g-inspection", map[string]graph.Node{"verify": node}, "nodes"); err != nil {
		t.Fatal(err)
	}
	for _, name := range allowed {
		if !agent.IsAcceptanceToolAllowed(name) {
			t.Fatalf("路由与租约工具权威漂移：%s", name)
		}
	}
	for _, name := range []string{"apply_change", "run_shell", "send_message", "request_replan"} {
		if agent.IsAcceptanceToolAllowed(name) {
			t.Fatalf("验收角色泄露行动工具：%s", name)
		}
	}
}
