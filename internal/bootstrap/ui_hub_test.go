package bootstrap

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/ui"
)

// UI Hub 集成测试：System 经 buildUIHub 装配出的 Hub 是三个 UI 通道
// （output/approval/status）的唯一消费者——向三条通道各推一条事件，
// 订阅者应看到对应的三种 Update；审批经 Controller 了结后离开快照；
// cancel 后 Hub 随 System.wg 退出（goroutine 不泄漏）。
func TestSystem_UIHub_EndToEnd(t *testing.T) {
	outputCh := make(chan output.Event, 4)
	approvalCh := make(chan shell.ApprovalRequest, 4)
	statusCh := make(chan string, 4)

	s := &System{
		Store:           store.NewMemoryTaskStore(nil, 100, 1, 300),
		EventCh:         make(chan model.Event, 4),
		MailboxRegistry: mailbox.NewRegistry(16),
		Scheduler:       &scheduler.Bundle{Mode: scheduler.NewModeStore()},
		OutputCh:        outputCh,
		ApprovalCh:      approvalCh,
		StatusCh:        statusCh,
	}
	// 与 BootstrapWithOptions 同一装配路径（Step 11）。
	s.UIHub = s.buildUIHub()

	ctx, cancel := context.WithCancel(context.Background())
	// 与 System.Start 同一启动路径（监督 goroutine 组）。
	s.startUIHub(ctx)

	updates, unsubscribe := s.UIHub.Subscribe(16)
	defer unsubscribe()

	// 首条必为全量快照（订阅建立即下发）。注意其内容可能早于 Run 的
	// 启动刷新（订阅与启动并发时快照为零值）；生产路径 RunCLI 订阅远在
	// Start 之后，不受影响。这里改为等待快照经轮询刷新后断言 Mode。
	u := recvUIUpdate(t, updates)
	if u.Kind != ui.KindSnapshotSync {
		t.Fatalf("首条更新 Kind = %v，期望 SnapshotSync", u.Kind)
	}
	waitForUI(t, "快照 Mode 经 ModeGet 刷新", func() bool {
		return s.UIHub.Snapshot().Mode == "immediate"
	})

	// 三条通道各推一条事件 → 订阅者看到三种 Update。
	outputCh <- output.Event{Kind: output.KindResult, AgentID: "worker-1", Text: "任务完成"}
	approvalCh <- shell.ApprovalRequest{
		RequestID: "r-1", AgentID: "worker-1", Command: "rm -rf /tmp/x",
		ReplyCh: make(chan shell.ApprovalReply, 1),
	}
	statusCh <- "[启动] 系统就绪"

	remaining := map[ui.UpdateKind]bool{
		ui.KindOutputResult: true,
		ui.KindApprovalNew:  true,
		ui.KindLogLine:      true,
	}
	for len(remaining) > 0 {
		select {
		case u := <-updates:
			delete(remaining, u.Kind)
		case <-time.After(3 * time.Second):
			t.Fatalf("等待 Update 超时，尚未收到: %v", remaining)
		}
	}

	// 待审批进入快照；经 Controller 回复送达后从快照消失。
	waitForUI(t, "待审批进入快照", func() bool {
		return len(s.UIHub.Snapshot().PendingApprovals) == 1
	})
	if !s.UIHub.ResolveApproval("r-1", shell.ApprovalReply{Approved: true}) {
		t.Fatal("存活审批应送达（true）")
	}
	waitForUI(t, "审批了结后离开快照", func() bool {
		return len(s.UIHub.Snapshot().PendingApprovals) == 0
	})

	// cancel → Hub 退出，wg 归零（goroutine 不泄漏）。
	cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel 后 UI Hub 未退出（goroutine 泄漏）")
	}
}

// recvUIUpdate 在超时护栏内接收一条 Update。
func recvUIUpdate(t *testing.T, ch <-chan ui.Update) ui.Update {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("等待 Update 超时")
		return ui.Update{}
	}
}

// waitForUI 轮询条件直到满足或超时（等 Hub 主循环推进内部状态）。
func waitForUI(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待条件超时：%s", what)
}
