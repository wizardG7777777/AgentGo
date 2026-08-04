package scheduler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

const (
	// Task result bodies are cold data. The hot board carries a small, shared
	// excerpt budget plus stable metadata; Scheduler can fetch the exact body
	// through get_task_result when a decision genuinely needs it.
	maxTaskResultExcerptRunes       = 768
	maxBoardResultExcerptTotalRunes = 4096
	maxTaskProgressTailRunes        = 1000
	maxTaskLastResponseRunes        = 1200
)

// boardSnapshot 是 scheduler agent 在每轮 reactLoop 看到的当前控制域 JSON 结构。
//
// Phase 3 初版只含 mode/trigger/tasks/resources 四段。Phase 3.1 扩展：
//   - resources.Agents 改为完整代理列表（含 mailbox 待处理数 + 当前认领任务）
//   - 新增顶层 SessionHistory 字段（本会话用户输入历史）
//
// v5 热快照不再内联无界 Results / PartialOutput：终态正文通过
// result_refs 按需分页读取，执行中输出只保留有界 progress tail。
//
// 两轴模式字段（exec / topo）总是出现（非 omitempty）。
type boardSnapshot struct {
	ExecMode               string                      `json:"exec_mode"`
	TopoMode               string                      `json:"topo_mode"`
	Trigger                triggerInfo                 `json:"trigger"`
	Tasks                  []taskSnapshot              `json:"tasks"`
	Resources              resourceInfo                `json:"resources"`
	SessionHistory         []sessionEntry              `json:"session_history,omitempty"`
	PendingDownstreamTasks []pendingDownstreamSnapshot `json:"pending_downstream_tasks,omitempty"`
}

// pendingDownstreamSnapshot 是 board snapshot 中"下游待处理任务"的一行。
// 当 reactor 触发了依赖当前 batch 的额外任务（如 verifier）时，
// SchedulerExecutor 会把这些任务注入 snapshot，让 LLM 决定是否汇报进度。
type pendingDownstreamSnapshot struct {
	TaskID      string `json:"task_id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	AgentID     string `json:"agent_id,omitempty"`
}

type resourceInfo struct {
	RuntimeMode       string                     `json:"runtime_mode"`
	WorkerCount       int                        `json:"worker_count"`
	BusyWorkers       int                        `json:"busy_workers"`
	AvailableWorkers  int                        `json:"available_workers"`
	UnavailableTools  []string                   `json:"unavailable_tools,omitempty"`
	Agents            []agentSnapshot            `json:"agents,omitempty"`
	SpecializedAgents []specializedAgentSnapshot `json:"specialized_agents,omitempty"`
	AgentCapabilities []agentCapabilitySnapshot  `json:"agent_capabilities,omitempty"`
	AgentTemplates    []agenttemplate.Summary    `json:"agent_templates,omitempty"`
}

// specializedAgentSnapshot 是 board snapshot 里"特化代理聚合视图"的一行。
// 与 agentSnapshot（per-instance）互补：agentSnapshot 展示每个代理的运行时
// 状态，specializedAgentSnapshot 展示"这一类代理的总数、忙碌数、能力描述"，
// 让 scheduler LLM 在任务规划时能回答"我有没有资源处理这类任务"。
//
// 数据来源：
//   - EventType / Count / Role 来自静态 AgentRegistry（bootstrap 注入）
//   - Busy 来自 live task 扫描——统计 EventType 匹配且 Status=processing 的任务数
//
// 本结构不暴露 package 外（仅 JSON 序列化给 LLM）。
type specializedAgentSnapshot struct {
	EventType string `json:"event_type"`
	Count     int    `json:"count"`
	Busy      int    `json:"busy"`
	Role      string `json:"role,omitempty"`
}

// agentCapabilitySnapshot 是 board snapshot 中"代理能力声明"的一行。
// 每种代理类型（worker / explorer / 其他特化代理）各一条记录，
// 让 Scheduler LLM 在路由决策时能基于结构化的能力标签选择 event_type。
type agentCapabilitySnapshot struct {
	AgentType    string   `json:"agent_type"`
	Profile      string   `json:"profile,omitempty"`
	Capabilities []string `json:"capabilities"`
	Description  string   `json:"description"`
}

// AgentCapabilityInfo 是传入 snapshot 构建的能力信息载体。
// Worker 不在 AgentRegistry 中注册，其能力声明通过此结构体单独传入。
type AgentCapabilityInfo struct {
	Capabilities []string
	Description  string
}

// agentSnapshot 是单个活跃代理的运行时快照。
// "活跃" = 已注册到 mailbox.Registry，包括 worker / explorer / scheduler 自己。
//
// 字段优先级：
//   - ID + Type 总是出现（必填）
//   - MailboxPending 总是出现（即使为 0，让 LLM 知道"该代理空闲")
//   - CurrentTaskID/CurrentTaskDesc 仅在该代理正在处理某个 task 时出现
//   - LockedFiles 仅在 roster 有文件 claim 时出现
type agentSnapshot struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"` // "worker" / "explorer" / "scheduler" / "unknown"
	Profile         string   `json:"profile,omitempty"`
	MailboxPending  int      `json:"mailbox_pending"`
	CurrentTaskID   string   `json:"current_task_id,omitempty"`
	CurrentTaskDesc string   `json:"current_task_desc,omitempty"`
	LockedFiles     []string `json:"locked_files,omitempty"`
}

