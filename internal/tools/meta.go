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

// MetaGroup 注册任务发布与代理间通信工具。
//
// 字段说明：
//   - Store：任务存储；nil 时不注册 publish_task
//   - Holder：当前任务持有器；nil = Scheduler 语义（无深度限制）；非 nil = Worker 语义
//   - MaxDepth：仅 Holder != nil 时生效；publish_task 创建的子任务深度超过该值时拒绝
//   - MBRegistry：邮箱注册表；nil 时不注册 send_message
//   - AgentID：当前代理 ID（send_message 的发件人）
type MetaGroup struct {
	Store      store.TaskStore
	Holder     TaskHolder
	MaxDepth   int
	MBRegistry *mailbox.Registry
	AgentID    string
}

// Register 把 publish_task / send_message 注册到 r。
// 各自的依赖缺失时自动跳过对应工具。
func (g MetaGroup) Register(r *agent.ToolRegistry) {
	if g.Store != nil {
		r.Register(
			"publish_task",
			"发布一个新任务到任务队列，由调度器或其他代理认领执行",
			schema.Object().
				String("description", "任务的详细描述", true).
				String("event_type", "任务类型，留空表示由 Worker 认领；填 \"explore\" 表示交给 Explorer 调查", false).
				Enum("priority", "任务优先级，默认 normal", []string{"low", "normal", "high"}, false).
				String("dependencies", "逗号分隔的依赖任务 ID 列表，留空表示无依赖", false).
				String("expected_artifacts", "逗号分隔的预期产出文件路径列表（相对项目根的相对路径）。任务结束时系统会校验这些文件是否真的写入；缺失则任务失败重试。强烈建议为'报告/总结/文档'类任务填写此字段以防止 report-only 失败", false).
				Build(),
			g.publishTask,
		)
	}
	if g.MBRegistry != nil {
		r.Register(
			"send_message",
			"向指定代理发送结构化消息（点对点或广播）",
			schema.Object().
				String("to", "收件人代理 ID（如 \"worker-1\"、\"scheduler\"），或 \"*\" 表示广播", true).
				String("content", "消息正文（详细内容）", true).
				String("summary", "一句话摘要，帮助收信方快速判断消息重点（建议始终填写）", false).
				Enum("msg_type", "消息类型：info=通知, question=提问/质疑（期望回复）, reply=回复先前消息, steer=纠偏指令。默认 info",
					[]string{"info", "question", "reply", "steer"}, false).
				Enum("priority", "优先级：low/normal/high，默认 normal",
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

	return fmt.Sprintf("已创建任务: id=%s, depth=%d, description=%s", task.ID, childDepth, desc), nil
}

// sendMessage 是 worker.MakeSendMessageTool 的内联端口，避免循环依赖。
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

	msg := mailbox.Message{
		From:     g.AgentID,
		To:       to,
		Content:  content,
		Summary:  summary,
		Type:     msgType,
		Priority: priority,
		SentAt:   time.Now(),
	}
	if err := g.MBRegistry.Send(msg); err != nil {
		return "", err
	}
	if to == "*" {
		return "消息已广播给所有代理", nil
	}
	return fmt.Sprintf("消息已发送给 %s (type=%s, priority=%s)", to, msgType, priority), nil
}
