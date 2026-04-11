package builtin

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"agentgo/internal/hook"
)

// TeamAwarenessConfig 配置 TeamAwarenessHook 的三个 section。
//
// 三个 section 各有独立的频率控制（方案乙），hook 每轮都跑，内部按各自
// 频率决定是否输出内容，最后合并非空 section。
//
// Token 预算截断优先级：goal > team > file。超预算时先丢 file，再丢 team，
// 永远保留 goal。理由：目标锚定是防止目标漂移的最后一道防线，比协作感知
// 更重要。详见 nextUpgrade_v3.md §8.7 + 用户拍板的三个决策。
type TeamAwarenessConfig struct {
	// SnapshotFn 是团队快照生成函数，传入 selfID 返回 <team-snapshot>...</team-snapshot>
	// XML。通常绑定到 worker.BuildTeamSnapshot 的闭包。
	// nil 时 Section 1 (TeamSnapshot) 始终为空。
	SnapshotFn func(selfID string) string

	// SnapshotRefreshInterval 是 Section 1 / Section 2 在 LoopPre 阶段的周期
	// 性刷新间隔（轮数）。默认 5。File section 与此共享频率（File 不单独配置）。
	// 0 禁用周期性刷新（但首轮由 PhaseTaskStart 仍会注入一次）。
	SnapshotRefreshInterval int

	// GoalRefreshInterval 是 Section 3 (GoalAnchor) 在 LoopPre 阶段的刷新间隔。
	// 默认 3——比 SnapshotRefreshInterval 更频繁。理由见文件头注释。
	GoalRefreshInterval int

	// ForceOnMail 为 true 时，当本轮 mailbox drain 收到新消息，Section 1 和
	// Section 2 立即刷新（不等周期）。Section 3 不受 ForceOnMail 影响——
	// 目标锚点与是否收到新消息无因果关系。
	ForceOnMail bool

	// MaxTokens 是整个注入内容的 token 预算上限。默认 800。
	// 内部按 1 token ≈ 2 runes 估算（对中英混合内容保守估值）。
	MaxTokens int

	// GoalEnabled 和 FileEnabled 控制对应 section 是否启用。
	// Team section 总是启用（除非 SnapshotFn 为 nil，此时视作禁用）。
	GoalEnabled bool
	FileEnabled bool

	// RecentToolsWindow 是 GoalAnchor section 展示的最近工具调用数。默认 5。
	RecentToolsWindow int
}

// withDefaults 返回补齐默认值的配置副本，不修改原配置。
func (c TeamAwarenessConfig) withDefaults() TeamAwarenessConfig {
	if c.SnapshotRefreshInterval <= 0 {
		c.SnapshotRefreshInterval = 5
	}
	if c.GoalRefreshInterval <= 0 {
		c.GoalRefreshInterval = 3
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 800
	}
	if c.RecentToolsWindow <= 0 {
		c.RecentToolsWindow = 5
	}
	return c
}

// NewTeamAwarenessHooks 构造一对共享配置的 AgentHook:
//   - TaskStart hook：注册到 PhaseTaskStart，任务首次进入时注入全部启用的 section
//   - LoopPre hook：注册到 PhaseLoopPre，按各 section 独立频率刷新
//
// 两个 hook 共享同一个 *teamAwarenessCore，确保配置只需写一份。
// 返回的 slice 供 bootstrap 一次性注册两个 hook。
func NewTeamAwarenessHooks(cfg TeamAwarenessConfig) []hook.AgentHook {
	core := &teamAwarenessCore{cfg: cfg.withDefaults()}
	return []hook.AgentHook{
		&teamAwarenessTaskStart{core: core},
		&teamAwarenessLoopPre{core: core},
	}
}

// teamAwarenessCore 承载配置和 section 渲染逻辑，被两个子 hook 共享。
// 不直接实现 AgentHook 接口——因为一个接口只能有一个 Phase，而本 hook
// 需要同时响应 TaskStart 和 LoopPre 两个阶段。
type teamAwarenessCore struct {
	cfg TeamAwarenessConfig
}

// ---- TaskStart 子 hook ----

type teamAwarenessTaskStart struct{ core *teamAwarenessCore }

func (h *teamAwarenessTaskStart) Name() string                { return "team-awareness-task-start" }
func (h *teamAwarenessTaskStart) Phase() hook.AgentHookPhase  { return hook.PhaseTaskStart }
func (h *teamAwarenessTaskStart) Priority() int               { return 500 }