type triggerInfo struct {
	Type                  string `json:"type"`
	TaskID                string `json:"task_id,omitempty"`
	Text                  string `json:"text,omitempty"`
	Reasons               string `json:"reasons,omitempty"`
	SourceTaskIDs         string `json:"source_task_ids,omitempty"`
	Urgency               string `json:"urgency,omitempty"`
	ExecutionStateVersion string `json:"execution_state_version,omitempty"`
}

type taskSnapshot struct {
	ID                  string                   `json:"id"`
	Description         string                   `json:"description"`
	Status              string                   `json:"status"`
	EventType           string                   `json:"event_type,omitempty"`
	ResultRefs          []taskResultRef          `json:"result_refs,omitempty"`
	Error               string                   `json:"error,omitempty"`
	Dependencies        []string                 `json:"dependencies,omitempty"`
	Progress            *taskProgressSnapshot    `json:"progress,omitempty"`
	Artifacts           []string                 `json:"artifacts,omitempty"`
	LastResponsePreview *taskTextPreviewSnapshot `json:"last_response_preview,omitempty"`
	// Capability 投影任务的 per-node 节点能力声明（tools 子集 / model 覆盖），
	// 让 Scheduler 重规划时能看到自己此前设过什么。tools 量小（个位数），
	// 不做 result_refs 式有界摘录，直接全量放置。
	Capability *model.NodeCapability `json:"capability,omitempty"`
}

// taskResultRef is the stable hot-board index for one agent result. Excerpt is
// deliberately optional: all results remain discoverable even after the small
// board-wide excerpt budget has been consumed.
type taskResultRef struct {
	AgentID       string `json:"agent_id"`
	OriginalBytes int    `json:"original_bytes"`
	OriginalRunes int    `json:"original_runes"`
	SHA256        string `json:"sha256"`
	Excerpt       string `json:"excerpt,omitempty"`
	Truncated     bool   `json:"truncated"`
}

// taskProgressSnapshot keeps only the latest part of an in-flight transcript.
// PartialOutput itself is an observability stream, not an authoritative result;
// copying its unbounded body into every Scheduler prompt caused the largest
// prompt spikes seen in real sessions.
type taskProgressSnapshot struct {
	OriginalBytes int    `json:"original_bytes"`
	OriginalRunes int    `json:"original_runes"`
	RetainedTail  string `json:"retained_tail"`
	Truncated     bool   `json:"truncated"`
}

type taskTextPreviewSnapshot struct {
	OriginalBytes int    `json:"original_bytes"`
	OriginalRunes int    `json:"original_runes"`
	Text          string `json:"text"`
	Truncated     bool   `json:"truncated"`
}

