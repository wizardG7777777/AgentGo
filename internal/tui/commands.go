package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
)

// handleSubmit 处理用户回车提交的一行文本。
// 返回 quit=true 表示应退出 TUI（仅 /quit 触发）。
//
// 非 / 开头：作为自由文本发送 EventUserInput。
// / 开头：解析命令名 + 参数后分发。
func (m *Model) handleSubmit(line string) (quit bool) {
	if !strings.HasPrefix(line, "/") {
		m.sendUserText(line)
		return false
	}

	cmd, args := splitCommand(line)
	switch cmd {
	case "/quit":
		m.appendMsg("[退出] 正在关闭...", msgInfo)
		m.deps.CancelFn()
		return true
	case "/help":
		m.appendMsg(helpText, msgInfo)
	case "/status":
		m.printStatus()
	case "/cancel":
		m.cancelTask(strings.TrimSpace(args))
	case "/mode":
		m.toggleMode()
	case "/steer":
		m.steer(args)
	case "/new":
		m.newSession()
	case "/session":
		m.handleSessionCmd(strings.TrimSpace(args))
	default:
		m.appendMsg(fmt.Sprintf("[错误] 未知命令: %s（输入 /help 查看帮助）", line), msgError)
	}
	return false
}

func splitCommand(line string) (cmd, args string) {
	parts := strings.SplitN(line, " ", 2)
	cmd = parts[0]
	if len(parts) > 1 {
		args = parts[1]
	}
	return
}

const helpText = `可用命令:
  /status              — 查看活跃任务
  /cancel <id>         — 取消指定任务
  /steer <agent> <msg> — 向指定代理发送用户纠偏消息
  /mode                — 切换即时/计划模式
  /new                 — 创建新 Session
  /session             — 列出 Session；/session <序号> 选择
  /help                — 显示此帮助
  /quit                — 退出程序
  其他文本             — 作为用户请求发送给调度器

审批面板键位: 1=通过  2=拒绝  3=输入指导  4=永远允许此模式（本进程内）`

func (m *Model) sendUserText(text string) {
	if m.deps.SessionMgr != nil {
		m.deps.SessionMgr.RecordFirstInput(text)
		m.deps.SessionMgr.IncrementTaskCount()
	}
	evt := model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": text},
	}
	select {
	case m.deps.EventCh <- evt:
		m.appendMsg("[已提交] "+truncate(text, 60), msgInfo)
	case <-time.After(5 * time.Second):
		m.appendMsg("[警告] 系统繁忙，请稍后重试", msgWarn)
	}
}

func (m *Model) printStatus() {
	tasks, err := m.deps.Store.ScanAll()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[错误] 读取任务列表失败: %v", err), msgError)
		return
	}
	nonTerminal := 0
	for _, task := range tasks {
		if model.IsTerminal(task.Status) {
			continue
		}
		idShort := task.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		m.appendMsg(fmt.Sprintf("  [%s] %s — %s", idShort, task.Status, task.Description), msgLog)
		nonTerminal++
	}
	if nonTerminal == 0 {
		m.appendMsg("  （无活跃任务）", msgLog)
	} else {
		m.appendMsg(fmt.Sprintf("  共 %d 个活跃任务", nonTerminal), msgLog)
	}
}

func (m *Model) cancelTask(taskID string) {
	if taskID == "" {
		m.appendMsg("[错误] 用法: /cancel <taskID>", msgError)
		return
	}
	err := store.TransitionStateWithCancelSource(m.deps.Store, taskID, model.TaskStatusPending, model.TaskStatusCancelled, "user")
	if err != nil {
		err = store.TransitionStateWithCancelSource(m.deps.Store, taskID, model.TaskStatusProcessing, model.TaskStatusCancelled, "user")
	}
	if err != nil {
		m.appendMsg(fmt.Sprintf("[错误] 取消失败: %v", err), msgError)
	} else {
		m.appendMsg(fmt.Sprintf("[取消] 任务 %s 已取消", taskID), msgInfo)
	}
}

