package builtin

import (
	"context"
	"log"
	"strings"
	"sync"

	"agentgo/internal/graph"
	"agentgo/internal/hook"
	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
)

// closureGraphLister 是 SchedulerClosureHook 对图存储的最小依赖
// （*graph.Store 天然满足）。
type closureGraphLister interface {
	List() []graph.GraphSummary
}

// closureInteractionPeek 是 SchedulerClosureHook 对 Interaction 服务的最小
// 依赖（*interaction.Service 天然满足）。
type closureInteractionPeek interface {
	ListPending(ctx context.Context, sessionID string) ([]interaction.Request, error)
}

// SchedulerClosureHook 在 report_done preCall 阶段审查非图 scheduler 任务的
// 收口（2026-08-19 Flask SWE 评测事故：scheduler 不建图、不发任务、空答复
// 直接收口，用户请求零交付且无任何机制拦截）。
//
// 收口契约：report_done 时本会话必须已留下至少一项工作痕迹——
//   - 当前 session 存在图（start_graph 已落 Execution），或
//   - 公告板存在 delegated 任务（非 __scheduler__ 任务），或
//   - 存在 pending Interaction（已向用户提问，等待回答）。
//
// 三者全无时：
//   - summary 为空 → Abort（空答复永远不可接受，无确认豁免）；
//   - summary 非空 → 第一次 Abort 并附确认建议（零证据直答必须显式确认），
//     同一任务再次调用则放行并记日志（scheduler_direct_answer）。
//
// 边界（刻意保守）：本 hook 只查「有没有留下东西」，不判「做得对不对」——
// 后者是验收节点的语义职责。依赖（Graphs / Interactions / SessionID）为 nil
// 时跳过对应证据源而非 fail-closed，不制造新的故障面。
//
// report_done 路径刻意不加 toolFailed 前提（2026-08-21 SWE-008）：能成功
// 调用 report_done 的模型恰恰证明其工具调用格式能力未崩（崩盘的定义就是
// 「调不成任何工具」）；格式崩盘嫌疑只存在于零工具调用的纯文本路径
// （ReviewNaturalExit 的三态语义）。
//
// Phase: PreCall, Priority: 500（默认层——不早于路径/模式类硬 Gate，
// 收口审查与工具参数合法性无关）。
type SchedulerClosureHook struct {
	Store        store.TaskStore
	Graphs       closureGraphLister
	Interactions closureInteractionPeek
	SessionID    func() string

	mu        sync.Mutex
	confirmed map[string]int // taskID → 纯文本/report_done 已被推回的次数
}

// NewSchedulerClosureHook 构造收口审查 hook；Graphs / Interactions /
// SessionID 可在 bootstrap 后续步骤装配（指针字段，注册后接线，装配期无并发）。
func NewSchedulerClosureHook(s store.TaskStore) *SchedulerClosureHook {
	return &SchedulerClosureHook{Store: s, confirmed: make(map[string]int)}
}

// Name 返回 hook 唯一标识。
func (h *SchedulerClosureHook) Name() string { return "scheduler-closure-review" }

