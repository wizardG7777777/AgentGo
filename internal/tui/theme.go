package tui

import "github.com/charmbracelet/lipgloss"

// Theme holds all pre-computed styles for the TUI. Inspired by crush's
// centralized Styles struct — all sub-components reference this single instance.
type Theme struct {
	// Header
	HeaderStyle   lipgloss.Style
	HeaderTitle   lipgloss.Style
	HeaderMeta    lipgloss.Style
	HeaderSep     lipgloss.Style

	// Sidebar
	SidebarBorder   lipgloss.Style
	SidebarTitle    lipgloss.Style
	SidebarAgent    lipgloss.Style
	SidebarSelected lipgloss.Style
	SidebarDim      lipgloss.Style
	SidebarSection  lipgloss.Style

	// Agent states
	StateIdle       lipgloss.Style
	StateProcessing lipgloss.Style
	StateApproval   lipgloss.Style
	StateTerminate  lipgloss.Style

	// Main content
	MainBorder lipgloss.Style

	// Dashboard cards
	CardBorder   lipgloss.Style
	CardTitle    lipgloss.Style
	CardBody     lipgloss.Style
	CardActive   lipgloss.Style
	CardIdle     lipgloss.Style

	// Messages
	MsgTimestamp lipgloss.Style
	MsgLog       lipgloss.Style
	MsgInfo      lipgloss.Style
	MsgWarn      lipgloss.Style
	MsgError     lipgloss.Style
	MsgResult    lipgloss.Style

	// Result card
	ResultBorder lipgloss.Style
	ResultTitle  lipgloss.Style

	// Approval
	ApprovalBorder lipgloss.Style
	ApprovalTitle  lipgloss.Style
	ApprovalKey    lipgloss.Style
	ApprovalCmd    lipgloss.Style
	ApprovalQueue  lipgloss.Style

	// Editor / Input
	EditorPrompt lipgloss.Style
	EditorCursor lipgloss.Style

	// Status bar
	StatusStyle lipgloss.Style
	StatusKey   lipgloss.Style
	StatusVal   lipgloss.Style

	// Markdown
	MdH1       lipgloss.Style
	MdH2       lipgloss.Style
	MdH3       lipgloss.Style
	MdBold     lipgloss.Style
	MdCode     lipgloss.Style
	MdCodeBlk  lipgloss.Style
	MdList     lipgloss.Style
	MdDivider  lipgloss.Style

	// Task status
	TaskPending    lipgloss.Style
	TaskProcessing lipgloss.Style
	TaskCompleted  lipgloss.Style
	TaskFailed     lipgloss.Style
	TaskCancelled  lipgloss.Style

	// Icons
	IconAgent     string
	IconIdle      string
	IconRunning   string
	IconApproval  string
	IconDone      string
	IconFailed    string
	IconPending   string
	IconArrow     string
	IconSection   string
}

