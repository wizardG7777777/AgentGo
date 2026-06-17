package tui

import (
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

// ViewState controls which content is shown in the main area.
type ViewState int

const (
	ViewDashboard   ViewState = iota // All agents in a card grid
	ViewAgentDetail                  // Selected agent's output stream
	ViewChat                         // System message history
	ViewResult                       // Full task result
)

// FocusState tracks which panel has keyboard focus.
type FocusState int

const (
	FocusInput   FocusState = iota // Text editor (default)
	FocusSidebar                   // Agent list navigation
	FocusMain                      // Main content area
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
	Text    string
	Kind    MsgKind
	At      time.Time
	AgentID string // non-empty for agent-attributed messages
}

// AgentInfo is a snapshot of an agent's current state, used by the TUI dashboard.
type AgentInfo struct {
	ID               string
	Type             string // "worker", "explorer", "scheduler"
	State            string // "idle", "processing", "waiting_approval", "terminating"
	CurrentTaskID    string
	CurrentTaskDesc  string
	MailboxPending   int
	PromptTokens     int64
	CompletionTokens int64
	CallCount        int
	Loop             int
	Phase            string
	LastModelText    string
	LastTool         string
	ToolCallCount    int
	LastActivityAt   time.Time
	ActivityAge      string
	LastError        string
}

// Deps aggregates all external dependencies for the TUI.
type Deps struct {
	Store         store.TaskStore
	EventCh       chan<- model.Event
	CancelFn      func()
	Scheduler     *scheduler.Bundle
	Mailbox       *mailbox.Registry
	ApprovalCh    <-chan shell.ApprovalRequest
	SessionMgr    *session.SessionManager
	SystemMsgCh   <-chan string
	OutputCh      <-chan string
	InitialResult string

	// AgentInfoFn returns current snapshots of all agents.
	// Set by bootstrap; nil means no agent info available.
	AgentInfoFn func() []AgentInfo
}

// Layout holds the calculated dimensions of each panel.
type Layout struct {
	// Overall
	Width, Height int

	// Header
	HeaderY, HeaderH int

	// Sidebar
	SidebarX, SidebarY, SidebarW, SidebarH int

	// Main content
	MainX, MainY, MainW, MainH int

	// Approval bar (overlaps input when active)
	ApprovalY, ApprovalH int

	// Input editor
	InputY, InputH int

	// Status bar
	StatusY, StatusH int

	// Compact mode (no sidebar, header collapses)
	Compact bool
}

const (
	sidebarMinWidth  = 24
	sidebarMaxWidth  = 32
	compactThreshold = 80
	headerHeight     = 1
	statusBarHeight  = 1
	inputHeight      = 3
)
