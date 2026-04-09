package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
)

// CLI 处理用户输入，分发命令，发送事件到调度器。
//
// Phase 3 改动：scheduler 不再是独立类型，而是 *scheduler.Bundle 的复合
// （Agent + Activator + ModeStore）。CLI 只需要 ModeStore 切换 plan/immediate，
// 不再持有 scheduler 整体。
type CLI struct {
	store      store.TaskStore
	eventCh    chan<- model.Event
	cancelFn   context.CancelFunc
	mode       *scheduler.ModeStore         // 由 scheduler.Bundle 提供，用于 /mode 命令
	mbRegistry *mailbox.Registry            // 邮箱注册表，用于 /steer 命令
	approvalCh <-chan shell.ApprovalRequest // 命令审批请求通道，由 Worker 发送
	reader     io.Reader
	writer     io.Writer
}

// New 创建 CLI 实例。reader/writer 用于输入输出，方便测试注入。
//
// Phase 3 改动：参数 sched 类型从 *scheduler.Scheduler 改为 *scheduler.Bundle。
func New(s store.TaskStore, eventCh chan<- model.Event, cancelFn context.CancelFunc, sched *scheduler.Bundle, mbRegistry *mailbox.Registry, approvalCh <-chan shell.ApprovalRequest, reader io.Reader, writer io.Writer) *CLI {
	var modeStore *scheduler.ModeStore
	if sched != nil {
		modeStore = sched.Mode
	}
	return &CLI{
		store:      s,
		eventCh:    eventCh,
		cancelFn:   cancelFn,
		mode:       modeStore,
		mbRegistry: mbRegistry,
		approvalCh: approvalCh,
		reader:     reader,
		writer:     writer,
	}
}

// Run 启动 CLI 主循环，阻塞直到 ctx 取消、用户输入 /quit、或 stdin 关闭。
// scanner 读取在单独 goroutine 中进行，整个生命周期只启动一次，避免泄漏。
func (c *CLI) Run(ctx context.Context) {
	lineCh := make(chan string)
	eofCh := make(chan struct{})

	go func() {
		scanner := bufio.NewScanner(c.reader)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(eofCh)
	}()

	fmt.Fprint(c.writer, "> ")
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-c.approvalCh:
			c.handleApproval(req, lineCh, ctx)
			fmt.Fprint(c.writer, "> ")
		case line := <-lineCh:
			shouldQuit := c.handleLine(strings.TrimSpace(line))
			if shouldQuit {
				return
			}
			fmt.Fprint(c.writer, "> ")
		case <-eofCh:
			// stdin 关闭或读取错误，终止所有服务
			fmt.Fprintln(c.writer, "[CLI] 输入流关闭，正在退出...")
			c.cancelFn()
			return
		}
	}
}

// handleLine 处理单行输入，返回 true 表示应退出。
func (c *CLI) handleLine(line string) bool {
	if line == "" {
		return false
	}

	switch {
	case line == "/quit":
		fmt.Fprintln(c.writer, "[退出] 正在关闭...")
		c.cancelFn()
		return true

	case line == "/mode":
		c.toggleMode()

	case line == "/status":
		c.printStatus()

	case strings.HasPrefix(line, "/cancel "):
		taskID := strings.TrimSpace(strings.TrimPrefix(line, "/cancel "))
		c.cancelTask(taskID)

	case strings.HasPrefix(line, "/steer "):
		c.steer(strings.TrimPrefix(line, "/steer "))

	case line == "/help":
		c.printHelp()

	case strings.HasPrefix(line, "/"):
		fmt.Fprintf(c.writer, "[错误] 未知命令: %s，输入 /help 查看帮助\n", line)

	default:
		// 用户自由文本 → 发送 EventUserInput（带超时，防止 eventCh 满时卡死）
		evt := model.Event{
			Type:    model.EventUserInput,
			Payload: map[string]string{"text": line},
		}
		select {
		case c.eventCh <- evt:
		case <-time.After(5 * time.Second):
			fmt.Fprintln(c.writer, "[警告] 系统繁忙，请稍后重试")
		}
	}

	return false
}

func (c *CLI) toggleMode() {
	if c.mode == nil {
		fmt.Fprintln(c.writer, "[模式] 模式切换不可用（scheduler 未注入 ModeStore）")
		return
	}
	current := c.mode.Get()
	var newMode scheduler.Mode
	if current == scheduler.ModeImmediate {
		newMode = scheduler.ModePlan
	} else {
		newMode = scheduler.ModeImmediate
	}
	c.mode.Set(newMode)

	if newMode == scheduler.ModeImmediate {
		fmt.Fprintln(c.writer, "[模式] 即时模式")
	} else {
		fmt.Fprintln(c.writer, "[模式] 计划模式")
	}
}

