package tui

import (
	"fmt"
	"strconv"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/ui"

	"github.com/charmbracelet/lipgloss"
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
		if m.deps.Controller != nil {
			m.deps.Controller.RequestQuit()
		}
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
		m.handleMode(parts)

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

	case "/doctor":
		if len(parts) < 2 || parts[1] != "agents" {
			m.appendMsg("[doctor] 用法: /doctor agents — 审计代理身份与实际权限的一致性（只读）", MsgWarn)
			return false
		}
		m.requestAgentAudit()

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

	case "/activity":
		m.view = ViewActivity

	case "/logs":
		m.view = ViewLogs

	case "/trace":
		m.view = ViewTrace

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

// showStatus 渲染系统状态。数据源是 Hub 最新快照（Observer.Snapshot()），
// 不再直读 Store / Scheduler——快照由 Hub 按轮询间隔刷新。
func (m *AppModel) showStatus() {
	snap := m.snapshot()
	tasks := snap.Tasks

	counts := map[string]int{}
	for _, t := range tasks {
		counts[t.Status]++
	}

	var lines []string
	lines = append(lines, "── 系统状态 ──")
	lines = append(lines, fmt.Sprintf("  Agents: %d", len(snap.Agents)))
	lines = append(lines, fmt.Sprintf("  Tasks: pending=%d  processing=%d  completed=%d  failed=%d",
		counts[string(model.TaskStatusPending)],
		counts[string(model.TaskStatusProcessing)],
		counts[string(model.TaskStatusCompleted)],
		counts[string(model.TaskStatusFailed)]))

	// exec / topo 两轴读自 Hub 快照；快照未装配对应 Getter 时回退默认值。
	execMode := snap.ExecMode
	if execMode == "" {
		execMode = "normal"
	}
	topoMode := snap.TopoMode
	if topoMode == "" {
		topoMode = "team"
	}
	lines = append(lines, fmt.Sprintf("  Exec: %s", execMode))
	lines = append(lines, fmt.Sprintf("  Topo: %s", topoMode))

	// Active tasks detail
	for _, t := range tasks {
		if t.Status != string(model.TaskStatusPending) && t.Status != string(model.TaskStatusProcessing) {
			continue
		}
		desc := truncateDisplay(t.Desc, 60)
		lines = append(lines, fmt.Sprintf("  %s [%s] %s — %s",
			t.Status, shortID(t.ID), desc, strings.Join(t.Agents, ",")))
	}

	m.appendMsg(strings.Join(lines, "\n"), MsgInfo)
}

func (m *AppModel) cancelTask(idPrefix string) {
	if m.deps.Controller == nil {
		m.appendMsg("[cancel] 取消功能未初始化", MsgError)
		return
	}
	// D2：前缀解析（过短/未找到/歧义）与 plan 守卫全部由 Hub 装配的
	// CancelTaskByPrefix 完成（与 LLM cancel_task 共用 tools.GuardedCancel，
	// source="user"）；错误原样渲染。
	taskID, err := m.deps.Controller.CancelTask(idPrefix)
	if err != nil {
		m.appendMsg(fmt.Sprintf("[cancel] %v", err), MsgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[cancel] 已取消任务 %s", shortID(taskID)), MsgInfo)
}

// shortID 返回任务 ID 的前 8 字符短形式；短于 8 字符时原样返回
// （F8：直接 t.ID[:8] 切片在短 ID 上会 panic）。
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// toggleMode 快速切换 topo 轴（team ↔ solo）。当前模式读自 Hub 快照；切换后的
// 界面显示在下一轮快照刷新（Hub 轮询间隔）时更新。
// V6 起 gate 轴已移除，/mode 无参的快捷 toggle 语义落在 topo 轴上。
func (m *AppModel) toggleMode() {
	if m.deps.Controller == nil {
		m.appendMsg("[mode] 控制面未初始化", MsgError)
		return
	}
	if m.snapshot().TopoMode == "solo" {
		if err := m.deps.Controller.SetTopoMode("team"); err != nil {
			m.appendMsg(fmt.Sprintf("[mode] %v", err), MsgError)
			return
		}
		m.appendMsg("[mode] 已切换到 team 模式", MsgInfo)
	} else {
		if err := m.deps.Controller.SetTopoMode("solo"); err != nil {
			m.appendMsg(fmt.Sprintf("[mode] %v", err), MsgError)
			return
		}
		m.appendMsg("[mode] 已切换到 solo 模式", MsgInfo)
	}
}

// modeUsageText 是 /mode 的中文用法说明（V6 起为 exec / topo 两轴），
// 非法参数时输出到消息流。
const modeUsageText = "[mode] 用法:\n" +
	"  /mode                                  快速切换 topo 轴（team ↔ solo）\n" +
	"  /mode exec normal|strict|readonly|yolo 设置执行权限轴\n" +
	"  /mode topo team|solo                   设置编排拓扑轴\n" +
	"  （gate 轴已于 V6 移除：执行前审阅改由 Graph approval 节点承担）"

// handleMode 分发 /mode 命令：无参快捷切换 topo 轴；带参时按轴（exec / topo）
// 设置到指定值，非法参数输出用法说明。
func (m *AppModel) handleMode(parts []string) {
	if len(parts) == 1 {
		m.toggleMode()
		return
	}
	if m.deps.Controller == nil {
		m.appendMsg("[mode] 控制面未初始化", MsgError)
		return
	}
	if len(parts) != 3 {
		m.appendMsg(modeUsageText, MsgWarn)
		return
	}
	axis := strings.ToLower(parts[1])
	value := parts[2]
	switch axis {
	case "gate":
		m.appendMsg("[mode] gate 轴已于 V6 移除：执行前审阅改由 Graph approval 节点承担", MsgWarn)
		m.appendMsg(modeUsageText, MsgWarn)
	case "exec":
		if err := m.deps.Controller.SetExecMode(value); err != nil {
			m.appendMsg(fmt.Sprintf("[mode] %v", err), MsgWarn)
			m.appendMsg(modeUsageText, MsgWarn)
			return
		}
		parsed, _ := modes.ParseExecMode(value) // Controller 已校验，此处不会失败
		m.appendMsg(fmt.Sprintf("[mode] exec 轴已切换到 %s", parsed.String()), MsgInfo)
	case "topo":
		if err := m.deps.Controller.SetTopoMode(value); err != nil {
			m.appendMsg(fmt.Sprintf("[mode] %v", err), MsgWarn)
			m.appendMsg(modeUsageText, MsgWarn)
			return
		}
		parsed, _ := modes.ParseTopoMode(value) // Controller 已校验，此处不会失败
		m.appendMsg(fmt.Sprintf("[mode] topo 轴已切换到 %s", parsed.String()), MsgInfo)
	default:
		m.appendMsg(fmt.Sprintf("[mode] 未知模式轴 %q", parts[1]), MsgWarn)
		m.appendMsg(modeUsageText, MsgWarn)
	}
}

// requestAgentAudit 触发 /doctor agents 只读代理审计：审计任务发布给
// Scheduler，报告作为普通任务结果回显到消息流（无需轮询）。
func (m *AppModel) requestAgentAudit() {
	if m.deps.Controller == nil {
		m.appendMsg("[doctor] 控制面未初始化", MsgError)
		return
	}
	taskID, err := m.deps.Controller.RequestAgentAudit()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[doctor] %v", err), MsgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[doctor] 代理审计任务已创建: %s — 审计报告将作为任务结果回显", shortID(taskID)), MsgInfo)
}

func (m *AppModel) steerAgent(agentID, msg string) {
	if m.deps.Controller == nil {
		m.appendMsg("[steer] 控制面未初始化", MsgError)
		return
	}
	// steer 邮件构造（From=user / MsgTypeSteer / PriorityHigh）在 Hub 侧。
	if err := m.deps.Controller.SteerAgent(agentID, msg); err != nil {
		m.appendMsg(fmt.Sprintf("[steer] 发送失败: %v", err), MsgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[steer] 已发送指导给 %s", agentID), MsgInfo)
}

func (m *AppModel) newSession() {
	if m.deps.Controller == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	// B3：经 Controller 走 System.NewSession（切换前快照旧 Session +
	// 重置系统结果快照），不直接操作 SessionManager。
	id, err := m.deps.Controller.NewSession()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[session] 创建失败: %v", err), MsgError)
		return
	}
	// B3：结果不跨 session——切换成功后清空结果视图
	m.lastResult = nil
	m.resultScroll = 0
	m.appendMsg(fmt.Sprintf("[session] 新 Session 已创建: %s", id), MsgInfo)
}

func (m *AppModel) listSessions() {
	if m.deps.Controller == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	sessions, err := m.deps.Controller.ListSessions()
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
		first := truncateDisplay(s.FirstUserInput, 50)
		lines = append(lines, fmt.Sprintf("  %d. %s [%s] %s",
			i+1, s.ID, s.CreatedAt, first))
	}
	m.appendMsg(strings.Join(lines, "\n"), MsgInfo)
}

func (m *AppModel) switchSession(numStr string) {
	if m.deps.Controller == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	sessions, err := m.deps.Controller.ListSessions()
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
	// B3：经 Controller 走 System.SwitchSession（语义同 newSession）。
	changed, err := m.deps.Controller.SwitchSession(target.ID)
	if err != nil {
		m.appendMsg(fmt.Sprintf("[session] 切换失败: %v", err), MsgError)
		return
	}
	if !changed {
		m.appendMsg(fmt.Sprintf("[session] 已是当前 Session: %s", target.ID), MsgInfo)
		return
	}
	// B3：结果不跨 session——切换成功后清空结果视图
	m.lastResult = nil
	m.resultScroll = 0
	m.appendMsg(fmt.Sprintf("[session] 已切换到 %s", target.ID), MsgInfo)
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

// helpText 由 ui.CommandCatalog 生成——命令目录是两个前端（TUI / WebUI）
// 的单一数据源，新增命令只需在目录登记，这里的帮助自动同步。
// 热键区由 keymap 声明表渲染（见 keymap.go），同样不需要手工维护。
var helpText = buildHelpText()

func buildHelpText() string {
	catalog := ui.CommandCatalog()
	usageW := 0
	for _, c := range catalog {
		if w := lipgloss.Width(c.Usage()); w > usageW {
			usageW = w
		}
	}
	var b strings.Builder
	b.WriteString("── AgentGo Commands ──\n")
	for _, c := range catalog {
		u := c.Usage()
		pad := strings.Repeat(" ", usageW-lipgloss.Width(u))
		fmt.Fprintf(&b, "  %s%s  %s\n", u, pad, c.Desc)
	}
	b.WriteString(helpHotkeys())
	return b.String()
}
