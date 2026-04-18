package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/trace"
)

// progressFlags 跟踪每任务级别的通知状态，防止同一触发条件重复通知。
// 在 processTask 入口初始化为零值。
type progressFlags struct {
	notifiedFileWrite bool
	notifiedSubtask   bool
	notifiedHalfway   bool
}

// detectFileWrite 检测本轮 ExecuteResult 中是否有成功的文件写入。
// 遍历 ToolCalls，匹配 Name 为 "write_file" 或 "edit_file" 的条目，
// 检查对应 ToolResults[i].Content 不以 "错误:" 开头，
// 从 ToolCall.Arguments["path"] 提取文件路径。
// 返回成功写入的文件路径列表。
func detectFileWrite(result ExecuteResult) []string {
	var paths []string
	for i, tc := range result.ToolCalls {
		if tc.Name != "write_file" && tc.Name != "edit_file" {
			continue
		}
		// 检查对应的 ToolResult 是否存在且不是错误
		if i >= len(result.ToolResults) {
			continue
		}
		if strings.HasPrefix(result.ToolResults[i].Content, "错误:") {
			continue
		}
		// 从 Arguments 中提取 path
		p := extractPathArg(tc)
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// extractPathArg 从 ToolCall.Arguments 中安全提取 "path" 字段。
// 如果字段不存在或不是字符串，返回空串。
func extractPathArg(tc llm.ToolCall) string {
	v, ok := tc.Arguments["path"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// detectSubtaskPublish 检测本轮 ToolCalls 中是否存在 publish_subtask 调用。
func detectSubtaskPublish(result ExecuteResult) bool {
	for _, tc := range result.ToolCalls {
		if tc.Name == "publish_subtask" {
			return true
		}
	}
	return false
}

// detectHalfway 检测当前循环是否已过半。
func detectHalfway(loopIndex, maxLoops int) bool {
	return loopIndex > maxLoops/2
}

// buildFileWriteMsg 构造文件写入进度通知消息。
// To="*" 广播给所有兄弟 Agent，Type/Priority/ChainDepth 固定。
// Content 包含 agentID、filepath.Base(path)、轮次信息。
// 多个文件时仅取第一个文件名展示。
func buildFileWriteMsg(agentID string, files []string, loopIndex, maxLoops int) mailbox.Message {
	baseName := ""
	if len(files) > 0 {
		baseName = filepath.Base(files[0])
	}
	return mailbox.Message{
		From:       agentID,
		To:         "*",
		Type:       mailbox.MsgTypeInfo,
		Priority:   mailbox.PriorityLow,
		ChainDepth: 0,
		Summary:    fmt.Sprintf("[进度] %s 写入了文件", agentID),
		Content:    fmt.Sprintf("代理 %s 在任务执行中写入了文件: %s（轮次 %d/%d）", agentID, baseName, loopIndex+1, maxLoops),
		SentAt:     time.Now(),
	}
}

// buildSubtaskMsg 构造子任务发布进度通知消息。
// To="scheduler" 点对点通知 Scheduler Agent。
func buildSubtaskMsg(agentID string, loopIndex, maxLoops int) mailbox.Message {
	return mailbox.Message{
		From:       agentID,
		To:         "scheduler",
		Type:       mailbox.MsgTypeInfo,
		Priority:   mailbox.PriorityLow,
		ChainDepth: 0,
		Summary:    fmt.Sprintf("[进度] %s 发布了子任务", agentID),
		Content:    fmt.Sprintf("代理 %s 在任务执行中发布了子任务（轮次 %d/%d）", agentID, loopIndex+1, maxLoops),
		SentAt:     time.Now(),
	}
}

// buildHalfwayMsg 构造任务过半进度通知消息。
// To="*" 广播给所有兄弟 Agent。
func buildHalfwayMsg(agentID string, loopIndex, maxLoops int) mailbox.Message {
	return mailbox.Message{
		From:       agentID,
		To:         "*",
		Type:       mailbox.MsgTypeInfo,
		Priority:   mailbox.PriorityLow,
		ChainDepth: 0,
		Summary:    fmt.Sprintf("[进度] %s 任务过半", agentID),
		Content:    fmt.Sprintf("代理 %s 任务执行已过半（轮次 %d/%d）", agentID, loopIndex+1, maxLoops),
		SentAt:     time.Now(),
	}
}

// progressNotify 检测本轮触发条件并发送进度通知。
// 所有错误静默降级（log.Printf），不影响 ReactLoop。
// defer/recover 保护，panic 不会逃逸。
func (a *Agent) progressNotify(ctx context.Context, taskID string, loopIndex int, result ExecuteResult, flags *progressFlags) {
	if a.MailRegistry == nil || !a.ProgressNotifyEnabled {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent %s] progressNotify panic 被恢复: %v", a.ID, r)
		}
	}()

	// 1. 文件写入检测
	if !flags.notifiedFileWrite {
		if files := detectFileWrite(result); len(files) > 0 {
			msg := buildFileWriteMsg(a.ID, files, loopIndex, a.MaxLoops)
			if err := a.MailRegistry.Send(msg); err != nil {
				log.Printf("[agent %s] 进度通知(file_write)发送失败: %v", a.ID, err)
			}
			flags.notifiedFileWrite = true
			trace.Emit(trace.Event{
				Kind:       trace.KindProgressNotify,
				TaskID:     taskID,
				AgentID:    a.ID,
				Loop:       loopIndex,
				NotifyType: "file_write",
			})
		}
	}

	// 2. 子任务发布检测
	if !flags.notifiedSubtask {
		if detectSubtaskPublish(result) {
			msg := buildSubtaskMsg(a.ID, loopIndex, a.MaxLoops)
			if err := a.MailRegistry.Send(msg); err != nil {
				log.Printf("[agent %s] 进度通知(subtask)发送失败: %v", a.ID, err)
			}
			flags.notifiedSubtask = true
			trace.Emit(trace.Event{
				Kind:       trace.KindProgressNotify,
				TaskID:     taskID,
				AgentID:    a.ID,
				Loop:       loopIndex,
				NotifyType: "subtask",
			})
		}
	}

	// 3. 任务过半检测
	if !flags.notifiedHalfway {
		if detectHalfway(loopIndex, a.MaxLoops) {
			msg := buildHalfwayMsg(a.ID, loopIndex, a.MaxLoops)
			if err := a.MailRegistry.Send(msg); err != nil {
				log.Printf("[agent %s] 进度通知(halfway)发送失败: %v", a.ID, err)
			}
			flags.notifiedHalfway = true
			trace.Emit(trace.Event{
				Kind:       trace.KindProgressNotify,
				TaskID:     taskID,
				AgentID:    a.ID,
				Loop:       loopIndex,
				NotifyType: "halfway",
			})
		}
	}
}
