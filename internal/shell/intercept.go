package shell

import (
	"context"
	"fmt"
	"log"
	"regexp"

	"agentgo/internal/agent"
)

// ApprovalRequest 灰名单命令的审批请求，由 Worker 发送到 CLI。
type ApprovalRequest struct {
	AgentID string             // 申请执行的代理 ID
	Command string             // 待执行的命令
	ReplyCh chan ApprovalReply // 无缓冲，Worker 阻塞等待用户回复
}

// ApprovalReply 用户对审批请求的回复。
type ApprovalReply struct {
	Approved bool   // true=放行执行, false=拒绝
	Message  string // 非空时为用户自由文本指导（此时 Approved 为 false）
}

// MVP 阶段硬编码的默认黑名单（正则模式，匹配即拒绝）。
var DefaultBlacklist = []string{
	`rm\s+-rf\s+/`,     // rm -rf /
	`mkfs\.`,           // 格式化磁盘
	`dd\s+if=`,         // 低级磁盘写入
	`:\(\)\{.*\|.*&\}`, // fork bomb
	`shutdown`,         // 关机
	`reboot`,           // 重启
	`init\s+0`,         // 关机
}

// MVP 阶段硬编码的默认灰名单（正则模式，匹配时需用户审批）。
var DefaultGreylist = []string{
	`git\s+push`,           // 推送到远程
	`git\s+reset\s+--hard`, // 硬重置
	`git\s+checkout\s+\.`,  // 丢弃所有修改
	`chmod`,                // 修改权限
	`chown`,                // 修改所有者
	`curl.*\|\s*sh`,        // 管道执行远程脚本
	`wget.*\|\s*sh`,        // 管道执行远程脚本
	`pip\s+install`,        // 安装 Python 包
	`npm\s+install\s+-g`,   // 全局安装 npm 包
	`apt\s+install`,        // 安装系统包
	`yum\s+install`,        // 安装系统包
}

// CommandFilter 命令拦截器，通过正则模式匹配危险命令。
type CommandFilter struct {
	blackPatterns []*regexp.Regexp
	greyPatterns  []*regexp.Regexp
	blackRaw      []string // 原始模式字符串，用于错误消息
	greyRaw       []string
}

// NewCommandFilter 创建命令拦截器。编译失败的正则模式会被跳过并记录警告。
func NewCommandFilter(blacklist, greylist []string) *CommandFilter {
	f := &CommandFilter{}
	for _, pattern := range blacklist {
		re, err := regexp.Compile(pattern)
		if err != nil {
			log.Printf("[shell-filter] 黑名单正则编译失败，已跳过: %s (%v)", pattern, err)
			continue
		}
		f.blackPatterns = append(f.blackPatterns, re)
		f.blackRaw = append(f.blackRaw, pattern)
	}
	for _, pattern := range greylist {
		re, err := regexp.Compile(pattern)
		if err != nil {
			log.Printf("[shell-filter] 灰名单正则编译失败，已跳过: %s (%v)", pattern, err)
			continue
		}
		f.greyPatterns = append(f.greyPatterns, re)
		f.greyRaw = append(f.greyRaw, pattern)
	}
	return f
}

// Check 检查命令是否命中黑名单或灰名单。
// 返回 action ("allow"/"block"/"approve") 和匹配的原始模式（block/approve 时非空）。
// 黑名单优先于灰名单。
func (f *CommandFilter) Check(command string) (action string, pattern string) {
	for i, re := range f.blackPatterns {
		if re.MatchString(command) {
			return "block", f.blackRaw[i]
		}
	}
	for i, re := range f.greyPatterns {
		if re.MatchString(command) {
			return "approve", f.greyRaw[i]
		}
	}
	return "allow", ""
}

// WrapShellTool 包装原始 run_shell 工具函数，加入黑名单/灰名单拦截层。
// approvalCh 用于向 CLI 发送审批请求；agentID 标识申请执行的代理。
func WrapShellTool(inner agent.ToolFunc, filter *CommandFilter,
	approvalCh chan<- ApprovalRequest, agentID string) agent.ToolFunc {

	return func(ctx context.Context, args map[string]any) (string, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return inner(ctx, args) // 空命令交给原始工具处理（它会报错）
		}

		action, pattern := filter.Check(command)

		switch action {
		case "block":
			log.Printf("[shell-filter] 黑名单拦截: agent=%s, command=%q, pattern=%s", agentID, command, pattern)
			return "", fmt.Errorf(
				"⚠ 命令被拒绝（黑名单）：该命令匹配危险模式 [%s]，不允许执行。请使用更安全的替代方案。", pattern)

		case "approve":
			log.Printf("[shell-filter] 灰名单审批: agent=%s, command=%q, pattern=%s", agentID, command, pattern)
			replyCh := make(chan ApprovalReply)

			// 向 CLI 发送审批请求
			select {
			case approvalCh <- ApprovalRequest{AgentID: agentID, Command: command, ReplyCh: replyCh}:
			case <-ctx.Done():
				return "", fmt.Errorf("命令审批被取消")
			}

			// 阻塞等待用户回复
			select {
			case reply := <-replyCh:
				if reply.Message != "" {
					// 用户输入了自由文本指导 → 返回给 Agent，不执行命令
					return "", fmt.Errorf("用户指导: %s", reply.Message)
				}
				if !reply.Approved {
					return "", fmt.Errorf("⚠ 命令被用户拒绝。请调整方案后重试。")
				}
				// 放行 → fall through 到执行
			case <-ctx.Done():
				return "", fmt.Errorf("命令审批超时")
			}
		}

		// action == "allow" 或灰名单已放行 → 执行原始工具
		return inner(ctx, args)
	}
}
