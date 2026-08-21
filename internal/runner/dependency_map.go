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
//	| write_file / edit_file | + Roster（文件级写锁，附 RosterWaitTimeoutSec）+ StoreHookView（同步 artifact ledger）；strict 审批 WrapHandler 在 runner.New 装配；+ EffectJournal（H2b 副作用账本，nil 不记账） |
//	| run_shell | + interaction.Service + SessionID + shell.CommandFilter + ShellTimeoutSec + Modes（exec 轴短路）；验收角色（白名单含 run_shell 且不含写工具）再注入 shell.AcceptanceHardeningGreylist；workdir 实现 tools.ActiveViewer 时（*workspace.Swapper）注入 ActiveViewer；+ EffectJournal |
//	| publish_task | + Store + TaskHolder + MaxSubtaskDepth |
//	| request_user_input | + interaction.Service + SessionID + TaskHolder |
//	| send_message | + mailbox.Registry + MailChainMaxDepth（常量）+ EffectJournal |
//	| web_search / web_fetch | + webtool.SearchProvider |
//	| request_replan | + Store + TaskHolder |
//	| submit_task_result | + Store + TaskHolder + FinalizationNotifier + SubmitState（runner.New 注入）+ OutletChecker（v2 图提交期出路检查，nil 不检查） |
//
// 实际注册由 resolveToolGroups 完成——它按 RunnerDeps 构造全部 ToolGroup，
// 再由 ToolRegistry 的 allowlist 自动剪枝。 unauthorized 工具根本不进 ToolRegistry。

import (
	"agentgo/internal/agent"
	"agentgo/internal/shell"
	"agentgo/internal/store"
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
	artifactStore := deps.StoreView
	if artifactStore == nil {
		// 生产 MemoryTaskStore 同时实现 TaskStore/StoreHookView；该回退
		// 让精简装配也不会遗漏写工具的同步 artifact ledger。
		artifactStore, _ = deps.Store.(store.StoreHookView)
	}
	return []tools.ToolGroup{
		readGroup,
		tools.LocalWriteGroup{
			LocalReadGroup: readGroup,
			Roster:         deps.Roster,
			AgentID:        instanceID,
			ArtifactStore:  artifactStore,
			WaitTimeoutSec: deps.RosterWaitTimeoutSec,
			EffectJournal:  deps.EffectJournal,
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
			EffectJournal:       deps.EffectJournal,
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
			EffectJournal:       deps.EffectJournal,
		},
		tools.PlanControlGroup{
			Store:                deps.Store,
			Holder:               holder,
			AgentID:              instanceID,
			FinalizationNotifier: finHolder,
			SubmitState:          submitState,
			ArtifactResolver:     agent.NewArtifactPhysicalResolver(deps.ProjectRoot, deps.WorkspaceManager),
			OutletChecker:        deps.OutletChecker,
		},
	}
}

// acceptanceShellGreylist 判定当前 runtime 是否为"验收角色"，是则返回
// 验收加固灰名单（写倾向 shell 命令一律升级为灰名单 Interaction 审批），
// 否则返回 nil——ShellGroup 行为与未注入时完全一致（非验收语境不变）。
//
// 判定依据（C6b 重键）：白名单含 run_shell 但不含 write_file/edit_file。
// 验收 runner（Graph acceptance 节点 / verifier 模板）的特征正是「有 shell
// 却没有写工具」——shell 是其唯一可能污染被验收对象的通道；普通执行 kind
// 持有写工具，不命中。原判定键 submit_acceptance_result 已随验收四工具删除。
//
// 注意：allowedTools 为 nil / 空时不加固。生产路径上 kind 必须声明 profile
// 或 tools（runtime_builder 强制二选一），nil 只出现在单测直构场景；
// 对 nil 加固会让"未配置白名单"的旧行为发生意外变化，违背非验收语境不变原则。
//
// 背景与边界：run_shell 不是只读沙箱（docs/activate/AgentTemplate.md:148
// 明示），verifier 可经 shell 污染被验收对象；本函数是 worktree 隔离
// （架构级方案，另案）落地前的过渡收紧。
func acceptanceShellGreylist(allowedTools []string) []string {
	if len(allowedTools) == 0 {
		return nil
	}
	hasShell := false
	hasWrite := false
	for _, name := range allowedTools {
		switch name {
		case "run_shell":
			hasShell = true
		case "write_file", "edit_file":
			hasWrite = true
		}
	}
	if hasShell && !hasWrite {
		return shell.AcceptanceHardeningGreylist
	}
	return nil
}
