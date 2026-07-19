package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/ui"
)

// suggestMaxRows 是提示框最多展示的命令条数；超出部分折叠为一行
// "…还有 N 条"，防止 "/" 刚输入时（全目录）提示框吃掉主区域。
const suggestMaxRows = 6

// matchCommands 返回输入框内容应提示的命令列表：
//   - 不以 "/" 开头 → nil（普通聊天输入，不提示）；
//   - "/" + 前缀（无空格）→ 目录中前缀匹配的命令（"/" 本身 = 全目录）；
//   - 已输入空格（进入参数阶段）→ 若命令名精确命中（含别名），持续展示
//     该命令的用法提示，否则 nil。
func matchCommands(input string) []ui.CommandSpec {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	if i := strings.IndexByte(input, ' '); i >= 0 {
		if c, ok := ui.MatchCommand(input[1:i]); ok {
			return []ui.CommandSpec{c}
		}
		return nil
	}
	return ui.PrefixCommands(input[1:])
}

// visibleMatches 应用 suggestMaxRows 截断，返回可见条目与折叠条数。
func visibleMatches(input string) (rows []ui.CommandSpec, hidden int) {
	matches := matchCommands(input)
	if len(matches) > suggestMaxRows {
		return matches[:suggestMaxRows], len(matches) - suggestMaxRows
	}
	return matches, 0
}

// suggestLineCount 返回提示框渲染后的行数（0 = 不显示提示框）。
// reflowInputLayoutFrom 用它给输入区预留高度，必须与 renderSuggestBox
// 的实际行数一致。
func suggestLineCount(input string) int {
	rows, hidden := visibleMatches(input)
	n := len(rows)
	if hidden > 0 {
		n++
	}
	return n
}

// renderSuggestBox 渲染输入区上方的斜杠命令提示框（无匹配返回空串）。
func renderSuggestBox(t Theme, input string) string {
	rows, hidden := visibleMatches(input)
	if len(rows) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rows)+1)
	for _, c := range rows {
		lines = append(lines, "  "+t.StatusKey.Render(c.Usage())+
			t.StatusVal.Render("  "+c.Desc))
	}
	if hidden > 0 {
		lines = append(lines, t.StatusVal.Render(
			fmt.Sprintf("  …还有 %d 条，继续输入以筛选", hidden)))
	}
	return strings.Join(lines, "\n")
}
