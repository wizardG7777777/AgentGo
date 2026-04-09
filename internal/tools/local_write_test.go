package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// --- recordingRoster: 模拟 roster.Roster，记录操作顺序 ---

type recordingRoster struct {
	mu         sync.Mutex
	events     []string
	occupied   bool   // true → TryClaim 返回 (false, nil)
	occupiedBy string // 提供给 IsOccupied
	claimErr   error  // TryClaim 返回的错误
}

func (r *recordingRoster) record(ev string) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingRoster) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingRoster) TryClaim(agentID, filePath string) (bool, error) {
	r.record(fmt.Sprintf("TryClaim:%s:%s", filePath, agentID))
	if r.claimErr != nil {
		return false, r.claimErr
	}
	if r.occupied {
		return false, nil
	}
	return true, nil
}

func (r *recordingRoster) Release(agentID, filePath string) error {
	r.record(fmt.Sprintf("Release:%s:%s", filePath, agentID))
	return nil
}

func (r *recordingRoster) ReleaseAll(agentID string) error { return nil }

func (r *recordingRoster) IsOccupied(filePath string) (string, bool, error) {
	who := r.occupiedBy
	if who == "" {
		who = "other-agent"
	}
	return who, r.occupied, nil
}

func (r *recordingRoster) ListByAgent(agentID string) ([]model.Claim, error) { return nil, nil }
func (r *recordingRoster) ListAllAgents() ([]string, error)                  { return nil, nil }

// --- test fixture helper ---

func newWriteGroup(t *testing.T, cache *agent.FileStateCache) (LocalWriteGroup, *recordingRoster, string) {
	t.Helper()
	tmp := t.TempDir()
	rr := &recordingRoster{}
	g := LocalWriteGroup{
		LocalReadGroup: LocalReadGroup{
			Workdir: &DefaultWorkdir{ProjectRoot: tmp},
			Cache:   cache,
		},
		Roster:  rr,
		AgentID: "agent-1",
	}
	return g, rr, tmp
}

func callWriteFile(g LocalWriteGroup, path, content, expectedHash string) (string, error) {
	args := map[string]any{
		"path":    path,
		"content": content,
	}
	if expectedHash != "" {
		args["expected_hash"] = expectedHash
	}
	return g.writeFile(context.Background(), args)
}

func callEditFile(g LocalWriteGroup, path, oldStr, newStr, expectedHash string) (string, error) {
	args := map[string]any{
		"path":    path,
		"old_str": oldStr,
		"new_str": newStr,
	}
	if expectedHash != "" {
		args["expected_hash"] = expectedHash
	}
	return g.editFile(context.Background(), args)
}

// --- tests ---

func TestLocalWriteGroup_Register_TwoTools(t *testing.T) {
	g, _, _ := newWriteGroup(t, nil)
	reg := agent.NewToolRegistry()
	g.Register(reg)
	defs := reg.Defs()
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools registered, got %d", len(defs))
	}
	names := map[string]bool{defs[0].Name: true, defs[1].Name: true}
	if !names["write_file"] || !names["edit_file"] {
		t.Fatalf("expected write_file and edit_file, got %v", defs)
	}
}

func TestWriteFile_LockAcquiredBeforeRead(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "foo.txt")
	// 预写一个已知内容的文件
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	// 传入错误的 expected_hash，期待 "写入冲突" 错误
	_, err := callWriteFile(g, path, "new content", "deadbeef")
	if err == nil || !strings.Contains(err.Error(), "写入冲突") {
		t.Fatalf("expected 写入冲突 error, got %v", err)
	}

	events := rr.snapshot()
	if len(events) < 2 {
		t.Fatalf("expected at least TryClaim+Release events, got %v", events)
	}
	if !strings.HasPrefix(events[0], "TryClaim:") {
		t.Fatalf("expected TryClaim to be first event, got %v", events)
	}
	if !strings.HasPrefix(events[len(events)-1], "Release:") {
		t.Fatalf("expected Release to be last event, got %v", events)
	}

	// 确认文件内容没被覆盖（lock-before-read 后 hash 校验失败应立即返回）
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatalf("file should not be modified, got %q", string(data))
	}
}

func TestWriteFile_BasicSuccess(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "a.txt")
	out, err := callWriteFile(g, path, "hello", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "文件已写入") {
		t.Fatalf("unexpected output: %s", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected %q, got %q", "hello", string(data))
	}
	events := rr.snapshot()
	if len(events) != 2 {
		t.Fatalf("expected exactly TryClaim+Release, got %v", events)
	}
}

func TestWriteFile_HashMismatch(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "h.txt")
	if err := os.WriteFile(path, []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := callWriteFile(g, path, "updated", "not-the-real-hash")
	if err == nil || !strings.Contains(err.Error(), "写入冲突") {
		t.Fatalf("expected 写入冲突, got %v", err)
	}
}