// sessionEntry 是 board snapshot 中"用户输入历史"段的单条记录。
//
// 与 SessionInput 的字段一一对应，但 SubmittedAt 用 RFC3339 字符串
// 而非 time.Time，让 LLM 看到的 JSON 易读。
type sessionEntry struct {
	Text            string `json:"text"`
	SchedulerTaskID string `json:"scheduler_task_id"`
	SubmittedAt     string `json:"submitted_at"`
	Outcome         string `json:"outcome,omitempty"` // "completed" / "failed" / "cancelled" / "processing" / "pending"
}

// SnapshotSources 把 BuildBoardJSON 的可选数据源打包成一个 struct，
// 避免函数签名继续膨胀（Phase 3.1 时已经从 4 参数涨到 6 参数）。
//
// 所有字段都允许 nil：
//   - MBRegistry == nil：不输出 Resources.Agents
//   - Roster == nil：Agents 中 LockedFiles 为空
//   - History == nil：不输出 SessionHistory
//   - AgentRegistry == nil：不输出 Resources.SpecializedAgents
//
// 这种 nil-tolerant 设计让单元测试可以选择性覆盖某段而不需要构造完整依赖。
type SnapshotSources struct {
	MBRegistry      *mailbox.Registry
	Roster          roster.Roster
	History         *SessionHistory
	AgentRegistry   *AgentRegistry
	TemplateCatalog *agenttemplate.Catalog
	// WorkerCapabilities 是默认队列的兼容输入。生产路径由
	// SchedulerExecutor 从 AgentRegistry 的 ready route 提取保证工具集后传入。
	// nil 时不输出 worker 的 agent_capabilities 记录。
	WorkerCapabilities *AgentCapabilityInfo
	// WorkerProfiles 是每个 Worker 的 profile 映射（agentID → profile 名称）。
	// 用于在 agentSnapshot 中填充 Profile 字段。
	// nil 时不输出 profile 字段（向后兼容）。
	WorkerProfiles map[string]string
	// WorkerCapabilitiesByProfile 是按 profile 分组的 Worker 能力声明。
	// 替代原来的单一 WorkerCapabilities 字段。
	// nil 时回退到 WorkerCapabilities 的旧行为。
	WorkerCapabilitiesByProfile map[string]*AgentCapabilityInfo
	// ToolHealth 是 Bootstrap 阶段的工具可用性探测结果。
	// nil 时不输出 unavailable_tools 字段（向后兼容）。
	ToolHealth *probe.ToolHealthStatus
	// PendingDownstreamTasks 是依赖于当前 SchedulerBatch 但尚未到达终态的下游任务列表。
	// 由 SchedulerExecutor.detectDownstreamTasks 扫描生成。
	// 非空时 BuildBoardJSON 会在 snapshot 中注入 "pending_downstream_tasks" 段，
	// 提示 LLM 还有 reactor 触发的任务在运行。
	// nil 或空时该段被 omitempty 省略。
	PendingDownstreamTasks []PendingDownstreamTask
	// CurrentControllerTaskID 是当前 controller（scheduler root）任务 ID。
	// 非空时快照只暴露当前请求树，并作为动态 Team 路由的归属 scope。
	CurrentControllerTaskID string
}

// PendingDownstreamTask 是下游待处理任务的描述信息。
type PendingDownstreamTask struct {
	TaskID      string
	Description string
	Status      string
	AgentID     string
}

