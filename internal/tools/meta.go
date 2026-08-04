package tools

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/effect"
	"agentgo/internal/interaction"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
)

// TaskHolder 提供“当前正在执行的任务 ID”。
// 用于 publish_task 在 Worker 模式下定位父任务、检查深度限制。
// nil 时表示 Scheduler 语义（无父任务、无深度限制）。
type TaskHolder interface {
	Get() string
}

// BatchTracker 是 publish_task 工具发布子任务后的副作用回调（Phase 3 引入）。
//
// scheduler 通过这个接口把每个新发布的子任务 ID 追加到 scheduler task 自身的
// SchedulerBatch 字段，使 SchedulerExecutor 后续能等待这一批 task 全部进入终态。
// worker / explorer bootstrap 时不传 BatchTracker（nil），publish_task 行为不变。
//
// 之所以是接口而不是直接传 store + currentTaskID：scheduler 内部由 holder 闭包
// 提供 currentTaskID，外面看不到；接口让 scheduler 把这个闭包能力注入 MetaGroup
// 而无需暴露 holder 实现细节。
type BatchTracker interface {
	// AppendBatch 在 publish_task 工具成功创建 task 后被调用一次。
	// 返回错误时 publish_task 工具记录日志但不失败（batch 跟踪是辅助能力，
	// 失败不应阻塞用户的任务发布）。
	AppendBatch(childTaskID string) error
}

// RouteValidator is the runtime authority for task routing. Production
// Scheduler and runners inject it so a catalog entry, stale event_type, or a
// Team route owned by another request scope cannot create an invalid Task.
// Isolated compatibility paths may leave it nil.
//
// CanRouteForPlan 的第一参数是路由归属 scope ID（controller 任务 ID；
// 空串 = 全局）。
type RouteValidator interface {
	CanRouteForPlan(ownerScopeID, eventType string, requiredTools ...string) bool
}

// MetaGroup 注册任务发布与代理间通信工具。
//
// 字段说明：
//   - Store：任务存储；nil 时不注册 publish_task
//   - Holder：当前任务持有器；nil = Scheduler 语义（无深度限制）；非 nil = Worker 语义
//   - MaxDepth：仅 Holder != nil 时生效；publish_task 创建的子任务深度超过该值时拒绝
//   - MBRegistry：邮箱注册表；nil 时不注册 send_message
//   - AgentID：当前代理 ID（send_message 的发件人）
//   - Interactions：通用结构化人机交互服务；nil 时不注册 request_user_input
//   - SessionID：创建 Interaction 时读取当前 Session；切换 Session 不会重标旧请求
//   - InteractionWaitHook：等待回答期间映射 waiting_interaction 状态
//   - BatchTracker：（可选，Phase 3）publish_task 成功后追加子任务 ID 到此 tracker；
//     scheduler 注入时把 ID 写入 scheduler task.SchedulerBatch；worker 不注入则无副作用
//
// 注：早期曾有 `DisablePublishTask bool` capability 位，用于让 Explorer 注入 Store/Holder
// 的同时仍然不暴露 publish_task。Phase D（2026-04-26）删除 internal/explorer 后该字段
// 失去全部调用方；v4 的 publish_task 准入完全由 runner 的 AllowedTools allowlist 过滤
// 控制（`tool_profiles` / `agents[].tools`），故于 2026-04-26 一并移除——见 runner.go
// 中 ToolRegistry 的 Filter 路径。
type MetaGroup struct {
	Store  store.TaskStore
	Holder TaskHolder
	// LineageHolder 只提供父任务身份，不启用 Worker 的深度限制。
	// Scheduler 使用它把自己发布的 Task 关联到当前 controller 任务
	// （ParentTaskID 谱系关联 + 路由归属 scope）。
	LineageHolder       TaskHolder
	MaxDepth            int
	MBRegistry          *mailbox.Registry
	AgentID             string
	Interactions        *interaction.Service
	SessionID           func() string
	InteractionWaitHook func(waiting bool)
	BatchTracker        BatchTracker
	RouteValidator      RouteValidator
	// AllowNodeCapability 仅由内置装配注入（Scheduler 装配置 true）。
	// 普通 Worker/Reactor 留零值，publish_task 据此拒绝它们经
	// tools/model/isolation 参数改变节点的执行边界。
	AllowNodeCapability bool
	// EffectJournal 是 V6 §4 H2b 副作用账本（internal/effect）；
	// nil 时 send_message 不记账（行为与引入账本前完全一致）。
	EffectJournal *effect.Journal
}

