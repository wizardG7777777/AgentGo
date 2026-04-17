package tools

// AllToolNames 是系统支持的所有工具名称。
//
// 用途：bootstrap 启动期校验 config.ToolProfiles 中的工具名拼写。
// 更新规则：新增或删除工具时必须同步更新此列表。
//
// 分组注释对应 ToolGroup 归属，方便查找。
var AllToolNames = []string{
	// LocalReadGroup
	"read_file",
	"list_dir",
	"grep_search",
	"glob_search",

	// LocalWriteGroup
	"write_file",
	"edit_file",

	// WebGroup
	"web_search",
	"web_fetch",

	// ShellGroup
	"run_shell",

	// MetaGroup
	"publish_task",
	"send_message",

	// SchedulerGroup（scheduler 专属，不走 profile 配置）
	"cancel_task",
	"report_done",
	"probe_directory",
}

// ValidateToolNames 校验给定的工具名列表是否全部在 AllToolNames 中。
// 返回第一个不识别的工具名和 error；全部合法返回 nil。
func ValidateToolNames(names []string) error {
	known := make(map[string]bool, len(AllToolNames))
	for _, n := range AllToolNames {
		known[n] = true
	}
	for _, n := range names {
		if !known[n] {
			return &UnknownToolError{Name: n, Known: AllToolNames}
		}
	}
	return nil
}

// UnknownToolError 表示配置中出现了系统不识别的工具名。
type UnknownToolError struct {
	Name  string
	Known []string
}

func (e *UnknownToolError) Error() string {
	return "未知工具名 \"" + e.Name + "\"，请检查拼写。系统支持的工具: " + formatToolList(e.Known)
}

func formatToolList(names []string) string {
	if len(names) == 0 {
		return "(空)"
	}
	s := names[0]
	for _, n := range names[1:] {
		s += ", " + n
	}
	return s
}
