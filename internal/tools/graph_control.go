package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// GraphControlGroup 注册 V6 Graph 的 Scheduler 控制面工具（C5b）：
//   - submit_graph：提交一张 JSON GraphDocument 作为多节点编排的执行契约并激活 root；
//   - read_graph：读取当前权威 revision/state/runtime 快照；
//   - patch_graph：以 base_revision CAS 应用定义面补丁（DefinitionPatch）。
//
// 装配纪律：本组只注册给 Scheduler（bootstrap 经 scheduler.New 注入
// System.GraphRuntime / System.GraphStore）。三个工具无条件注册——未注入
// 对应依赖时调用返回明确中文错误（出现在非 Scheduler 上下文即装配错误，
// 必须显式暴露而不是静默 missing）。
type GraphControlGroup struct {
	Runtime        *graph.Runtime // submit_graph 的提交入口；nil 时调用报错
	Store          *graph.Store   // read_graph + submit_graph 的 root 激活读回
	RouteValidator RouteValidator // 生产装配注入；提交前 fail-closed 核对 route/capability
	TaskStore      store.TaskStore
	Holder         TaskHolder // 当前 Scheduler task；Graph controller 的控制面只允许操作自身 Graph
	// FinalizationNotifier 与 Scheduler agent 的 FinalizationHolder 共用。
	// submit_graph 完整成功（durable + root 已激活）后立即标记
	// origin Scheduler task 进入 finalizing：当前请求已交棒给 Graph，
	// origin 只应静默结束以释放 Scheduler，不得再自然回答或调
	// report_done 把“图已提交”冒充为用户最终结果。提交失败时
	// 不标记，保留 Scheduler 修正后重试的机会。
	FinalizationNotifier FinalizationNotifier
	// DisableSubmitGraph 用于新 root Scheduler registry：保留 read_graph 与
	// controller 兼容 patch_graph，但不再向新请求暴露一体化 submit→activate。
	// 零值保留 legacy 测试/兼容装配。
	DisableSubmitGraph bool
	// PatchControllerOnly 使 patch_graph 只服务于 legacy Graph controller；
	// root Scheduler 与事务化 Definition 必须走 GraphChangeProposal。
	PatchControllerOnly bool
}

func (g GraphControlGroup) Register(r *agent.ToolRegistry) {
	if !g.DisableSubmitGraph {
		r.Register("submit_graph",
			"提交一张 V6 JSON GraphDocument 作为多节点编排的执行契约，校验通过后立即 durable 并激活 root 节点。"+
				"这是需要执行工作的 Graph-first 主控制面：多步调查、Shell、写入、验证、并行、分支、回边、审批与等待均应在图内表达；publish_task 仅保留 legacy/恢复兼容。"+
				"graph 参数是完整 JSON：schema 恰为 \"agentgo.graph/v2\"，graph_id 全局唯一（重复提交拒绝），root 指向唯一起点节点；"+
				"节点 kind ∈ controller/agent/router/end/join/wait_event/tool/approval/subgraph/acceptance；"+
				"边条件 when 只两形态：{event: completed|failed|blocked|always}（agent/controller 出边仅这四个系统事件可用）或 {path: \"$.字段\", operator: eq|ne|in|exists, value: ...}（业务分支的唯一通道，字段须在该节点 description 的输出契约中声明），缺省无条件。"+
				"认领路由规则：节点 metadata.route 显式覆盖优先；缺省 controller→__scheduler__（由你认领）、agent→默认队列（\"\"）、acceptance→acceptance.verify。"+
				"agent/controller 节点禁止提交 event；业务路由字段写入 result object，提交期系统预求值出边，无匹配将被拒绝并要求重交。"+
				"校验失败返回含阶段与 JSON 路径的中文错误；提交后节点任务自动发布到公告板、终态自动推进，你等待 graph_ended 或中间唤醒即可，不得替图内节点执行工作。",
			schema.Object().String("graph", "完整的 JSON GraphDocument（schema/graph_id/root/nodes 必填；nodes 内每节点含 kind/task/next）", true).Build(),
			g.submitGraph)
	}

	r.Register("read_graph",
		"读取一张已提交图的当前权威快照，返回 revision、state_version、图/节点状态、当前 activation/task_id/result_ref 与定义面。"+
			"patch_graph 前必须先调用本工具取得 base_revision；revision 冲突后也必须重新读取，禁止猜测或盲目自增。",
		schema.Object().String("graph_id", "目标图 ID", true).Build(),
		g.readGraph)

	r.Register("patch_graph",
		"以 base_revision CAS 修改一张已提交图的定义面：patch 是 JSON DefinitionPatch——upsert_nodes（整体替换节点的 kind/task/capability/next 等定义字段，运行字段保留）、remove_nodes（删除节点）、root（改起点）；"+
			"应用后自动重跑语义校验，任一失败整体不生效。"+
			"base_revision 必须等于你最近读到的图 revision；冲突时返回当前 revision，重新读取最新图后再改，禁止盲目自增重试。"+
			"patch 只影响未来的转移求值：已激活/在途节点继续按旧定义执行，后续激活（含回边重进）才用新定义。"+
			"修改在途节点的 next 不改变其本次出边求值，禁止用它给在途节点（含 controller 自身）改路由；运行时分支应在建图时预铺条件边。",
		schema.Object().String("graph_id", "目标图 ID", true).
			Int("base_revision", "你最近读到的图 revision（CAS 前提；submit_graph 初始图为 1）", true).
			String("patch", "JSON DefinitionPatch，如 {\"upsert_nodes\":[{\"id\":\"verify\",\"kind\":\"agent\",\"task\":{\"title\":\"验证\"},\"next\":[{\"to\":\"finish\"}]}]}", true).Build(),
		g.patchGraph)
}

