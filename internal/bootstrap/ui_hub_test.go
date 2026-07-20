package bootstrap

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/output"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/ui"
)

// UI Hub 集成测试：System 经 buildUIHub 装配出的 Hub 消费 output/status
// 与 Interaction 服务；订阅者应看到对应 Update，Interaction 经 Controller
// 了结后离开快照；
// cancel 后 Hub 随 System.wg 退出（goroutine 不泄漏）。
func TestSystem_UIHub_EndToEnd(t *testing.T) {
	outputCh := make(chan output.Event, 4)
	statusCh := make(chan string, 4)
	interactions := interaction.NewService(interaction.NewMemoryStore())

	s := &System{
		Store:           store.NewMemoryTaskStore(nil, 100, 1, 300),
		EventCh:         make(chan model.Event, 4),
		MailboxRegistry: mailbox.NewRegistry(16),
		Scheduler:       &scheduler.Bundle{Modes: modes.DefaultStore()},
		OutputCh:        outputCh,
		StatusCh:        statusCh,
		Interactions:    interactions,
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

	// 两条通道与一条结构化请求 → 订阅者看到三种 Update。
	outputCh <- output.Event{Kind: output.KindResult, AgentID: "worker-1", Text: "任务完成"}
	created, err := interactions.Create(context.Background(), interaction.CreateRequest{
		ID: "ix_hub_e2e", Kind: interaction.KindAuthorization,
		Purpose: shell.PurposeShellCommand, Prompt: "allow?",
		Options:    []interaction.Option{{ID: shell.ActionAllowOnce, Label: "allow", ActionRef: shell.ActionAllowOnce}},
		Resolution: interaction.ResolutionSpec{Handler: shell.ResolutionHandlerShellCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	statusCh <- "[启动] 系统就绪"

	remaining := map[ui.UpdateKind]bool{
		ui.KindOutputResult:        true,
		ui.KindInteractionsChanged: true,
		ui.KindLogLine:             true,
	}
	for len(remaining) > 0 {
		select {
		case u := <-updates:
			delete(remaining, u.Kind)
		case <-time.After(3 * time.Second):
			t.Fatalf("等待 Update 超时，尚未收到: %v", remaining)
		}
	}
	waitForUI(t, "结果进入统一 UI 快照", func() bool {
		result := s.UIHub.Snapshot().LastResult
		return result != nil && result.Text == "任务完成" && result.AgentID == "worker-1"
	})

	// 待交互进入快照；经 Controller 回复并应用后从快照消失。
	waitForUI(t, "Interaction 进入快照", func() bool {
		return len(s.UIHub.Snapshot().PendingInteractions) == 1
	})
	if _, err := s.UIHub.RespondInteraction(context.Background(), interaction.ResolveInput{
		RequestID: created.ID, ExpectedVersion: created.Version,
		OptionID: shell.ActionAllowOnce, RespondedBy: "test",
	}); err != nil {
		t.Fatalf("RespondInteraction: %v", err)
	}
	waitForUI(t, "Interaction 了结后离开快照", func() bool {
		return len(s.UIHub.Snapshot().PendingInteractions) == 0
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

// TestSystem_UIHub_ThreeModeAxes 三轴模式的装配闭环：经 buildUIHub 装配的
// Hub，SetExecMode / SetTopoMode 写回同一个 modes.Store，快照经轮询刷新后
// 读出三轴当前值；非法值返回中文错误且不写 store。
func TestSystem_UIHub_ThreeModeAxes(t *testing.T) {
	modeStore := modes.DefaultStore()
	s := &System{
		Store:           store.NewMemoryTaskStore(nil, 100, 1, 300),
		EventCh:         make(chan model.Event, 4),
		MailboxRegistry: mailbox.NewRegistry(16),
		Scheduler:       &scheduler.Bundle{Modes: modeStore},
		OutputCh:        make(chan output.Event, 4),
		StatusCh:        make(chan string, 4),
		Interactions:    interaction.NewService(interaction.NewMemoryStore()),
	}
	s.UIHub = s.buildUIHub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startUIHub(ctx)

	// 初值为三轴默认（immediate / normal / team）。
	waitForUI(t, "快照三轴初值", func() bool {
		snap := s.UIHub.Snapshot()
		return snap.Mode == "immediate" && snap.ExecMode == "normal" && snap.TopoMode == "team"
	})

	// SetExecMode / SetTopoMode 写回同一个 modes.Store。
	if err := s.UIHub.SetExecMode("readonly"); err != nil {
		t.Fatalf("SetExecMode(readonly) 返回错误：%v", err)
	}
	if got := modeStore.GetExec(); got != modes.ExecReadonly {
		t.Fatalf("GetExec = %v，期望 ExecReadonly", got)
	}
	if err := s.UIHub.SetTopoMode("solo"); err != nil {
		t.Fatalf("SetTopoMode(solo) 返回错误：%v", err)
	}
	if got := modeStore.GetTopo(); got != modes.TopoSolo {
		t.Fatalf("GetTopo = %v，期望 TopoSolo", got)
	}

	// 快照经轮询刷新后读出三轴新值。
	waitForUI(t, "快照三轴更新", func() bool {
		snap := s.UIHub.Snapshot()
		return snap.ExecMode == "readonly" && snap.TopoMode == "solo"
	})

	// 非法值：中文错误且 store 不变。
	if err := s.UIHub.SetExecMode("nope"); err == nil {
		t.Fatal("SetExecMode(nope) 应返回错误")
	}
	if got := modeStore.GetExec(); got != modes.ExecReadonly {
		t.Fatalf("非法值写入后 GetExec = %v，期望仍为 ExecReadonly", got)
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
