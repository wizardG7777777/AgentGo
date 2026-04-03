package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

// CLI 处理用户输入，分发命令，发送事件到调度器。
type CLI struct {
	store     store.TaskStore
	eventCh   chan<- model.Event
	cancelFn  context.CancelFunc
	scheduler *scheduler.Scheduler
	reader    io.Reader
	writer    io.Writer
}

// New 创建 CLI 实例。reader/writer 用于输入输出，方便测试注入。
func New(s store.TaskStore, eventCh chan<- model.Event, cancelFn context.CancelFunc, sched *scheduler.Scheduler, reader io.Reader, writer io.Writer) *CLI {
	return &CLI{
		store:     s,
		eventCh:   eventCh,
		cancelFn:  cancelFn,
		scheduler: sched,
		reader:    reader,
		writer:    writer,
	}
}

// Run 启动 CLI 主循环，阻塞直到 ctx 取消、用户输入 /quit、或 stdin 关闭。
func (c *CLI) Run(ctx context.Context) {
	scanner := bufio.NewScanner(c.reader)
	fmt.Fprint(c.writer, "> ")

	for {
		// 用 channel 让阻塞的 Scan 可以被 ctx 取消
		lineCh := make(chan string, 1)
		eofCh := make(chan struct{}, 1)

		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
			} else {
				eofCh <- struct{}{}
			}
		}()

		select {
		case <-ctx.Done():
			return
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

	case line == "/help":
		c.printHelp()

	case strings.HasPrefix(line, "/"):
		fmt.Fprintf(c.writer, "[错误] 未知命令: %s，输入 /help 查看帮助\n", line)

	default:
		// 用户自由文本 → 发送 EventUserInput
		c.eventCh <- model.Event{
			Type:    model.EventUserInput,
			Payload: map[string]string{"text": line},
		}
	}

	return false
}

func (c *CLI) toggleMode() {
	current := c.scheduler.GetMode()
	var newMode scheduler.Mode
	if current == scheduler.ModeImmediate {
		newMode = scheduler.ModePlan
	} else {
		newMode = scheduler.ModeImmediate
	}
	c.scheduler.SetMode(newMode)

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

func (c *CLI) printHelp() {
	fmt.Fprintln(c.writer, "可用命令:")
	fmt.Fprintln(c.writer, "  /status       — 查看活跃任务")
	fmt.Fprintln(c.writer, "  /cancel <id>  — 取消指定任务")
	fmt.Fprintln(c.writer, "  /mode         — 切换即时/计划模式")
	fmt.Fprintln(c.writer, "  /quit         — 退出程序")
	fmt.Fprintln(c.writer, "  /help         — 显示此帮助")
	fmt.Fprintln(c.writer, "  其他文本      — 作为用户请求发送给调度器")
}