// Register 把 publish_task / send_message / request_user_input 注册到 r。
// 各自的依赖缺失时自动跳过对应工具。
func (g MetaGroup) Register(r *agent.ToolRegistry) {
	if g.Store != nil {
		r.Register(
			"publish_task",
			"发布一个新任务到任务队列，由调度器或其他代理认领执行",
			schema.Object().
				String("description", "任务的详细描述", true).
				String("event_type", "ready Agent route；静态默认 Worker 可留空，动态 Team 必须填写 provision_agent_team 返回的真实 event_type", false).
				Enum("priority", "任务优先级，默认 normal", []string{"low", "normal", "high"}, false).
				String("dependencies", "逗号分隔的依赖任务 UUID 列表。每个 ID 必须是之前 publish_task 调用返回的真实 task UUID（形如 7b52b232-4e9b-4b97-8bbc-f3d5927dc814），禁止使用占位符（如 \"task-part1\"、\"A\"、\"<id>\"）或自造 ID。若被依赖任务尚未发布，请先发布被依赖任务、从返回值中读取 id 之后再发布当前任务。留空表示无依赖", false).
				String("expected_artifacts", "逗号分隔的预期产出文件路径列表（相对项目根的相对路径）。任务结束时系统会校验这些文件是否真的写入；缺失则任务失败重试。强烈建议为'报告/总结/文档'类任务填写此字段以防止 report-only 失败", false).
				String("tools", "逗号分隔的工具名子集：限定本 DAG 节点只允许使用这些工具（必须是认领 Agent 白名单的子集，否则无人能认领）。仅 Scheduler 计划控制面可设置；留空表示不限制", false).
				String("model", "本 DAG 节点的模型覆盖（如 deepseek-r1）。仅 Scheduler 计划控制面可设置；留空表示沿用认领 Agent 的默认模型", false).
				String("isolation", "本 DAG 节点的执行隔离：唯一合法值 \"workspace\"——认领该节点的 Agent 在写时复制 overlay 中执行（读穿透主根、写落任务专属 workspace .agentgo/workspaces/<taskID>/），任务成功终态由控制面自动合并回主根；合并冲突时任务失败并自动 replan，由 Scheduler 裁决。仅 Scheduler 计划控制面可设置；留空表示不隔离", false).
				Int("max_concurrency", "该任务允许几个 Agent 同时认领执行，默认 1（单交付物任务必须是 1，否则多个 Agent 会重复执行同一份工作并互相覆盖产出）。仅当确实需要多个 Agent 冗余/分片执行同一任务时才设为 >1", false).
				Build(),
			g.publishTask,
		)
	}
	if g.MBRegistry != nil {
		r.Register(
			"send_message",
			"向指定代理发送结构化消息（点对点或广播）。"+
				"**重要——消息类型决定收件方响应语义**：\n"+
				"• 如果你需要对方**立即停下手头的事来响应**本消息，必须用 `msg_type=\"question\"` 或 `msg_type=\"steer\"`，或显式标注 `priority=\"high\"`；系统会为空闲的收件方发起一次唤醒任务。\n"+
				"• 如果只是**广播通知 / 进度汇报 / 顺带提一句**，继续用默认 `msg_type=\"info\"` + `priority=\"normal\"`；收件方在其下一轮任务的 reactLoop 开头自然读取到。\n"+
				"• **特别注意**：`msg_type=\"info\"` + `priority=\"low\"` 的组合会被系统判定为\"纯广播噪音\"，若收件方全程空闲可能被自动丢弃以避免邮箱污染——仅当你确实不在乎是否被读时才用这个组合（典型是系统自动生成的 progress-notify，LLM 通常不需要手动发 low 优先级）。\n"+
				"**收件人必须是真实 agent ID**（如 \"worker-1\"、\"scheduler\"、\"explorer-1\"）或 \"*\" 表示广播。",
			schema.Object().
				String("to", "收件人代理 ID（如 \"worker-1\"、\"scheduler\"），或 \"*\" 表示广播", true).
				String("content", "消息正文（详细内容）", true).
				String("summary", "一句话摘要，帮助收信方快速判断消息重点（建议始终填写）", false).
				Enum("msg_type", "消息类型：info=通知（默认，不触发立即唤醒）, question=提问（期望对方立即回复，会触发唤醒）, reply=回复先前消息, steer=纠偏指令（触发唤醒）。选错 type 会让紧急消息被当作噪音或让广播消息烧掉 token——按语义选择",
					[]string{"info", "question", "reply", "steer"}, false).
				Enum("priority", "优先级：low=纯噪音广播可丢弃 / normal=默认 / high=立即唤醒空闲收件方。默认 normal。LLM 主动发消息通常用 normal；写 low 意味着你同意对方看不到也没关系",
					[]string{"low", "normal", "high"}, false).
				Build(),
			g.sendMessage,
		)
	}
	if g.Interactions != nil {
		params := schema.Object().
			String("prompt", "需要用户回答的明确问题", true).
			String("options_json", "JSON 数组（2-8 项）；每项只能包含 id、label、可选 description、可选 requires_text。普通 Agent 提问只会收到回答，不会授予 Shell 权限或改变图执行状态", true).
			Build()
		params["additionalProperties"] = false
		r.Register(
			"request_user_input",
			"向用户提出一个结构化选择题并等待回答。该工具只返回用户选择，不替代 run_shell 的授权 Interaction，也不替代图审批（approval）节点。",
			params,
			g.requestUserInput,
		)
	}
}

