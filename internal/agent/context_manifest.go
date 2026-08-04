// context_manifest.go 实现 V6 §3 的 Context Manifest（CM1）：每次 LLM 调用前，
// 对即将进入上下文窗口的各段内容生成影子账本（shadow ledger），记录来源、
// 作用域、权威等级、新鲜度、内容摘要（digest）、token 估算与裁剪/压缩处置。
//
// CM1 纯观测：不改变任何消息内容与装配顺序——buildMessages 的产物字节级不变，
// Manifest 只是对同一份输入的旁路描述。
//
// 装配优先级（docs/nextUpgrade-V6.md §3 第 2 条，由高到低）：
//  1. 安全与控制契约：system_prompt / task_context / validation_feedback / tools_schema
//     （authority=authoritative——系统与任务契约，LLM 必须服从）
//  2. 当前任务与有效约束：task_desc（user-input 工作契约）
//  3. 工具协议：tools_schema（与第 1 层同为 authoritative，但属于独立消息通道）
//  4. 历史 / Memory / 外部资料：history / dep_results / dep_task_memory /
//     memory_* / mailbox / injected_segment（informational 或 untrusted）
//
// 权威等级口径：
//   - authoritative：控制面与契约内容（system prompt、task-context、任务描述、
//     校验反馈、工具协议）；
//   - informational：参考性内容（依赖结果、上游 Task Memory、历史、Memory 快照、
//     静态团队感知、未识别注入段）；
//   - untrusted：代理间邮件——外部实体投递，可能携带注入尝试，不可当作指令服从。
//
// 新鲜度口径：
//   - live：本次任务执行期内产生或持续刷新的内容；
//   - snapshot：认领时刻（或上个 attempt）的时点快照（dep 结果、dep Task
//     Memory、无法判定时间的 Memory 段）；
//   - stale：明确早于当前任务开始时间的内容（Memory 段按 Entry.UpdatedAt 与
//     任务开始时间比较判定）。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/model"
)

// --- 段 ID 词表（稳定标识，trace 摘要与测试断言共用） ---
const (
	ManifestSectionSystemPrompt = "system_prompt"
	ManifestSectionTaskContext  = "task_context"
	ManifestSectionTaskDesc     = "task_desc"
	ManifestSectionDepResults   = "dep_results"
	// ManifestSectionDepTaskMemory 是 V6 CM4 的依赖任务 Task Memory 交接段
	// （<dep-task-memory> 注入块；Source=task-memory，informational，snapshot）。
	ManifestSectionDepTaskMemory       = "dep_task_memory"
	ManifestSectionHistory             = "history"
	ManifestSectionMailbox             = "mailbox"
	ManifestSectionMemoryTeamSnapshot  = "memory_team_snapshot"
	ManifestSectionMemoryFileAwareness = "memory_file_awareness"
	// ManifestSectionTaskMemory 是 V6 §3 CM2 的 Task Memory 注入段
	// （有界渲染，紧随 user 首条；Source=task-memory，informational）。
	ManifestSectionTaskMemory = "task_memory"
	// ManifestSectionSessionMemory 是 V6 §3 CM3 的 Session Memory 召回注入段
	// （任务入口一次，经 IncomingMail 进 history；Source=session-memory，
	// informational，Freshness 按命中条目的最新 UpdatedAt 判定）。
	ManifestSectionSessionMemory = "session_memory"
	// ManifestSectionBudgetWarning 预留：V6 当前不存在预算警告注入点（v4 时代
	// 的预算注入已随预算系统删除），词表占位以保持文档 §3 段清单完整。
	ManifestSectionBudgetWarning      = "budget_warning"
	ManifestSectionValidationFeedback = "validation_feedback"
	ManifestSectionToolsSchema        = "tools_schema"
	// ManifestSectionInjectedSegment 收纳未识别的 IncomingMail 注入段
	// （scheduler 的 board snapshot JSON 等）。
	ManifestSectionInjectedSegment = "injected_segment"
	// ManifestSectionTeamAwareness 是 executor 构造期注入的静态团队感知文本
	// （当前生产装配恒为空串，非空才登记）。
	ManifestSectionTeamAwareness = "team_awareness"
)

