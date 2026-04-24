package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/agent"
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

// MetaGroup 注册任务发布与代理间通信工具。
//
// 字段说明：
//   - Store：任务存储；nil 时不注册 publish_task
//   - Holder：当前任务持有器；nil = Scheduler 语义（无深度限制）；非 nil = Worker 语义
//   - MaxDepth：仅 Holder != nil 时生效；publish_task 创建的子任务深度超过该值时拒绝
//   - MBRegistry：邮箱注册表；nil 时不注册 send_message
//   - AgentID：当前代理 ID（send_message 的发件人）
//   - BatchTracker：（可选，Phase 3）publish_task 成功后追加子任务 ID 到此 tracker；
//     scheduler 注入时把 ID 写入 scheduler task.SchedulerBatch；worker 不注入则无副作用
//   - DisablePublishTask：capability bit——true 时即便 Store 非空也不注册 publish_task。
//     Explorer 用来在注入 Store/Holder（让 send_message 能读 MailChainDepth）的同时
//     保住只读契约。替代了"用 Store=nil 当权限开关"的旧耦合写法。
type MetaGroup struct {
	Store              store.TaskStore
	Holder             TaskHolder
	MaxDepth           int
	MBRegistry         *mailbox.Registry
	AgentID            string
	BatchTracker       BatchTracker
	DisablePublishTask bool
}

// Register 把 publish_task / send_message 注册到 r。
// 各自的依赖缺失时自动跳过对应工具。
func (g MetaGroup) Register(r *agent.ToolRegistry) {
	if g.Store != nil && !g.DisablePublishTask {
		r.Register(
			"publish_task",
			"发布一个新任务到任务队列，由调度器或其他代理认领执行",
			schema.Object().
				String("description", "任务的详细描述", true).
				String("event_type", "任务类型，留空表示由 Worker 认领；填 \"explore\" 表示交给 Explorer 调查", false).
				Enum("priority", "任务优先级，默认 normal", []string{"low", "normal", "high"}, false).
				String("dependencies", "逗号分隔的依赖任务 UUID 列表。每个 ID 必须是之前 publish_task 调用返回的真实 task UUID（形如 7b52b232-4e9b-4b97-8bbc-f3d5927dc814），禁止使用占位符（如 \"task-part1\"、\"A\"、\"<id>\"）或自造 ID。若被依赖任务尚未发布，请先发布被依赖任务、从返回值中读取 id 之后再发布当前任务。留空表示无依赖", false).
				String("expected_artifacts", "逗号分隔的预期产出文件路径列表（相对项目根的相对路径）。任务结束时系统会校验这些文件是否真的写入；缺失则任务失败重试。强烈建议为'报告/总结/文档'类任务填写此字段以防止 report-only 失败", false).
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
	parentDepth := -1 // Scheduler 模式下 childDepth = 0
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
	}

	childDepth := parentDepth + 1
	if g.Holder != nil && g.MaxDepth > 0 && childDepth > g.MaxDepth {
		return "", fmt.Errorf(
			"已达到最大子任务深度 %d，不能再继续拆分：当前任务深度为 %d",
			g.MaxDepth, parentDepth,
		)
	}

	task := &model.Task{
		Description: desc,
		EventType:   eventType,
		EventSource: parentID,
		Depth:       childDepth,
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
	if eventType == "explore" && len(task.ExpectedArtifacts) > 0 {
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
			fmt.Printf("[meta] BatchTracker.AppendBatch 失败 (task=%s): %v\n", task.ID, err)
		}
	}

	return fmt.Sprintf("已创建任务: id=%s, depth=%d, description=%s", task.ID, childDepth, desc), nil
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
	if err := g.MBRegistry.Send(msg); err != nil {
		return "", err
	}
	if to == "*" {
		return "消息已广播给所有代理", nil
	}
	return fmt.Sprintf("消息已发送给 %s (type=%s, priority=%s)", to, msgType, priority), nil
}