// BuildBoardJSON 构造 scheduler agent 在每轮 reactLoop 注入到 history 的 board 快照 JSON。
//
// 参数：
//   - s: TaskStore，用于 ScanAll
//   - cfg: 配置（读取 WorkerCount 计算可用 worker 数）
//   - modeSnap: 两轴模式快照（modes.Snapshot）——Exec / Topo 分别写入
//     "exec_mode" / "topo_mode"
//   - trigger: 当前 reactLoop 的触发事件（用户输入、task completed 等）
//   - sources: 可选数据源（mailbox / roster / session history）
//
// 设计原则：
//   - 自包含 helper 函数，不依赖 *Scheduler 或 *agent.Agent，方便单测
//   - 终态 Results 只展开 result_refs + 有界 excerpt；完整正文由 get_task_result 按需读取
//   - 失败/重试中的 LastResponse 与 processing PartialOutput 都只展示有界预览
//   - sources 任一字段为 nil 时对应段缺省（向后兼容）
//
// Phase 3.1 改动：新增 sources 参数，扩展 Resources.Agents + SessionHistory 两段。
func BuildBoardJSON(
	s store.TaskStore,
	cfg *config.Config,
	modeSnap modes.Snapshot,
	trigger model.Event,
	sources SnapshotSources,
) string {
	allTasks, _ := s.ScanAll()
	tasks := allTasks
	if sources.CurrentControllerTaskID != "" {
		// 只暴露当前请求树，不暴露无关 root 或历史会话留存的任务，
		// 让 board 与 get_task_result 的可见域口径一致。
		visible := store.LegacyRequestTaskIDs(allTasks, sources.CurrentControllerTaskID)
		filtered := make([]*model.Task, 0, len(visible))
		for _, task := range allTasks {
			if _, ok := visible[task.ID]; ok {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}

	ti := triggerInfo{Type: string(trigger.Type), TaskID: trigger.TaskID}
	if trigger.Payload != nil {
		ti.Text = trigger.Payload["text"]
		ti.Reasons = trigger.Payload["reasons"]
		ti.SourceTaskIDs = trigger.Payload["source_task_ids"]
		ti.Urgency = trigger.Payload["urgency"]
		ti.ExecutionStateVersion = trigger.Payload["execution_state_version"]
	}

	resultExcerpts := allocateTaskResultExcerpts(tasks, trigger)
	var taskSnaps []taskSnapshot
	for _, t := range tasks {
		snap := taskSnapshot{
			ID:          t.ID,
			Description: t.Description,
			Status:      string(t.Status),
			EventType:   t.EventType,
		}
		if model.IsTerminal(t.Status) && len(t.Results) > 0 {
			snap.ResultRefs = buildTaskResultRefs(t.Results, resultExcerpts[t.ID])
		}
		if t.Error != "" {
			snap.Error = t.Error
		}
		if len(t.Dependencies) > 0 {
			snap.Dependencies = t.Dependencies
		}
		if t.Status == model.TaskStatusProcessing && t.PartialOutput != "" {
			snap.Progress = buildTaskProgressSnapshot(t.PartialOutput)
		}
		if len(t.Artifacts) > 0 {
			snap.Artifacts = t.Artifacts
		}
		// 失败/重试中的任务只展开 LastResponse 预览；completed 使用 result_refs。
		if t.LastResponse != "" && t.Status != model.TaskStatusCompleted {
			snap.LastResponsePreview = buildTaskTextPreview(t.LastResponse, maxTaskLastResponseRunes)
		}
		// 节点能力投影：克隆防共享（ScanAll 克隆体仍不应被快照层别名）。
		// IsolationSpec 仅一个字符串字段，共享指针无别名风险；
		// Scheduler 修复/重规划隔离节点时需要看到隔离声明。
		if t.Capability != nil {
			snap.Capability = &model.NodeCapability{
				Tools:     append([]string(nil), t.Capability.Tools...),
				Model:     t.Capability.Model,
				Isolation: t.Capability.Isolation,
			}
		}
		taskSnaps = append(taskSnaps, snap)
	}

	// D3：busy 推导统一走 store.BusyAgentTasks；board 口径只统计默认队列
	// （EventType == ""）。helper 按 agent 去重（每 agent 计 1），旧实现按任务
	// 累加 len(t.Agents)——同一 agent 同一时刻只处理一个任务，两者数值相同。
	busyWorkers := 0
	for _, t := range store.BusyAgentTasks(allTasks) {
		if t.EventType == "" {
			busyWorkers++
		}
	}
	// v4：worker count 来自 cfg.Agents 中所有监听默认队列（event_type=""）的 kind 的 replicas 总和。
	// 这与 busyWorkers（仅统计 EventType=="" 的 processing 任务）口径一致。
	workerCount := 0
	for _, k := range cfg.Agents {
		if k.EventType == "" {
			workerCount += k.Replicas
		}
	}
	available := max(workerCount-busyWorkers, 0)

	// 动态 Team 路由的归属 scope 即当前 controller 任务 ID：快照只暴露全局
	// 静态 route 与归属当前 controller 的 Team；空串时只见全局路由。
	snapshotOwnerID := sources.CurrentControllerTaskID

	// 构造 agents 列表（来自 mailbox.Registry + roster + store 的反向映射）。
	agents := buildAgentSnapshotsForPlan(allTasks, sources.MBRegistry, sources.Roster, sources.WorkerProfiles, sources.AgentRegistry, snapshotOwnerID)

	// 构造特化代理聚合视图。归属 scope 之外的动态 Team 不暴露；
	// 无 controller 的兼容/诊断 snapshot 仍使用完整 registry。
	specialized := buildSpecializedAgentSnapshotsForPlan(allTasks, sources.AgentRegistry, snapshotOwnerID)

	// 构造代理能力声明（来自 AgentRegistry + Worker 能力声明）
	agentCaps := buildAgentCapabilitiesForPlan(sources.AgentRegistry, sources.WorkerCapabilities, sources.WorkerCapabilitiesByProfile, snapshotOwnerID)
	var templateSummaries []agenttemplate.Summary
	if sources.TemplateCatalog != nil {
		templateSummaries = sources.TemplateCatalog.List()
	}
	runtimeMode := "agent_team"
	if workerCount == 0 && len(specialized) == 0 {
		runtimeMode = "scheduler_only"
	}

	// 构造 session history（来自 SessionHistory + store 的状态查询）
	sessionEntries := buildSessionEntries(s, sources.History)

	// 构造下游待处理任务列表
	var pendingDownstream []pendingDownstreamSnapshot
	for _, pt := range sources.PendingDownstreamTasks {
		pendingDownstream = append(pendingDownstream, pendingDownstreamSnapshot{
			TaskID:      pt.TaskID,
			Description: pt.Description,
			Status:      pt.Status,
			AgentID:     pt.AgentID,
		})
	}

	bs := boardSnapshot{
		ExecMode: modeSnap.Exec,
		TopoMode: modeSnap.Topo,
		Trigger:  ti,
		Tasks:    taskSnaps,
		Resources: resourceInfo{
			RuntimeMode:       runtimeMode,
			WorkerCount:       workerCount,
			BusyWorkers:       busyWorkers,
			AvailableWorkers:  available,
			UnavailableTools:  sources.ToolHealth.UnavailableTools(),
			Agents:            agents,
			SpecializedAgents: specialized,
			AgentCapabilities: agentCaps,
			AgentTemplates:    templateSummaries,
		},
		SessionHistory:         sessionEntries,
		PendingDownstreamTasks: pendingDownstream,
	}
	data, _ := json.MarshalIndent(bs, "", "  ")
	return string(data)
}

// allocateTaskResultExcerpts distributes one small budget across all visible
// result refs. Trigger/source tasks are considered first, followed by the most
// recently created visible tasks. Metadata is emitted for every result even
// when its excerpt receives no budget.
func allocateTaskResultExcerpts(tasks []*model.Task, trigger model.Event) map[string]map[string]string {
	byID := make(map[string]*model.Task, len(tasks))
	for _, task := range tasks {
		if task != nil {
			byID[task.ID] = task
		}
	}

	priorityIDs := make([]string, 0, len(tasks)+4)
	priorityIDs = append(priorityIDs, trigger.TaskID)
	if trigger.Payload != nil {
		priorityIDs = append(priorityIDs, splitSnapshotIDs(trigger.Payload["source_task_ids"])...)
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i] != nil {
			priorityIDs = append(priorityIDs, tasks[i].ID)
		}
	}

	remaining := maxBoardResultExcerptTotalRunes
	seenTasks := make(map[string]bool, len(priorityIDs))
	out := make(map[string]map[string]string)
	for _, taskID := range priorityIDs {
		if remaining <= 0 {
			break
		}
		if taskID == "" || seenTasks[taskID] {
			continue
		}
		seenTasks[taskID] = true
		task := byID[taskID]
		if task == nil || !model.IsTerminal(task.Status) || len(task.Results) == 0 {
			continue
		}

		agentIDs := sortedResultAgentIDs(task.Results)
		for _, agentID := range agentIDs {
			if remaining <= 0 {
				break
			}
			limit := min(maxTaskResultExcerptRunes, remaining)
			excerpt, _ := compactSnapshotExcerpt(task.Results[agentID], limit)
			if excerpt == "" {
				continue
			}
			if out[taskID] == nil {
				out[taskID] = make(map[string]string)
			}
			out[taskID][agentID] = excerpt
			remaining -= utf8.RuneCountInString(excerpt)
		}
	}
	return out
}

func splitSnapshotIDs(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func sortedResultAgentIDs(results map[string]string) []string {
	ids := make([]string, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func buildTaskResultRefs(results map[string]string, excerpts map[string]string) []taskResultRef {
	ids := sortedResultAgentIDs(results)
	refs := make([]taskResultRef, 0, len(ids))
	for _, agentID := range ids {
		value := results[agentID]
		excerpt := excerpts[agentID]
		sum := sha256.Sum256([]byte(value))
		refs = append(refs, taskResultRef{
			AgentID:       agentID,
			OriginalBytes: len(value),
			OriginalRunes: utf8.RuneCountInString(value),
			SHA256:        fmt.Sprintf("%x", sum),
			Excerpt:       excerpt,
			Truncated:     excerpt != value,
		})
	}
	return refs
}

func buildTaskProgressSnapshot(value string) *taskProgressSnapshot {
	runes := []rune(value)
	retained := value
	truncated := len(runes) > maxTaskProgressTailRunes
	if truncated {
		retained = string(runes[len(runes)-maxTaskProgressTailRunes:])
	}
	return &taskProgressSnapshot{
		OriginalBytes: len(value), OriginalRunes: len(runes),
		RetainedTail: retained, Truncated: truncated,
	}
}

func buildTaskTextPreview(value string, limit int) *taskTextPreviewSnapshot {
	text, truncated := compactSnapshotExcerpt(value, limit)
	return &taskTextPreviewSnapshot{
		OriginalBytes: len(value), OriginalRunes: utf8.RuneCountInString(value),
		Text: text, Truncated: truncated,
	}
}

// compactSnapshotExcerpt preserves both the opening conclusion and trailing
// caveats. The returned string never exceeds limit runes.
func compactSnapshotExcerpt(value string, limit int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}
	if limit <= 0 {
		return "", len(runes) > 0
	}
	if limit == 1 {
		return "…", true
	}
	contentBudget := limit - 1
	head := contentBudget * 3 / 4
	tail := contentBudget - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:]), true
}

// buildAgentCapabilities 合并 AgentRegistry 中的特化代理 + Worker 的能力声明，
// 产出 agent_capabilities 数组。
//
// 排序规则：
//   - Worker 记录始终排在第一位（当 workerCaps 非 nil 时）
//   - 当 capsByProfile 非空时，为每个不同的 profile 输出一条带 profile 字段的记录（按 profile 名字典序排列）
//   - 特化代理按 registry.Specialized() 的注册顺序排列
//
// 当 workerCaps 为 nil、capsByProfile 为空且 registry 为空/nil 时返回 nil（omitempty 省略字段）。
func buildAgentCapabilities(
	registry *AgentRegistry,
	workerCaps *AgentCapabilityInfo,
	capsByProfile map[string]*AgentCapabilityInfo,
) []agentCapabilitySnapshot {
	return buildAgentCapabilitiesForPlan(registry, workerCaps, capsByProfile, "")
}

func buildAgentCapabilitiesForPlan(
	registry *AgentRegistry,
	workerCaps *AgentCapabilityInfo,
	capsByProfile map[string]*AgentCapabilityInfo,
	ownerID string,
) []agentCapabilitySnapshot {
	entries := registry.Specialized()
	if ownerID != "" {
		entries = registry.SpecializedForPlan(ownerID)
	}

	// Per-profile 模式：capsByProfile 非空时，忽略 workerCaps，为每个 profile 输出一条记录
	if len(capsByProfile) > 0 {
		if len(entries) == 0 && len(capsByProfile) == 0 {
			return nil
		}

		out := make([]agentCapabilitySnapshot, 0, len(capsByProfile)+len(entries))

		// 按 profile 名字典序排列，确保稳定输出
		profiles := make([]string, 0, len(capsByProfile))
		for p := range capsByProfile {
			profiles = append(profiles, p)
		}
		sort.Strings(profiles)

		for _, p := range profiles {
			info := capsByProfile[p]
			out = append(out, agentCapabilitySnapshot{
				AgentType:    "worker",
				Profile:      p,
				Capabilities: info.Capabilities,
				Description:  info.Description,
			})
		}

		// 特化代理从 registry 读取
		for _, e := range entries {
			out = append(out, agentCapabilitySnapshot{
				AgentType:    e.EventType,
				Capabilities: e.Capabilities,
				Description:  e.Role,
			})
		}

		return out
	}

	// 旧路径：单条 worker 记录（向后兼容）
	if workerCaps == nil && len(entries) == 0 {
		return nil
	}

	out := make([]agentCapabilitySnapshot, 0, len(entries)+1)

	// Worker 始终排在第一位
	if workerCaps != nil {
		out = append(out, agentCapabilitySnapshot{
			AgentType:    "worker",
			Capabilities: workerCaps.Capabilities,
			Description:  workerCaps.Description,
		})
	}

	// 特化代理从 registry 读取
	for _, e := range entries {
		out = append(out, agentCapabilitySnapshot{
			AgentType:    e.EventType,
			Capabilities: e.Capabilities,
			Description:  e.Role,
		})
	}

	return out
}

// buildSpecializedAgentSnapshots 合并静态 AgentRegistry + live task 扫描，
// 产出 specialized_agents 聚合视图。registry 为 nil 或为空时返回 nil。
//
// Busy 计算：扫描所有 status=processing 的任务，按 EventType 累计实例数。
// 一个任务如果有 2 个 agent 同时认领（Agents 列表长度 > 1），busy 计 2。
func buildSpecializedAgentSnapshots(tasks []*model.Task, registry *AgentRegistry) []specializedAgentSnapshot {
	return buildSpecializedAgentSnapshotsForPlan(tasks, registry, "")
}

func buildSpecializedAgentSnapshotsForPlan(tasks []*model.Task, registry *AgentRegistry, ownerID string) []specializedAgentSnapshot {
	entries := registry.Specialized()
	if ownerID != "" {
		entries = registry.SpecializedForPlan(ownerID)
	}
	if len(entries) == 0 {
		return nil
	}
	busyByEventType := make(map[string]int)
	for _, t := range tasks {
		if t.Status != model.TaskStatusProcessing {
			continue
		}
		if t.EventType == "" {
			continue // 通用 worker 不计入特化代理统计
		}
		busyByEventType[t.EventType] += len(t.Agents)
	}
	out := make([]specializedAgentSnapshot, 0, len(entries))
	for _, e := range entries {
		out = append(out, specializedAgentSnapshot{
			EventType: e.EventType,
			Count:     e.Count,
			Busy:      busyByEventType[e.EventType],
			Role:      e.Role,
		})
	}
	return out
}

// buildAgentSnapshots 从 mailbox.Registry + roster + 当前 task 列表构造代理快照。
//
// mb == nil 时返回 nil（snapshot 中省略 agents 字段）。
//
// 算法：
//  1. mb.ScanAll() 拿到所有已注册代理 + mailbox 状态
//  2. 反向遍历 tasks 构造 agentID → currentTask 映射（仅 processing 状态）
//  3. 对每个代理调 roster.ListByAgent 拿当前 file claims（roster nil 时跳过）
//  4. 按 agentID 字典序排序确保稳定输出
func buildAgentSnapshots(tasks []*model.Task, mb *mailbox.Registry, r roster.Roster, workerProfiles map[string]string) []agentSnapshot {
	return buildAgentSnapshotsForPlan(tasks, mb, r, workerProfiles, nil, "")
}

func buildAgentSnapshotsForPlan(
	tasks []*model.Task,
	mb *mailbox.Registry,
	r roster.Roster,
	workerProfiles map[string]string,
	registry *AgentRegistry,
	ownerID string,
) []agentSnapshot {
	if mb == nil {
		return nil
	}
	statuses := mb.ScanAll()
	if len(statuses) == 0 {
		return nil
	}

	// 反向映射：agentID → 当前正在处理的 task。
	// D3：统一走 store.BusyAgentTasks——若同一代理认领了多个任务（并发处理），
	// 保留 first-seen（ScanAll 确定序，实践中 worker 一般串行，这是个保守假设）。
	currentTask := store.BusyAgentTasks(tasks)

	out := make([]agentSnapshot, 0, len(statuses))
	for _, st := range statuses {
		if ownerID != "" && strings.HasPrefix(st.EventType, "team:") &&
			(registry == nil || !registry.CanRouteForPlan(ownerID, st.EventType)) {
			continue
		}
		snap := agentSnapshot{
			ID:             st.AgentID,
			Type:           agentTypeFromEventType(st.EventType),
			MailboxPending: st.Count,
		}
		if workerProfiles != nil {
			if profile, ok := workerProfiles[st.AgentID]; ok {
				snap.Profile = profile
			}
		}
		if cur, ok := currentTask[st.AgentID]; ok {
			snap.CurrentTaskID = cur.ID
			snap.CurrentTaskDesc = truncateText(cur.Description, 80)
		}
		if r != nil {
			if claims, err := r.ListByAgent(st.AgentID); err == nil && len(claims) > 0 {
				files := make([]string, 0, len(claims))
				for _, c := range claims {
					files = append(files, c.FilePath)
				}
				snap.LockedFiles = files
			}
		}
		out = append(out, snap)
	}

	// 字典序排序便于测试断言 + LLM 阅读
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// agentTypeFromEventType 把 mailbox 注册时的 eventType 翻译成可读类型名。
//
// 与 worker/explorer/scheduler 各自的 EventType 约定保持同步：
//   - "" → worker
//   - "explore" → explorer
//   - "__scheduler__" → scheduler
//   - custom/static/Team route → 保留 event_type，避免把可路由资源折叠成 unknown
func agentTypeFromEventType(eventType string) string {
	switch eventType {
	case "":
		return "worker"
	case "explore":
		return "explorer"
	case "__scheduler__":
		return "scheduler"
	default:
		return eventType
	}
}

// buildSessionEntries 把 SessionHistory 转换为 sessionEntry 切片。
//
// history == nil 时返回 nil。Outcome 字段从 store 当前状态查询：
//   - GetTask 失败：留空（任务可能已被淘汰）
//   - 其他：写入 task.Status 字符串
func buildSessionEntries(s store.TaskStore, history *SessionHistory) []sessionEntry {
	if history == nil {
		return nil
	}
	entries := history.Snapshot(0)
	if len(entries) == 0 {
		return nil
	}
	out := make([]sessionEntry, 0, len(entries))
	for _, in := range entries {
		e := sessionEntry{
			Text:            in.Text,
			SchedulerTaskID: in.SchedulerTaskID,
			SubmittedAt:     in.SubmittedAt.Format(time.RFC3339),
		}
		if t, err := s.GetTask(in.SchedulerTaskID); err == nil && t != nil {
			e.Outcome = string(t.Status)
		}
		out = append(out, e)
	}
	return out
}

// truncateText 把字符串截断到最多 maxRunes 个 rune，超出部分用 "..." 代替。
// 与 mailbox.truncate 同语义但独立实现（避免跨包依赖只为这一个 helper）。
func truncateText(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