func (g GraphControlGroup) readGraph(_ context.Context, args map[string]any) (string, error) {
	if g.Store == nil {
		return "", fmt.Errorf("read_graph 不可用：Graph Store 未注入（本工具只在 Scheduler 上下文装配）")
	}
	graphID, _ := args["graph_id"].(string)
	graphID = strings.TrimSpace(graphID)
	if graphID == "" {
		return "", fmt.Errorf("graph_id 不能为空")
	}
	if err := g.authorizeGraphTarget("read_graph", graphID); err != nil {
		return "", err
	}
	doc, ok := g.Store.Get(graphID)
	if !ok {
		return "", fmt.Errorf("read_graph 失败: 图 %s 不存在", graphID)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("read_graph 序列化图 %s 失败: %w", graphID, err)
	}
	return string(raw), nil
}

// submitGraph 流程：ParseAndValidate（失败返回含阶段/路径的中文校验错误）→
// Runtime.SubmitGraph（durable + root 激活）→ 读回 root activation 信息返回。
func (g GraphControlGroup) submitGraph(_ context.Context, args map[string]any) (string, error) {
	if g.Runtime == nil {
		return "", fmt.Errorf("submit_graph 不可用：Graph Runtime 未注入（本工具只在 Scheduler 上下文装配）")
	}
	if err := g.authorizeGraphSubmit(); err != nil {
		return "", err
	}
	raw, _ := args["graph"].(string)
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("graph 参数不能为空：请提交完整的 JSON GraphDocument")
	}
	doc, err := graph.ParseAndValidate([]byte(raw))
	if err != nil {
		// *graph.ValidationError 自带「校验[阶段]」前缀与出错路径，原样透出。
		return "", fmt.Errorf("图校验失败: %w", err)
	}
	if originTask, taskErr := g.currentSchedulerTask(); taskErr != nil {
		return "", taskErr
	} else if originTask != nil {
		doc.RunID = originTask.RunID
		if originTask.RunContract != nil {
			run := *originTask.RunContract
			doc.RunContract = &run
		}
	}
	if err := g.validateRoutes(doc.GraphID, doc.Nodes, "nodes"); err != nil {
		trace.Emit(trace.Event{Kind: trace.KindGraphSubmissionRejected, GraphID: doc.GraphID, Error: err.Error()})
		return "", err
	}
	if err := g.Runtime.SubmitGraph(doc); err != nil {
		// 提交失败（同 graph_id 已存在 / 落盘失败）或 root 激活失败（图已提交，
		// 错误如实上抛，Scheduler 可据此决策 patch 或放弃）。
		return "", err
	}
	// Graph 已成为后续执行的唯一权威。这个标记同时使
	// LLMExecutor 的 finalizing fence 跳过同一响应中排在
	// submit_graph 之后的 report_done/其它工具，且让下一轮
	// ReAct 在不产生用户 ResultOutput 的情况下收尾 origin task。
	if g.FinalizationNotifier != nil {
		g.FinalizationNotifier.MarkTaskFinalized()
	}
	// 读回 root 激活事实，给 Scheduler 可引用的激活信息（activation_id 是后续
	// trace/恢复的检索键）。Store 缺失时退化为无激活信息的确认（不阻断提交）。
	activation := ""
	if g.Store != nil {
		if stored, ok := g.Store.Get(doc.GraphID); ok {
			if root, ok2 := stored.Nodes[stored.Root]; ok2 && root.Execution != nil {
				activation = root.Execution.ActivationID
			}
		}
	}
	msg := fmt.Sprintf("图已提交并激活: graph_id=%s revision=1 root=%s root_activation=%s（root 任务已按路由发布到公告板；后续节点终态会经 graph-terminal-feed 自动推进，图终态见 graph_ended 事件）",
		doc.GraphID, doc.Root, activation)
	return msg, nil
}

