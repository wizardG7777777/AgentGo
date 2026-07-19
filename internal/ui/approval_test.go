package ui

import (
	"testing"
	"time"

	"agentgo/internal/shell"
)

// newApprovalReq 构造一个带 cap=1 ReplyCh 的审批请求。
func newApprovalReq(id string) shell.ApprovalRequest {
	return shell.ApprovalRequest{
		RequestID: id,
		TaskID:    "task-" + id,
		AgentID:   "agent-" + id,
		Command:   "git push",
		Pattern:   `git\s+push`,
		ReplyCh:   make(chan shell.ApprovalReply, 1),
	}
}

func TestBroker_ResolveApprovedFlow(t *testing.T) {
	apprCh := make(chan shell.ApprovalRequest, 4)
	h := startHub(t, Deps{ApprovalCh: apprCh})
	sub, cancel := h.Subscribe(16)
	defer cancel()
	recvUpdate(t, sub) // 吃掉 SnapshotSync

	req := newApprovalReq("r1")
	apprCh <- req

	u := recvUpdate(t, sub)
	if u.Kind != KindApprovalNew || u.Approval.RequestID != "r1" {
		t.Fatalf("更新 = %+v，期望 ApprovalNew/r1", u)
	}
	// 广播前快照已更新：此时待审批列表必含 r1
	if got := h.Snapshot().PendingApprovals; len(got) != 1 || got[0].RequestID != "r1" {
		t.Fatalf("PendingApprovals = %+v", got)
	}

	if ok := h.ResolveApproval("r1", shell.ApprovalReply{Approved: true}); !ok {
		t.Fatal("ResolveApproval 应送达")
	}
	// 请求方收到回复
	select {
	case reply := <-req.ReplyCh:
		if !reply.Approved {
			t.Fatal("请求方收到的回复 Approved = false")
		}
	case <-time.After(testTimeout):
		t.Fatal("请求方未收到回复")
	}
	// 广播了结
	u = recvUpdate(t, sub)
	if u.Kind != KindApprovalResolved {
		t.Fatalf("Kind = %v，期望 ApprovalResolved", u.Kind)
	}
	if u.Resolved.RequestID != "r1" || u.Resolved.Outcome != OutcomeApproved {
		t.Fatalf("Resolved = %+v，期望 r1/approved", u.Resolved)
	}
	// 待审批列表已移除
	if got := len(h.Snapshot().PendingApprovals); got != 0 {
		t.Fatalf("了结后 PendingApprovals 长度 = %d", got)
	}

	// 同一 ID 再次 Resolve：ID 已移除，按未知处理，返回 false 且不再广播
	if ok := h.ResolveApproval("r1", shell.ApprovalReply{Approved: true}); ok {
		t.Fatal("重复 Resolve 应返回 false")
	}
	select {
	case u := <-sub:
		t.Fatalf("重复 Resolve 不应再广播，收到 %+v", u)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBroker_OutcomeDerivation(t *testing.T) {
	cases := []struct {
		name    string
		reply   shell.ApprovalReply
		outcome string
	}{
		{"批准", shell.ApprovalReply{Approved: true}, OutcomeApproved},
		{"拒绝", shell.ApprovalReply{Approved: false}, OutcomeRejected},
		{"指导优先于批准", shell.ApprovalReply{Approved: true, Message: "请先 dry-run"}, OutcomeGuidance},
		{"纯指导", shell.ApprovalReply{Message: "换个命令"}, OutcomeGuidance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apprCh := make(chan shell.ApprovalRequest, 1)
			h := startHub(t, Deps{ApprovalCh: apprCh})
			sub, cancel := h.Subscribe(16)
			defer cancel()
			recvUpdate(t, sub)

			apprCh <- newApprovalReq("rx")
			recvUpdate(t, sub) // ApprovalNew

			if ok := h.ResolveApproval("rx", tc.reply); !ok {
				t.Fatal("ResolveApproval 应送达")
			}
			u := recvUpdate(t, sub)
			if u.Resolved.Outcome != tc.outcome {
				t.Fatalf("Outcome = %q，期望 %q", u.Resolved.Outcome, tc.outcome)
			}
		})
	}
}

func TestBroker_ExpiredOnUndeliverable(t *testing.T) {
	apprCh := make(chan shell.ApprovalRequest, 1)
	h := startHub(t, Deps{ApprovalCh: apprCh})
	sub, cancel := h.Subscribe(16)
	defer cancel()
	recvUpdate(t, sub)

	req := newApprovalReq("r-exp")
	// 模拟"已被应答"：cap=1 的 ReplyCh 已被占满，Resolve 的非阻塞发送必然失败
	req.ReplyCh <- shell.ApprovalReply{Approved: true}
	apprCh <- req
	recvUpdate(t, sub) // ApprovalNew

	if ok := h.ResolveApproval("r-exp", shell.ApprovalReply{Approved: false}); ok {
		t.Fatal("ReplyCh 已满时应返回 false")
	}
	u := recvUpdate(t, sub)
	if u.Kind != KindApprovalResolved || u.Resolved.Outcome != OutcomeExpired {
		t.Fatalf("更新 = %+v，期望 ApprovalResolved/expired", u)
	}
	if got := len(h.Snapshot().PendingApprovals); got != 0 {
		t.Fatalf("过期后 PendingApprovals 长度 = %d", got)
	}
}

func TestBroker_UnknownID(t *testing.T) {
	h := startHub(t, Deps{})
	sub, cancel := h.Subscribe(4)
	defer cancel()
	recvUpdate(t, sub)

	if ok := h.ResolveApproval("不存在的ID", shell.ApprovalReply{Approved: true}); ok {
		t.Fatal("未知 ID 应返回 false")
	}
	select {
	case u := <-sub:
		t.Fatalf("未知 ID 不应广播，收到 %+v", u)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBroker_PendingListOrderAndRemoval(t *testing.T) {
	apprCh := make(chan shell.ApprovalRequest, 4)
	h := startHub(t, Deps{ApprovalCh: apprCh})

	apprCh <- newApprovalReq("ra")
	apprCh <- newApprovalReq("rb")
	apprCh <- newApprovalReq("rc")
	waitFor(t, "三条待审批", func() bool { return len(h.Snapshot().PendingApprovals) == 3 })

	got := h.Snapshot().PendingApprovals
	want := []string{"ra", "rb", "rc"}
	for i, id := range want {
		if got[i].RequestID != id {
			t.Fatalf("PendingApprovals[%d] = %q，期望 %q（应按到达顺序）", i, got[i].RequestID, id)
		}
	}

	// 删除中间一条，剩余保持顺序
	if !h.ResolveApproval("rb", shell.ApprovalReply{Approved: false}) {
		t.Fatal("ResolveApproval(rb) 应送达")
	}
	got = h.Snapshot().PendingApprovals
	if len(got) != 2 || got[0].RequestID != "ra" || got[1].RequestID != "rc" {
		t.Fatalf("删除后 PendingApprovals = %+v", got)
	}

	// 重复 RequestID 不产生第二条待审批
	apprCh <- newApprovalReq("ra")
	time.Sleep(50 * time.Millisecond)
	if got := len(h.Snapshot().PendingApprovals); got != 2 {
		t.Fatalf("重复 RequestID 后 PendingApprovals 长度 = %d，期望 2", got)
	}
}
