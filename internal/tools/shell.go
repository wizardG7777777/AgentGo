package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/effect"
	"agentgo/internal/interaction"
	"agentgo/internal/modes"
	"agentgo/internal/shell"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// shellOutputLimit 限制 run_shell 单次输出的最大字符数，超过则保留尾部。
const shellOutputLimit = 10000

// defaultShellTimeoutSec 当未显式配置 TimeoutSec 时的默认超时（秒）。
const defaultShellTimeoutSec = 30

// ShellGroup 注册 run_shell 工具，包含黑名单与 Interaction 灰名单授权链路。
//
// 必填字段：
//   - Workdir：动态工作目录提供者
//   - TimeoutSec：单次命令的超时上限（秒），<=0 时回退为 30
//   - Interactions：结构化人机交互服务；灰名单命令在 nil 时 fail-closed
//   - AgentID：用于 Interaction 请求的来源标识
//
// 可选字段：
//   - Filter：命令过滤器，nil 时使用 shell.NewCommandFilter(DefaultBlacklist, DefaultGreylist)
//   - ExtraGreylist：追加的命令灰名单模式，在 Filter（或默认名单）的灰名单之后
//     参与匹配；命中后与内置灰名单同一通道处理（创建 shell_command 授权
//     Interaction，服务不可用时 fail-closed）。派生过滤器与 Filter 共享运行时
//     白名单，allow_session 语义不变。验收角色由 dependency_map 注入
//     shell.AcceptanceHardeningGreylist；普通角色留空，行为与之前完全一致
//   - SessionID：返回当前 Session ID，nil 时请求不绑定 Session
//   - Modes：两轴模式 store，exec 轴驱动 strict 全量审批 / yolo 灰名单自动放行；
//     nil 等价 normal
//   - InteractionWaitHook：交互等待钩子，进入/退出"等待用户回复"时各回调一次
//     （true/false），供调用方接线 agent 状态机（waiting_interaction）；nil 为 no-op
//   - ActiveViewer：按任务写时复制隔离的活动视图提供者（runner 装配的
//     workspace.Swapper）；非 nil 且 ActiveView() 非 nil 时，LLM 未显式传
//     working_dir 的默认执行目录切到 workspace 根（尽力隔离：命令写主根绝对
//     路径不可完全阻止，属设计上有意接受的 shell 残余风险，见 workspace/types.go）
type ShellGroup struct {
	Workdir             WorkdirProvider
	TimeoutSec          int
	Interactions        *interaction.Service
	SessionID           func() string
	AgentID             string
	Filter              *shell.CommandFilter // optional
	ExtraGreylist       []string             // optional
	Modes               *modes.Store         // optional
	InteractionWaitHook func(waiting bool)   // optional
	ActiveViewer        ActiveViewer         // optional
	// EffectJournal 是 V6 §4 H2b 副作用账本（internal/effect）；
	// nil 时 run_shell 不记账（行为与引入账本前完全一致）。
	EffectJournal *effect.Journal // optional
}

