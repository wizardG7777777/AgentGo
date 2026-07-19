package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/team"
	"agentgo/internal/trace"
)

// B2/B3 修复的端到端回归测试：真实 SessionManager + onSessionSwitched 钩子。
// B2：切换触发 plan/team store 持久化位置迁移——新 Session 目录从切换时刻起
//     持有反映内存活态的 plan-state.json / agent-teams.json，旧目录冻结。
// B3：System.NewSession/SwitchSession 在切换前把运行时快照刷新到旧 Session
//     目录，切换成功后清空 lastResult；切换失败时旧 Session 保持 active。

// switchTestEnv 装配一套带 plan/team store 与运行时状态的 System。
type switchTestEnv struct {
	sm        *session.SessionManager
	sys       *System
	planCoord *plan.Coordinator
	teamStore *team.Store
	sessDir   string // 当前（绑定时刻）Session 目录
}

// preserveTraceGlobals 保存/恢复 trace 包级默认 writer/dispatcher——
// onSessionSwitched 会 SwapDefaultWriter，测试不能污染其他用例的全局态。
func preserveTraceGlobals(t *testing.T) {
	t.Helper()
	origW, origD := trace.Default(), trace.DefaultDispatcher()
	trace.SetDefault(nil)
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() {
		if w := trace.Default(); w != nil {
			_ = w.Close() // 关闭钩子换绑后的当前 writer，避免 Windows 句柄泄漏
		}
		trace.SetDefault(origW)
		trace.SetDefaultDispatcher(origD)
	})
}

func newSwitchTestEnv(t *testing.T, sm *session.SessionManager) *switchTestEnv {
	t.Helper()
	preserveTraceGlobals(t)

	sess := sm.Current()
	if sess == nil {
		t.Fatal("无当前 Session")
	}

	eventCh := make(chan model.Event, 8)
	taskStore := store.NewMemoryTaskStore(eventCh, 10, 1, 60)
	if err := taskStore.PublishTask(&model.Task{Description: "live workload"}); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	planStore, err := plan.OpenStore(filepath.Join(sess.Dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("plan.OpenStore: %v", err)
	}
	planCoord := plan.NewCoordinator(planStore, nil)
	if _, err := planCoord.Create(t.Context(), plan.CreateInput{
		PlanID: "plan-live", RootTaskID: "root-live", Budget: model.PlanBudget{},
	}); err != nil {
		t.Fatalf("plan Create: %v", err)
	}

	teamStore, err := team.OpenStore(filepath.Join(sess.Dir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("team.OpenStore: %v", err)
	}
	if _, _, err := teamStore.Ensure(team.TeamSpec{
		ID: "team-a", TemplateRef: "builtin/explorer@1", TemplateDigest: "sha256:test",
		PlanID: "plan-live", ControllerTaskID: "ctrl-1", Purpose: "investigate",
		EventType: "team:team-a", Replicas: 1, Status: team.StatusReady,
	}); err != nil {
		t.Fatalf("team Ensure: %v", err)
	}

	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	mb.Register("agent-1", "")
	hist := scheduler.NewSessionHistory(4)

	sys := &System{
		Store:           taskStore,
		Roster:          r,
		MailboxRegistry: mb,
		Scheduler:       &scheduler.Bundle{History: hist},
		SessionMgr:      sm,
		PlanStore:       planStore,
		TeamStore:       teamStore,
		PlanCoordinator: planCoord,
	}
	sys.seedResult(&session.ResultSnapshot{Text: "old session result", SavedAt: time.Now().UTC().Format(time.RFC3339Nano)})

	sm.SetOnSwitch(sys.onSessionSwitched)
	return &switchTestEnv{sm: sm, sys: sys, planCoord: planCoord, teamStore: teamStore, sessDir: sess.Dir}
}

// snapshotAt 读取指定 Session 目录下的 snapshot.json；不存在时返回 nil。
func snapshotAt(t *testing.T, sessDir string) *session.Snapshot {
	t.Helper()
	path := filepath.Join(sessDir, "snapshot.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	snap, err := session.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot %s: %v", path, err)
	}
	return snap
}

func assertSnapshotHasTaskAndResult(t *testing.T, snap *session.Snapshot) {
	t.Helper()
	if snap == nil {
		t.Fatal("snapshot.json 不存在")
	}
	found := false
	for _, task := range snap.Tasks {
		if task.Description == "live workload" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("快照缺少切换前的任务: %#v", snap.Tasks)
	}
	if snap.Result == nil || snap.Result.Text != "old session result" {
		t.Fatalf("快照缺少切换前的结果: %#v", snap.Result)
	}
}

func assertSnapshotHasContinuousRuntimeWithoutResult(t *testing.T, snap *session.Snapshot) {
	t.Helper()
	if snap == nil {
		t.Fatal("snapshot.json 不存在")
	}
	found := false
	for _, task := range snap.Tasks {
		if task.Description == "live workload" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("新 current Session 快照缺少连续运行时任务: %#v", snap.Tasks)
	}
	if snap.Result != nil {
		t.Fatalf("任务结果不应跨 session: %#v", snap.Result)
	}
}

func TestNewSessionSerializesWithPeriodicSnapshotBoundary(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)

	// Simulate a periodic snapshot already owning the save/switch boundary.
	// NewSession must not activate a new current Session until that save exits.
	env.sys.snapshotMu.Lock()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := env.sys.NewSession()
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		env.sys.snapshotMu.Unlock()
		t.Fatalf("NewSession crossed an in-flight snapshot boundary: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	env.sys.snapshotMu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("NewSession after snapshot boundary: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NewSession did not resume after snapshot boundary released")
	}
}

