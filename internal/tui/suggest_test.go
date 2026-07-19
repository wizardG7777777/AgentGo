package tui

import (
	"strings"
	"testing"

	"agentgo/internal/ui"

	tea "github.com/charmbracelet/bubbletea"
)

// matchCommands 是 "/" 输入提示框的数据源：提示集必须与命令目录
// （两个前端的单一数据源）保持一致。

func TestMatchCommands_NonSlashInput(t *testing.T) {
	if got := matchCommands("帮我查一下日志"); got != nil {
		t.Fatalf("非 / 输入不应提示命令，得到 %+v", got)
	}
}

func TestMatchCommands_BareSlashListsCatalog(t *testing.T) {
	got := matchCommands("/")
	if len(got) != len(ui.CommandCatalog()) {
		t.Fatalf("\"/\" 应列出整个目录（%d 条），得到 %d 条", len(ui.CommandCatalog()), len(got))
	}
}

func TestMatchCommands_PrefixFilters(t *testing.T) {
	got := matchCommands("/ca")
	if len(got) != 1 || got[0].Name != "cancel" {
		t.Fatalf("\"/ca\" 应只提示 cancel，得到 %+v", got)
	}
}

func TestMatchCommands_UnknownPrefixEmpty(t *testing.T) {
	// 用户曾被占位符误导输入 "/command"——它不是有效命令，必须没有提示。
	if got := matchCommands("/command"); len(got) != 0 {
		t.Fatalf("\"/command\" 不是有效命令前缀，不应有提示，得到 %+v", got)
	}
}

func TestMatchCommands_AliasPrefix(t *testing.T) {
	got := matchCommands("/dash")
	if len(got) != 1 || got[0].Name != "dashboard" {
		t.Fatalf("\"/dash\" 应命中 dashboard，得到 %+v", got)
	}
}

func TestMatchCommands_ArgPhaseKeepsUsageHint(t *testing.T) {
	got := matchCommands("/cancel abc123")
	if len(got) != 1 || got[0].Name != "cancel" {
		t.Fatalf("参数阶段应持续展示 cancel 的用法提示，得到 %+v", got)
	}
	if got := matchCommands("/xyz abc"); len(got) != 0 {
		t.Fatalf("未知命令的参数阶段不应提示，得到 %+v", got)
	}
}

// 提示框高度（suggestLineCount）必须与渲染行数一致，否则布局会
// 抖动或截断输入区。

func TestSuggestLineCount_MatchesRenderedBox(t *testing.T) {
	for _, input := range []string{"", "/", "/ca", "/cancel abc", "/xyz"} {
		want := 0
		if box := renderSuggestBox(DefaultTheme(), input); box != "" {
			want = len(strings.Split(box, "\n"))
		}
		if got := suggestLineCount(input); got != want {
			t.Fatalf("suggestLineCount(%q) = %d, 渲染行数 = %d", input, got, want)
		}
	}
}

func TestSuggestBox_CapsRowsWithOverflowHint(t *testing.T) {
	box := renderSuggestBox(DefaultTheme(), "/")
	lines := strings.Split(box, "\n")
	if len(lines) != suggestMaxRows+1 {
		t.Fatalf("全目录提示应截断为 %d 行 + 1 行折叠提示，得到 %d 行", suggestMaxRows, len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "继续输入以筛选") {
		t.Fatalf("末行应为折叠提示: %q", lines[len(lines)-1])
	}
}

// helpText 现在由命令目录生成：目录里的每条命令都必须出现在帮助里。

func TestHelpText_CoversWholeCatalog(t *testing.T) {
	for _, c := range ui.CommandCatalog() {
		if !strings.Contains(helpText, c.Usage()) {
			t.Fatalf("helpText 缺少命令 %s", c.Usage())
		}
	}
	if !strings.Contains(helpText, "Hotkeys") {
		t.Fatal("helpText 应保留快捷键说明段")
	}
}

// 命令反馈（错误提示、/mode、/cancel 等）写在消息流里；默认视图是
// 仪表板（不渲染消息流），执行命令后若视图未变必须切到消息视图，
// 否则反馈不可见——用户曾因此误以为命令系统整体失效。

func enterKey(t *testing.T, m AppModel, line string) AppModel {
	t.Helper()
	m.input.SetValue(line)
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return result.(AppModel)
}

func TestCommand_UnknownCommandSwitchesToChatView(t *testing.T) {
	m := newAppModel(testDeps())
	m.view = ViewDashboard

	m = enterKey(t, m, "/command")

	if m.view != ViewChat {
		t.Fatalf("未知命令反馈后视图 = %d, want ViewChat（反馈需可见）", m.view)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "未知命令") {
		t.Fatalf("消息流应包含未知命令提示: %q", got)
	}
}

func TestCommand_ModeFeedbackSwitchesToChatView(t *testing.T) {
	m := newAppModel(testDeps())
	m.view = ViewDashboard

	m = enterKey(t, m, "/mode")

	if m.view != ViewChat {
		t.Fatalf("/mode 反馈后视图 = %d, want ViewChat", m.view)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "模式") {
		t.Fatalf("消息流应包含模式切换反馈: %q", got)
	}
}

func TestCommand_ViewCommandKeepsOwnView(t *testing.T) {
	m := newAppModel(testDeps())
	m.view = ViewChat

	m = enterKey(t, m, "/dashboard")

	if m.view != ViewDashboard {
		t.Fatalf("/dashboard 后视图 = %d, want ViewDashboard（视图命令不受回退影响）", m.view)
	}
}

func TestCommand_HelpShowsGeneratedHelpInChatView(t *testing.T) {
	m := newAppModel(testDeps())

	m = enterKey(t, m, "/help")

	if m.view != ViewChat {
		t.Fatalf("/help 后视图 = %d, want ViewChat", m.view)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "/cancel <task-id>") {
		t.Fatalf("/help 输出应来自命令目录: %q", got)
	}
}

// 占位符曾写着 "/command"——一个根本不存在的命令，直接误导用户。
func TestPlaceholder_NoLongerAdvertisesCommandSlashCommand(t *testing.T) {
	m := newAppModel(testDeps())
	if strings.Contains(m.input.Placeholder, "/command") {
		t.Fatalf("占位符仍在宣传不存在的 /command: %q", m.input.Placeholder)
	}
}
