package bootstrap

// 本文件是 V6 Graph approval 桥（C5c，graph_approval.go）的测试：
// 真实 interaction.Service（内存 Store）+ 真实 graph.Runtime/Store +
// graphBoard，断言批准→approved 转移、拒绝/取消→rejected 转移、requestID
// 记录与恢复不重复请求（含重启后 rearm 补登记）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/interaction"
	"agentgo/internal/store"
)

// graphApprovalEnv 是 approval 桥测试环境：真实 Interaction 服务（内存）+
// 真实图持久化（TempDir）+ 真实 Runtime/公告板桥。
type graphApprovalEnv struct {
	service *interaction.Service
	tasks   *store.MemoryTaskStore
	graphs  *graph.Store
	runtime *graph.Runtime
	gw      *graphApprovalGateway
}

// newGraphApprovalEnv 装配测试环境并注入桥（同 wireGraphApprovalBridge 的
// 生产调用形态）。Windows 纪律：graph Store 先 Close 再让 TempDir 清理。
func newGraphApprovalEnv(t *testing.T) *graphApprovalEnv {
	t.Helper()
	service := interaction.NewService(interaction.NewMemoryStore())
	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	gs, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 graph Store: %v", err)
	}
	rt := graph.NewRuntime(gs, newGraphBoard(tasks))
	gw := wireGraphApprovalBridge(service, rt)
	if gw == nil {
		t.Fatal("wireGraphApprovalBridge 应返回非空网关")
	}
	t.Cleanup(func() { _ = gs.Close() })
	return &graphApprovalEnv{service: service, tasks: tasks, graphs: gs, runtime: rt, gw: gw}
}

