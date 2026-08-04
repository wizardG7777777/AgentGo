package shell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/modes"
)

const (
	// PurposeShellCommand 标识一条 Shell 灰名单授权请求。
	PurposeShellCommand interaction.Purpose = "shell_command"
	// ResolutionHandlerShellCommand 是 Bootstrap 回答路由使用的稳定 handler。
	ResolutionHandlerShellCommand = "shell_command"

	ActionAllowOnce    = "allow_once"
	ActionDeny         = "deny"
	ActionGuidance     = "guidance"
	ActionAllowSession = "allow_session"

	MetadataCommand    = "command"
	MetadataPattern    = "pattern"
	MetadataWorkingDir = "working_dir"
	MetadataDigest     = "digest"
)

// 默认黑名单（正则模式，匹配即拒绝）。同时覆盖 Unix 与 Windows/PowerShell
// 危险命令形态——run_shell 在 Windows 由 PowerShell 解释、POSIX 由 sh 解释，
// 两套方言的不可逆操作都必须硬拒（2026-07-21 跨平台排查 H2）。
var DefaultBlacklist = []string{
	`rm\s+-rf\s+/`,     // rm -rf /
	`mkfs\.`,           // 格式化磁盘
	`dd\s+if=`,         // 低级磁盘写入
	`:\(\)\{.*\|.*&\}`, // fork bomb
	`shutdown`,         // 关机
	`reboot`,           // 重启
	`init\s+0`,         // 关机
	// ---- Windows / PowerShell 形态（cmdlet 大小写不敏感，统一加 (?i)）----
	`(?i)\bformat\s+[a-z]:`,           // format C: 格式化磁盘
	`(?i)\bdiskpart\b`,                // 磁盘分区操作
	`(?i)\bbcdedit\b`,                 // 启动配置修改
	`(?i)-enc(odedcommand)?(\s|$)`,    // PowerShell base64 编码载荷（-enc 为其合法前缀缩写）
	`(?i)\biex\b|Invoke-Expression`,   // 动态执行任意字符串
	`(?i)\breg\s+delete\b`,            // 注册表删除
	`(?i)\b(rd|rmdir)\b[^|&]*/s\b`,    // cmd 递归删除目录
	`(?i)\bdel\b[^|&]*/s\b`,           // cmd 递归删除文件
	`(?i)Remove-Item\b[^|&]*-Recurse`, // PowerShell 递归删除
}

// 默认灰名单（正则模式，匹配时需创建用户 Interaction）。
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
	// ---- Windows / PowerShell 形态 ----
	`(?i)\bStop-Process\b`,                  // 终止进程
	`(?i)\bSet-ExecutionPolicy\b`,           // 修改脚本执行策略
	`(?i)\bicacls\b|\btakeown\b`,            // 修改 ACL / 所有权
	`(?i)\brm\s+-[a-z]*(r[a-z]*f|f[a-z]*r)`, // rm -rf 递归强制删除（PowerShell rm 别名同形）
}

// CommandFilter 命令拦截器，通过正则模式匹配危险命令。
//
// 运行时白名单（wl）是"本次运行始终允许"的进程内记忆：用户选择
// allow_session 后，该模式被加入此白名单，本进程后续命中同模式不再创建请求。
// 进程重启失效——这是有意的安全边界，避免风险随时间累积。
// wl 为共享指针：DeriveWithExtraGreylist 派生的过滤器与源过滤器共用同一份
// 白名单状态，保持单过滤器时期的全局 allow_session 语义不变。
type CommandFilter struct {
	blackPatterns []*regexp.Regexp
	greyPatterns  []*regexp.Regexp
	blackRaw      []string // 原始模式字符串，用于错误消息
	greyRaw       []string

	wl *runtimeWhitelistState
}

// runtimeWhitelistState 是运行时白名单的共享状态容器。独立成指针字段，
// 使派生过滤器（DeriveWithExtraGreylist）与源过滤器共享同一份
// allow_session 记忆。
type runtimeWhitelistState struct {
	mu      sync.RWMutex
	entries []runtimeWhitelistEntry
}

type runtimeWhitelistEntry struct {
	raw string
	re  *regexp.Regexp
}

