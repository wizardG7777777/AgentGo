package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
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
	Runtime *graph.Runtime // submit_graph 的提交入口；nil 时调用报错
	Store   *graph.Store   // read_graph + submit_graph 的 root 激活读回
}

func (g GraphControlGroup) Register(r *agent.ToolRegistry) {
	r.Register("submit_graph",
		"提交一张 V6 JSON GraphDocument 作为多节点编排的执行契约，校验通过后立即 durable 并激活 root 节点。"+
			"适用场景：条件分支、实现↔验证回边、并行 fan-out/join、人工审批、等待外部事件等多节点编排；单任务直发仍用 publish_task（Plan 迁移期两路径并存，图是推荐的多节点编排方式）。"+
			"graph 参数是完整 JSON：schema 恰为 \"agentgo.graph/v1\"，graph_id 全局唯一（重复提交拒绝），root 指向唯一起点节点；"+
			"节点 kind ∈ controller/agent/router/end/join/wait_event/tool/approval/subgraph/acceptance；"+
			"边条件 when 只两形态：{event: ready|completed|fixable|failed|blocked|pass|approved|rejected|timeout|always} 或 {path: \"$.字段\", operator: eq|ne|in|exists, value: ...}，缺省无条件。"+
			"认领路由规则：节点 metadata.route 显式覆盖优先；缺省 controller→__scheduler__（由你认领）、agent→默认队列（\"\"）、acceptance→acceptance.verify。"+
			"agent 节点任务结束时应经 submit_task_result 的 event 参数报告事件名，驱动下游 {event: ...} 边。"+
			"校验失败返回含阶段与 JSON 路径的中文错误；提交后节点任务自动发布到公告板、终态自动推进，你等待 graph_ended 或中间唤醒即可，不得替图内节点执行工作。",
		schema.Object().String("graph", "完整的 JSON GraphDocument（schema/graph_id/root/nodes 必填；nodes 内每节点含 kind/task/next）", true).Build(),
		g.submitGraph)

	r.Register("read_graph",
		"读取一张已提交图的当前权威快照，返回 revision、state_version、图/节点状态、当前 activation/task_id/result_ref 与定义面。"+
			"patch_graph 前必须先调用本工具取得 base_revision；revision 冲突后也必须重新读取，禁止猜测或盲目自增。",
		schema.Object().String("graph_id", "目标图 ID", true).Build(),
		g.readGraph)

	r.Register("patch_graph",
		"以 base_revision CAS 修改一张已提交图的定义面：patch 是 JSON DefinitionPatch——upsert_nodes（整体替换节点的 kind/task/capability/next 等定义字段，运行字段保留）、remove_nodes（删除节点）、root（改起点）；"+
			"应用后自动重跑语义校验，任一失败整体不生效。"+
			"base_revision 必须等于你最近读到的图 revision；冲突时返回当前 revision，重新读取最新图后再改，禁止盲目自增重试。"+
			"patch 只影响未来的转移求值：已激活/在途节点继续按旧定义执行，后续激活（含回边重进）才用新定义。",
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
	raw, _ := args["graph"].(string)
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("graph 参数不能为空：请提交完整的 JSON GraphDocument")
	}
	doc, err := graph.ParseAndValidate([]byte(raw))
	if err != nil {
		// *graph.ValidationError 自带「校验[阶段]」前缀与出错路径，原样透出。
		return "", fmt.Errorf("图校验失败: %w", err)
	}
	if err := g.Runtime.SubmitGraph(doc); err != nil {
		// 提交失败（同 graph_id 已存在 / 落盘失败）或 root 激活失败（图已提交，
		// 错误如实上抛，Scheduler 可据此决策 patch 或放弃）。
		return "", err
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
	return fmt.Sprintf("图已提交并激活: graph_id=%s revision=1 root=%s root_activation=%s（root 任务已按路由发布到公告板；后续节点终态会经 graph-terminal-feed 自动推进，图终态见 graph_ended 事件）",
		doc.GraphID, doc.Root, activation), nil
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