func TestWriteFile_HashMatch(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "m.txt")
	body := []byte("original-body")
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	realHash := computeSHA256(body)
	_, err := callWriteFile(g, path, "new-body", realHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new-body" {
		t.Fatalf("expected new-body, got %q", string(data))
	}
}

func TestWriteFile_NewFileNoHashCheck(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "brand-new.txt")
	// 文件不存在时 expected_hash 应当被忽略
	_, err := callWriteFile(g, path, "fresh", "irrelevant")
	if err != nil {
		t.Fatalf("expected success for new file, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "fresh" {
		t.Fatalf("expected 'fresh', got %q", string(data))
	}
}

func TestWriteFile_LockContention(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	rr.occupied = true
	rr.occupiedBy = "other-worker"
	path := filepath.Join(tmp, "locked.txt")
	_, err := callWriteFile(g, path, "x", "")
	if err == nil || !strings.Contains(err.Error(), "正被代理") {
		t.Fatalf("expected 正被代理 error, got %v", err)
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Fatalf("expected 占用 in error, got %v", err)
	}
}

func TestWriteFile_ParentDirCreated(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "sub", "sub2", "file.txt")
	_, err := callWriteFile(g, path, "nested", "")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if string(data) != "nested" {
		t.Fatalf("unexpected content %q", string(data))
	}
}

func TestEditFile_LockAcquiredBeforeRead(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "edit.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "world", "Go", "wrong-hash")
	if err == nil || !strings.Contains(err.Error(), "编辑冲突") {
		t.Fatalf("expected 编辑冲突, got %v", err)
	}
	events := rr.snapshot()
	if len(events) < 2 {
		t.Fatalf("expected TryClaim+Release events, got %v", events)
	}
	if !strings.HasPrefix(events[0], "TryClaim:") {
		t.Fatalf("expected first event to be TryClaim, got %v", events)
	}
	if !strings.HasPrefix(events[len(events)-1], "Release:") {
		t.Fatalf("expected last event to be Release, got %v", events)
	}
	// 文件未被修改
	data, _ := os.ReadFile(path)
	if string(data) != "hello world" {
		t.Fatalf("file should not be modified, got %q", string(data))
	}
}

func TestEditFile_BasicReplace(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "e.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "world", "Go", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "hello Go" {
		t.Fatalf("expected 'hello Go', got %q", string(data))
	}
}

func TestEditFile_NoMatch(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "n.txt")
	if err := os.WriteFile(path, []byte("alpha beta"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "gamma", "delta", "")
	if err == nil || !strings.Contains(err.Error(), "未找到匹配内容") {
		t.Fatalf("expected 未找到匹配内容, got %v", err)
	}
}

