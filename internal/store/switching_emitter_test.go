package store

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

// TestSwitchingEmitter_StoreHistorySurvivesSessionSwitch 复刻 bootstrap 的接线
// （一个共享 SwitchingEmitter 注入 store），验证 Session 切换后：
//   - 切换前的事件落在旧 Session 的 history.jsonl
//   - 切换后的事件落在新 Session 的 history.jsonl
//   - 全程无 ErrHistoryLogClosed WARN（旧实现注入裸 *HistoryLog 时必然刷屏）
func TestSwitchingEmitter_StoreHistorySurvivesSessionSwitch(t *testing.T) {
	dir := t.TempDir()
	cfg := session.SessionConfig{RetentionDays: 30, ArchiveMax: 50, Enabled: true}
	sm, err := session.NewSessionManager(dir, cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	sm.EnableHistoryLog()
	// Windows：必须先关句柄，TempDir 清理才不会失败
	t.Cleanup(func() { _ = sm.Close() })

	s, _ := newTestStore(10, 100)
	s.SetHistoryEmitter(session.NewSwitchingEmitter(sm))

	// 捕获标准日志，断言 emit 路径没有刷错误
	var logBuf bytes.Buffer
	prevLogOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevLogOut) })

	sessA := sm.Current().ID

	// 切换前：publish + transition（claim）任务 1，事件应进 A 的 history.jsonl
	task1 := &model.Task{Description: "before switch"}
	if err := s.PublishTask(task1); err != nil {
		t.Fatalf("PublishTask task1: %v", err)
	}
	if err := s.ClaimTask("agent-1", task1.ID); err != nil {
		t.Fatalf("ClaimTask task1: %v", err)
	}

	// 切换 Session（/new 等价路径）
	if _, err := sm.CreateNew(); err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	sessB := sm.Current().ID
	if sessB == sessA {
		t.Fatal("CreateNew did not switch session")
	}

	// 切换后：publish 任务 2，事件应进 B 的 history.jsonl
	task2 := &model.Task{Description: "after switch"}
	if err := s.PublishTask(task2); err != nil {
		t.Fatalf("PublishTask task2: %v", err)
	}

	readHist := func(id string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "sess-"+id, "history.jsonl"))
		if err != nil {
			t.Fatalf("read history.jsonl for %s: %v", id, err)
		}
		return string(data)
	}

	histA := readHist(sessA)
	if !strings.Contains(histA, task1.ID) {
		t.Errorf("session A history missing pre-switch task %s, got: %s", task1.ID, histA)
	}
	if strings.Contains(histA, task2.ID) {
		t.Errorf("session A history must not contain post-switch task %s, got: %s", task2.ID, histA)
	}

	histB := readHist(sessB)
	if !strings.Contains(histB, task2.ID) {
		t.Errorf("session B history missing post-switch task %s（裸句柄注入时这里必空）, got: %s", task2.ID, histB)
	}
	if strings.Contains(histB, task1.ID) {
		t.Errorf("session B history must not contain pre-switch task %s, got: %s", task1.ID, histB)
	}

	if out := logBuf.String(); strings.Contains(out, "history emit") || strings.Contains(out, "history log is closed") {
		t.Errorf("emit path logged warnings (ErrHistoryLogClosed 回归？): %s", out)
	}
}
