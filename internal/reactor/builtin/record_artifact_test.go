package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
)

// fakeStoreView 是只实现 StoreHookView 的最小 mock，记录 AppendArtifact 的调用。
// task 非 nil 时 GetTask 返回它（shell 补登路径需要带 ExpectedArtifacts 的任务）。
type fakeStoreView struct {
	mu           sync.Mutex
	calls        []artifactCall
	failOnAppend bool
	task         *model.Task
}

type artifactCall struct {
	taskID string
	path   string
	meta   model.ArtifactMeta
}

func (s *fakeStoreView) GetTask(taskID string) (*model.Task, error) {
	if s.task != nil && s.task.ID == taskID {
		return s.task, nil
	}
	return nil, nil
}
func (s *fakeStoreView) AppendArtifact(taskID, path string) error {
	return s.AppendArtifactWithMeta(taskID, path, model.ArtifactMeta{})
}
func (s *fakeStoreView) AppendArtifactWithMeta(taskID, path string, meta model.ArtifactMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOnAppend {
		return errors.New("simulated store failure")
	}
	s.calls = append(s.calls, artifactCall{taskID, path, meta})
	return nil
}
func (s *fakeStoreView) QueryToolCalls(taskID, tool string) ([]store.ToolCallRecord, error) {
	return nil, nil
}
func (s *fakeStoreView) GetToolCallHistory(taskID string) []store.ToolCallRecord { return nil }
func (s *fakeStoreView) ScanPendingByEventSource(source, eventType string) []*model.Task {
	return nil
}
func (s *fakeStoreView) GetReadSet(taskID string) (map[string]model.ReadInfo, error) {
	return nil, nil
}
func (s *fakeStoreView) snapshotCalls() []artifactCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]artifactCall(nil), s.calls...)
}

var _ store.StoreHookView = (*fakeStoreView)(nil)

func TestRecordArtifactReactor_BasicSubscribe(t *testing.T) {
	r := NewRecordArtifactReactor(nil, "")
	subs := r.Subscribe()
	if len(subs) != 2 || subs[0] != trace.KindFileWritten || subs[1] != trace.KindShellExecuted {
		t.Errorf("expected [KindFileWritten KindShellExecuted] subscribe, got %v", subs)
	}
	if r.IsSync() {
		t.Error("should be async")
	}
	if r.Priority() != 950 {
		t.Errorf("priority=%d want 950", r.Priority())
	}
	if r.Name() != "record-artifact" {
		t.Errorf("name=%q want record-artifact", r.Name())
	}
}

func TestRecordArtifactReactor_AppendsAbsolutePathRelativized(t *testing.T) {
	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, "/proj")

	err := r.Run(trace.Event{
		Kind:   trace.KindFileWritten,
		TaskID: "t1",
		Path:   "/proj/docs/foo.md",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := s.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].taskID != "t1" || calls[0].path != "docs/foo.md" {
		t.Errorf("call wrong: %+v want {t1 docs/foo.md}", calls[0])
	}
}