func TestEditFile_MultipleMatches(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "m.txt")
	if err := os.WriteFile(path, []byte("x x x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "x", "y", "")
	if err == nil || !strings.Contains(err.Error(), "匹配到 3 处") {
		t.Fatalf("expected 匹配到 3 处, got %v", err)
	}
}

func TestEditFile_HashMismatch(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "h.txt")
	if err := os.WriteFile(path, []byte("one world"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "world", "Go", "obviously-wrong")
	if err == nil || !strings.Contains(err.Error(), "编辑冲突") {
		t.Fatalf("expected 编辑冲突, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "one world" {
		t.Fatalf("file should not be modified, got %q", string(data))
	}
}

func TestEditFile_CacheInvalidatedAfterEdit(t *testing.T) {
	cache := agent.NewFileStateCache(10)
	g, _, tmp := newWriteGroup(t, cache)
	path := filepath.Join(tmp, "c.txt")
	if err := os.WriteFile(path, []byte("foo bar"), 0644); err != nil {
		t.Fatal(err)
	}
	// 预填充缓存
	cache.Put(path, "foo bar", computeSHA256([]byte("foo bar")))
	if _, _, ok := cache.Get(path); !ok {
		t.Fatalf("cache setup failed")
	}
	_, err := callEditFile(g, path, "bar", "baz", "")
	if err != nil {
		t.Fatalf("unexpected edit error: %v", err)
	}
	if _, _, ok := cache.Get(path); ok {
		t.Fatalf("expected cache entry to be invalidated after edit")
	}
}

// --- Artifact recording tests ---

// captureStore 是一个最小化的 fake TaskStore，只实现 Artifact 相关方法。
// 用于验证 LocalWriteGroup.recordArtifact 的调用与去重。
type captureStore struct {
	mu        sync.Mutex
	taskID    string
	artifacts []string
}

func newCaptureStore(taskID string) *captureStore {
	return &captureStore{taskID: taskID}
}

func (c *captureStore) AppendArtifact(taskID string, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if taskID != c.taskID {
		return nil
	}
	for _, p := range c.artifacts {
		if p == path {
			return nil // 去重
		}
	}
	c.artifacts = append(c.artifacts, path)
	return nil
}

// 以下方法仅为满足 store.TaskStore 接口，实际不会被调用
func (c *captureStore) PublishTask(*model.Task) error             { return nil }
func (c *captureStore) ClaimTask(string, string) error            { return nil }
func (c *captureStore) SubmitResult(string, string, string) error { return nil }
func (c *captureStore) TransitionState(string, model.TaskStatus, model.TaskStatus) error {
	return nil
}
func (c *captureStore) FailTask(string, string, string) error                      { return nil }
func (c *captureStore) FailTaskBySystem(string, string) error                      { return nil }
func (c *captureStore) RetryRollback(string, string, string) error                 { return nil }
func (c *captureStore) AppendOutput(string, string, string) error                  { return nil }
func (c *captureStore) QueryAvailable(string) ([]*model.Task, error)               { return nil, nil }
func (c *captureStore) GetTask(string) (*model.Task, error)                        { return nil, nil }
func (c *captureStore) GetDependencyResults(string) (map[string]string, error)     { return nil, nil }
func (c *captureStore) GetDependencyArtifacts(string) (map[string][]string, error) { return nil, nil }
func (c *captureStore) RecordLastResponse(string, string) error                    { return nil }
func (c *captureStore) ScanAll() ([]*model.Task, error)                            { return nil, nil }
func (c *captureStore) AppendToolCall(string, store.ToolCallRecord) error          { return nil }
func (c *captureStore) QueryToolCalls(string, string) ([]store.ToolCallRecord, error) {
	return nil, nil
}

func TestWriteFile_RecordsArtifact(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "out.md")
	cs := newCaptureStore("task-001")
	g := LocalWriteGroup{
		LocalReadGroup: LocalReadGroup{Workdir: &DefaultWorkdir{ProjectRoot: tmp}},
		Roster:         &recordingRoster{},
		AgentID:        "agent-1",
		Store:          cs,
		ProjectRoot:    tmp,
	}
	r := agent.NewToolRegistry()
	g.Register(r)

	// 注入 task ID 到 context
	ctx := agent.WithAgentContext(context.Background(), "agent-1", "task-001", 0)
	_, err := r.Dispatch(ctx, llm.ToolCall{
		Name:      "write_file",
		Arguments: map[string]any{"path": path, "content": "hello"},
	})
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if len(cs.artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d: %v", len(cs.artifacts), cs.artifacts)
	}
	if cs.artifacts[0] != "out.md" {
		t.Errorf("expected relative path 'out.md', got %s", cs.artifacts[0])
	}
}

func TestWriteFile_ArtifactDedupOnRewrite(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "out.md")
	cs := newCaptureStore("task-001")
	g := LocalWriteGroup{
		LocalReadGroup: LocalReadGroup{Workdir: &DefaultWorkdir{ProjectRoot: tmp}},
		Roster:         &recordingRoster{},
		AgentID:        "agent-1",
		Store:          cs,
		ProjectRoot:    tmp,
	}
	r := agent.NewToolRegistry()
	g.Register(r)
	ctx := agent.WithAgentContext(context.Background(), "agent-1", "task-001", 0)

	// 写入同一文件 3 次
	for i := 0; i < 3; i++ {
		_, _ = r.Dispatch(ctx, llm.ToolCall{
			Name:      "write_file",
			Arguments: map[string]any{"path": path, "content": fmt.Sprintf("v%d", i)},
		})
	}

	if len(cs.artifacts) != 1 {
		t.Errorf("expected dedup to 1 entry, got %d: %v", len(cs.artifacts), cs.artifacts)
	}
}

func TestNormalizeArtifactPath(t *testing.T) {
	tests := []struct {
		name        string
		absPath     string
		projectRoot string
		want        string
	}{
		{
			name:        "relative to project root",
			absPath:     "/Users/dev/project/docs/foo.md",
			projectRoot: "/Users/dev/project",
			want:        "docs/foo.md",
		},
		{
			name:        "deeply nested under project root",
			absPath:     "/Users/dev/project/internal/tools/local_write.go",
			projectRoot: "/Users/dev/project",
			want:        "internal/tools/local_write.go",
		},
		{
			name:        "outside project root → fallback to absolute",
			absPath:     "/etc/passwd",
			projectRoot: "/Users/dev/project",
			want:        "/etc/passwd",
		},
		{
			name:        "empty projectRoot → original cleaned path",
			absPath:     "/tmp/foo.md",
			projectRoot: "",
			want:        "/tmp/foo.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeArtifactPath(tt.absPath, tt.projectRoot)
			if got != tt.want {
				t.Errorf("normalizeArtifactPath(%q, %q) = %q, want %q", tt.absPath, tt.projectRoot, got, tt.want)
			}
		})
	}
}
