package tools

import (
	"context"
	"fmt"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// PlanControlGroup exposes narrow, audited control-plane operations. Tool
// visibility is still governed by ToolRegistry allowlists: Scheduler receives
// the full group; ordinary execution agents normally receive only
// submit_task_result and request_replan.
//
// C6b 已随 Plan 控制面整包删除验收四工具（define_acceptance_spec /
// ensure_acceptance_run / submit_acceptance_result / get_acceptance_evidence）
// 与 request_replan 的 Plan 控制面路径；本组剩余 submit_task_result 与
// request_replan，验收语义由 V6 Graph acceptance 节点 + submit_task_result 的
// verdict-only 契约承担（event 只供普通 Graph 节点事件路由）。
// OutletChecker 是 submit_task_result 对 Graph Runtime 提交期出路检查能力
// 的最小依赖（终态契约 v2，docs/design/graph-terminal-contract-v2.md §5/§6）。
// *graph.Runtime 直接实现；nil 时不做提交期检查（行为与引入前一致）。
// GraphSchema 在图不存在时返回空串（按非 v2 处理）。
type OutletChecker interface {
	GraphSchema(graphID string) string
	CheckActivationOutlet(graphID, nodeID, activationID string, status string, result map[string]any) error
}

type PlanControlGroup struct {
	Store   store.TaskStore
	Holder  TaskHolder
	AgentID string
	// FinalizationNotifier / SubmitState 是 submit_task_result 的提交通道注入
	// （runner 与 scheduler 装配都传入 agent.FinalizationHolder +
	// agent.SubmitState）。任一 nil 则不注册 submit_task_result。工具实现会在
	// 运行时区分角色：Graph controller 可用，非图 scheduler 仍用 report_done。
	FinalizationNotifier FinalizationNotifier
	SubmitState          *agent.SubmitState
	// ArtifactResolver 是 submit_task_result 的 expected-artifacts 磁盘兜底
	// 解析器（runner 装配注入）。nil 时校验退化为纯账本比对。
	ArtifactResolver agent.ArtifactPhysicalResolver
	// OutletChecker 是终态契约 v2 的提交期出路检查器（bootstrap/runner/
	// scheduler 装配注入 *graph.Runtime）。nil 时 v2 图任务不做提交期
	// 出路检查与 event 废弃拦截（行为与引入前一致）。
	OutletChecker OutletChecker
}

func (g PlanControlGroup) Register(r *agent.ToolRegistry) {
	if g.Store == nil || g.Holder == nil {
		return
	}
	if g.FinalizationNotifier != nil && g.SubmitState != nil {
		params := schema.Object().String("summary", "一两句话的任务结果概括；会随依赖结果传递给下游任务", true).
			String("checks_performed", "逗号分隔的已执行检查清单（如 go build, go test ./internal/...）", false).
			String("evidence", "逗号分隔的证据清单（文件路径、命令输出要点等），随结果渲染传递给下游", false).
			String("remaining_risks", "逗号分隔的残余风险清单", false).
			Enum("status", "自述终态：completed=正常完成（缺省）；blocked=无法完成，以 blocked 终态收尾并自动唤醒 Scheduler 重新规划（此时 blocked_reason 必填）", []string{"completed", "blocked"}, false).
			String("blocked_reason", "无法完成时的阻塞原因；status=blocked 时必填，其余情况非空时随提交向 Scheduler 登记高优 ReplanRequest", false).
			Bool("request_replan", "true 时随提交请求 Scheduler 重新评估当前任务图", false).
			String("event", "本结果对应的事件名，供 Graph 边条件 {event: ...} 匹配（仅允许 ready/completed/fixable/failed/blocked/pass/approved/rejected/timeout/always）；任务不属于图或下游不按事件路由时省略", false).
			String("verdict", "本结果的验收结论，仅允许 pass/fixable/failed；写入 Results[\"verdict\"] 供 Graph acceptance 节点的路径边条件 {$.verdict eq ...} 精确匹配。填写 verdict 时不得再填写 event；仅验收类任务需要填", false).
			String("cited_evidence", "验收结论引用的证据清单（逗号分隔的不透明稳定 EvidenceRef）：只复制任务描述「上游输入」段已经展示且实际消费的引用，不得按展示序号构造，也不得把 CallID/ResultRef 当作 EvidenceRef；Graph acceptance 会做谱系核验，越谱系引用使 verdict 不被采信（disputed，节点判 failed 并唤醒 Scheduler）；可选，不引用不影响 verdict 采信；非验收任务省略", false).Build()
		params["additionalProperties"] = false
		properties := params["properties"].(map[string]any)
		properties["result"] = map[string]any{
			"type":                 "object",
			"description":          "可选的自定义结构化结果字段；字段会类型保真地展开到 Graph Result 顶层，可由 $.coverage 或 $.metrics.score 等路径条件读取。顶层不得使用 status/event/verdict/cited_evidence 等系统保留键；大内容应走 artifact/evidence。",
			"maxProperties":        structuredResultMaxKeys,
			"propertyNames":        map[string]any{"pattern": "^[A-Za-z_][A-Za-z0-9_]{0,63}$"},
			"additionalProperties": true,
		}
		r.Register("submit_task_result", "以结构化字段提交当前执行节点的最终结果并结束任务，Graph controller 节点也使用本工具；只有非图 scheduler 任务改用 report_done。summary 必填（一两句话概括结果，会随依赖结果传递给下游任务）；result 可选，必须是紧凑 JSON object，其字段类型保真地进入 Graph Result 顶层，供 $.coverage 等条件路由及下游数据流消费，status/event/verdict/cited_evidence 为系统保留键，必须使用各自专用参数；checks_performed/evidence/remaining_risks 为逗号分隔的可选清单；status 可选（缺省 completed）：status=blocked 表示任务无法完成、以 blocked 终态收尾并自动唤醒 Scheduler 重新规划（blocked 终态不会放行下游依赖任务），此时 blocked_reason 必填；无法完成但不算 blocked 时也可只填 blocked_reason（会随提交向 Scheduler 登记高优 ReplanRequest），request_replan=true 仅请求重规划；event 已废弃——agentgo.graph/v2 图任务禁止携带（业务路由一律把字段写入 result object，供 {path: ...} 边条件求值；仅 v1 存量图与 legacy publish_task 路径保留旧语义）；verdict 仅用于 acceptance，固定为 pass/fixable/failed。提交前系统会执行 expected_artifacts 校验与出路预求值，缺失产物或无匹配出路时返回错误且不结束任务（可修正后重交，同一 activation 两次无匹配将升级 Scheduler 裁决）。调用成功即进入收尾（finalizing）：同一响应中排在其后的工具调用会被系统跳过不执行，因此提交前必须先完成所有写操作；每个任务只能成功提交一次。",
			params,
			g.submitTaskResult)
	}
	r.Register("request_replan", "请求重新唤醒 Scheduler 评估当前任务编排；不会直接修改 DAG。图（Graph）节点任务调用时登记 graph change 请求并以 __scheduler__ 唤醒任务交给 Scheduler 用 patch_graph 裁决（同一 activation 的重复请求幂等）；非图任务登记通用 replan 唤醒任务（同一任务的重复请求幂等），由 Scheduler 裁决后续编排。",
		schema.Object().String("reason_code", "结构化原因代码", true).
			Enum("urgency", "优先级", []string{"normal", "high"}, false).
			String("detail", "补充说明", false).
			String("idempotency_key", "可选幂等键；留空由系统生成", false).Build(), g.requestReplan)
}

func (g PlanControlGroup) requestReplan(ctx context.Context, args map[string]any) (string, error) {
	// 图任务（V6 Graph，GraphID 非空）走 graph change 流（C5d）；
	// 非图任务（C6b 起 Plan 控制面已删除）走通用 replan 唤醒任务——同一
	// 机制：发布 __scheduler__ 唤醒任务交给 Scheduler 裁决，不做服务端
	// 状态迁移。
	if taskID := g.Holder.Get(); taskID != "" {
		task, err := g.Store.GetTask(taskID)
		if err != nil {
			return "", err
		}
		if task.GraphID != "" {
			return g.requestGraphChange(task, args)
		}
		return g.requestGenericReplan(ctx, task, args)
	}
	return "", fmt.Errorf("no current task context")
}

// replanRequestMarker 是非图 replan 唤醒任务描述中的幂等标记；查重按该
// 子串匹配。幂等键为 <taskID>/replan：同一任务的重复请求只保留一个唤醒任务。
func replanRequestMarker(taskID string) string {
	return "[replan-request: " + taskID + "/replan]"
}

// requestGenericReplan 是 request_replan 的非图路径（C6b）：不触碰任何
// 服务端控制面状态，只向公告板发布 __scheduler__ 唤醒任务（描述含幂等
// 标记与请求者上下文），Scheduler 认领后自行裁决后续编排；同一任务已有
// 未处理（非终态）的同类唤醒任务时幂等返回，不重复发布。
//
// 唤醒任务刻意不携带 GraphID/NodeID/ActivationID：它是 Scheduler 的控制面
// 输入而非图节点任务，带图身份会被 graph-terminal-feed 当作节点终态回填引擎。
func (g PlanControlGroup) requestGenericReplan(_ context.Context, task *model.Task, args map[string]any) (string, error) {
	reason, _ := args["reason_code"].(string)
	urgency, _ := args["urgency"].(string)
	detail, _ := args["detail"].(string)
	marker := replanRequestMarker(task.ID)

	// 幂等查重：同一任务的未处理同类请求只保留一个唤醒任务。
	// MemoryTaskStore.ScanAll 永不返回错误；其它实现扫描失败时退化为直接
	// 发布（多一个唤醒任务无害，Scheduler 裁决天然幂等）。
	if tasks, err := g.Store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t == nil || t.EventType != "__scheduler__" || model.IsTerminal(t.Status) {
				continue
			}
			if strings.Contains(t.Description, marker) {
				return fmt.Sprintf("本任务的 replan 请求已登记（唤醒任务 %s 待处理），无需重复请求；请继续完成当前任务", t.ID), nil
			}
		}
	}

	// 审计事实由唤醒任务自身的 task_published 事件承担（描述含幂等标记、
	// reason_code 与 detail），C6c 起不再单独 emit replan 时代事件。
	description := fmt.Sprintf(
		"%s\n任务 %s（event_type=%s）在执行中请求 replan。\nreason_code=%s urgency=%s\n详情：%s\n处理指引：读取公告板当前任务状态，裁决是否需要补充/调整后续任务编排；判断无需调整时直接结束本任务。",
		marker, task.ID, task.EventType, reason, urgency, detail)
	wake := &model.Task{
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    "replan-request",
		ParentTaskID:   task.ID,
		MaxConcurrency: 1, // 同一时刻只允许一个 Scheduler 处理同一请求
	}
	if err := taskcontract.Inherit(task, wake, loopcontract.WorkCoordination); err != nil {
		return "", fmt.Errorf("继承 replan RunContract: %w", err)
	}
	if wake.RunContract != nil {
		wake.RunPhase = runcontract.PhaseRecovery
	}
	if err := g.Store.PublishTask(wake); err != nil {
		return "", fmt.Errorf("发布 replan 唤醒任务失败: %w", err)
	}
	return fmt.Sprintf("Replan 请求已登记并唤醒 Scheduler（唤醒任务 %s）。Scheduler 将裁决后续编排；请继续完成当前任务，不要等待裁决结果。", wake.ID), nil
}

