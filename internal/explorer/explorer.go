package explorer

import (
	"context"
	"sync"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/webtool"
)

// currentTaskHolder 线程安全地保存当前正在执行的任务 ID，实现 tools.TaskHolder。
// 与 worker.currentTaskHolder 结构一致——send_message 通过它读 task.MailChainDepth。
type currentTaskHolder struct {
	mu sync.Mutex
	id string
}

func (h *currentTaskHolder) Set(id string) { h.mu.Lock(); h.id = id; h.mu.Unlock() }
func (h *currentTaskHolder) Get() string   { h.mu.Lock(); defer h.mu.Unlock(); return h.id }

// explorerMaxRetries 是 Explorer 角色的任务级重试上限。
//
// 角色语义：Explorer 执行只读调查，语义上与 Worker 同档（同样由 scheduler 发布，
// 遇 LLM 故障应有限重试 + 崩溃汇报）。该常量故意不暴露 yaml 配置——理由同
// workerMaxRetries。
const explorerMaxRetries = 3

const systemPrompt = `你是一个调查代理（Explorer），专门执行只读的信息检索和验证任务。

你的职责：
- 读取项目文件，了解代码结构和内容
- 搜索项目中的关键字和模式
- 验证历史结论是否仍然成立
- 使用 web_search 和 web_fetch 进行网络调查与事实核查
- 返回简洁明确的调查结果

你的限制：
- 只能执行只读操作，不能修改任何文件
- 结果应简短明确：结论成立/结论已过时（附当前状态摘要）
- 不要猜测，只报告你实际观察到的内容

# ⚠️ 如何结束调查并交付结果（机制说明，必读）

**Explorer 没有 report_done 工具**——那是 scheduler 的专属编排工具，不要尝试调用它（会得到 "tool not found" 错误）。

完成任务的**唯一方式**是：
> **当你认为信息收集充分时，本轮响应直接输出一段总结文本，不调用任何工具。**

系统会把这段纯文本响应作为你对调用方（通常是 scheduler）的最终产出提交。只要本轮不调任何工具，任务就会被判定完成、你的文本输出会进入下游任务的上下文。

继续调用工具（即便只是多调一次 read_file）= "我还没完成"，系统会进入下一轮 reactLoop。**没有任何工具能显式标记"调查结束"**——是否结束完全由"你这一轮是否调工具"决定。

## 触发停止输出工具的判断（任一满足即应立即停下并输出总结）

- 已经掌握了**足够回答调用方问题**的信息（不要追求"全知"完美覆盖）
- 再读更多文件只会是**重复确认**，不会产生新的决策价值
- **loop 轮次接近上限**（通常 MaxLoops ≈ 10，过半时就该警觉）
- **context 快被文件内容填满**，新的 read 可能挤掉前面关键内容
- 任务描述里用户带有**"简短 / 不用详细 / 不用撰写报告"** 等轻量化意图时

## 不停下的后果

系统有两道硬限制会强制终止你：
1. **reactLoop MaxLoops** 到达 → 触发 RetryRollback，本次积累的 history 绝大部分作废
2. **watchdog 超时**（默认 5 分 30 秒）→ 直接 FailTaskBySystem，任务进入失败终态

**两种强制终止都会导致你已做的所有工作全部丢失**——read_file 读到的内容、grep_search 的匹配都只停留在你自己的 history 里，**没有 report_done 等价物、没有任何其他通道**能让你的发现传递出去。唯一能让下游看到你工作的方式就是"停下来 + 输出总结"。

## 反面案例（2026-04-23 真实事故）

一个 explorer 被要求"调查多 Agent 交互机制"，连续 9 轮 read_file 读了 20+ 个文件，从未停下输出总结；5m52s 后被 watchdog 杀掉；上游 scheduler 看不到任何分析成果，又发布了一次几乎相同的任务；同样模式又重复 6 次，52 分钟后用户 Ctrl+C。**你若不主动停下输出总结，这就是你的命运**。

代理间通信规范：
- 使用 send_message 时，必须填写 summary（一句话重点）
- 向 scheduler 汇报调查结果时用 msg_type="info"
- 收到 <agent-mail type="question"> 时，**如果你能立即给出对发送方有价值的回答**再回复；如果对方只是在做通信测试或广播闲聊，可以忽略——盲目回复会触发邮件级联爆炸
- 收到 <agent-mail type="steer"> 时（尤其 from=user），应立即调整调查方向
- **严禁的"通信测试"反例**：不要为了"验证通信通畅"而主动 send_message。系统的 mail-notifier 会把每条消息变成对方的唤醒任务，引发雪崩

调查与研究类任务的额外约束：
- 每个关键结论（claim）必须对应可溯源的来源 URL，在报告中以"[来源: URL | 日期]"形式标注
- 无法找到来源的结论必须显式标注"[未验证]"，不得以确定口吻呈现
- 禁止凭推断填补信息空白——宁可报告"未找到该信息"，也不可捏造合理细节
- 调查近期事件时，优先使用 time_range="week" 或 "month" 参数缩小时间范围
- 搜索广度要求：至少使用不同关键词执行 3 次独立 web_search，再进行总结

"先读后报告"红线（防止凭空捏造）：
- 若任务要求"调查/分析/验证"已存在的材料（文档、前序任务结果），第一步必须是 list_dir 或 read_file
- 如果你看到"前置任务结果"段中提到了具体的文件路径，那是上游 worker 真实写入的产物，
  **必须 read_file 读取**后再做调查结论。不要只看上游的文本摘要就下结论`

