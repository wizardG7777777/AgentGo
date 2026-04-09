package builtin

import (
	"fmt"
	"os"

	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// RequireReadBeforeWriteHook 强制执行"先读后写"硬约束：在 write_file
// 或 edit_file 调用之前，要求当前任务的 ToolCallRecord 历史里有过对该
// 路径的成功 read_file 调用。
//
// **C8 是阶段 1 内唯一的新增 hook，不是迁移。**
// 在迁移之前 worker prompt 里有"先读后写"的软约束（是 prompt 文本而非
// 硬性检查）。这个 hook 把软约束升级为硬约束 —— 通过 store→hook 查询链路
// 验证 LLM 是否真的执行过对应的 read_file。
//
// 这是阶段 1 内**第一个**真正使用 StoreHookView.GetToolCallHistory 的 hook，
// 验证整条 ToolCallRecord 写入和查询链路工作正常。
//
// 决策（用户在 plan 阶段确认）：
//   - **新文件豁免**：os.Stat 显示文件不存在 → Continue（写新文件不可能先读）
//   - **list_dir 不算"已读"**：只有对该具体路径的 read_file 才算（list_dir
//     看到的是文件名，没看到内容，不构成"了解后再修改"的语义）
//   - **失败的 read 不计入**：Success=false 的 ToolCallRecord 被忽略
//   - **不同 path 的 read 不算**：路径精确匹配
//
// Phase: PreCall, Priority: 30（位于 PathBoundary=10、ValidateExpectedHash=20
// 之后；先做安全检查再做语义检查）。
type RequireReadBeforeWriteHook struct {
	Store store.StoreHookView
}

// NewRequireReadBeforeWriteHook 是 RequireReadBeforeWriteHook 的构造函数。
// store 为 nil 时 hook 退化为永远 Continue（防御式降级，避免测试环境崩溃）。
func NewRequireReadBeforeWriteHook(s store.StoreHookView) *RequireReadBeforeWriteHook {
	return &RequireReadBeforeWriteHook{Store: s}
}

// Name 返回 hook 唯一标识。
func (h *RequireReadBeforeWriteHook) Name() string { return "require-read-before-write" }

// Phase 返回 PhasePreCall。
func (h *RequireReadBeforeWriteHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 30。
func (h *RequireReadBeforeWriteHook) Priority() int { return 30 }

// Matches 仅匹配 write_file 和 edit_file。
func (h *RequireReadBeforeWriteHook) Matches(toolName string) bool {
	return toolName == "write_file" || toolName == "edit_file"
}

// Run 执行"先读后写"校验。
func (h *RequireReadBeforeWriteHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	// 防御：store 为 nil 时降级为 Continue（避免在最小测试环境下崩溃）
	if h.Store == nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	target, ok := hctx.Args["path"].(string)
	if !ok || target == "" {
		// path 缺失/类型错 → 让其他 hook (PathBoundary) 或工具自报错
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	// 新文件豁免：文件不存在 → 视为创建场景，无需先读
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	// 查询本任务的工具调用历史
	history := h.Store.GetToolCallHistory(hctx.TaskID)
	for _, rec := range history {
		// 只接受成功的 read_file（按用户决议）
		if rec.ToolName != "read_file" || !rec.Success {
			continue
		}
		// 路径精确匹配（按用户决议）
		if p, ok := rec.Args["path"].(string); ok && p == target {
			return hook.ToolHookDecision{Action: hook.Continue}
		}
	}
	return hook.ToolHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: fmt.Sprintf(
			"先读后写约束：写入文件 %s 之前必须先成功调用 read_file 读取该文件。"+
				"若任务真的需要在不读取的情况下修改，先 read_file 一次再 write_file。",
			target,
		),
	}
}