// publishTask 统一实现 Worker / Scheduler 的任务发布逻辑。
//
//   - Holder == nil：Scheduler 模式，新任务 Depth=0，无深度限制
//   - Holder != nil：Worker 模式，从当前任务读取 Depth，子任务 Depth=parent+1，
//     超过 MaxDepth 时拒绝（childDepth > MaxDepth）
func (g MetaGroup) publishTask(ctx context.Context, args map[string]any) (string, error) {
	desc, _ := args["description"].(string)
	if desc == "" {
		return "", fmt.Errorf("缺少 description 参数")
	}
	eventType, _ := args["event_type"].(string)

	parentID := ""
	parentOwnerID := "" // Worker 任务不再携带控制面归属：固定空串（全局）
	parentDepth := -1   // Scheduler 模式下 childDepth = 0
	if g.Holder != nil {
		parentID = g.Holder.Get()
		if parentID == "" {
			return "", fmt.Errorf("无法获取当前任务上下文")
		}
		parentTask, err := g.Store.GetTask(parentID)
		if err != nil {
			return "", fmt.Errorf("读取父任务失败: %w", err)
		}
		parentDepth = parentTask.Depth
	} else if g.LineageHolder != nil {
		// Scheduler 不受子任务深度限制，但仍必须留下真实父任务，
		// 保持 ParentTaskID 谱系关联；controller 任务 ID 即路由归属 scope。
		parentID = g.LineageHolder.Get()
		if parentID == "" {
			return "", fmt.Errorf("无法获取当前任务上下文")
		}
		parentOwnerID = parentID
	}

	if g.RouteValidator != nil && !g.RouteValidator.CanRouteForPlan(parentOwnerID, eventType) {
		display := eventType
		if display == "" {
			display = "<default>"
		}
		return "", fmt.Errorf("发布任务被拒绝: event_type=%q 没有 ready Agent route 可供当前请求使用；请先 provision Agent Team，并在下一轮使用返回的真实 event_type", display)
	}

	childDepth := parentDepth + 1
	if g.Holder != nil && g.MaxDepth > 0 && childDepth > g.MaxDepth {
		return "", fmt.Errorf(
			"已达到最大子任务深度 %d，不能再继续拆分：当前任务深度为 %d",
			g.MaxDepth, parentDepth,
		)
	}

	task := &model.Task{
		Description:    desc,
		EventType:      eventType,
		EventSource:    parentID,
		ParentTaskID:   parentID,
		ReplyToAgentID: g.AgentID,
		BatchID:        parentID,
		Depth:          childDepth,
		// 单交付物任务的语义默认是"执行一次"。未显式指定时显式置 1，
		// 不落进 store 的 default_concurrency 兜底——该配置是兼容层，
		// 大于 1 会让多个 Agent 重复执行同一任务（2026-07-22 排查）。
		MaxConcurrency: 1,
	}
	if v, ok := args["max_concurrency"].(float64); ok && v >= 1 {
		task.MaxConcurrency = int(v)
	}
	if prio, _ := args["priority"].(string); prio != "" {
		switch prio {
		case "low":
			task.Priority = -1
		case "high":
			task.Priority = 1
		default: // "normal"
			task.Priority = 0
		}
	}
	if deps, _ := args["dependencies"].(string); deps != "" {
		for _, dep := range strings.Split(deps, ",") {
			dep = strings.TrimSpace(dep)
			if dep != "" {
				task.Dependencies = append(task.Dependencies, dep)
			}
		}
	}
	// 解析 expected_artifacts：逗号分隔的预期产出文件路径
	if exp, _ := args["expected_artifacts"].(string); exp != "" {
		for _, p := range strings.Split(exp, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				task.ExpectedArtifacts = append(task.ExpectedArtifacts, p)
			}
		}
	}

	// 节点能力覆盖（per-node NodeCapability）：tools 子集 + model 覆盖 + isolation 隔离。
	// 三道校验：a. 写入权限 → b. 静态合法 → c. 伴生关系软警告（不拒绝）。
	toolsArg, _ := args["tools"].(string)
	modelArg, _ := args["model"].(string)
	isolationArg, _ := args["isolation"].(string)
	isolationMode := strings.TrimSpace(isolationArg)
	// a. 写入权限：节点能力只能由控制面（AllowNodeCapability=true 的
	//    Scheduler 装配）写入。普通 Worker / Reactor 携带任一参数即拒绝——
	//    它们无权改变 DAG 节点的执行边界，否则认领约束（子集 ⊆ 白名单）
	//    会被非控制面绕过。
	if (strings.TrimSpace(toolsArg) != "" || strings.TrimSpace(modelArg) != "" || isolationMode != "") &&
		!g.AllowNodeCapability {
		return "", fmt.Errorf("发布任务被拒绝: tools/model/isolation 节点能力参数只能由 Scheduler 计划控制面设置")
	}
	var capabilityWarnings []string
	if strings.TrimSpace(toolsArg) != "" || strings.TrimSpace(modelArg) != "" || isolationMode != "" {
		// b. 静态合法：isolation 目前唯一合法值是 "workspace"（写时复制隔离）；
		//    其他值直接报错——拼错的隔离声明若静默忽略会让并行节点失去保护。
		if isolationMode != "" && isolationMode != model.IsolationModeWorkspace {
			return "", fmt.Errorf("发布任务被拒绝: isolation 参数只接受 %q（写时复制工作区隔离），收到 %q", model.IsolationModeWorkspace, isolationMode)
		}
		var capTools []string
		for _, name := range strings.Split(toolsArg, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				capTools = append(capTools, name)
			}
		}
		// b. 静态合法（续）：工具名必须真实存在（与 tool_profiles 同一权威清单）；
		//    model 只做非空/去空白校验——模型名是否可用由 LLM 端点决定，
		//    发布面不维护模型清单。
		if err := ValidateToolNames(capTools); err != nil {
			return "", fmt.Errorf("发布任务被拒绝: tools 参数含未注册工具名: %w", err)
		}
		capModel := strings.TrimSpace(modelArg)
		task.Capability = &model.NodeCapability{Tools: capTools, Model: capModel}
		if isolationMode != "" {
			task.Capability.Isolation = &model.IsolationSpec{Mode: isolationMode}
		}
		// c. 伴生关系软警告：子集明显不自洽时在返回文本里提示，但不硬拒——
		//    合法极简节点（如纯 web_fetch 调查节点）不应被误伤。
		capabilityWarnings = nodeCapabilityWarnings(capTools)
	}
	if g.RouteValidator != nil && len(task.ExpectedArtifacts) > 0 &&
		!g.RouteValidator.CanRouteForPlan(parentOwnerID, eventType, "write_file") &&
		!g.RouteValidator.CanRouteForPlan(parentOwnerID, eventType, "edit_file") {
		return "", fmt.Errorf("发布任务被拒绝: event_type=%q 的 ready route 没有 write_file/edit_file，不能声明 expected_artifacts=%v", eventType, task.ExpectedArtifacts)
	}

	// 校验 dependencies：每个依赖任务必须真实存在于 Store 中（层 B 兜底）。
	//
	// 主校验已由 hook/builtin.DependencyValidatorHook 承担（层 A），它会在 PreCall
	// 阶段做 UUID 格式前置校验 + store 存在性校验，并返回指导性错误消息。
	// 本处保留最简兜底的原因与 PathBoundaryHook 决策 A1 一致：即使所有 hook 被
	// 禁用（V6/V9 回归验证场景），也不能让永远无法满足的依赖进入 store。
	for _, depID := range task.Dependencies {
		if _, err := g.Store.GetTask(depID); err != nil {
			return "", fmt.Errorf("依赖任务 %s 不存在（meta 层兜底校验）", depID)
		}
	}

	// 能力边界硬校验：explore 任务由只读 Explorer 执行，无写权限，
	// 不能声明 expected_artifacts，否则会陷入"声称完成→校验失败→重试"死循环。
	// 注：这里硬编码 "explore"，与 config.ExplorerEventType 默认值保持一致。
	// Keep the legacy fallback only for compatibility call sites that do not
	// have the runtime registry. Scheduler paths use the capability-based check
	// above, so a custom route named "explore" is not incorrectly treated as
	// read-only when it actually guarantees write_file/edit_file.
	if g.RouteValidator == nil && eventType == "explore" && len(task.ExpectedArtifacts) > 0 {
		return "", fmt.Errorf(
			"发布任务被拒绝: explore 类型任务由只读 Explorer 执行，不能声明 expected_artifacts。"+
				"如需产出文件，请将 event_type 留空改用执行代理（Worker）。当前传入: %v",
			task.ExpectedArtifacts,
		)
	}

	if err := g.Store.PublishTask(task); err != nil {
		return "", fmt.Errorf("发布任务失败: %w", err)
	}

	// Phase 3：scheduler 注入了 BatchTracker 时，把新 task ID 追加到
	// scheduler task.SchedulerBatch。worker / explorer 不注入则跳过。
	// 失败仅记日志，不阻塞用户的任务发布——batch 跟踪是辅助能力。
	if g.BatchTracker != nil {
		if err := g.BatchTracker.AppendBatch(task.ID); err != nil {
			log.Printf("[meta] BatchTracker.AppendBatch 失败 (task=%s): %v", task.ID, err)
		}
	}

	result := fmt.Sprintf("已创建任务: id=%s, depth=%d, description=%s", task.ID, childDepth, desc)
	if len(capabilityWarnings) > 0 {
		result += "；节点能力警告: " + strings.Join(capabilityWarnings, "；")
	}
	return result, nil
}

