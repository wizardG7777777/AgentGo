package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/cli"
	"agentgo/internal/config"
	"agentgo/internal/hook"
	"agentgo/internal/hook/builtin"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/runner"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/watchdog"
	"agentgo/internal/webtool"
)

type System struct {
	Config          *config.Config
	Store           store.TaskStore
	Roster          roster.Roster
	EventCh         chan model.Event
	Watchdog        *watchdog.Watchdog
	CancelRegistry  *store.TaskCancelRegistry
	MailboxRegistry *mailbox.Registry
	MailNotifier    *mailbox.MailNotifier
	Scheduler       *scheduler.Bundle // Phase 3：scheduler 现在是 agent.Agent + Activator + ModeStore 的复合
	// v4：所有执行/调查代理都是 runner.Runner（取代旧 worker.Worker / explorer.Explorer
	// 两个 package；详见 nextUpgrade_v4.md §11.6.6）。kind × replicas 实例化在 Bootstrap()
	// 主流程展开。
	Runners    []*runner.Runner
	ApprovalCh chan shell.ApprovalRequest // 命令审批通道，Worker→CLI
	CLI        *cli.CLI
	ArtifactLog *store.ArtifactLog      // Artifacts 持久化日志，Shutdown 时需 Close；nil 表示持久化已禁用
	SessionMgr  *session.SessionManager // Session 管理器，nil 表示无 Session 模式
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

func Bootstrap(configPath string, explicit bool) (*System, error) {
	// Step 1: 加载配置
	cfg, err := config.LoadConfig(configPath, explicit)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("[启动] 全局配置加载完成")

	// Step 1.05: v4 配置校验（仅 cfg.Agents 非空时执行 12 条 §11.5.3 规则；
	//             v3 yaml 用户不受影响——cfg.Agents 为空时 Validate 退化为只校验 StartupProbe*）
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("v4 配置校验失败: %w", err)
	}

	// Step 1.1: 启动期 banner（§9.5.1）——打印逐 kind 摘要 + 脱敏 api_key，
	//             让用户视觉核对 YAML 是否被正确读取。
	//             configPath 单独打印，避免"测 v4 但启动了 v3 默认"之类的混淆。
	printStartupBanner(os.Stdout, configPath, cfg)

	// Step 1.2: 启动期 TCP probe（§9.5）——best-effort 连通性检查
	//             失败行为：默认 warning + 启动继续；startup_probe_failure_action="exit" 改为硬退出
	if probeErr := startupProbe(os.Stdout, cfg); probeErr != nil {
		if cfg.StartupProbeFailureAction == "exit" {
			return nil, fmt.Errorf("启动期 probe 失败（startup_probe_failure_action=exit）: %w", probeErr)
		}
		log.Printf("[WARN] startup probe: %v (best-effort, 启动继续)", probeErr)
	}

	// Step 1.3: 初始化 Session 管理器
	homeDir := cfg.ProjectRoot
	sessionCfg := session.SessionConfig{
		RetentionDays: cfg.SessionRetentionDays,
		ArchiveMax:    cfg.SessionArchiveMax,
		Enabled:       true,
	}
	sessDir := filepath.Join(homeDir, ".agentgo", "sessions")
	sessMgr, sessErr := session.NewSessionManager(sessDir, sessionCfg)
	if sessErr != nil {
		fmt.Printf("[启动] WARNING: Session 初始化失败: %v —— 以无 Session 模式运行\n", sessErr)
	}
	// 开启 history.jsonl 事件溯源（默认关闭，由 bootstrap 显式启用）
	if sessMgr != nil && sessMgr.Current() != nil {
		sessMgr.EnableHistoryLog()
	}

	// Step 1.5: 初始化 trace 系统（每任务一份 JSONL 文件，保留最近 100 个）
	// trace 写入失败仅打印 warning，不中断主流程
	traceDir := filepath.Join(cfg.ProjectRoot, ".agentgo", "traces")

	// 如果 Session 初始化成功，将 trace 目录重定向到 Session 的 logs/ 子目录
	if sessMgr != nil && sessMgr.Current() != nil {
		traceDir = sessMgr.LogDir()
	}

	traceWriter, traceErr := trace.NewWriter(traceDir, 100)
	if traceErr != nil {
		fmt.Printf("[启动] WARNING: trace 系统初始化失败 (dir=%s): %v\n", traceDir, traceErr)
	} else {
		trace.SetDefault(traceWriter)
		fmt.Printf("[启动] Trace 系统已启动 (dir=%s, 保留最近 100 个任务)\n", traceDir)
	}

	// Step 1.6: 初始化 prompt dumper（仅在 AGENTGO_DUMP_PROMPTS=1 时启用）
	dumpEnabled := os.Getenv("AGENTGO_DUMP_PROMPTS") == "1"
	dumper, dumperErr := trace.NewPromptDumper(traceDir, dumpEnabled)
	if dumperErr != nil {
		fmt.Printf("[启动] WARNING: prompt dumper 初始化失败: %v\n", dumperErr)
	} else if dumpEnabled {
		trace.SetDefaultDumper(dumper)
		fmt.Println("[启动] Prompt dump 已启用 (AGENTGO_DUMP_PROMPTS=1)")
	}

	// Step 2: 初始化公告板
	eventCh := make(chan model.Event, cfg.Infra.Store.EventChannelBuffer)
	taskStore := store.NewMemoryTaskStore(eventCh, cfg.Infra.Store.FIFOLimit, cfg.Infra.Store.DefaultConcurrency, cfg.Infra.Store.DefaultTimeoutSec)
	cancelRegistry := store.NewTaskCancelRegistry()
	taskStore.SetCancelRegistry(cancelRegistry)
	fmt.Println("[启动] 公告板初始化完成")

	// Step 2.3: Artifacts 持久化（JSONL 追加日志，2026-04-12 持久化专题起头）
	//
	// 只覆盖 Task.Artifacts 字段——下次启动时重放日志让未被 FIFO 淘汰的任务
	// 能恢复"这个任务都写过哪些文件"的记忆。其他字段（Task 状态 / Results /
	// Mailbox / Roster）仍然是纯内存，等具体需求驱动时再扩展。
	//
	// 设计决策（nextUpgrade_v3.md §9.6 + 2026-04-12 讨论）：
	//   - 选 JSONL 而不是 SQLite/BoltDB——单进程 KV 追加写没有关系库价值
	//   - 零新依赖——仅用标准库 encoding/json + os
	//   - 不做压缩——MVP 规模（~10 MB/年）日志增长可控
	//   - 初始化失败只打印 warning，不中断启动——持久化不是 P0，不能让
	//     工程上任何磁盘问题都阻塞 CLI 启动
	artifactLogDir := filepath.Join(cfg.ProjectRoot, ".agentgo", "state")
	artifactLog, alErr := store.OpenArtifactLog(artifactLogDir)
	if alErr != nil {
		fmt.Printf("[启动] WARNING: artifact log 初始化失败 (dir=%s): %v —— Artifacts 持久化已禁用\n", artifactLogDir, alErr)
	} else {
		// 先 replay 重建 map，然后注入 store 并恢复
		rebuilt, repErr := artifactLog.Replay()
		if repErr != nil {
			fmt.Printf("[启动] WARNING: artifact log 重放失败: %v —— 以空状态启动\n", repErr)
		}
		taskStore.SetArtifactLog(artifactLog)
		// RestoreArtifacts 在此刻是 no-op——store 里还没有任何任务，rebuilt 中
		// 的 taskID 全部找不到对应 task，调用会返回 (0, 0)。这是有意为之：
		// 本次专题的完整价值要等到"Task 状态持久化"专题落地后才能兑现——届时
		// bootstrap 会先 restore task 元数据，再 RestoreArtifacts 把 artifacts 填
		// 回对应 task.Artifacts 字段。当前阶段 artifact log 的作用是：
		//   (a) 新任务运行期间的 AppendArtifact 调用被持久化到日志
		//   (b) 日志本身作为 forensic 审计链（grep/jq 可读）
		//   (c) 为未来的 Task 状态持久化专题提供 ready-to-go 的存储组件
		restoredTasks, restoredArts := taskStore.RestoreArtifacts(rebuilt)
		if restoredTasks > 0 {
			fmt.Printf("[启动] Artifact 持久化已启用 (log=%s，恢复 %d 个任务 / %d 个 artifact)\n",
				artifactLog.Path(), restoredTasks, restoredArts)
		} else {
			fmt.Printf("[启动] Artifact 持久化已启用 (log=%s，日志中 %d 行记录，当前 store 中无匹配任务可恢复)\n",
				artifactLog.Path(), len(rebuilt))
		}
	}

	// Step 2.5: 初始化 Hook 系统（阶段 1）
	// hookReg 以单例方式被所有 worker/explorer 共享。recordToolCall 闭包
	// 是 llm_executor.go 自动写入 ToolCallRecord 的通道 —— 独立于 StoreHookView，
	// 避免 hook 通过接口写入任务历史（C4.3 方案 A，详见 hookSystem.md §11.1）。
	hookReg := hook.NewToolHookRegistry()
	var storeView store.StoreHookView = taskStore
	recordToolCall := func(taskID string, rec store.ToolCallRecord) {
		_ = taskStore.AppendToolCall(taskID, rec)
	}
	// 注册具体 hook（按 commit 渐进式增加）
	//
	// V6 回归验证（2026-04-09 已验证）：注释掉以下所有 Register 调用之后，
	// 整个测试套仍然全绿且行为字节级一致 — 这是阶段 1 可逆性的硬证明，
	// 也是 hookSystem.md §10.2 退出条件中最关键的一项。
	if err := hookReg.Register(builtin.NewRecordArtifactHook(storeView, cfg.ProjectRoot)); err != nil {
		return nil, fmt.Errorf("注册 RecordArtifactHook 失败: %w", err)
	}
	if err := hookReg.Register(builtin.NewPathBoundaryHook(cfg.ProjectRoot)); err != nil {
		return nil, fmt.Errorf("注册 PathBoundaryHook 失败: %w", err)
	}
	if err := hookReg.Register(builtin.NewValidateExpectedHashHook()); err != nil {
		return nil, fmt.Errorf("注册 ValidateExpectedHashHook 失败: %w", err)
	}
	if err := hookReg.Register(builtin.NewRequireReadBeforeWriteHook(storeView)); err != nil {
		return nil, fmt.Errorf("注册 RequireReadBeforeWriteHook 失败: %w", err)
	}
	if err := hookReg.Register(builtin.NewDependencyValidatorHook(storeView)); err != nil {
		return nil, fmt.Errorf("注册 DependencyValidatorHook 失败: %w", err)
	}
	if err := hookReg.Register(builtin.NewEnforceExpectedArtifactsHook(storeView, cfg.ProjectRoot)); err != nil {
		return nil, fmt.Errorf("注册 EnforceExpectedArtifactsHook 失败: %w", err)
	}
	if err := hookReg.Register(builtin.NewValidateLineAnchorsHook()); err != nil {
		return nil, fmt.Errorf("注册 ValidateLineAnchorsHook 失败: %w", err)
	}
	fmt.Println("[启动] Hook 系统初始化完成（已注册：record-artifact, path-boundary, validate-expected-hash, validate-line-anchors, require-read-before-write, dependency-validator, enforce-expected-artifacts）")

	// Step 3: 初始化花名册
	r := roster.NewMemoryRoster()
	fmt.Println("[启动] 花名册初始化完成")

	// Step 3.5: 初始化邮箱注册表（v4：缓冲区大小是系统级常量，不暴露 yaml）
	mbRegistry := mailbox.NewRegistry(mailbox.DefaultInboxSize)
	fmt.Println("[启动] 邮箱注册表初始化完成")

	// Step 3.5.1: 将 Session 的 HistoryEmitter 注入 store/roster/mailbox，
	// 否则 history.jsonl 永远不会被写入（v3 §9.9 阶段三装配补齐）。
	if sessMgr != nil && sessMgr.History() != nil {
		taskStore.SetHistoryEmitter(sessMgr.History())
		r.SetHistoryEmitter(sessMgr.History())
		mbRegistry.SetHistoryEmitter(sessMgr.History())
		fmt.Println("[启动] Session history.jsonl 事件发射器已注入（store/roster/mailbox）")
	}

	// Step 3.6: 初始化 Mailbox Hook 系统（Phase 2）
	// 与 ToolHookRegistry 并列共存、独立。registry 创建后立即通过
	// AsMailboxRunner 适配器挂接到 mbRegistry，使后续的 mailbox.Send 路径
	// 走 BeforeSend / BeforeDeliver 决策。
	//
	// V9 回归验证（B9 步骤）：注释掉以下所有 Register 调用之后，整个测试套
	// 仍然全绿且 mailbox 行为字节级一致 — 这是 Phase 2 可逆性的硬证明。
	mailboxHookReg := hook.NewMailboxHookRegistry()
	if err := mailboxHookReg.Register(builtin.NewChainDepthLimitHook(mailbox.DefaultChainMaxDepth)); err != nil {
		return nil, fmt.Errorf("注册 ChainDepthLimitHook 失败: %w", err)
	}
	if err := mailboxHookReg.Register(builtin.NewPerAgentDedupHook(storeView)); err != nil {
		return nil, fmt.Errorf("注册 PerAgentDedupHook 失败: %w", err)
	}
	if err := mailboxHookReg.Register(builtin.NewWakeWorthyFilterHook(mbRegistry, mbRegistry)); err != nil {
		return nil, fmt.Errorf("注册 WakeWorthyFilterHook 失败: %w", err)
	}
	if err := mailboxHookReg.Register(builtin.NewWakeContextExpandHook(mbRegistry, 5)); err != nil {
		return nil, fmt.Errorf("注册 WakeContextExpandHook 失败: %w", err)
	}
	mbRegistry.AttachHookRunner(hook.AsMailboxRunner(mailboxHookReg))
	fmt.Printf("[启动] Mailbox Hook 系统初始化完成（已注册：chain-depth-limit max=%d, per-agent-dedup, wake-worthy-filter, wake-context-expand）\n", mailbox.DefaultChainMaxDepth)

	// Step 3.7: 初始化 Agent Hook 系统（Sprint 1）
	// 覆盖 ReactLoop 4 个生命周期事件（PhaseTaskStart / LoopPre / LoopPost / TaskEnd）。
	// 与 ToolHookRegistry / MailboxHookRegistry 并列独立。
	//
	// 当前注册内容：TeamAwarenessHook（三 section 合并版）——
	//   Section 1 TeamSnapshot：委托给 worker.BuildTeamSnapshot，每 5 轮 + ForceOnMail 刷新
	//   Section 2 FileAwareness：读 Roster.ListClaims，与 Team 共享频率
	//   Section 3 GoalAnchor：每 3 轮刷新，防目标漂移（ROI 最高的注入）
	// 超预算时截断优先级：goal > team > file
	//
	// Scheduler **不**注册 Agent Hook —— scheduler 自带 board snapshot 机制，
	// 与 TeamAwarenessHook 职责重叠。见 nextUpgrade_v3.md §7.10。
	//
	// 可逆性验证：注释掉以下 Register 调用后，既有测试套仍必须全绿且
	// 行为与 Sprint 1 前完全一致（代码里唯一的差别是硬编码 TeamSnapshot
	// 注入已被 PhaseTaskStart hook 取代，二者对同一输入产出相同内容）。
	agentHookReg := hook.NewAgentHookRegistry()
	// SnapshotFn 闭包捕获 taskStore 和 mbRegistry。BuildTeamSnapshot 的逻辑
	// 与原 agent.go:215 硬编码调用的函数是同一个，迁移时行为字节级保留。
	taCfg := builtin.TeamAwarenessConfig{
		SnapshotFn: func(selfID string) string {
			return agent.BuildTeamSnapshot(selfID, taskStore, mbRegistry)
		},
		SnapshotRefreshInterval: 5,
		GoalRefreshInterval:     3,
		ForceOnMail:             true,
		MaxTokens:               800,
		GoalEnabled:             true,
		FileEnabled:             true,
		RecentToolsWindow:       5,
	}
	for _, h := range builtin.NewTeamAwarenessHooks(taCfg) {
		if err := agentHookReg.Register(h); err != nil {
			return nil, fmt.Errorf("注册 %s 失败: %w", h.Name(), err)
		}
	}
	fmt.Println("[启动] Agent Hook 系统初始化完成（已注册：team-awareness-task-start, team-awareness-loop-pre）")

	// Agent Hook 所需的只读视图：StoreHookView 适配器（从已有的 storeView 构造）
	// + Roster 本身实现了 hook.AgentRosterView（通过 roster/hookview.go 扩展）。
	agentStoreView := agent.NewStoreHookAdapter(storeView)
	var agentRosterView hook.AgentRosterView = r

	// Step 4: 创建 scheduler LLM 客户端
	// scheduler model 优先用 cfg.Scheduler.Model（§11.5.5 仅允许该字段外部覆盖），
	// 缺省回落 cfg.LLM.DefaultModel。LLM endpoint / api_key / provider 与 worker 共享。
	schedulerLLM := buildKindLLMClient(cfg.LLM, cfg.Scheduler.Model)

	// Step 5.5: 构造特化代理注册表（Sprint 3 #7 Scheduler 分配感知）
	// v4：扫描 cfg.Agents，把所有 EventType != "" 的 kind 注册为特化代理。
	// 这取代了 v3 时代基于 cfg.AgentDeclarations + cfg.ExplorerEventType 的硬编码逻辑——
	// 现在用户可以声明任意命名的特化 kind（不止 explorer），event_type 字段就是分派键。
	agentRegistry := scheduler.NewAgentRegistry()
	for _, kind := range cfg.Agents {
		if kind.EventType == "" {
			continue // 默认队列（worker 类）—— 不算特化
		}
		caps := kind.Tools
		if len(caps) == 0 && kind.Profile != "" {
			caps = cfg.ToolProfiles[kind.Profile] // profile 解析失败留 nil，启动校验已保证 profile 存在
		}
		agentRegistry.Register(scheduler.SpecializedAgent{
			EventType:    kind.EventType,
			Count:        kind.Replicas,
			Role:         fmt.Sprintf("kind=%s（监听 event_type=%q）", kind.Kind, kind.EventType),
			Capabilities: caps,
		})
	}
	for _, sa := range agentRegistry.Specialized() {
		desc := sa.Role
		if len(desc) > 80 {
			desc = desc[:80] + "…"
		}
		fmt.Printf("[启动] 特化代理已注册: EventType=%s, description=%s\n", sa.EventType, desc)
	}
	fmt.Printf("[启动] Agent 注册表初始化完成（%d 个特化代理）\n", len(agentRegistry.Specialized()))

	// Step 6: 创建看门狗（先于 scheduler 创建——scheduler 需要 approvalCh，但 watchdog 不需要）
	w := watchdog.New(taskStore, cfg, eventCh, r, mbRegistry)

	// Step 6.5: 校验 profile 中的工具名拼写（v4：不再在此预解析 worker/explorer profile，
	//             各 kind 的 profile 解析延后到 buildAgentRuntime）
	for profileName, toolNames := range cfg.ToolProfiles {
		if err := tools.ValidateToolNames(toolNames); err != nil {
			return nil, fmt.Errorf("tool_profiles.%s 校验失败: %w", profileName, err)
		}
	}

	// Step 6.8: 工具可用性探针
	//
	// 2026-04-27 修复：先构造 SearchProvider，再用其 Name() 派发 probe。
	// 历史问题：probe.NewWebSearchProbe 按 cfg.SearchAPIProvider（用户配置原文）派发，
	// 而 webtool.NewProvider 在 key/URL 缺失时静默回落 DDG——导致 probe 报
	// "serper unavailable"，但实际跑的是 DDG，可工作。Scheduler 因 unavailable_tools
	// 误以为 web_search 不可用，自我克制不派网络任务。修复：把 fallback 决策从
	// webtool 抽到 bootstrap，并按实际 provider.Name() 派发 probe。
	searchProvider, fellBack, fallbackReason := webtool.NewProviderWithDefault(
		cfg.SearchAPIProvider, cfg.SearchAPIURL, cfg.SearchAPIKey)
	if fellBack {
		fmt.Printf("[启动] web_search: %s，已回落到 %s（工具仍可用，但能力可能降级）\n",
			fallbackReason, searchProvider.Name())
	}
	// fallback 后 DDG 不需要 apiURL/apiKey；显式置空避免误导后续维护者。
	probeURL, probeKey := cfg.SearchAPIURL, cfg.SearchAPIKey
	if fellBack {
		probeURL, probeKey = "", ""
	}
	probes := []probe.Probe{
		probe.NewWebSearchProbe(searchProvider.Name(), probeURL, probeKey),
		probe.NewWebFetchProbe(""),
	}
	toolHealth := probe.RunAll(context.Background(), probes, 10*time.Second)

	// 打印启动日志
	if unavailable := toolHealth.UnavailableTools(); len(unavailable) == 0 {
		fmt.Println("[启动] 工具可用性探测完成（全部可用）")
	} else {
		for _, r := range toolHealth.Results() {
			if !r.Available {
				fmt.Printf("[警告] 工具 %s 不可用: %s，相关代理将降级运行\n", r.Tool, r.Error)
			}
		}
	}

	// Step 7.5: 创建命令审批通道（Worker→CLI）
	approvalCh := make(chan shell.ApprovalRequest, 8)

	// Step 5: 创建调度器（Phase 3：scheduler 是 agent.Agent 实例 + Activator + ModeStore）
	// 工具集 = Worker 全集 + SchedulerGroup，可以读文件 / 搜索 / 查网页 / 跑 shell。
	// EventCh 由 Activator 监听，转换为 EventType="__scheduler__" 的 task。
	sched := scheduler.New(
		taskStore, r, schedulerLLM, eventCh, cfg, cancelRegistry, mbRegistry, approvalCh,
		hookReg, storeView, recordToolCall, agentRegistry,
	)
	sched.SchedulerExec.ToolHealth = toolHealth

	// Step 8: 创建执行代理（v4 §11.6.1 唯一路径——按 kind × replicas 实例化统一 Runner）
	// 共享 RunnerDeps 一次构造、所有 kind/replica 共用
	// searchProvider 已在 Step 6.8 构造，复用同一实例（避免重复 fallback 日志）。
	shellFilter, fErr := shell.BuildFilter(cfg.ProjectRoot, cfg.ShellBlacklist, cfg.ShellGreylist)
	if fErr != nil {
		shellFilter = shell.NewCommandFilter(shell.DefaultBlacklist, shell.DefaultGreylist)
		fmt.Printf("[启动] WARNING: shell 过滤器规则加载失败，使用默认规则: %v\n", fErr)
	}
	deps := runner.RunnerDeps{
		Store:                 taskStore,
		Roster:                r,
		HookReg:               hookReg,
		StoreView:             storeView,
		RecordToolCall:        recordToolCall,
		AgentHookReg:          agentHookReg,
		AgentStoreView:        agentStoreView,
		AgentRosterView:       agentRosterView,
		MBRegistry:            mbRegistry,
		CancelRegistry:        cancelRegistry,
		SearchProvider:        searchProvider,
		ShellFilter:           shellFilter,
		ApprovalCh:            approvalCh,
		ProjectRoot:           cfg.ProjectRoot,
		RosterWaitTimeoutSec:  cfg.Infra.Roster.WaitTimeoutSec,
		ShellTimeoutSec:       cfg.ShellTimeoutSec,
		MaxSubtaskDepth:       cfg.MaxSubtaskDepth,
		TransferNoteMaxTokens: cfg.TransferNoteMaxTokens,
		ProgressNotifyEnabled: cfg.ProgressNotifyEnabled,
		HashlineEnabled:       *cfg.HashlineEnabled,
	}
	var runners []*runner.Runner
	for _, kind := range cfg.Agents {
		kindLLM := buildKindLLMClient(cfg.LLM, kind.Model)
		for i := 1; i <= kind.Replicas; i++ {
			rt, rtErr := buildAgentRuntime(kind, cfg.LLM, cfg.ToolProfiles, i)
			if rtErr != nil {
				return nil, fmt.Errorf("kind=%q replica=%d 运行时构造失败: %w", kind.Kind, i, rtErr)
			}
			if err := tools.ValidateToolNames(rt.AllowedTools); err != nil {
				return nil, fmt.Errorf("kind=%q replica=%d 工具名校验失败: %w", kind.Kind, i, err)
			}
			kindDeps := deps
			kindDeps.LLMClient = kindLLM
			rn := runner.New(rt, kindDeps)
			runners = append(runners, rn)
			fmt.Printf("[启动] Runner %s 已启动 [kind=%s, model=%s]\n",
				rt.InstanceID, kind.Kind, rt.Model)
		}
	}

	// Step 9: 创建邮差通知器
	notifierInterval := time.Duration(cfg.Infra.MailNotifier.IntervalSec) * time.Second
	mailNotifier := mailbox.NewMailNotifier(mbRegistry, taskStore, notifierInterval)

	sys := &System{
		Config:          cfg,
		Store:           taskStore,
		Roster:          r,
		EventCh:         eventCh,
		Watchdog:        w,
		CancelRegistry:  cancelRegistry,
		MailboxRegistry: mbRegistry,
		MailNotifier:    mailNotifier,
		ArtifactLog:     artifactLog, // 可能为 nil（OpenArtifactLog 失败时），Shutdown 会判空
		SessionMgr:      sessMgr,     // 可能为 nil（Session 初始化失败时），Shutdown 会判空
		Scheduler:       sched,
		Runners:         runners,
		ApprovalCh:      approvalCh,
	}

	return sys, nil
}

