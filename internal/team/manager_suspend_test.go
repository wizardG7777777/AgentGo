package team

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/runner"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// SuspendAll 停掉全部 team 运行时（route 注销、runner 退出、activity 移除），
// 但 durable TeamSpec 保持 StatusReady 不动（区别于 graph_ended 回收的
// StatusStopped），邮箱保留注册（未读邮件不丢），且重复调用幂等。
func TestManagerSuspendAllStopsRuntimePreservesReadySpecAndMailbox(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-suspend")
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), Activity: activity,
		MBRegistry: mailboxes, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, routes, 2)
	t.Cleanup(manager.Shutdown)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "freeze session team", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	agentID := result.AgentIDs[0]
	if err := mailboxes.Send(mailbox.Message{
		From: "scheduler", To: agentID, Summary: "unread", Content: "unread",
		SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	manager.SuspendAll()

	// 运行时已停：route 注销、active 清空、activity 移除。
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("SuspendAll 后仍残留 route: %+v", current)
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("SuspendAll 后 active=%d, 应为 0", got)
	}
	if _, ok := activity.Snapshot(agentID); ok {
		t.Fatalf("SuspendAll 后 %s 的 activity 仍注册", agentID)
	}
	// durable 不动：spec 保持 StatusReady 且无 StopReason。
	stored, err := durable.Get(result.TeamID)
	if err != nil || stored.Status != StatusReady || stored.StopReason != "" {
		t.Fatalf("SuspendAll 不得写 durable 状态: stored=%+v err=%v", stored, err)
	}
	// 邮箱保留注册，未读邮件不丢。
	foundUnread := false
	for _, status := range mailboxes.ScanNonEmpty() {
		if status.AgentID == agentID && status.Count == 1 {
			foundUnread = true
		}
	}
	if !foundUnread {
		t.Fatalf("SuspendAll 后 team 邮箱未保留注册: %+v", mailboxes.ScanAll())
	}
	// 挂起窗口 Provision fail-closed（同未 Start）。
	if _, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "during suspend", Replicas: 1,
	}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("挂起窗口 Provision err=%v, 应为 ErrNotStarted", err)
	}

	// 幂等：重复调用是 no-op，不 panic、不改变上述任何事实。
	manager.SuspendAll()
	if stored, err := durable.Get(result.TeamID); err != nil || stored.Status != StatusReady {
		t.Fatalf("重复 SuspendAll 后 durable 变化: stored=%+v err=%v", stored, err)
	}
	if got := mailboxes.ScanAll(); len(got) != 1 {
		t.Fatalf("重复 SuspendAll 后邮箱丢失: %+v", got)
	}
}

