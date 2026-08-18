package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/runner"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/team"
)

type shutdownIdleLLM struct{}

func (shutdownIdleLLM) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	return llm.Response{}, errors.New("shutdown persistence test LLM must not be called")
}

type shutdownMailboxTestEnv struct {
	sys        *System
	mailboxes  *mailbox.Registry
	sessionDir string
	agentID    string
	eventType  string
	catalog    *agenttemplate.Catalog
	durable    team.TeamStore
}

func newShutdownMailboxTestEnv(t *testing.T) *shutdownMailboxTestEnv {
	t.Helper()
	preserveTraceGlobals(t)
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model",
		ValidateTools: func([]string) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("load template catalog: %v", err)
	}

	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	mailboxes := mailbox.NewRegistry(8)
	r := roster.NewMemoryRoster()
	durable := team.NewMemoryStore()
	manager := team.NewManager(
		runner.RunnerDeps{
			Store: taskStore, Roster: r, MBRegistry: mailboxes,
			ProjectRoot: t.TempDir(),
		},
		func(string) llm.Client { return shutdownIdleLLM{} },
		catalog, durable, scheduler.NewAgentRegistry(), 1,
	)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("TeamManager Start: %v", err)
	}
	provisioned, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: "controller-shutdown-mailbox",
		TemplateRef:      "builtin/explorer@1", Purpose: "persist shutdown unread mail", Replicas: 1,
	})
	if err != nil {
		manager.Shutdown()
		t.Fatalf("Provision: %v", err)
	}
	agentID := provisioned.AgentIDs[0]
	for _, summary := range []string{"first", "second"} {
		if err := mailboxes.Send(mailbox.Message{
			From: "scheduler", To: agentID, Content: summary, Summary: summary,
			SentAt: time.Now().UTC(),
		}); err != nil {
			manager.Shutdown()
			t.Fatalf("Send(%s): %v", summary, err)
		}
	}

	sm, err := session.NewSessionManager(filepath.Join(t.TempDir(), "sessions"), session.SessionConfig{Enabled: true})
	if err != nil {
		manager.Shutdown()
		t.Fatalf("NewSessionManager: %v", err)
	}
	// 标记为非空会话（2026-08 二期：空会话在 Shutdown 时被丢弃）——生产上
	// team 邮箱流量必以用户提交过任务为前提，此处等效模拟。
	sm.RecordFirstInput("persist shutdown unread mail")
	sm.IncrementTaskCount()
	sessionDir := sm.Current().Dir
	sys := &System{
		Store: taskStore, Roster: r, MailboxRegistry: mailboxes,
		SessionMgr: sm, TeamManager: manager,
	}
	t.Cleanup(func() { _ = sys.Shutdown() })
	return &shutdownMailboxTestEnv{
		sys: sys, mailboxes: mailboxes, sessionDir: sessionDir,
		agentID: agentID, eventType: provisioned.EventType,
		catalog: catalog, durable: durable,
	}
}

func TestSystemShutdownSnapshotsDynamicTeamUnreadMailboxBeforeCleanup(t *testing.T) {
	env := newShutdownMailboxTestEnv(t)
	if err := env.sys.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	snap, err := session.LoadSnapshot(filepath.Join(env.sessionDir, "snapshot.json"))
	if err != nil {
		t.Fatalf("LoadSnapshot after Shutdown: %v", err)
	}
	restarted := mailbox.NewRegistry(8)
	if err := restarted.ImportSnapshot(snap.Mailboxes); err != nil {
		t.Fatalf("restart ImportSnapshot: %v", err)
	}

	// 真正用同一 durable TeamStore 重建 TeamManager，验证 System.Shutdown
	// 产出的 snapshot 能贯通 ImportSnapshot → Start → stable-ID claim。
	// Team 恢复要求 controller 任务存活（manager.Start 对 controller 缺失的
	// Team 标记 stopped 并跳过）：模拟真实重启时 controller 任务随快照恢复。
	restoredTasks := store.NewMemoryTaskStore(nil, 100, 1, 30)
	if err := restoredTasks.PublishTask(&model.Task{
		ID: "controller-shutdown-mailbox", Description: "recovered controller", EventType: "__scheduler__",
	}); err != nil {
		t.Fatalf("恢复 controller 任务: %v", err)
	}
	restartedManager := team.NewManager(
		runner.RunnerDeps{
			Store: restoredTasks, Roster: roster.NewMemoryRoster(),
			MBRegistry: restarted, ProjectRoot: t.TempDir(),
		},
		func(string) llm.Client { return shutdownIdleLLM{} },
		env.catalog, env.durable, scheduler.NewAgentRegistry(), 1,
	)
	if err := restartedManager.Start(context.Background()); err != nil {
		t.Fatalf("restarted TeamManager Start: %v", err)
	}
	if claimed, err := restarted.ClaimRecovered(env.agentID, env.eventType); claimed != nil || !errors.Is(err, mailbox.ErrRecoveredMailboxConflict) {
		t.Fatalf("active restarted Team mailbox was not claimed: (%v, %v)", claimed, err)
	}
	restartedSnapshot := restarted.ExportSnapshot()
	restartedManager.Shutdown()

	// 再导入一次只为从公开 API 读取 FIFO；此时数据已经走过真实 Start。
	verify := mailbox.NewRegistry(8)
	if err := verify.ImportSnapshot(restartedSnapshot); err != nil {
		t.Fatalf("verify ImportSnapshot: %v", err)
	}
	recovered, err := verify.ClaimRecovered(env.agentID, env.eventType)
	if err != nil || recovered == nil {
		t.Fatalf("verify ClaimRecovered = (%v, %v); mailboxes=%+v", recovered, err, restartedSnapshot)
	}
	msgs := recovered.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "first" || msgs[1].Summary != "second" {
		t.Fatalf("System Shutdown/restart FIFO mismatch: %+v", msgs)
	}
	for _, status := range env.mailboxes.ScanAll() {
		if status.AgentID == env.agentID {
			t.Fatal("System Shutdown did not finalize the preserved runtime mailbox")
		}
	}
}

func TestSystemShutdownSnapshotFailureKeepsDynamicTeamMailbox(t *testing.T) {
	env := newShutdownMailboxTestEnv(t)
	// SaveSnapshot 原子替换前固定写 snapshot.json.tmp；同名目录提供稳定、
	// 跨平台的故障注入，三次有限重试都会失败。
	if err := os.Mkdir(filepath.Join(env.sessionDir, "snapshot.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := env.sys.Shutdown()
	if err == nil || !strings.Contains(err.Error(), "最终 Session 快照") {
		t.Fatalf("Shutdown snapshot failure = %v", err)
	}
	found := false
	for _, status := range env.mailboxes.ScanNonEmpty() {
		if status.AgentID == env.agentID && status.Count == 2 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("failed final snapshot finalized/lost Team mailbox: %+v", env.mailboxes.ScanNonEmpty())
	}
}

func TestSystemShutdownConcurrentCallsRunOnce(t *testing.T) {
	preserveTraceGlobals(t)
	var releases atomic.Int32
	sys := &System{releaseInstanceLock: func() { releases.Add(1) }}
	const callers = 16
	start := make(chan struct{})
	errCh := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			errCh <- sys.Shutdown()
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Shutdown: %v", err)
		}
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("full Shutdown executed %d times, want 1", got)
	}
}