// Phase 返回 PhasePreCall。
func (h *SchedulerClosureHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 500（默认层）。
func (h *SchedulerClosureHook) Priority() int { return 500 }

// Matches 只匹配 report_done。
func (h *SchedulerClosureHook) Matches(toolName string) bool { return toolName == "report_done" }

// Run 执行收口审查，详见类型注释。
func (h *SchedulerClosureHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	if h.Store == nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	task, err := h.Store.GetTask(hctx.TaskID)
	if err != nil || task == nil {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	// 只审查非图 scheduler 控制面任务：graph controller 用 submit_task_result
	// 收口，worker/explorer 的工具面里根本没有 report_done。
	if task.EventType != "__scheduler__" || task.GraphID != "" {
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	if requiresGraphBeforeClosure(task) && !h.hasGraphEvidence() {
		return hook.ToolHookDecision{
			Action: hook.Abort, HookName: h.Name(), ReasonCode: "scheduler_graph_required",
			AbortReason: "report_done 被拒绝：新 Run 在 execution/recovery 阶段必须先形成并启动持久化 Graph；直接答复不是合法收口路径。",
			Suggestions: []hook.Suggestion{
				hook.NewSuggestion(h.Name(), "scheduler_graph_required", hctx.ToolName, true,
					hook.ToolCallAction("create_graph_draft", nil, "创建新的事务化 GraphDraft")),
			},
		}
	}
	if h.hasClosureEvidence(hctx.Ctx, task) {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	summary, _ := hctx.Args["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return hook.ToolHookDecision{
			Action:   hook.Abort,
			HookName: h.Name(),
			AbortReason: "report_done 被拒绝：本次请求没有产生任何图、任务或交互，且 summary 为空——" +
				"没有可汇报的内容。若有工作要做请从 create_graph_draft 开始事务化建图；若这是纯问答，请给出非空的直接答复。",
			ReasonCode: "report_done_empty_summary",
			Suggestions: []hook.Suggestion{
				hook.NewSuggestion(h.Name(), "report_done_empty_summary", hctx.ToolName, true,
					hook.ToolCallAction("create_graph_draft", nil, "有实际工作时：创建不可执行 Draft"),
				),
			},
		}
	}

	h.mu.Lock()
	count := h.confirmed[task.ID]
	h.confirmed[task.ID] = count + 1
	h.mu.Unlock()
	if count >= 1 {
		log.Printf("[gate %s] 任务 %s 零证据直答经确认放行（scheduler_direct_answer）", h.Name(), task.ID)
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	return hook.ToolHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: "report_done 收口审查：本次请求没有留下任何工作痕迹（无图、无 delegated 任务、无 pending 交互）。" +
			"若确认这是纯问答、无需建图执行，请再次调用 report_done 给出直接答复（将再次审查并放行）；" +
			"否则请从 create_graph_draft 开始建图。",
		ReasonCode: "scheduler_zero_evidence_closure",
		Suggestions: []hook.Suggestion{
			hook.NewSuggestion(h.Name(), "scheduler_zero_evidence_closure", hctx.ToolName, true,
				hook.ToolCallAction("create_graph_draft", nil, "有实际工作时：创建不可执行 Draft"),
				hook.ToolCallAction("report_done", nil, "确认纯问答时：再次调用 report_done 给出直接答复"),
			),
		},
	}
}

// ReviewNaturalExit 实现 hook.NaturalExitReviewer（2026-08-20 SWE-001 兜底 1，
// 2026-08-21 SWE-008 升级为三态状态机）：
// 纯文本自然退出路径（react_loop_exit:natural → text_only_submission）不经任何
// 工具调用，PreCall 的 Run 结构性够不到；本方法挂在 processTask 终态判别处，
// 与 Run 共享同一份零证据判定与 confirmed 计数。
//
// 三态语义（exitCount = confirmed[task.ID]，此前已被推回的次数）：
//   - exitCount=0 → 拒绝 + 通用 nudge（纯问答直接答复 / 有工作 create_graph_draft）；
//   - exitCount=1 且 toolFailed=false（纯问答场景）→ 放行记
//     scheduler_direct_answer——模型已被提醒过一次且本任务从未发生工具失败，
//     它的第二次答复视为确认后的合法直答；
//   - exitCount=1 且 toolFailed=true（工作场景，疑格式崩盘）→ 拒绝 + 格式
//     提醒——SWE-008 取证：三起「直答」全是长 JSON 损坏后的工具调用格式崩盘
//     （DSML/自造标记），放行即把残片落盘为正式答复；
//   - exitCount≥2 且 toolFailed=true → Retry=true 交 agent 按可恢复错误重试
//     换上下文（换干净上下文是模型格式能力自救的唯一有效通道），本方法同时
//     把退出计数清零，新 attempt 重获完整提醒梯度；防无限循环由 agent 侧
//     MaxRetries 全局上限兜底。
//
// output 恒非空（空响应已被上游守卫拦截），无「空 summary 永不豁免」分支。
func (h *SchedulerClosureHook) ReviewNaturalExit(ctx context.Context, task *model.Task, _ string, toolFailed bool) hook.NaturalExitDecision {
	if h.Store == nil || task == nil {
		return hook.NaturalExitDecision{Allow: true}
	}
	// 与 Run 相同的审查范围：只查非图 scheduler 控制面任务。
	if task.EventType != "__scheduler__" || task.GraphID != "" {
		return hook.NaturalExitDecision{Allow: true}
	}
	if task.RunPhase == runcontract.PhaseFinalization || task.EventSource == "graph-ended" {
		return hook.NaturalExitDecision{Allow: true}
	}
	if requiresGraphBeforeClosure(task) {
		if h.hasGraphEvidence() {
			return hook.NaturalExitDecision{Allow: true}
		}
	} else if h.hasClosureEvidence(ctx, task) {
		return hook.NaturalExitDecision{Allow: true}
	}

	h.mu.Lock()
	count := h.confirmed[task.ID]
	h.confirmed[task.ID] = count + 1
	if count >= 2 && toolFailed {
		// 重试前清零：新 attempt 从 exitCount=0 重新获得完整提醒梯度。
		delete(h.confirmed, task.ID)
	}
	h.mu.Unlock()
	if requiresGraphBeforeClosure(task) {
		if count == 0 {
			return hook.NaturalExitDecision{
				Nudge: "<system-reminder>你的纯文本答复未被接受：新 Run 必须形成持久化 Graph，execution/recovery 阶段没有直接答复出口。请从 create_graph_draft 开始；不要在正文中描述计划。</system-reminder>",
			}
		}
		h.mu.Lock()
		delete(h.confirmed, task.ID)
		h.mu.Unlock()
		return hook.NaturalExitDecision{Retry: true}
	}

	switch {
	case count == 0:
		return hook.NaturalExitDecision{
			Allow: false,
			Nudge: "<system-reminder>你的上次答复未被接受：本次请求没有留下任何工作痕迹（无图、无 delegated 任务、无 pending 交互）。" +
				"若确认这是纯问答、无需建图执行，请再次直接给出最终答复（将再次审查并放行）；" +
				"若有实际工作要做，请从 create_graph_draft 开始事务化建图。</system-reminder>",
		}
	case count == 1 && !toolFailed:
		log.Printf("[gate %s] 任务 %s 零证据直答经确认放行（scheduler_direct_answer，纯文本路径）", h.Name(), task.ID)
		return hook.NaturalExitDecision{Allow: true}
	case count == 1 && toolFailed:
		return hook.NaturalExitDecision{
			Allow: false,
			Nudge: "<system-reminder>你的上次答复未被接受：本次请求没有留下任何工作痕迹，且本任务此前发生过工具调用失败。" +
				"如果你刚才试图调用工具但调用未被识别——不要在正文中输出任何 XML/标记文本（如 DSML、<tool_call> 等），" +
				"直接以系统工具调用格式重新提交（新请求从 create_graph_draft 开始，再用原生 patch_graph_draft 分批构造）；" +
				"再次纯文本回复将被放弃并换上下文重试。</system-reminder>",
		}
	case count >= 2 && toolFailed:
		// 连续纯文本退出且存在工具失败记录：提醒已无意义，换上下文重试。
		log.Printf("[gate %s] 任务 %s 零证据直答连续 %d 次且存在工具失败记录，转可恢复重试（疑工具调用格式崩盘）",
			h.Name(), task.ID, count)
		return hook.NaturalExitDecision{Retry: true}
	default:
		// count>=2 且 toolFailed=false：理论不可达（count=1 时已放行终态），
		// 防御性按放行处理——防御分支不得引入比可达分支更严格的行为。
		log.Printf("[gate %s] 任务 %s 零证据直答经确认放行（scheduler_direct_answer，纯文本路径，防御分支）", h.Name(), task.ID)
		return hook.NaturalExitDecision{Allow: true}
	}
}

func requiresGraphBeforeClosure(task *model.Task) bool {
	return task != nil && task.EventType == "__scheduler__" && task.GraphID == "" &&
		task.RunContract != nil && task.RunPhase != runcontract.PhaseFinalization && task.EventSource != "graph-ended"
}

func (h *SchedulerClosureHook) hasGraphEvidence() bool {
	if h.Graphs == nil {
		return false
	}
	sid := ""
	if h.SessionID != nil {
		sid = h.SessionID()
	}
	for _, candidate := range h.Graphs.List() {
		if candidate.SessionID == sid {
			return true
		}
	}
	return false
}

// ResetExitCount 清零指定任务的纯文本退出计数。agent 在 ReviewNaturalExit
// 返回 Retry=true 并走 ErrRecoverable 重试时调用——新 attempt 从 exitCount=0
// 重新获得完整提醒梯度（本方法的 default 分支已先行清零，此入口供 agent
// 显式调用与测试对账，幂等）。
func (h *SchedulerClosureHook) ResetExitCount(taskID string) {
	h.mu.Lock()
	delete(h.confirmed, taskID)
	h.mu.Unlock()
}

// hasClosureEvidence 报告本任务是否已留下工作痕迹（三个证据源任一命中）。
func (h *SchedulerClosureHook) hasClosureEvidence(ctx context.Context, task *model.Task) bool {
	// 图证据：当前 session 存在图（无 Session 模式退化为「任意图」）
	if h.hasGraphEvidence() {
		return true
	}
	// 任务证据：公告板存在 delegated 任务。公告板随 session 切换整体换板
	// （ReplaceSnapshot），天然按 session 隔离，无需再过滤 session。
	if tasks, err := h.Store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t != nil && t.ID != task.ID && t.EventType != "__scheduler__" {
				return true
			}
		}
	}
	// 交互证据：存在 pending Interaction（已向用户提问）
	if h.Interactions != nil {
		sid := ""
		if h.SessionID != nil {
			sid = h.SessionID()
		}
		if reqs, err := h.Interactions.ListPending(ctx, sid); err == nil && len(reqs) > 0 {
			return true
		}
	}
	return false
}
