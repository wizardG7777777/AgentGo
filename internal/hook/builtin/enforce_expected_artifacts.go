package builtin

import (
	"fmt"

	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// EnforceExpectedArtifactsHook 在 write_file / edit_file 调用之前校验 path 参数
// 必须严格等于当前任务的 expected_artifacts 中声明的某一条。
//
// # 背景（2026-04-14 多 Worker 系统测试发现）
//
// Scheduler 发布 worker 任务时声明 `expected_artifacts=[...]`，任务结束时
// `agent.checkExpectedArtifacts` 做 PostCall 校验（层 B）。但这是"事后校验"：
// 漂移已经发生、token 和 wall-clock 时间已经浪费，只能靠任务重试来修正。
//
// 本 hook 把校验前置到 PreCall 阶段，直接在 write_file 调用之前判断：
//   - path == expected_artifacts 中的任一字符串 → Continue
//   - path 是任务声明之外的路径 → Abort，指导 LLM 使用字面路径或联系 scheduler
//
// # 覆盖的两个问题
//
//   - **Expected_artifacts 路径漂移**：LLM 把 "config_group1_scheduler_agent_llm.md"
//     自由联想为 "config_fields_analysis.md"，本 hook 在第一次 write_file 就拦下，
//     避免 "漂移→PostCall 失败→重试→require-read-before-write 拦→read 再写" 的浪费循环
//
//   - **Worker 越权写用户最终产物**：worker-1 的 expected_artifacts 是
//     "config_group3_*.md"，但它试图写 "test_result.md"（scheduler 才该写的最终
//     产物），本 hook 直接 Abort，从根本上阻止越权
//
// # 双层校验（参照 PathBoundaryHook 决策 A1 / DependencyValidatorHook 层次）
//
//   - 层 A（本 hook）：PreCall 严格精确匹配，提供指导性错误消息
//   - 层 B（agent.checkExpectedArtifacts）：PostCall 末尾校验 + basename 容忍（2026-04-08
//     第二轮引入），禁用 hook 时仍生效，保证 V6/V9 可逆性
//
// # 不限制的场景
//
//   - 任务未声明 expected_artifacts（空切片 / nil）→ Continue（free-form 任务仍允许任意写入）
//   - Store 或 TaskID 缺失（测试环境）→ Continue（防御式降级）
//   - path 参数缺失或非 string → Continue（由 PathBoundaryHook 处理）
//
// # 不匹配的工具
//
// 仅匹配 write_file / edit_file。read_file / list_dir / grep_search / glob_search
// 都是只读工具，不涉及产出。
//
// Phase: PreCall, Priority: 35（晚于 PathBoundary=10、ValidateExpectedHash=20、
// DependencyValidator=25、RequireReadBeforeWrite=30，在所有路径 / UUID / 安全
// 校验通过之后再做任务级产出约束）。
type EnforceExpectedArtifactsHook struct {
	Store       store.StoreHookView
	ProjectRoot string
}

// NewEnforceExpectedArtifactsHook 是 EnforceExpectedArtifactsHook 的构造函数。
// store 或 projectRoot 为空时 hook 仍然返回 Continue（防御式降级），与
// DependencyValidatorHook 的空值处理策略一致。
func NewEnforceExpectedArtifactsHook(s store.StoreHookView, projectRoot string) *EnforceExpectedArtifactsHook {
	return &EnforceExpectedArtifactsHook{Store: s, ProjectRoot: projectRoot}
}

// Name 返回 hook 唯一标识。
func (h *EnforceExpectedArtifactsHook) Name() string { return "enforce-expected-artifacts" }

// Phase 返回 PhasePreCall。
func (h *EnforceExpectedArtifactsHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 35（任务级产出约束，排在所有安全和格式校验之后）。
func (h *EnforceExpectedArtifactsHook) Priority() int { return 35 }

// Matches 仅匹配 write_file / edit_file。
func (h *EnforceExpectedArtifactsHook) Matches(toolName string) bool {
	return toolName == "write_file" || toolName == "edit_file"
}

// Run 执行 expected_artifacts 精确匹配校验。
func (h *EnforceExpectedArtifactsHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	// 防御式降级
	if h.Store == nil || hctx.TaskID == "" {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	task, err := h.Store.GetTask(hctx.TaskID)
	if err != nil || task == nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	// 任务未声明 expected_artifacts → 不限制（free-form 任务保留原有自由度）
	if len(task.ExpectedArtifacts) == 0 {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	rawPath, _ := hctx.Args["path"].(string)
	if rawPath == "" {
		// 其他 hook（PathBoundary）会处理空 path
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	// 规范化为相对项目根的相对路径，与 RecordArtifactHook 使用的同一函数，
	// 确保 expected_artifacts 的声明路径（通常是相对路径）能与 write_file 的
	// 实际 path（可能是绝对或相对）做可比较的字符串匹配。
	normalized := normalizeArtifactPath(rawPath, h.ProjectRoot)

	for _, expected := range task.ExpectedArtifacts {
		// expected 也做规范化：scheduler 可能写 "./foo.md" 或 "foo.md"
		expectedNorm := normalizeArtifactPath(expected, h.ProjectRoot)
		if normalized == expectedNorm {
			return hook.ToolHookDecision{Action: hook.Continue}
		}
	}

	// Abort 并给出指导性错误消息，明确告知 LLM 三种合法出路
	return hook.ToolHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: fmt.Sprintf(
			"%s 被拒绝：路径 %q 不在本任务声明的 expected_artifacts 列表中。\n\n"+
				"本任务允许写入的文件：%v\n\n"+
				"正确做法（三选一）：\n"+
				"  (1) 修正 path 参数为 expected_artifacts 中的字面字符串（最常见情况）。"+
				"expected_artifacts 是 scheduler 与你之间的合约，必须字面执行，"+
				"禁止改文件名、加目录前缀、或根据内容自由联想；\n"+
				"  (2) 如果你确实需要写入多个文件（如主报告+附图），通过 send_message "+
				"向 scheduler 发送 question 类型消息，请求补充 expected_artifacts 声明，"+
				"等 scheduler 更新后再写；\n"+
				"  (3) 如果当前文件确实不该写（例如你错把用户最终产物理解成自己该写的产物），"+
				"直接停止 write_file，改为在文本响应中总结你的发现。",
			hctx.ToolName, normalized, task.ExpectedArtifacts,
		),
	}
}