// 解冻路径：SuspendAll →（模拟 store 重绑换内容，即目标 session 的 team 清单）
// → Start 复用进程启动恢复路径重新物化新清单；旧 session 的 durable 清单保持
// ready 原样，不被本次恢复触碰。
func TestManagerSuspendAllThenStartRematerializesReboundManifest(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerA := newControllerTask(t, taskStore, "controller-session-a")
	controllerB := newControllerTask(t, taskStore, "controller-session-b")
	routes := newFakeRoutes()

	// session A 的清单：一个 ready team，正常 Start + Provision 后挂起。
	storeA := NewMemoryStore()
	manager := testManagerWithStore(t, catalog, taskStore, storeA, routes, 4)
	t.Cleanup(manager.Shutdown)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	teamA, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerA,
		TemplateRef:      "builtin/explorer@1", Purpose: "session A team", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision(A): %v", err)
	}
	manager.SuspendAll()

	// 模拟解冻目标 session：store 重绑换内容（B 目录的清单，现实流程由
	// onSessionSwitched 重绑；测试直接替换 Manager 持有的 store 实例）。
	// B 的 team 属另一个 controller，用另一个模板与真实 digest 以通过恢复核对。
	storeB := NewMemoryStore()
	tmpl, err := catalog.Resolve("builtin/generalist@1")
	if err != nil {
		t.Fatalf("resolve generalist: %v", err)
	}
	specB := testSpec("session-b-team", controllerB, "session B team")
	specB.TemplateRef = tmpl.Ref
	specB.TemplateDigest = tmpl.Digest
	specB.Replicas = 1
	if _, _, err := storeB.Ensure(specB); err != nil {
		t.Fatalf("Ensure(B): %v", err)
	}
	manager.store = storeB

	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("解冻 Start: %v", err)
	}
	// 新清单物化：B 的 route 注册、active=1；A 的 route 不复活。
	if got := manager.ActiveCount(); got != 1 {
		t.Fatalf("解冻后 active=%d, 应为 1", got)
	}
	current, _ := routes.snapshot()
	if _, ok := current[specB.EventType]; !ok {
		t.Fatalf("解冻后缺少 B 的 route: %+v", current)
	}
	if _, ok := current[teamA.EventType]; ok {
		t.Fatalf("解冻后 A 的 route 不应复活: %+v", current)
	}
	// A 的 durable 清单保持 ready 原样（冻结语义），未被本次恢复触碰。
	storedA, err := storeA.Get(teamA.TeamID)
	if err != nil || storedA.Status != StatusReady {
		t.Fatalf("session A spec 应保持 ready: %+v err=%v", storedA, err)
	}
	manager.Shutdown()
	// Shutdown 不改 durable：B 的 spec 仍 ready，可被下次进程/解冻恢复。
	storedB, err := storeB.Get(specB.ID)
	if err != nil || storedB.Status != StatusReady {
		t.Fatalf("Shutdown 后 B spec 应保持 ready: %+v err=%v", storedB, err)
	}
}

// 挂起窗口（SuspendAll 之后、Start 之前）收到 controller 终态事件与
// graph_ended 都必须 no-op：不 panic、不写 durable 回收状态；owner 在冻结
// 期间到达终态的 Team 由解冻时 Start 的恢复核对兜底回收。
func TestManagerSuspendAllRunReactorTerminalEventDoesNotRecycle(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-suspend-event")
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManagerWithStore(t, catalog, taskStore, durable, routes, 2)
	t.Cleanup(manager.Shutdown)
	graphStatus := "running"
	if err := manager.SetGraphStateResolver(func(graphID string) (string, bool, bool) {
		if graphID != "g-suspend" {
			return "", false, false
		}
		return graphStatus, graphStatus == "completed", true
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	legacy, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "legacy team", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision legacy: %v", err)
	}
	graphTeam, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID, GraphID: "g-suspend",
		TemplateRef: "builtin/generalist@1", Purpose: "graph team", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision graph: %v", err)
	}
	manager.SuspendAll()

	// 冻结窗口：两类终态事件都不回收（active 已空、durable 必须保持 ready）。
	terminalControllerTask(t, taskStore, controllerID, model.TaskStatusCompleted)
	if err := manager.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: controllerID}); err != nil {
		t.Fatalf("挂起窗口 Run(controller 终态) err=%v", err)
	}
	graphStatus = "completed"
	if err := manager.Run(trace.Event{Kind: trace.KindGraphEnded, GraphID: "g-suspend"}); err != nil {
		t.Fatalf("挂起窗口 Run(graph_ended) err=%v", err)
	}
	for _, id := range []string{legacy.TeamID, graphTeam.TeamID} {
		stored, err := durable.Get(id)
		if err != nil || stored.Status != StatusReady {
			t.Fatalf("挂起窗口终态事件不得回收 durable spec: id=%s stored=%+v err=%v", id, stored, err)
		}
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("挂起窗口 Run 后 active=%d, 应为 0", got)
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("挂起窗口 Run 后残留 route: %+v", current)
	}

	// 解冻：Start 恢复核对兜底——controller 已终态的 legacy team 与 graph 已
	// 终态的 graph team 都在恢复时被 durable 回收，且不物化运行时。
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("解冻 Start: %v", err)
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("解冻恢复 active=%d, 应为 0（两个 team 的 owner 均已终态）", got)
	}
	legacyStored, err := durable.Get(legacy.TeamID)
	if err != nil || legacyStored.Status != StatusStopped || legacyStored.StopReason != "controller_terminal:completed" {
		t.Fatalf("解冻恢复应回收 legacy team: %+v err=%v", legacyStored, err)
	}
	graphStored, err := durable.Get(graphTeam.TeamID)
	if err != nil || graphStored.Status != StatusStopped || graphStored.StopReason != "graph_terminal:completed" {
		t.Fatalf("解冻恢复应回收 graph team: %+v err=%v", graphStored, err)
	}
}