func TestRecordArtifactReactor_ViaTraceDispatcher(t *testing.T) {
	s := &fakeStoreView{}
	reg := reactor.NewRegistry()
	if err := reg.Register(NewRecordArtifactReactor(s, "/proj")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	originalWriter := trace.Default()
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefault(nil)
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() {
		trace.SetDefault(originalWriter)
		trace.SetDefaultDispatcher(originalDispatcher)
	})

	trace.Emit(trace.Event{
		Kind:   trace.KindFileWritten,
		TaskID: "t-dispatch",
		Path:   "/proj/docs/dispatch.md",
	})

	deadline := time.After(500 * time.Millisecond)
	for {
		calls := s.snapshotCalls()
		if len(calls) == 1 {
			if calls[0].taskID != "t-dispatch" || calls[0].path != "docs/dispatch.md" {
				t.Fatalf("call wrong: %+v", calls[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("record-artifact reactor did not append artifact via trace dispatcher; calls=%+v", calls)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestRecordArtifactReactor_NilStoreSilent(t *testing.T) {
	r := NewRecordArtifactReactor(nil, "/proj")
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t1", Path: "/proj/x.md"}); err != nil {
		t.Errorf("nil store should be silent, got %v", err)
	}
}

func TestRecordArtifactReactor_EmptyPathSkipped(t *testing.T) {
	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, "/proj")
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t1"}); err != nil {
		t.Errorf("empty path should not error, got %v", err)
	}
	if calls := s.snapshotCalls(); len(calls) != 0 {
		t.Errorf("empty path should skip Append, got %d calls", len(calls))
	}
}

func TestRecordArtifactReactor_StoreFailureTolerated(t *testing.T) {
	// store.AppendArtifact 失败时 Reactor 不返回 error——artifact 是 best-effort 审计
	s := &fakeStoreView{failOnAppend: true}
	r := NewRecordArtifactReactor(s, "/proj")
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t1", Path: "/proj/a.md"}); err != nil {
		t.Errorf("store failure should be tolerated, got %v", err)
	}
}

func TestRecordArtifactReactor_ComputesMetaFromDisk(t *testing.T) {
	// hash 正确性：reactor 读取落盘文件计算 sha256/bytes 并随路径一并登记
	root := t.TempDir()
	content := "hello artifact meta\n"
	abs := filepath.Join(root, "docs", "foo.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, root)
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t1", Path: abs}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := s.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].path != "docs/foo.md" {
		t.Errorf("path=%q want docs/foo.md", calls[0].path)
	}
	wantSum := sha256.Sum256([]byte(content))
	if want := hex.EncodeToString(wantSum[:]); calls[0].meta.SHA256 != want {
		t.Errorf("sha256=%q want %q", calls[0].meta.SHA256, want)
	}
	if calls[0].meta.Bytes != int64(len(content)) {
		t.Errorf("bytes=%d want %d", calls[0].meta.Bytes, len(content))
	}
}

func TestRecordArtifactReactor_MetaDegradesOnReadFailure(t *testing.T) {
	// 写文件失败降级：文件不可读（不存在）时只登记路径、meta 为零值，不报错
	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, t.TempDir())
	missing := filepath.Join(t.TempDir(), "no-such-file.md")
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t1", Path: missing}); err != nil {
		t.Fatalf("读取失败不应返回错误, got %v", err)
	}
	calls := s.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("读取失败仍应登记路径, calls=%d", len(calls))
	}
	if !calls[0].meta.IsZero() {
		t.Errorf("读取失败应降级为零值 meta, got %+v", calls[0].meta)
	}
}

func TestNormalizeArtifactPath_RelativeProjectRoot(t *testing.T) {
	// 回归（2026-07-21 验收马拉松事故）：setting.yaml 的 project_root: "." 是
	// 相对路径，filepath.Rel(".", 绝对路径) 直接报错会让 artifact 被登记成
	// 绝对路径，导致验收比对永远失败。修复后必须先 Abs 再 Rel。
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cwd, "docs", "foo.md")
	if got := normalizeArtifactPath(abs, "."); got != "docs/foo.md" {
		t.Errorf("got=%q want=%q", got, "docs/foo.md")
	}
}

func TestNormalizeArtifactPath(t *testing.T) {
	cases := []struct {
		name        string
		abs         string
		root        string
		want        string
		wantContain string
	}{
		{"inside-root", "/proj/sub/foo.md", "/proj", "sub/foo.md", ""},
		{"outside-root", "/elsewhere/x.md", "/proj", "", "x.md"}, // 走 cleaned 路径
		{"empty-root", "/proj/x.md", "", "/proj/x.md", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeArtifactPath(tc.abs, tc.root)
			if tc.want != "" && got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
			if tc.wantContain != "" && !strings.Contains(got, tc.wantContain) {
				t.Errorf("got=%q expected to contain %q", got, tc.wantContain)
			}
		})
	}
}

// ===== workspace.Manager 注入（ResolveForTask 路径解析）=====
// file_written 的 Path 恒为主根逻辑路径；隔离任务的真实文件在合并前位于
// workspace 副本。这里锁定注入 Manager 后 reactor 经 ResolveForTask
// 定位真实物理文件，同时保持 artifact 登记使用主根逻辑路径。

// 注入 Manager 且任务有活动视图：meta 仍按解析后的物理路径正确重算。
func TestRecordArtifactReactor_WithWorkspaceManager(t *testing.T) {
	root := t.TempDir()
	content := "isolated write\n"
	abs := filepath.Join(root, "docs", "foo.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := workspace.NewManager(root, nil)
	if _, err := mgr.Materialize("t-iso"); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, root, mgr)
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t-iso", Path: abs}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := s.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].path != "docs/foo.md" {
		t.Errorf("登记路径应仍为主根逻辑相对路径, got %q", calls[0].path)
	}
	wantSum := sha256.Sum256([]byte(content))
	if want := hex.EncodeToString(wantSum[:]); calls[0].meta.SHA256 != want {
		t.Errorf("注入 Manager 后 sha256 应仍按物理文件重算, got %q want %q", calls[0].meta.SHA256, want)
	}
}

