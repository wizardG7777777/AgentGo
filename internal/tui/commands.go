package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

// handleCommand processes slash commands. Returns true if the app should quit.
func (m *AppModel) handleCommand(line string) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}

	cmd := strings.ToLower(parts[0])
	switch cmd {
	case "/quit":
		m.appendMsg("[退出] 用户退出", MsgInfo)
		m.deps.CancelFn()
		return true

	case "/help":
		m.appendMsg(helpText, MsgInfo)
		m.view = ViewChat

	case "/status":
		m.showStatus()

	case "/cancel":
		if len(parts) < 2 {
			m.appendMsg("[cancel] 用法: /cancel <task-id>", MsgWarn)
			return false
		}
		m.cancelTask(parts[1])

	case "/mode":
		m.toggleMode()

	case "/steer":
		if len(parts) < 3 {
			m.appendMsg("[steer] 用法: /steer <agentID> <message>", MsgWarn)
			return false
		}
		agentID := parts[1]
		msg := strings.Join(parts[2:], " ")
		m.steerAgent(agentID, msg)

	case "/new":
		m.newSession()

	case "/session":
		if len(parts) < 2 {
			m.listSessions()
		} else {
			m.switchSession(parts[1])
		}

	case "/dashboard", "/dash":
		m.view = ViewDashboard
		m.appendMsg("[view] 切换到仪表板视图", MsgInfo)

	case "/chat":
		m.view = ViewChat
		m.appendMsg("[view] 切换到消息视图", MsgInfo)

	case "/detail", "/result":
		if m.lastResult == nil {
			m.appendMsg("[result] 暂无完整任务结果", MsgWarn)
			return false
		}
		m.view = ViewResult
		m.resultScroll = 0
		m.appendMsg("[view] 切换到完整结果视图", MsgInfo)

	case "/agent":
		if len(parts) < 2 {
			m.appendMsg("[agent] 用法: /agent <id> — 查看代理详情", MsgWarn)
			return false
		}
		m.selectAgentByID(parts[1])

	default:
		m.appendMsg(fmt.Sprintf("[command] 未知命令: %s (输入 /help 查看帮助)", cmd), MsgWarn)
	}

	return false
}

func (m *AppModel) showStatus() {
	tasks, err := m.deps.Store.ScanAll()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[status] 读取失败: %v", err), MsgError)
		return
	}

	counts := map[model.TaskStatus]int{}
	for _, t := range tasks {
		counts[t.Status]++
	}

	var lines []string
	lines = append(lines, "── 系统状态 ──")
	lines = append(lines, fmt.Sprintf("  Agents: %d", len(m.agents)))
	lines = append(lines, fmt.Sprintf("  Tasks: pending=%d  processing=%d  completed=%d  failed=%d",
		counts[model.TaskStatusPending],
		counts[model.TaskStatusProcessing],
		counts[model.TaskStatusCompleted],
		counts[model.TaskStatusFailed]))

	mode := "Immediate"
	if m.deps.Scheduler.Mode.Get() == scheduler.ModePlan {
		mode = "Plan"
	}
	lines = append(lines, fmt.Sprintf("  Mode: %s", mode))

	// Active tasks detail
	for _, t := range tasks {
		if t.Status != model.TaskStatusPending && t.Status != model.TaskStatusProcessing {
			continue
		}
		desc := t.Description
		if len(desc) > 60 {
			desc = desc[:59] + "…"
		}
		lines = append(lines, fmt.Sprintf("  %s [%s] %s — %s",
			string(t.Status), t.ID[:8], desc, strings.Join(t.Agents, ",")))
	}

	m.appendMsg(strings.Join(lines, "\n"), MsgInfo)
}

func (m *AppModel) cancelTask(idPrefix string) {
	tasks, err := m.deps.Store.ScanAll()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[cancel] 读取失败: %v", err), MsgError)
		return
	}
	for _, t := range tasks {
		if !strings.HasPrefix(t.ID, idPrefix) {
			continue
		}
		if t.Status != model.TaskStatusPending && t.Status != model.TaskStatusProcessing {
			m.appendMsg(fmt.Sprintf("[cancel] 任务 %s 状态为 %s，无法取消", t.ID[:8], t.Status), MsgWarn)
			return
		}
		if err := store.TransitionStateWithCancelSource(m.deps.Store, t.ID, t.Status, model.TaskStatusCancelled, "user"); err != nil {
			m.appendMsg(fmt.Sprintf("[cancel] 失败: %v", err), MsgError)
			return
		}
		m.appendMsg(fmt.Sprintf("[cancel] 已取消任务 %s", t.ID[:8]), MsgInfo)
		return
	}
	m.appendMsg(fmt.Sprintf("[cancel] 未找到以 %s 开头的任务", idPrefix), MsgWarn)
}

