package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBootstrap_DefaultConfig(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if sys.Store == nil {
		t.Error("Store should not be nil")
	}
	if sys.Roster == nil {
		t.Error("Roster should not be nil")
	}
	if sys.EventCh == nil {
		t.Error("EventCh should not be nil")
	}
	if sys.Watchdog == nil {
		t.Error("Watchdog should not be nil")
	}
	if sys.Config.MaxRetry != 3 {
		t.Errorf("MaxRetry = %d, want default 3", sys.Config.MaxRetry)
	}
}

func TestBootstrap_StartAndShutdown(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sys.Start(ctx, cancel)

	// Give goroutines time to start
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		sys.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not complete in time")
	}
}

func TestBootstrap_NewComponents(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if sys.Scheduler == nil {
		t.Error("Scheduler should not be nil")
	}
	if sys.Explorer == nil {
		t.Error("Explorer should not be nil")
	}
	if sys.CancelRegistry == nil {
		t.Error("CancelRegistry should not be nil")
	}
}

func TestBootstrap_MultipleWorkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("worker_count: 3\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	sys, err := Bootstrap(path, true)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if len(sys.Workers) != 3 {
		t.Errorf("Workers count = %d, want 3", len(sys.Workers))
	}
}

func TestBootstrap_DefaultSingleWorker(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if len(sys.Workers) != 1 {
		t.Errorf("Workers count = %d, want 1", len(sys.Workers))
	}
}

func TestBootstrap_MailboxComponentsInitialized(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if sys.MailboxRegistry == nil {
		t.Fatal("MailboxRegistry should not be nil")
	}
	if sys.MailNotifier == nil {
		t.Fatal("MailNotifier should not be nil")
	}
}

// TestBootstrap_ExplorerDeclaration_Default 验证默认配置下 Explorer 获得默认 Role 和 Capabilities。
// Validates: Requirements 2.1, 2.2
func TestBootstrap_ExplorerDeclaration_Default(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	// 默认配置下，ResolvedAgentDeclaration 应返回内置默认值
	caps, desc := sys.Config.ResolvedAgentDeclaration("explorer")

	// 验证默认 capabilities
	expectedCaps := []string{"codebase_read", "web_search", "message"}
	if len(caps) != len(expectedCaps) {
		t.Fatalf("default explorer capabilities length = %d, want %d", len(caps), len(expectedCaps))
	}
	for i, c := range caps {
		if c != expectedCaps[i] {
			t.Errorf("default explorer capabilities[%d] = %q, want %q", i, c, expectedCaps[i])
		}
	}

	// 验证默认 description 非空且包含关键词
	if desc == "" {
		t.Error("default explorer description should not be empty")
	}
	if !strings.Contains(desc, "Explorer") && !strings.Contains(desc, "只读") {
		t.Errorf("default explorer description should mention Explorer or 只读, got: %s", desc)
	}
}

