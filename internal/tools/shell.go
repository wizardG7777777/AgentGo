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
	"agentgo/internal/pathutil"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// defaultShellTimeoutSec 当未显式配置 TimeoutSec 时的默认超时（秒）。
const defaultShellTimeoutSec = 30

func shellPipelineScope(args map[string]any) (bool, error) {
	command, _ := args["command"].(string)
	hasPipeline := shell.HasPipeline(command)
	acceptLastPipelineStatus, _ := args["accept_last_pipeline_exit_code"].(bool)
	if hasPipeline && !acceptLastPipelineStatus {
		return true, fmt.Errorf("reason_code=shell_pipeline_exit_scope_ambiguous：检测到 Shell pipeline，默认退出码只代表最后一个管道段，不能证明整条命令成功；suggested_action=remove_pipeline。仅当末段状态就是明确判定目标时，才可设置 accept_last_pipeline_exit_code=true")
	}
	return hasPipeline, nil
}

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
//     workspace.Swapper）；非 nil 且 ActiveView() 非 nil 时，默认 cwd 与显式
//     working_dir 都限制在 workspace 根内。命令正文写主根绝对路径仍不可完全
//     阻止，属设计上有意接受的宿主 Shell 残余风险（见 workspace/types.go）
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

	// allowedWorkDirRoot 是当前调用唯一允许的 cwd 根：隔离任务严格限定在
	// workspace 视图内，普通任务限定在项目根内。它只约束进程 cwd；命令正文
	// 仍可引用宿主绝对路径，因此 run_shell 依然是宿主机高权限能力而非沙箱。
	allowedWorkDirRoot := func() string {
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
	resolveWorkingDir := func(args map[string]any) (string, error) {
		root := allowedWorkDirRoot()
		if root == "" {
			return "", fmt.Errorf("Shell 工作目录边界未配置，拒绝执行")
		}
		raw, _ := args["working_dir"].(string)
		if raw == "" {
			raw = "."
		}
		resolved, err := pathutil.ValidatePath(raw, root)
		if err != nil {
			return "", fmt.Errorf("Shell working_dir 被拒绝: %w", err)
		}
		return resolved, nil
	}

	rawFn := func(ctx context.Context, args map[string]any) (string, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return "", fmt.Errorf("缺少 command 参数")
		}
		hasPipeline, err := shellPipelineScope(args)
		if err != nil {
			return "", err
		}

		// 确定有效超时：args 优先，其次 Group 配置。
		effectiveTimeoutSec := timeoutSec
		if v, ok := args["timeout_sec"].(float64); ok && v > 0 {
			effectiveTimeoutSec = int(v)
		} else if v, ok := args["timeout_sec"].(int); ok && v > 0 {
			effectiveTimeoutSec = v
		}

		// wrappedFn 已把目录 canonicalize；这里再次 fail-closed，避免未来新增
		// 包装路径时绕过工作目录边界。
		workingDir, err := resolveWorkingDir(args)
		if err != nil {
			return "", err
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
		effID, err := effectPrepare(g.EffectJournal, ctx, g.AgentID,
			effect.KindShell, "cmd:"+digest12([]byte(command)),
			digest12([]byte(command+"\n"+workingDir)), effect.PolicyManualOnly)
		if err != nil {
			return "", err
		}

		start := time.Now()
		output, err := cmd.CombinedOutput()
		durationMS := time.Since(start).Milliseconds()
		// 完整 stdout/stderr 交给 L3 ToolResult envelope 持久化；本工具不在
		// ContentStore 之前做不可恢复截断。
		outStr := string(output)

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
				if journalErr := effectMarkUnknown(g.EffectJournal, effID,
					fmt.Sprintf("命令超时（%d 秒），已执行部分的副作用不可知", effectiveTimeoutSec)); journalErr != nil {
					return "", journalErr
				}
				return "", fmt.Errorf("命令执行超时（%d 秒）: %s", effectiveTimeoutSec, command)
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				execEv.ShellExec.Outcome = "failure"
				execEv.Error = err.Error()
				trace.Emit(execEv)
				// 启动失败（进程未运行）——结果已知：未产生进程副作用。
				if journalErr := effectSettle(g.EffectJournal, effID,
					"启动失败，未执行: "+err.Error(), false); journalErr != nil {
					return "", journalErr
				}
				return "", fmt.Errorf("启动命令失败: %w", err)
			}
		}

		exitScope := store.ShellExitCodeScopeWholeCommand
		if hasPipeline {
			exitScope = store.ShellExitCodeScopeLastPipelineCommand
		}
		execEv.ShellExec.ExitCode = exitCode
		execEv.ShellExec.ExitCodeScope = string(exitScope)
		if exitCode == 0 {
			execEv.ShellExec.Outcome = "success"
		} else {
			execEv.ShellExec.Outcome = "failure"
		}
		trace.Emit(execEv)

		if err := effectSettle(g.EffectJournal, effID,
			fmt.Sprintf("exit_code=%d exit_code_scope=%s outcome=%s duration_ms=%d out_bytes=%d out_sha256=%s",
				exitCode, exitScope, execEv.ShellExec.Outcome, durationMS, len(output), digest12(output)), true); err != nil {
			return "", err
		}

		warning := ""
		if exitScope == store.ShellExitCodeScopeLastPipelineCommand {
			warning = "\nwarning: pipeline detected; exit_code 只代表最后一个管道段，禁止据此声称整条测试/构建命令通过"
		}
		return fmt.Sprintf("exit_code: %d\nexit_code_scope: %s%s\nstdout+stderr:\n%s",
			exitCode, exitScope, warning, outStr), nil
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
	// Interaction 必须绑定 canonical 实际执行目录，而不是用户原始字符串。
	// 在进入拦截器前统一解析显式/默认目录，避免授权目录与真实 cwd 分叉。
	wrappedFn := func(ctx context.Context, args map[string]any) (string, error) {
		dir, err := resolveWorkingDir(args)
		if err != nil {
			return "", err
		}
		if _, err := shellPipelineScope(args); err != nil {
			return "", err
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
		String("working_dir", "执行命令的工作目录；普通任务必须位于 project_root 内，workspace 隔离任务必须位于该任务 workspace 内；留空使用当前允许根", false).
		Int("timeout_sec", "本次执行的超时秒数，留空时使用配置默认值", false).
		Bool("accept_last_pipeline_exit_code", "命令含 pipeline 时必须显式为 true 才执行；表示你接受 exit_code 仅属于最后一个管道段，且不会把它当成整条测试/构建命令的通过证据", false).
		Build()

	r.Register("run_shell", "[宿主机高权限能力，不是 OS 沙箱] 在受限工作目录下执行 shell 命令；命令正文仍可访问宿主绝对路径、网络和子进程。返回 stdout、stderr、exit_code 与 exit_code_scope；pipeline 默认拒绝，避免末段 exit=0 被误判为整条命令成功"+shellDialectNote(), params, wrappedFn)
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
