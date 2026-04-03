package explorer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// mockLLMClient 用于测试的 LLM mock。
type mockLLMClient struct {
	responses []llm.Response
	callIndex int
}

func (m *mockLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}

func setup() (store.TaskStore, roster.Roster, chan model.Event) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	return s, r, ch
}

func TestExplorer_OnlyClaimsExploreEvents(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()

	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "调查结果"}},
	}

	// 发布一个 explore 任务和一个 code 任务
	exploreTask := &model.Task{Description: "调查文件结构", EventType: "explore"}
	s.PublishTask(exploreTask)
	codeTask := &model.Task{Description: "写代码", EventType: "code"}
	s.PublishTask(codeTask)

	exp := New(s, r, mock, cfg, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 5 // 测试中启用空闲退出

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	exp.Run(ctx)

	// explore 任务应被完成
	got, _ := s.GetTask(exploreTask.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("explore task status = %s, want completed", got.Status)
	}

	// code 任务应仍然 pending
	got2, _ := s.GetTask(codeTask.ID)
	if got2.Status != model.TaskStatusPending {
		t.Errorf("code task status = %s, want pending", got2.Status)
	}
}

func TestExplorer_UsesReadOnlyTools(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()

	// LLM 返回一个 read_file 工具调用，然后完成
	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: map[string]string{"path": "nonexistent.txt"}},
				},
			},
			{Content: "文件不存在"},
		},
	}

	task := &model.Task{Description: "检查文件", EventType: "explore"}
	s.PublishTask(task)

	exp := New(s, r, mock, cfg, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 3

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	exp.Run(ctx)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("task status = %s, want completed", got.Status)
	}
}

func TestExplorer_ContextCancellation(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()
	mock := &mockLLMClient{}

	exp := New(s, r, mock, cfg, nil)
	exp.agent.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		exp.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("explorer did not stop after context cancellation")
	}
}

// 工具函数单元测试

func TestToolReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	content, err := toolReadFile(context.Background(), map[string]string{"path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "hello world" {
		t.Errorf("content = %q, want %q", content, "hello world")
	}
}

func TestToolReadFile_NotFound(t *testing.T) {
	_, err := toolReadFile(context.Background(), map[string]string{"path": "/nonexistent/file.txt"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestToolReadFile_MissingArg(t *testing.T) {
	_, err := toolReadFile(context.Background(), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestToolListFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(""), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	result, err := toolListFiles(context.Background(), map[string]string{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "a.txt") {
		t.Errorf("result should contain 'a.txt': %s", result)
	}
	if !strings.Contains(result, "subdir/") {
		t.Errorf("result should contain 'subdir/': %s", result)
	}
}

func TestToolGrepSearch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "code.go"), []byte("func main() {\n\tfmt.Println(\"hello\")\n}"), 0644)

	result, err := toolGrepSearch(context.Background(), map[string]string{
		"pattern": "Println",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Println") {
		t.Errorf("result should contain match: %s", result)
	}
}

func TestToolGrepSearch_NoMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "empty.txt"), []byte("nothing here"), 0644)

	result, err := toolGrepSearch(context.Background(), map[string]string{
		"pattern": "nonexistent_pattern_xyz",
		"path":    dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "未找到匹配项" {
		t.Errorf("result = %q, want %q", result, "未找到匹配项")
	}
}

// 验证 registerReadOnlyTools 注册了预期的工具
func TestRegisterReadOnlyTools(t *testing.T) {
	tools := agent.NewToolRegistry()
	registerReadOnlyTools(tools)

	defs := tools.Defs()
	if len(defs) != 3 {
		t.Fatalf("tool count = %d, want 3", len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, expected := range []string{"read_file", "list_files", "grep_search"} {
		if !names[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}
