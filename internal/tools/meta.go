package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
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
// 通信、检视与图工具用它取得当前调用的来源身份。
type TaskHolder interface {
	Get() string
}

// RouteValidator is the runtime authority for task routing. Production
// Scheduler and runners inject it so a catalog entry, stale event_type, or a
// Team route owned by another request scope cannot create an invalid Task.
// CanRouteForPlan 第一参数是路由归属 scope ID；图使用 graph:<id>，空串为全局。
type RouteValidator interface {
	CanRouteForPlan(ownerScopeID, eventType string, requiredTools ...string) bool
}

// RouteCapabilityResolver 返回某个 owner scope 下，指定 route 的每个可认领
// listener 都保证具备的工具集合，供节点 route 能力校验使用。
type RouteCapabilityResolver interface {
	RouteCapabilitiesForPlan(ownerScopeID, eventType string) ([]string, bool)
}

// RouteCapabilityEnvelopeResolver 返回某个 owner scope 下，指定 route 的任一
// 可认领 listener **可能拥有**的工具并集。必需能力用上面的交集证明；禁止能力
// / 正向闭集必须用并集证明，否则一个低权限 listener 会把另一个高权限
// listener 的额外工具从交集中隐藏。AgentRegistry 实现此接口。
type RouteCapabilityEnvelopeResolver interface {
	RouteCapabilityEnvelopeForPlan(ownerScopeID, eventType string) ([]string, bool)
}

// CommunicationGroup 注册信息传递及用户提问。Store/Holder 提供来源任务身份；
// MBRegistry/Interactions 分别决定通信、用户提问入口是否可注册。
type CommunicationGroup struct {
	Store  store.TaskStore
	Holder TaskHolder

	MBRegistry          *mailbox.Registry
	AgentID             string
	Interactions        *interaction.Service
	SessionID           func() string
	InteractionWaitHook func(waiting bool)

	// EffectJournal 是 V6 §4 H2b 副作用账本（internal/effect）；
	// nil 时 send_message 不记账（行为与引入账本前完全一致）。
	EffectJournal *effect.Journal
}

// Register 把 send_message / request_user_input 注册到 r。
// 各自的依赖缺失时自动跳过对应工具。
func (g CommunicationGroup) Register(r *agent.ToolRegistry) {
	if g.MBRegistry != nil {
		r.Register("send_message", "向指定代理或广播范围传递信息。问题、回复和通知均不唤醒、不打断代理，不创建任务或改变图状态。返回投递回执，不代表已阅读或已执行。",
			schema.Object().
				String("to", "真实代理 ID 或 * 表示广播", true).
				String("content", "消息正文", true).
				String("summary", "消息摘要", false).
				Enum("msg_type", "信息类型", []string{"info", "question", "reply"}, false).
				String("reply_to", "回复所关联的消息 ID", false).
				Build(), g.sendMessage)
	}
	if g.Interactions != nil {
		params := schema.Object().
			String("prompt", "需要用户回答的明确问题", true).
			String("options_json", "JSON 数组（2-8 项）；每项只能包含 id、label、可选 description、可选 requires_text。普通 Agent 提问只会收到回答，不会授予 Shell 权限或改变图执行状态", true).
			Build()
		params["additionalProperties"] = false
		r.Register(
			"request_user_input",
			"向用户提出一个结构化选择题并等待回答。只返回用户选择，不授予 run_shell 执行权限，不创建或改变图节点。",
			params,
			g.requestUserInput,
		)
	}
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
func (g CommunicationGroup) sendMessage(ctx context.Context, args map[string]any) (string, error) {
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
	for key := range args {
		switch key {
		case "to", "content", "summary", "msg_type", "reply_to":
		default:
			return "", fmt.Errorf("send_message 不支持参数 %q；通信不能携带控制指令", key)
		}
	}
	switch msgType {
	case mailbox.MsgTypeInfo, mailbox.MsgTypeQuestion, mailbox.MsgTypeReply:
	default:
		return "", fmt.Errorf("消息类型 %q 无效：仅允许 info/question/reply", msgType)
	}
	priority := mailbox.PriorityNormal
	replyTo, _ := args["reply_to"].(string)
	if msgType == mailbox.MsgTypeReply && strings.TrimSpace(replyTo) == "" {
		return "", fmt.Errorf("reply 消息缺少 reply_to")
	}
	summary, _ := args["summary"].(string)

	// 读当前任务的 MailChainDepth 作为新邮件链深度的起点。
	// 不存在 / 出错时退化为 0，与"用户 /steer 投递的初始邮件"等价。
	chainDepth := 0
	var sourceTask *model.Task
	if g.Holder != nil && g.Store != nil {
		if taskID := g.Holder.Get(); taskID != "" {
			if task, err := g.Store.GetTask(taskID); err == nil && task != nil {
				chainDepth = task.MailChainDepth + 1
				sourceTask = task
			}
		}
	}

	msg := mailbox.Message{
		DeliveryOnly: true, ID: uuid.NewString(), ReplyTo: replyTo,
		From:       g.AgentID,
		To:         to,
		Content:    content,
		Summary:    summary,
		Type:       msgType,
		Priority:   priority,
		SentAt:     time.Now(),
		ChainDepth: chainDepth,
	}
	if sourceTask != nil && sourceTask.RunID != "" {
		msg.SourceTaskID = sourceTask.ID
		msg.RunID = sourceTask.RunID
		if g.SessionID != nil {
			msg.SessionID = g.SessionID()
		}
	}
	// H2b Effect Journal：发送前先落账（prepared）。Target 只载收件人，
	// ArgsDigest 是 路由+正文 的 digest（脱敏：正文不进账本）；
	// Policy=manual_only——外部消息不得重发，恢复裁决不自动执行任何动作。
	effID, err := effectPrepare(g.EffectJournal, ctx, g.AgentID,
		effect.KindMessage, to,
		digest12([]byte(g.AgentID+"->"+to+"\n"+content)), effect.PolicyManualOnly)
	if err != nil {
		return "", err
	}
	if err := g.MBRegistry.Send(msg); err != nil {
		// 发送返回错误：广播场景可能已部分投递，外部状态不可知 → unknown。
		if journalErr := effectMarkUnknown(g.EffectJournal, effID, "发送返回错误: "+err.Error()); journalErr != nil {
			return "", journalErr
		}
		return "", err
	}
	if err := effectSettle(g.EffectJournal, effID,
		fmt.Sprintf("已受理（type=%s priority=%s to=%s）", msgType, priority, to), true); err != nil {
		return "", err
	}
	description := "消息已投递"
	if to == "*" {
		description = "消息已广播"
	}
	receipt, err := json.Marshal(map[string]any{
		"schema": "agentgo.message-receipt/v1", "message_id": msg.ID,
		"reply_to": msg.ReplyTo, "to": to, "status": "delivered", "description": description,
	})
	return string(receipt), err
}
