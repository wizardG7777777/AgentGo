package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/shell"
)

// updateApproval 在审批面板激活、且非指导模式时处理键盘事件。
//
// 键位：
//   1     → Approved=true，放行
//   2/esc → Approved=false，拒绝
//   3     → 切到指导输入模式（Approved=false + Message=用户文本）
//   4     → "永远允许"：Approved=true + RememberPattern=req.Pattern
//           shell 层把该模式加入运行时白名单，本进程后续命中此模式不再询问
//   ctrl+c → 关闭整个 TUI（拒绝当前请求避免 Worker 阻塞）
func (m Model) updateApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "1":
		m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: true}
		m.appendMsg(fmt.Sprintf("[审批] 已放行: %s", m.activeApproval.Command))
		m.advanceApproval()
		return m, nil

	case "2", "esc":
		m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: false}
		m.appendMsg(fmt.Sprintf("[审批] 已拒绝: %s", m.activeApproval.Command))
		m.advanceApproval()
		return m, nil

	case "3":
		m.guidanceMode = true
		m.input.SetValue("")
		m.input.Placeholder = "输入指导消息后回车发送"
		m.input.Focus()
		return m, nil

	case "4":
		// 永远允许：Pattern 为空时降级为单次放行（理论不会发生，灰名单审批一定带 Pattern）
		pattern := m.activeApproval.Pattern
		m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: true, RememberPattern: pattern}
		if pattern != "" {
			m.appendMsg(fmt.Sprintf("[审批] 已放行并记住模式: %s", pattern))
		} else {
			m.appendMsg(fmt.Sprintf("[审批] 已放行（无模式可记忆）: %s", m.activeApproval.Command))
		}
		m.advanceApproval()
		return m, nil

	case "ctrl+c":
		m.activeApproval.ReplyCh <- shell.ApprovalReply{Approved: false}
		m.appendMsg("[退出] Ctrl-C，已拒绝待审批请求")
		m.deps.CancelFn()
		return m, tea.Quit
	}
	return m, nil
}

// advanceApproval 把队列里下一个请求提升为 active；队列空则清空 active，
// 并复位输入栏占位符与指导模式开关。
func (m *Model) advanceApproval() {
	if len(m.pendingApprovals) > 0 {
		next := m.pendingApprovals[0]
		m.pendingApprovals = m.pendingApprovals[1:]
		m.activeApproval = &next
	} else {
		m.activeApproval = nil
	}
	m.guidanceMode = false
	m.input.Placeholder = placeholderDefault
}

// renderApproval 渲染审批面板。queueLen 是后续排队请求数，>0 时显示提示。
//
// width 是终端宽度，<=0 时不约束（适合非终端测试场景）。
func renderApproval(req shell.ApprovalRequest, queueLen int, width int) string {
	queueHint := ""
	if queueLen > 0 {
		queueHint = fmt.Sprintf("  （后续还有 %d 个待审批）", queueLen)
	}

	var b strings.Builder
	b.WriteString(approvalTitleStyle.Render("⚠ 命令审批请求") + queueHint + "\n")
	b.WriteString(approvalLabelStyle.Render("代理: ") + req.AgentID + "\n")
	b.WriteString(approvalLabelStyle.Render("命令: ") + req.Command + "\n")
	if req.Pattern != "" {
		b.WriteString(approvalLabelStyle.Render("模式: ") + req.Pattern + "\n")
	}
	b.WriteString("\n")
	b.WriteString(approvalKeyStyle.Render("[1]") + " 通过    ")
	b.WriteString(approvalKeyStyle.Render("[2]") + " 拒绝    ")
	b.WriteString(approvalKeyStyle.Render("[3]") + " 输入指导    ")
	if req.Pattern != "" {
		b.WriteString(approvalKeyStyle.Render("[4]") + " 永远允许此模式\n")
	} else {
		b.WriteString("\n")
	}

	box := approvalBoxStyle
	if width > 4 {
		box = box.Width(width - 2)
	}
	return box.Render(b.String())
}

var (
	approvalTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	approvalLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	approvalKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	approvalBoxStyle   = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("196")).
				Padding(0, 1)
)
