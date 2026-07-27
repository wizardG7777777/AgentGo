package runner

// dependency_map.go 实现 nextUpgrade_v4.md §11.6.2 工具 → 依赖项静态映射。
//
// 用户在 YAML 里只声明工具名，不需要懂内部依赖图。新增工具时同步更新本文件。
//
// 当前映射表：
//
//	| 工具 | 自动注入的依赖 |
//	|---|---|
//	| read_file / list_dir / grep_search / glob_search | Workdir + FileStateCache |
//	| write_file / edit_file | + Roster（文件级写锁，附 RosterWaitTimeoutSec）；strict 审批 WrapHandler 在 runner.New 装配 |
//	| run_shell | + interaction.Service + SessionID + shell.CommandFilter + ShellTimeoutSec + Modes（exec 轴短路）；验收角色（白名单含 submit_acceptance_result）再注入 shell.AcceptanceHardeningGreylist；workdir 实现 tools.ActiveViewer 时（*workspace.Swapper）注入 ActiveViewer |
//	| publish_task | + Store + TaskHolder + MaxSubtaskDepth |
//	| request_user_input | + interaction.Service + SessionID + TaskHolder |
//	| send_message | + mailbox.Registry + MailChainMaxDepth（常量）|
//	| web_search / web_fetch | + webtool.SearchProvider |
//	| request_replan / acceptance tools | + PlanCoordinator + Store + TaskHolder |
//	| submit_task_result | + Store + TaskHolder + FinalizationNotifier + SubmitState（runner.New 注入）|
//
// 实际注册由 resolveToolGroups 完成——它按 RunnerDeps 构造全部 ToolGroup，
// 再由 ToolRegistry 的 allowlist 自动剪枝。 unauthorized 工具根本不进 ToolRegistry。

import (
	"agentgo/internal/agent"
	"agentgo/internal/shell"
	"agentgo/internal/tools"
)

// resolveToolGroups 按 RunnerDeps 构造全部 ToolGroup。
//
// 返回的 slice 包含所有可能用到的 Group，由调用方传入 ToolRegistry 后
// 由 allowlist 过滤实际生效集。这样新增工具时只需修改本函数一处。
// allowedTools 即该 runtime 的工具白名单（rt.AllowedTools），除注册剪枝外，
// 目前还用于验收角色判定（见 acceptanceShellGreylist）。
//
// holder / finHolder / submitState / fileCache / workdir 在 New() 中提前创建，
// 供 agent 回调和 ToolGroup 共享。workdir 生产上是 per-runner 的
// *workspace.Swapper（同时实现 tools.ActiveViewer——run_shell 默认工作目录
// 随隔离视图切换）；单测直构的 DefaultWorkdir 不满足该可选接口，shell 行为
// 与旧版完全一致。
// interactionWaitHook 同时透传给 ShellGroup 与 MetaGroup（交互等待 → agent 状态机
// 接线，见 runner.New）。
func resolveToolGroups(
	instanceID string,
	allowedTools []string,
	deps RunnerDeps,
	holder *CurrentTaskHolder,
	finHolder *agent.FinalizationHolder,
	submitState *agent.SubmitState,
	fileCache *agent.FileStateCache,
	workdir tools.WorkdirProvider,
	interactionWaitHook func(waiting bool),
) []tools.ToolGroup {
	readGroup := tools.LocalReadGroup{
		Workdir:         workdir,
		Cache:           fileCache,
		HashlineEnabled: deps.HashlineEnabled,
	}
	// 按任务写时复制隔离：workdir 实现 ActiveViewer 时（*workspace.Swapper）
	// 把它注入 shell 组；未实现时 ActiveViewer=nil，run_shell 默认目录恒为主根。
	activeViewer, _ := workdir.(tools.ActiveViewer)
	return []tools.ToolGroup{
		readGroup,
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         deps.Roster,
			AgentID:        instanceID,
			WaitTimeoutSec: deps.RosterWaitTimeoutSec,
		},
		tools.WebGroup{Provider: deps.SearchProvider},
		tools.ShellGroup{
			Workdir:             workdir,
			TimeoutSec:          deps.ShellTimeoutSec,
			Interactions:        deps.Interactions,
			SessionID:           deps.SessionID,
			AgentID:             instanceID,
			Filter:              deps.ShellFilter,
			ExtraGreylist:       acceptanceShellGreylist(allowedTools),
			Modes:               deps.Modes,
			InteractionWaitHook: interactionWaitHook,
			ActiveViewer:        activeViewer,
		},
		tools.MetaGroup{
			Store:               deps.Store,
			Holder:              holder,
			MaxDepth:            deps.MaxSubtaskDepth,
			MBRegistry:          deps.MBRegistry,
			AgentID:             instanceID,
			Interactions:        deps.Interactions,
			SessionID:           deps.SessionID,
			InteractionWaitHook: interactionWaitHook,
			RouteValidator:      deps.RouteValidator,
		},
		tools.PlanControlGroup{
			Coordinator:          deps.PlanCoordinator,
			Store:                deps.Store,
			Holder:               holder,
			AgentID:              instanceID,
			FinalizationNotifier: finHolder,
			SubmitState:          submitState,
		},
	}
}

// acceptanceShellGreylist 判定当前 runtime 是否为"正式验收角色"，是则返回
// 验收加固灰名单（写倾向 shell 命令一律升级为灰名单 Interaction 审批），
// 否则返回 nil——ShellGroup 行为与未注入时完全一致（非验收语境不变）。
//
// 判定依据：工具白名单包含 submit_acceptance_result。该工具仅对绑定
// AcceptanceRun 的验收 Task 有意义（见 tools/plan_control.go 与
// tools/known_tools.go 的分组注释），普通执行 / 探索 kind 不会持有；
// 比 kind 名 / event_type 可靠——kind 名可由用户在 YAML 任意命名，而验收
// 能力必须通过白名单显式授予该工具（config.example.yaml 的
// acceptance_verifier profile 与 agenttemplate 的 builtin/verifier 均如此）。
//
// 注意：allowedTools 为 nil / 空时不加固。生产路径上 kind 必须声明 profile
// 或 tools（runtime_builder 强制二选一），nil 只出现在单测直构场景；
// 对 nil 加固会让"未配置白名单"的旧行为发生意外变化，违背非验收语境不变原则。
//
// 背景与边界：run_shell 不是只读沙箱（docs/activate/AgentTemplate.md:148
// 明示），verifier 可经 shell 污染被验收对象；本函数是 worktree 隔离
// （架构级方案，另案）落地前的过渡收紧。
func acceptanceShellGreylist(allowedTools []string) []string {
	for _, name := range allowedTools {
		if name == "submit_acceptance_result" {
			return shell.AcceptanceHardeningGreylist
		}
	}
	return nil
}
