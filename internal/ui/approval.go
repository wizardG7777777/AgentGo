package ui

import (
	"time"

	"agentgo/internal/shell"
)

// 审批了结的 Outcome 取值。
const (
	// OutcomeApproved 用户批准执行。
	OutcomeApproved = "approved"
	// OutcomeRejected 用户拒绝执行。
	OutcomeRejected = "rejected"
	// OutcomeGuidance 用户以自由文本指导代替批准 / 拒绝（Message 非空）。
	OutcomeGuidance = "guidance"
	// OutcomeExpired 回复未能送达（ReplyCh 已被应答或代理已放弃等待）。
	OutcomeExpired = "expired"
)

// pendingApproval 是 Hub 私有持有的待审批请求：对前端暴露 ApprovalItem，
// ReplyCh 只经 ResolveApproval 使用，不外泄。
type pendingApproval struct {
	item    ApprovalItem
	replyCh chan shell.ApprovalReply
}

// addApproval 登记一条新审批请求：包装为 ApprovalItem、私有保存 ReplyCh、
// 更新快照待审批列表，并广播 KindApprovalNew。重复的 RequestID 直接忽略
// （ReplyCh 只有一份，重复登记会让前一条永远无法回复）。
func (h *Hub) addApproval(req shell.ApprovalRequest) {
	item := ApprovalItem{
		RequestID:  req.RequestID,
		TaskID:     req.TaskID,
		AgentID:    req.AgentID,
		Command:    req.Command,
		Pattern:    req.Pattern,
		ReceivedAt: time.Now(),
	}

	h.mu.Lock()
	if _, dup := h.pending[item.RequestID]; dup {
		h.mu.Unlock()
		return
	}
	h.pending[item.RequestID] = pendingApproval{item: item, replyCh: req.ReplyCh}
	h.pendingOrder = append(h.pendingOrder, item.RequestID)
	h.snapshot.PendingApprovals = h.pendingListLocked()
	h.mu.Unlock()

	h.broadcast(Update{Kind: KindApprovalNew, Approval: item, At: time.Now()})
}

// ResolveApproval 回复一条待审批请求。
//
// 流程：按 RequestID 查找 → 对 cap=1 的 ReplyCh 非阻塞发送 → 无论送达
// 与否都从待审批表移除并广播 KindApprovalResolved：
//   - 送达：Outcome 由回复内容推导（Message 非空=guidance，否则按
//     Approved 取 approved / rejected），返回 true；
//   - 未送达（cap=1 已被应答，或代理已放弃等待）：Outcome=expired，
//     返回 false；
//   - 未知 RequestID：不广播，返回 false。
func (h *Hub) ResolveApproval(requestID string, reply shell.ApprovalReply) bool {
	h.mu.Lock()
	pa, ok := h.pending[requestID]
	if !ok {
		h.mu.Unlock()
		return false
	}
	delivered := false
	select {
	case pa.replyCh <- reply:
		delivered = true
	default:
	}
	delete(h.pending, requestID)
	h.pendingOrder = removeString(h.pendingOrder, requestID)
	h.snapshot.PendingApprovals = h.pendingListLocked()
	h.mu.Unlock()

	outcome := OutcomeExpired
	if delivered {
		outcome = outcomeFromReply(reply)
	}
	h.broadcast(Update{
		Kind:     KindApprovalResolved,
		Resolved: ApprovalResolved{RequestID: requestID, Outcome: outcome},
		At:       time.Now(),
	})
	return delivered
}

// outcomeFromReply 按 tui 审批弹窗语义推导了结类别：
// 附带自由文本指导优先于批准 / 拒绝。
func outcomeFromReply(r shell.ApprovalReply) string {
	if r.Message != "" {
		return OutcomeGuidance
	}
	if r.Approved {
		return OutcomeApproved
	}
	return OutcomeRejected
}

// pendingListLocked 按到达顺序重建待审批列表。调用方必须持有 h.mu。
func (h *Hub) pendingListLocked() []ApprovalItem {
	if len(h.pendingOrder) == 0 {
		return nil
	}
	out := make([]ApprovalItem, 0, len(h.pendingOrder))
	for _, id := range h.pendingOrder {
		out = append(out, h.pending[id].item)
	}
	return out
}

// removeString 删除切片中首个等于 s 的元素，保持其余元素顺序。
func removeString(ss []string, s string) []string {
	for i, v := range ss {
		if v == s {
			return append(ss[:i], ss[i+1:]...)
		}
	}
	return ss
}