// Run 在任务入口触发。重试任务（RetryCount > 0）跳过——因为 LastHistory
// 恢复时已包含上次任务结束时的快照，重复注入会造成误导性的时间戳。
// 这精确复刻了原 agent.go 硬编码 `task.RetryCount == 0 && a.TeamSnapshot != nil`
// 的行为边界。
func (h *teamAwarenessTaskStart) Run(hctx hook.AgentHookContext) hook.AgentHookResult {
	if hctx.Store != nil {
		if task, ok := hctx.Store.GetTask(hctx.TaskID); ok && task.RetryCount > 0 {
			return hook.AgentHookResult{}
		}
	}
	// 首次注入：所有启用的 section 都渲染。LoopIndex=-1 在 renderSelective
	// 内部会被 GoalAnchor 渲染成 "当前轮次: 0" 风格的首轮语义。
	content := h.core.renderSelective(
		hctx,
		h.core.cfg.SnapshotFn != nil,
		h.core.cfg.FileEnabled,
		h.core.cfg.GoalEnabled,
	)
	if content == "" {
		return hook.AgentHookResult{}
	}
	return hook.AgentHookResult{InjectContent: content}
}

// ---- LoopPre 子 hook ----

type teamAwarenessLoopPre struct{ core *teamAwarenessCore }

func (h *teamAwarenessLoopPre) Name() string                { return "team-awareness-loop-pre" }
func (h *teamAwarenessLoopPre) Phase() hook.AgentHookPhase  { return hook.PhaseLoopPre }
func (h *teamAwarenessLoopPre) Priority() int               { return 500 }

// Run 在每轮 ReactLoop 顶部触发。LoopIndex=0 时跳过——首轮注入由
// teamAwarenessTaskStart 负责，避免双重注入。后续按各 section 独立频率刷新。
func (h *teamAwarenessLoopPre) Run(hctx hook.AgentHookContext) hook.AgentHookResult {
	if hctx.LoopIndex == 0 {
		return hook.AgentHookResult{}
	}
	cfg := h.core.cfg

	// 方案乙：每个 section 独立判断是否刷新
	wantTeam := false
	wantFile := false
	wantGoal := false

	// Team section: 周期性 OR 强制刷新（收到新消息）
	if cfg.SnapshotFn != nil {
		if hctx.HasNewMail && cfg.ForceOnMail {
			wantTeam = true
		} else if cfg.SnapshotRefreshInterval > 0 && hctx.LoopIndex%cfg.SnapshotRefreshInterval == 0 {
			wantTeam = true
		}
	}

	// File section: 与 Team 共享触发条件（v3 §7.4.3 建议）
	if cfg.FileEnabled && wantTeam {
		wantFile = true
	}

	// Goal section: 独立频率，不受 ForceOnMail 影响
	if cfg.GoalEnabled {
		if cfg.GoalRefreshInterval > 0 && hctx.LoopIndex%cfg.GoalRefreshInterval == 0 {
			wantGoal = true
		}
	}

	if !wantTeam && !wantFile && !wantGoal {
		return hook.AgentHookResult{}
	}
	content := h.core.renderSelective(hctx, wantTeam, wantFile, wantGoal)
	if content == "" {
		return hook.AgentHookResult{}
	}
	return hook.AgentHookResult{InjectContent: content}
}

// ---- 渲染 + 预算截断 ----

// renderSelective 按三个 bool 决定渲染哪些 section，并做 token 预算截断。
// 返回的字符串是可以直接注入 history 的完整内容（多段间用空行分隔）。
func (c *teamAwarenessCore) renderSelective(hctx hook.AgentHookContext, wantTeam, wantFile, wantGoal bool) string {
	var goalStr, teamStr, fileStr string

	if wantGoal && c.cfg.GoalEnabled {
		goalStr = c.renderGoalAnchor(hctx)
	}
	if wantTeam && c.cfg.SnapshotFn != nil {
		teamStr = c.cfg.SnapshotFn(hctx.AgentID)
	}
	if wantFile && c.cfg.FileEnabled && hctx.Roster != nil {
		fileStr = c.renderFileAwareness(hctx)
	}

	return c.assembleWithinBudget(goalStr, teamStr, fileStr)
}