// patchGraph 流程：解码 JSON DefinitionPatch → Runtime.PatchGraph（与所有
// 状态推进共用 rt.mu 串行，再由 Store 做 CAS + 语义重校验）。
// ErrRevisionConflict 返回含当前 revision 的中文冲突错误。
// patch 生效后只影响未来的转移求值——已激活/在途节点按激活时的定义继续执行。
func (g GraphControlGroup) patchGraph(_ context.Context, args map[string]any) (string, error) {
	if g.Runtime == nil {
		return "", fmt.Errorf("patch_graph 不可用：Graph Runtime 未注入（本工具只在 Scheduler 上下文装配）")
	}
	graphID, _ := args["graph_id"].(string)
	graphID = strings.TrimSpace(graphID)
	if graphID == "" {
		return "", fmt.Errorf("graph_id 不能为空")
	}
	if g.PatchControllerOnly {
		task, err := g.currentSchedulerTask()
		if err != nil {
			return "", err
		}
		if task == nil || task.GraphID == "" || task.GraphID != graphID {
			return "", fmt.Errorf("patch_graph 仅保留给 exact same legacy Graph controller；新 root Scheduler 请用 GraphChangeProposal")
		}
		if g.Store == nil {
			return "", fmt.Errorf("patch_graph 无法核对 Graph Definition 来源")
		}
		if doc, ok := g.Store.Get(graphID); !ok || doc.DefinitionDigest != "" {
			return "", fmt.Errorf("事务化 Graph 禁止 direct patch_graph；请 request_replan 后由 Scheduler 提交 GraphChangeProposal")
		}
	}
	if err := g.authorizeGraphTarget("patch_graph", graphID); err != nil {
		return "", err
	}
	baseRevision := int64(intArg(args, "base_revision"))
	if baseRevision <= 0 {
		return "", fmt.Errorf("base_revision 必须为正整数：它必须等于你最近读到的图 revision（submit_graph 初始图为 1）")
	}
	raw, _ := args["patch"].(string)
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("patch 参数不能为空：请提交 JSON DefinitionPatch（upsert_nodes/remove_nodes/root）")
	}
	var patch graph.DefinitionPatch
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		return "", fmt.Errorf("patch JSON 解码失败: %w", err)
	}
	upserts := make(map[string]graph.Node, len(patch.UpsertNodes))
	for _, def := range patch.UpsertNodes {
		upserts[def.ID] = graph.Node{
			Kind: def.Kind, Task: def.Task, Capability: def.Capability, Next: def.Next,
			Wait: def.Wait, Tool: def.Tool, Subgraph: def.Subgraph,
			Metadata: def.Metadata, Extensions: def.Extensions,
		}
	}
	if err := g.validateRoutes(graphID, upserts, "patch.upsert_nodes"); err != nil {
		return "", err
	}
	newRevision, err := g.Runtime.PatchGraph(graphID, baseRevision, patch)
	if err != nil {
		var conflict *graph.RevisionConflictError
		if errors.As(err, &conflict) {
			return "", fmt.Errorf("patch_graph 冲突：你基于 revision %d 修改，但图 %s 当前 revision=%d；请重新读取最新图后再改（禁止盲目自增 base_revision 重试）",
				conflict.Base, graphID, conflict.Current)
		}
		return "", fmt.Errorf("patch_graph 失败: %w", err)
	}
	// 审计事件（C5d）：TaskID 为空、GraphID 非空，事件归入 graph 分片，
	// 与该图的其它运行事件同账。Description 只载 revision 与 patch 摘要，
	// 不复制整图 JSON。
	trace.Emit(trace.Event{
		Kind:        trace.KindGraphRevisionCommitted,
		GraphID:     graphID,
		Description: fmt.Sprintf("new_revision=%d %s", newRevision, patchSummary(&patch)),
	})
	return fmt.Sprintf("图已更新: graph_id=%s new_revision=%d；patch 只影响未来的转移求值，已激活/在途节点不受影响", graphID, newRevision), nil
}

