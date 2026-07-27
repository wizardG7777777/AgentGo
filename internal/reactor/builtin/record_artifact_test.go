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

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
)

// fakeStoreView 是只实现 StoreHookView 的最小 mock，记录 AppendArtifact 的调用。
type fakeStoreView struct {
	mu           sync.Mutex
	calls        []artifactCall
	failOnAppend bool
}

type artifactCall struct {
	taskID string
	path   string
	meta   model.ArtifactMeta
}

func (s *fakeStoreView) GetTask(taskID string) (*model.Task, error) { return nil, nil }
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
	if len(subs) != 1 || subs[0] != trace.KindFileWritten {
		t.Errorf("expected only KindFileWritten subscribe, got %v", subs)
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