// DefaultTheme creates the default color theme.
func DefaultTheme() Theme {
	accent := lipgloss.Color("39")    // blue
	green := lipgloss.Color("82")     // bright green
	yellow := lipgloss.Color("214")   // orange/yellow
	red := lipgloss.Color("196")      // red
	dim := lipgloss.Color("240")      // dark gray
	dimmer := lipgloss.Color("236")   // very dark gray
	bright := lipgloss.Color("252")   // bright gray
	cyan := lipgloss.Color("87")      // cyan
	_ = lipgloss.Color("141")         // purple, reserved

	return Theme{
		// Header
		HeaderStyle: lipgloss.NewStyle().
			Background(lipgloss.Color("235")).
			Foreground(bright).
			Bold(true),
		HeaderTitle: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		HeaderMeta: lipgloss.NewStyle().
			Foreground(dim),
		HeaderSep: lipgloss.NewStyle().
			Foreground(lipgloss.Color("237")),

		// Sidebar
		SidebarBorder: lipgloss.NewStyle().
			Border(lipgloss.Border{Right: "│"}).
			BorderForeground(lipgloss.Color("237")),
		SidebarTitle: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true).
			Padding(0, 1),
		SidebarAgent: lipgloss.NewStyle().
			Foreground(bright).
			Padding(0, 1),
		SidebarSelected: lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(cyan).
			Bold(true).
			Padding(0, 1),
		SidebarDim: lipgloss.NewStyle().
			Foreground(dim).
			Padding(0, 1),
		SidebarSection: lipgloss.NewStyle().
			Foreground(yellow).
			Bold(true).
			Padding(0, 1),

		// Agent states
		StateIdle: lipgloss.NewStyle().
			Foreground(dim),
		StateProcessing: lipgloss.NewStyle().
			Foreground(green).
			Bold(true),
		StateApproval: lipgloss.NewStyle().
			Foreground(yellow).
			Bold(true),
		StateTerminate: lipgloss.NewStyle().
			Foreground(red),

		// Main content
		MainBorder: lipgloss.NewStyle(),

		// Dashboard cards
		CardBorder: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("237")).
			Padding(0, 1),
		CardTitle: lipgloss.NewStyle().
			Foreground(bright).
			Bold(true),
		CardBody: lipgloss.NewStyle().
			Foreground(dim),
		CardActive: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(green).
			Padding(0, 1),
		CardIdle: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("237")).
			Padding(0, 1),

		// Messages
		MsgTimestamp: lipgloss.NewStyle().Foreground(dim),
		MsgLog:       lipgloss.NewStyle().Foreground(dim),
		MsgInfo:      lipgloss.NewStyle().Foreground(bright),
		MsgWarn:      lipgloss.NewStyle().Foreground(yellow).Bold(true),
		MsgError:     lipgloss.NewStyle().Foreground(red).Bold(true),
		MsgResult:    lipgloss.NewStyle().Foreground(green),

		// Result card
		ResultBorder: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(green).
			Padding(0, 1),
		ResultTitle: lipgloss.NewStyle().
			Foreground(green).
			Bold(true),

		// Approval
		ApprovalBorder: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(red).
			Padding(0, 1),
		ApprovalTitle: lipgloss.NewStyle().
			Foreground(red).
			Bold(true),
		ApprovalKey: lipgloss.NewStyle().
			Foreground(cyan).
			Bold(true),
		ApprovalCmd: lipgloss.NewStyle().
			Foreground(yellow),
		ApprovalQueue: lipgloss.NewStyle().
			Foreground(dim),

		// Editor
		EditorPrompt: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		EditorCursor: lipgloss.NewStyle().
			Foreground(accent),

		// Status bar
		StatusStyle: lipgloss.NewStyle().
			Background(lipgloss.Color("235")).
			Foreground(dim),
		StatusKey: lipgloss.NewStyle().
			Foreground(cyan).
			Bold(true),
		StatusVal: lipgloss.NewStyle().
			Foreground(dim),

		// Markdown
		MdH1:      lipgloss.NewStyle().Bold(true).Foreground(yellow),
		MdH2:      lipgloss.NewStyle().Bold(true).Foreground(accent),
		MdH3:      lipgloss.NewStyle().Bold(true).Foreground(green),
		MdBold:    lipgloss.NewStyle().Bold(true).Foreground(bright),
		MdCode:    lipgloss.NewStyle().Foreground(lipgloss.Color("247")).Background(dimmer),
		MdCodeBlk: lipgloss.NewStyle().Foreground(bright).Background(lipgloss.Color("235")),
		MdList:    lipgloss.NewStyle().Foreground(bright),
		MdDivider: lipgloss.NewStyle().Foreground(dim),

		// Task status
		TaskPending:    lipgloss.NewStyle().Foreground(dim),
		TaskProcessing: lipgloss.NewStyle().Foreground(green),
		TaskCompleted:  lipgloss.NewStyle().Foreground(green),
		TaskFailed:     lipgloss.NewStyle().Foreground(red),
		TaskCancelled:  lipgloss.NewStyle().Foreground(dim),

		// Icons
		IconAgent:    "◉",
		IconIdle:     "○",
		IconRunning:  "●",
		IconApproval: "⏳",
		IconDone:     "✓",
		IconFailed:   "✗",
		IconPending:  "◇",
		IconArrow:    "▸",
		IconSection:  "─",
	}
}
