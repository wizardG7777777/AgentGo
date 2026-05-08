package builtin

import (
	"fmt"
	"os"
	"path/filepath"

	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// RequireReadBeforeWriteHook 强制执行"先读后写"硬约束：在 write_file
// 或 edit_file 调用之前，要求当前任务的 task.ReadSet 中存在对该路径的
// 成功 read_file 记录。
//
// **v5 Phase 6 重写**（ReactiveSystem.md §5.2.1）：
//   - 旧（v4）：反查 GetToolCallHistory ToolCallRecord 历史，O(N) per check
//   - 新（v5 Phase 6）：直接查 task.ReadSet（O(1)），由 read-set-write Reactor
//     在 read_file 成功事件触发时异步写入
//
// 决策（v4 时代用户拍板，v5 保持语义不变）：
//   - **新文件豁免**：os.Stat 显示文件不存在 → Continue（写新文件不可能先读）
//   - **list_dir 不算"已读"**：read-set-write Reactor 只为 read_file 写 ReadSet
//   - **失败的 read 不计入**：Reactor 在 ev.Error != "" 时不写 ReadSet
//   - **不同 path 的 read 不算**：精确匹配（绝对路径 key）
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

// Run 执行"先读后写"校验。v5 Phase 6 起改读 task.ReadSet 取代反查工具历史。
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
	// 把目标路径规范化为绝对路径，与 ReadSet key（read-set-write Reactor 写入
	// 时也调 filepath.Abs）对齐。Abs 失败时退化为原 path 直接对比。
	targetAbs, err := filepath.Abs(target)
	if err == nil {
		targetAbs = filepath.Clean(targetAbs)
	} else {
		targetAbs = target
	}

	// 查 task.ReadSet（O(1)）。任务不存在或查询失败时降级——不能阻塞工具调用
	// 链路（hookSystem.md §11.1.4 容错原则）；ReadSet 缺失走 Abort 才是预期。
	readSet, qerr := h.Store.GetReadSet(hctx.TaskID)
	if qerr == nil {
		if _, ok := readSet[targetAbs]; ok {
			return hook.ToolHookDecision{Action: hook.Continue}
		}
		// 兼容回退：read-set-write Reactor 是 Async，理论上有"刚 read_file 完
		// 立即 write_file 但 ReadSet 还没写入"的窄竞争窗口。同时也兜底 Abs
		// 失败导致的 key 不一致——对原始 path 也查一次。
		if _, ok := readSet[target]; ok {
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