// closed（进程关闭进行中/完成后）调用 SuspendAll：按注释约定退化为等待
// ShutdownPreservingMailboxes 完成的 no-op——单向关闭语义优先，不复活、
// 不改 durable、不重复排空。
func TestManagerSuspendAllAfterClosedKeepsShutdownSemantics(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-closed-suspend")
	durable := NewMemoryStore()
	mailboxes := mailbox.NewRegistry(8)
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, newFakeRoutes(), 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "closed then suspend", Replicas: 1,
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	manager.ShutdownPreservingMailboxes()

	// shutdownDone 已关闭，SuspendAll 必须立即返回且不产生任何副作用。
	manager.SuspendAll()
	specs, err := durable.List()
	if err != nil || len(specs) != 1 || specs[0].Status != StatusReady {
		t.Fatalf("closed 后 SuspendAll 不得改 durable: %+v err=%v", specs, err)
	}
	if err := manager.Start(context.Background()); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("closed 后 Start err=%v, 应为 ErrManagerClosed", err)
	}
	manager.FinalizeShutdownMailboxes()
}

// 从未 Start（含零值 Manager）时 SuspendAll 是安全 no-op：不 panic，且之后
// 首次 Start 不受影响（复位 started/parentCtx 不改变「从未启动」的事实）。
func TestManagerSuspendAllBeforeStartKeepsStartable(t *testing.T) {
	zero := &Manager{}
	zero.SuspendAll() // 零值：nil map/nil channel 路径不得 panic

	catalog := testCatalog(t)
	durable := NewMemoryStore()
	manager := testManager(t, catalog, durable, newFakeRoutes(), 2)
	t.Cleanup(manager.Shutdown)
	manager.SuspendAll()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("SuspendAll 后首次 Start: %v", err)
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("空清单首次 Start 后 active=%d, 应为 0", got)
	}
}

// SuspendAll 把被保留邮箱的 agentID 记入 suspendedMailboxIDs；
// FinalizeSuspendedMailboxes 注销这些邮箱并清空记录，且幂等（无记录时
// no-op，含从未挂起的 Manager）。
func TestManagerSuspendAllRecordsAndFinalizeSuspendedMailboxes(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-suspend-finalize")
	durable := NewMemoryStore()
	mailboxes := mailbox.NewRegistry(8)
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, newFakeRoutes(), 2)
	t.Cleanup(manager.Shutdown)

	// 从未挂起时 Finalize 是纯 no-op（不 panic、不改注册表）。
	manager.FinalizeSuspendedMailboxes()

	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "record suspended mailboxes", Replicas: 2,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	manager.SuspendAll()

	if len(manager.suspendedMailboxIDs) != 2 {
		t.Fatalf("SuspendAll 应记录 2 个保留邮箱 ID, 实际=%v", manager.suspendedMailboxIDs)
	}
	for _, agentID := range result.AgentIDs {
		if _, ok := manager.suspendedMailboxIDs[agentID]; !ok {
			t.Fatalf("SuspendAll 未记录 %s: %v", agentID, manager.suspendedMailboxIDs)
		}
	}
	if got := mailboxes.ScanAll(); len(got) != 2 {
		t.Fatalf("Finalize 前挂起邮箱应保留注册: %+v", got)
	}

	manager.FinalizeSuspendedMailboxes()
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("FinalizeSuspendedMailboxes 应注销全部挂起邮箱: %+v", got)
	}
	if manager.suspendedMailboxIDs != nil {
		t.Fatalf("FinalizeSuspendedMailboxes 应清空记录: %v", manager.suspendedMailboxIDs)
	}

	// 幂等：重复调用不 panic、注册表不再变化。
	manager.FinalizeSuspendedMailboxes()
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("重复 Finalize 后注册表变化: %+v", got)
	}
}

