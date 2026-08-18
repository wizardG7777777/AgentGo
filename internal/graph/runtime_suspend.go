package graph

// 本文件实现图按 session 的停驻/恢复（Session 生命周期隔离的 graph 切片）。
//
// 停驻（SuspendGraphsForSession）是纯运行时闸门：session 冻结时其拥有的
// 全部非终态图进入停驻表，此后输入闸门生效——
//   - OnTaskTerminal：吞掉（任务终态在公告板上有权威事实，解冻恢复时经
//     reconcileTaskLocked 对账回填，与 graph-terminal-feed 同语义；
//     公告板桥接侧 cancelled 已映射 failed，见 graphTerminalStatusOf）；
//   - OnApprovalDecided：暂存 pendingApprovals（审批裁决无 durable 面可
//     重建——恢复侧因 RequestID 已记录不重发请求，吞掉会让 waiting
//     approval 节点永久悬挂），解冻后回放；
//   - OnExternalEvent：吞掉（事件是时点信号、无 durable 面对账，视为冻结
//     期间未发生，与进程停止期间的事件语义一致）；
//   - onChildGraphEnded（子图终态回调）：吞掉（解冻恢复时
//     ensureSubgraphLocked 见子图已终态会补结算）；
//   - wait timer 全部停走（timer 只是进程内唤醒器；权威 deadline 在
//     Execution.WaitDeadline durable，解冻恢复时按原 wall-clock 重建，
//     已过期立即超时结算——冻结暂停图的推进，不暂停时间本身）。
//
// 图数据（activation 状态、journal）在停驻期间保持不动：闸门不写任何
// durable 面。停驻表本身是纯内存态，进程重启即丢失——启动恢复后由控制面
// （bootstrap）重新建立：2026-08 起会话模式下全部历史图（含无归属图与
// --resume 会话自己的图）经 SuspendGraphsExceptSession/SuspendGraphsForSession
// 一次性停驻；进入会话不再自动续跑，旧图没有恢复入口（僵尸停驻，随其
// 会话归档退出）。
//
// 恢复（ResumeGraphsForSession）逐张解除停驻后走既有 ResumeGraph 幂等
// 补发路径（崩溃恢复同款），再回放暂存的审批裁决。2026-08 起运行时解冻
// 路径（thaw）已不再调用它；保留的合法调用方是冻结失败回滚与无 Session
// 模式的启动恢复。

import (
	"log"
)

// approvalDecision 是一条停驻期间暂存的审批裁决（无 durable 面，必须内存
// 暂存、解冻回放，见 Runtime.pendingApprovals 注释）。
type approvalDecision struct {
	graphID      string
	nodeID       string
	activationID string
	approved     bool
	text         string
}

// approvalKeyOf 是暂存裁决的幂等键：同一 (graph, node, activation) 的最新
// 裁决覆盖旧值（重复裁决本就以 activation 守卫幂等）。
func approvalKeyOf(graphID, nodeID, activationID string) string {
	return graphID + "\x00" + nodeID + "\x00" + activationID
}

// isSuspendedLocked 报告图是否在停驻表（调用方须持 rt.mu）。
func (rt *Runtime) isSuspendedLocked(graphID string) bool {
	_, ok := rt.suspended[graphID]
	return ok
}

// suspendOneLocked 把一张图加入停驻表并停走其全部 wait timer（调用方须持
// rt.mu）；幂等判断由调用方负责。SuspendGraphsForSession 与
// SuspendGraphsExceptSession 的公共内部逻辑。
func (rt *Runtime) suspendOneLocked(graphID string) {
	rt.suspended[graphID] = struct{}{}
	rt.cancelGraphWaitTimersLocked(graphID)
}

// SuspendGraphsForSession 停驻归属 sessionID 的全部非终态图（纯运行时闸门，
// 语义见文件头注释），返回本次新停驻的 graph ID 列表（按 ID 排序）。
//
// 幂等：已停驻的图跳过（不计入返回）；终态图不停驻（无需闸门，其输入本
// 就被终态检查吞掉）。空 sessionID 匹配无归属图——无 Session 模式下全部
// 图归属空串，停驻空串即全量停驻（与现状语义一致）。归属过滤用
// Store.List 的 SessionID 快照。
func (rt *Runtime) SuspendGraphsForSession(sessionID string) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []string
	for _, sum := range rt.store.List() {
		if sum.SessionID != sessionID || sum.Status.IsTerminal() {
			continue
		}
		if rt.isSuspendedLocked(sum.GraphID) {
			continue // 幂等：已停驻
		}
		rt.suspendOneLocked(sum.GraphID)
		out = append(out, sum.GraphID)
	}
	return out // Store.List 已按 graph_id 排序，结果确定
}