// --- Source / Scope / Authority / Freshness / Disposition 词表 ---
const (
	SourceControlPlane = "control-plane"
	SourceAgentPrompt  = "agent-prompt"
	SourceUserInput    = "user-input"
	SourceDependency   = "dependency"
	SourceMailbox      = "mailbox"
	SourceMemory       = "memory"
	SourceHistory      = "history"
	SourceTools        = "tools"
	// SourceTaskMemory 是 Task Memory（CM2）注入段的来源标注。
	SourceTaskMemory = "task-memory"
	// SourceSessionMemory 是 Session Memory（CM3）召回注入段的来源标注。
	SourceSessionMemory = "session-memory"
)

const (
	ScopeSystem  = "system"
	ScopeTask    = "task"
	ScopeProcess = "process"
	ScopeSession = "session"
)

const (
	AuthorityAuthoritative = "authoritative"
	AuthorityInformational = "informational"
	AuthorityUntrusted     = "untrusted"
)

const (
	FreshnessLive     = "live"
	FreshnessSnapshot = "snapshot"
	FreshnessStale    = "stale"
)

const (
	DispositionIncluded   = "included"
	DispositionCompressed = "compressed"
	DispositionSnipped    = "snipped"
	DispositionTruncated  = "truncated"
	// DispositionDroppedPrefix 是 "dropped:<原因>" 形态的前缀；CM1 装配路径
	// 不丢段，预留供后续裁剪策略使用。
	DispositionDroppedPrefix = "dropped:"
)

// ContextItem 是 Manifest 中一个上下文段的观测记录。正文永不落账本——
// 只有 sha256 前 12 位 digest 与 token 估算。
type ContextItem struct {
	ID          string // 稳定段 ID（见上方词表）
	Source      string // control-plane / agent-prompt / user-input / dependency / mailbox / memory / history / tools
	Scope       string // system / task / process / session
	Authority   string // authoritative / informational / untrusted
	Freshness   string // live / snapshot / stale
	Digest      string // 内容 sha256 前 12 位（hex）
	Tokens      int    // 估算 token 数（rune/3，与 diagnoseLLMError 的历史估算口径一致）
	Disposition string // included / compressed[:strategy] / snipped / truncated / dropped:<原因>
	// Count 是段的条目计数：history=历史条数，tools_schema=工具数，
	// 其余同 ID 合并登记的段=合并次数（通常为 1）。
	Count int `json:"count,omitempty"`
}

// ContextManifest 是一轮 LLM 调用的完整上下文影子账本（Seal 后不可变）。
type ContextManifest struct {
	TaskID               string
	Loop                 int
	Items                []ContextItem
	TotalEstimatedTokens int
}

// manifestItemSummary 是 trace 事件 Description 承载的单段摘要（不含正文）。
type manifestItemSummary struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Authority   string `json:"authority"`
	Freshness   string `json:"freshness"`
	Tokens      int    `json:"tokens"`
	Disposition string `json:"disposition"`
	Count       int    `json:"count,omitempty"`
}

