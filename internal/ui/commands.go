package ui

import "strings"

// CommandScope 标记一条斜杠命令适用的前端范围。
type CommandScope string

const (
	// ScopeShared 表示命令在全部前端可用（TUI 与 WebUI 都能执行，
	// 底层能力由 Controller 或快照提供）。
	ScopeShared CommandScope = "shared"
	// ScopeTUI 表示命令仅 TUI 可用（视图切换类命令，以及退出权——
	// /quit 属于主前端，dashboard 不提供 /api/quit）。
	ScopeTUI CommandScope = "tui"
)

// CommandSpec 描述一条斜杠命令，是所有前端（TUI 提示框、WebUI 补全
// 下拉、/help 帮助）共用的单一数据源。新增命令时在此登记，两个前端
// 的提示与帮助自动同步。
type CommandSpec struct {
	Name    string       `json:"name"`              // 命令名，不含 "/"，如 "cancel"
	Aliases []string     `json:"aliases,omitempty"` // 别名，不含 "/"，如 "dash"
	Args    string       `json:"args,omitempty"`    // 参数形态，如 "<task-id>"；无参为空
	Desc    string       `json:"desc"`              // 一行中文说明
	Scope   CommandScope `json:"scope"`
}

// Usage 返回 "/cancel <task-id>" 形态的完整用法串。
func (c CommandSpec) Usage() string {
	if c.Args == "" {
		return "/" + c.Name
	}
	return "/" + c.Name + " " + c.Args
}

// CommandCatalog 返回全部已登记命令。顺序即帮助展示顺序（稳定）。
func CommandCatalog() []CommandSpec {
	return []CommandSpec{
		{Name: "help", Desc: "显示命令帮助", Scope: ScopeShared},
		{Name: "status", Desc: "查看系统状态（代理 / 任务 / 模式）", Scope: ScopeShared},
		{Name: "cancel", Args: "<task-id>", Desc: "取消任务（ID 可只输前缀）", Scope: ScopeShared},
		{Name: "mode", Args: "[gate|exec|topo <值>]", Desc: "切换三轴模式（无参切换 gate 轴 immediate/plan）", Scope: ScopeShared},
		{Name: "plan", Args: "[approve|reject <前缀>]", Desc: "查看 / 批准 / 拒绝待批准的计划（gate=plan）", Scope: ScopeShared},
		{Name: "steer", Args: "<agent-id> <消息>", Desc: "向代理发送指导", Scope: ScopeShared},
		{Name: "new", Desc: "创建新 Session", Scope: ScopeShared},
		{Name: "session", Args: "[编号]", Desc: "列出 / 切换 Session", Scope: ScopeShared},
		{Name: "dashboard", Aliases: []string{"dash"}, Desc: "切换到仪表板视图", Scope: ScopeTUI},
		{Name: "chat", Desc: "切换到消息视图", Scope: ScopeTUI},
		{Name: "activity", Desc: "查看跨代理实时动态", Scope: ScopeTUI},
		{Name: "logs", Desc: "查看原始系统日志", Scope: ScopeTUI},
		{Name: "trace", Desc: "查看 Trace 与工具调用", Scope: ScopeTUI},
		{Name: "result", Aliases: []string{"detail"}, Desc: "查看完整任务结果", Scope: ScopeTUI},
		{Name: "agent", Args: "<id>", Desc: "查看代理详情", Scope: ScopeTUI},
		{Name: "quit", Desc: "退出系统", Scope: ScopeTUI},
	}
}

// MatchCommand 按"精确名 / 别名"解析命令名（不含 "/"）；未命中返回 false。
// TUI 命令分发与 WebUI 命令解析共用，保证两端命令集一致。
func MatchCommand(name string) (CommandSpec, bool) {
	name = strings.ToLower(name)
	for _, c := range CommandCatalog() {
		if c.Name == name {
			return c, true
		}
		for _, a := range c.Aliases {
			if a == name {
				return c, true
			}
		}
	}
	return CommandSpec{}, false
}

// PrefixCommands 返回命令名或别名以 prefix（不含 "/"）开头的全部命令，
// 供 "/" 输入补全使用。prefix 为空时返回整个目录。
func PrefixCommands(prefix string) []CommandSpec {
	prefix = strings.ToLower(prefix)
	var out []CommandSpec
	for _, c := range CommandCatalog() {
		if strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
			continue
		}
		for _, a := range c.Aliases {
			if strings.HasPrefix(a, prefix) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// IsSlashCommandText reports whether text belongs to the UI command channel
// rather than the Scheduler task channel. Trimming here is intentional: an
// input such as "  /help" must not bypass the backend boundary merely because
// a frontend forgot to normalize whitespace first.
func IsSlashCommandText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}