// NewCommandFilter 创建命令拦截器。编译失败的正则模式会被跳过并记录警告。
func NewCommandFilter(blacklist, greylist []string) *CommandFilter {
	f := &CommandFilter{wl: &runtimeWhitelistState{}}
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

// Check 检查命令是否命中重定向写文件硬规则、黑名单或灰名单。
// 返回 action ("allow"/"block"/"ask") 和匹配的原始模式（block/ask 时非空）。
//
// 顺序：重定向写文件 > 黑名单 > 运行时白名单 > 灰名单。
// 重定向写文件与黑名单同为始终硬拒（无法被 "永远允许" 覆盖），命中时
// pattern 带 RedirectWritePatternPrefix 前缀；运行时白名单只短路灰名单匹配。
func (f *CommandFilter) Check(command string) (action string, pattern string) {
	// 重定向写文件硬规则（redirect.go）：与黑名单同通道 block，但优先级
	// 最高——写文件必须走 write_file / edit_file，shell 重定向一律不放行。
	if detail := detectRedirectWrite(command); detail != "" {
		return "block", RedirectWritePatternPrefix + detail
	}
	for i, re := range f.blackPatterns {
		if re.MatchString(command) {
			return "block", f.blackRaw[i]
		}
	}
	f.wl.mu.RLock()
	for _, entry := range f.wl.entries {
		if entry.re.MatchString(command) {
			f.wl.mu.RUnlock()
			return "allow", ""
		}
	}
	f.wl.mu.RUnlock()
	for i, re := range f.greyPatterns {
		if re.MatchString(command) {
			return "ask", f.greyRaw[i]
		}
	}
	return "allow", ""
}

// AddRuntimeWhitelist 把一个正则模式加入运行时白名单。
//
// 重复添加同一模式（按原始字符串比对）会被忽略，不报错。
// 编译失败的模式返回错误，调用方决定是否上抛。
func (f *CommandFilter) AddRuntimeWhitelist(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is empty")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("compile %q: %w", pattern, err)
	}
	f.wl.mu.Lock()
	defer f.wl.mu.Unlock()
	for _, entry := range f.wl.entries {
		if entry.raw == pattern {
			return nil
		}
	}
	f.wl.entries = append(f.wl.entries, runtimeWhitelistEntry{raw: pattern, re: re})
	return nil
}

// RuntimeWhitelist 返回运行时白名单的快照（原始模式字符串），供 TUI / 调试展示。
func (f *CommandFilter) RuntimeWhitelist() []string {
	f.wl.mu.RLock()
	defer f.wl.mu.RUnlock()
	out := make([]string, 0, len(f.wl.entries))
	for _, entry := range f.wl.entries {
		out = append(out, entry.raw)
	}
	return out
}

// IsRuntimeWhitelisted 报告命令是否命中运行时白名单（allow_session 授权的
// 进程内记忆）。strict 执行模式用它区分"用户已显式放行的模式"与"普通命令"：
// 前者直接执行，后者一律转入 ask 路径。
func (f *CommandFilter) IsRuntimeWhitelisted(command string) bool {
	f.wl.mu.RLock()
	defer f.wl.mu.RUnlock()
	for _, entry := range f.wl.entries {
		if entry.re.MatchString(command) {
			return true
		}
	}
	return false
}

// execModeOf 读取 exec 轴当前值；nil store 等价 normal（单测直构场景）。
func execModeOf(modeStore *modes.Store) modes.ExecMode {
	if modeStore == nil {
		return modes.ExecNormal
	}
	return modeStore.GetExec()
}

