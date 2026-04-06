package agent

import (
	"errors"
	"strings"
	"testing"

	"agentgo/internal/llm"
)

// makeEntry 创建一个包含指定工具调用的 HistoryEntry。
func makeEntry(toolName, content string) HistoryEntry {
	return HistoryEntry{
		Output:           "output",
		ToolCalled:       true,
		AssistantContent: "thinking about " + toolName,
		ToolCalls: []llm.ToolCall{
			{ID: "call_" + toolName, Name: toolName, Arguments: map[string]any{"path": "/tmp"}},
		},
		ToolResults: []ToolResult{
			{ToolCallID: "call_" + toolName, Content: content},
		},
	}
}

func TestSnipOldToolResults(t *testing.T) {
	// 5 个 run_shell 调用，keepRecent=3 → 前 2 个应被清空
	history := []HistoryEntry{
		makeEntry("run_shell", "output-1"),
		makeEntry("run_shell", "output-2"),
		makeEntry("run_shell", "output-3"),
		makeEntry("run_shell", "output-4"),
		makeEntry("run_shell", "output-5"),
	}

	snipOldToolResults(history, 3)

	// 前 2 个应被清空
	for i := 0; i < 2; i++ {
		got := history[i].ToolResults[0].Content
		if got != "[已清空，内容过长]" {
			t.Errorf("history[%d] content = %q, want [已清空，内容过长]", i, got)
		}
	}
	// 后 3 个应保持不变
	for i := 2; i < 5; i++ {
		got := history[i].ToolResults[0].Content
		if got == "[已清空，内容过长]" {
			t.Errorf("history[%d] content should NOT be snipped, got %q", i, got)
		}
	}

	// 验证 ToolCallID 仍然保留
	if history[0].ToolResults[0].ToolCallID != "call_run_shell" {
		t.Errorf("ToolCallID was modified, got %q", history[0].ToolResults[0].ToolCallID)
	}
}

func TestSnipOldToolResults_PreservesNonTargetTools(t *testing.T) {
	history := []HistoryEntry{
		makeEntry("write_file", "wrote something"),
		makeEntry("write_file", "wrote more"),
		makeEntry("write_file", "wrote even more"),
		makeEntry("write_file", "wrote again"),
		makeEntry("write_file", "wrote last"),
	}

	snipOldToolResults(history, 3)

	// write_file 不在目标工具列表中，全部应保持原样
	for i, entry := range history {
		if entry.ToolResults[0].Content == "[已清空，内容过长]" {
			t.Errorf("history[%d] write_file content should NOT be snipped", i)
		}
	}
}

func TestBuildHistorySummary(t *testing.T) {
	history := []HistoryEntry{
		makeEntry("read_file", "file content"),
		makeEntry("grep_search", "search results"),
		{
			Output:           "final output",
			ToolCalled:       false,
			AssistantContent: "done thinking",
		},
	}

	summary := buildHistorySummary(history)

	if !strings.Contains(summary, "步骤 1:") {
		t.Error("summary should contain '步骤 1:'")
	}
	if !strings.Contains(summary, "步骤 2:") {
		t.Error("summary should contain '步骤 2:'")
	}
	if !strings.Contains(summary, "[read_file]") {
		t.Error("summary should contain tool name [read_file]")
	}
	if !strings.Contains(summary, "=== 历史摘要 ===") {
		t.Error("summary should contain header")
	}
}

func TestBuildHistorySummary_TruncatesLongContent(t *testing.T) {
	longContent := strings.Repeat("x", 300)
	history := []HistoryEntry{
		{
			Output:           "output",
			ToolCalled:       false,
			AssistantContent: longContent,
		},
	}

	summary := buildHistorySummary(history)

	if strings.Contains(summary, strings.Repeat("x", 250)) {
		t.Error("summary should truncate long assistant content")
	}
	if !strings.Contains(summary, "...") {
		t.Error("summary should contain '...' for truncated content")
	}
}

func TestCompressHistory(t *testing.T) {
	history := make([]HistoryEntry, 10)
	for i := 0; i < 10; i++ {
		history[i] = makeEntry("read_file", "content-"+string(rune('0'+i)))
	}

	result := compressHistory(history, 3)

	if len(result) != 4 {
		t.Fatalf("compressHistory should return 4 entries, got %d", len(result))
	}
	if !strings.Contains(result[0].Output, "=== 历史摘要 ===") {
		t.Error("first entry should be summary")
	}
	if result[0].ToolCalled {
		t.Error("summary entry should have ToolCalled=false")
	}
}

func TestCompressHistory_NoCompressWhenFewEntries(t *testing.T) {
	history := []HistoryEntry{
		makeEntry("read_file", "content-1"),
		makeEntry("read_file", "content-2"),
	}

	result := compressHistory(history, 3)

	if len(result) != 2 {
		t.Fatalf("compressHistory should not compress when len <= keepRecent, got %d entries", len(result))
	}
}

func TestIsContextOverflow(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"contains length", errors.New("finish_reason=length"), true},
		{"contains 截断", errors.New("响应被截断"), true},
		{"contains context", errors.New("context window exceeded"), true},
		{"normal error", errors.New("rate limit exceeded"), false},
		{"auth error", errors.New("invalid api key"), false},
		{"wrapped with length", &ErrRecoverable{Err: errors.New("max_tokens length exceeded")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isContextOverflow(tt.err)
			if got != tt.expected {
				t.Errorf("isContextOverflow(%q) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}