// Start 启动所有后台 goroutine。cancel 用于 CLI /quit 触发全局退出。
func (s *System) Start(ctx context.Context, cancel context.CancelFunc) {
	s.cancel = cancel

	// Step 5: 启动调度器（Phase 3：两个 goroutine —— Agent poll + Activator 事件桥）
	// Activator 必须先就绪，否则 EventUserInput 在 Agent 未启动时就到达可能会被丢
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Scheduler.Activator.Run(ctx)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Scheduler.Agent.Run(ctx)
	}()
	fmt.Println("[启动] 调度器已启动 (agent + activator)")

	// Step 6: 启动看门狗
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWatchdogWithRecover(ctx)
	}()
	fmt.Println("[启动] 看门狗已启动")

	// Step 6.5: 启动邮差通知器（默认开启；可通过 infra.mail_notifier.enabled=false 关闭）
	if s.Config.Infra.MailNotifier.Enabled {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.MailNotifier.Run(ctx)
		}()
		fmt.Println("[启动] 邮差通知器已启动")
	} else {
		fmt.Println("[启动] 邮差通知器已禁用 (infra.mail_notifier.enabled=false) — 邮件不会自动唤醒空闲代理")
	}

	// Step 7+8: 启动所有 v4 Runner（worker / explorer / 自定义 kind 统一走这条路径）
	for _, rn := range s.Runners {
		rn := rn // 闭包捕获
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			rn.Run(ctx)
		}()
	}
	fmt.Printf("[启动] kind-based agents 已启动 (%d 个 runner 实例)\n", len(s.Runners))

	fmt.Println("[启动] 系统就绪，等待用户输入")
}

