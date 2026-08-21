package graph

// 本文件实现终态回填失败的回落处置（SWE-002 第三层防线）。
//
// graph-terminal-feed 把任务终态事实回填 OnTaskTerminal 时，任何一步 durable
// 写入失败（历史上的事故形状：畸形工具名撞 evidence 边界，整条 activation
// result 被 store 全有或全无拒写）都不能让终态事实无处置——否则节点滞留
// running、图僵尸停摆。FailTerminalWriteback 把该 activation 显式结算为
// failed（reason 含 graph_writeback_failed 与截断 cause），按统一结算路径
// 求值转移（failed/always 兜底边照常生效，无兜底边则图 fail-closed），并经
// GraphChangeWakeSpec 机制发布幂等 writeback-failed 唤醒任务交 Scheduler
// 裁决（patch_graph 重激活/改道/宣布图失败）。

import (
	"fmt"
	"strings"
)

// writebackCauseMaxRunes 是回落 reason 中携带的原始错误摘要上限（错误本身
// 可能含超长证据内容，必须 bounded 后才能再进 durable journal）。
const writebackCauseMaxRunes = 400

// FailTerminalWriteback 是 OnTaskTerminal 回填失败的回落入口：把该
// activation 的节点显式置 failed 并唤醒 Scheduler 裁决，保证任务终态事实
// 永远有处置路径（成功回填 / 回落标 failed+唤醒 / 回落也失败只剩
// persistence-degraded 告警，三态穷尽）。
//
// 守卫与 OnTaskTerminal 同源：图已终态/停驻、activation 过期、节点非
// running、task_id 不符时安全 no-op（这些情形 OnTaskTerminal 本不会报错，
// 回落只是防御）。图不存在/节点不存在时返回错误，由调用方记严重告警。
//
// 回落结算刻意不携带 TerminalFact.Evidence——触发拒写的极可能就是这批证据
// 本身，携带会让回落在同一边界上再次失败；原始调用事实仍留在任务账本与
// trace，验收所需证据由重激活后的新 execution 重新装配。
func (rt *Runtime) FailTerminalWriteback(f TerminalFact, cause error) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, err := rt.graph(f.GraphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil // 图已终态：回填失败不再影响任何在途节点
	}
	if rt.isSuspendedLocked(f.GraphID) {
		return nil // 停驻图：解冻时经公告板对账回填，无需回落
	}
	node, ok := doc.Nodes[f.NodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, f.GraphID, f.NodeID)
	}
	ex := node.Execution
	if ex == nil || ex.ActivationID == "" || ex.ActivationID != f.ActivationID {
		return nil // activation 已更替：失败事实属过期 activation，无需处置
	}
	if node.Status != NodeRunning {
		return nil // 节点已有终态（如两击协议抢先置 failed）：回填失败无影响
	}
	if ex.TaskID != "" && f.TaskID != "" && ex.TaskID != f.TaskID {
		return nil // task_id 不符：与 OnTaskTerminal 同一守卫
	}
	causeText := "未知原因"
	if cause != nil {
		causeText = cause.Error()
	}
	reason := fmt.Sprintf("节点 %s（activation %s）终态回填失败（graph_writeback_failed）: %s；节点已置 failed 并唤醒 Scheduler 裁决",
		f.NodeID, f.ActivationID, truncateRunes(causeText, writebackCauseMaxRunes))
	failResult := map[string]any{
		"error": reason, "graph_writeback_failed": true,
		"original_status": string(f.Status),
	}
	exec := *ex
	if err := rt.writeTerminalContinuationLocked(f.GraphID, f.NodeID, exec, NodeFailed, failResult, SettlementContinueTransitions, reason); err != nil {
		return fmt.Errorf("graph: 回填失败回落的节点终态落盘失败: %w", err)
	}
	evalErr := rt.evalTransitionsLocked(f.GraphID, f.NodeID, f.ActivationID, NodeFailed, failResult)
	rt.wakeGraphChangeKind(TerminalFact{
		GraphID: f.GraphID, NodeID: f.NodeID, ActivationID: f.ActivationID,
		TaskID: ex.TaskID, Status: NodeFailed,
	}, "graph_writeback_failed", reason, WakeMarkerWritebackFailed)
	return evalErr
}

// truncateRunes 把字符串按 rune 截断到 max 个（超出时末位替换为省略号），
// 与 bootstrap boundedEvidenceValue 同手法；本包用于回落 reason 的有界化。
func truncateRunes(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	if max <= 0 {
		return ""
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}