// bridgeApprovalGraphJSON 审批分流图：root(agent) → ap(approval)
//
//	--event approved--> ok(agent) --> finish(end)
//	--event rejected--> ng(agent) --> finish(end)
const bridgeApprovalGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-appr-bridge", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"提交变更"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"ap"}]},
    "ap": {"kind":"approval","task":{"title":"批准上线","description":"确认变更可以发布"},"status":"inactive","executor":null,"execution":null,
      "next":[
        {"to":"ok","when":{"event":"approved"}},
        {"to":"ng","when":{"event":"rejected"}}
      ]},
    "ok": {"kind":"agent","task":{"title":"执行上线"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "ng": {"kind":"agent","task":{"title":"打回修改"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// driveRootToApproval 提交图并把 root 跑到终态，返回等待中的审批 Interaction。
func driveRootToApproval(t *testing.T, env *graphApprovalEnv, graphJSON, graphID string) interaction.Request {
	t.Helper()
	doc, err := graph.ParseAndValidate([]byte(graphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
	rootTask := mustFindGraphTask(t, env.tasks, graphID, "root", "root@1")
	if err := env.runtime.OnTaskTerminal(graph.TerminalFact{
		GraphID: graphID, NodeID: "root", ActivationID: "root@1", TaskID: rootTask.ID, Status: graph.NodeCompleted,
	}); err != nil {
		t.Fatalf("root 终态回填应成功: %v", err)
	}

	// approval 节点 waiting 且 requestID 记入 execution。
	g, ok := env.graphs.Get(graphID)
	if !ok {
		t.Fatal("图应存在")
	}
	ap := g.Nodes["ap"]
	if ap.Status != graph.NodeWaiting || ap.Execution == nil || ap.Execution.RequestID == "" {
		t.Fatalf("ap 应 waiting 且 requestID 已记录: status=%s execution=%+v", ap.Status, ap.Execution)
	}

	// Interaction 已创建：purpose/kind/选项/身份元数据全对。
	request, err := env.service.Get(context.Background(), ap.Execution.RequestID)
	if err != nil {
		t.Fatalf("审批 Interaction 应存在: %v", err)
	}
	if request.ID != graphApprovalRequestID(graphID, "ap@1") {
		t.Errorf("requestID 应由 (graph_id, activation_id) 确定性派生: %q", request.ID)
	}
	if request.Purpose != purposeGraphApproval || request.Kind != interaction.KindAuthorization {
		t.Errorf("purpose/kind 不符: %s / %s", request.Purpose, request.Kind)
	}
	if request.State != interaction.StatePending {
		t.Errorf("请求应为 pending，实际 %s", request.State)
	}
	if len(request.Options) != 2 || request.Options[0].ID != "approve" || request.Options[1].ID != "reject" {
		t.Errorf("选项应为 批准/拒绝 两项: %+v", request.Options)
	}
	if request.Metadata["graph_id"] != graphID || request.Metadata["node_id"] != "ap" ||
		request.Metadata["activation_id"] != "ap@1" {
		t.Errorf("身份元数据不符: %+v", request.Metadata)
	}
	if !strings.Contains(request.Prompt, "批准上线") || !strings.Contains(request.Prompt, "确认变更可以发布") {
		t.Errorf("Prompt 应携带节点标题与描述: %q", request.Prompt)
	}
	if request.Resolution.Handler != graphApprovalHandler {
		t.Errorf("Resolution.Handler = %q，应为 %q", request.Resolution.Handler, graphApprovalHandler)
	}
	return request
}

// resolveGraphApproval 模拟用户经两阶段协议回答（TUI/Web 的真实路径：
// BeginResolve 锁定 → 服务端零 effect → Complete 落终态）。
func resolveGraphApproval(t *testing.T, service *interaction.Service, request interaction.Request, optionID, text string) {
	t.Helper()
	ctx := context.Background()
	locked, err := service.BeginResolve(ctx, interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: optionID, Text: text,
	})
	if err != nil {
		t.Fatalf("BeginResolve 应成功: %v", err)
	}
	if _, err := service.Complete(ctx, locked.ID, locked.Version); err != nil {
		t.Fatalf("Complete 应成功: %v", err)
	}
}

// TestGraphApprovalBridgeApproved 批准路径：请求落成 Interaction → 用户批准
// → 决议经终态回调异步回填 → ap completed（event=approved）→ ok 任务发布。
func TestGraphApprovalBridgeApproved(t *testing.T) {
	env := newGraphApprovalEnv(t)
	request := driveRootToApproval(t, env, bridgeApprovalGraphJSON, "g-appr-bridge")

	resolveGraphApproval(t, env.service, request, "approve", "可以上线")
	eventually(t, "批准后 ok 任务应被发布", func() bool {
		return findGraphTask(env.tasks, "g-appr-bridge", "ok", "ok@1") != nil
	})
	if task := findGraphTask(env.tasks, "g-appr-bridge", "ng", "ng@1"); task != nil {
		t.Error("批准不应路由 ng")
	}
	g, _ := env.graphs.Get("g-appr-bridge")
	ap := g.Nodes["ap"]
	if ap.Status != graph.NodeCompleted || !strings.Contains(ap.Execution.ResultRef, `"approved"`) ||
		!strings.Contains(ap.Execution.ResultRef, "可以上线") {
		t.Errorf("ap 应 completed 且 Result 载 approved/文本: status=%s result_ref=%s", ap.Status, ap.Execution.ResultRef)
	}

	// ok 跑完 → 图 completed。
	okTask := mustFindGraphTask(t, env.tasks, "g-appr-bridge", "ok", "ok@1")
	if err := env.runtime.OnTaskTerminal(graph.TerminalFact{
		GraphID: "g-appr-bridge", NodeID: "ok", ActivationID: "ok@1", TaskID: okTask.ID, Status: graph.NodeCompleted,
	}); err != nil {
		t.Fatalf("ok 终态回填应成功: %v", err)
	}
	if g, _ := env.graphs.Get("g-appr-bridge"); g.Status != graph.GraphCompleted {
		t.Errorf("图应为 completed，实际 %s", g.Status)
	}
}

// TestGraphApprovalBridgeRejected 拒绝路径：用户选拒绝（附文本）→ rejected 转移。
func TestGraphApprovalBridgeRejected(t *testing.T) {
	env := newGraphApprovalEnv(t)
	request := driveRootToApproval(t, env, bridgeApprovalGraphJSON, "g-appr-bridge")

	resolveGraphApproval(t, env.service, request, "reject", "风险太大")
	eventually(t, "拒绝后 ng 任务应被发布", func() bool {
		return findGraphTask(env.tasks, "g-appr-bridge", "ng", "ng@1") != nil
	})
	if task := findGraphTask(env.tasks, "g-appr-bridge", "ok", "ok@1"); task != nil {
		t.Error("拒绝不应路由 ok")
	}
	g, _ := env.graphs.Get("g-appr-bridge")
	if ap := g.Nodes["ap"]; ap.Status != graph.NodeCompleted || !strings.Contains(ap.Execution.ResultRef, `"rejected"`) {
		t.Errorf("ap 应 completed 且 Result 载 rejected: status=%s result_ref=%s", ap.Status, ap.Execution.ResultRef)
	}
}

// TestGraphApprovalBridgeCancelled 取消路径：非 resolved 终态映射为 rejected
// 且 text 载明原因。
func TestGraphApprovalBridgeCancelled(t *testing.T) {
	env := newGraphApprovalEnv(t)
	request := driveRootToApproval(t, env, bridgeApprovalGraphJSON, "g-appr-bridge")

	if _, err := env.service.Cancel(context.Background(), request.ID, request.Version, "用户撤销了本次审批"); err != nil {
		t.Fatalf("Cancel 应成功: %v", err)
	}
	eventually(t, "取消后 ng 任务应被发布（取消映射为 rejected）", func() bool {
		return findGraphTask(env.tasks, "g-appr-bridge", "ng", "ng@1") != nil
	})
	g, _ := env.graphs.Get("g-appr-bridge")
	ap := g.Nodes["ap"]
	if ap.Status != graph.NodeCompleted || !strings.Contains(ap.Execution.ResultRef, `"rejected"`) ||
		!strings.Contains(ap.Execution.ResultRef, "取消") || !strings.Contains(ap.Execution.ResultRef, "用户撤销了本次审批") {
		t.Errorf("取消应映射为 rejected 且载明原因: status=%s result_ref=%s", ap.Status, ap.Execution.ResultRef)
	}
}

// TestGraphApprovalBridgeIdempotentRecovery requestID 记录与恢复不重复请求：
// ResumeGraph / 直接 RequestApproval / rearm 三条补发路径全部幂等。
func TestGraphApprovalBridgeIdempotentRecovery(t *testing.T) {
	env := newGraphApprovalEnv(t)
	request := driveRootToApproval(t, env, bridgeApprovalGraphJSON, "g-appr-bridge")

	countPending := func() int {
		requests, err := env.service.List(context.Background(), interaction.Filter{Purpose: purposeGraphApproval})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		return len(requests)
	}
	if got := countPending(); got != 1 {
		t.Fatalf("应只有 1 个审批请求，实际 %d", got)
	}

	// 1) ResumeGraph：execution 已记录 requestID，不重复 RequestApproval。
	if err := env.runtime.ResumeGraph("g-appr-bridge"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	if got := countPending(); got != 1 {
		t.Errorf("ResumeGraph 不应重复创建请求，实际 %d 个", got)
	}

	// 2) 直接 RequestApproval（崩溃窗口后的补发形态）：同 spec 返回同 ID。
	id, err := env.gw.RequestApproval(graph.ApprovalSpec{
		GraphID: "g-appr-bridge", NodeID: "ap", ActivationID: "ap@1", Title: "批准上线",
	})
	if err != nil || id != request.ID {
		t.Errorf("幂等补发应返回原 requestID %q，实际 %q, err=%v", request.ID, id, err)
	}
	if got := countPending(); got != 1 {
		t.Errorf("幂等补发不应新建请求，实际 %d 个", got)
	}

	// 3) 进程内索引丢失的新网关（重启形态）：service.Get 兜底去重。
	gw2 := newGraphApprovalGateway(env.service, env.runtime)
	id2, err := gw2.RequestApproval(graph.ApprovalSpec{
		GraphID: "g-appr-bridge", NodeID: "ap", ActivationID: "ap@1", Title: "批准上线",
	})
	if err != nil || id2 != request.ID {
		t.Errorf("新网关应经 Get 去重返回原 requestID %q，实际 %q, err=%v", request.ID, id2, err)
	}
	if got := countPending(); got != 1 {
		t.Errorf("Get 去重不应新建请求，实际 %d 个", got)
	}

	// 4) rearm：请求仍在（非终态）→ 不重复创建。
	rearmPendingGraphApprovals(env.graphs, env.gw)
	if got := countPending(); got != 1 {
		t.Errorf("rearm 不应重复创建非终态请求，实际 %d 个", got)
	}
}

// TestGraphApprovalBridgeRearmAfterRestart 重启形态端到端：新 Store（同目录
// Recover）+ 新 Interaction 服务 + 新 Runtime；resume 不重复请求（requestID
// 已 durable），rearm 以确定性 requestID 在新服务里补登记同一请求；用户随后
// 批准，决议回填新 Runtime，图继续推进到 completed。
func TestGraphApprovalBridgeRearmAfterRestart(t *testing.T) {
	// 第一世：跑到 ap waiting 并记录 requestID，然后「进程结束」。
	dir := t.TempDir()
	gs1, err := graph.NewStore(dir)
	if err != nil {
		t.Fatalf("创建 graph Store: %v", err)
	}
	svc1 := interaction.NewService(interaction.NewMemoryStore())
	tasks1 := store.NewMemoryTaskStore(nil, 100, 1, 300)
	rt1 := graph.NewRuntime(gs1, newGraphBoard(tasks1))
	if wireGraphApprovalBridge(svc1, rt1) == nil {
		t.Fatal("装配桥应成功")
	}
	doc, err := graph.ParseAndValidate([]byte(bridgeApprovalGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := rt1.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
	rootTask := mustFindGraphTask(t, tasks1, "g-appr-bridge", "root", "root@1")
	if err := rt1.OnTaskTerminal(graph.TerminalFact{
		GraphID: "g-appr-bridge", NodeID: "root", ActivationID: "root@1", TaskID: rootTask.ID, Status: graph.NodeCompleted,
	}); err != nil {
		t.Fatalf("root 终态回填应成功: %v", err)
	}
	g1, _ := gs1.Get("g-appr-bridge")
	requestID := g1.Nodes["ap"].Execution.RequestID
	if requestID == "" {
		t.Fatal("requestID 应已 durable")
	}
	if err := gs1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 第二世：同目录 Recover；Interaction 服务全新（内存不跨重启）。
	gs2, err := graph.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(重启): %v", err)
	}
	t.Cleanup(func() { _ = gs2.Close() })
	if err := gs2.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	svc2 := interaction.NewService(interaction.NewMemoryStore())
	tasks2 := store.NewMemoryTaskStore(nil, 100, 1, 300)
	rt2 := graph.NewRuntime(gs2, newGraphBoard(tasks2))
	gw2 := wireGraphApprovalBridge(svc2, rt2)
	if err := rt2.ResumeGraph("g-appr-bridge"); err != nil {
		t.Fatalf("ResumeGraph 应成功: %v", err)
	}
	// resume 不重复请求（requestID 已记录）——此时新服务里还没有任何请求。
	if requests, _ := svc2.List(context.Background(), interaction.Filter{Purpose: purposeGraphApproval}); len(requests) != 0 {
		t.Fatalf("resume 不应在新服务里创建请求，实际 %d 个", len(requests))
	}
	// rearm 补登记：同确定性 requestID 的请求出现在新服务里。
	rearmPendingGraphApprovals(gs2, gw2)
	request, err := svc2.Get(context.Background(), requestID)
	if err != nil {
		t.Fatalf("rearm 应以原 requestID 补登记请求: %v", err)
	}
	if request.State != interaction.StatePending {
		t.Errorf("补登记的请求应为 pending，实际 %s", request.State)
	}

	// 用户在新一世里批准 → 决议回填新 Runtime → 图推进到 completed。
	resolveGraphApproval(t, svc2, request, "approve", "")
	eventually(t, "重启后批准应发布 ok 任务", func() bool {
		return findGraphTask(tasks2, "g-appr-bridge", "ok", "ok@1") != nil
	})
	okTask := mustFindGraphTask(t, tasks2, "g-appr-bridge", "ok", "ok@1")
	if err := rt2.OnTaskTerminal(graph.TerminalFact{
		GraphID: "g-appr-bridge", NodeID: "ok", ActivationID: "ok@1", TaskID: okTask.ID, Status: graph.NodeCompleted,
	}); err != nil {
		t.Fatalf("ok 终态回填应成功: %v", err)
	}
	if g, _ := gs2.Get("g-appr-bridge"); g.Status != graph.GraphCompleted {
		t.Errorf("重启恢复后图应能走完到 completed，实际 %s", g.Status)
	}
}

// TestGraphApprovalBridgeIgnoresUnrelated 终态回调只处理 purpose=
// graph_approval 的请求；其它 purpose 的决议不触达 Runtime。
func TestGraphApprovalBridgeIgnoresUnrelated(t *testing.T) {
	env := newGraphApprovalEnv(t)
	other, err := env.service.Create(context.Background(), interaction.CreateRequest{
		Kind:    interaction.KindChoice,
		Purpose: "agent_question",
		Prompt:  "随便一个问题",
		Options: []interaction.Option{{ID: "a", Label: "A"}},
		Resolution: interaction.ResolutionSpec{
			Handler: "agent_response",
		},
	})
	if err != nil {
		t.Fatalf("创建无关请求: %v", err)
	}
	resolveGraphApproval(t, env.service, other, "a", "")
	// 无断言失败即通过——不相关的决议不会 panic、不会误回填（Runtime 里
	// 根本没有对应 activation，OnApprovalDecided 也只会 debug 忽略）。
	time.Sleep(20 * time.Millisecond) // 给异步回填 goroutine 一个出错窗口
	if g, ok := env.graphs.Get("g-appr-bridge"); ok {
		t.Errorf("不应存在图，实际 %+v", g)
	}
}

// 静态断言：graphApprovalGateway 实现 graph.ApprovalGateway。
var _ graph.ApprovalGateway = (*graphApprovalGateway)(nil)