// RunCLI 启动 CLI 主循环，阻塞直到用户退出或 ctx 取消。
func (s *System) RunCLI(ctx context.Context, reader io.Reader, writer io.Writer) {
	s.CLI = cli.New(s.Store, s.EventCh, s.cancel, s.Scheduler, s.MailboxRegistry, s.ApprovalCh, reader, writer, s.SessionMgr)
	s.CLI.Run(ctx)
}

// Shutdown 优雅关闭所有服务。
func (s *System) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	// 关闭 trace 写入器，flush 所有打开的文件句柄
	if w := trace.Default(); w != nil {
		w.Close()
	}
	if d := trace.DefaultDumper(); d != nil {
		d.Close()
	}
	// 关闭 artifact 持久化日志——确保缓冲内容 flush 到磁盘
	if s.ArtifactLog != nil {
		if err := s.ArtifactLog.Close(); err != nil {
			fmt.Printf("[关闭] WARNING: artifact log 关闭失败: %v\n", err)
		}
	}
	// 关闭 Session 管理器——更新 metadata 并关闭日志文件句柄
	if s.SessionMgr != nil {
		if err := s.SessionMgr.Close(); err != nil {
			fmt.Printf("[关闭] WARNING: Session 关闭失败: %v\n", err)
		}
	}
	fmt.Println("[关闭] 系统已停止")
}

func (s *System) runWatchdogWithRecover(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[watchdog] panic recovered: %v, restarting...", r)
				}
			}()
			s.Watchdog.Run(ctx)
		}()

		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}