// nodeCapabilityWarnings 对节点工具子集做伴生关系检查，返回软警告列表。
// 只提示明显不自洽的组合，不拒绝——合法极简节点（纯 web_fetch 调查等）
// 不应被误伤；执行类工具集是否够用的最终判定在认领侧（子集 ⊆ 白名单）。
func nodeCapabilityWarnings(capTools []string) []string {
	if len(capTools) == 0 {
		return nil
	}
	set := make(map[string]bool, len(capTools))
	for _, name := range capTools {
		set[name] = true
	}
	var warnings []string
	// require-read-before-write Gate 要求写前必须先读：子集带写工具却不带
	// read_file 时，第一次写会被该 Gate 永远拦截，节点必然失败重试。
	if (set["write_file"] || set["edit_file"]) && !set["read_file"] {
		warnings = append(warnings, "tools 含 write_file/edit_file 但不含 read_file——require-read-before-write Gate 会拦截所有写入，节点将无法产出文件")
	}
	// 收尾通道提示：子集不含任何执行类工具（读/写/web/shell）时，节点只能
	// 以纯文字响应收尾；若发布方期待它操作文件或网络，结果必然落空。
	execTools := []string{"read_file", "list_dir", "grep_search", "glob_search", "write_file", "edit_file", "web_search", "web_fetch", "run_shell"}
	hasExec := false
	for _, name := range execTools {
		if set[name] {
			hasExec = true
			break
		}
	}
	if !hasExec {
		warnings = append(warnings, "tools 子集不含任何执行类工具（read/write/web/shell），该节点只能以纯文字响应收尾")
	}
	return warnings
}