// B2 bootstrap 级：真实 SessionManager.CreateNew 触发 onSessionSwitched，
// 两个 store 的持久化位置迁移到新 Session 目录且反映活态，旧目录冻结。
func TestOnSessionSwitched_RebindsPlanAndTeamStores(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	oldDir := env.sessDir

	newSess, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew: %v", err)
	}

	// 新 Session 目录从切换时刻起持有完整副本
	newPlanStore, err := plan.OpenStore(filepath.Join(newSess.Dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("reopen new plan store: %v", err)
	}
	if _, err := newPlanStore.GetPlan("plan-live"); err != nil {
		t.Fatalf("新 Session plan-state.json 缺少活态 plan: %v", err)
	}
	newTeamStore, err := team.OpenStore(filepath.Join(newSess.Dir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("reopen new team store: %v", err)
	}
	if _, err := newTeamStore.Get("team-a"); err != nil {
		t.Fatalf("新 Session agent-teams.json 缺少活态 team: %v", err)
	}

	// 切换后的变更只落新目录
	if _, err := env.planCoord.Create(t.Context(), plan.CreateInput{
		PlanID: "plan-after", RootTaskID: "root-after", Budget: model.PlanBudget{},
	}); err != nil {
		t.Fatalf("plan Create after switch: %v", err)
	}
	if _, err := env.teamStore.SetStatus("team-a", team.StatusStopped, "done"); err != nil {
		t.Fatalf("team SetStatus after switch: %v", err)
	}
	newPlanStore, err = plan.OpenStore(filepath.Join(newSess.Dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("reopen new plan store again: %v", err)
	}
	if _, err := newPlanStore.GetPlan("plan-after"); err != nil {
		t.Fatalf("新 plan-state.json 缺少切换后的变更: %v", err)
	}
	newTeamStore, err = team.OpenStore(filepath.Join(newSess.Dir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("reopen new team store again: %v", err)
	}
	got, err := newTeamStore.Get("team-a")
	if err != nil || got.Status != team.StatusStopped {
		t.Fatalf("新 agent-teams.json 缺少切换后的变更: got=%+v err=%v", got, err)
	}

	// 旧目录冻结在切换时刻
	oldPlanStore, err := plan.OpenStore(filepath.Join(oldDir, "plan-state.json"))
	if err != nil {
		t.Fatalf("reopen old plan store: %v", err)
	}
	if _, err := oldPlanStore.GetPlan("plan-after"); err == nil {
		t.Fatal("旧 plan-state.json 混入了切换后的变更——未冻结")
	}
	oldTeamStore, err := team.OpenStore(filepath.Join(oldDir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("reopen old team store: %v", err)
	}
	got, err = oldTeamStore.Get("team-a")
	if err != nil || got.Status != team.StatusReady {
		t.Fatalf("旧 agent-teams.json 未冻结在切换时刻: got=%+v err=%v", got, err)
	}
}

// B3(a)：SwitchSession 切换前把运行时快照刷新到旧 Session；切换成功后立即
// 把连续运行时写入新 current Session，并清空 lastResult。
func TestSwitchSession_SnapshotsOldSessionAndClearsResult(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	// 预建目标 Session B（此时尚未注册钩子，纯目录准备），再切回 A
	oldID := sm.Current().ID
	target, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew target: %v", err)
	}
	if err := sm.SwitchTo(oldID); err != nil {
		t.Fatalf("SwitchTo back: %v", err)
	}

	env := newSwitchTestEnv(t, sm)
	oldDir := env.sessDir

	if changed, err := env.sys.SwitchSession(target.ID); err != nil || !changed {
		t.Fatalf("SwitchSession: %v", err)
	}

	assertSnapshotHasTaskAndResult(t, snapshotAt(t, oldDir))
	assertSnapshotHasContinuousRuntimeWithoutResult(t, snapshotAt(t, target.Dir))
	if env.sys.resultSnapshot() != nil {
		t.Fatal("切换成功后 lastResult 未清空——结果跨 session")
	}
	if sm.Current().ID != target.ID {
		t.Fatalf("current = %s, want %s", sm.Current().ID, target.ID)
	}

	// B2 链路随 System.SwitchSession 同样生效：目标目录持有活态 store 副本
	newPlanStore, err := plan.OpenStore(filepath.Join(target.Dir, "plan-state.json"))
	if err != nil {
		t.Fatalf("reopen new plan store: %v", err)
	}
	if _, err := newPlanStore.GetPlan("plan-live"); err != nil {
		t.Fatalf("目标 Session plan-state.json 缺少活态 plan: %v", err)
	}
}

func TestSwitchSession_CurrentSessionIsNoOpAndKeepsResult(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	env.sys.saveRuntimeSnapshot()

	current := sm.Current()
	if current == nil {
		t.Fatal("无当前 Session")
	}
	before := snapshotAt(t, current.Dir)
	assertSnapshotHasTaskAndResult(t, before)

	changed, err := env.sys.SwitchSession("  " + current.ID + "  ")
	if err != nil || changed {
		t.Fatalf("same-session SwitchSession = changed=%v err=%v", changed, err)
	}
	if sm.Current().ID != current.ID {
		t.Fatalf("same-session no-op changed current: %s", sm.Current().ID)
	}
	if result := env.sys.resultSnapshot(); result == nil || result.Text != "old session result" {
		t.Fatalf("same-session no-op cleared in-memory result: %#v", result)
	}
	after := snapshotAt(t, current.Dir)
	assertSnapshotHasTaskAndResult(t, after)
}

// B3(b)：NewSession 同语义——旧 Session 快照刷新，新 Session 立即持久化连续
// 运行时，结果清空。
func TestNewSession_SnapshotsOldSessionAndClearsResult(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	oldDir := env.sessDir

	newID, err := env.sys.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if newID == "" || sm.Current().ID != newID {
		t.Fatalf("NewSession 返回 %q 但 current = %q", newID, sm.Current().ID)
	}

	assertSnapshotHasTaskAndResult(t, snapshotAt(t, oldDir))
	newDir := sm.Current().Dir
	assertSnapshotHasContinuousRuntimeWithoutResult(t, snapshotAt(t, newDir))
	if env.sys.resultSnapshot() != nil {
		t.Fatal("切换成功后 lastResult 未清空——结果跨 session")
	}
}

func TestNewSession_PreSnapshotFailureKeepsOldSessionActive(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	oldID := sm.Current().ID

	// SaveSnapshot writes snapshot.json.tmp first. A directory at that exact
	// path is a deterministic cross-platform write failure.
	if err := os.Mkdir(filepath.Join(sm.Current().Dir, "snapshot.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if newID, err := env.sys.NewSession(); err == nil || newID != "" || !strings.Contains(err.Error(), "切换前") {
		t.Fatalf("NewSession pre-save failure: id=%q err=%v", newID, err)
	}
	if sm.Current().ID != oldID {
		t.Fatalf("pre-save failure changed current Session: got=%s want=%s", sm.Current().ID, oldID)
	}
	if env.sys.resultSnapshot() == nil {
		t.Fatal("pre-save failure cleared the old Session result")
	}
}

func TestSwitchSession_PostSnapshotFailureRollsBack(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	oldID := sm.Current().ID
	target, err := sm.CreateNew()
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SwitchTo(oldID); err != nil {
		t.Fatal(err)
	}
	env := newSwitchTestEnv(t, sm)
	if err := os.Mkdir(filepath.Join(target.Dir, "snapshot.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = env.sys.SwitchSession(target.ID)
	if err == nil || !strings.Contains(err.Error(), "新 Session 快照") || !strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("post-save failure should be reported as rolled back: %v", err)
	}
	if sm.Current().ID != oldID {
		t.Fatalf("post-save failure current=%s, want rollback to %s", sm.Current().ID, oldID)
	}
	if result := env.sys.resultSnapshot(); result == nil || result.Text != "old session result" {
		t.Fatalf("rollback did not restore old result: %#v", result)
	}
	assertSnapshotHasTaskAndResult(t, snapshotAt(t, env.sessDir))
}

func TestSwitchSession_CriticalRebindFailureRollsBack(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	oldID := sm.Current().ID
	target, err := sm.CreateNew()
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SwitchTo(oldID); err != nil {
		t.Fatal(err)
	}
	env := newSwitchTestEnv(t, sm)
	if err := os.Mkdir(filepath.Join(target.Dir, "plan-state.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = env.sys.SwitchSession(target.ID)
	if err == nil || !strings.Contains(err.Error(), "重绑失败") || !strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("critical rebind failure should be reported as rolled back: %v", err)
	}
	if sm.Current().ID != oldID {
		t.Fatalf("rebind failure current=%s, want rollback to %s", sm.Current().ID, oldID)
	}
	if result := env.sys.resultSnapshot(); result == nil || result.Text != "old session result" {
		t.Fatalf("rebind rollback changed old result: %#v", result)
	}
	assertSnapshotHasTaskAndResult(t, snapshotAt(t, env.sessDir))
	if _, err := env.planCoord.Create(t.Context(), plan.CreateInput{
		PlanID: "plan-after-rebind-rollback", RootTaskID: "root-after-rebind-rollback",
	}); err != nil {
		t.Fatal(err)
	}
	reopenedOld, err := plan.OpenStore(filepath.Join(env.sessDir, "plan-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopenedOld.GetPlan("plan-after-rebind-rollback"); err != nil {
		t.Fatalf("PlanStore did not return to old Session after rollback: %v", err)
	}
}

// B3(c)：切换到不存在的目标失败——旧 Session 保持 active，错误透出；
// 切换前写入旧 Session 的快照无害保留；lastResult 不重置（旧 Session 仍活跃）。
func TestSwitchSession_FailureKeepsOldSessionActive(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	oldDir := env.sessDir
	oldID := sm.Current().ID

	_, err = env.sys.SwitchSession("nonexistent-uuid")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("SwitchSession 应失败并透出错误: %v", err)
	}
	if sm.Current().ID != oldID {
		t.Fatalf("失败后 current = %s, want 仍为 %s", sm.Current().ID, oldID)
	}
	// 失败前的快照写入无害：内容本来就是旧 Session 的真实状态
	assertSnapshotHasTaskAndResult(t, snapshotAt(t, oldDir))
	if env.sys.resultSnapshot() == nil {
		t.Fatal("切换失败时 lastResult 不应重置（旧 Session 仍活跃）")
	}
}

func TestResultRollbackCASDoesNotOverwriteConcurrentResult(t *testing.T) {
	sys := &System{}
	sys.seedResult(&session.ResultSnapshot{Text: "old result"})
	oldResult, oldVersion := sys.resultSnapshotWithVersion()

	clearedVersion, ok := sys.resetResultIfVersion(oldVersion)
	if !ok {
		t.Fatal("result CAS reset unexpectedly failed")
	}
	// 模拟切换失败后的回滚窗口内产生了更新结果。旧实现会无条件 seed
	// oldResult，从而静默覆盖这里的新结果。
	sys.seedResult(&session.ResultSnapshot{Text: "new concurrent result"})
	if sys.restoreResultIfVersion(clearedVersion, oldResult) {
		t.Fatal("rollback restored stale result over a newer generation")
	}
	if got := sys.resultSnapshot(); got == nil || got.Text != "new concurrent result" {
		t.Fatalf("newer result was overwritten: %#v", got)
	}
}

func TestRecordResultWaitsForSnapshotSwitchBoundary(t *testing.T) {
	sys := &System{}
	sys.seedResult(&session.ResultSnapshot{Text: "before switch"})
	sys.snapshotMu.Lock()
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		sys.recordResult("after switch")
		close(done)
	}()
	<-started

	select {
	case <-done:
		sys.snapshotMu.Unlock()
		t.Fatal("recordResult crossed the snapshot/session-switch boundary")
	case <-time.After(25 * time.Millisecond):
	}
	if got := sys.resultSnapshot(); got == nil || got.Text != "before switch" {
		sys.snapshotMu.Unlock()
		t.Fatalf("result changed while switch boundary was held: %#v", got)
	}
	sys.snapshotMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recordResult did not resume after switch boundary released")
	}
	if got := sys.resultSnapshot(); got == nil || got.Text != "after switch" {
		t.Fatalf("result after boundary release = %#v", got)
	}
}