// SummaryJSON 生成 trace.KindContextManifestBuilt 事件 Description 用的
// 紧凑 JSON 摘要：逐段 ID/Source/Authority/Freshness/Tokens/Disposition/Count。
func (m ContextManifest) SummaryJSON() string {
	summaries := make([]manifestItemSummary, 0, len(m.Items))
	for _, item := range m.Items {
		summaries = append(summaries, manifestItemSummary{
			ID:          item.ID,
			Source:      item.Source,
			Authority:   item.Authority,
			Freshness:   item.Freshness,
			Tokens:      item.Tokens,
			Disposition: item.Disposition,
			Count:       item.Count,
		})
	}
	data, err := json.Marshal(summaries)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// manifestItemAccum 是 Builder 内部的可变累加器。同 ID 段重复登记时合并：
// 增量内容追加到 buf，digest 在 Seal 时对全部内容的连接统一计算。
type manifestItemAccum struct {
	item ContextItem
	buf  []string
}

// ManifestBuilder 逐项登记上下文段并 Seal 出不可变 Manifest。
// 同 ID 段允许重复 Register（合并：digest 基于全部内容的连接、Tokens 累加、
// Count 累加、其余元数据以最后一次登记为准——同 ID 段同源同属性，最后一次
// 登记携带最新 freshness/disposition）。
type ManifestBuilder struct {
	taskID string
	loop   int
	items  []*manifestItemAccum
	byID   map[string]*manifestItemAccum
	sealed bool
}

// NewManifestBuilder 创建一轮 LLM 调用的 Manifest 构建器。
func NewManifestBuilder(taskID string, loop int) *ManifestBuilder {
	return &ManifestBuilder{
		taskID: taskID,
		loop:   loop,
		byID:   make(map[string]*manifestItemAccum),
	}
}

// estimateManifestTokens 按 rune/3 估算 token 数（与 diagnoseLLMError 对历史
// 的 len/3 估算同量级口径；中文按 rune 计更贴近真实 token 消耗）。
func estimateManifestTokens(s string) int {
	return len([]rune(s)) / 3
}

// manifestDigest 计算内容 sha256 的前 12 位 hex。多段内容按顺序连接后计算。
func manifestDigest(contents ...string) string {
	h := sha256.New()
	for _, c := range contents {
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Register 登记一个上下文段。contents 只参与派生 digest/tokens，不进入
// Manifest（正文不落账本）。Seal 之后调用静默忽略——观测通道绝不 panic
// 主流程，登记遗漏只影响账本完整性。
func (b *ManifestBuilder) Register(id, source, scope, authority, freshness, disposition string, contents ...string) {
	if b.sealed {
		return
	}
	tokens := 0
	for _, c := range contents {
		tokens += estimateManifestTokens(c)
	}
	if acc, ok := b.byID[id]; ok {
		// 同 ID 合并：增量内容追加缓冲，Seal 时对全部内容统一计算 digest
		//（段数量级小，内存代价可忽略）。
		acc.buf = append(acc.buf, contents...)
		acc.item.Tokens += tokens
		acc.item.Count++
		acc.item.Source = source
		acc.item.Scope = scope
		acc.item.Authority = authority
		acc.item.Freshness = freshness
		acc.item.Disposition = disposition
		return
	}
	acc := &manifestItemAccum{
		item: ContextItem{
			ID:          id,
			Source:      source,
			Scope:       scope,
			Authority:   authority,
			Freshness:   freshness,
			Tokens:      tokens,
			Disposition: disposition,
			Count:       1,
		},
		buf: append([]string(nil), contents...),
	}
	b.items = append(b.items, acc)
	b.byID[id] = acc
}

// Seal 生成不可变 Manifest：计算各段 digest、汇总 token 总量，并把内部
// 累加器与返回值解耦（之后修改返回值不影响 Builder；Seal 幂等）。
func (b *ManifestBuilder) Seal() ContextManifest {
	items := make([]ContextItem, 0, len(b.items))
	total := 0
	for _, acc := range b.items {
		item := acc.item
		item.Digest = manifestDigest(acc.buf...)
		items = append(items, item)
		total += item.Tokens
	}
	b.sealed = true
	return ContextManifest{
		TaskID:               b.taskID,
		Loop:                 b.loop,
		Items:                items,
		TotalEstimatedTokens: total,
	}
}

// --- 侧信息载体（processTask → executor，经 ctx 单向传递） ---

// manifestSideInfo 是 processTask 每个 attempt 创建一份的侧信息：executor
// 构建 Manifest 时需要的、无法从消息内容本身推导的事实——Memory 段的
// Entry.UpdatedAt（新鲜度判定）与本 attempt 内已发生的压缩处置。
// 写方（processTask / injectMemoryContext / handleFailure）与读方（executor）
// 同在 ReAct 循环 goroutine 内串行执行，无需加锁。
type manifestSideInfo struct {
	taskStartedAt   time.Time
	memoryUpdatedAt map[string]time.Time // "<team-snapshot>" / "<file-awareness>" → Entry.UpdatedAt
	l2Strategy      string               // 非空 = 本 attempt 已发生 L2 摘要压缩（值=strategy）
	l3Truncated     bool                 // 本 attempt 内 handleFailure 溢出分支已做 L3 激进压缩
	// depTaskMemDropped 非空 = 任务有依赖但 dep Task Memory 交接注入被跳过
	// （store 未装配 / 加载失败），Manifest 登记 dep_task_memory dropped:<原因>。
	depTaskMemDropped string
}

func newManifestSideInfo(taskStartedAt time.Time) *manifestSideInfo {
	return &manifestSideInfo{
		taskStartedAt:   taskStartedAt,
		memoryUpdatedAt: make(map[string]time.Time),
	}
}

// withManifestSideInfo 把侧信息挂到 ctx（processTask 任务入口调用一次，
// 之后每轮 loop 派生的 execCtx 共享同一指针）。
func withManifestSideInfo(ctx context.Context, info *manifestSideInfo) context.Context {
	return context.WithValue(ctx, ctxManifestSideInfo, info)
}

func manifestSideInfoFromContext(ctx context.Context) *manifestSideInfo {
	info, _ := ctx.Value(ctxManifestSideInfo).(*manifestSideInfo)
	return info
}

// recordMemorySectionUpdatedAt 记录某 Memory 段本次注入所用 Entry 的
// UpdatedAt，供 executor 判定 freshness。nil-safe。
func (m *manifestSideInfo) recordMemorySectionUpdatedAt(section string, updatedAt time.Time) {
	if m == nil || updatedAt.IsZero() {
		return
	}
	m.memoryUpdatedAt[section] = updatedAt
}

// memoryFreshness 按 Entry.UpdatedAt 与任务开始时间比较判定 Memory 段新鲜度：
// 任务开始后写入/刷新 = live；早于任务开始 = stale；无时间信息 = snapshot。
func (m *manifestSideInfo) memoryFreshness(section string) string {
	if m == nil {
		return FreshnessSnapshot
	}
	updatedAt, ok := m.memoryUpdatedAt[section]
	if !ok || updatedAt.IsZero() {
		return FreshnessSnapshot
	}
	if !updatedAt.Before(m.taskStartedAt) {
		return FreshnessLive
	}
	return FreshnessStale
}

// --- 装配（executor 调用点） ---

// 注入段识别标记（与产生处的字面量保持一致）：
//   - task_memory.go "<dep-task-memory>"（CM4 依赖任务 Task Memory 交接）
//   - agent.go "<validation-feedback>"（ExpectedArtifacts 校验反馈）
//   - agent.go "<agent-mail"（代理间邮件）
//   - team_snapshot.go:72 "<team-snapshot>" / memory_context.go:183 "<file-awareness>"
//   - session_memory.go "<session-memory"（CM3 Session Memory 召回注入）
//   - agent.go snipStub 墓碑前缀 "[已清空]"（L1 已清理的工具结果）
//   - agent.go buildHistorySummary 摘要前缀 "=== 历史摘要 ==="（L2/L3 压缩产物）
const (
	markerDepTaskMemory      = "<dep-task-memory>"
	markerValidationFeedback = "<validation-feedback>"
	markerAgentMail          = "<agent-mail"
	markerTeamSnapshot       = "<team-snapshot>"
	markerFileAwareness      = "<file-awareness>"
	markerSessionMemory      = "<session-memory"
	markerSnipTombstone      = "[已清空]"
	markerHistorySummary     = "=== 历史摘要 ==="
)

// buildContextManifest 在 LLM 调用前对同一份装配输入生成影子账本。
// 这是 CM1 的唯一装配入口：逐项 Register 后 Seal。不改变任何消息内容。
func buildContextManifest(
	ctx context.Context,
	effectivePrompt string,
	task *model.Task,
	depResults map[string]string,
	history []HistoryEntry,
	teamAwareness string,
	toolDefs []llm.ToolDef,
) ContextManifest {
	side := manifestSideInfoFromContext(ctx)
	loop, _ := ctx.Value(ctxLoopNum).(int)
	b := NewManifestBuilder(task.ID, loop)

	// 第 1 层：安全与控制契约。
	if effectivePrompt != "" {
		// 任务级 SystemPrompt 覆盖时，内容来自任务发布方（控制面）而非
		// agent kind 的静态配置，Source 随之切换以反映真实来源。
		source := SourceAgentPrompt
		if task.SystemPrompt != "" {
			source = SourceControlPlane
		}
		b.Register(ManifestSectionSystemPrompt, source, ScopeProcess,
			AuthorityAuthoritative, FreshnessLive, DispositionIncluded, effectivePrompt)
	}
	if teamAwareness != "" {
		b.Register(ManifestSectionTeamAwareness, SourceAgentPrompt, ScopeProcess,
			AuthorityInformational, FreshnessLive, DispositionIncluded, teamAwareness)
	}
	b.Register(ManifestSectionTaskContext, SourceControlPlane, ScopeTask,
		AuthorityAuthoritative, FreshnessLive, DispositionIncluded, renderTaskContextBlock(task))

	// 第 2 层：当前任务与有效约束。
	b.Register(ManifestSectionTaskDesc, SourceUserInput, ScopeTask,
		AuthorityAuthoritative, FreshnessLive, DispositionIncluded, task.Description)

	// CM2：Task Memory 段（紧随任务契约注入；滚动工作状态，informational）。
	// 载体降级时记 dropped:<原因>（无正文）。
	if carrier := taskMemCarrierFromContext(ctx); carrier != nil {
		if disposition := taskMemManifestDisposition(carrier); disposition != "" {
			b.Register(ManifestSectionTaskMemory, SourceTaskMemory, ScopeTask,
				AuthorityInformational, FreshnessLive, disposition, carrier.text)
		}
	}

	// 第 4 层前段：依赖结果（认领时刻快照）。
	if len(depResults) > 0 {
		b.Register(ManifestSectionDepResults, SourceDependency, ScopeTask,
			AuthorityInformational, FreshnessSnapshot, DispositionIncluded,
			renderDepResultsCanonical(depResults))
		if item := b.byID[ManifestSectionDepResults]; item != nil {
			item.item.Count = len(depResults)
		}
	}

	// 历史流：按 history 切片顺序逐项分类。注入段（IncomingMail 非空）各自
	// 成项；ReAct 主体条目聚合为单个 history 项（条数 + 总 digest）。
	var bodyParts []string
	bodyEntries := 0
	summaryDetected := false
	tombstoneDetected := false
	for i := range history {
		entry := &history[i]
		if entry.IncomingMail != "" {
			classifyInjection(b, entry.IncomingMail, side)
			continue
		}
		bodyEntries++
		if !summaryDetected && !entry.ToolCalled && strings.HasPrefix(entry.Output, markerHistorySummary) {
			// 压缩摘要条目（compressHistory 产物）。从 LastHistory 恢复时
			// 无法区分 L2/L3 来源，统一按 compressed 记录（见 disposition 注释）。
			summaryDetected = true
		}
		if entry.ToolCalled && len(entry.ToolCalls) > 0 {
			bodyParts = append(bodyParts, entry.AssistantContent)
			if calls, err := json.Marshal(entry.ToolCalls); err == nil {
				bodyParts = append(bodyParts, string(calls))
			}
			for _, tr := range entry.ToolResults {
				if strings.HasPrefix(tr.Content, markerSnipTombstone) {
					tombstoneDetected = true
				}
				bodyParts = append(bodyParts, tr.Content)
			}
		} else {
			bodyParts = append(bodyParts, entry.Output)
		}
		if len(entry.ExtraFields) > 0 {
			if extra, err := json.Marshal(entry.ExtraFields); err == nil {
				bodyParts = append(bodyParts, string(extra))
			}
		}
	}
	if bodyEntries > 0 {
		b.Register(ManifestSectionHistory, SourceHistory, ScopeTask,
			AuthorityInformational, FreshnessLive,
			historyDisposition(side, summaryDetected, tombstoneDetected), bodyParts...)
		if item := b.byID[ManifestSectionHistory]; item != nil {
			item.item.Count = bodyEntries
		}
	}

	// CM4：dep Task Memory 降级登记——任务有依赖但交接注入被跳过
	// （store 未装配 / 加载失败）时记 dropped:<原因>（无正文）；注入成功时
	// 本段已被 classifyInjection 登记，此处不重复。
	if side != nil && side.depTaskMemDropped != "" {
		if _, ok := b.byID[ManifestSectionDepTaskMemory]; !ok {
			b.Register(ManifestSectionDepTaskMemory, SourceTaskMemory, ScopeTask,
				AuthorityInformational, FreshnessSnapshot,
				DispositionDroppedPrefix+side.depTaskMemDropped)
		}
	}

	// 第 3 层：工具协议（独立消息通道）。
	if len(toolDefs) > 0 {
		schemaJSON := "[]"
		if data, err := json.Marshal(toolDefs); err == nil {
			schemaJSON = string(data)
		}
		b.Register(ManifestSectionToolsSchema, SourceTools, ScopeProcess,
			AuthorityAuthoritative, FreshnessLive, DispositionIncluded, schemaJSON)
		if item := b.byID[ManifestSectionToolsSchema]; item != nil {
			item.item.Count = len(toolDefs)
		}
	}

	return b.Seal()
}

// classifyInjection 把一条 IncomingMail 历史条目按内容标记分类登记到 Builder。
func classifyInjection(b *ManifestBuilder, content string, side *manifestSideInfo) {
	switch {
	case strings.Contains(content, markerDepTaskMemory):
		// CM4：依赖任务 Task Memory 交接块——认领时刻的快照。
		b.Register(ManifestSectionDepTaskMemory, SourceTaskMemory, ScopeTask,
			AuthorityInformational, FreshnessSnapshot, DispositionIncluded, content)
	case strings.Contains(content, markerValidationFeedback):
		b.Register(ManifestSectionValidationFeedback, SourceControlPlane, ScopeTask,
			AuthorityAuthoritative, FreshnessLive, DispositionIncluded, content)
	case strings.Contains(content, markerAgentMail):
		b.Register(ManifestSectionMailbox, SourceMailbox, ScopeTask,
			AuthorityUntrusted, FreshnessLive, DispositionIncluded, content)
	case strings.Contains(content, markerTeamSnapshot), strings.Contains(content, markerFileAwareness):
		teamPart, filePart := splitMemorySections(content)
		if teamPart != "" {
			b.Register(ManifestSectionMemoryTeamSnapshot, SourceMemory, ScopeProcess,
				AuthorityInformational, side.memoryFreshness(markerTeamSnapshot), DispositionIncluded, teamPart)
		}
		if filePart != "" {
			b.Register(ManifestSectionMemoryFileAwareness, SourceMemory, ScopeProcess,
				AuthorityInformational, side.memoryFreshness(markerFileAwareness), DispositionIncluded, filePart)
		}
	case strings.Contains(content, markerSessionMemory):
		// CM3 Session Memory 召回注入：session 作用域、informational；
		// freshness 按命中条目的最新 UpdatedAt（侧信息由 recallSessionMemory 记录）。
		b.Register(ManifestSectionSessionMemory, SourceSessionMemory, ScopeSession,
			AuthorityInformational, side.memoryFreshness(markerSessionMemory), DispositionIncluded, content)
	default:
		// 未识别注入（scheduler board snapshot 等）：系统侧注入，按
		// informational 记录。
		b.Register(ManifestSectionInjectedSegment, SourceControlPlane, ScopeTask,
			AuthorityInformational, FreshnessLive, DispositionIncluded, content)
	}
}

// splitMemorySections 把 joinSections 合并的 Memory 注入文本拆回两个段
// （team_snapshot 在前、file_awareness 在后，见 injectMemoryContext）。
// 只出现一个段时整段归该段。
func splitMemorySections(content string) (teamPart, filePart string) {
	teamIdx := strings.Index(content, markerTeamSnapshot)
	fileIdx := strings.Index(content, markerFileAwareness)
	switch {
	case teamIdx >= 0 && fileIdx > teamIdx:
		return strings.TrimSpace(content[:fileIdx]), strings.TrimSpace(content[fileIdx:])
	case teamIdx >= 0:
		return content, ""
	case fileIdx >= 0:
		return "", content
	}
	return "", ""
}

// historyDisposition 判定 history 主体项的处置。优先级：truncated > compressed
// > snipped > included（取本轮上下文经历过的最强压缩形态）。
//   - L3：handleFailure 溢出分支置 side.l3Truncated，同 attempt 随后的
//     LLM 调用的 Manifest 可见；
//   - L2：循环内触发后置 side.l2Strategy（strategy 一并记录）；
//   - 跨 attempt 恢复（LastHistory 内含摘要条目）：内容检测只能确认"压缩过"，
//     无法区分 L2/L3，统一记 compressed；
//   - L1：墓碑内容检测（snipOldToolResults 每轮跑，墓碑持久留在历史里）。
func historyDisposition(side *manifestSideInfo, summaryDetected, tombstoneDetected bool) string {
	switch {
	case side != nil && side.l3Truncated:
		return DispositionTruncated
	case side != nil && side.l2Strategy != "":
		return DispositionCompressed + ":" + side.l2Strategy
	case summaryDetected:
		return DispositionCompressed
	case tombstoneDetected:
		return DispositionSnipped
	default:
		return DispositionIncluded
	}
}

// renderTaskContextBlock 渲染 <task-context> 控制面块。与 buildMessages
// 共用同一实现，保证 Manifest digest 与消息内容字节级一致。
func renderTaskContextBlock(task *model.Task) string {
	var sb strings.Builder
	if task.GraphID != "" {
		// 图任务追加 graph_id / node_id / activation_id（V6 Graph 路由语境）。
		sb.WriteString("<task-context source=\"control-plane\">\n")
		sb.WriteString("task_id: " + task.ID + "\n")
		sb.WriteString("graph_id: " + task.GraphID + "\n")
		sb.WriteString("node_id: " + task.NodeID + "\n")
		sb.WriteString("activation_id: " + task.ActivationID + "\n")
		sb.WriteString("</task-context>\n")
		return sb.String()
	}
	return "<task-context source=\"control-plane\">\ntask_id: " + task.ID + "\n</task-context>\n"
}

// renderDepResultsCanonical 渲染依赖结果的规范形式（按 depID 排序）。
// buildMessages 直接 range map（迭代序随机），Manifest digest 不能与消息字节
// 序绑定，故对排序后的规范形式计算——同一份 depResults 的 digest 跨轮稳定。
func renderDepResultsCanonical(depResults map[string]string) string {
	keys := make([]string, 0, len(depResults))
	for depID := range depResults {
		keys = append(keys, depID)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, depID := range keys {
		sb.WriteString("[" + depID + "] " + depResults[depID] + "\n")
	}
	return sb.String()
}