// sendMessage 是 worker.MakeSendMessageTool 的内联端口，避免循环依赖。
//
// Phase 2 改动：邮件链跳数继承（B5）。读取当前任务的 MailChainDepth，
// 写入 outgoing message 的 ChainDepth = parent.MailChainDepth + 1。
// 这条值随后被 ChainDepthLimitHook 在 BeforeSend 阶段校验，超过
// cfg.MailChainMaxDepth 的消息被拒绝，从而打断邮件级联爆炸。
//
// 兜底语义：
//   - g.Holder == nil（Scheduler 模式）→ chainDepth = 0
//   - g.Store == nil（不应发生，但防御）→ chainDepth = 0
//   - 当前任务 ID 为空 → chainDepth = 0
//   - GetTask 失败 → chainDepth = 0（不阻断 send_message，只是失去链跟踪）
func (g MetaGroup) sendMessage(ctx context.Context, args map[string]any) (string, error) {
	to, _ := args["to"].(string)
	content, _ := args["content"].(string)
	if to == "" {
		return "", fmt.Errorf("缺少 to 参数")
	}
	if content == "" {
		return "", fmt.Errorf("缺少 content 参数")
	}

	msgType, _ := args["msg_type"].(string)
	if msgType == "" {
		msgType = mailbox.MsgTypeInfo
	}
	priority, _ := args["priority"].(string)
	if priority == "" {
		priority = mailbox.PriorityNormal
	}
	summary, _ := args["summary"].(string)

	// 读当前任务的 MailChainDepth 作为新邮件链深度的起点。
	// 不存在 / 出错时退化为 0，与"用户 /steer 投递的初始邮件"等价。
	chainDepth := 0
	if g.Holder != nil && g.Store != nil {
		if taskID := g.Holder.Get(); taskID != "" {
			if task, err := g.Store.GetTask(taskID); err == nil && task != nil {
				chainDepth = task.MailChainDepth + 1
			}
		}
	}

	msg := mailbox.Message{
		From:       g.AgentID,
		To:         to,
		Content:    content,
		Summary:    summary,
		Type:       msgType,
		Priority:   priority,
		SentAt:     time.Now(),
		ChainDepth: chainDepth,
	}
	// H2b Effect Journal：发送前先落账（prepared）。Target 只载收件人，
	// ArgsDigest 是 路由+正文 的 digest（脱敏：正文不进账本）；
	// Policy=manual_only——外部消息不得重发，恢复裁决不自动执行任何动作。
	effID := effectPrepare(g.EffectJournal, ctx, g.AgentID,
		effect.KindMessage, to,
		digest12([]byte(g.AgentID+"->"+to+"\n"+content)), effect.PolicyManualOnly)
	if err := g.MBRegistry.Send(msg); err != nil {
		// 发送返回错误：广播场景可能已部分投递，外部状态不可知 → unknown。
		effectMarkUnknown(g.EffectJournal, effID, "发送返回错误: "+err.Error())
		return "", err
	}
	effectSettle(g.EffectJournal, effID,
		fmt.Sprintf("已受理（type=%s priority=%s to=%s）", msgType, priority, to))
	if to == "*" {
		return "消息已广播给所有代理", nil
	}
	return fmt.Sprintf("消息已发送给 %s (type=%s, priority=%s)", to, msgType, priority), nil
}
