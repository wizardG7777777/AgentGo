package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ── 键位声明表（TUI 键位的单一事实源）──
//
// 这张表登记 TUI 的全部键位：handleKey 的键位分发引用下方键名常量，
// /help 的热键区与状态栏 hints 都从 keymap 表渲染——三处不再手工同步。
// 新增/调整键位：改表 + 在 handleKey 接入分发即可。
//
// 克制原则：这里只收敛"键名与文案"这个事实源，handleKey 的上下文分发
// 结构（Esc 链、Interaction 链）保持原样，不做表驱动状态机。

// 键名常量（bubbletea tea.KeyMsg.String() 形式）。handleKey 分发与
// 声明表共用，避免键位字符串字面量散落多处。
const (
	keyCtrlC    = "ctrl+c"
	keyCtrlL    = "ctrl+l"
	keyCtrlJ    = "ctrl+j"
	keyCtrlV    = "ctrl+v"
	keyCtrlB    = "ctrl+b"
	keyCtrlF    = "ctrl+f"
	keyAltEnter = "alt+enter"
	keyTab      = "tab"
	keyShiftTab = "shift+tab"
	keyEsc      = "esc"
	keyEnter    = "enter"
	keyUp       = "up"
	keyDown     = "down"
	keyPgUp     = "pgup"
	keyPgDown   = "pgdown"
	keyHome     = "home"
	keyEnd      = "end"
)

// keyContext 标识键位生效的上下文/视图（状态栏按它取舍 hints）。
type keyContext int

const (
	ctxGlobal          keyContext = iota // 任意焦点/视图
	ctxInput                             // 普通输入框聚焦
	ctxInteraction                       // 结构化交互面板聚焦
	ctxInteractionText                   // 交互补充文本输入
	ctxSidebar                           // 侧边栏聚焦
	ctxMain                              // Dashboard 主面板聚焦
	ctxAgentDetail                       // Agent 详情轮次历史主面板
	ctxResult                            // 完整结果视图（主面板焦点）
)

// keymapEntry 是一条键位声明。
type keymapEntry struct {
	id       string     // 语义 ID（如 "cycle-focus"），测试引用
	ctx      keyContext // 生效上下文（状态栏 hints 取舍）
	keys     string     // 状态栏展示键位（如 "↑↓"、"Ctrl+J"）
	hint     string     // 状态栏短动作（"" = 状态栏不显示）
	helpKeys string     // /help 热键区展示键位（可与 keys 不同，如组合键）
	help     string     // /help 热键区文案（"" = 不进 /help）
	trim     int        // 状态栏裁剪优先级：0 = 始终保留，数字越大越早被裁
}

