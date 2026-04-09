package builtin

import (
	"fmt"

	"agentgo/internal/hook"
	"agentgo/internal/pathutil"
)

// PathBoundaryHook 在文件相关工具调用之前校验 path 参数是否在项目根
// 目录内，并阻止访问敏感文件（详见 pathutil.SensitivePatterns）。
//
// C6 的设计决策（hookSystem.md §10.1 + plan §F C6 + 用户确认）：
//
//   - **决策 A1（双重校验）**：本 hook 只做校验返回 Continue/Abort，
//     工具内部仍保留 pathutil.ValidatePath 调用做路径标准化（相对→绝对）。
//     原因：hook 系统只支持 Continue/Abort 不支持 Replace，hook 没有把
//     标准化路径写回 args 的途径；保留双重校验代价是每次工具调用多一次
//     纯函数调用，可忽略。**更重要**的是禁用所有 hook 时工具行为仍然
//     正确——这是回归测试的关键
//
//   - **不匹配 run_shell**：当前 internal/tools/shell.go 没有 path 参数，
//     run_shell 的命令字符串通过 sh -c 解析自身路径，hook 无法在调用前
//     截获路径。如未来 run_shell 引入 working_dir 参数，再扩展 Matches 集
//
//   - **path 缺失或非字符串 → Abort**（用户决议）：file 系工具没有 path
//     参数是不合法调用。hook 拒绝比让工具自己报错更早、更显式
//
// Phase: PreCall, Priority: 10（系统级最早，与 hookSystem.md §5.2 的
// 0-100 系统强制段对齐）。
type PathBoundaryHook struct {
	ProjectRoot string
}

// NewPathBoundaryHook 是 PathBoundaryHook 的构造函数。
func NewPathBoundaryHook(projectRoot string) *PathBoundaryHook {
	return &PathBoundaryHook{ProjectRoot: projectRoot}
}

// Name 返回 hook 唯一标识。
func (h *PathBoundaryHook) Name() string { return "path-boundary" }

// Phase 返回 PhasePreCall。
func (h *PathBoundaryHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 10（系统级最早）。
func (h *PathBoundaryHook) Priority() int { return 10 }

// Matches 返回是否匹配本 hook 的工具集合。
//
// 注意：不包含 run_shell —— 详见类型注释中的决策。
// 不包含 web_* / publish_task / send_message —— 它们没有 path 参数。
func (h *PathBoundaryHook) Matches(toolName string) bool {
	switch toolName {
	case "read_file", "list_dir", "list_files",
		"grep_search", "glob_search",
		"write_file", "edit_file":
		return true
	}
	return false
}

// Run 执行路径校验。
//   - args["path"] 缺失或非字符串 → Abort（用户决议：严格策略）
//   - pathutil.ValidatePath 返回 error（越界 / 敏感文件）→ Abort
//   - 其他情况 → Continue
func (h *PathBoundaryHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	rawPath, exists := hctx.Args["path"]
	if !exists {
		return hook.ToolHookDecision{
			Action:      hook.Abort,
			HookName:    h.Name(),
			AbortReason: fmt.Sprintf("工具 %s 缺少 path 参数", hctx.ToolName),
		}
	}
	pathStr, ok := rawPath.(string)
	if !ok {
		return hook.ToolHookDecision{
			Action:      hook.Abort,
			HookName:    h.Name(),
			AbortReason: fmt.Sprintf("工具 %s 的 path 参数类型必须是 string，收到 %T", hctx.ToolName, rawPath),
		}
	}
	if pathStr == "" {
		return hook.ToolHookDecision{
			Action:      hook.Abort,
			HookName:    h.Name(),
			AbortReason: fmt.Sprintf("工具 %s 的 path 参数不能为空", hctx.ToolName),
		}
	}
	if _, err := pathutil.ValidatePath(pathStr, h.ProjectRoot); err != nil {
		return hook.ToolHookDecision{
			Action:      hook.Abort,
			HookName:    h.Name(),
			AbortReason: fmt.Sprintf("路径校验失败: %v", err),
		}
	}
	return hook.ToolHookDecision{Action: hook.Continue}
}