// 冻结协议完整链路：SuspendAll（保留+记录）→ 导出 session 快照 →
// FinalizeSuspendedMailboxes 注销 → 切换 →（ResetAll + ImportSnapshot(目标
// 快照)）→ Start 经 ClaimRecovered 认领 recovered 邮箱——无 fail-closed
// 冲突，未读 FIFO 原样恢复；切回原 session（同 agentID）同样成立，即
// session 切换对邮箱域 ≡ 进程重启。Start 成功后记录被防御性清空。
func TestManagerSuspendFinalizeRebindStartClaimsRecoveredMailbox(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerA := newControllerTask(t, taskStore, "controller-a-chain")
	controllerB := newControllerTask(t, taskStore, "controller-b-chain")
	mailboxes := mailbox.NewRegistry(8)
	routes := newFakeRoutes()
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}

	// session A：provision 一个 team，并留一条未读邮件。
	storeA := NewMemoryStore()
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, storeA, routes, 2)
	t.Cleanup(manager.Shutdown)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start(A): %v", err)
	}
	teamA, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerA,
		TemplateRef:      "builtin/explorer@1", Purpose: "session A team", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision(A): %v", err)
	}
	agentA := teamA.AgentIDs[0]
	if err := mailboxes.Send(mailbox.Message{
		From: "scheduler", To: agentA, Summary: "a-mail", Content: "a-mail",
		SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Send(a-mail): %v", err)
	}

	// 冻结 A：挂起 → 导出 session 快照（未读随快照归档）→ Finalize 注销。
	manager.SuspendAll()
	snapsA := mailboxes.ExportSnapshot()
	if len(snapsA) != 1 || snapsA[0].OwnerID != agentA || len(snapsA[0].Messages) != 1 {
		t.Fatalf("冻结快照应含 A 的未读邮箱: %+v", snapsA)
	}
	manager.FinalizeSuspendedMailboxes()
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("Finalize 后 A 邮箱应已注销: %+v", got)
	}

	// 解冻 session B：重绑 store 换清单 + 清空消息域 + 导入 B 的快照（B 的
	// team 邮箱成为 recoveredUnclaimed）。
	storeB := NewMemoryStore()
	tmplB, err := catalog.Resolve("builtin/generalist@1")
	if err != nil {
		t.Fatalf("resolve generalist: %v", err)
	}
	specB := testSpec("session-b-team", controllerB, "session B team")
	specB.TemplateRef = tmplB.Ref
	specB.TemplateDigest = tmplB.Digest
	specB.Replicas = 1
	if _, _, err := storeB.Ensure(specB); err != nil {
		t.Fatalf("Ensure(B): %v", err)
	}
	agentB := teamAgentIDs(specB, tmplB.Name)[0]
	now := time.Now().UTC().Format(time.RFC3339)
	mailboxes.ResetAll()
	if err := mailboxes.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: agentB, EventType: specB.EventType,
		Messages: []session.MessageSnapshot{
			{From: "scheduler", To: agentB, Summary: "b-mail", SentAt: now},
		},
	}}); err != nil {
		t.Fatalf("ImportSnapshot(B): %v", err)
	}
	manager.store = storeB
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("解冻 Start(B): %v", err)
	}
	if got := manager.ActiveCount(); got != 1 {
		t.Fatalf("解冻 B 后 active=%d, 应为 1", got)
	}
	// Start 经 ClaimRecovered 认领了 B 的 recovered 邮箱：FIFO 原样恢复。
	activeB, ok := manager.active[specB.ID]
	if !ok || len(activeB.runners) != 1 {
		t.Fatalf("解冻 B 后 active team 不符: %+v", activeB)
	}
	msgs := activeB.runners[0].Agent().Mailbox.Drain()
	if len(msgs) != 1 || msgs[0].Summary != "b-mail" {
		t.Fatalf("B 的 recovered 邮箱 FIFO 不符: %+v", msgs)
	}
	// Start 成功重新物化后防御性清空挂起邮箱记录。
	if len(manager.suspendedMailboxIDs) != 0 {
		t.Fatalf("Start 后 suspendedMailboxIDs 应清空: %v", manager.suspendedMailboxIDs)
	}

	// 切回 session A（同 agentID）：再走一遍完整协议，ClaimRecovered 不冲突。
	manager.SuspendAll()
	manager.FinalizeSuspendedMailboxes()
	mailboxes.ResetAll()
	if err := mailboxes.ImportSnapshot(snapsA); err != nil {
		t.Fatalf("ImportSnapshot(A): %v", err)
	}
	manager.store = storeA
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("解冻 Start(A): %v", err)
	}
	activeA, ok := manager.active[teamA.TeamID]
	if !ok || len(activeA.runners) != 1 {
		t.Fatalf("切回 A 后 active team 不符: %+v", activeA)
	}
	msgs = activeA.runners[0].Agent().Mailbox.Drain()
	if len(msgs) != 1 || msgs[0].Summary != "a-mail" {
		t.Fatalf("切回 A 的 recovered 邮箱 FIFO 不符: %+v", msgs)
	}
	if len(manager.suspendedMailboxIDs) != 0 {
		t.Fatalf("切回 A Start 后 suspendedMailboxIDs 应清空: %v", manager.suspendedMailboxIDs)
	}
}

