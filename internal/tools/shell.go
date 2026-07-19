package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/shell"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// shellOutputLimit 限制 run_shell 单次输出的最大字符数，超过则保留尾部。
const shellOutputLimit = 10000

// defaultShellTimeoutSec 当未显式配置 TimeoutSec 时的默认超时（秒）。
const defaultShellTimeoutSec = 30

// ShellGroup 注册 run_shell 工具，包含黑/灰名单审批拦截链路。
//
// 必填字段：
//   - Workdir：动态工作目录提供者
//   - TimeoutSec：单次命令的超时上限（秒），<=0 时回退为 30
//   - ApprovalCh：发往 CLI 的审批请求通道（灰名单命令通过它请求人工审批）
//   - AgentID：用于审批请求的来源标识
//
// 可选字段：
//   - Filter：命令过滤器，nil 时使用 shell.NewCommandFilter(DefaultBlacklist, DefaultGreylist)
//   - ApprovalWaitHook：审批等待钩子，进入/退出"等待用户回复"阻塞时各回调一次
//     （true/false），供调用方接线 agent 状态机（waiting_approval）；nil 为 no-op
type ShellGroup struct {
	Workdir          WorkdirProvider
	TimeoutSec       int
	ApprovalCh       chan<- shell.ApprovalRequest
	AgentID          string
	Filter           *shell.CommandFilter // optional
	ApprovalWaitHook func(waiting bool)   // optional
}

// Register 实现 ToolGroup 接口。
func (g ShellGroup) Register(r *agent.ToolRegistry) {
	timeoutSec := g.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultShellTimeoutSec
	}

	workdir := g.Workdir

	rawFn := func(ctx context.Context, args map[string]any) (string, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return "", fmt.Errorf("缺少 command 参数")
		}

		// 确定有效超时：args 优先，其次 Group 配置。
		effectiveTimeoutSec := timeoutSec
		if v, ok := args["timeout_sec"].(float64); ok && v > 0 {
			effectiveTimeoutSec = int(v)
		} else if v, ok := args["timeout_sec"].(int); ok && v > 0 {
			effectiveTimeoutSec = v
		}

		// 确定工作目录：args 优先，其次 Workdir.Get()。
		workingDir, _ := args["working_dir"].(string)
		if workingDir == "" && workdir != nil {
			workingDir = workdir.Get()
		}

		timeout := time.Duration(effectiveTimeoutSec) * time.Second
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		shellBin, shellArgs := shellCommand(command)
		cmd := newShellExecCommand(execCtx, shellBin, shellArgs...)
		if workingDir != "" {
			cmd.Dir = workingDir
		}

		start := time.Now()
		output, err := cmd.CombinedOutput()
		durationMS := time.Since(start).Milliseconds()
		outStr := truncateKeepTail(string(output), shellOutputLimit)

		// 每次真实执行（成功/非零退出/超时/启动失败）都恰好 emit 一条
		// shell_executed 事件（D4：该 Kind 此前有 schema/CLI 渲染/Reactor
		// 白名单但零发射点）。黑名单拦截/审批拒绝的命令到不了这里，不产生事件。
		execEv := trace.Event{
			Kind:    trace.KindShellExecuted,
			TaskID:  agent.TaskIDFromContext(ctx),
			AgentID: g.AgentID,
			Tool:    "run_shell",
			ShellExec: &trace.ShellExec{
				Command:    command,
				DurationMS: durationMS,
			},
		}

		exitCode := 0
		if err != nil {
			if execCtx.Err() == context.DeadlineExceeded {
				execEv.ShellExec.Outcome = "timeout"
				execEv.Error = fmt.Sprintf("命令执行超时（%d 秒）", effectiveTimeoutSec)
				trace.Emit(execEv)
				return "", fmt.Errorf("命令执行超时（%d 秒）: %s", effectiveTimeoutSec, command)
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				execEv.ShellExec.Outcome = "failure"
				execEv.Error = err.Error()
				trace.Emit(execEv)
				return "", fmt.Errorf("启动命令失败: %w", err)
			}
		}

		execEv.ShellExec.ExitCode = exitCode
		if exitCode == 0 {
			execEv.ShellExec.Outcome = "success"
		} else {
			execEv.ShellExec.Outcome = "failure"
		}
		trace.Emit(execEv)

		return fmt.Sprintf("exit_code: %d\nstdout+stderr:\n%s", exitCode, outStr), nil
	}

	// 构造过滤器：未提供时使用默认黑/灰名单。
	filter := g.Filter
	if filter == nil {
		filter = shell.NewCommandFilter(shell.DefaultBlacklist, shell.DefaultGreylist)
	}

	wrappedFn := shell.WrapShellTool(rawFn, filter, g.ApprovalCh, g.AgentID, g.ApprovalWaitHook)

	params := schema.Object().
		String("command", "要执行的 shell 命令", true).
		String("working_dir", "执行命令的工作目录，留空时使用代理当前工作目录", false).
		Int("timeout_sec", "本次执行的超时秒数，留空时使用配置默认值", false).
		Build()

	r.Register("run_shell", "在指定目录下执行 shell 命令，返回 stdout、stderr 和 exit code", params, wrappedFn)
}

// shellCommand 根据当前操作系统返回合适的 shell 执行器和参数。
// Windows: cmd /C；Unix: sh -c。
func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

// truncateKeepTail 截断字符串，保留尾部 limit 个字符。
// 当 len(output) <= limit 时原样返回；否则保留最后 limit 个字符并在前面添加截断提示。
func truncateKeepTail(output string, limit int) string {
	if len(output) <= limit {
		return output
	}
	truncated := len(output) - limit
	return fmt.Sprintf("[截断提示] 原始输出共 %d 字符，已截断前 %d 字符，仅保留最后 %d 字符\n%s",
		len(output), truncated, limit, output[truncated:])
}