// TestBootstrap_ExplorerDeclaration_Custom 验证自定义配置下 Explorer 获得自定义 Role 和 Capabilities。
// Validates: Requirements 2.1, 2.2
func TestBootstrap_ExplorerDeclaration_Custom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`agent_declarations:
  explorer:
    capabilities:
      - custom_read
      - custom_search
    description: "自定义调查代理描述"
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	sys, err := Bootstrap(path, true)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	caps, desc := sys.Config.ResolvedAgentDeclaration("explorer")

	// 验证自定义 capabilities
	expectedCaps := []string{"custom_read", "custom_search"}
	if len(caps) != len(expectedCaps) {
		t.Fatalf("custom explorer capabilities length = %d, want %d", len(caps), len(expectedCaps))
	}
	for i, c := range caps {
		if c != expectedCaps[i] {
			t.Errorf("custom explorer capabilities[%d] = %q, want %q", i, c, expectedCaps[i])
		}
	}

	// 验证自定义 description
	if desc != "自定义调查代理描述" {
		t.Errorf("custom explorer description = %q, want %q", desc, "自定义调查代理描述")
	}
}

// --- Session Bootstrap 集成测试 ---

// TestBootstrap_SessionMgrInitialized 验证 Bootstrap 后 SessionMgr 非 nil 且有活跃 Session。
func TestBootstrap_SessionMgrInitialized(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if sys.SessionMgr == nil {
		t.Fatal("SessionMgr should not be nil after Bootstrap")
	}
	if sys.SessionMgr.Current() == nil {
		t.Fatal("SessionMgr.Current() should not be nil — expected an active session")
	}
}

// TestBootstrap_SessionTraceRedirect 验证 Session 初始化成功时 trace 目录被重定向到 Session 的 logs/ 子目录。
func TestBootstrap_SessionTraceRedirect(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if sys.SessionMgr == nil {
		t.Fatal("SessionMgr should not be nil")
	}

	logDir := sys.SessionMgr.LogDir()
	if logDir == "" {
		t.Fatal("SessionMgr.LogDir() should not be empty")
	}

	// Verify the logs directory exists on disk
	info, err := os.Stat(logDir)
	if err != nil {
		t.Fatalf("Session logs directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("Session logs path should be a directory")
	}

	// Verify the logs directory is under the session directory
	if !strings.Contains(logDir, "sessions") {
		t.Errorf("LogDir should be under sessions directory, got: %s", logDir)
	}
	if !strings.HasSuffix(logDir, "logs") {
		t.Errorf("LogDir should end with 'logs', got: %s", logDir)
	}
}

// TestBootstrap_SessionMgrInSystem 验证 SessionMgr 被正确注入到 System 结构体。
func TestBootstrap_SessionMgrInSystem(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	// SessionMgr should be the same instance that has an active session
	if sys.SessionMgr == nil {
		t.Fatal("System.SessionMgr should not be nil")
	}
	sess := sys.SessionMgr.Current()
	if sess == nil {
		t.Fatal("System.SessionMgr.Current() should not be nil")
	}
	if sess.ID == "" {
		t.Error("Session ID should not be empty")
	}
}

// TestBootstrap_ShutdownClosesSession 验证 Shutdown 关闭 SessionManager（metadata 更新为 closed）。
func TestBootstrap_ShutdownClosesSession(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	sess := sys.SessionMgr.Current()
	if sess == nil {
		t.Fatal("expected active session before shutdown")
	}
	sessDir := sess.Dir

	ctx, cancel := context.WithCancel(context.Background())
	sys.Start(ctx, cancel)
	time.Sleep(50 * time.Millisecond)

	sys.Shutdown()

	// After shutdown, metadata should be updated to "closed"
	metaPath := filepath.Join(sessDir, "metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read metadata after shutdown: %v", err)
	}
	if !strings.Contains(string(data), `"status": "closed"`) {
		t.Errorf("metadata should have status=closed after shutdown, got: %s", string(data))
	}
	if !strings.Contains(string(data), `"ended_at"`) {
		t.Errorf("metadata should have ended_at after shutdown, got: %s", string(data))
	}
}

// TestBootstrap_RunCLI_PassesSessionMgr 验证 RunCLI 将 SessionMgr 传递给 CLI。
func TestBootstrap_RunCLI_PassesSessionMgr(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sys.Start(ctx, cancel)

	// Run CLI with /new command to verify SessionMgr is injected
	input := strings.NewReader("/new\n/quit\n")
	output := &bytes.Buffer{}

	sys.RunCLI(ctx, input, output)

	out := output.String()
	// If SessionMgr was properly passed, /new should create a new session (not show "未启用" error)
	if strings.Contains(out, "Session 管理器未启用") {
		t.Error("CLI should have SessionMgr injected, but got '未启用' error")
	}
	if !strings.Contains(out, "[session] 新 Session 已创建") {
		t.Errorf("expected session creation message, got: %s", out)
	}

	sys.Shutdown()
}

// TestBootstrap_SessionConfigFromConfig 验证 Session 配置从 Config 正确传递。
func TestBootstrap_SessionConfigFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("session_retention_days: 60\nsession_archive_max: 100\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	sys, err := Bootstrap(path, true)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	if sys.Config.SessionRetentionDays != 60 {
		t.Errorf("SessionRetentionDays = %d, want 60", sys.Config.SessionRetentionDays)
	}
	if sys.Config.SessionArchiveMax != 100 {
		t.Errorf("SessionArchiveMax = %d, want 100", sys.Config.SessionArchiveMax)
	}
	if sys.SessionMgr == nil {
		t.Fatal("SessionMgr should not be nil")
	}
}

// TestBootstrap_ShutdownNilSessionMgr 验证 SessionMgr 为 nil 时 Shutdown 不 panic。
func TestBootstrap_ShutdownNilSessionMgr(t *testing.T) {
	sys := &System{
		SessionMgr: nil,
	}
	// Should not panic
	sys.Shutdown()
}

// TestBootstrap_WorkersListCreatesCorrectWorkers verifies that when cfg.Workers
// is populated, Bootstrap creates the correct number of Worker instances and
// each Worker's agentID matches the declared ID.
// Validates: Requirements 2.1, 2.2
func TestBootstrap_WorkersListCreatesCorrectWorkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`tool_profiles:
  worker_standard:
    - read_file
    - write_file
  worker_readonly:
    - read_file