// validateRoutes checks every task-producing node before the Graph definition
// becomes durable. It prevents a guessed/stale/cross-Graph Team event_type from
// entering the runtime and waiting for Watchdog to discover the problem.
func (g GraphControlGroup) validateRoutes(graphID string, nodes map[string]graph.Node, path string) error {
	return g.validateRoutesForScope(graphID, model.GraphRouteScope(graphID), nodes, path, false)
}

// validateRoutesForScope recursively validates task routes. Inline subgraphs
// materialize as <parentGraphID>/<activationID>, so their private owner scope
// cannot be known or remain stable at definition time (a loop may create @2,
// @3, ...). They therefore may use only global/static routes. Treating a
// parent-Graph Team as inherited would pass submission but fail at runtime when
// Watchdog evaluates the child Graph scope.
func (g GraphControlGroup) validateRoutesForScope(
	graphID, ownerScope string,
	nodes map[string]graph.Node,
	path string,
	inlineSubgraph bool,
) error {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := nodes[id]
		route, producesTask := graphControlRoute(node)
		if producesTask {
			var required []string
			if node.Capability != nil {
				required = node.Capability.Tools
			}
			if route != graph.RouteScheduler && g.RouteValidator == nil {
				return fmt.Errorf("图路由校验失败: %s.%s 需要 route=%q，但 runtime route 权威未注入；为避免提交后无人认领，按 fail-closed 拒绝", path, id, route)
			}
			if route != graph.RouteScheduler && !g.RouteValidator.CanRouteForPlan(ownerScope, route, required...) {
				display := route
				if display == "" {
					display = "<default>"
				}
				if inlineSubgraph {
					return fmt.Errorf("图路由校验失败: %s.%s 的 route=%q 不能在内联 subgraph 使用；内联子图的运行时 graph_id 由 activation 派生，不继承父 Graph 的私有 Team scope，请改用全局静态 route，或把该节点放回父图/拆成具有明确 graph_id 的独立 Graph", path, id, display)
				}
				return fmt.Errorf("图路由校验失败: %s.%s 的 route=%q 在 Graph %q 下无 ready 且能力匹配的 Agent；动态 Team 必须用相同 graph_id 调用 provision_agent_team，并在下一轮使用返回的真实 event_type", path, id, display, graphID)
			}
			if node.Kind == graph.KindAcceptance {
				if err := g.validateAcceptanceCapabilities(ownerScope, route, node.Capability, path+"."+id); err != nil {
					return err
				}
			}
		}
		if node.Subgraph != nil {
			// Empty owner scope means only global/static registrations are visible;
			// any task-/Graph-private Team registration is excluded.
			if err := g.validateRoutesForScope(graphID, "", node.Subgraph.Nodes, path+"."+id+".subgraph.nodes", true); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAcceptanceCapabilities 对 acceptance 的实际工具面做闭集校验。
// verifier 的只读边界不是 prompt 建议：只有当 per-node capability 明确收窄
// 时才按收窄集合判断，否则按 route 的保证能力判断；无能力权威或出现闭集外
// 工具时一律 fail-closed，避免把具有协调、交互或其它副作用能力的 Agent
// 误当成独立裁判。未来若引入 MCP，只能在工具元数据能够证明 read-only 后扩表。
func (g GraphControlGroup) validateAcceptanceCapabilities(ownerScope, route string, capability *graph.Capability, path string) error {
	if route == graph.RouteScheduler {
		return fmt.Errorf("图路由校验失败: %s 的 acceptance 不得路由到 Scheduler；verifier 必须是无写工具、无 Shell 的独立 route", path)
	}
	var effective []string
	if capability != nil && len(capability.Tools) > 0 {
		effective = capability.Tools
	} else {
		// 未显式收窄时，实际 claimant 可能是 route 上的任意 listener。闭集
		// 属性必须检查所有 listener 的能力并集，不能使用“每个 listener 都保证
		// 具备”的交集；后者会隐藏某个高权限 listener 独有的副作用工具。
		envelope, ok := g.RouteValidator.(RouteCapabilityEnvelopeResolver)
		if !ok {
			return fmt.Errorf("图路由校验失败: %s 的 acceptance 无法核对 route=%q 的可能工具并集；缺少 RouteCapabilityEnvelopeResolver，按 fail-closed 拒绝", path, route)
		}
		var found bool
		effective, found = envelope.RouteCapabilityEnvelopeForPlan(ownerScope, route)
		if !found {
			return fmt.Errorf("图路由校验失败: %s 的 acceptance route=%q 没有可核对的能力权威", path, route)
		}
	}
	hasSubmit := false
	for _, tool := range effective {
		if tool == "submit_task_result" {
			hasSubmit = true
			break
		}
	}
	if !hasSubmit {
		return fmt.Errorf("图路由校验失败: %s 的 acceptance 实际工具面缺少 submit_task_result，无法提交 verdict", path)
	}
	allowed := map[string]struct{}{
		"read_file": {}, "list_dir": {}, "grep_search": {}, "glob_search": {},
		"web_search": {}, "web_fetch": {}, "read_content_ref": {}, "submit_task_result": {},
	}
	for _, tool := range effective {
		if _, ok := allowed[tool]; !ok {
			return fmt.Errorf("图路由校验失败: %s 的 acceptance 实际工具面含只读闭集外工具 %q；verifier 只允许 read/list/grep/glob/web/read_content_ref 与 submit_task_result，不得持有写入、Shell、消息、发任务、用户交互或重规划工具；需要命令检查时改由 checker agent 经数据流提供证据", path, tool)
		}
	}
	return nil
}

func (g GraphControlGroup) currentSchedulerTask() (*model.Task, error) {
	if g.Holder == nil {
		return nil, nil
	}
	taskID := strings.TrimSpace(g.Holder.Get())
	if taskID == "" {
		return nil, fmt.Errorf("Graph 控制面无法确定当前 Scheduler task，按 fail-closed 拒绝")
	}
	if g.TaskStore == nil {
		return nil, fmt.Errorf("Graph 控制面未注入 TaskStore，无法校验当前任务 %s 的 Graph 归属，按 fail-closed 拒绝", taskID)
	}
	task, err := g.TaskStore.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("Graph 控制面读取当前 Scheduler task %s 失败: %w", taskID, err)
	}
	if err := validateFinalReportScope(task); err != nil {
		return nil, fmt.Errorf("Graph 控制面 final-report scope 无效: %w", err)
	}
	return task, nil
}

func (g GraphControlGroup) authorizeGraphSubmit() error {
	task, err := g.currentSchedulerTask()
	if err != nil {
		return err
	}
	if task != nil && task.GraphID != "" {
		return fmt.Errorf("Graph controller 禁止 submit_graph 创建脱离当前执行契约的新图（current_graph_id=%s）；请先 read_graph，再用当前 revision 调用 patch_graph 修改当前图", task.GraphID)
	}
	return nil
}

func (g GraphControlGroup) authorizeGraphTarget(operation, graphID string) error {
	task, err := g.currentSchedulerTask()
	if err != nil {
		return err
	}
	if task != nil && task.GraphID != "" && task.GraphID != graphID {
		return fmt.Errorf("Graph controller 禁止 %s 其他 Graph：current_graph_id=%s target_graph_id=%s；只能读取或修改当前执行契约", operation, task.GraphID, graphID)
	}
	if task != nil && task.FinalReportGraphID != "" && task.FinalReportGraphID != graphID {
		return fmt.Errorf("graph-ended final-report 禁止 %s 其他 Graph：final_report_graph_id=%s target_graph_id=%s",
			operation, task.FinalReportGraphID, graphID)
	}
	return nil
}

func graphControlRoute(node graph.Node) (string, bool) {
	if route := strings.TrimSpace(node.Metadata["route"]); route != "" {
		switch node.Kind {
		case graph.KindController, graph.KindAgent, graph.KindAcceptance:
			return route, true
		default:
			return "", false
		}
	}
	switch node.Kind {
	case graph.KindController:
		return graph.RouteScheduler, true
	case graph.KindAgent:
		return graph.RouteDefaultQueue, true
	case graph.KindAcceptance:
		return graph.RouteAcceptance, true
	default:
		return "", false
	}
}

// patchSummary 生成 DefinitionPatch 的单行摘要：upsert/remove 的节点 ID
// 清单与 root 变更，不复制任何定义体（保持 trace 行轻量）。
func patchSummary(patch *graph.DefinitionPatch) string {
	var parts []string
	if len(patch.UpsertNodes) > 0 {
		ids := make([]string, 0, len(patch.UpsertNodes))
		for _, n := range patch.UpsertNodes {
			ids = append(ids, n.ID)
		}
		parts = append(parts, "upsert=["+strings.Join(ids, " ")+"]")
	}
	if len(patch.RemoveNodes) > 0 {
		parts = append(parts, "remove=["+strings.Join(patch.RemoveNodes, " ")+"]")
	}
	if patch.Root != nil {
		parts = append(parts, "root="+*patch.Root)
	}
	if len(parts) == 0 {
		return "（空 patch）"
	}
	return strings.Join(parts, " ")
}
