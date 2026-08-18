package bootstrap

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/team"
	"agentgo/internal/trace"
	"agentgo/internal/watchdog"
)

// Session 隔离（2026-08）的端到端回归测试：真实 SessionManager + onSessionSwitched 钩子。
// B2：切换触发 team store 持久化位置迁移——新 Session 目录从切换时刻起
//     持有反映内存活态的 agent-teams.json，旧目录冻结。
// 隔离语义：System.NewSession/SwitchSession 执行「冻结 → 切换 → 解冻」——
//     当前 session 运行时停驻并归档快照，再从目标 session 的快照整体重建
//     公告板/邮箱/历史/结果；公告板内容不再跨 session 连续。

// switchTestEnv 装配一套带 team store 与运行时状态的 System。
type switchTestEnv struct {
	sm        *session.SessionManager
	sys       *System
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

	teamStore, err := team.OpenStore(filepath.Join(sess.Dir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("team.OpenStore: %v", err)
	}
	if _, _, err := teamStore.Ensure(team.TeamSpec{
		ID: "team-a", TemplateRef: "builtin/explorer@1", TemplateDigest: "sha256:test",
		ControllerTaskID: "ctrl-1", Purpose: "investigate",
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
		TeamStore:       teamStore,
	}
	sys.seedResult(&session.ResultSnapshot{Text: "old session result", SavedAt: time.Now().UTC().Format(time.RFC3339Nano)})

	sm.SetOnSwitch(sys.onSessionSwitched)
	return &switchTestEnv{sm: sm, sys: sys, teamStore: teamStore, sessDir: sess.Dir}
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

// assertSnapshotHasNoTasks 断言快照存在且公告板为空（新 session 解冻后的
// 初始快照、或目标 session 从未有过任务时的解冻结果）。
func assertSnapshotHasNoTasks(t *testing.T, snap *session.Snapshot) {
	t.Helper()
	if snap == nil {
		t.Fatal("snapshot.json 不存在")
	}
	if len(snap.Tasks) != 0 {
		t.Fatalf("快照公告板应为空: %#v", snap.Tasks)
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
// team store 的持久化位置迁移到新 Session 目录且反映活态，旧目录冻结。
func TestOnSessionSwitched_RebindsTeamStore(t *testing.T) {
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
	newTeamStore, err := team.OpenStore(filepath.Join(newSess.Dir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("reopen new team store: %v", err)
	}
	if _, err := newTeamStore.Get("team-a"); err != nil {
		t.Fatalf("新 Session agent-teams.json 缺少活态 team: %v", err)
	}

	// 切换后的变更只落新目录
	if _, err := env.teamStore.SetStatus("team-a", team.StatusStopped, "done"); err != nil {
		t.Fatalf("team SetStatus after switch: %v", err)
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
	oldTeamStore, err := team.OpenStore(filepath.Join(oldDir, "agent-teams.json"))
	if err != nil {
		t.Fatalf("reopen old team store: %v", err)
	}
	got, err = oldTeamStore.Get("team-a")
	if err != nil || got.Status != team.StatusReady {
		t.Fatalf("旧 agent-teams.json 未冻结在切换时刻: got=%+v err=%v", got, err)
	}
}

// 隔离语义核心用例：A→B→A→B 往返切换——公告板与结果按 session 各自独立，
// 旧 session 冻结归档、目标 session 从其快照解冻重建。
func TestSwitchSession_RoundTripIsolatesBoard(t *testing.T) {
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

	env := newSwitchTestEnv(t, sm) // A：任务 "live workload"(pending) + 结果 "old session result"
	oldDir := env.sessDir

	// ① A→B：A 冻结归档（任务+结果进快照），B 解冻为空板。
	if changed, err := env.sys.SwitchSession(target.ID); err != nil || !changed {
		t.Fatalf("SwitchSession A→B: %v", err)
	}
	assertSnapshotHasTaskAndResult(t, snapshotAt(t, oldDir))
	if tasks, err := env.sys.Store.ScanAll(); err != nil || len(tasks) != 0 {
		t.Fatalf("切到 B 后公告板应为空: tasks=%v err=%v", tasks, err)
	}
	if env.sys.resultSnapshot() != nil {
		t.Fatal("任务结果不应跨 session（B 无冻结结果，应为空）")
	}
	assertSnapshotHasNoTasks(t, snapshotAt(t, target.Dir))

	// ② 在 B 发布 task-B，B→A：B 冻结归档 task-B，A 解冻重建出 live workload
	//    的历史事实（2026-08 二期：非终态任务阻断为 blocked，不自动续跑）
	//    与冻结时归档的结果。
	if err := env.sys.Store.PublishTask(&model.Task{Description: "task-B"}); err != nil {
		t.Fatalf("PublishTask task-B: %v", err)
	}
	if changed, err := env.sys.SwitchSession(oldID); err != nil || !changed {
		t.Fatalf("SwitchSession B→A: %v", err)
	}
	tasks, err := env.sys.Store.ScanAll()
	if err != nil || len(tasks) != 1 || tasks[0].Description != "live workload" {
		t.Fatalf("切回 A 后公告板应只有 live workload: tasks=%v err=%v", tasks, err)
	}
	if tasks[0].Status != model.TaskStatusBlocked {
		t.Fatalf("A 的非终态任务解冻时应阻断为 blocked（不自动续跑）: %s", tasks[0].Status)
	}
	if result := env.sys.resultSnapshot(); result == nil || result.Text != "old session result" {
		t.Fatalf("A 冻结时归档的结果应随解冻播种回来: %#v", result)
	}
	bSnap := snapshotAt(t, target.Dir)
	if bSnap == nil || len(bSnap.Tasks) != 1 || bSnap.Tasks[0].Description != "task-B" {
		t.Fatalf("B 的归档快照应含 task-B: %#v", bSnap)
	}
	if bSnap.Result != nil {
		t.Fatalf("B 从未产生结果，归档不应有 Result: %#v", bSnap.Result)
	}

	// ③ A→B 再次切换：公告板重建为 B 的 task-B，A 的内容不可见。
	if changed, err := env.sys.SwitchSession(target.ID); err != nil || !changed {
		t.Fatalf("SwitchSession A→B（再次）: %v", err)
	}
	tasks, err = env.sys.Store.ScanAll()
	if err != nil || len(tasks) != 1 || tasks[0].Description != "task-B" {
		t.Fatalf("再次切到 B 后公告板应只有 task-B: tasks=%v err=%v", tasks, err)
	}
	if env.sys.resultSnapshot() != nil {
		t.Fatal("B 的结果视图应为空（A 的结果不泄漏）")
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

// NewSession 隔离语义：旧 session 冻结归档（任务+结果进快照），新 session
// 从空公告板解冻起步，结果不跨 session。
func TestNewSession_FreezesOldAndStartsEmpty(t *testing.T) {
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
	assertSnapshotHasNoTasks(t, snapshotAt(t, sm.Current().Dir))
	if tasks, err := env.sys.Store.ScanAll(); err != nil || len(tasks) != 0 {
		t.Fatalf("新 session 公告板应为空: tasks=%v err=%v", tasks, err)
	}
	if env.sys.resultSnapshot() != nil {
		t.Fatal("切换成功后 lastResult 未清空——结果跨 session")
	}
}

// 归档快照保存失败（冻结失败）时：切换中止，旧 session 经 thawInPlaceLocked
// 原地恢复——公告板任务与结果原样保留，current 不变。
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
	if newID, err := env.sys.NewSession(); err == nil || newID != "" || !strings.Contains(err.Error(), "归档快照") {
		t.Fatalf("NewSession pre-save failure: id=%q err=%v", newID, err)
	}
	if sm.Current().ID != oldID {
		t.Fatalf("pre-save failure changed current Session: got=%s want=%s", sm.Current().ID, oldID)
	}
	if env.sys.resultSnapshot() == nil {
		t.Fatal("pre-save failure cleared the old Session result")
	}
	// 原地恢复：公告板任务仍在（静默期公告板未被变更，解冻重建原样返回）。
	tasks, err := env.sys.Store.ScanAll()
	if err != nil || len(tasks) != 1 || tasks[0].Description != "live workload" {
		t.Fatalf("原地恢复后公告板应保留 live workload: tasks=%v err=%v", tasks, err)
	}
}

// 解冻末段的快照保存失败不再回滚：session 已切换且解冻后的内存状态正确，
// 持久化失败只降级记 WARNING（由周期快照兜底），切换本身成功返回。
func TestSwitchSession_ThawSnapshotFailureKeepsSwitchedSession(t *testing.T) {
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
	// 让解冻末段写不进目标目录，但冻结段写旧目录不受影响。
	if err := os.Mkdir(filepath.Join(target.Dir, "snapshot.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := env.sys.SwitchSession(target.ID)
	if err != nil || !changed {
		t.Fatalf("解冻快照失败不应阻断切换: changed=%v err=%v", changed, err)
	}
	if sm.Current().ID != target.ID {
		t.Fatalf("current=%s, want %s", sm.Current().ID, target.ID)
	}
	// 解冻后的内存公告板正确（B 是空板），旧 session 归档完整。
	if tasks, err := env.sys.Store.ScanAll(); err != nil || len(tasks) != 0 {
		t.Fatalf("解冻后公告板应为空: tasks=%v err=%v", tasks, err)
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
	if err := os.Mkdir(filepath.Join(target.Dir, "agent-teams.json"), 0o755); err != nil {
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
	if _, err := env.teamStore.SetStatus("team-a", team.StatusStopped, "after-rollback"); err != nil {
		t.Fatal(err)
	}
	reopenedOld, err := team.OpenStore(filepath.Join(env.sessDir, "agent-teams.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopenedOld.Get("team-a")
	if err != nil || got.Status != team.StatusStopped {
		t.Fatalf("TeamStore did not return to old Session after rollback: got=%+v err=%v", got, err)
	}
}

// 切换到不存在的目标失败——旧 Session 保持 active，错误透出；冻结段写入的
// 归档快照无害保留（内容本来就是旧 Session 的真实状态），且解冻重建后
// 公告板与结果完整恢复。
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
	// 冻结段归档无害保留；失败路径经 thaw(oldID) 从归档完整解冻旧 session。
	assertSnapshotHasTaskAndResult(t, snapshotAt(t, oldDir))
	if env.sys.resultSnapshot() == nil {
		t.Fatal("切换失败解冻后 lastResult 应恢复（旧 Session 仍活跃）")
	}
	tasks, err := env.sys.Store.ScanAll()
	if err != nil || len(tasks) != 1 || tasks[0].Description != "live workload" {
		t.Fatalf("失败解冻后公告板应恢复 live workload: tasks=%v err=%v", tasks, err)
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

// /new force 端到端：旧 Session 以全终态快照归档（Result 保留），
// Roster/Mailbox/公告板/Scheduler 历史全部清空，新 Session 从空板开始。
func TestNewSessionForce_TerminatesAndStartsFresh(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	oldDir := env.sessDir

	// 布置跨子系统运行时状态：文件占用 + 未读邮件。
	if ok, err := env.sys.Roster.TryClaim("agent-1", "foo.go"); err != nil || !ok {
		t.Fatalf("TryClaim: ok=%v err=%v", ok, err)
	}
	if err := env.sys.MailboxRegistry.Send(mailbox.Message{From: "user", To: "agent-1", Content: "未读"}); err != nil {
		t.Fatalf("mailbox Send: %v", err)
	}

	newID, err := env.sys.NewSessionForce()
	if err != nil {
		t.Fatalf("NewSessionForce: %v", err)
	}
	if newID == "" || sm.Current().ID != newID {
		t.Fatalf("NewSessionForce 返回 %q 但 current = %q", newID, sm.Current().ID)
	}

	// 旧 Session 归档快照：任务全部终态（live workload 被批量取消）、
	// Result 保留、Roster claims 清空（死租约不归档）；未读邮件按隔离
	// 语义随旧 session 一并归档保留（解冻回来时仍可取回），但活跃
	// 邮箱在新 session 已清空。
	oldSnap := snapshotAt(t, oldDir)
	if oldSnap == nil {
		t.Fatal("旧 Session snapshot.json 不存在")
	}
	found := false
	for _, task := range oldSnap.Tasks {
		if !model.IsTerminal(model.TaskStatus(task.Status)) {
			t.Fatalf("归档快照含非终态任务: %s (%s)", task.Description, task.Status)
		}
		if task.Description == "live workload" {
			found = true
			if task.Status != string(model.TaskStatusCancelled) {
				t.Fatalf("live workload 应被批量取消, got %s", task.Status)
			}
		}
	}
	if !found {
		t.Fatalf("归档快照缺少原任务: %#v", oldSnap.Tasks)
	}
	if oldSnap.Result == nil || oldSnap.Result.Text != "old session result" {
		t.Fatalf("归档快照未保留任务结果: %#v", oldSnap.Result)
	}
	if len(oldSnap.Roster.Claims) != 0 {
		t.Fatalf("归档快照仍含文件占用: %#v", oldSnap.Roster.Claims)
	}
	mailArchived := false
	for _, mb := range oldSnap.Mailboxes {
		if mb.OwnerID == "agent-1" && len(mb.Messages) == 1 && mb.Messages[0].Content == "未读" {
			mailArchived = true
		}
	}
	if !mailArchived {
		t.Fatalf("归档快照应保留旧 session 的未读邮件: %#v", oldSnap.Mailboxes)
	}

	// 新 Session 运行时：公告板、Roster、Mailbox、结果全部为空。
	if tasks, err := env.sys.Store.ScanAll(); err != nil || len(tasks) != 0 {
		t.Fatalf("新 Session 公告板应为空: tasks=%v err=%v", tasks, err)
	}
	if claims := env.sys.Roster.ListClaims(); len(claims) != 0 {
		t.Fatalf("新 Session Roster 应为空: %#v", claims)
	}
	for _, mb := range env.sys.MailboxRegistry.ScanAll() {
		if mb.Count != 0 {
			t.Fatalf("新 Session Mailbox 应为空: %#v", mb)
		}
	}
	if env.sys.resultSnapshot() != nil {
		t.Fatal("任务结果不应跨 session")
	}
	newSnap := snapshotAt(t, sm.Current().Dir)
	if newSnap == nil {
		t.Fatal("新 Session snapshot.json 不存在")
	}
	if len(newSnap.Tasks) != 0 || newSnap.Result != nil {
		t.Fatalf("新 Session 快照应为空板: tasks=%#v result=%#v", newSnap.Tasks, newSnap.Result)
	}
}

// ===== 冻结 session workspace 豁免接线（session 隔离，2026-08）=====
// 冻结归档成功后，该 session 快照里非终态任务的 taskID 登记进 Watchdog
// workspace 豁免集（其 workspace 归冻结 session 所有，解冻重排后以同一
// taskID 复用）；解冻/原地恢复后移出。

// attachTestWatchdog 给 switchTestEnv 挂一个最小 Watchdog——测试只经
// session 切换路径调用其豁免方法（不跑巡检），独立 eventCh 即可。
func attachTestWatchdog(env *switchTestEnv) *watchdog.Watchdog {
	wd := watchdog.New(env.sys.Store, config.DefaultConfig(), make(chan model.Event, 8), env.sys.Roster, nil)
	env.sys.Watchdog = wd
	return wd
}

// boardTaskIDs 返回当前公告板全部任务 ID（按发布顺序外的稳定序不做要求）。
func boardTaskIDs(t *testing.T, env *switchTestEnv) []string {
	t.Helper()
	tasks, err := env.sys.Store.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

// 冻结登记豁免：NewSession 冻结旧 session 后，其非终态任务 ID 进入
// Watchdog workspace 豁免集——任务已不在活跃公告板，workspace 不得被
// cleanupWorkspaceOrphans 按孤儿误清。新 session 是空板，解冻不清豁免。
func TestNewSession_ExemptsFrozenTaskWorkspaces(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	wd := attachTestWatchdog(env)
	frozenID := boardTaskIDs(t, env)[0]

	if _, err := env.sys.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !wd.IsWorkspaceExempt(frozenID) {
		t.Fatalf("冻结后任务 %s 应在 workspace 豁免集中", frozenID)
	}
}

// 解冻移出豁免：A→B 后 A 的任务在豁免集；B→A 解冻后 A 的任务移出
// （已重排回活跃公告板），B 新冻结的任务进入豁免集。
func TestSwitchSession_ThawClearsWorkspaceExemptions(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	oldID := sm.Current().ID
	target, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew target: %v", err)
	}
	if err := sm.SwitchTo(oldID); err != nil {
		t.Fatalf("SwitchTo back: %v", err)
	}
	env := newSwitchTestEnv(t, sm)
	wd := attachTestWatchdog(env)
	aTaskID := boardTaskIDs(t, env)[0]

	// A→B：A 冻结，aTaskID 入豁免。
	if changed, err := env.sys.SwitchSession(target.ID); err != nil || !changed {
		t.Fatalf("SwitchSession A→B: %v", err)
	}
	if !wd.IsWorkspaceExempt(aTaskID) {
		t.Fatalf("A 冻结后 %s 应在豁免集中", aTaskID)
	}

	// B 发布 task-B，B→A：A 解冻移出豁免，B 冻结入豁免。
	if err := env.sys.Store.PublishTask(&model.Task{Description: "task-B"}); err != nil {
		t.Fatalf("PublishTask task-B: %v", err)
	}
	bTaskID := boardTaskIDs(t, env)[0]
	if changed, err := env.sys.SwitchSession(oldID); err != nil || !changed {
		t.Fatalf("SwitchSession B→A: %v", err)
	}
	if wd.IsWorkspaceExempt(aTaskID) {
		t.Fatalf("解冻回 A 后 %s 应已移出豁免集", aTaskID)
	}
	if !wd.IsWorkspaceExempt(bTaskID) {
		t.Fatalf("B 冻结后 %s 应在豁免集中", bTaskID)
	}
}

// /new force 空豁免：任务全部批量取消、以全终态归档，豁免登记按非终态
// 过滤后天然为空（代码无特判）。
func TestNewSessionForce_NoWorkspaceExemptions(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	wd := attachTestWatchdog(env)
	frozenID := boardTaskIDs(t, env)[0]

	if _, err := env.sys.NewSessionForce(); err != nil {
		t.Fatalf("NewSessionForce: %v", err)
	}
	if wd.IsWorkspaceExempt(frozenID) {
		t.Fatalf("force 路径任务已全终态，%s 不应进入豁免集", frozenID)
	}
}

// 冻结中止原地恢复（归档快照保存失败）：公告板未变更，任务仍在活跃板上，
// 防御性移出豁免集（即便有前次冻结的残留登记）。
func TestNewSession_PreSnapshotFailureClearsWorkspaceExemptions(t *testing.T) {
	sessRoot := filepath.Join(t.TempDir(), "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	env := newSwitchTestEnv(t, sm)
	wd := attachTestWatchdog(env)
	taskID := boardTaskIDs(t, env)[0]
	wd.ExemptWorkspaces([]string{taskID}) // 模拟前次冻结的残留登记

	// SaveSnapshot 先写 snapshot.json.tmp——同名目录是跨平台确定的写失败。
	if err := os.Mkdir(filepath.Join(sm.Current().Dir, "snapshot.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if newID, err := env.sys.NewSession(); err == nil || newID != "" {
		t.Fatalf("NewSession pre-save failure: id=%q err=%v", newID, err)
	}
	if wd.IsWorkspaceExempt(taskID) {
		t.Fatalf("冻结中止原地恢复后 %s 应已移出豁免集", taskID)
	}
}

// 启动期豁免重建：枚举 sess-*/snapshot.json，只收集【非当前活跃 session】
// 快照里的非终态任务；逐文件损坏只告警不阻断。
func TestRebuildFrozenWorkspaceExemptions(t *testing.T) {
	root := t.TempDir()
	writeSnap := func(sessID string, tasks ...session.TaskSnapshot) {
		t.Helper()
		dir := filepath.Join(root, "sess-"+sessID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		snap := &session.Snapshot{Version: 4, Tasks: tasks}
		if err := session.SaveSnapshot(filepath.Join(dir, "snapshot.json"), snap); err != nil {
			t.Fatal(err)
		}
	}
	writeSnap("frozen-a",
		session.TaskSnapshot{ID: "a-pending", Status: string(model.TaskStatusPending)},
		session.TaskSnapshot{ID: "a-processing", Status: string(model.TaskStatusProcessing)},
		session.TaskSnapshot{ID: "a-done", Status: string(model.TaskStatusCompleted)},
	)
	writeSnap("frozen-b", session.TaskSnapshot{ID: "b-failed", Status: string(model.TaskStatusFailed)})
	writeSnap("current", session.TaskSnapshot{ID: "cur-pending", Status: string(model.TaskStatusPending)})
	// 损坏快照：只告警不阻断，不影响其余 session 的重建。
	badDir := filepath.Join(root, "sess-corrupt")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "snapshot.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	ids := rebuildFrozenWorkspaceExemptions(root, "current")
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a-pending" || ids[1] != "a-processing" {
		t.Fatalf("豁免重建应只含冻结 session 的非终态任务: %v", ids)
	}

	// 无活跃 session（currentID=""）：公告板为空，全部 session 按冻结处理。
	ids = rebuildFrozenWorkspaceExemptions(root, "")
	sort.Strings(ids)
	if len(ids) != 3 || ids[0] != "a-pending" || ids[1] != "a-processing" || ids[2] != "cur-pending" {
		t.Fatalf("currentID 为空时全部非终态任务都应豁免: %v", ids)
	}
}
