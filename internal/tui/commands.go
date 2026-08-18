package tui

import (
	"encoding/json"
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
		if len(parts) >= 2 && strings.ToLower(parts[1]) == "force" {
			m.newSessionForce()
		} else {
			m.newSession()
		}

	case "/doctor":
		if len(parts) < 2 || parts[1] != "agents" {
			m.appendMsg("[doctor] 用法: /doctor agents — 审计代理身份与实际权限的一致性（只读）", MsgWarn)
			return false
		}
		m.requestAgentAudit()

	case "/event":
		if len(parts) < 3 {
			m.appendMsg("[event] 用法: /event <graph-id> <事件名> [数据JSON] — 向图的 wait_event 节点投递外部事件", MsgWarn)
			return false
		}
		var data map[string]any
		if len(parts) > 3 {
			if err := json.Unmarshal([]byte(strings.Join(parts[3:], " ")), &data); err != nil {
				m.appendMsg(fmt.Sprintf("[event] 数据不是合法 JSON 对象: %v", err), MsgError)
				return false
			}
		}
		m.emitGraphEvent(parts[1], parts[2], data)

	case "/session":
		if len(parts) < 2 {
			// 无参打开会话选择面板（↑/↓ 选择，Enter 切换，Esc 关闭）。
			m.openSessionPicker()
		} else {
			m.switchSession(parts[1])
		}

	case "/graph":
		m.view = ViewGraph
		m.appendMsg("[view] 切换到执行图视图", MsgInfo)

	case "/chat":
		m.view = ViewChat
		m.appendMsg("[view] 切换到会话视图", MsgInfo)

	case "/detail", "/result":
		if m.lastResult == nil {
			m.appendMsg("[result] 暂无完整任务结果", MsgWarn)
			return false
		}
		m.view = ViewResult
		m.resultScroll = 0
		m.appendMsg("[view] 切换到完整结果视图", MsgInfo)

	case "/node":
		if len(parts) < 2 {
			m.appendMsg("[node] 用法: /node <id> — 查看当前图的节点详情", MsgWarn)
			return false
		}
		m.selectNodeByID(parts[1])

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
	lines = append(lines, fmt.Sprintf("  Graphs: %d", len(snap.Graphs)))
	for _, graph := range snap.Graphs {
		completed, active := graphProgress(graph)
		lines = append(lines, fmt.Sprintf("  Graph %s: %s  nodes=%d/%d completed  active=%d",
			graph.GraphID, graph.Status, completed, len(graph.Nodes), active))
	}
	lines = append(lines, fmt.Sprintf("  Runtime agents: %d", len(snap.Agents)))
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

// emitGraphEvent 经控制面向指定图的 wait_event 节点投递外部事件（/event）。
// 事件是时点信号：节点未在等待或所属 Session 冻结时到达视为未发生，
// 由 Runtime 内部闸门静默忽略（这里回报的是"已投递"，不保证命中）。
func (m *AppModel) emitGraphEvent(graphID, event string, data map[string]any) {
	if m.deps.Controller == nil {
		m.appendMsg("[event] 控制面未初始化", MsgError)
		return
	}
	if err := m.deps.Controller.EmitGraphEvent(graphID, event, data); err != nil {
		m.appendMsg(fmt.Sprintf("[event] %v", err), MsgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[event] 已投递事件 %q → 图 %s（节点未在等待时事件被忽略）", event, graphID), MsgInfo)
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

// resetSessionViews 在 Session 发生切换（强制新建 / 切换）成功后清空本地
// 会话相关视图：消息流、完整结果与结果滚动位置都不跨 Session（B3）。
// /new 普通新建是连续语义，只清结果视图，不调用本函数。
func (m *AppModel) resetSessionViews() {
	m.messages = nil
	m.lastResult = nil
	m.resultScroll = 0
}

// newSessionForce 终止当前 Session 的全部运行内容（任务取消、Graph 终结、
// Team 回收）后开新 Session。与 /new 的连续语义不同，这是破坏性重置：
// 本地消息流一并清空，旧 Session 以全终态快照归档。
func (m *AppModel) newSessionForce() {
	if m.deps.Controller == nil {
		m.appendMsg("[session] Session 管理器未初始化", MsgError)
		return
	}
	id, err := m.deps.Controller.NewSessionForce()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[session] 强制新建失败: %v", err), MsgError)
		return
	}
	m.resetSessionViews()
	m.appendMsg(fmt.Sprintf("[session] 已终止旧 Session 运行内容并创建新 Session: %s", id), MsgInfo)
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
	// B3：消息流与结果不跨 session——切换成功后清空本地会话视图
	//（与会话选择面板的切换成功路径共用同一清理）。
	m.resetSessionViews()
	m.appendMsg(fmt.Sprintf("[session] 已切换到 %s", target.ID), MsgInfo)
	m.appendMsg("[session] 历史上下文已恢复；非终态任务已阻断（不自动续跑），输入新提示词继续", MsgInfo)
}

func (m *AppModel) selectNodeByID(id string) {
	if !m.ensureSelectedGraph() {
		m.appendMsg("[node] 当前还没有执行图", MsgWarn)
		return
	}
	graph := &m.graphs[m.selectedGraph]
	matches := make([]int, 0, 1)
	for index, node := range graph.Nodes {
		if node.NodeID == id {
			matches = []int{index}
			break
		}
		if strings.HasPrefix(node.NodeID, id) {
			matches = append(matches, index)
		}
	}
	if len(matches) == 0 {
		m.appendMsg(fmt.Sprintf("[node] 图 %s 中未找到以 %s 开头的节点", graph.GraphID, id), MsgWarn)
		return
	}
	if len(matches) > 1 {
		m.appendMsg(fmt.Sprintf("[node] 前缀 %s 匹配多个节点，请输入更完整的 ID", id), MsgWarn)
		return
	}
	m.selectedNode = matches[0]
	m.nodeDetailScroll = 0
	m.view = ViewNodeDetail
	m.appendMsg(fmt.Sprintf("[node] 查看 %s/%s", graph.GraphID, graph.Nodes[matches[0]].NodeID), MsgInfo)
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
