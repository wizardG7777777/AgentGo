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

	// EvidenceGroup
	"read_evidence",
	"inspect_board",
	"inspect_node",

	// LocalWriteGroup
	"apply_change",

	// WebGroup
	"web_search",
	"web_fetch",

	// ShellGroup
	"run_shell",

	// CommunicationGroup
	"send_message",
	"request_user_input",

	// PlanControlGroup（是否可见由 profile/内置 Scheduler 装配决定）
	"submit_task_result",
	"request_replan",

	// AgentTemplateGroup（scheduler 专属，不走 profile 配置）
	"list_agent_templates",
	"provision_agent_team",

	// 图编排：request_replan 在节点提交通道注册。
	"read_graph_definition",
	"apply_graph_change",
	"control_graph",
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