// SuspendGraphsExceptSession 停驻「不属于 sessionID」的全部非终态图（补集
// 停驻，含无归属图），返回本次新停驻的 graph ID 列表（按 ID 排序）。
//
// 用途：启动恢复——停驻表是纯内存态、随进程重启清空，本函数把不属于当前
// 活跃 session 的历史图一次性重新停驻（吞终态事件、停走 wait timer、不被
// 启动恢复推进）。2026-08 起启动永远是全新 Session 且进入会话不自动续跑，
// 无归属历史图不再归并给当前 session（AdoptSessionlessGraphs 已删），因此
// 空归属图在会话模式下同样停驻——它们是僵尸图，随其时代退出，没有恢复入口。
//
// sessionID 为空串时整体空操作：没有「当前 session」就无所谓补集——无
// Session 模式下全部图正常驱动，行为同今。
// 幂等：已在停驻表的跳过；终态图跳过。
func (rt *Runtime) SuspendGraphsExceptSession(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []string
	for _, sum := range rt.store.List() {
		if sum.SessionID == sessionID || sum.Status.IsTerminal() {
			continue
		}
		if rt.isSuspendedLocked(sum.GraphID) {
			continue // 幂等：已停驻
		}
		rt.suspendOneLocked(sum.GraphID)
		out = append(out, sum.GraphID)
	}
	return out // Store.List 已按 graph_id 排序，结果确定
}

// ResumeGraphsForSession 解冻归属 sessionID 的停驻图：逐张解除停驻后走既有
// ResumeGraph 幂等补发路径（崩溃恢复同款），随后回放停驻期间暂存的审批
// 裁决，返回本次实际解冻恢复的 graph ID 列表（按 ID 排序）。
//
// 只处理「仍属该 session + 非终态 + 在停驻表」的图：归属已变的图保持
// 停驻；未停驻的图跳过（正常驱动中，无需对账）；终态图跳过并从停驻表
// 清除（冻结期经控制面取消的图不再占闸门）。单图恢复失败只记 WARNING、
// 不中断其余图（图仍非终态，遗留对账由下次恢复兜底）。
//
// 自愈保证（停驻期间被吞输入的对账）：
//   - 任务型 activation 的任务已终态：ResumeGraph 的 reconcileTaskLocked
//     从公告板回填（含 cancelled→failed 映射）；
//   - 过期的 wait deadline：resumeWaitingLocked 按原 deadline 重建，已过期
//     立即超时结算；
//   - 审批裁决：下方 pendingApprovals 回放（唯一无 durable 面的输入）；
//   - 子图终态回调：父图恢复时 ensureSubgraphLocked 补结算。
func (rt *Runtime) ResumeGraphsForSession(sessionID string) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	var out []string
	for _, sum := range rt.store.List() {
		if sum.SessionID != sessionID || sum.Status.IsTerminal() {
			continue
		}
		if !rt.isSuspendedLocked(sum.GraphID) {
			continue // 未停驻：正常驱动中
		}
		delete(rt.suspended, sum.GraphID)
		rt.synchronousSteps = 0
		if err := rt.resumeGraphLocked(sum.GraphID); err != nil {
			log.Printf("[graph] WARNING: 解冻恢复图 %s 失败: %v（图保持非终态，遗留对账由下次恢复兜底）", sum.GraphID, err)
		}
		out = append(out, sum.GraphID)
	}

	// 清理停驻表：终态或已消失的图不再占闸门（其输入本就被终态/缺失
	// 检查吞掉，表项只是纯内存残留）。
	resumed := make(map[string]struct{}, len(out))
	for _, id := range out {
		resumed[id] = struct{}{}
	}
	for _, sum := range rt.store.List() {
		if sum.Status.IsTerminal() {
			delete(rt.suspended, sum.GraphID)
		}
	}

	// 回放停驻期间暂存的审批裁决：仅回放本次解冻的图（其它 session 仍
	// 停驻的继续暂存）；图已终态/消失的裁决丢弃。回放走正常锁内入口，
	// activation/状态守卫天然过滤过期项。
	for key, d := range rt.pendingApprovals {
		if _, ok := resumed[d.graphID]; !ok {
			doc, ok := rt.store.Get(d.graphID)
			if !ok || doc.Status.IsTerminal() {
				delete(rt.pendingApprovals, key) // 裁决已无处可用
			}
			continue
		}
		delete(rt.pendingApprovals, key)
		if err := rt.onApprovalDecidedLocked(d.graphID, d.nodeID, d.activationID, d.approved, d.text); err != nil {
			log.Printf("[graph] WARNING: 回放图 %s 节点 %s 的暂存审批裁决失败: %v", d.graphID, d.nodeID, err)
		}
	}
	return out
}
