package tui

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/ui"

	"github.com/charmbracelet/lipgloss"
)

// ── styledMsgLines：单条消息渲染（排放与活动区共用的样式事实源）──

func TestStyledMsgLines_Kinds(t *testing.T) {
	theme := DefaultTheme()
	now := time.Now()
	for _, tc := range []struct {
		kind MsgKind
		text string
	}{
		{MsgLog, "system log entry"},
		{MsgInfo, "info message"},
		{MsgWarn, "warning!"},
		{MsgError, "error occurred"},
	} {
		lines := styledMsgLines(theme, 80, StyledMsg{Text: tc.text, Kind: tc.kind, At: now})
		if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), tc.text) {
			t.Errorf("kind=%d: rendered lines missing %q", tc.kind, tc.text)
		}
	}
}

func TestStyledMsgLines_TimestampAndAgentPrefix(t *testing.T) {
	theme := DefaultTheme()
	now := time.Date(2026, 5, 26, 14, 30, 45, 0, time.UTC)
	lines := styledMsgLines(theme, 80, StyledMsg{
		Text: "agent output", Kind: MsgAgent, At: now, AgentID: "worker-1",
	})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "14:30:45") {
		t.Error("should show timestamp in HH:MM:SS format")
	}
	if !strings.Contains(joined, "[worker-1]") {
		t.Error("should show agent ID prefix")
	}
}

func TestStyledMsgLines_LongLineWrapsWithoutContentLoss(t *testing.T) {
	theme := DefaultTheme()
	longLine := strings.Repeat("x", 200)
	lines := styledMsgLines(theme, 80, StyledMsg{Text: longLine, Kind: MsgInfo, At: time.Now()})
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "…") || strings.Count(joined, "x") != len(longLine) {
		t.Error("long lines should wrap instead of being truncated")
	}
}

func TestStyledMsgLines_WideLineWrapsWithinCellWidth(t *testing.T) {
	theme := DefaultTheme()
	lines := styledMsgLines(theme, 40, StyledMsg{
		Text: strings.Repeat("处理🙂", 20), Kind: MsgInfo, At: time.Now(), AgentID: "worker-1",
	})
	if strings.Contains(strings.Join(lines, "\n"), "…") {
		t.Error("wide lines should wrap instead of being truncated")
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > 40 {
			t.Fatalf("line %d visual width = %d, want <= 40: %q", i, got, line)
		}
	}
}

func TestStyledMsgLines_Reasoning(t *testing.T) {
	theme := DefaultTheme()
	lines := styledMsgLines(theme, 80, StyledMsg{
		Text: "正文", Reasoning: "先想清楚再回答", Kind: MsgAgent, At: time.Now(), AgentID: "worker-1",
	})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Reasoning") || !strings.Contains(joined, "先想清楚再回答") {
		t.Fatalf("reasoning block missing: %q", joined)
	}
}

// ── renderChatActive：Chat 主态活动区（活跃流尾部 + Live Activity）──

func TestRenderChatActive_IdleReturnsEmpty(t *testing.T) {
	theme := DefaultTheme()
	if got := renderChatActive(theme, 80, 20, nil, nil); got != "" {
		t.Fatalf("idle activity area should be empty, got %q", got)
	}
}

func TestRenderChatActive_TooSmall(t *testing.T) {
	theme := DefaultTheme()
	msgs := []StyledMsg{{Text: "streaming", Kind: MsgAgent, At: time.Now(), StreamID: "s1"}}
	if got := renderChatActive(theme, 5, 1, msgs, nil); got != "" {
		t.Error("should return empty for too-small dimensions")
	}
}

func TestRenderChatActive_OnlyStreamsRendered(t *testing.T) {
	theme := DefaultTheme()
	now := time.Now()
	msgs := []StyledMsg{
		// 非流条目（无 StreamID）不应出现在活动区：定稿内容已排放。
		{Text: "finalized message", Kind: MsgInfo, At: now},
		{Text: "streaming chunk", Kind: MsgAgent, At: now, AgentID: "worker-1", StreamID: "s1"},
	}
	result := renderChatActive(theme, 80, 20, msgs, nil)
	if strings.Contains(result, "finalized message") {
		t.Error("non-stream message should not render in activity area")
	}
	if !strings.Contains(result, "streaming chunk") {
		t.Error("active stream should render in activity area")
	}
}

