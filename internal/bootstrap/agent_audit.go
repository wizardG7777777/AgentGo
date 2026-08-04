package bootstrap

// agent_audit.go 实现 V6 §2（P1b）/doctor agents 只读代理审计。
//
// 数据流：UI 命令（/doctor agents）→ ui.Controller.RequestAgentAudit →
// System.RequestAgentAudit——按启动期收集的装配事实（AgentAuditEntry）
// + 请求时刻的路由/模式状态构建审计包，包装成只读审计任务发布给
// Scheduler（EventType=__scheduler__，EventSource=agent-audit，与 C5d
// 唤醒任务同通道）。Scheduler 只做语义对照（prompt 身份/职责 vs 实际
// 权限），审计报告作为普通任务结果回显到消息流（IsUserFacing →
// ResultOutput → OutputCh → feed），无专门回收通道。
//
// trace 账本：agent_audit_started（发布）→ agent_audit_warning（快照期
// 确定性 warning，当前仅 route_missing 类）→ agent_audit_completed
//（审计任务终态，由 agentAuditReactor 在终态事件上补记；meta 为进程内
// 事实，重启后未完成审计任务的 completed 事件不补——观测事件，不阻断
// 任何主流程）。Scheduler 语义对照得出的「身份/权限」warning 属自由
// 文本，只进任务结果，不进 trace（与正文不落账本同一纪律）。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/prompt"
	"agentgo/internal/trace"
)

// agentAuditPromptExcerptRunes 是审计包中系统 prompt 摘录的最大 rune 数。
const agentAuditPromptExcerptRunes = 200

// agentAuditPendingLimit 是 agentAuditReactor 待补记 meta 的 FIFO 容量
//（审计由用户显式触发，正常远低于此；溢出时丢弃最旧一条，只影响
// agent_audit_completed 补记，不影响审计任务本身）。
const agentAuditPendingLimit = 32

// AgentAuditEntry 是单个 agent（静态 kind 或 scheduler）的装配期审计要素：
// 启动期收集一次，之后只读（运行期路由/模式状态在请求时另取）。
type AgentAuditEntry struct {
	Kind          string   // agent kind（"worker" / "scheduler" / ...）
	EventType     string   // 认领路由（""=默认队列，"__scheduler__"=控制面）
	Description   string   // 角色描述（配置值或启动期兜底生成）
	PromptDigest  string   // 最终系统 prompt 的 sha256 前 12
	PromptExcerpt string   // 系统 prompt 前 agentAuditPromptExcerptRunes 个 rune
	AllowedTools  []string // 装配后的真实工具白名单（rt.AllowedTools / 注册全集）
	Replicas      int      // 静态副本数
}

// agentAuditWarning 是快照构建期可确定性判定的 warning（当前仅
// route_missing）。「身份高于权限 / 权限超出身份」两类语义冲突由
// Scheduler 对照后写入审计报告正文，不在此判定。
type agentAuditWarning struct {
	Agent    string `json:"agent"`
	Type     string `json:"type"` // route_missing
	Evidence string `json:"evidence"`
}

// agentAuditMeta 是 agent_audit_completed 补记所需的发布期计数事实。
type agentAuditMeta struct {
	Agents         int            `json:"agents"`
	Warnings       int            `json:"warnings"`
	WarningTypes   map[string]int `json:"warning_types,omitempty"`
	SnapshotDigest string         `json:"snapshot_digest"`
}

// agentAuditReactor 订阅任务终态事件，为 EventSource=agent-audit 的任务
// 补记 agent_audit_completed。Async（100 档），meta 是进程内 FIFO map。
type agentAuditReactor struct {
	mu      sync.Mutex
	pending map[string]agentAuditMeta
	order   []string
}

func newAgentAuditReactor() *agentAuditReactor {
	return &agentAuditReactor{pending: make(map[string]agentAuditMeta)}
}

func (r *agentAuditReactor) Name() string { return "agent-audit-terminal" }

func (r *agentAuditReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted, trace.KindTaskFailed,
		trace.KindTaskBlocked, trace.KindTaskCancelled,
	}
}

func (r *agentAuditReactor) IsSync() bool { return false }

func (r *agentAuditReactor) Priority() int { return 100 }

// track 登记一条待补记的审计任务（发布时调用）。
func (r *agentAuditReactor) track(taskID string, meta agentAuditMeta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.pending[taskID]; !exists {
		r.order = append(r.order, taskID)
	}
	r.pending[taskID] = meta
	for len(r.order) > agentAuditPendingLimit {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.pending, oldest)
	}
}

