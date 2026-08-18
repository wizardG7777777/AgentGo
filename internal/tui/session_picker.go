package tui

import (
	"fmt"
	"strings"

	"agentgo/internal/ui"

	tea "github.com/charmbracelet/bubbletea"
)

// sessionPickerPageRows 是会话选择面板一次可视的 Session 行数；列表更长时
// 渲染窗口跟随光标滚动，PgUp/PgDn 按页移动光标。
const sessionPickerPageRows = 8

// sessionPickerState 是 /session 无参打开的会话选择面板（模态覆盖层）。
// 打开期间 handleKey 顶部把全部按键路由给面板（仅 Ctrl+C 透传全局强退），
// 可打印字符不会落入输入框。err 非空表示最近一次加载/切换失败，在面板内
// 显示；列表为空时的 Enter 是重试加载。
type sessionPickerState struct {
	open     bool
	sessions []ui.SessionInfo
	cursor   int
	err      string
}

// openSessionPicker 打开会话选择面板并同步加载 Session 列表（ListSessions
// 是廉价本地读，与旧 /session 列表命令同一调用路径）；加载失败只在面板内
// 显错，可 Enter 重试，不写入消息流。
func (m *AppModel) openSessionPicker() {
	if m.deps.Controller == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	m.sessionPicker = sessionPickerState{open: true}
	m.reloadSessionPicker()
}

// closeSessionPicker 关闭面板并丢弃其全部状态；不发生任何切换副作用。
func (m *AppModel) closeSessionPicker() {
	m.sessionPicker = sessionPickerState{}
}

// reloadSessionPicker （重新）加载列表：成功时填充并把光标定位到当前
// Session（找不到则回首项）；失败时清空列表、只在面板内记录错误。
func (m *AppModel) reloadSessionPicker() {
	sessions, err := m.deps.Controller.ListSessions()
	if err != nil {
		m.sessionPicker.sessions = nil
		m.sessionPicker.cursor = 0
		m.sessionPicker.err = fmt.Sprintf("列表失败: %v", err)
		return
	}
	m.sessionPicker.sessions = sessions
	m.sessionPicker.err = ""
	cursor := 0
	if currentID := m.currentSessionID(); currentID != "" {
		for i, s := range sessions {
			if s.ID == currentID {
				cursor = i
				break
			}
		}
	}
	m.sessionPicker.cursor = cursor
}

// currentSessionID 返回当前活跃 Session ID：优先取 Hub 快照；快照未携带
// （轻量 Hub / 测试替身）时回退到面板列表里第一个 active 条目。
func (m AppModel) currentSessionID() string {
	if id := m.snapshot().Session.ID; id != "" {
		return id
	}
	for _, s := range m.sessionPicker.sessions {
		if s.Status == "active" {
			return s.ID
		}
	}
	return ""
}

// moveSessionPickerCursor 按 delta 移动光标并钳位到 [0, len-1]；
// 移动即放弃上一次切换失败的面板错误（新选择不应背着旧错误）。
func (m *AppModel) moveSessionPickerCursor(delta int) {
	n := len(m.sessionPicker.sessions)
	if n == 0 {
		m.sessionPicker.cursor = 0
		return
	}
	next := m.sessionPicker.cursor + delta
	if next < 0 {
		next = 0
	}
	if next >= n {
		next = n - 1
	}
	m.sessionPicker.cursor = next
	m.sessionPicker.err = ""
}

// confirmSessionPicker 处理面板 Enter：列表加载失败/为空时重试加载；否则
// 经 Controller 切换到光标选中的 Session——成功后关闭面板并清空本地会话
// 视图（消息流与结果不跨 Session，同 newSessionForce），失败留在面板显错。
func (m *AppModel) confirmSessionPicker() {
	if m.deps.Controller == nil {
		m.sessionPicker.err = "Session 管理器未初始化"
		return
	}
	if len(m.sessionPicker.sessions) == 0 {
		m.reloadSessionPicker()
		return
	}
	target := m.sessionPicker.sessions[m.sessionPicker.cursor]
	// B3：经 Controller 走 System.SwitchSession（语义同 /session <编号>）。
	changed, err := m.deps.Controller.SwitchSession(target.ID)
	if err != nil {
		m.sessionPicker.err = fmt.Sprintf("切换失败: %v", err)
		return
	}
	m.closeSessionPicker()
	if !changed {
		// 目标已是当前 Session：无副作用 no-op，本地视图保持原样。
		m.appendMsg(fmt.Sprintf("[session] 已是当前 Session: %s", target.ID), MsgInfo)
		return
	}
	m.resetSessionViews()
	m.appendMsg(fmt.Sprintf("[session] 已切换到 %s", target.ID), MsgInfo)
	m.appendMsg("[session] 历史上下文已恢复；非终态任务已阻断（不自动续跑），输入新提示词继续", MsgInfo)
}