func (m *AppModel) toggleMode() {
	curr := m.deps.Scheduler.Mode.Get()
	if curr == scheduler.ModeImmediate {
		m.deps.Scheduler.Mode.Set(scheduler.ModePlan)
		m.appendMsg("[mode] 已切换到 Plan 模式", MsgInfo)
	} else {
		m.deps.Scheduler.Mode.Set(scheduler.ModeImmediate)
		m.appendMsg("[mode] 已切换到 Immediate 模式", MsgInfo)
	}
}

func (m *AppModel) steerAgent(agentID, msg string) {
	if m.deps.Mailbox == nil {
		m.appendMsg("[steer] 邮箱未初始化", MsgError)
		return
	}
	m.deps.Mailbox.Send(mailbox.Message{
		From:     "user",
		To:       agentID,
		Content:  msg,
		Summary:  msg,
		Type:     mailbox.MsgTypeSteer,
		Priority: mailbox.PriorityHigh,
		SentAt:   time.Now(),
	})
	m.appendMsg(fmt.Sprintf("[steer] 已发送指导给 %s", agentID), MsgInfo)
}

func (m *AppModel) newSession() {
	if m.deps.SessionMgr == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	sess, err := m.deps.SessionMgr.CreateNew()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[session] 创建失败: %v", err), MsgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[session] 新 Session 已创建: %s", sess.ID), MsgInfo)
}

func (m *AppModel) listSessions() {
	if m.deps.SessionMgr == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	sessions, err := m.deps.SessionMgr.List()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[session] 列表失败: %v", err), MsgError)
		return
	}
	if len(sessions) == 0 {
		m.appendMsg("[session] 无 Session 记录", MsgInfo)
		return
	}

	var lines []string
	lines = append(lines, "── Sessions ──")
	for i, s := range sessions {
		first := s.FirstUserInput
		if len(first) > 50 {
			first = first[:49] + "…"
		}
		lines = append(lines, fmt.Sprintf("  %d. %s [%s] %s",
			i+1, s.SessionID, s.CreatedAt, first))
	}
	m.appendMsg(strings.Join(lines, "\n"), MsgInfo)
}

func (m *AppModel) switchSession(numStr string) {
	if m.deps.SessionMgr == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	sessions, err := m.deps.SessionMgr.List()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[session] 列表失败: %v", err), MsgError)
		return
	}
	num, err := strconv.Atoi(numStr)
	if err != nil || num < 1 || num > len(sessions) {
		m.appendMsg(fmt.Sprintf("[session] 无效编号: %s (1-%d)", numStr, len(sessions)), MsgWarn)
		return
	}
	target := sessions[num-1]
	if err := m.deps.SessionMgr.SwitchTo(target.SessionID); err != nil {
		m.appendMsg(fmt.Sprintf("[session] 切换失败: %v", err), MsgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[session] 已切换到 %s", target.SessionID), MsgInfo)
}

func (m *AppModel) selectAgentByID(id string) {
	for i, ag := range m.agents {
		if strings.HasPrefix(ag.ID, id) {
			m.selectedAgent = i
			m.view = ViewAgentDetail
			m.appendMsg(fmt.Sprintf("[agent] 查看代理 %s", ag.ID), MsgInfo)
			return
		}
	}
	m.appendMsg(fmt.Sprintf("[agent] 未找到以 %s 开头的代理", id), MsgWarn)
}

const helpText = `── AgentGo Commands ──
  /help              显示此帮助
  /status            查看系统状态
  /cancel <id>       取消任务
  /mode              切换 Immediate/Plan 模式
  /steer <id> <msg>  向代理发送指导
  /new               创建新 Session
  /session [num]     列出/切换 Session
  /dashboard         切换到仪表板视图
  /chat              切换到消息视图
  /result            查看完整任务结果
  /detail            查看完整任务结果
  /agent <id>        查看代理详情
  /quit              退出

── Hotkeys ──
  Tab                切换焦点 (Input → Sidebar → Main)
  ↑/↓                侧边栏代理选择
  j/k PgUp/PgDn      在完整结果视图中滚动
  Enter              选中代理 / 提交输入
  Esc                返回仪表板
  Ctrl+C             退出`
