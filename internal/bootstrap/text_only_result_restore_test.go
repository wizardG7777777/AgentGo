// E8 端到端守护：text-only 结果在产生处经 agent.Agent.OnTextOnlyPersisted 回调
// 结构化记入 ResultSnapshot，随 saveRuntimeSnapshot 落盘，下次启动经既有
// snapshot 路径恢复——全程不依赖 system.log 文本刮取（该路径已随 E8 删除）。
package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/store"
)

// TestTextOnlyResult_StructuralSnapshotRoundTrip 模拟完整重启闭环：
// boot1 接线回调 → 经回调路径产生 text-only 结果 → saveRuntimeSnapshot 落盘；
// boot2 在同一 session 目录新建 System 并从 snapshot 播种，断言恢复出的结果
// 正文/路径与产生时一致（Restored 标记置位），且全程无 system.log 参与。
func TestTextOnlyResult_StructuralSnapshotRoundTrip(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "sessions")

	// ---- boot 1：接线 + 产生 text-only 结果 + Shutdown 落盘 ----
	sm1, err := session.NewSessionManager(sessDir, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager boot1: %v", err)
	}
	t.Cleanup(func() { _ = sm1.Close() })

	schedAgent := &agent.Agent{ID: "scheduler-1", TextOnlyReportsDir: filepath.Join(root, "reports")}
	sys1 := &System{
		Store:           store.NewMemoryTaskStore(make(chan model.Event, 4), 10, 1, 60),
		Roster:          roster.NewMemoryRoster(),
		MailboxRegistry: mailbox.NewRegistry(8),
		Scheduler:       &scheduler.Bundle{Agent: schedAgent, History: scheduler.NewSessionHistory(4)},
		SessionMgr:      sm1,
	}
	// Bootstrap 启动路径：restoreOrReconcileRuntime 负责接线回调（首次启动无
	// snapshot 时同样接线）。
	if err := restoreOrReconcileRuntime(sys1, nil); err != nil {
		t.Fatalf("restoreOrReconcileRuntime boot1: %v", err)
	}
	if schedAgent.OnTextOnlyPersisted == nil {
		t.Fatal("scheduler agent 的 OnTextOnlyPersisted 未接线")
	}

	// agent 侧 persistTextOnlySubmission 落盘成功后触发回调（persist→回调的
	// 衔接由 internal/agent 的回调测试守护）；此处经回调路径产生结果。
	content := "text-only 最终交付正文\n第二行"
	reportPath := filepath.Join(schedAgent.TextOnlyReportsDir, "text_only_task-1.md")
	if err := os.MkdirAll(schedAgent.TextOnlyReportsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll reports: %v", err)
	}
	if err := os.WriteFile(reportPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile report: %v", err)
	}
	schedAgent.OnTextOnlyPersisted(reportPath, content)

	snap1 := sys1.resultSnapshot()
	if snap1 == nil || snap1.Text != content || snap1.Path != reportPath {
		t.Fatalf("产生处记账结果 = %#v", snap1)
	}

	sys1.saveRuntimeSnapshot()

	// 前置断言：全程没有 system.log——恢复不允许依赖日志文本刮取。
	if _, statErr := os.Stat(filepath.Join(sm1.LogDir(), "system.log")); !os.IsNotExist(statErr) {
		t.Fatalf("测试前置条件失败：不应存在 system.log (err=%v)", statErr)
	}

	// ---- boot 2：同一 session 目录新建 System，从 snapshot 播种 ----
	sm2, err := session.NewSessionManager(sessDir, session.SessionConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewSessionManager boot2: %v", err)
	}
	t.Cleanup(func() { _ = sm2.Close() })

	snap := currentRecoveredSnapshot(sm2)
	if snap == nil || snap.Result == nil {
		t.Fatalf("重启后 snapshot 应携带结果: %#v", snap)
	}

	sys2 := &System{
		Store:           store.NewMemoryTaskStore(make(chan model.Event, 4), 10, 1, 60),
		Roster:          roster.NewMemoryRoster(),
		MailboxRegistry: mailbox.NewRegistry(8),
		Scheduler:       &scheduler.Bundle{History: scheduler.NewSessionHistory(4)},
		SessionMgr:      sm2,
	}
	if err := restoreOrReconcileRuntime(sys2, snap); err != nil {
		t.Fatalf("restoreOrReconcileRuntime boot2: %v", err)
	}
	// 与 Bootstrap 启动路径一致：loadInitialResult 标记 Restored 后播种。
	if initial := loadInitialResult(root, sm2, snap); initial != nil {
		sys2.seedResult(initial)
	}

	got := sys2.resultSnapshot()
	if got == nil {
		t.Fatal("重启后结果未恢复")
	}
	if got.Text != content {
		t.Errorf("恢复正文 = %q, want %q", got.Text, content)
	}
	if got.Path != reportPath {
		t.Errorf("恢复路径 = %q, want %q", got.Path, reportPath)
	}
	if !got.Restored {
		t.Error("恢复结果应标记 Restored")
	}
	if got.SavedAt == "" {
		t.Error("恢复结果应携带 SavedAt")
	}
	if text := initialResultText(got); text != content {
		t.Errorf("TUI InitialResult = %q, want %q", text, content)
	}
}

// TestLoadInitialResult_SnapshotOnly 钉住 E8 后的恢复语义：无 snapshot 结果时
// 返回 nil（不再有 system.log 刮取兜底）；有结果时复制一份并标记 Restored，
// 不改写入参 snapshot。
func TestLoadInitialResult_SnapshotOnly(t *testing.T) {
	if got := loadInitialResult(t.TempDir(), nil, nil); got != nil {
		t.Fatalf("无 snapshot 应返回 nil, got %#v", got)
	}
	if got := loadInitialResult(t.TempDir(), nil, &session.Snapshot{}); got != nil {
		t.Fatalf("snapshot 无结果应返回 nil, got %#v", got)
	}
	snap := &session.Snapshot{Result: &session.ResultSnapshot{
		Text: "正文", Path: "/r.md", SavedAt: "2026-07-18T00:00:00Z",
	}}
	got := loadInitialResult(t.TempDir(), nil, snap)
	if got == nil || got.Text != "正文" || got.Path != "/r.md" || !got.Restored {
		t.Fatalf("snapshot 结果应原样复制并标记 Restored, got %#v", got)
	}
	if snap.Result.Restored {
		t.Fatal("不应改写入参 snapshot")
	}
}

// TestWireTextOnlyResultPersistence_NilSafety 覆盖接线函数的降级场景：
// nil System / nil Scheduler / nil Agent 均安全跳过；装配后回调正常记账。
func TestWireTextOnlyResultPersistence_NilSafety(t *testing.T) {
	wireTextOnlyResultPersistence(nil)
	wireTextOnlyResultPersistence(&System{})
	wireTextOnlyResultPersistence(&System{Scheduler: &scheduler.Bundle{}})

	sys := &System{Scheduler: &scheduler.Bundle{Agent: &agent.Agent{ID: "scheduler-1"}}}
	wireTextOnlyResultPersistence(sys)
	if sys.Scheduler.Agent.OnTextOnlyPersisted == nil {
		t.Fatal("应完成接线")
	}
	// 回调记账不依赖 Store/SessionMgr（seedResult 仅操作内存态）。
	sys.Scheduler.Agent.OnTextOnlyPersisted("/p.md", "正文")
	if got := sys.resultSnapshot(); got == nil || got.Text != "正文" || got.Path != "/p.md" {
		t.Fatalf("回调记账 = %#v", got)
	}
}