// Explorer 是轻量级只读调查代理，内部组合 agent.Agent。
type Explorer struct {
	agent *agent.Agent
}

// New 创建调查代理。使用低成本 LLM 和只读工具集 + 网络工具 + send_message。
// searchProvider 可为 nil（变参未提供），表示不注册 web_search/web_fetch 工具。
//
// 工具集组合：LocalReadGroup + WebGroup + MetaGroup（无 Holder/Store=publish_task 不注册）
// 编译期保证 Explorer 不持有 Roster/ApprovalCh/Store，因此无法获得写入、shell、publish 能力。
//
// Hook 系统参数分为两组：
//   - hookReg / storeView / recordToolCall: Tool Hook（Phase 1）接入点
//   - agentHookReg / agentStoreView / agentRosterView: Agent Hook（Sprint 1）接入点
//
// 两组参数均允许 nil——对应 hook 路径退化为 no-op。
func New(s store.TaskStore, r roster.Roster, llmClient llm.Client, cfg *config.Config, cancelReg *store.TaskCancelRegistry, mbRegistry *mailbox.Registry, hookReg *hook.ToolHookRegistry, storeView store.StoreHookView, recordToolCall func(string, store.ToolCallRecord), agentHookReg *hook.AgentHookRegistry, agentStoreView hook.AgentStoreView, agentRosterView hook.AgentRosterView, allowedTools []string, searchProvider ...webtool.SearchProvider) *Explorer {
	const agentID = "explorer-1"
	fileCache := agent.NewFileStateCache(50)
	workdir := &tools.DefaultWorkdir{ProjectRoot: cfg.ProjectRoot}
	holder := &currentTaskHolder{}

	var sp webtool.SearchProvider
	if len(searchProvider) > 0 {
		sp = searchProvider[0]
	}

	// 通过 ToolGroup 组合 Explorer 的只读 + 网络工具集。
	// MetaGroup 注入 Store+Holder 让 send_message 能读当前 task.MailChainDepth；
	// DisablePublishTask=true 保住 Explorer 的只读契约（不以 Store=nil 作为权限开关）。
	toolReg := agent.NewToolRegistryWithAllowlist(allowedTools)
	tools.RegisterGroups(toolReg,
		tools.LocalReadGroup{Workdir: workdir, Cache: fileCache},
		tools.WebGroup{Provider: sp},
		tools.MetaGroup{
			Store:              s,
			Holder:             holder,
			MBRegistry:         mbRegistry,
			AgentID:            agentID,
			DisablePublishTask: true,
		},
	)

	executor := agent.NewLLMExecutor(llmClient, toolReg, hookReg, storeView, recordToolCall, systemPrompt)

	a := agent.NewAgent(
		agentID,
		cfg.ExplorerEventType, // "explore"
		s, r, executor,
		cfg.AgentMaxLoops,
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = explorerMaxRetries
	a.IdleThreshold = 0 // 预制代理不因空闲退出
	a.CompactTokenThreshold = cfg.CompactTokenThreshold
	a.CompactKeepRecent = cfg.CompactKeepRecent
	a.TransferNoteMaxTokens = cfg.TransferNoteMaxTokens
	a.OnTaskStart = func(taskID string) { holder.Set(taskID) }
	a.OnTaskEnd = func(taskID string, success bool) { holder.Set("") }
	a.FileCache = fileCache
	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(agentID, cfg.ExplorerEventType)
		a.MailRegistry = mbRegistry
	}

	// Agent Hook 接入点（Sprint 1）——取代原来的 a.TeamSnapshot 硬编码注入。
	// nil 时 hook 路径退化为 no-op。
	a.AgentHookReg = agentHookReg
	a.HookStoreView = agentStoreView
	a.HookRosterView = agentRosterView

	return &Explorer{agent: a}
}

// Run 启动调查代理的轮询循环，阻塞直到 ctx 取消。
func (e *Explorer) Run(ctx context.Context) {
	e.agent.Run(ctx)
}
