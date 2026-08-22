package bootstrap

// 本文件是 acceptance 谱系核验的 Bootstrap 装配桥（graph.GraphChangeWaker
// 的活系统实现 + 装配点）：
//   - graphChangeWaker：acceptance 谱系核验 disputed 时按既有 graph change
//     机制发布 __scheduler__ 唤醒任务（幂等标记
//     [graph-change-request: <graphID>/<activationID>/change]，与
//     request_replan 图路径同一格式，跨路径查重共享）；
//   - wireGraphAcceptanceBridge：装配注入点。
//
// G1b 的机器格式契约核验（command 对照 Effect Journal 逐字比对 /
// file_hash 重算 / task_status 词表）已随旧证据契约整体退役：谱系核验
// （引用是否属于该 activation 的上游 Input 谱系或 verifier 自身证据）是
// 引擎内生行为，见 internal/graph/acceptance.go 的判定矩阵。

import (
	"fmt"
	"strings"

	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
)

// ============================================================
// graphChangeWaker —— graph.GraphChangeWaker 的公告板实现
// ============================================================

// graphChangeWaker 按 C5d 既有 graph change 机制发布 __scheduler__ 唤醒任务。
type graphChangeWaker struct {
	store  store.TaskStore
	graphs *graph.Store
}

// graphChangeWakeMarkerKind 按种类构造唤醒任务描述中的幂等标记（与
// tools/plan_control.go 的 graphChangeMarker 同一格式）；kind 为空回落
// "change"——两条触发路径（request_replan 图任务 / acceptance 谱系核验不
// 通过）共享查重。终态契约 v2 两击升级使用独立 "no-outlet" 标记（与
// change 标记互不查重，同一 activation 可同时挂两种唤醒）。
func graphChangeWakeMarkerKind(graphID, activationID, kind string) string {
	if kind == "" {
		kind = "change"
	}
	return "[graph-change-request: " + graphID + "/" + activationID + "/" + kind + "]"
}

// WakeGraphChange 实现 graph.GraphChangeWaker：同一 activation 已有未处理
// （非终态）的同类唤醒任务时幂等返回，不重复发布。唤醒任务刻意不携带
// GraphID/NodeID/ActivationID：它是 Scheduler 的控制面输入而非图节点任务，
// 带图身份会被 graph-terminal-feed 当作节点终态回填引擎。
func (w graphChangeWaker) WakeGraphChange(spec graph.GraphChangeWakeSpec) error {
	marker := graphChangeWakeMarkerKind(spec.GraphID, spec.ActivationID, spec.MarkerKind)
	// 幂等查重（MemoryTaskStore.ScanAll 永不返回错误；其它实现扫描失败时
	// 退化为直接发布——多一个唤醒任务无害，Scheduler 裁决天然幂等）。
	if tasks, err := w.store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t == nil || t.EventType != "__scheduler__" || model.IsTerminal(t.Status) {
				continue
			}
			if strings.Contains(t.Description, marker) {
				return nil
			}
		}
	}
	description := ""
	switch spec.MarkerKind {
	case graph.WakeMarkerNoOutlet:
		// 终态契约 v2 两击升级（§6 Scheduler 纪律）：必须先 read_graph 再
		// 裁决，禁止机械地把当前值塞进新边了事。
		description = fmt.Sprintf(
			"%s\n图 %s 的节点 %s（activation %s）两次提交均无匹配出路（%s），节点已按终态契约 v2 置 failed。\n原因：%s\n来源任务：%s\n处理指引：先 read_graph 读取该图当前状态（当前 revision）再裁决——判断是生产者漏字段（返工）、边写错值（修边）还是需求变化（改道），禁止机械地把当前提交值塞进新边了事；用 patch_graph（base_revision CAS）修改图定义，冲突时重新读取最新 revision 再改；确认图不可修复时宣布图失败；判断无需修改时直接结束本任务。",
			marker, spec.GraphID, spec.NodeID, spec.ActivationID, spec.Reason, spec.Detail, spec.TaskID)
	case graph.WakeMarkerWritebackFailed:
		// SWE-002 第三层防线：终态回填失败的回落唤醒。同样必须先 read_graph
		// 再裁决——节点已被置 failed，Scheduler 决定重激活/改道/宣布图失败。
		description = fmt.Sprintf(
			"%s\n图 %s 的节点 %s（activation %s）终态回填失败（%s），节点已按 SWE-002 回落置 failed。\n原因：%s\n来源任务：%s\n处理指引：先 read_graph 读取该图当前状态（当前 revision）再裁决——回填失败多为持久化层数据/IO 问题而非业务失败：判断应重激活该节点重跑（patch_graph 改道或补返工回边）、沿 failed 兜底边续走，还是宣布图失败；用 patch_graph（base_revision CAS）修改图定义，冲突时重新读取最新 revision 再改；判断无需修改时直接结束本任务。",
			marker, spec.GraphID, spec.NodeID, spec.ActivationID, spec.Reason, spec.Detail, spec.TaskID)
	default:
		description = fmt.Sprintf(
			"%s\n图 %s 的验收节点 %s（activation %s）谱系核验未通过（%s），自报 verdict 未被采信，节点已置 failed。\n原因：%s\n来源任务：%s\n处理指引：读取该图当前状态（当前 revision），用 patch_graph（base_revision CAS）裁决是否修改图定义（如调整验收判据、修复节点或改路由）；冲突时重新读取最新 revision 再改；判断无需修改时直接结束本任务。",
			marker, spec.GraphID, spec.NodeID, spec.ActivationID, spec.Reason, spec.Detail, spec.TaskID)
	}
	wake := &model.Task{
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    "graph-change-request",
		ParentTaskID:   spec.TaskID,
		MaxConcurrency: 1, // 同一时刻只允许一个 Scheduler 处理同一请求
	}
	var parent *model.Task
	if spec.TaskID != "" {
		parent, _ = w.store.GetTask(spec.TaskID)
	}
	if w.graphs != nil {
		if doc, ok := w.graphs.Get(spec.GraphID); ok && doc != nil {
			if parent == nil {
				parent = &model.Task{RunID: doc.RunID, RunContract: doc.RunContract}
			}
		}
	}
	if parent != nil {
		if err := taskcontract.Inherit(parent, wake, loopcontract.WorkCoordination); err != nil {
			return fmt.Errorf("graph change 唤醒继承 RunContract: %w", err)
		}
		if wake.RunContract != nil {
			wake.RunPhase = runcontract.PhaseRecovery
		}
	}
	if err := w.store.PublishTask(wake); err != nil {
		return fmt.Errorf("发布 graph change 唤醒任务失败: %w", err)
	}
	return nil
}

// ============================================================
// Bootstrap 装配点
// ============================================================

// wireGraphAcceptanceBridge 装配 acceptance 桥：graph change 唤醒器注入
// graph.Runtime。谱系核验本身是引擎内生行为（acceptance 节点 completed
// 终态一律执行，数据全部来自图内 durable 事实），无需外部注入。
//
// 调用时序约束：与 graph approval/tool 桥同批（resumeNonTerminalGraphs 之前）——
// 恢复路径不触发 OnTaskTerminal，但启动后第一批验收任务终态必须已注入。
func wireGraphAcceptanceBridge(taskStore store.TaskStore, rt *graph.Runtime, graphs ...*graph.Store) {
	var graphStore *graph.Store
	if len(graphs) > 0 {
		graphStore = graphs[0]
	}
	rt.SetChangeWaker(graphChangeWaker{store: taskStore, graphs: graphStore})
}
