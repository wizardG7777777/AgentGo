// Package builtin 提供 Hook System 的内置 hook 实现。
// 阶段 1 范围（hookSystem.md §10.2）：
//   - RecordArtifactHook       — Post，迁移自 LocalWriteGroup.recordArtifact（C5）
//   - PathBoundaryHook         — Pre，迁移自 pathutil.ValidatePath 散布调用（C6）
//   - ValidateExpectedHashHook — Pre，迁移自 LocalWriteGroup 内的 SHA256 校验段（C7）
//   - RequireReadBeforeWriteHook — Pre，新增（C8）
package builtin

import (
	"path/filepath"
	"strings"

	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// RecordArtifactHook 在 write_file / edit_file 成功执行后，把目标路径
// 追加到任务的 task.Artifacts 列表。
//
// 这是 C5 的迁移产物：原 LocalWriteGroup.recordArtifact 内联实现已删除，
// 整套语义在本 hook 中重建。
//
// 设计要点：
//   - Phase: PhasePostCall — 工具执行后的纯观察操作
//   - Priority: 950 — 观察类高位，让所有前置 hook 决策都先发生
//   - 仅在 hctx.Err == nil 时记录 — 工具失败/被 abort 都不记录，避免
//     "失败的写"被错误地登记为产物
//   - 路径标准化：normalizeArtifactPath 把绝对路径转换为相对项目根的相对路径
//   - 任务不存在 / Store 写入失败：静默吞错并 Continue，因为 hook 不能
//     阻塞工具调用链路（hookSystem.md §11.1.4 的容错原则）
type RecordArtifactHook struct {
	Store       store.StoreHookView
	ProjectRoot string
}

// NewRecordArtifactHook 是 RecordArtifactHook 的构造函数。
// 用闭包形式封装两个依赖（store 与 projectRoot），便于 bootstrap 注册。
func NewRecordArtifactHook(s store.StoreHookView, projectRoot string) *RecordArtifactHook {
	return &RecordArtifactHook{Store: s, ProjectRoot: projectRoot}
}

// Name 返回 hook 唯一标识。
func (h *RecordArtifactHook) Name() string { return "record-artifact" }

// Phase 返回 PhasePostCall。
func (h *RecordArtifactHook) Phase() hook.ToolHookPhase { return hook.PhasePostCall }

// Priority 返回 950（观察类高位）。
func (h *RecordArtifactHook) Priority() int { return 950 }

// Matches 返回是否匹配本 hook 的工具集合。
// 仅匹配 write_file 和 edit_file —— 这两个工具会真正改变文件状态。
func (h *RecordArtifactHook) Matches(toolName string) bool {
	return toolName == "write_file" || toolName == "edit_file"
}

// Run 执行本 hook 的核心逻辑。post 阶段返回值被 Registry 忽略，
// 但仍按接口约定返回 Continue 决策。
func (h *RecordArtifactHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	// 工具失败或被前置 hook abort 时不记录 artifact。
	// 这与 C5 之前 LocalWriteGroup.recordArtifact 的行为一致：
	// 原实现把 recordArtifact 调用放在 os.WriteFile 成功之后。
	if hctx.Err != nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	// store 为 nil 时静默跳过 —— 测试环境或最小注册场景。
	if h.Store == nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	path, _ := hctx.Args["path"].(string)
	if path == "" {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	rel := normalizeArtifactPath(path, h.ProjectRoot)
	// 写入失败（任务不存在等）也只记一次决策为 Continue —— 不能阻塞链路。
	_ = h.Store.AppendArtifact(hctx.TaskID, rel)
	return hook.ToolHookDecision{Action: hook.Continue}
}

// normalizeArtifactPath 把绝对路径转换为相对项目根的相对路径。
//
// 实现来源：原 internal/tools/local_write.go 的同名函数，C5 迁移到 hook 包，
// 同时**修复**了 Windows 路径分隔符问题：用 filepath.ToSlash 把 \ 替换为 /，
// 让 task.Artifacts 在所有平台上都使用 / 分隔符。
//
// 行为契约：
//   - projectRoot 非空且路径在其内部 → 返回 / 风格相对路径（如 docs/foo.md）
//   - 路径在 projectRoot 之外（filepath.Rel 返回 ".." 前缀）→ 返回 / 风格 cleaned 路径
//   - projectRoot 为空 → 返回 / 风格 cleaned 路径
//
// 设计理由：artifact 路径主要供 LLM 阅读和下游 worker 解析，跨平台一致比
// OS native 分隔符更重要。原 tools 包的 TestNormalizeArtifactPath 在 Windows
// 上一直失败的根因就是这里 —— C5 迁移顺手解决，对应测试也已删除。
func normalizeArtifactPath(absPath, projectRoot string) string {
	cleaned := filepath.Clean(absPath)
	if projectRoot != "" {
		if rel, err := filepath.Rel(projectRoot, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(cleaned)
}