// Register 实现 ToolGroup 接口。
func (g ShellGroup) Register(r *agent.ToolRegistry) {
	timeoutSec := g.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultShellTimeoutSec
	}

	workdir := g.Workdir

	// defaultWorkDir 是 LLM 未显式传 working_dir 时的默认执行目录：
	// 隔离任务（ActiveViewer 有活动视图）切到 workspace 根——写倾向命令的
	// 相对路径落点随之进 workspace；显式传 working_dir 时维持现状按主根
	// 校验（shell 残余风险，有意接受）。无视图时回退主根（Workdir.Get），
	// 与旧行为完全一致。
	defaultWorkDir := func() string {
		if g.ActiveViewer != nil {
			if view := g.ActiveViewer.ActiveView(); view != nil {
				return view.Root()
			}
		}
		if workdir != nil {
			return workdir.Get()
		}
		return ""
	}

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

		// 确定工作目录：args 优先，其次默认目录（隔离时 workspace 根，否则主根）。
		workingDir, _ := args["working_dir"].(string)
		if workingDir == "" {
			workingDir = defaultWorkDir()
		}

		timeout := time.Duration(effectiveTimeoutSec) * time.Second
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		shellBin, shellArgs := shellCommand(command)
		cmd := newShellExecCommand(execCtx, shellBin, shellArgs...)
		if workingDir != "" {
			cmd.Dir = workingDir
		}

		// H2b Effect Journal：命令执行前先落账（prepared）。Target 只载命令
		// digest（脱敏：完整命令不进账本），Policy=manual_only——命令副作用
		// 不可盲目重放，恢复裁决不自动执行任何动作。
		effID := effectPrepare(g.EffectJournal, ctx, g.AgentID,
			effect.KindShell, "cmd:"+digest12([]byte(command)),
			digest12([]byte(command+"\n"+workingDir)), effect.PolicyManualOnly)

		start := time.Now()
		output, err := cmd.CombinedOutput()
		durationMS := time.Since(start).Milliseconds()
		outStr := truncateKeepTail(string(output), shellOutputLimit)

		// 每次真实执行（成功/非零退出/超时/启动失败）都恰好 emit 一条
		// shell_executed 事件（D4：该 Kind 此前有 schema/CLI 渲染/Reactor
		// 白名单但零发射点）。黑名单拦截或用户未授权的命令到不了这里，不产生事件。
		// V6 §7.4：Command 过默认脱敏（截 200 字符；AGENTGO_TRACE_FULL_ARGS=1
		// 旁路）——record-artifact 只消费 Outcome，不受影响。
		execEv := trace.Event{
			Kind:    trace.KindShellExecuted,
			TaskID:  agent.TaskIDFromContext(ctx),
			AgentID: g.AgentID,
			Tool:    "run_shell",
			ShellExec: &trace.ShellExec{
				Command:    trace.RedactShellCommand(command),
				DurationMS: durationMS,
			},
		}

		exitCode := 0
		if err != nil {
			if execCtx.Err() == context.DeadlineExceeded {
				execEv.ShellExec.Outcome = "timeout"
				execEv.Error = fmt.Sprintf("命令执行超时（%d 秒）", effectiveTimeoutSec)
				trace.Emit(execEv)
				// 超时时进程已被杀，但已执行部分产生的副作用不可知 → unknown。
				effectMarkUnknown(g.EffectJournal, effID,
					fmt.Sprintf("命令超时（%d 秒），已执行部分的副作用不可知", effectiveTimeoutSec))
				return "", fmt.Errorf("命令执行超时（%d 秒）: %s", effectiveTimeoutSec, command)
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				execEv.ShellExec.Outcome = "failure"
				execEv.Error = err.Error()
				trace.Emit(execEv)
				// 启动失败（进程未运行）——结果已知：未产生进程副作用。
				effectSettle(g.EffectJournal, effID, "启动失败，未执行: "+err.Error())
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

		effectSettle(g.EffectJournal, effID, fmt.Sprintf("exit_code=%d outcome=%s duration_ms=%d out_bytes=%d out_sha256=%s",
			exitCode, execEv.ShellExec.Outcome, durationMS, len(output), digest12(output)))

		return fmt.Sprintf("exit_code: %d\nstdout+stderr:\n%s", exitCode, outStr), nil
	}

	// 构造过滤器：未提供时使用默认黑/灰名单。
	filter := g.Filter
	if filter == nil {
		filter = shell.NewCommandFilter(shell.DefaultBlacklist, shell.DefaultGreylist)
	}
	// 验收加固：追加灰名单只在派生过滤器上生效，原 Filter（可能是全局
	// 共享实例）的灰名单判定不变；运行时白名单与原 Filter 共享，
	// allow_session 语义不变。
	if len(g.ExtraGreylist) > 0 {
		filter = filter.DeriveWithExtraGreylist(g.ExtraGreylist)
	}

	authorizedFn := shell.WrapShellTool(rawFn, filter, g.Interactions, g.SessionID,
		g.AgentID, g.InteractionWaitHook, g.Modes)
	// Interaction 必须绑定实际执行目录，而不是只看到用户是否显式传参。
	// 在进入拦截器前复制参数并补齐默认目录（隔离时 workspace 根，否则
	// Workdir fallback），避免修改 LLM 调用方持有的 map。
	wrappedFn := func(ctx context.Context, args map[string]any) (string, error) {
		workingDir, _ := args["working_dir"].(string)
		if workingDir != "" {
			return authorizedFn(ctx, args)
		}
		dir := defaultWorkDir()
		if dir == "" {
			return authorizedFn(ctx, args)
		}
		resolvedArgs := make(map[string]any, len(args)+1)
		for key, value := range args {
			resolvedArgs[key] = value
		}
		resolvedArgs["working_dir"] = dir
		return authorizedFn(ctx, resolvedArgs)
	}

	params := schema.Object().
		String("command", "要执行的 shell 命令", true).
		String("working_dir", "执行命令的工作目录，留空时使用代理当前工作目录", false).
		Int("timeout_sec", "本次执行的超时秒数，留空时使用配置默认值", false).
		Build()

	r.Register("run_shell", "在指定目录下执行 shell 命令，返回 stdout、stderr 和 exit code"+shellDialectNote(), params, wrappedFn)
}

// shellCommand 根据当前操作系统返回合适的 shell 执行器和参数。
// Windows: powershell -NoProfile -NonInteractive -Command（现代 Windows 均自带；
// -NoProfile 跳过用户 profile 保证行为可预测，-NonInteractive 防止命令意外阻塞等输入）。
// Unix: sh -c。
func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	return "sh", []string{"-c", command}
}

// shellDialectNote 返回面向 LLM 的当前 shell 环境说明，拼入 run_shell 工具描述。
// 2026-07-21 验收马拉松事故的根因之一就是 LLM 不知道命令由谁解释，按 Unix
// 先验写 test -s / mkdir -p 等命令在 Windows 上失败或产生副作用（字面 -p 目录）。
func shellDialectNote() string {
	if runtime.GOOS == "windows" {
		return "\n\n当前环境：Windows，命令由 PowerShell（powershell -NoProfile -Command）解释。" +
			"\n- 使用 PowerShell 语法；ls/cat/cp/mv/rm/echo/pwd 等常见 Unix 别名可用，但不要假设 bash/sed/awk/grep 存在" +
			"\n- 常用对照：test -s <f> → Test-Path <f>；ls -la → Get-ChildItem；cat <f> → Get-Content <f>；" +
			"mkdir -p <d> → New-Item -ItemType Directory -Force <d>；grep <pat> <f> → Select-String <pat> <f>" +
			"\n- 系统会硬拒绝写文件的重定向（>、>>、Out-File、tee 等）：这类命令不会执行，直接报错；写文件一律使用 write_file / edit_file 工具（PowerShell 5.1 重定向还会产生 UTF-16 编码文件）"
	}
	return "\n\n当前环境：" + runtime.GOOS + "，命令由 POSIX sh（sh -c）解释。" +
		"\n- 使用 POSIX sh 语法，不要假设 bash 专有特性（[[ ]]、数组等）可用" +
		"\n- 系统会硬拒绝写文件的重定向（>、>> 等）：这类命令不会执行，直接报错；写文件一律使用 write_file / edit_file 工具"
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