// 注入 Manager 但任务无活动视图：ResolveForTask 原样返回主根路径，行为不变。
func TestRecordArtifactReactor_ManagerWithoutActiveView(t *testing.T) {
	root := t.TempDir()
	content := "plain write\n"
	abs := filepath.Join(root, "a.md")
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := workspace.NewManager(root, nil) // 不 Materialize：任务无视图
	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, root, mgr)
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t-plain", Path: abs}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := s.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	wantSum := sha256.Sum256([]byte(content))
	if want := hex.EncodeToString(wantSum[:]); calls[0].meta.SHA256 != want {
		t.Errorf("无视图任务应回退主根路径重算, got %q want %q", calls[0].meta.SHA256, want)
	}
}

// nil Manager（缺省注入）：维持注入前行为——直接用主根 Path 重算。
func TestRecordArtifactReactor_NilManagerUnchanged(t *testing.T) {
	root := t.TempDir()
	content := "legacy path\n"
	abs := filepath.Join(root, "b.md")
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &fakeStoreView{}
	r := NewRecordArtifactReactor(s, root) // 不传 wsMgr
	if err := r.Run(trace.Event{Kind: trace.KindFileWritten, TaskID: "t-legacy", Path: abs}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := s.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	wantSum := sha256.Sum256([]byte(content))
	if want := hex.EncodeToString(wantSum[:]); calls[0].meta.SHA256 != want {
		t.Errorf("nil Manager 应按注入前行为直读主根, got %q want %q", calls[0].meta.SHA256, want)
	}
}

// === shell 写事实补登（KindShellExecuted → ExpectedArtifacts 盘后补登） ===

// shellExecEvent 构造一次 run_shell 执行事件。
func shellExecEvent(taskID, outcome string) trace.Event {
	return trace.Event{
		Kind:      trace.KindShellExecuted,
		TaskID:    taskID,
		AgentID:   "agent-x",
		Tool:      "run_shell",
		ShellExec: &trace.ShellExec{Command: "Set-Content out.txt x", Outcome: outcome, ExitCode: 0},
	}
}

// shell 成功 + 声明的预期产物已在盘上 → 补登路径与落盘重算的 meta。
func TestRecordArtifactReactor_ShellBackfillRegistersExpectedArtifact(t *testing.T) {
	root := t.TempDir()
	content := "line2-agentA（shell 写入）"
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	fs := &fakeStoreView{task: &model.Task{ID: "t1", ExpectedArtifacts: []string{"out.txt"}}}
	r := NewRecordArtifactReactor(fs, root)
	if err := r.Run(shellExecEvent("t1", "success")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := fs.snapshotCalls()
	if len(calls) != 1 || calls[0].path != "out.txt" || calls[0].taskID != "t1" {
		t.Fatalf("应补登 1 条 out.txt，实际: %+v", calls)
	}
	sum := sha256.Sum256([]byte(content))
	wantSHA := hex.EncodeToString(sum[:])
	if calls[0].meta.SHA256 != wantSHA || calls[0].meta.Bytes != int64(len(content)) {
		t.Fatalf("meta 应为落盘重算值 sha=%s bytes=%d，实际: %+v", wantSHA, len(content), calls[0].meta)
	}
}

// 盘上文件变化后再次补登：应携带新 hash（store 侧据以更新 ArtifactMeta）。
func TestRecordArtifactReactor_ShellBackfillRefreshesMetaOnChange(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "out.txt")
	fs := &fakeStoreView{task: &model.Task{ID: "t1", ExpectedArtifacts: []string{"out.txt"}}}
	r := NewRecordArtifactReactor(fs, root)

	_ = os.WriteFile(p, []byte("v1"), 0o644)
	if err := r.Run(shellExecEvent("t1", "success")); err != nil {
		t.Fatalf("Run#1: %v", err)
	}
	_ = os.WriteFile(p, []byte("v2-changed"), 0o644)
	if err := r.Run(shellExecEvent("t1", "success")); err != nil {
		t.Fatalf("Run#2: %v", err)
	}
	calls := fs.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("应补登 2 次，实际 %d", len(calls))
	}
	v1SHA := calls[0].meta.SHA256
	v2SHA := calls[1].meta.SHA256
	if v1SHA == v2SHA {
		t.Fatalf("文件变更后两次补登的 hash 应不同，实际均为 %s", v1SHA)
	}
	sum := sha256.Sum256([]byte("v2-changed"))
	if v2SHA != hex.EncodeToString(sum[:]) {
		t.Fatalf("第二次补登 hash 应为新内容重算值，实际 %s", v2SHA)
	}
}

