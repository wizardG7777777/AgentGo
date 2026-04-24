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
	"agentgo/internal/explorer"
	"agentgo/internal/hook"
	"agentgo/internal/hook/builtin"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/watchdog"
	"agentgo/internal/webtool"
	"agentgo/internal/worker"
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
	Explorer        *explorer.Explorer
	Workers         []*worker.Worker
	ApprovalCh      chan shell.ApprovalRequest // 命令审批通道，Worker→CLI
	CLI             *cli.CLI
	ArtifactLog     *store.ArtifactLog      // Artifacts 持久化日志，Shutdown 时需 Close；nil 表示持久化已禁用
	SessionMgr      *session.SessionManager // Session 管理器，nil 表示无 Session 模式
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

func Bootstrap(configPath string, explicit bool) (*System, error) {
	// Step 1: 加载配置
	cfg, err := config.LoadConfig(configPath, explicit)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("[启动] 全局配置加载完成")

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
	eventCh := make(chan model.Event, cfg.EventChannelBuffer)
	taskStore := store.NewMemoryTaskStore(eventCh, cfg.FIFOLimit, cfg.DefaultConcurrency, cfg.DefaultTimeoutSec)
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
	fmt.Println("[启动] Hook 系统初始化完成（已注册：record-artifact, path-boundary, validate-expected-hash, require-read-before-write, dependency-validator, enforce-expected-artifacts）")

	// Step 3: 初始化花名册
	r := roster.NewMemoryRoster()
	fmt.Println("[启动] 花名册初始化完成")

	// Step 3.5: 初始化邮箱注册表
	mbRegistry := mailbox.NewRegistry(cfg.MailboxBufferSize)
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
	if err := mailboxHookReg.Register(builtin.NewChainDepthLimitHook(cfg.MailChainMaxDepth)); err != nil {
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
	fmt.Printf("[启动] Mailbox Hook 系统初始化完成（已注册：chain-depth-limit max=%d, per-agent-dedup, wake-worthy-filter, wake-context-expand）\n", cfg.MailChainMaxDepth)

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
			return worker.BuildTeamSnapshot(selfID, taskStore, mbRegistry)
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

	// Step 4: 创建 LLM 客户端
	// ExplorerProvider 为空时 fallback 到主 LLMProvider
	explorerProviderName := cfg.ExplorerProvider
	if explorerProviderName == "" {
		explorerProviderName = cfg.LLMProvider
	}
	schedulerLLM := llm.NewSDKClient(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
		"", // system prompt 由 scheduler 内部管理
		cfg.LLMProvider,
		time.Duration(cfg.LLMTimeoutSec)*time.Second,
	)
	explorerLLM := llm.NewSDKClient(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.ExplorerModel,
		"", // system prompt 由 explorer 内部管理
		explorerProviderName,
		time.Duration(cfg.LLMTimeoutSec)*time.Second,
	)

	// Step 5.5: 构造特化代理注册表（Sprint 3 #7 Scheduler 分配感知）
	// 当前 AgentGo 只有 Explorer 一种特化代理，静态声明即可。未来出现第二种
	// 特化类型时，在这里追加 Register 调用；scheduler 的 board snapshot 会
	// 自动把它渲染到 Resources.SpecializedAgents。
	agentRegistry := scheduler.NewAgentRegistry()
	// 从配置读取 Explorer 声明（Requirements 2.1, 2.2）
	explorerCaps, explorerDesc := cfg.ResolvedAgentDeclaration("explorer")
	agentRegistry.Register(scheduler.SpecializedAgent{
		EventType:    cfg.ExplorerEventType, // 通常是 "explore"
		Count:        1,                     // 当前架构每种特化代理各一个实例
		Role:         explorerDesc,
		Capabilities: explorerCaps,
	})
	// 启动日志：输出每个已注册特化代理的 EventType 和 description 摘要（Requirements 2.3）
	for _, sa := range agentRegistry.Specialized() {
		desc := sa.Role
		if len(desc) > 80 {
			desc = desc[:80] + "…"
		}
		fmt.Printf("[启动] 特化代理已注册: EventType=%s, description=%s\n", sa.EventType, desc)
	}
	fmt.Printf("[启动] Agent 注册表初始化完成（%d 个特化代理）\n", len(agentRegistry.Specialized()))

	// Step 6: 创建看门狗（先于 scheduler 创建——scheduler 需要 approvalCh，但 watchdog 不需要）
	w := watchdog.New(taskStore, cfg, eventCh, r)

	// Step 6.5: 解析工具集 profile（§9.1 Tool Set Profiles）
	workerAllowed, err := cfg.ResolveProfile(cfg.WorkerProfile)
	if err != nil {
		return nil, fmt.Errorf("worker profile 解析失败: %w", err)
	}
	explorerAllowed, err := cfg.ResolveProfile(cfg.ExplorerProfile)
	if err != nil {
		return nil, fmt.Errorf("explorer profile 解析失败: %w", err)
	}
	// 校验 profile 中的工具名拼写
	for profileName, toolNames := range cfg.ToolProfiles {
		if err := tools.ValidateToolNames(toolNames); err != nil {
			return nil, fmt.Errorf("tool_profiles.%s 校验失败: %w", profileName, err)
		}
	}

	// Step 6.8: 工具可用性探针
	probes := []probe.Probe{
		probe.NewWebSearchProbe(cfg.SearchAPIProvider, cfg.SearchAPIURL, cfg.SearchAPIKey),
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

	// Step 7: 创建调查代理（复用与 Worker 相同的 SearchProvider 配置）
	explorerSearchProvider := webtool.NewProvider(cfg.SearchAPIProvider, cfg.SearchAPIURL, cfg.SearchAPIKey)
	exp := explorer.New(taskStore, r, explorerLLM, cfg, cancelRegistry, mbRegistry, hookReg, storeView, recordToolCall, agentHookReg, agentStoreView, agentRosterView, explorerAllowed, explorerSearchProvider)

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

	// Step 8: 创建执行代理（使用主 LLM，认领 event_type="" 的执行任务）
	var workers []*worker.Worker
	if len(cfg.Workers) > 0 {
		// 新路径：按 workers 列表逐一创建 Worker
		if err := cfg.ValidateWorkers(); err != nil {
			return nil, fmt.Errorf("workers 列表校验失败: %w", err)
		}

		// 构建 WorkerProfiles 和 WorkerCapabilitiesByProfile 供 Scheduler board snapshot 使用
		workerProfiles := make(map[string]string, len(cfg.Workers))
		capsByProfile := make(map[string]*scheduler.AgentCapabilityInfo)
		for _, decl := range cfg.Workers {
			workerProfiles[decl.ID] = decl.Profile
			if _, exists := capsByProfile[decl.Profile]; !exists {
				caps, desc := cfg.ResolvedWorkerDeclaration(decl.Profile)
				capsByProfile[decl.Profile] = &scheduler.AgentCapabilityInfo{
					Capabilities: caps,
					Description:  desc,
				}
			}
		}
		sched.SchedulerExec.WorkerProfiles = workerProfiles
		sched.SchedulerExec.WorkerCapabilitiesByProfile = capsByProfile

		for _, decl := range cfg.Workers {
			allowedTools, err := cfg.ResolveProfile(decl.Profile)
			if err != nil {
				return nil, fmt.Errorf("worker %q profile 解析失败: %w", decl.ID, err)
			}
			if allowedTools != nil {
				if err := tools.ValidateToolNames(allowedTools); err != nil {
					return nil, fmt.Errorf("worker %q profile %q 工具名校验失败: %w", decl.ID, decl.Profile, err)
				}
			}
			workerLLM := llm.NewSDKClient(
				cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
				"", // system prompt 由 worker 内部管理
				cfg.LLMProvider,
				time.Duration(cfg.LLMTimeoutSec)*time.Second,
			)
			wk := worker.NewWithID(decl.ID, taskStore, r, workerLLM, cfg, cancelRegistry, mbRegistry, approvalCh, hookReg, storeView, recordToolCall, agentHookReg, agentStoreView, agentRosterView, allowedTools)
			workers = append(workers, wk)
			profileLabel := decl.Profile
			if profileLabel == "" {
				profileLabel = "全量工具"
			}
			fmt.Printf("[启动] Worker %s 已启动 [profile=%s]\n", decl.ID, profileLabel)
		}
	} else {
		// 旧路径：worker_count + worker_profile
		workerCount := cfg.WorkerCount
		if workerCount <= 0 {
			workerCount = 1
		}
		for i := 1; i <= workerCount; i++ {
			workerLLM := llm.NewSDKClient(
				cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
				"", // system prompt 由 worker 内部管理
				cfg.LLMProvider,
				time.Duration(cfg.LLMTimeoutSec)*time.Second,
			)
			wk := worker.NewWithID(fmt.Sprintf("worker-%d", i), taskStore, r, workerLLM, cfg, cancelRegistry, mbRegistry, approvalCh, hookReg, storeView, recordToolCall, agentHookReg, agentStoreView, agentRosterView, workerAllowed)
			workers = append(workers, wk)
		}
	}

	// Step 9: 创建邮差通知器
	notifierInterval := time.Duration(cfg.MailNotifierIntervalSec) * time.Second
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
		Explorer:        exp,
		Workers:         workers,
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

	// Step 6.5: 启动邮差通知器（默认禁用，防止邮件级联爆炸 — 见 KNOWN_ISSUES.md）
	if s.Config.MailNotifierEnabled {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.MailNotifier.Run(ctx)
		}()
		fmt.Println("[启动] 邮差通知器已启动")
	} else {
		fmt.Println("[启动] 邮差通知器已禁用 (mail_notifier_enabled=false) — 邮件不会自动唤醒空闲代理")
	}

	// Step 7: 启动调查代理
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Explorer.Run(ctx)
	}()
	if s.Config.ExplorerProfile != "" {
		fmt.Printf("[启动] 调查代理已启动 [profile=%s]\n", s.Config.ExplorerProfile)
	} else {
		fmt.Println("[启动] 调查代理已启动")
	}

	// Step 8: 启动执行代理
	for _, wk := range s.Workers {
		wk := wk // 闭包捕获
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			wk.Run(ctx)
		}()
	}
	if s.Config.WorkerProfile != "" {
		fmt.Printf("[启动] 执行代理已启动 (%d 个) [profile=%s]\n", len(s.Workers), s.Config.WorkerProfile)
	} else {
		fmt.Printf("[启动] 执行代理已启动 (%d 个)\n", len(s.Workers))
	}

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
