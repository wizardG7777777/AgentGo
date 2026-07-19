package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderAgentDetail_Basic(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:               "worker-1",
		Type:             "worker",
		State:            "processing",
		CurrentTaskDesc:  "doing work",
		CallCount:        3,
		PromptTokens:     5000,
		CompletionTokens: 1000,
	}
	result := renderAgentDetail(theme, 80, 20, "worker-1", info, "output line 1\noutput line 2")

	if !strings.Contains(result, "worker-1") {
		t.Error("should show agent ID")
	}
	if !strings.Contains(result, "worker") {
		t.Error("should show agent type")
	}
	if !strings.Contains(result, "processing") {
		t.Error("should show state")
	}
	if !strings.Contains(result, "doing work") {
		t.Error("should show task desc")
	}
	if !strings.Contains(result, "output line") {
		t.Error("should show output content")
	}
}

func TestRenderAgentDetail_NoOutput(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{ID: "w-1", Type: "worker", State: "idle"}
	result := renderAgentDetail(theme, 80, 20, "w-1", info, "")

	if !strings.Contains(result, "No output yet") {
		t.Error("should show empty state message")
	}
}

func TestRenderAgentDetail_TooSmall(t *testing.T) {
	theme := DefaultTheme()
	result := renderAgentDetail(theme, 5, 2, "w-1", nil, "")
	if result != "" {
		t.Error("should return empty for too-small dimensions")
	}
}

// D5：terminating 状态与 dashboard 卡片渲染一致（"⊘ terminating"），
// 不再落入 default 裸文本分支。
func TestRenderAgentDetail_TerminatingState(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{ID: "w-1", Type: "worker", State: "terminating"}
	result := renderAgentDetail(theme, 80, 20, "w-1", info, "some output")

	if !strings.Contains(result, "⊘ terminating") {
		t.Errorf("terminating 状态应渲染为 ⊘ terminating（对齐 dashboard）: %q", result)
	}
}

func TestRenderAgentDetail_NilInfo(t *testing.T) {
	theme := DefaultTheme()
	result := renderAgentDetail(theme, 80, 20, "w-1", nil, "some output")

	if !strings.Contains(result, "w-1") {
		t.Error("should still show agent ID")
	}
	if !strings.Contains(result, "some output") {
		t.Error("should still show output")
	}
}

func TestRenderAgentDetail_States(t *testing.T) {
	theme := DefaultTheme()
	states := []struct {
		state string
		want  string
	}{
		{"processing", "processing"},
		{"waiting_approval", "approval"},
		{"idle", "idle"},
		{"unknown_state", "unknown_state"},
	}
	for _, tc := range states {
		info := &AgentInfo{ID: "w-1", Type: "t", State: tc.state}
		result := renderAgentDetail(theme, 80, 20, "w-1", info, "")
		if !strings.Contains(result, tc.want) {
			t.Errorf("state=%q: should contain %q", tc.state, tc.want)
		}
	}
}

func TestRenderAgentDetail_ActivityHeader(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:            "worker-1",
		Type:          "worker",
		State:         "processing",
		Phase:         "tooling",
		Loop:          4,
		LastTool:      "grep_search",
		ActivityAge:   "2s ago",
		LastModelText: "Searching for call sites",
	}
	result := renderAgentDetail(theme, 100, 20, "worker-1", info, "")

	for _, want := range []string{"phase: tooling", "loop: 4", "tool: grep_search", "active: 2s ago", "Searching for call sites"} {
		if !strings.Contains(result, want) {
			t.Errorf("detail should contain %q", want)
		}
	}
}

func TestRenderAgentDetail_LastErrorFallback(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:        "worker-1",
		Type:      "worker",
		State:     "processing",
		LastError: "exit status 1",
	}
	result := renderAgentDetail(theme, 80, 20, "worker-1", info, "")

	if !strings.Contains(result, "error: exit status 1") {
		t.Error("detail should show last error when output is empty")
	}
}

func TestRenderAgentDetail_WideOutputWraps(t *testing.T) {
	theme := DefaultTheme()
	info := &AgentInfo{
		ID:              "worker-1",
		Type:            "worker",
		State:           "processing",
		CurrentTaskDesc: strings.Repeat("修复🙂", 20),
	}
	result := renderAgentDetail(theme, 42, 16, "worker-1", info, strings.Repeat("输出🙂", 20))

	if !strings.Contains(result, "…") {
		t.Error("wide task desc should be truncated")
	}
	for i, line := range strings.Split(result, "\n") {
		if got := lipgloss.Width(line); got > 42 {
			t.Fatalf("line %d visual width = %d, want <= 42: %q", i, got, line)
		}
	}
}

func TestRenderAgentDetail_LongChineseTaskDoesNotOverflow(t *testing.T) {
	theme := DefaultTheme()
	width := 72
	height := 16
	info := &AgentInfo{
		ID:              "explorer-1",
		Type:            "explorer",
		State:           "processing",
		CurrentTaskDesc: strings.Repeat("结合已经有的调查结果继续分析这个项目的配置文件关系，", 8),
		CallCount:       2,
		PromptTokens:    1200,
		Phase:           "thinking",
		Loop:            3,
		LastTool:        "run_shell",
		ActivityAge:     "1s ago",
	}
	output := strings.Repeat("工具输出里也可能包含很长的中文行，需要避免终端自动换行。", 6)

	result := renderAgentDetail(theme, width, height, "explorer-1", info, output)
	lines := strings.Split(result, "\n")
	if len(lines) > height {
		t.Fatalf("rendered lines = %d, want <= %d", len(lines), height)
	}
	for i, line := range lines {
		if strings.Contains(line, "�") {
			t.Fatalf("line %d contains replacement character: %q", i, line)
		}
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d: %q", i, got, width, line)
		}
	}
}

func TestBuildBoundedInfoLine_TruncatesByDisplayWidth(t *testing.T) {
	width := 40
	line := buildBoundedInfoLine([]string{
		"type: explorer",
		"● processing",
		"task: " + strings.Repeat("中文任务", 12),
	}, width)

	if strings.Contains(line, "�") {
		t.Fatalf("line contains replacement character: %q", line)
	}
	if got := lipgloss.Width(line); got != width {
		t.Fatalf("line width = %d, want %d: %q", got, width, line)
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("line should show truncation marker: %q", line)
	}
}
