package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// B5/B7 修复的回归测试：SessionManager OnSwitch 钩子语义——
// 每次成功的 CreateNew/SwitchTo 恰好触发一次（锁外、可回调 manager 方法）；
// 失败不触发；启动期 init 不触发；钩子 panic 被 recover 不击穿 manager。

// hookRecorder 记录钩子收到的 Session，并在回调内调用 manager 方法——
// 若钩子在 sm.mu 内触发，这些回调会立即死锁（外层超时兜底）。
type hookRecorder struct {
	mu  sync.Mutex
	got []*Session
	sm  *SessionManager
}

func (h *hookRecorder) hook(s *Session) {
	// 锁外调用证明：回调里可安全调用 manager 方法
	_ = h.sm.Current()
	_ = h.sm.LogDir()
	h.mu.Lock()
	h.got = append(h.got, s)
	h.mu.Unlock()
}

func (h *hookRecorder) sessions() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*Session(nil), h.got...)
}

// runWithTimeout 在 goroutine 中执行 fn，10s 不返回则判死锁。
func runWithTimeout(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s 疑似死锁（10s 超时）——钩子可能在 sm.mu 内被调用", what)
		return nil
	}
}

// TestOnSwitch_FiresOnceOnCreateNewAndSwitchTo：每次成功切换恰好触发一次，
// 入参为新的当前 Session。
func TestOnSwitch_FiresOnceOnCreateNewAndSwitchTo(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	initialID := sm.Current().ID

	rec := &hookRecorder{sm: sm}
	sm.SetOnSwitch(rec.hook)

	var newSess *Session
	if err := runWithTimeout(t, "CreateNew", func() error {
		var err error
		newSess, err = sm.CreateNew()
		return err
	}); err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	got := rec.sessions()
	if len(got) != 1 {
		t.Fatalf("CreateNew 后钩子应触发 1 次，实际 %d", len(got))
	}
	if got[0].ID != newSess.ID {
		t.Fatalf("钩子收到的 Session ID = %s, want %s", got[0].ID, newSess.ID)
	}
	if got[0] != newSess {
		t.Fatal("钩子收到的应是 CreateNew 返回的同一个 *Session")
	}

	// 切回 initial（目录/metadata 仍在，仅 metadata 置为 closed）
	if err := runWithTimeout(t, "SwitchTo", func() error {
		return sm.SwitchTo(initialID)
	}); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	got = rec.sessions()
	if len(got) != 2 {
		t.Fatalf("SwitchTo 后钩子应累计触发 2 次，实际 %d", len(got))
	}
	if got[1].ID != initialID {
		t.Fatalf("钩子收到的 Session ID = %s, want %s", got[1].ID, initialID)
	}
	if sm.Current().ID != initialID {
		t.Fatalf("当前 Session = %s, want %s", sm.Current().ID, initialID)
	}
}

// TestOnSwitch_NotFiredOnFailedSwitchTo：失败的 SwitchTo（B4 失败原子语义）
// 不得触发钩子。
func TestOnSwitch_NotFiredOnFailedSwitchTo(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	count := 0
	sm.SetOnSwitch(func(*Session) { count++ })

	// 失败 fixture 1（B4）：目标不存在
	if err := sm.SwitchTo("nonexistent-uuid"); err == nil {
		t.Fatal("切换到不存在的 Session 应返回错误")
	}
	if count != 0 {
		t.Fatalf("失败的 SwitchTo 不应触发钩子，实际触发 %d 次", count)
	}

	// 失败 fixture 2（B4）：目标 metadata 损坏
	corruptID := "corrupt-target"
	corruptDir := filepath.Join(dir, "sess-"+corruptID)
	if err := os.MkdirAll(corruptDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "metadata.json"), []byte("{invalid json"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := sm.SwitchTo(corruptID); err == nil {
		t.Fatal("切换到 metadata 损坏的 Session 应返回错误")
	}
	if count != 0 {
		t.Fatalf("失败的 SwitchTo 不应触发钩子，实际触发 %d 次", count)
	}
}

// TestOnSwitch_NotFiredOnFailedCreateNew：失败的 CreateNew（B4：预建
// active-session.tmp 目录让写入确定性失败）不得触发钩子。
func TestOnSwitch_NotFiredOnFailedCreateNew(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	count := 0
	sm.SetOnSwitch(func(*Session) { count++ })

	blocker := filepath.Join(dir, "active-session.tmp")
	if err := os.MkdirAll(blocker, 0755); err != nil {
		t.Fatalf("MkdirAll blocker: %v", err)
	}
	if _, err := sm.CreateNew(); err == nil {
		t.Fatal("active-session 不可写时 CreateNew 应返回错误")
	}
	if count != 0 {
		t.Fatalf("失败的 CreateNew 不应触发钩子，实际触发 %d 次", count)
	}

	// 解除阻塞后 CreateNew 恢复成功且钩子正常触发
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatalf("RemoveAll blocker: %v", err)
	}
	if _, err := sm.CreateNew(); err != nil {
		t.Fatalf("解除阻塞后 CreateNew 应成功: %v", err)
	}
	if count != 1 {
		t.Fatalf("成功的 CreateNew 应触发 1 次钩子，实际 %d", count)
	}
}

