package tui

import (
	"time"

	"agentgo/internal/ui"
)

// ViewState controls which content is shown in the main area.
type ViewState int

const (
	ViewChat       ViewState = iota // 会话视图（默认）：inline 排放流 + 活跃轮次尾部
	ViewGraph                       // 执行图全屏视图（/graph 进入）
	ViewNodeDetail                  // Selected graph node and its execution activity
	ViewResult                      // Full task result
)

// FocusState tracks which panel has keyboard focus.
type FocusState int

const (
	FocusInput       FocusState = iota // Text editor (default)
	FocusInteraction                   // Structured user interaction
	FocusMain                          // Main content area (graph/node navigation)
)

// MsgKind determines message styling.
type MsgKind int

const (
	MsgLog    MsgKind = iota // System logs (dim)
	MsgInfo                  // General notices
	MsgWarn                  // Warnings (yellow)
	MsgError                 // Errors (red)
	MsgResult                // Task results (green card)
	MsgAgent                 // Agent output (per-agent)
)

// StyledMsg is a message with kind, timestamp, and optional agent attribution.
type StyledMsg struct {
	Text      string
	Reasoning string
	Kind      MsgKind
	At        time.Time
	AgentID   string // non-empty for agent-attributed messages
	StreamID  string // non-empty for replace-in-place LLM stream snapshots
}

// AgentInfo 是单个代理的运行状态快照（仪表板渲染用）。
// 自 UI Hub 接入起，它只是 ui.AgentCard 的别名——数据由 Hub 轮询产生，
// 渲染代码因此完全不需要改动。
type AgentInfo = ui.AgentCard

// GraphInfo / GraphNodeInfo keep rendering code independent from GraphStore.
// The UI Hub owns the authoritative frontend projection.
type GraphInfo = ui.GraphView
type GraphNodeInfo = ui.GraphNodeView

// Deps aggregates all external dependencies for the TUI.
//
// TUI 不再直接持有任何系统通道 / 组件：所有系统状态经 Observer 订阅
// （首条必为 KindSnapshotSync 全量快照），所有写操作经 Controller 进入
// UI Hub。两者的生产实现都是 bootstrap 装配的 *ui.Hub。
type Deps struct {
	// Controller 是 UI Hub 控制面（发用户输入、Interaction 回答、/cancel、/mode、
	// /steer、session 切换、/quit）。nil 时各命令渲染"未初始化"错误。
	Controller ui.Controller
	// Observer 是 UI Hub 观测面（Subscribe + Snapshot）。nil 时界面只显示
	// 初始空状态（测试用）。
	Observer ui.Observer
	// InitialResult 是启动时恢复的上次任务结果（进入结果视图的内容）。
	InitialResult string
}

// Layout holds the calculated dimensions of each panel.
type Layout struct {
	// Overall
	Width, Height int

	// Main content
	MainX, MainY, MainW, MainH int

	// Interaction panel (shown above the input when pending)
	InteractionY, InteractionH int

	// Input editor
	InputY, InputH int

	// Status bar
	StatusY, StatusH int

	// Compact mode (narrow terminal)
	Compact bool
}

const (
	compactThreshold = 80
	statusBarHeight  = 1
	inputMinHeight   = 3
	inputMaxHeight   = 15
	minBodyHeight    = 1
	inputHeight      = inputMinHeight
)