// keymap 是全部键位的声明表。条目顺序即 /help 热键区与状态栏 hints 的
// 展示顺序（Esc、/help 固定殿后，与既有布局一致）。
var keymap = []keymapEntry{
	// 全局
	{id: "cycle-focus", ctx: ctxGlobal, keys: "Tab", hint: "focus",
		helpKeys: "Tab", help: "向前切换焦点 (含待处理 Interaction)"},
	{id: "cycle-focus-reverse", ctx: ctxGlobal, keys: "Shift+Tab", hint: "focus←", trim: 2,
		helpKeys: "Shift+Tab", help: "向后切换焦点"},
	{id: "clear-messages", ctx: ctxGlobal, keys: "Ctrl+L", hint: "clear", trim: 3,
		helpKeys: "Ctrl+L", help: "清空消息流"},
	// 侧边栏
	{id: "sidebar-select", ctx: ctxSidebar, keys: "↑↓", hint: "select",
		helpKeys: "↑/↓", help: "侧边栏代理选择"},
	{id: "sidebar-view", ctx: ctxSidebar, keys: "Enter", hint: "view"},
	// Dashboard 主面板（/help 与侧边栏共用"代理选择"一行，不重复列）
	{id: "main-select", ctx: ctxMain, keys: "↑↓", hint: "agent"},
	{id: "main-view", ctx: ctxMain, keys: "Enter", hint: "view"},
	// Agent 详情轮次历史
	{id: "agent-turn-scroll", ctx: ctxAgentDetail, keys: "↑↓", hint: "turns",
		helpKeys: "↑/↓ PgUp/PgDn", help: "Agent 轮次历史滚动"},
	{id: "agent-turn-latest", ctx: ctxAgentDetail, keys: "End", hint: "latest",
		helpKeys: "Home/End", help: "跳到最早轮次/恢复跟随最新轮次"},
	// 输入框
	{id: "input-submit", ctx: ctxInput, keys: "Enter", hint: "send",
		helpKeys: "Enter", help: "提交输入"},
	{id: "input-newline", ctx: ctxInput, keys: "Ctrl+J", hint: "newline",
		helpKeys: "Ctrl+J", help: "输入框内换行"},
	// Ctrl+V 读系统剪贴板整体插入（多行保留、不触发提交）；macOS 终端
	// 拦截 Cmd+V 后以 bracketed paste 投递，等效走 msg.Paste 分支。
	{id: "input-paste", ctx: ctxInput, keys: "Ctrl+V", hint: "paste", trim: 4,
		helpKeys: "Ctrl+V", help: "粘贴剪贴板内容（保留多行）"},
	// 输入历史只进 /help：状态栏空间紧张，再添一条 ↑↓ 提示会破坏既有
	// 宽度取舍（sidebar/input 长度对比回归测试）。
	{id: "input-history", ctx: ctxInput, keys: "↑↓",
		helpKeys: "↑/↓", help: "输入历史（光标在首行/末行）"},
	// Interaction
	{id: "interaction-select", ctx: ctxInteraction, keys: "↑↓", hint: "select",
		helpKeys: "↑/↓", help: "Interaction 选项选择"},
	{id: "interaction-submit", ctx: ctxInteraction, keys: "Enter", hint: "submit",
		helpKeys: "Enter", help: "提交选中的 Interaction 选项"},
	{id: "interaction-prompt-page", ctx: ctxInteraction, keys: "PgUp/PgDn", hint: "question",
		helpKeys: "PgUp/PgDn", help: "Interaction 问题文本翻页"},
	{id: "interaction-text-submit", ctx: ctxInteractionText, keys: "Enter", hint: "submit"},
	// 结果视图
	{id: "result-scroll", ctx: ctxResult, keys: "↑↓", hint: "scroll",
		helpKeys: "↑/↓ PgUp/PgDn", help: "在完整结果视图中滚动"},
	{id: "result-page", ctx: ctxResult, keys: "PgUp/PgDn", hint: "page"},
	// Esc 的状态栏动作随视图切换（back / 取消请求），由 renderStatusBar 特判。
	{id: "esc", ctx: ctxGlobal, keys: "Esc", hint: "取消请求",
		helpKeys: "Esc", help: "交互中返回；详情视图返回；顶层取消请求"},
	{id: "quit-warn", ctx: ctxGlobal, keys: "Ctrl+C",
		helpKeys: "Ctrl+C", help: "清输入/警告，再按一次强制退出"},
	// /help 入口本身是斜杠命令（不参与按键分发），热键区不重复列。
	{id: "help", ctx: ctxGlobal, keys: "/help", hint: "commands"},
}

// helpHotkeys 从 keymap 表渲染 /help 的热键区——与状态栏 hints 共用
// 同一张声明表，键位文案只有一个事实源。
func helpHotkeys() string {
	keyW := 0
	for _, e := range keymap {
		if e.help == "" {
			continue
		}
		if w := lipgloss.Width(e.helpKeys); w > keyW {
			keyW = w
		}
	}
	var b strings.Builder
	b.WriteString("\n── Hotkeys ──\n")
	for _, e := range keymap {
		if e.help == "" {
			continue
		}
		pad := strings.Repeat(" ", keyW-lipgloss.Width(e.helpKeys))
		fmt.Fprintf(&b, "  %s%s  %s\n", e.helpKeys, pad, e.help)
	}
	return b.String()
}
