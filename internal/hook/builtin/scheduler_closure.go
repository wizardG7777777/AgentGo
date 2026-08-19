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
//   - 当前 session 存在图（submit_graph 落过图），或
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
// Phase: PreCall, Priority: 500（默认层——不早于路径/模式类硬 Gate，
// 收口审查与工具参数合法性无关）。
type SchedulerClosureHook struct {
	Store        store.TaskStore
	Graphs       closureGraphLister
	Interactions closureInteractionPeek
	SessionID    func() string

	mu        sync.Mutex
	confirmed map[string]bool // taskID → 已被推回确认过一次（第二次放行）
}

// NewSchedulerClosureHook 构造收口审查 hook；Graphs / Interactions /
// SessionID 可在 bootstrap 后续步骤装配（指针字段，注册后接线，装配期无并发）。
func NewSchedulerClosureHook(s store.TaskStore) *SchedulerClosureHook {
	return &SchedulerClosureHook{Store: s, confirmed: make(map[string]bool)}
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
	if h.hasClosureEvidence(hctx.Ctx, task) {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	summary, _ := hctx.Args["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return hook.ToolHookDecision{
			Action:   hook.Abort,
			HookName: h.Name(),
			AbortReason: "report_done 被拒绝：本次请求没有产生任何图、任务或交互，且 summary 为空——" +
				"没有可汇报的内容。若有工作要做请 submit_graph 建图；若这是纯问答，请给出非空的直接答复。",
			ReasonCode: "report_done_empty_summary",
			Suggestions: []hook.Suggestion{
				hook.NewSuggestion(h.Name(), "report_done_empty_summary", hctx.ToolName, true,
					hook.ToolCallAction("submit_graph", nil, "有实际工作时：构建任务图并执行"),
				),
			},
		}
	}

	h.mu.Lock()
	already := h.confirmed[task.ID]
	if !already {
		h.confirmed[task.ID] = true
	}
	h.mu.Unlock()
	if already {
		log.Printf("[gate %s] 任务 %s 零证据直答经确认放行（scheduler_direct_answer）", h.Name(), task.ID)
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	return hook.ToolHookDecision{
		Action:   hook.Abort,
		HookName: h.Name(),
		AbortReason: "report_done 收口审查：本次请求没有留下任何工作痕迹（无图、无 delegated 任务、无 pending 交互）。" +
			"若确认这是纯问答、无需建图执行，请再次调用 report_done 给出直接答复（将再次审查并放行）；" +
			"否则请 submit_graph 建图把工作委派出去。",
		ReasonCode: "scheduler_zero_evidence_closure",
		Suggestions: []hook.Suggestion{
			hook.NewSuggestion(h.Name(), "scheduler_zero_evidence_closure", hctx.ToolName, true,
				hook.ToolCallAction("submit_graph", nil, "有实际工作时：构建任务图并执行"),
				hook.ToolCallAction("report_done", nil, "确认纯问答时：再次调用 report_done 给出直接答复"),
			),
		},
	}
}

// hasClosureEvidence 报告本任务是否已留下工作痕迹（三个证据源任一命中）。
func (h *SchedulerClosureHook) hasClosureEvidence(ctx context.Context, task *model.Task) bool {
	// 图证据：当前 session 存在图（无 Session 模式退化为「任意图」）
	if h.Graphs != nil {
		sid := ""
		if h.SessionID != nil {
			sid = h.SessionID()
		}
		for _, g := range h.Graphs.List() {
			if g.SessionID == sid {
				return true
			}
		}
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
