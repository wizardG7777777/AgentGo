package scheduler

import (
	"context"
	"errors"

	"agentgo/internal/agent"
	"agentgo/internal/modes"
)

// soloPublishTaskBlockedMsg 是 topo=solo 时 publish_task 被拦截返回给 LLM 的错误消息。
// 措辞要求：明确说明禁止原因，并给出可直接执行的替代路径，让 LLM 能立即调整策略。
const soloPublishTaskBlockedMsg = "solo 编排模式禁止派发子任务（publish_task 已被拦截）：当前你是唯一执行者，" +
	"请直接使用 read_file / write_file / edit_file / run_shell / web_search / web_fetch 等工具完成任务，" +
	"再用自然语言汇报或 report_done 收尾"

// wrapPublishTaskForSolo 生成 publish_task 执行函数的包装器：
// topo=solo 时直接拒绝（返回 soloPublishTaskBlockedMsg），否则透传原始 handler。
// modeStore 为 nil 时等价 team（永不拦截）——nil 只出现在单测直构场景。
//
// 该包装只在 scheduler.New 装配点作用于 scheduler agent 自己的 ToolRegistry；
// runner 的 ToolRegistry 不经此路径，因此 runner 的 publish_task 不受拦截，
// send_message 等其它 MetaGroup 工具同样不受影响。
func wrapPublishTaskForSolo(modeStore *modes.Store) func(agent.ToolFunc) agent.ToolFunc {
	return func(inner agent.ToolFunc) agent.ToolFunc {
		return func(ctx context.Context, args map[string]any) (string, error) {
			if modeStore != nil && modeStore.GetTopo() == modes.TopoSolo {
				return "", errors.New(soloPublishTaskBlockedMsg)
			}
			return inner(ctx, args)
		}
	}
}