// 失败/超时命令不补登（部分写入是噪音）。
func TestRecordArtifactReactor_ShellBackfillSkipsFailureAndTimeout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	fs := &fakeStoreView{task: &model.Task{ID: "t1", ExpectedArtifacts: []string{"out.txt"}}}
	r := NewRecordArtifactReactor(fs, root)
	for _, outcome := range []string{"failure", "timeout"} {
		if err := r.Run(shellExecEvent("t1", outcome)); err != nil {
			t.Fatalf("Run(%s): %v", outcome, err)
		}
	}
	if got := len(fs.snapshotCalls()); got != 0 {
		t.Fatalf("failure/timeout 不应补登，实际 %d 条", got)
	}
}

// 声明了但盘上不存在 → 跳过（不登记幽灵产物）；任务无 ExpectedArtifacts → 短路。
func TestRecordArtifactReactor_ShellBackfillSkipsMissingAndNoExpected(t *testing.T) {
	root := t.TempDir()
	fs := &fakeStoreView{task: &model.Task{ID: "t1", ExpectedArtifacts: []string{"ghost.txt"}}}
	r := NewRecordArtifactReactor(fs, root)
	if err := r.Run(shellExecEvent("t1", "success")); err != nil {
		t.Fatalf("Run ghost: %v", err)
	}

	fs2 := &fakeStoreView{task: &model.Task{ID: "t2"}}
	r2 := NewRecordArtifactReactor(fs2, root)
	if err := r2.Run(shellExecEvent("t2", "success")); err != nil {
		t.Fatalf("Run no-expected: %v", err)
	}
	if len(fs.snapshotCalls()) != 0 || len(fs2.snapshotCalls()) != 0 {
		t.Fatal("幽灵路径与无声明任务均不应补登")
	}
}

// 隔离任务：shell 在 workspace 根下执行，产物落副本——补登必须经
// ResolveForTask 读 workspace 副本的内容（主根旧版本不得污染 meta）。
func TestRecordArtifactReactor_ShellBackfillResolvesWorkspaceCopy(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "out.txt")
	if err := os.WriteFile(mainPath, []byte("主根旧内容"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mgr := workspace.NewManager(root, nil)
	v, err := mgr.Materialize("t1")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// 模拟 shell 在 workspace 根写入副本（经 COW 解析后的落点）。
	wsPath, err := v.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath: %v", err)
	}
	wsContent := "workspace 副本新内容"
	if err := os.WriteFile(wsPath, []byte(wsContent), 0o644); err != nil {
		t.Fatalf("写副本: %v", err)
	}

	fs := &fakeStoreView{task: &model.Task{ID: "t1", ExpectedArtifacts: []string{"out.txt"}}}
	r := NewRecordArtifactReactor(fs, root, mgr)
	if err := r.Run(shellExecEvent("t1", "success")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := fs.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("应补登 1 条，实际 %d", len(calls))
	}
	sum := sha256.Sum256([]byte(wsContent))
	if calls[0].meta.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("meta 应来自 workspace 副本而非主根旧内容，实际 sha=%s", calls[0].meta.SHA256)
	}
}

// e2e 回归（2026-07-27 事故）：worker 用 shell 写预期产物，补登后
// agent.CheckExpectedArtifacts 不得再报「你实际没有写入任何文件」假阴性。
func TestRecordArtifactReactor_ShellBackfillClosesExpectedArtifactsCheck(t *testing.T) {
	root := t.TempDir()
	s := store.NewMemoryTaskStore(nil, 16, 4, 60)
	task := &model.Task{ID: "t1", ExpectedArtifacts: []string{"shared.txt"}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	// worker 用 shell 写出产物（账本尚无任何记录）。
	content := "line1\nline2-agentA\n"
	if err := os.WriteFile(filepath.Join(root, "shared.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := agent.CheckExpectedArtifacts(s, "t1")
	if len(before.Missing) != 1 {
		t.Fatalf("补登前应有 1 个缺失产物，实际: %+v", before)
	}

	r := NewRecordArtifactReactor(s, root)
	if err := r.Run(shellExecEvent("t1", "success")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	after := agent.CheckExpectedArtifacts(s, "t1")
	if len(after.Missing) != 0 {
		t.Fatalf("补登后 expected_artifacts 校验应通过，实际缺失: %v", after.Missing)
	}
	// 账本里的 meta 应可用于后续 file_hash 证据（sha256 与内容一致）。
	got, err := s.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	meta := got.ArtifactMeta["shared.txt"]
	sum := sha256.Sum256([]byte(content))
	if meta.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("ArtifactMeta sha 不符: %s", meta.SHA256)
	}
}