func (c *CLI) printStatus() {
	tasks, err := c.store.ScanAll()
	if err != nil {
		fmt.Fprintf(c.writer, "[错误] 读取任务列表失败: %v\n", err)
		return
	}

	nonTerminal := 0
	for _, task := range tasks {
		if !model.IsTerminal(task.Status) {
			fmt.Fprintf(c.writer, "  [%s] %s — %s\n", task.ID[:8], task.Status, task.Description)
			nonTerminal++
		}
	}

	if nonTerminal == 0 {
		fmt.Fprintln(c.writer, "  （无活跃任务）")
	} else {
		fmt.Fprintf(c.writer, "  共 %d 个活跃任务\n", nonTerminal)
	}
}

func (c *CLI) cancelTask(taskID string) {
	err := c.store.TransitionState(taskID, model.TaskStatusPending, model.TaskStatusCancelled)
	if err != nil {
		err = c.store.TransitionState(taskID, model.TaskStatusProcessing, model.TaskStatusCancelled)
	}
	if err != nil {
		fmt.Fprintf(c.writer, "[错误] 取消失败: %v\n", err)
	} else {
		fmt.Fprintf(c.writer, "[取消] 任务 %s 已取消\n", taskID)
	}
}

func (c *CLI) steer(args string) {
	// 格式: /steer <agentID> <message>
	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		fmt.Fprintln(c.writer, "[错误] 用法: /steer <agentID> <消息内容>")
		fmt.Fprintln(c.writer, "  示例: /steer worker-1 请改用 JSON 格式")
		return
	}
	agentID := parts[0]
	content := parts[1]

	if c.mbRegistry == nil {
		fmt.Fprintln(c.writer, "[错误] 邮箱系统未启用")
		return
	}

	msg := mailbox.Message{
		From:     "user",
		To:       agentID,
		Content:  content,
		Summary:  content, // 用户纠偏消息通常简短，summary 直接用原文
		Type:     mailbox.MsgTypeSteer,
		Priority: mailbox.PriorityHigh,
		SentAt:   time.Now(),
	}
	if err := c.mbRegistry.Send(msg); err != nil {
		fmt.Fprintf(c.writer, "[错误] 发送失败: %v\n", err)
		return
	}
	fmt.Fprintf(c.writer, "[steer] 已向 %s 发送用户消息\n", agentID)
}

func (c *CLI) printHelp() {
	fmt.Fprintln(c.writer, "可用命令:")
	fmt.Fprintln(c.writer, "  /status              — 查看活跃任务")
	fmt.Fprintln(c.writer, "  /cancel <id>         — 取消指定任务")
	fmt.Fprintln(c.writer, "  /steer <agent> <msg> — 向指定代理发送用户纠偏消息")
	fmt.Fprintln(c.writer, "  /mode                — 切换即时/计划模式")
	fmt.Fprintln(c.writer, "  /quit                — 退出程序")
	fmt.Fprintln(c.writer, "  /help                — 显示此帮助")
	fmt.Fprintln(c.writer, "  其他文本             — 作为用户请求发送给调度器")
}

// handleApproval 处理来自 Worker 的命令审批请求，阻塞等待用户输入。
func (c *CLI) handleApproval(req shell.ApprovalRequest, lineCh <-chan string, ctx context.Context) {
	fmt.Fprintf(c.writer, "\n╔══════════════════════════════════════╗\n")
	fmt.Fprintf(c.writer, "║  ⚠ 命令审批请求                      ║\n")
	fmt.Fprintf(c.writer, "╠══════════════════════════════════════╣\n")
	fmt.Fprintf(c.writer, "  代理: %s\n", req.AgentID)
	fmt.Fprintf(c.writer, "  命令: %s\n", req.Command)
	fmt.Fprintf(c.writer, "╠══════════════════════════════════════╣\n")
	fmt.Fprintf(c.writer, "  y = 允许一次  n = 禁止\n")
	fmt.Fprintf(c.writer, "  或直接输入文字作为指导发送给代理\n")
	fmt.Fprintf(c.writer, "╚══════════════════════════════════════╝\n")
	fmt.Fprint(c.writer, "[审批] > ")

	select {
	case <-ctx.Done():
		req.ReplyCh <- shell.ApprovalReply{Approved: false}
	case answer := <-lineCh:
		answer = strings.TrimSpace(answer)
		switch strings.ToLower(answer) {
		case "y", "yes":
			req.ReplyCh <- shell.ApprovalReply{Approved: true}
			fmt.Fprintln(c.writer, "[审批] 已放行")
		case "n", "no", "":
			req.ReplyCh <- shell.ApprovalReply{Approved: false}
			fmt.Fprintln(c.writer, "[审批] 已拒绝")
		default:
			req.ReplyCh <- shell.ApprovalReply{Approved: false, Message: answer}
			fmt.Fprintf(c.writer, "[审批] 已将指导发送给 %s\n", req.AgentID)
		}
	}
}
