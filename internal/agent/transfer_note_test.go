package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// --- L3 mechanicalTransferNote 纯代码兑底 ---

func TestMechanicalTransferNote_FullFields(t *testing.T) {
	task := &model.Task{
		ID:           "task-x",
		Description:  "重构认证模块",
		Artifacts:    []string{"internal/auth/token.go", "internal/auth/middleware.go"},
		LastResponse: "完成了 token 刷新逻辑的重写",
		RetryReasons: []string{"expected artifact docs/auth.md 未找到"},
	}
	toolHistory := []store.ToolCallRecord{
		{ToolName: "read_file", Args: map[string]any{"path": "internal/auth/token.go"}, Success: true},
		{ToolName: "edit_file", Args: map[string]any{"path": "internal/auth/token.go"}, Success: true},
		{ToolName: "write_file", Args: map[string]any{"path": "docs/auth.md"}, Success: false},
	}
	note := mechanicalTransferNote(task, nil, toolHistory, 3000)

	// 必须包含所有 section 的关键内容。
	// 工具调用保留完整路径（非 basename）——接手者需要知道具体哪个目录下的文件。
	mustContain := []string{
		"<transfer-note level=\"raw\">",
		"任务目标: 重构认证模块",
		"read_file(internal/auth/token.go)",
		"edit_file(internal/auth/token.go)",
		"write_file(docs/auth.md) [失败]",
		"已修改文件:",
		"internal/auth/token.go",
		"最后一轮输出:",
		"完成了 token 刷新逻辑的重写",
		"失败原因: expected artifact docs/auth.md 未找到",
		"</transfer-note>",
	}
	for _, s := range mustContain {
		if !strings.Contains(note, s) {
			t.Errorf("mechanicalTransferNote 应包含 %q，实际输出:\n%s", s, note)
		}
	}
}

func TestMechanicalTransferNote_EmptyTaskIsTolerated(t *testing.T) {
	note := mechanicalTransferNote(nil, nil, nil, 3000)
	// 即使没任何数据，也应当输出空 shell（开闭标签）
	if !strings.Contains(note, "<transfer-note") {
		t.Errorf("空任务应输出 XML shell，实际: %s", note)
	}
}

func TestMechanicalTransferNote_TokenBudgetTruncation(t *testing.T) {
	// 构造一个超长的 LastResponse，确保触发截断
	longText := strings.Repeat("这是一段很长的文本。", 2000) // ~20000 runes
	task := &model.Task{
		ID:           "task-long",
		Description:  "测试",
		LastResponse: longText,
	}
	note := mechanicalTransferNote(task, nil, nil, 100) // 100 tokens = 200 runes
	// 输出应在 200 runes 左右（加上 truncated 标记）
	runes := []rune(note)
	if len(runes) > 250 {
		t.Errorf("mechanicalTransferNote 预算 100 tokens 应 ≤ 250 runes，实际 %d", len(runes))
	}
	if !strings.Contains(note, "truncated") {
		t.Errorf("截断后应含 truncated 标记，实际:\n%s", note)
	}
}

func TestMechanicalTransferNote_ToolHistoryWindow(t *testing.T) {
	// 构造 30 条工具调用，mechanical 应只保留最近 20 条
	var toolHistory []store.ToolCallRecord
	for i := 0; i < 30; i++ {
		toolHistory = append(toolHistory, store.ToolCallRecord{
			ToolName: "read_file",
			Args:     map[string]any{"path": "file" + string(rune('A'+i)) + ".go"},
			Success:  true,
		})
	}
	task := &model.Task{ID: "task", Description: "测试"}
	note := mechanicalTransferNote(task, nil, toolHistory, 3000)

	// 应包含第 10 条（index 10，字母 K），不包含第 0 条（字母 A）
	// 最近 20 条是 index 10..29
	if strings.Contains(note, "fileA.go") {
		t.Errorf("超出 window 的第 0 条不应出现，实际:\n%s", note)
	}
	if !strings.Contains(note, "fileK.go") {
		t.Errorf("window 内第 10 条 fileK 应出现，实际:\n%s", note)
	}
}

// --- L1 generateTransferNote 走 Execute，需要 mock executor ---

// fakeExecutor 返回预设的 ExecuteResult。calls 记录被调用次数，
// 用来验证 L1 确实触发了最后一次 Execute。
type fakeExecutor struct {
	result ExecuteResult
	err    error
	calls  int
}

func (f *fakeExecutor) exec(_ context.Context, _ *model.Task, _ map[string]string, _ []HistoryEntry) (ExecuteResult, error) {
	f.calls++
	return f.result, f.err
}

func TestGenerateTransferNote_L1_Success(t *testing.T) {
	fe := &fakeExecutor{
		result: ExecuteResult{
			Output:           "这是 L1 压缩后的交接备忘文本",
			AssistantContent: "这是 L1 压缩后的交接备忘文本",
			ToolCalled:       false, // 纯文本响应 = L1 成功
		},
	}
	a := &Agent{
		ID:      "test-agent",
		Execute: fe.exec,
	}
	note := a.generateTransferNote(context.Background(), &model.Task{ID: "t1"}, nil, nil, 3000)
	if fe.calls != 1 {
		t.Errorf("L1 应调用 Execute 1 次，实际 %d", fe.calls)
	}
	if note != "这是 L1 压缩后的交接备忘文本" {
		t.Errorf("L1 返回文本不正确: %q", note)
	}
}