// handleSessionPickerKey 是面板的模态键位分发：↑/↓（与 PgUp/PgDn 翻页）
// 移动光标，Enter 切换/重试，Esc 关闭不切换；其余键一律吞掉。
func (m AppModel) handleSessionPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		m.closeSessionPicker()
	case keyUp:
		m.moveSessionPickerCursor(-1)
	case keyDown:
		m.moveSessionPickerCursor(1)
	case keyPgUp:
		m.moveSessionPickerCursor(-sessionPickerPageRows)
	case keyPgDown:
		m.moveSessionPickerCursor(sessionPickerPageRows)
	case keyEnter:
		m.confirmSessionPicker()
	}
	return m, nil
}

// sessionPickerHeight 报告面板渲染后的行数（0 = 未打开），供输入区布局
// 扣除高度——与 interactionPanelHeight 同一模式。
func (m AppModel) sessionPickerHeight() int {
	if !m.sessionPicker.open || m.width <= 0 {
		return 0
	}
	return renderedLineCount(renderSessionPicker(m.theme, m.width, m.sessionPicker, m.currentSessionID()))
}

// sessionPickerWindow 计算跟随光标的可视窗口 [start, end)。
func sessionPickerWindow(cursor, total int) (start, end int) {
	if total <= sessionPickerPageRows {
		return 0, total
	}
	start = cursor - sessionPickerPageRows/2
	if start < 0 {
		start = 0
	}
	end = start + sessionPickerPageRows
	if end > total {
		end = total
		start = end - sessionPickerPageRows
	}
	return start, end
}

// renderSessionPicker 渲染会话选择面板（样式复用 Interaction 面板的边框与
// 选中/暗色风格）。Session 的首条用户输入是不可信展示文本，渲染前 sanitize。
func renderSessionPicker(t Theme, w int, p sessionPickerState, currentID string) string {
	innerW := w - 4
	if innerW < 20 {
		innerW = 20
	}

	lines := []string{t.InteractionTitle.Render("◆ 选择 Session")}
	if p.err != "" {
		lines = append(lines, t.MsgWarn.Render("  "+truncateDisplay(p.err, innerW-2)))
	}
	switch {
	case len(p.sessions) == 0 && p.err != "":
		lines = append(lines, t.SidebarDim.Render("  Enter 重试加载"))
	case len(p.sessions) == 0:
		lines = append(lines, t.SidebarDim.Render("  无 Session 记录"))
	default:
		start, end := sessionPickerWindow(p.cursor, len(p.sessions))
		for i := start; i < end; i++ {
			s := p.sessions[i]
			first := truncateDisplay(sanitizeTerminalText(s.FirstUserInput), 40)
			line := strings.TrimSpace(fmt.Sprintf("%s [%s] %s", s.ID, s.CreatedAt, first))
			if s.ID == currentID {
				line += "（当前）"
			}
			prefix := "  "
			style := t.SidebarDim
			if i == p.cursor {
				prefix = "› "
				style = t.InteractionKey
			}
			lines = append(lines, style.Render(prefix+truncateDisplay(line, innerW-2)))
		}
		if len(p.sessions) > sessionPickerPageRows {
			lines = append(lines, t.InteractionQueue.Render(fmt.Sprintf(
				"  %d-%d/%d  %s 翻页", start+1, end, len(p.sessions), pageKeysDisplay)))
		}
	}
	lines = append(lines, t.InteractionQueue.Render("↑/↓ 选择  Enter 切换  Esc 关闭"))
	return t.InteractionBorder.Width(innerW).Render(strings.Join(lines, "\n"))
}
