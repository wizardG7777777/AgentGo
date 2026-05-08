package userdef

import (
	"fmt"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/trace"
)

// callReactor 是 §6.1 B 选项 "call:" 动作的 reactor.Reactor 实现。
//
// 与 invoke_llm 不同：call 是**直接**工具调用，不经过 LLM——args 中的字符串模板
// 渲染后直接喂给工具。语义上等价于"reactor 自动按你给的参数调一次工具"。
//
// v1 支持的工具集（白名单，loader 阶段校验）：
//   - send_message: args = { to, content, type?, priority? }
//
// 不支持其他工具的理由（v5 边界）：
//   - publish_task / spawn_agent 已是独立动词，重复入口会让用户困惑
//   - 多数工具（如 read_file / web_search）需要 agent 上下文（FileCache / Tool 权限），
//     从 reactor 调用语义不清晰；要扩展时按需逐个加白名单
//
// 通过 Run 内 switch 而非 dispatch table，让"哪些工具可被 call"显式可见。
type callReactor struct {
	name   string
	onKind trace.EventKind
	when   *whenCond
	tool   string
	args   map[string]string
	sender MailboxSender
}

func (r *callReactor) Name() string                 { return r.name }
func (r *callReactor) Subscribe() []trace.EventKind { return []trace.EventKind{r.onKind} }
func (r *callReactor) IsSync() bool                 { return false }
func (r *callReactor) Priority() int                { return 500 }

func (r *callReactor) Run(ev trace.Event) error {
	if !r.when.eval(ev) {
		return nil
	}
	switch r.tool {
	case "send_message":
		return r.runSendMessage(ev)
	}
	// 不应到达——loader 已白名单校验
	return fmt.Errorf("call[%s]: unsupported tool %q (should have been rejected at load)", r.name, r.tool)
}

func (r *callReactor) runSendMessage(ev trace.Event) error {
	if r.sender == nil {
		return fmt.Errorf("call[%s] send_message: mailbox not configured", r.name)
	}
	to := renderTemplate(r.args["to"], ev)
	if to == "" {
		return fmt.Errorf("call[%s] send_message: rendered 'to' is empty", r.name)
	}
	content := renderTemplate(r.args["content"], ev)
	if content == "" {
		return fmt.Errorf("call[%s] send_message: rendered 'content' is empty", r.name)
	}
	msgType := renderTemplate(r.args["type"], ev)
	if msgType == "" {
		msgType = "info"
	}
	priority := renderTemplate(r.args["priority"], ev)
	if priority == "" {
		priority = "normal"
	}
	return r.sender.Send(mailbox.Message{
		From:     "reactor:call",
		To:       to,
		Content:  content,
		Summary:  truncateForSummary(content, 120),
		Type:     msgType,
		Priority: priority,
		SentAt:   time.Now(),
	})
}

// supportedCallTools 是 call: 动作的白名单。loader 用此 set 启动期校验。
var supportedCallTools = map[string]struct{}{
	"send_message": {},
}
