package tui

import (
	"strings"
	"testing"

	"agentgo/internal/interaction"
	"agentgo/internal/ui"
)

func TestRenderInteractionPanel_OptionsAndHints(t *testing.T) {
	req := testInteraction("r-1",
		ui.InteractionOption{ID: "safe", Label: "安全方案", Description: "先验证再执行"},
		ui.InteractionOption{ID: "custom", Label: "自定义", RequiresText: true},
	)
	req.SubjectKind = "plan"
	req.SubjectID = "plan-1"

	view := renderInteractionPanel(DefaultTheme(), 100, req, 1, 0, 2, true)
	for _, want := range []string{
		"需要用户选择", "+2 queued", "scheduler_question", "请选择下一步",
		"安全方案", "先验证再执行", "自定义", "需要补充文本",
		"↑/↓ 选择", "Enter 提交", "Esc 返回输入框",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("panel missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "[1]") || strings.Contains(view, "[2]") {
		t.Fatal("panel must not advertise single-number shortcuts")
	}
}

func TestRenderInteractionPanel_FreeText(t *testing.T) {
	req := testInteraction("r-text")
	req.Kind = string(interaction.KindText)
	req.AllowFreeText = true
	view := renderInteractionPanel(DefaultTheme(), 80, req, 0, 0, 0, false)
	if !strings.Contains(view, "自定义回答") {
		t.Fatalf("free-text choice missing:\n%s", view)
	}
}

func TestRenderInteractionPanel_LongPromptCanPageWithoutLosingOptions(t *testing.T) {
	req := testInteraction("r-long",
		ui.InteractionOption{ID: "execute", Label: "执行"})
	req.Prompt = strings.Join([]string{
		"问题第1行", "问题第2行", "问题第3行", "问题第4行", "问题第5行",
		"问题第6行", "问题第7行", "问题第8行", "问题第9行",
	}, "\n")

	first := renderInteractionPanel(DefaultTheme(), 80, req, 0, 0, 0, true)
	if !strings.Contains(first, "问题第1行") || strings.Contains(first, "问题第9行") ||
		!strings.Contains(first, "PgUp/PgDn 翻页") || !strings.Contains(first, "执行") {
		t.Fatalf("first prompt page is incomplete:\n%s", first)
	}
	last := renderInteractionPanel(DefaultTheme(), 80, req, 0, 99, 0, true)
	if strings.Contains(last, "问题第1行") || !strings.Contains(last, "问题第9行") ||
		!strings.Contains(last, "执行") {
		t.Fatalf("last prompt page is incomplete:\n%s", last)
	}
}

func TestRenderInteractionPanel_StripsTerminalControlSequences(t *testing.T) {
	req := testInteraction("r-control", ui.InteractionOption{
		ID:          "safe",
		Label:       "\x1b[31m红色\x1b[0m",
		Description: "保留\x00文本",
	})
	req.AgentID = "worker\x1b]52;c;Y2xpcGJvYXJk\x07-1"
	req.Prompt = "问题\x1b[2J不得清屏\n第二行\x9b31m"

	view := renderInteractionPanel(DefaultTheme(), 100, req, 0, 0, 0, true)
	for _, forbidden := range []string{"\x1b[2J", "\x1b]52", "\x00", "\x07", "\x9b31m"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("渲染结果仍含输入的终端控制序列 %q: %q", forbidden, view)
		}
	}
	for _, want := range []string{"worker-1", "问题不得清屏", "第二行", "红色", "保留文本"} {
		if !strings.Contains(view, want) {
			t.Errorf("净化后丢失可见文本 %q:\n%s", want, view)
		}
	}
}

func TestSanitizeTerminalText_PreservesNewlineAndTab(t *testing.T) {
	got := sanitizeTerminalText("a\r\nb\tc\x01d")
	if got != "a\nb\tcd" {
		t.Fatalf("sanitizeTerminalText = %q", got)
	}
}

// 底栏提示必须跟随焦点：未聚焦（◇）时 ↑/↓ 归输入框，面板若不提示
// 先 Tab 聚焦，用户对着它按方向键会毫无反应（本仓库实收过的可用性 bug）。
func TestRenderInteractionPanel_FooterFollowsFocus(t *testing.T) {
	req := testInteraction("r-foot", ui.InteractionOption{ID: "ok", Label: "好"})

	focused := renderInteractionPanel(DefaultTheme(), 80, req, 0, 0, 0, true)
	if !strings.Contains(focused, "↑/↓ 选择") || strings.Contains(focused, "按 Tab 聚焦") {
		t.Fatalf("聚焦时应显示操作提示:\n%s", focused)
	}

	unfocused := renderInteractionPanel(DefaultTheme(), 80, req, 0, 0, 0, false)
	if !strings.Contains(unfocused, "按 Tab 聚焦此面板后选择") {
		t.Fatalf("未聚焦时应提示先 Tab 聚焦:\n%s", unfocused)
	}
	if strings.Contains(unfocused, "Enter 提交") {
		t.Fatalf("未聚焦时不应暗示可直接提交:\n%s", unfocused)
	}
}