// Run 在审计任务终态补记 agent_audit_completed（其余任务直接忽略）。
func (r *agentAuditReactor) Run(ev trace.Event) error {
	if ev.TaskID == "" {
		return nil
	}
	r.mu.Lock()
	meta, ok := r.pending[ev.TaskID]
	if ok {
		delete(r.pending, ev.TaskID)
		for i, id := range r.order {
			if id == ev.TaskID {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	status := strings.TrimPrefix(string(ev.Kind), "task_")
	summary, err := json.Marshal(meta)
	if err != nil {
		summary = []byte("{}")
	}
	trace.Emit(trace.Event{
		Kind:        trace.KindAgentAuditCompleted,
		TaskID:      ev.TaskID,
		AgentID:     ev.AgentID,
		Reason:      status,
		Description: string(summary),
	})
	return nil
}

// auditExcerpt 截取 prompt 前 agentAuditPromptExcerptRunes 个 rune 作为摘录。
func auditExcerpt(text string) string {
	runes := []rune(text)
	if len(runes) <= agentAuditPromptExcerptRunes {
		return text
	}
	return string(runes[:agentAuditPromptExcerptRunes]) + "……"
}

// auditDescriptionFallback 与启动期 route 注册的 description 兜底同款
// （bootstrap.go agentRegistry.RegisterRoute 的 role 兜底）。
func auditDescriptionFallback(kind config.AgentKind) string {
	if kind.Description != "" {
		return kind.Description
	}
	return fmt.Sprintf("kind=%s（监听 event_type=%q）", kind.Kind, kind.EventType)
}

// agentAuditControlTools 派生审计快照的控制通道工具（与
// agent.deriveControlTools 同口径的 kind 级视图：scheduler=report_done，
// 执行任务=submit_task_result）。
func agentAuditControlTools(eventType string) []string {
	if eventType == "__scheduler__" {
		return []string{"report_done"}
	}
	return []string{"submit_task_result"}
}

// agentAuditRouteReady 判定请求时刻该 entry 的 event_type 是否有 ready
// runner 认领（scheduler 常驻视为 ready）。
func (s *System) agentAuditRouteReady(entry AgentAuditEntry) bool {
	if entry.EventType == "__scheduler__" {
		return s.Scheduler != nil && s.Scheduler.Agent != nil
	}
	if s.Scheduler == nil || s.Scheduler.SchedulerExec == nil || s.Scheduler.SchedulerExec.AgentRegistry == nil {
		return false
	}
	_, ok := s.Scheduler.SchedulerExec.AgentRegistry.RouteCapabilities(entry.EventType)
	return ok
}

// RequestAgentAudit 是 /doctor agents 的后端入口（ui.Deps 注入）：构建
// 审计包并发布只读审计任务给 Scheduler，返回审计任务 ID。
// 无 Scheduler 或任务存储未装配时返回中文错误。
func (s *System) RequestAgentAudit() (string, error) {
	if s == nil || s.Scheduler == nil || s.Scheduler.Agent == nil {
		return "", fmt.Errorf("系统未装配 Scheduler，无法执行代理审计")
	}
	if s.Store == nil {
		return "", fmt.Errorf("任务存储未装配，无法执行代理审计")
	}

	// 快照：装配期要素 + 请求时刻的路由/模式状态。
	execMode, topoMode := "normal", "team"
	if s.Scheduler.Modes != nil {
		execMode = s.Scheduler.Modes.GetExec().String()
		topoMode = s.Scheduler.Modes.GetTopo().String()
	}
	entries := append([]AgentAuditEntry(nil), s.agentAuditEntries...)
	warnings := make([]agentAuditWarning, 0)
	for _, entry := range entries {
		if !s.agentAuditRouteReady(entry) {
			warnings = append(warnings, agentAuditWarning{
				Agent:    entry.Kind,
				Type:     "route_missing",
				Evidence: fmt.Sprintf("event_type=%q 无 ready runner 认领", entry.EventType),
			})
		}
	}

	// 能力快照 digest：覆盖全部审计要素与运行模式（不含正文外的可变状态）。
	digestPayload, _ := json.Marshal(struct {
		Entries  []AgentAuditEntry `json:"entries"`
		ExecMode string            `json:"exec_mode"`
		TopoMode string            `json:"topo_mode"`
	}{entries, execMode, topoMode})
	snapshotDigest := prompt.DigestText(string(digestPayload))

	description := renderAgentAuditDescription(entries, warnings, execMode, topoMode, snapshotDigest)
	task := &model.Task{
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    "agent-audit",
		MaxConcurrency: 1, // 同一时刻只跑一个审计
	}
	if err := s.Store.PublishTask(task); err != nil {
		return "", fmt.Errorf("发布代理审计任务失败: %w", err)
	}

	warningTypes := make(map[string]int, len(warnings))
	for _, w := range warnings {
		warningTypes[w.Type]++
	}
	meta := agentAuditMeta{
		Agents: len(entries), Warnings: len(warnings),
		WarningTypes: warningTypes, SnapshotDigest: snapshotDigest,
	}
	if s.agentAudit != nil {
		s.agentAudit.track(task.ID, meta)
	}

	startedSummary, _ := json.Marshal(struct {
		Agents         int    `json:"agents"`
		SnapshotDigest string `json:"snapshot_digest"`
		Warnings       int    `json:"deterministic_warnings"`
	}{len(entries), snapshotDigest, len(warnings)})
	trace.Emit(trace.Event{
		Kind:        trace.KindAgentAuditStarted,
		TaskID:      task.ID,
		Description: string(startedSummary),
	})
	for _, w := range warnings {
		warningSummary, _ := json.Marshal(w)
		trace.Emit(trace.Event{
			Kind:        trace.KindAgentAuditWarning,
			TaskID:      task.ID,
			Description: string(warningSummary),
		})
	}
	log.Printf("[doctor] 代理审计任务已创建：%s（agents=%d 确定性warning=%d digest=%s）",
		task.ID, len(entries), len(warnings), snapshotDigest)
	return task.ID, nil
}

// renderAgentAuditDescription 渲染审计任务描述：只读指令 + 逐 agent 快照
// 要素 + 输出格式契约。
func renderAgentAuditDescription(entries []AgentAuditEntry, warnings []agentAuditWarning, execMode, topoMode, snapshotDigest string) string {
	var b strings.Builder
	b.WriteString("[agent-audit]\n")
	b.WriteString("你是只读审计员。下面列出系统中每个 agent 的身份声明与实际权限事实（能力快照 digest=" + snapshotDigest + "）。\n\n")
	b.WriteString("纪律（硬约束）：\n")
	b.WriteString("- 只做语义对照：「prompt 给予的身份/职责」与「实际权限」明显冲突时才报告 warning；措辞差异不算冲突。\n")
	b.WriteString("- 不修改任何配置、不授予或收回任何工具、不发布或取消任何任务——本任务为只读审计。\n\n")
	b.WriteString("运行模式：exec=" + execMode + "，topo=" + topoMode + "\n\n")

	for _, entry := range entries {
		fmt.Fprintf(&b, "=== agent: %s（event_type=%q，replicas=%d）===\n", entry.Kind, entry.EventType, entry.Replicas)
		b.WriteString("- 角色描述：" + entry.Description + "\n")
		fmt.Fprintf(&b, "- 系统 prompt：digest=%s，摘录「%s」\n", entry.PromptDigest, entry.PromptExcerpt)
		b.WriteString("- 实际工具白名单：" + strings.Join(entry.AllowedTools, ", ") + "\n")
		b.WriteString("- 控制通道工具：" + strings.Join(agentAuditControlTools(entry.EventType), ", ") + "\n")
		if entry.EventType == "__scheduler__" {
			b.WriteString("- 路由状态：ready（scheduler 常驻控制面）\n")
		}
		b.WriteString("\n")
	}

	if len(warnings) > 0 {
		b.WriteString("快照构建期已确定的 warning（请纳入报告）：\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "- agent=%s 类型=路由缺失 证据=%s\n", w.Agent, w.Evidence)
		}
		b.WriteString("\n")
	}

	b.WriteString("输出要求：\n")
	b.WriteString("- 每条 warning 输出：agent、冲突类型（身份高于权限 / 权限超出身份 / 路由缺失）、prompt 片段摘要、权限证据。\n")
	b.WriteString("- 语义对照无冲突时明确回答「无 warning」，不要为产出而编造冲突。\n")
	b.WriteString("- 最后以自然语言给出简明审计报告（本报告将直接回显给用户）。\n")
	return b.String()
}