// 防御路径：session 层遗漏 FinalizeSuspendedMailboxes 时，Start 成功重新
// 物化后仍清空 suspendedMailboxIDs，避免陈旧记录跨入下一个挂起周期（解冻
// team 可能已用同 agentID 重新认领 recovered 邮箱，陈旧记录会让下次
// Finalize 误注销在用邮箱；被遗漏注销的邮箱本身是协议违反，不在兜底范围）。
func TestManagerStartClearsSuspendedMailboxRecordDefensively(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerA := newControllerTask(t, taskStore, "controller-a-defensive")
	controllerB := newControllerTask(t, taskStore, "controller-b-defensive")
	storeA := NewMemoryStore()
	manager := testManagerWithStore(t, catalog, taskStore, storeA, newFakeRoutes(), 2)
	t.Cleanup(manager.Shutdown)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start(A): %v", err)
	}
	if _, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerA,
		TemplateRef:      "builtin/explorer@1", Purpose: "session A team", Replicas: 1,
	}); err != nil {
		t.Fatalf("Provision(A): %v", err)
	}
	manager.SuspendAll()
	if len(manager.suspendedMailboxIDs) != 1 {
		t.Fatalf("SuspendAll 应记录 1 个 ID: %v", manager.suspendedMailboxIDs)
	}

	// 模拟 session 层遗漏 Finalize，直接重绑换清单解冻（新清单 agentID 不同，
	// 不触发邮箱冲突）。
	tmplB, err := catalog.Resolve("builtin/generalist@1")
	if err != nil {
		t.Fatalf("resolve generalist: %v", err)
	}
	specB := testSpec("session-b-team-def", controllerB, "session B team")
	specB.TemplateRef = tmplB.Ref
	specB.TemplateDigest = tmplB.Digest
	specB.Replicas = 1
	storeB := NewMemoryStore()
	if _, _, err := storeB.Ensure(specB); err != nil {
		t.Fatalf("Ensure(B): %v", err)
	}
	manager.store = storeB
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("解冻 Start(B): %v", err)
	}
	if manager.suspendedMailboxIDs != nil {
		t.Fatalf("Start 应防御性清空 suspendedMailboxIDs: %v", manager.suspendedMailboxIDs)
	}
	if got := manager.ActiveCount(); got != 1 {
		t.Fatalf("解冻 B 后 active=%d, 应为 1", got)
	}
}