// WrapShellTool 包装原始 run_shell 工具函数，加入黑名单与 Interaction 灰名单
// 授权层。灰名单命令创建服务器端 shell_command 请求，并等待该请求真正进入
// resolved；仅 allow_once / allow_session 会执行 inner。
//
// exec 轴在过滤器判定之后施加短路（docs/design/interaction.md §5.1）：
//   - yolo：灰名单 ask 自动放行（不创建 Interaction，省一次用户往返），并打
//     一行中文审计日志；黑名单依旧硬拒。
//   - strict：所有未命中黑名单 / 运行时白名单的命令一律转入 ask——包括原本
//     直接放行的普通命令。白名单命中仍放行：那是用户 allow_session 的显式
//     授权，不应被 strict 重复询问（粒度取舍见设计文档）。黑名单依旧硬拒。
//
// interactions 为 nil 或创建/等待失败时一律 fail-closed。sessionID 可为 nil；
// waitHook 在成功创建请求后、开始等待前收到 true，并在所有退出路径收到 false。
// modeStore 为 nil 等价 normal（单测直构场景）。
func WrapShellTool(inner agent.ToolFunc, filter *CommandFilter,
	interactions *interaction.Service, sessionID func() string,
	agentID string, waitHook func(waiting bool), modeStore *modes.Store) agent.ToolFunc {
	if filter == nil {
		filter = NewCommandFilter(DefaultBlacklist, DefaultGreylist)
	}

	return func(ctx context.Context, args map[string]any) (string, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return inner(ctx, args) // 空命令交给原始工具处理（它会报错）
		}
		action, pattern := filter.Check(command)

		// exec 轴短路：黑名单 block 在任何模式下都保持硬拒绝，不在此触碰。
		switch execModeOf(modeStore) {
		case modes.ExecYolo:
			if action == "ask" {
				log.Printf("[yolo] 灰名单命令已自动放行: agent=%s, command=%q, pattern=%s", agentID, command, pattern)
				action = "allow"
			}
		case modes.ExecStrict:
			if action == "allow" && !filter.IsRuntimeWhitelisted(command) {
				action = "ask"
				pattern = "" // 非灰名单命中——strict 全量审批，无可捕获模式
			}
		}

		switch action {
		case "block":
			// 重定向写文件与黑名单同通道硬拒，但拒绝消息单独定制：
			// 明确指引改用 write_file / edit_file 工具。
			if detail, ok := strings.CutPrefix(pattern, RedirectWritePatternPrefix); ok {
				log.Printf("[shell-filter] 重定向写文件拦截: agent=%s, command=%q, redirect=%s", agentID, command, detail)
				return "", fmt.Errorf(
					"⚠ 命令被拒绝（禁止 shell 重定向写文件）：检测到 %s 会把输出写入文件。写文件请改用 write_file / edit_file 工具。", detail)
			}
			log.Printf("[shell-filter] 黑名单拦截: agent=%s, command=%q, pattern=%s", agentID, command, pattern)
			return "", fmt.Errorf(
				"⚠ 命令被拒绝（黑名单）：该命令匹配危险模式 [%s]，不允许执行。请使用更安全的替代方案。", pattern)

		case "ask":
			if pattern != "" {
				log.Printf("[shell-filter] 灰名单 Interaction: agent=%s, command=%q, pattern=%s", agentID, command, pattern)
			} else {
				log.Printf("[shell-filter] strict 全量审批 Interaction: agent=%s, command=%q", agentID, command)
			}
			if interactions == nil {
				return "", fmt.Errorf("⚠ 命令需要人工授权，但 Interaction 服务不可用；已拒绝执行")
			}

			workingDir, _ := args["working_dir"].(string)
			digest := shellCommandDigest(command, pattern, workingDir)
			taskID := agent.TaskIDFromContext(ctx)
			currentSessionID := ""
			if sessionID != nil {
				currentSessionID = sessionID()
			}
			var expiresAt time.Time
			if deadline, ok := ctx.Deadline(); ok {
				expiresAt = deadline
			}
			options := []interaction.Option{
				{ID: ActionAllowOnce, Label: "仅允许本次", ActionRef: ActionAllowOnce},
				{ID: ActionDeny, Label: "拒绝", ActionRef: ActionDeny},
				{ID: ActionGuidance, Label: "提供指导", RequiresText: true, ActionRef: ActionGuidance},
			}
			prompt := fmt.Sprintf("Agent %s 请求执行命令（strict 模式逐条审批）：\n%s", agentID, command)
			if pattern != "" {
				prompt = fmt.Sprintf("Agent %s 请求执行灰名单命令：\n%s", agentID, command)
				// 只有捕获到灰名单模式时才提供 allow_session——strict 全量审批
				// 没有可加入会话白名单的服务端捕获模式。
				options = append(options, interaction.Option{
					ID: ActionAllowSession, Label: "本次运行始终允许该模式", ActionRef: ActionAllowSession,
				})
			}
			request, err := interactions.Create(ctx, interaction.CreateRequest{
				SessionID:     currentSessionID,
				Kind:          interaction.KindAuthorization,
				Purpose:       PurposeShellCommand,
				Prompt:        prompt,
				AllowFreeText: true,
				Options:       options,
				Origin:        interaction.Origin{Component: "shell", AgentID: agentID, TaskID: taskID},
				Subject:       interaction.Subject{Kind: "shell_command", ID: digest, TaskID: taskID, Digest: digest},
				Resolution: interaction.ResolutionSpec{
					Handler: ResolutionHandlerShellCommand, TargetID: digest,
					AgentID: agentID, TaskID: taskID,
				},
				Metadata: map[string]string{
					MetadataCommand: command, MetadataPattern: pattern,
					MetadataWorkingDir: workingDir, MetadataDigest: digest,
				},
				ExpiresAt: expiresAt,
			})
			if err != nil {
				return "", fmt.Errorf("⚠ 无法创建命令授权请求；已拒绝执行: %w", err)
			}

			if waitHook != nil {
				waitHook(true)
			}
			resolved, err := interactions.Await(ctx, request.ID)
			if waitHook != nil {
				waitHook(false)
			}
			if err != nil {
				if ctx.Err() != nil {
					bestEffortInterrupt(interactions, request.ID, "命令任务已取消或系统正在关闭")
					return "", fmt.Errorf("命令授权等待被取消（任务结束、超时或系统关闭）: %w", ctx.Err())
				}
				switch {
				case errors.Is(err, interaction.ErrCancelled):
					return "", fmt.Errorf("⚠ 命令授权已取消: %w", err)
				case errors.Is(err, interaction.ErrExpired):
					return "", fmt.Errorf("⚠ 命令授权已过期: %w", err)
				case errors.Is(err, interaction.ErrInterrupted):
					return "", fmt.Errorf("⚠ 命令授权已中断: %w", err)
				default:
					return "", fmt.Errorf("⚠ 命令授权未完成；已拒绝执行: %w", err)
				}
			}
			if !matchesShellRequest(resolved, command, pattern, workingDir, digest, agentID, taskID) {
				return "", fmt.Errorf("⚠ 命令授权与当前调用不匹配；已拒绝执行")
			}
			selected, ok := resolved.SelectedOption()
			if !ok {
				return "", fmt.Errorf("⚠ 命令授权缺少有效选项；已拒绝执行")
			}
			switch selected.ActionRef {
			case ActionAllowOnce:
				// 仅放行当前精确命令调用。
			case ActionDeny:
				return "", fmt.Errorf("⚠ 命令被用户拒绝。请调整方案后重试。")
			case ActionGuidance:
				if resolved.Response == nil || resolved.Response.Text == "" {
					return "", fmt.Errorf("⚠ 用户指导为空；命令未执行")
				}
				return "", fmt.Errorf("用户指导: %s", resolved.Response.Text)
			case ActionAllowSession:
				// 只能加入创建 Interaction 时由 CommandFilter 捕获的原始模式；
				// 用户回答和 ActionRef 均不能提供替代正则。
				if err := filter.AddRuntimeWhitelist(pattern); err != nil {
					return "", fmt.Errorf("⚠ 无法应用会话白名单；命令未执行: %w", err)
				}
				log.Printf("[shell-filter] 会话白名单已生效: pattern=%q", pattern)
			default:
				return "", fmt.Errorf("⚠ 未知命令授权动作 %q；已拒绝执行", selected.ActionRef)
			}
		}

		// action == "allow" 或灰名单已放行 → 执行原始工具
		return inner(ctx, args)
	}
}

