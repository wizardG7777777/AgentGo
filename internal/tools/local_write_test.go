package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/tools/hashline"
)

// --- recordingRoster: 模拟 roster.Roster，记录操作顺序 ---

type recordingRoster struct {
	mu         sync.Mutex
	events     []string
	occupied   bool   // true → TryClaim 返回 (false, nil)
	occupiedBy string // 提供给 IsOccupied
	claimErr   error  // TryClaim 返回的错误

	// §8.3：WaitForRelease mock 控制
	waitErr      error // WaitForRelease 返回的错误（nil = 模拟成功唤醒）
	claimAfterWait bool // WaitForRelease 成功后，下次 TryClaim 是否放行
	tryClaims    int   // TryClaim 调用计数
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
	r.mu.Lock()
	r.tryClaims++
	callNum := r.tryClaims
	r.mu.Unlock()
	r.record(fmt.Sprintf("TryClaim:%s:%s", filePath, agentID))
	if r.claimErr != nil {
		return false, r.claimErr
	}
	// 首次 occupied，等待后可配置放行
	if r.occupied {
		if r.claimAfterWait && callNum > 1 {
			return true, nil // 排队唤醒后的重试放行
		}
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
func (r *recordingRoster) ListClaims() map[string][]string                   { return nil }

func (r *recordingRoster) WaitForRelease(_ context.Context, agentID, filePath string, _ time.Duration) error {
	r.record(fmt.Sprintf("WaitForRelease:%s:%s", filePath, agentID))
	return r.waitErr
}

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

// C7 删除：TestWriteFile_LockAcquiredBeforeRead
//
// 该测试断言的是"hash 校验在 Roster 锁内执行"的不变式。这是 C7
// 决议 B1 明确放弃的语义：hash 校验迁移到 ValidateExpectedHashHook
// (PreCall) 后发生在 Roster 锁外，引入微秒级 TOCTOU 窗口，已被
// hookSystem.md §10.1 接受。等价覆盖在
// internal/hook/builtin/validate_expected_hash_test.go 中重建。

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

// C7 删除：TestWriteFile_HashMismatch
// 等价覆盖在 internal/hook/builtin/validate_expected_hash_test.go 中。

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

// C7 删除：TestEditFile_LockAcquiredBeforeRead — 同 TestWriteFile_LockAcquiredBeforeRead，
// hash 校验已迁移到 hook 层（决策 B1）。等价覆盖在
// internal/hook/builtin/validate_expected_hash_test.go 中。

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

// C7 删除：TestEditFile_HashMismatch — 等价覆盖在
// internal/hook/builtin/validate_expected_hash_test.go 中。

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
func (c *captureStore) GetDependencyTransferNotes(string) (map[string]string, error) {
	return nil, nil
}
func (c *captureStore) SetTransferNote(string, string) error { return nil }
func (c *captureStore) RecordLastResponse(string, string) error                    { return nil }
func (c *captureStore) ScanAll() ([]*model.Task, error)                            { return nil, nil }
func (c *captureStore) AppendToolCall(string, store.ToolCallRecord) error          { return nil }
func (c *captureStore) AppendSchedulerBatch(string, string) error                  { return nil }
func (c *captureStore) ClearSchedulerBatch(string) error                           { return nil }
func (c *captureStore) QueryToolCalls(string, string) ([]store.ToolCallRecord, error) {
	return nil, nil
}

// --- §8.3 claimOrWait 排队路径单测 ---

func TestWriteFile_WaitAndRetrySuccess(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	rr.occupied = true
	rr.occupiedBy = "worker-2"
	rr.claimAfterWait = true // 排队唤醒后第二次 TryClaim 放行
	g.WaitTimeoutSec = 10

	path := filepath.Join(tmp, "wait-ok.txt")
	out, err := callWriteFile(g, path, "waited-content", "")
	if err != nil {
		t.Fatalf("expected success after wait, got %v", err)
	}
	if !strings.Contains(out, "文件已写入") {
		t.Fatalf("unexpected output: %s", out)
	}

	events := rr.snapshot()
	// 应当是: TryClaim(fail) → WaitForRelease → TryClaim(success) → Release
	expected := []string{"TryClaim:", "WaitForRelease:", "TryClaim:", "Release:"}
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d: %v", len(events), events)
	}
	for i, prefix := range expected {
		if !strings.HasPrefix(events[i], prefix) {
			t.Errorf("event[%d] = %q, want prefix %q", i, events[i], prefix)
		}
	}
}

func TestWriteFile_WaitTimeoutFallback(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	rr.occupied = true
	rr.occupiedBy = "worker-2"
	rr.waitErr = context.DeadlineExceeded
	g.WaitTimeoutSec = 10

	path := filepath.Join(tmp, "wait-timeout.txt")
	_, err := callWriteFile(g, path, "x", "")
	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Fatalf("expected 占用 in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("expected 超时 in error, got %v", err)
	}
}

func TestWriteFile_WaitDisabledWhenZero(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	rr.occupied = true
	rr.occupiedBy = "worker-2"
	g.WaitTimeoutSec = 0 // 不排队

	path := filepath.Join(tmp, "no-wait.txt")
	_, err := callWriteFile(g, path, "x", "")
	if err == nil {
		t.Fatal("expected error when wait disabled")
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Fatalf("expected 占用 in error, got %v", err)
	}

	// 确认没有调用 WaitForRelease
	events := rr.snapshot()
	for _, ev := range events {
		if strings.HasPrefix(ev, "WaitForRelease:") {
			t.Fatalf("WaitForRelease should not be called when WaitTimeoutSec=0, got %v", events)
		}
	}
}

func TestEditFile_WaitAndRetrySuccess(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)
	rr.occupied = true
	rr.occupiedBy = "worker-2"
	rr.claimAfterWait = true
	g.WaitTimeoutSec = 10

	path := filepath.Join(tmp, "wait-edit.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := callEditFile(g, path, "world", "Go", "")
	if err != nil {
		t.Fatalf("expected success after wait, got %v", err)
	}
	if !strings.Contains(out, "文件已编辑") {
		t.Fatalf("unexpected output: %s", out)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "hello Go" {
		t.Fatalf("expected 'hello Go', got %q", string(data))
	}
}

// C5 删除：TestWriteFile_RecordsArtifact / TestWriteFile_ArtifactDedupOnRewrite /
// TestNormalizeArtifactPath 三个测试已经删除。
//
// 前两个测试覆盖的是 LocalWriteGroup.recordArtifact 的内联实现，C5 把这套逻辑
// 整体迁移到 internal/hook/builtin/record_artifact.go 后，对应的等价测试已在
// internal/hook/builtin/record_artifact_test.go 中重建。
//
// TestNormalizeArtifactPath 一并删除，因为 normalizeArtifactPath 函数也随
// recordArtifact 一起迁移到了 hook/builtin 包，tools 包内不再持有该实现。
// 该函数的等价测试也在新的 record_artifact_test.go 中。


// === §7 Hashline 行哈希增强测试 ===

func TestEditFile_StripHashPrefix(t *testing.T) {
	g, _, tmpDir := newWriteGroup(t, nil)
	fp := filepath.Join(tmpDir, "foo.go")
	orig := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(fp, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// old_str 粘回带 hashline 前缀的整行，应仍能匹配
	oldStrWithHash := "1#VK|package main"
	out, err := callEditFile(g, fp, oldStrWithHash, "package hashline", "")
	if err != nil {
		t.Fatalf("edit_file 失败: %v", err)
	}
	if !strings.Contains(out, "已编辑") {
		t.Errorf("输出应包含'已编辑': %q", out)
	}

	// 验证文件确实被改了
	data, _ := os.ReadFile(fp)
	if !strings.Contains(string(data), "package hashline") {
		t.Errorf("文件内容未被修改: %q", string(data))
	}
}

func TestEditFile_StripHashPrefix_MultiLine(t *testing.T) {
	g, _, tmpDir := newWriteGroup(t, nil)
	fp := filepath.Join(tmpDir, "bar.go")
	orig := "func a() {}\nfunc b() {}\n"
	if err := os.WriteFile(fp, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// 多行 old_str，全部带 hashline 前缀
	oldStr := "1#VK|func a() {}\n2#QZ|func b() {}"
	out, err := callEditFile(g, fp, oldStr, "func combined() {}", "")
	if err != nil {
		t.Fatalf("edit_file 失败: %v", err)
	}
	if !strings.Contains(out, "已编辑") {
		t.Errorf("输出应包含'已编辑': %q", out)
	}
}

// TestEditFile_StripHashPrefix_NewStr 是 §7 P1 修复（2026-04-26）的回归护栏：
// new_str 同样需要 StripHashPrefix。
//
// 修复前的失败模式：LLM 把 read_file 输出里整段 hashline 行复制进 new_str（典型误用），
// 工具不剥前缀，结果把 "12#VK|content" 这种字面字符串写入文件——产物被前缀污染。
//
// 评审历史：本 case 在最初评审时被标 P1 必修；同 commit 修复 + 加测试。
// 负向自检：临时撤回 local_write.go 中的 newStr StripHashPrefix 一行，本测试应红。
func TestEditFile_StripHashPrefix_NewStr(t *testing.T) {
	g, _, tmpDir := newWriteGroup(t, nil)
	fp := filepath.Join(tmpDir, "newstr.go")
	orig := "package main\nfunc old() {}\nfunc keep() {}\n"
	if err := os.WriteFile(fp, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// new_str 整段都是带 hashline 前缀的多行——模拟 LLM 直接把 read_file 输出粘进来
	// 阈值 50% 起作用：3 行有 2 行带前缀（含 1 个空行）→ 命中 strip 路径
	newStrWithHash := "10#VK|func renamed() {\n11#QZ|    return 42\n12#NB|}"

	out, err := callEditFile(g, fp, "func old() {}", newStrWithHash, "")
	if err != nil {
		t.Fatalf("edit_file 失败: %v", err)
	}
	if !strings.Contains(out, "已编辑") {
		t.Errorf("输出应包含'已编辑': %q", out)
	}

	// 关键断言：文件内容不应含任何 hashline 前缀字面（"12#VK|" / "10#VK|" 等）
	data, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	got := string(data)
	for _, leak := range []string{"10#VK|", "11#QZ|", "12#NB|"} {
		if strings.Contains(got, leak) {
			t.Errorf("new_str 的 hashline 前缀 %q 泄漏到文件内容中——StripHashPrefix 未对 new_str 生效:\n%s",
				leak, got)
		}
	}
	// 正向断言：剥前缀后的纯内容应当被写入
	if !strings.Contains(got, "func renamed()") {
		t.Errorf("剥前缀后的纯内容应写入文件，实际:\n%s", got)
	}
	if !strings.Contains(got, "return 42") {
		t.Errorf("剥前缀后的纯内容应写入文件，实际:\n%s", got)
	}
}

// TestEditFile_AcceptsLineAnchorsArg 验证 §7 schema 改动：
// edit_file 接受 line_anchors []string 参数而不报"未知参数"。
// 注意：本测试只验证 schema/参数透传，不验证哈希校验逻辑——后者在
// ValidateLineAnchorsHook 里，并由 internal/hook/builtin 的测试覆盖。
func TestEditFile_AcceptsLineAnchorsArg(t *testing.T) {
	g, _, tmpDir := newWriteGroup(t, nil)
	fp := filepath.Join(tmpDir, "anchored.go")
	if err := os.WriteFile(fp, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args := map[string]any{
		"path":         fp,
		"old_str":      "hello",
		"new_str":      "hi",
		"line_anchors": []any{"1#VK"}, // 锚点不在工具层校验，只由 hook 校验
	}
	out, err := g.editFile(context.Background(), args)
	if err != nil {
		t.Fatalf("edit_file 不应因 line_anchors 参数报错: %v", err)
	}
	if !strings.Contains(out, "已编辑") {
		t.Errorf("输出应包含'已编辑': %q", out)
	}
}

// TestReadEditRoundTrip_LineAnchors 是 §7 端到端往返测试：
// read_file（HashlineEnabled=true）的输出格式必须能被 ParseLineRef 直接消费，
// 且重新计算的哈希与 read_file 嵌入的一致——即 read_file 与 ValidateLineAnchorsHook
// 之间没有"格式漂移"。
//
// 不测 ValidateLineAnchorsHook 本身（其单元测试已覆盖），而是测两侧的"形状契约"——
// 这是把"装配漏接"提前到 build 时发现的护栏。
func TestReadEditRoundTrip_LineAnchors(t *testing.T) {
	tmp := t.TempDir()

	fp := filepath.Join(tmp, "roundtrip.go")
	content := "package main\n\nfunc main() {\n\treturn\n}\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. read_file with HashlineEnabled
	rg := LocalReadGroup{
		Workdir:         &DefaultWorkdir{ProjectRoot: tmp},
		HashlineEnabled: true,
	}
	out, err := rg.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}

	// 2. 输出体内必须包含每一行的 hashline 前缀（"N#HH|content"）
	// 不依赖 header 格式，仅扫描内容行
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if line == "" {
			continue // 跳过空行——尾部空行的哈希前缀仍存在但内容为空
		}
		// 期望 read_file 输出含 "N#HH|line" 子串，其中 HH = ComputeLineHash(N, line)
		// 由于 hash 是 2 字符的字典字符，这里宽松匹配前缀格式
		expectedAnchor := hashline.FormatHashLine(i+1, line)
		if !strings.Contains(out, expectedAnchor) {
			t.Errorf("read_file 输出缺第 %d 行的 hashline %q;\n实际输出:\n%s",
				i+1, expectedAnchor, out)
		}
	}

	// 3. 取第 1 行的 hashline 作为 anchor 投给 ValidateLineAnchorsHook 验证
	// （绕过 hook 直接用 ParseLineRef + ComputeLineHash 验证形状契约）
	firstAnchor := hashline.FormatHashLine(1, "package main")
	ref, err := hashline.ParseLineRef(firstAnchor)
	if err != nil {
		t.Fatalf("ParseLineRef(%q): %v", firstAnchor, err)
	}
	if ref.Line != 1 {
		t.Errorf("Line = %d, want 1", ref.Line)
	}
	// 重算应一致——这是 §7 的核心契约
	recomputed := hashline.ComputeLineHash(1, "package main")
	if ref.Hash != recomputed {
		t.Errorf("Hash mismatch: anchor=%q recomputed=%q", ref.Hash, recomputed)
	}
}