func TestGenerateTransferNote_L1_FailedOnLLMError(t *testing.T) {
	fe := &fakeExecutor{
		err: context.DeadlineExceeded,
	}
	a := &Agent{
		ID:      "test-agent",
		Execute: fe.exec,
	}
	note := a.generateTransferNote(context.Background(), &model.Task{ID: "t1"}, nil, nil, 3000)
	if note != "" {
		t.Errorf("L1 失败应返回空串，实际 %q", note)
	}
}

func TestGenerateTransferNote_L1_FailedOnToolCalled(t *testing.T) {
	// 如果 LLM 反而又调用了工具（没理解 <transfer-request> 指令），视为 L1 失败
	fe := &fakeExecutor{
		result: ExecuteResult{
			ToolCalled: true,
			ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "read_file"}},
		},
	}
	a := &Agent{
		ID:      "test-agent",
		Execute: fe.exec,
	}
	note := a.generateTransferNote(context.Background(), &model.Task{ID: "t1"}, nil, nil, 3000)
	if note != "" {
		t.Errorf("LLM 调用工具时应视为 L1 失败，实际 %q", note)
	}
}

// --- buildTransferNote 两级链：L1 失败时自动降级 L3 ---

func TestBuildTransferNote_L1ToL3Fallback(t *testing.T) {
	// L1 会失败（LLM 出错），L3 应接管
	fe := &fakeExecutor{err: context.DeadlineExceeded}

	// 构造一个带 store 的 Agent 让 L3 能读到工具历史
	eventCh := make(chan model.Event, 8)
	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	task := &model.Task{Description: "测试任务"}
	_ = s.PublishTask(task)
	_ = s.AppendToolCall(task.ID, store.ToolCallRecord{
		ToolName: "read_file",
		Args:     map[string]any{"path": "config.go"},
		Success:  true,
	})

	a := &Agent{
		ID:      "test-agent",
		Execute: fe.exec,
		Store:   s,
	}
	note := a.buildTransferNote(context.Background(), task, nil, nil, 3000)

	// L1 应该被调用
	if fe.calls != 1 {
		t.Errorf("L1 应调用 Execute 1 次，实际 %d", fe.calls)
	}
	// 结果应当是 L3 机械格式（含 XML shell + 任务目标 + 工具历史）
	if !strings.Contains(note, "<transfer-note level=\"raw\">") {
		t.Errorf("L1 失败后应降级 L3 机械格式，实际:\n%s", note)
	}
	if !strings.Contains(note, "任务目标: 测试任务") {
		t.Errorf("L3 应包含任务目标，实际:\n%s", note)
	}
	if !strings.Contains(note, "read_file(config.go)") {
		t.Errorf("L3 应包含工具历史，实际:\n%s", note)
	}
}

func TestBuildTransferNote_L1SuccessPathSkipsL3(t *testing.T) {
	fe := &fakeExecutor{
		result: ExecuteResult{
			AssistantContent: "L1 生成的精炼备忘",
			ToolCalled:       false,
		},
	}
	a := &Agent{
		ID:      "test-agent",
		Execute: fe.exec,
	}
	note := a.buildTransferNote(context.Background(), &model.Task{Description: "x"}, nil, nil, 3000)
	// L1 成功时应直接返回 LLM 文本，不走 L3
	if note != "L1 生成的精炼备忘" {
		t.Errorf("L1 成功应直接返回 LLM 文本，实际 %q", note)
	}
	// 不应包含 L3 的 XML 标签
	if strings.Contains(note, "<transfer-note") {
		t.Errorf("L1 成功时不应降级 L3，实际包含了 L3 标签:\n%s", note)
	}
}

// --- TransferNoteMaxTokens default ---

func TestAgent_TransferNoteMaxTokens_DefaultIs3000(t *testing.T) {
	a := &Agent{}
	if a.transferNoteMaxTokens() != 3000 {
		t.Errorf("未设置时应返回默认 3000，实际 %d", a.transferNoteMaxTokens())
	}
	a.TransferNoteMaxTokens = 1500
	if a.transferNoteMaxTokens() != 1500 {
		t.Errorf("自定义值应被使用，实际 %d", a.transferNoteMaxTokens())
	}
	a.TransferNoteMaxTokens = -1 // 非法值也退回默认
	if a.transferNoteMaxTokens() != 3000 {
		t.Errorf("负数应退回默认 3000，实际 %d", a.transferNoteMaxTokens())
	}
}

// --- truncateToTokenBudget 辅助函数 ---

func TestTruncateToTokenBudget(t *testing.T) {
	// 短文本不截断
	short := "hello world"
	if got := truncateToTokenBudget(short, 1000); got != short {
		t.Errorf("短文本不应截断，got %q", got)
	}
	// 长文本被截断
	long := strings.Repeat("a", 1000)
	got := truncateToTokenBudget(long, 100) // 100 tokens = 200 runes
	if !strings.Contains(got, "truncated") {
		t.Errorf("长文本应被截断并标记，got %q", got[:50]+"...")
	}
	// maxTokens=0 视为无限
	if got := truncateToTokenBudget(long, 0); got != long {
		t.Errorf("maxTokens=0 应不截断")
	}
}
