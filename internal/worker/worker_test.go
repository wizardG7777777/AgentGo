package worker

import (
	"strings"
	"testing"
)

// TestSystemPrompt_ContainsToolNames 验证 system prompt 引用了所有核心工具名。
// 这防止 prompt 与工具集脱节（例如工具改名后 prompt 没同步更新）。
func TestSystemPrompt_ContainsToolNames(t *testing.T) {
	for _, toolName := range []string{"run_shell", "edit_file", "glob_search", "publish_task"} {
		if !strings.Contains(systemPrompt, toolName) {
			t.Errorf("systemPrompt should contain tool name %q", toolName)
		}
	}

	// system prompt 应引导 LLM 优先使用 edit_file 而非 write_file
	if !strings.Contains(systemPrompt, "优先") {
		t.Error("systemPrompt should contain edit_file priority guidance (优先)")
	}
	if !strings.Contains(systemPrompt, "edit_file") {
		t.Error("systemPrompt should mention edit_file in priority guidance")
	}
}