func TestRenderChatActive_HeightConstraint(t *testing.T) {
	theme := DefaultTheme()
	var msgs []StyledMsg
	for i := 0; i < 10; i++ {
		msgs = append(msgs, StyledMsg{
			Text: strings.Repeat("line\n", 20), Kind: MsgAgent, At: time.Now(), StreamID: "s",
		})
	}
	result := renderChatActive(theme, 80, 10, msgs, nil)
	if lines := strings.Split(result, "\n"); len(lines) > 10 {
		t.Errorf("activity area lines = %d, should be ≤ maxH 10", len(lines))
	}
}

func TestRenderChatActive_KeepsTailWhenOverflowing(t *testing.T) {
	theme := DefaultTheme()
	msgs := []StyledMsg{{
		Text: "first\nsecond\nthird\nfourth", Kind: MsgAgent, At: time.Now(), StreamID: "s1",
	}}
	result := renderChatActive(theme, 80, 2, msgs, nil)
	if strings.Contains(result, "first") {
		t.Error("overflowing activity area should drop the head (bottom-aligned)")
	}
	if !strings.Contains(result, "fourth") {
		t.Error("overflowing activity area should keep the tail")
	}
}

func TestRenderChatActive_LiveActivityShown(t *testing.T) {
	theme := DefaultTheme()
	agents := []AgentInfo{{ID: "worker-1", State: "processing", Phase: "streaming"}}
	result := renderChatActive(theme, 80, 20, nil, agents)
	if !strings.Contains(result, "worker-1") {
		t.Error("live activity section should list active agents")
	}
}

// ── renderResultDetail（保留：/result 全屏视图）──

func TestRenderResultDetail_FullResult(t *testing.T) {
	theme := DefaultTheme()
	msg := &StyledMsg{
		Text: "line 1\nline 2\nline 3",
		Kind: MsgResult,
		At:   time.Now(),
	}
	result := renderResultDetail(theme, 80, 12, msg, nil, 0)

	if !strings.Contains(result, "Task Result") {
		t.Error("should show result view title")
	}
	if !strings.Contains(result, "line 1") || !strings.Contains(result, "line 3") {
		t.Error("should show full result content when it fits")
	}
}

func TestRenderResultDetail_ScrollOffset(t *testing.T) {
	theme := DefaultTheme()
	msg := &StyledMsg{
		Text: strings.Join([]string{"line 1", "line 2", "line 3", "line 4", "line 5"}, "\n"),
		Kind: MsgResult,
		At:   time.Now(),
	}
	result := renderResultDetail(theme, 80, 7, msg, nil, 2)

	if strings.Contains(result, "line 1") {
		t.Error("offset result view should skip earlier lines")
	}
	if !strings.Contains(result, "line 3") {
		t.Error("offset result view should show scrolled content")
	}
}

func TestRenderResultDetail_PreservesRawReasoningBeforeFinalAnswer(t *testing.T) {
	theme := DefaultTheme()
	msg := &StyledMsg{Text: "最终回答", Kind: MsgResult, At: time.Now()}
	turns := []ui.AgentTurn{{
		AgentID: "scheduler-1", TaskID: "task-1", Loop: 2, Status: "completed",
		Reasoning: "第一步：检查提交。\n第二步：汇总差异。",
	}}
	result := renderResultDetail(theme, 100, 20, msg, turns, 0)
	for _, want := range []string{"Raw Reasoning · Loop 2", "第一步：检查提交", "第二步：汇总差异", "Final Answer", "最终回答"} {
		if !strings.Contains(result, want) {
			t.Fatalf("result detail missing %q: %q", want, result)
		}
	}
}