// assembleWithinBudget 按优先级 goal > team > file 逐步丢弃直至满足预算。
// 预算：MaxTokens * 2（1 token ≈ 2 runes，保守估算）。
func (c *teamAwarenessCore) assembleWithinBudget(goal, team, file string) string {
	maxRunes := c.cfg.MaxTokens * 2

	// 优先级 1：三段全部保留
	combined := joinSections(goal, team, file)
	if runeLen(combined) <= maxRunes {
		return combined
	}

	// 优先级 2：丢 file
	combined = joinSections(goal, team, "")
	if runeLen(combined) <= maxRunes {
		return combined
	}

	// 优先级 3：丢 team（只保留 goal）
	combined = joinSections(goal, "", "")
	if runeLen(combined) <= maxRunes {
		return combined
	}

	// 优先级 4：goal 自身也超预算，硬截断
	runes := []rune(combined)
	if len(runes) > maxRunes {
		combined = string(runes[:maxRunes]) + "\n<!-- truncated -->"
	}
	return combined
}

// joinSections 用空行拼接非空 section，自动 trim 空白。
func joinSections(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// runeLen 用 rune 数替代 byte 数作为长度估算，对中文场景更友好。
func runeLen(s string) int {
	return len([]rune(s))
}

// ---- Section 1: TeamSnapshot ----
// 通过 cfg.SnapshotFn 委托给 worker.BuildTeamSnapshot，核心逻辑不在本文件。

// ---- Section 2: FileAwareness ----

// renderFileAwareness 读取 Roster 的文件占用快照，按 agent 分组展示。
// 当前 agent 的占用用 "你" 前缀标记，其他 agent 用 "队友" 语义。
func (c *teamAwarenessCore) renderFileAwareness(hctx hook.AgentHookContext) string {
	if hctx.Roster == nil {
		return ""
	}
	claims := hctx.Roster.ListClaims()
	if len(claims) == 0 {
		return ""
	}

	// 按 agentID 排序，输出稳定（便于测试断言）
	ids := make([]string, 0, len(claims))
	for id := range claims {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var sb strings.Builder
	sb.WriteString("<file-awareness>\n")
	for _, agentID := range ids {
		files := claims[agentID]
		if len(files) == 0 {
			continue
		}
		// 排序文件名使测试断言稳定
		sorted := make([]string, len(files))
		copy(sorted, files)
		sort.Strings(sorted)
		if agentID == hctx.AgentID {
			fmt.Fprintf(&sb, "  - 你（%s）已占用: %s\n", agentID, strings.Join(sorted, ", "))
		} else {
			fmt.Fprintf(&sb, "  - %s 正在修改: %s\n", agentID, strings.Join(sorted, ", "))
		}
	}
	sb.WriteString("</file-awareness>")
	return sb.String()
}

// ---- Section 3: GoalAnchor ----

// renderGoalAnchor 组装目标锚点。完全机械拼接——不调用 LLM，数据全部从
// Store 已有字段提取。设计约束详见 nextUpgrade_v3.md §8.7：
//  1. 原始目标始终在第一行
//  2. 工具轨迹只取最近 N 条，只保留 toolName + filepath.Base(path)
//  3. 不让 LLM 自行更新目标（整段完全由代码生成）
func (c *teamAwarenessCore) renderGoalAnchor(hctx hook.AgentHookContext) string {
	if hctx.Store == nil {
		return ""
	}
	task, ok := hctx.Store.GetTask(hctx.TaskID)
	if !ok {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<goal-anchor>\n")
	fmt.Fprintf(&sb, "任务目标: %s\n", task.Description)

	// 轮次显示：TaskStart 阶段 LoopIndex == -1，显示为 0
	loopDisplay := hctx.LoopIndex
	if loopDisplay < 0 {
		loopDisplay = 0
	}
	fmt.Fprintf(&sb, "当前轮次: %d\n", loopDisplay)

	if len(task.Artifacts) > 0 {
		fmt.Fprintf(&sb, "已写入文件: %s\n", strings.Join(task.Artifacts, ", "))
	}

	// 最近工具调用
	records := hctx.Store.GetToolCallHistory(hctx.TaskID)
	if len(records) > 0 {
		window := c.cfg.RecentToolsWindow
		start := 0
		if len(records) > window {
			start = len(records) - window
		}
		recent := records[start:]
		parts := make([]string, 0, len(recent))
		for _, r := range recent {
			label := r.ToolName
			if path, pathOK := r.Args["path"].(string); pathOK && path != "" {
				label = fmt.Sprintf("%s(%s)", r.ToolName, filepath.Base(path))
			}
			parts = append(parts, label)
		}
		fmt.Fprintf(&sb, "最近操作: %s\n", strings.Join(parts, " → "))
	}

	sb.WriteString("请在本轮操作中核对：你的下一步行动是否仍然服务于\"任务目标\"？\n")
	sb.WriteString("</goal-anchor>")
	return sb.String()
}