func (m *Model) toggleMode() {
	if m.deps.Scheduler == nil || m.deps.Scheduler.Mode == nil {
		m.appendMsg("[模式] 模式切换不可用（scheduler 未注入 ModeStore）", msgWarn)
		return
	}
	mode := m.deps.Scheduler.Mode
	current := mode.Get()
	var next scheduler.Mode
	if current == scheduler.ModeImmediate {
		next = scheduler.ModePlan
	} else {
		next = scheduler.ModeImmediate
	}
	mode.Set(next)
	if next == scheduler.ModeImmediate {
		m.appendMsg("[模式] 即时模式", msgInfo)
	} else {
		m.appendMsg("[模式] 计划模式", msgInfo)
	}
}

func (m *Model) steer(args string) {
	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	if len(parts) < 2 || parts[0] == "" || strings.TrimSpace(parts[1]) == "" {
		m.appendMsg("[错误] 用法: /steer <agentID> <消息内容>", msgError)
		return
	}
	if m.deps.Mailbox == nil {
		m.appendMsg("[错误] 邮箱系统未启用", msgError)
		return
	}
	agentID := parts[0]
	content := parts[1]
	msg := mailbox.Message{
		From:     "user",
		To:       agentID,
		Content:  content,
		Summary:  content,
		Type:     mailbox.MsgTypeSteer,
		Priority: mailbox.PriorityHigh,
		SentAt:   time.Now(),
	}
	if err := m.deps.Mailbox.Send(msg); err != nil {
		m.appendMsg(fmt.Sprintf("[错误] 发送失败: %v", err), msgError)
		return
	}
	m.appendMsg(fmt.Sprintf("[steer] 已向 %s 发送用户消息", agentID), msgInfo)
}

func (m *Model) newSession() {
	if m.deps.SessionMgr == nil {
		m.appendMsg("[错误] Session 管理器未启用", msgError)
		return
	}
	if err := m.deps.SessionMgr.Close(); err != nil {
		m.appendMsg(fmt.Sprintf("[警告] 关闭当前 Session 失败: %v", err), msgWarn)
	}
	sess, err := m.deps.SessionMgr.CreateNew()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[错误] 创建新 Session 失败: %v", err), msgError)
		return
	}
	id := sess.ID
	if len(id) > 8 {
		id = id[:8]
	}
	m.appendMsg(fmt.Sprintf("[session] 新 Session 已创建: %s", id), msgInfo)
}

// handleSessionCmd 实现 /session 列表展示与 /session <序号> 切换。
//
// v1 把旧版"先列出再等待二次输入"的两步交互拆为两次命令，避免 bubbletea 状态机
// 多一个 awaiting-selection 子状态。后续可在此引入交互式 list 选择器。
func (m *Model) handleSessionCmd(arg string) {
	if m.deps.SessionMgr == nil {
		m.appendMsg("[错误] Session 管理器未启用", msgError)
		return
	}
	sessions, err := m.deps.SessionMgr.List()
	if err != nil {
		m.appendMsg(fmt.Sprintf("[错误] 获取 Session 列表失败: %v", err), msgError)
		return
	}
	if len(sessions) == 0 {
		m.appendMsg("[session] Empty session list", msgInfo)
		return
	}

	if arg == "" {
		m.appendMsg("[session] Session 列表（用 /session <序号> 切换）:", msgInfo)
		for i, meta := range sessions {
			desc := meta.FirstUserInput
			if desc == "" {
				desc = "（无描述）"
			}
			createdAt := meta.CreatedAt
			if len(createdAt) >= 16 {
				createdAt = createdAt[:10] + " " + createdAt[11:16]
			}
			idShort := meta.SessionID
			if len(idShort) > 8 {
				idShort = idShort[:8]
			}
			m.appendMsg(fmt.Sprintf("  [%d] %s | %s | %s", i+1, idShort, createdAt, desc), msgLog)
		}
		return
	}

	idx, err := strconv.Atoi(arg)
	if err != nil || idx < 1 || idx > len(sessions) {
		m.appendMsg(fmt.Sprintf("[错误] 无效的选择: %s，请输入 1-%d 的序号", arg, len(sessions)), msgError)
		return
	}
	selected := sessions[idx-1]
	idShort := selected.SessionID
	if len(idShort) > 8 {
		idShort = idShort[:8]
	}
	m.appendMsg(fmt.Sprintf("[session] 已选择 Session: %s（快照恢复需要后续阶段支持）", idShort), msgInfo)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