// TestOnSwitch_NotFiredByBootstrapInit：启动期 initSession / initSessionByID
// 不经 CreateNew/SwitchTo，结构性不触发钩子——注册钩子后计数必须为 0，
// 直到第一次显式切换。
func TestOnSwitch_NotFiredByBootstrapInit(t *testing.T) {
	dir := t.TempDir()
	sm1, err := NewSessionManager(dir, SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	id := sm1.Current().ID
	if err := sm1.Close(); err != nil {
		t.Fatalf("Close sm1: %v", err)
	}

	// resume 路径（initSessionByID）
	sm2, err := NewSessionManagerWithResume(dir, SessionConfig{}, id)
	if err != nil {
		t.Fatalf("NewSessionManagerWithResume: %v", err)
	}
	count := 0
	sm2.SetOnSwitch(func(*Session) { count++ })
	if count != 0 {
		t.Fatalf("启动期 init 不应触发钩子，实际 %d 次", count)
	}

	// 对照：显式 SwitchTo（含切到自身）正常触发
	if err := sm2.SwitchTo(id); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	if count != 1 {
		t.Fatalf("显式 SwitchTo 应触发 1 次钩子，实际 %d", count)
	}
}

// TestOnSwitch_PanicRecovered：钩子 panic 被 recover（打 WARN），
// 不影响已提交的切换结果，manager 后续切换仍正常工作。
func TestOnSwitch_PanicRecovered(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSessionManager(dir, SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	calls := 0
	sm.SetOnSwitch(func(*Session) {
		calls++
		panic("boom")
	})

	newSess, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("钩子 panic 不应影响切换结果: %v", err)
	}
	if calls != 1 {
		t.Fatalf("钩子应被调用 1 次，实际 %d", calls)
	}
	if sm.Current().ID != newSess.ID {
		t.Fatalf("钩子 panic 后当前 Session = %s, want %s", sm.Current().ID, newSess.ID)
	}

	// 换上正常钩子后一切照旧
	count := 0
	sm.SetOnSwitch(func(*Session) { count++ })
	if _, err := sm.CreateNew(); err != nil {
		t.Fatalf("第二次 CreateNew: %v", err)
	}
	if count != 1 {
		t.Fatalf("正常钩子应触发 1 次，实际 %d", count)
	}
}

// TestActiveSessionLogsDir：main.go trace 子命令共用的布局解析出口（B5 收敛）。
func TestActiveSessionLogsDir(t *testing.T) {
	// active-session 不存在 → ""
	empty := t.TempDir()
	if got := ActiveSessionLogsDir(empty); got != "" {
		t.Fatalf("无 active-session 时应返回 \"\"，实际 %q", got)
	}

	// 正常：active-session 指向有 logs/ 的 sess 目录（含尾部换行，F4 语义）
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sess-abc123")
	logsDir := filepath.Join(sessDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active-session"), []byte("abc123\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := ActiveSessionLogsDir(dir); got != logsDir {
		t.Fatalf("ActiveSessionLogsDir = %q, want %q", got, logsDir)
	}

	// active-session 指向的 logs/ 不存在 → ""
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "active-session"), []byte("missing"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := ActiveSessionLogsDir(dir2); got != "" {
		t.Fatalf("logs/ 不存在时应返回 \"\"，实际 %q", got)
	}

	// active-session 内容只有空白 → ""
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, "active-session"), []byte("  \n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := ActiveSessionLogsDir(dir3); got != "" {
		t.Fatalf("空白 active-session 应返回 \"\"，实际 %q", got)
	}
}
