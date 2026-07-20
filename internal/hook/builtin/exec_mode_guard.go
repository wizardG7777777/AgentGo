package builtin

import (
	"fmt"

	"agentgo/internal/hook"
	"agentgo/internal/modes"
)

// ExecModeGuardHook 在 exec 轴为 readonly（只读）模式时拦截写类工具
// （write_file / edit_file / run_shell），让 LLM 立即收到可自愈的中文错误，
// 而不是在工具执行后才发现副作用未发生。
//
// 设计决策：
//
//   - **只实现 readonly 语义**：normal / strict / yolo 一律 Continue——
//     strict / yolo 的行为约束是后续切片，本 hook 不越权实现。
//
//   - **Priority 5（早于 path-boundary 的 10）**：readonly 拦截是模式级
//     快速失败，与路径是否合法无关；先拦截可以避免在 Abort 已定的情况下
//     还跑一轮路径标准化 / 敏感文件校验，错误消息也更聚焦（"模式不允许"
//     优先于"路径有问题"）。
//
//   - **nil Store 安全**：未注入模式 store 时视为 normal（不拦截），与
//     modes 包"行为由消费方按轴读取"的约定一致——没有 store 即没有模式
//     声明，退化为 v4 行为。
//
// Phase: PreCall, Priority: 5（系统级最早，见上）。
type ExecModeGuardHook struct {
	Modes *modes.Store
}

// NewExecModeGuardHook 是 ExecModeGuardHook 的构造函数；modes 可为 nil
// （视为 normal）。
func NewExecModeGuardHook(modesStore *modes.Store) *ExecModeGuardHook {
	return &ExecModeGuardHook{Modes: modesStore}
}

// Name 返回 hook 唯一标识。
func (h *ExecModeGuardHook) Name() string { return "exec-mode-guard" }

// Phase 返回 PhasePreCall。
func (h *ExecModeGuardHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 5——早于 path-boundary(10) 快速失败，详见类型注释。
func (h *ExecModeGuardHook) Priority() int { return 5 }

// readonlyBlockedTools 声明 readonly 模式下被禁用的写类工具集合。
var readonlyBlockedTools = map[string]bool{
	"write_file": true,
	"edit_file":  true,
	"run_shell":  true,
}

// Matches 只匹配写类工具（write_file / edit_file / run_shell）。
func (h *ExecModeGuardHook) Matches(toolName string) bool {
	return readonlyBlockedTools[toolName]
}

// Run 在 readonly 模式下 Abort 写类工具；其它模式（normal / strict / yolo）
// 与 nil store 一律 Continue。
func (h *ExecModeGuardHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	if h.Modes == nil || h.Modes.GetExec() != modes.ExecReadonly {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	return hook.ToolHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: fmt.Sprintf(
			"当前处于 readonly 只读模式，工具 %s 已被禁用（readonly 模式下 write_file / edit_file / run_shell 均不可用）。"+
				"如需执行写操作，请先用 /mode exec normal 切换执行权限模式，或在配置文件 modes.exec 中调整",
			hctx.ToolName,
		),
	}
}