workers:
  - id: writer-1
    profile: worker_standard
  - id: writer-2
    profile: worker_standard
  - id: reader-1
    profile: worker_readonly
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	sys, err := Bootstrap(path, true)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	// Verify System.Workers length equals cfg.Workers list length
	if len(sys.Workers) != 3 {
		t.Fatalf("Workers count = %d, want 3", len(sys.Workers))
	}

	// Verify each Worker's agentID matches the declared ID in order
	expectedIDs := []string{"writer-1", "writer-2", "reader-1"}
	for i, wk := range sys.Workers {
		if wk.ID() != expectedIDs[i] {
			t.Errorf("Workers[%d].ID() = %q, want %q", i, wk.ID(), expectedIDs[i])
		}
	}
}

// TestBootstrap_WorkersListEmptyProfileFullTools verifies that a Worker with
// an empty profile string gets created successfully (full tool access).
// Validates: Requirements 2.1, 2.2
func TestBootstrap_WorkersListEmptyProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`workers:
  - id: general-1
    profile: ""
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	sys, err := Bootstrap(path, true)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	if len(sys.Workers) != 1 {
		t.Fatalf("Workers count = %d, want 1", len(sys.Workers))
	}
	if sys.Workers[0].ID() != "general-1" {
		t.Errorf("Workers[0].ID() = %q, want %q", sys.Workers[0].ID(), "general-1")
	}
}

// TestBootstrap_ToolHealthProbeIntegration verifies that Bootstrap creates a
// ToolHealthStatus and injects it into the Scheduler's SchedulerExecutor.
// Validates: Requirements 1.1, 1.4, 7.4
func TestBootstrap_ToolHealthProbeIntegration(t *testing.T) {
	sys, err := Bootstrap("nonexistent.yaml", false)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	if sys.Scheduler == nil {
		t.Fatal("Scheduler should not be nil")
	}
	if sys.Scheduler.SchedulerExec == nil {
		t.Fatal("Scheduler.SchedulerExec should not be nil")
	}
	if sys.Scheduler.SchedulerExec.ToolHealth == nil {
		t.Fatal("Scheduler.SchedulerExec.ToolHealth should not be nil — Bootstrap must always create it")
	}

	// Verify that probe results exist (web_search and web_fetch probes were executed).
	// We don't assert on Available because the test environment may lack network,
	// but the results slice must be populated.
	results := sys.Scheduler.SchedulerExec.ToolHealth.Results()
	if results == nil {
		t.Fatal("ToolHealth.Results() should not be nil")
	}

	// Expect exactly 2 probe results: web_search and web_fetch
	foundTools := make(map[string]bool)
	for _, r := range results {
		foundTools[r.Tool] = true
	}
	if !foundTools["web_search"] {
		t.Error("ToolHealth results should include an entry for web_search")
	}
	if !foundTools["web_fetch"] {
		t.Error("ToolHealth results should include an entry for web_fetch")
	}
}
