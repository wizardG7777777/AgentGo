package agent

import (
	"agentgo/internal/model"
	"sort"
	"strings"
	"time"
)

type attemptContextStats struct {
	historyProjectionCount int
	l3Truncated            bool
}

func newAttemptContextStats(_ time.Time) *attemptContextStats { return &attemptContextStats{} }
func renderTaskContextBlock(task *model.Task) string {
	var sb strings.Builder
	if task.GraphID != "" {
		// 图任务追加 graph_id / node_id / activation_id（V6 Graph 路由语境）。
		sb.WriteString("<task-context source=\"control-plane\">\n")
		sb.WriteString("task_id: " + task.ID + "\n")
		sb.WriteString("graph_id: " + task.GraphID + "\n")
		sb.WriteString("node_id: " + task.NodeID + "\n")
		sb.WriteString("activation_id: " + task.ActivationID + "\n")
		sb.WriteString("</task-context>\n")
		return sb.String()
	}
	return "<task-context source=\"control-plane\">\ntask_id: " + task.ID + "\n</task-context>\n"
}

// renderDepResultsCanonical 渲染依赖结果的规范形式（按 depID 排序）。
// buildMessages 直接 range map（迭代序随机），Manifest digest 不能与消息字节
// 序绑定，故对排序后的规范形式计算——同一份 depResults 的 digest 跨轮稳定。
func renderDepResultsCanonical(depResults map[string]string) string {
	keys := make([]string, 0, len(depResults))
	for depID := range depResults {
		keys = append(keys, depID)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, depID := range keys {
		sb.WriteString("[" + depID + "] " + depResults[depID] + "\n")
	}
	return sb.String()
}
