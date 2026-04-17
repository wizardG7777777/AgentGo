package builtin

import (
	"fmt"
	"regexp"
	"strings"

	"agentgo/internal/hook"
	"agentgo/internal/store"
)

// uuidRE 匹配 publish_task 返回的任务 ID 格式（uuid.NewString() 输出）：
// 8-4-4-4-12 的十六进制串，如 7b52b232-4e9b-4b97-8bbc-f3d5927dc814。
var uuidRE = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// DependencyValidatorHook 在 publish_task 调用之前校验 dependencies 参数中
// 的每个 ID：
//
//  1. UUID 格式前置校验 —— 快速拦截占位符 / 自造 ID（如 "task-part1"、"A"、
//     "<A 的 task_id>"）。这是 LLM 在 top-down 规划时最常见的幻觉模式。
//  2. store 存在性校验 —— 格式合法但任务尚未发布（或已被取消/清理）时拦截。
//
// 设计背景（2026-04-13 多 Worker 系统测试发现）：
// Scheduler 在 Immediate 模式下倾向于 top-down 规划——先把最终目标（汇总任务）
// 固化下来，再铺设中间步骤。结果就是在第一步就调 publish_task 发布汇总任务，
// 并用占位符填 dependencies。这是一种 LLM 心智模型错位（top-down 规划 vs
// bottom-up 发布），prompt 指引是必要的前置手段，但运行时 hook 必须作为硬兜底。
//
// 双层校验决策（参照 PathBoundaryHook 的决策 A1）：
//   - 层 A（本 hook）：UUID 格式校验 + store 存在性 + 指导性错误消息（分清
//     "占位符"和"未发布/已清理"两种场景），教 LLM 怎么改
//   - 层 B（meta.go publishTask 内保留 GetTask 兜底）：禁用所有 hook 时仍能
//     挡住"依赖永远无法满足的任务"进入 store，保证 V6/V9 回归可逆性
//
// Phase: PreCall, Priority: 25（系统级早段；与其他 hook 目标工具不同
// 因此顺序无语义约束：PathBoundary=10 [read/write]、ValidateExpectedHash=20
// [write]、DependencyValidator=25 [publish_task]、RequireReadBeforeWrite=30
// [write]）。
type DependencyValidatorHook struct {
	Store store.StoreHookView
}

// NewDependencyValidatorHook 是 DependencyValidatorHook 的构造函数。
// store 为 nil 时 hook 退化为永远 Continue（防御式降级，避免测试环境崩溃），
// 与 RequireReadBeforeWriteHook 的降级策略一致。
func NewDependencyValidatorHook(s store.StoreHookView) *DependencyValidatorHook {
	return &DependencyValidatorHook{Store: s}
}

// Name 返回 hook 唯一标识。
func (h *DependencyValidatorHook) Name() string { return "dependency-validator" }

// Phase 返回 PhasePreCall。
func (h *DependencyValidatorHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 25（系统级早段；与其他 hook 目标工具不同，顺序无语义约束）。
func (h *DependencyValidatorHook) Priority() int { return 25 }

// Matches 仅匹配 publish_task 工具。send_message / cancel_task / 其他工具
// 都没有 dependencies 参数。
func (h *DependencyValidatorHook) Matches(toolName string) bool {
	return toolName == "publish_task"
}

// Run 执行 dependencies 参数校验。
//   - dependencies 参数缺失或为空串 → Continue（无依赖是合法调用）
//   - dependencies 中任意一个 ID 不是 UUID 格式 → Abort，提示占位符禁用
//   - dependencies 中任意一个 ID 在 store 中不存在 → Abort，提示"先发布再依赖"
//   - 全部通过 → Continue
func (h *DependencyValidatorHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	// Store 为 nil 时降级为 Continue（防御式）
	if h.Store == nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	depsRaw, exists := hctx.Args["dependencies"]
	if !exists {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	depsStr, ok := depsRaw.(string)
	if !ok {
		return hook.ToolHookDecision{
			Action:   hook.Abort,
			HookName: h.Name(),
			AbortReason: fmt.Sprintf(
				"publish_task 的 dependencies 参数类型必须是 string（逗号分隔的 UUID 列表），收到 %T",
				depsRaw,
			),
		}
	}
	depsStr = strings.TrimSpace(depsStr)
	if depsStr == "" {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	for _, id := range strings.Split(depsStr, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		// 层 A-1：UUID 格式前置校验 —— 快速拦截占位符
		if !uuidRE.MatchString(id) {
			return hook.ToolHookDecision{
				Action:   hook.Abort,
				HookName: h.Name(),
				AbortReason: fmt.Sprintf(
					"publish_task 被拒绝：dependencies 中 %q 不是合法的 UUID 格式。"+
						"每个依赖 ID 必须是之前 publish_task 调用返回的真实 task UUID"+
						"（形如 7b52b232-4e9b-4b97-8bbc-f3d5927dc814），"+
						"禁止使用占位符（如 \"task-part1\"、\"A\"、\"<id>\"）或自造 ID。"+
						"若被依赖任务尚未发布，请先调用 publish_task 发布它、"+
						"从返回值中读取真实 id 之后再发布当前任务。",
					id,
				),
			}
		}

		// 层 A-2：store 存在性校验 —— 格式对但任务不存在
		if _, err := h.Store.GetTask(id); err != nil {
			return hook.ToolHookDecision{
				Action:   hook.Abort,
				HookName: h.Name(),
				AbortReason: fmt.Sprintf(
					"publish_task 被拒绝：依赖任务 %s 不存在于 store 中。"+
						"可能原因：(a) 该任务尚未发布 —— 请先调用 publish_task 发布它，"+
						"从返回值读取真实 id 之后再发布当前任务；"+
						"(b) 该任务已被取消或清理 —— 请检查最新的 board snapshot 确认可用任务列表。",
					id,
				),
			}
		}
	}

	return hook.ToolHookDecision{Action: hook.Continue}
}
