package explorer

import (
	"context"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/webtool"
	"agentgo/internal/worker"
)

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
// Hook 系统参数（hookReg / storeView / recordToolCall）均允许 nil，详见
// agent.NewLLMExecutor 的说明。
func New(s store.TaskStore, r roster.Roster, llmClient llm.Client, cfg *config.Config, cancelReg *store.TaskCancelRegistry, mbRegistry *mailbox.Registry, hookReg *hook.ToolHookRegistry, storeView store.StoreHookView, recordToolCall func(string, store.ToolCallRecord), searchProvider ...webtool.SearchProvider) *Explorer {
	const agentID = "explorer-1"
	fileCache := agent.NewFileStateCache(50)
	workdir := &tools.DefaultWorkdir{ProjectRoot: cfg.ProjectRoot}

	var sp webtool.SearchProvider
	if len(searchProvider) > 0 {
		sp = searchProvider[0]
	}

	// 通过 ToolGroup 组合 Explorer 的只读 + 网络工具集
	toolReg := agent.NewToolRegistry()
	tools.RegisterGroups(toolReg,
		tools.LocalReadGroup{Workdir: workdir, Cache: fileCache},
		tools.WebGroup{Provider: sp},
		tools.MetaGroup{MBRegistry: mbRegistry, AgentID: agentID}, // Store=nil → 不注册 publish_task
	)

	executor := agent.NewLLMExecutor(llmClient, toolReg, hookReg, storeView, recordToolCall, systemPrompt)

	a := agent.NewAgent(
		agentID,
		cfg.ExplorerEventType, // "explore"
		s, r, executor,
		cfg.AgentMaxLoops,
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = cfg.MaxRetry
	a.IdleThreshold = 0 // 预制代理不因空闲退出
	a.CompactTokenThreshold = cfg.CompactTokenThreshold
	a.CompactKeepRecent = cfg.CompactKeepRecent
	a.FileCache = fileCache
	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(agentID, cfg.ExplorerEventType)
		a.MailRegistry = mbRegistry
	}
	a.TeamSnapshot = func() string { return worker.BuildTeamSnapshot(agentID, s, mbRegistry) }

	return &Explorer{agent: a}
}

// Run 启动调查代理的轮询循环，阻塞直到 ctx 取消。
func (e *Explorer) Run(ctx context.Context) {
	e.agent.Run(ctx)
}