func shellCommandDigest(command, pattern, workingDir string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(command))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(pattern))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(workingDir))
	return hex.EncodeToString(hash.Sum(nil))
}

func matchesShellRequest(request interaction.Request, command, pattern, workingDir, digest, agentID, taskID string) bool {
	return request.State == interaction.StateResolved &&
		request.Purpose == PurposeShellCommand &&
		request.Resolution.Handler == ResolutionHandlerShellCommand &&
		request.Resolution.TargetID == digest &&
		request.Resolution.AgentID == agentID &&
		request.Resolution.TaskID == taskID &&
		request.Origin.AgentID == agentID &&
		request.Origin.TaskID == taskID &&
		request.Subject.ID == digest &&
		request.Subject.TaskID == taskID &&
		request.Subject.Digest == digest &&
		request.Metadata[MetadataCommand] == command &&
		request.Metadata[MetadataPattern] == pattern &&
		request.Metadata[MetadataWorkingDir] == workingDir &&
		request.Metadata[MetadataDigest] == digest
}

// bestEffortInterrupt 在原 ctx 已取消后使用独立短超时上下文收尾，避免留下
// 永久 pending 的幽灵请求。版本冲突时重新读取一次；任何错误都只记录日志，
// 原命令仍保持 fail-closed。
func bestEffortInterrupt(interactions *interaction.Service, id, reason string) {
	if interactions == nil || id == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		request, err := interactions.Get(cleanupCtx, id)
		if err != nil || request.State.IsTerminal() {
			return
		}
		if _, err = interactions.Interrupt(cleanupCtx, id, request.Version, reason); err == nil {
			return
		}
		if !errors.Is(err, interaction.ErrVersionConflict) {
			log.Printf("[shell-filter] 收尾 Interaction %s 失败: %v", id, err)
			return
		}
	}
}