// graphChangeRequestID 是 graph change 请求的确定性幂等键：同一 activation
// 的重复请求共享同一 ID，据此查重，避免唤醒任务刷屏。
func graphChangeRequestID(task *model.Task) string {
	return task.GraphID + "/" + task.ActivationID + "/change"
}

// graphChangeMarker 是唤醒任务描述中的幂等标记（含 requestID），查重按
// 该子串匹配；同时让 Scheduler 一眼识别任务来源。
func graphChangeMarker(requestID string) string {
	return "[graph-change-request: " + requestID + "]"
}

// requestGraphChange 是 request_replan 的图任务路径（C5d）：
//  1. emit graph_change_requested 审计事件（图/节点/activation + 请求者任务）；
//  2. 向公告板发布 __scheduler__ 唤醒任务，Scheduler 认领后用 patch_graph 裁决；
//  3. 同一 activation 已有未处理（非终态）的同类唤醒任务时幂等返回，不重复发布。
//
// 唤醒任务刻意不携带 GraphID/NodeID/ActivationID：它是 Scheduler 的控制面
// 输入而非图节点任务，带图身份会被 graph-terminal-feed 当作节点终态回填引擎。
func (g PlanControlGroup) requestGraphChange(task *model.Task, args map[string]any) (string, error) {
	reason, _ := args["reason_code"].(string)
	urgency, _ := args["urgency"].(string)
	detail, _ := args["detail"].(string)
	requestID := graphChangeRequestID(task)
	marker := graphChangeMarker(requestID)

	// 幂等查重：同一 (graph_id, activation_id) 的未处理同类请求只保留一个
	// 唤醒任务。MemoryTaskStore.ScanAll 永不返回错误；其它实现扫描失败时
	// 退化为直接发布（多一个唤醒任务无害，Scheduler 裁决天然幂等）。
	if tasks, err := g.Store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t == nil || t.EventType != "__scheduler__" || model.IsTerminal(t.Status) {
				continue
			}
			if strings.Contains(t.Description, marker) {
				return fmt.Sprintf("同一 activation 的 graph change 请求已登记（唤醒任务 %s 待处理），无需重复请求；请继续完成当前任务", t.ID), nil
			}
		}
	}

	trace.Emit(trace.Event{
		Kind: trace.KindGraphChangeRequested, TaskID: task.ID,
		GraphID: task.GraphID, NodeID: task.NodeID, ActivationID: task.ActivationID,
		Reason: reason, Description: detail,
	})

	description := fmt.Sprintf(
		"%s\n图 %s 的节点 %s（activation %s）在执行中请求 graph change。\nreason_code=%s urgency=%s\n详情：%s\n请求者任务：%s（event_type=%s）\n处理指引：读取该图当前状态（当前 revision），用 patch_graph（base_revision CAS）裁决是否修改；冲突时重新读取最新 revision 再改；判断无需修改时直接结束本任务。",
		marker, task.GraphID, task.NodeID, task.ActivationID, reason, urgency, detail, task.ID, task.EventType)
	wake := &model.Task{
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    "graph-change-request",
		ParentTaskID:   task.ID,
		MaxConcurrency: 1, // 与用户请求一致：同一时刻只允许一个 Scheduler 处理同一请求
	}
	if err := taskcontract.Inherit(task, wake, loopcontract.WorkCoordination); err != nil {
		return "", fmt.Errorf("继承 graph change RunContract: %w", err)
	}
	if wake.RunContract != nil {
		wake.RunPhase = runcontract.PhaseRecovery
	}
	if err := g.Store.PublishTask(wake); err != nil {
		return "", fmt.Errorf("发布 graph change 唤醒任务失败: %w", err)
	}
	return fmt.Sprintf("Graph change 请求已登记并唤醒 Scheduler：graph=%s node=%s activation=%s（唤醒任务 %s）。Scheduler 将用 patch_graph 裁决是否修改图定义；请继续完成当前任务，不要等待改图结果。",
		task.GraphID, task.NodeID, task.ActivationID, wake.ID), nil
}

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func intArg(args map[string]any, key string) int {
	switch value := args[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}
