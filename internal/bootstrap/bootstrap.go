package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/gate"
	"agentgo/internal/hook"
	"agentgo/internal/hook/builtin"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/probe"
	"agentgo/internal/reactor"
	reactorbuiltin "agentgo/internal/reactor/builtin"
	"agentgo/internal/reactor/userdef"
	"agentgo/internal/roster"
	"agentgo/internal/runner"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
	"agentgo/internal/shell"
	"agentgo/internal/spawn"
	"agentgo/internal/store"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/tui"
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
	Runners      []*runner.Runner
	ApprovalCh   chan shell.ApprovalRequest // 命令审批通道，Worker→TUI
	ArtifactLog  *store.ArtifactLog         // Artifacts 持久化日志，Shutdown 时需 Close；nil 表示持久化已禁用
	SessionMgr   *session.SessionManager    // Session 管理器，nil 表示无 Session 模式
	SpawnManager *spawn.Manager             // v5 Phase 5 S5+S6：ad-hoc agent 生命周期管理器
	StatusCh     chan string                // TUI 日志/进度消息通道；Bootstrap 创建，RunCLI 消费
	OutputCh     chan string                // TUI Agent 用户可见输出通道（result 卡片），与日志分离
	LogFile      *os.File                   // system.log 句柄，Bootstrap 打开，Shutdown 关闭
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func Bootstrap(configPath string, explicit bool, skipStartupProbe bool) (*System, error) {
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
	//             --skip-startup-probe 命令行旗标可整体跳过（等价于 startup_probe: off）。
	if !skipStartupProbe {
		if probeErr := startupProbe(os.Stdout, cfg); probeErr != nil {
			if cfg.StartupProbeFailureAction == "exit" {
				return nil, fmt.Errorf("启动期 probe 失败（startup_probe_failure_action=exit）: %w", probeErr)
			}
			log.Printf("[WARN] startup probe: %v (best-effort, 启动继续)", probeErr)
		}
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

	// 初始化 system.log，启动阶段诊断日志全部收敛到文件
	logFilePath := filepath.Join(cfg.ProjectRoot, ".agentgo", "system.log")
	if sessMgr != nil && sessMgr.Current() != nil {
		logFilePath = filepath.Join(sessMgr.LogDir(), "system.log")
	}
	logFile, logErr := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logErr == nil {
		log.SetOutput(logFile)
	} else {
		log.Printf("[bootstrap] 无法创建 system.log: %v", logErr)
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
		log.Printf("[启动] Trace 系统已启动 (dir=%s, 保留最近 100 个任务)", traceDir)
	}

	// Step 1.6: 初始化 prompt dumper（仅在 AGENTGO_DUMP_PROMPTS=1 时启用）
	dumpEnabled := os.Getenv("AGENTGO_DUMP_PROMPTS") == "1"
	dumper, dumperErr := trace.NewPromptDumper(traceDir, dumpEnabled)
	if dumperErr != nil {
		fmt.Printf("[启动] WARNING: prompt dumper 初始化失败: %v\n", dumperErr)
	} else if dumpEnabled {
		trace.SetDefaultDumper(dumper)
		log.Println("[启动] Prompt dump 已启用 (AGENTGO_DUMP_PROMPTS=1)")
	}

	// Step 2: 初始化公告板
	eventCh := make(chan model.Event, cfg.Infra.Store.EventChannelBuffer)
	taskStore := store.NewMemoryTaskStore(eventCh, cfg.Infra.Store.FIFOLimit, cfg.Infra.Store.DefaultConcurrency, cfg.Infra.Store.DefaultTimeoutSec)
	cancelRegistry := store.NewTaskCancelRegistry()
	taskStore.SetCancelRegistry(cancelRegistry)
	log.Println("[启动] 公告板初始化完成")

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
			log.Printf("[启动] Artifact 持久化已启用 (log=%s，恢复 %d 个任务 / %d 个 artifact)",
				artifactLog.Path(), restoredTasks, restoredArts)
		} else {
			log.Printf("[启动] Artifact 持久化已启用 (log=%s，日志中 %d 行记录，当前 store 中无匹配任务可恢复)",
				artifactLog.Path(), len(rebuilt))
		}
	}

	// Step 2.5: 初始化统一 Gate 系统（v5 Phase 1，ReactiveSystem.md §4.4）
	//
	// v4 时代分立的 ToolHookRegistry / MailboxHookRegistry 在 v5 合并为单一
	// gate.Registry。impl 仍保留在 internal/hook/builtin/，注册时通过
	// gate.WrapToolHook / WrapMailboxHook 包装为 gate.Gate。
	gateReg := gate.NewRegistry()
	var storeView store.StoreHookView = taskStore
	recordToolCall := func(taskID string, rec store.ToolCallRecord) {
		_ = taskStore.AppendToolCall(taskID, rec)
	}
	// 注册 6 个 Tool 域 Gate（impl 仍是 hook.ToolHook 接口，通过 adapter 包装）。
	// 注：v5 Phase 4 起 record-artifact 已迁移为 Reactor（订阅 KindFileWritten），
	// 不再走 Tool PostCall hook 路径——避免 hook 与 reactor 双写导致 task.Artifacts 重复。
	for _, h := range []hook.ToolHook{
		builtin.NewPathBoundaryHook(cfg.ProjectRoot),
		builtin.NewValidateExpectedHashHook(),
		builtin.NewRequireReadBeforeWriteHook(storeView),
		builtin.NewDependencyValidatorHook(storeView),
		builtin.NewEnforceExpectedArtifactsHook(storeView, cfg.ProjectRoot),
		builtin.NewValidateLineAnchorsHook(),
	} {
		if err := gateReg.Register(gate.WrapToolHook(h)); err != nil {
			return nil, fmt.Errorf("注册 %s 失败: %w", h.Name(), err)
		}
	}
	log.Println("[启动] Tool 域 Gate 注册完成（path-boundary, validate-expected-hash, validate-line-anchors, require-read-before-write, dependency-validator, enforce-expected-artifacts）")

	// Step 3: 初始化花名册
	r := roster.NewMemoryRoster()
	log.Println("[启动] 花名册初始化完成")

	// Step 3.5: 初始化邮箱注册表（v4：缓冲区大小是系统级常量，不暴露 yaml）
	mbRegistry := mailbox.NewRegistry(mailbox.DefaultInboxSize)
	log.Println("[启动] 邮箱注册表初始化完成")

	// Step 3.5.1: 将 Session 的 HistoryEmitter 注入 store/roster/mailbox，
	// 否则 history.jsonl 永远不会被写入（v3 §9.9 阶段三装配补齐）。
	if sessMgr != nil && sessMgr.History() != nil {
		taskStore.SetHistoryEmitter(sessMgr.History())
		r.SetHistoryEmitter(sessMgr.History())
		mbRegistry.SetHistoryEmitter(sessMgr.History())
		log.Println("[启动] Session history.jsonl 事件发射器已注入（store/roster/mailbox）")
	}

	// Step 3.6: 注册 Mailbox 域 Gate（v5 Phase 1 与 Tool 域 Gate 共用 gateReg）
	// AttachHookRunner 走 gate.AsMailboxRunner 适配器，把 gateReg 反向注入 mbRegistry。
	for _, h := range []hook.MailboxHook{
		builtin.NewChainDepthLimitHook(mailbox.DefaultChainMaxDepth),
		builtin.NewPerAgentDedupHook(storeView),
		builtin.NewWakeWorthyFilterHook(mbRegistry, mbRegistry),
		builtin.NewWakeContextExpandHook(mbRegistry, 5),
	} {
		if err := gateReg.Register(gate.WrapMailboxHook(h)); err != nil {
			return nil, fmt.Errorf("注册 %s 失败: %w", h.Name(), err)
		}
	}
	mbRegistry.AttachHookRunner(gate.AsMailboxRunner(gateReg))
	log.Printf("[启动] Mailbox 域 Gate 注册完成（chain-depth-limit max=%d, per-agent-dedup, wake-worthy-filter, wake-context-expand)", mailbox.DefaultChainMaxDepth)

	// Step 3.7: 初始化 Agent Hook 系统（v5 Phase 1 后空壳运行）。
	//
	// v4 时代这里注册 TeamAwarenessHook 三 section（TeamSnapshot / FileAwareness /
	// GoalAnchor），v5 已被 Memory System 取代（MemoryManageSystem.md MM6）：
	//   - TeamSnapshot / FileAwareness 由 Agent.injectMemoryContext 直接接管，
	//     write-through 到 ScopeProcess Memory（key = team_snapshot:<id> /
	//     file_awareness:<id>）
	//   - GoalAnchor 直接删除（task.Description 已是目标，注入是冗余）
	//
	// 注：v5 Phase 4 (MM7) 已删除 AgentHook 子系统——0 个 builtin AgentHook 注册者，
	// PhaseTaskStart / PhaseLoopPre / PhaseLoopPost / PhaseTaskEnd 等观察/注入语义
	// 现由 trace.Event + Reactor 系统承接（KindAgentStateChanged / KindLLMCallStart
	// / KindToolResult 等覆盖原 phase 边界）；inject 类需求由 Memory System 承接。

	// Step 3.8: 初始化 Memory System（v5 Phase 1 引入，MemoryManageSystem.md）。
	// ScopeProcess 内存存储；所有 worker / scheduler / explorer agent 共用同一实例，
	// 让 file_awareness 等全局共享条目能被读侧看到统一视图。
	memoryStore := memory.NewProcessStore()
	log.Println("[启动] Memory System 初始化完成（process scope 内存存储）")

	// Step 3.9: 初始化 Reactor 系统（v5 Phase 4，ReactiveSystem.md §6.6）。
	// trace.Emit 派发到本 Registry——Reactor 在状态变化后程序化响应（不可决策）。
	//
	// 内置 Reactor 清单（§5.1.1 + §5.2.1 决议）：
	//   - record-artifact (Async)：从 v4 hook 迁移，订阅 KindFileWritten 写 task.Artifacts
	//   - task-end-callback (Sync)：订阅 task lifecycle 退出事件，执行 callback
	//   - trace-history-event (Async)：订阅历史压缩 / 截断事件，原子计数累加
	//   - read-set-write (Async)：v5 Phase 6 引入，订阅 KindToolResult filter read_file，
	//     写 task.ReadSet 取代 require-read-before-write Gate 反查日志
	reactorReg := reactor.NewRegistry()
	if err := reactorReg.Register(reactorbuiltin.NewRecordArtifactReactor(storeView, cfg.ProjectRoot)); err != nil {
		return nil, fmt.Errorf("注册 RecordArtifactReactor 失败: %w", err)
	}
	taskEndReactor := reactorbuiltin.NewTaskEndCallbackReactor()
	if err := reactorReg.Register(taskEndReactor); err != nil {
		return nil, fmt.Errorf("注册 TaskEndCallbackReactor 失败: %w", err)
	}
	historyEventReactor := reactorbuiltin.NewTraceHistoryEventReactor()
	if err := reactorReg.Register(historyEventReactor); err != nil {
		return nil, fmt.Errorf("注册 TraceHistoryEventReactor 失败: %w", err)
	}
	if err := reactorReg.Register(reactorbuiltin.NewReadSetWriteReactor(taskStore)); err != nil {
		return nil, fmt.Errorf("注册 ReadSetWriteReactor 失败: %w", err)
	}
	// 注：用户 reactor + spawn.Manager + trace.SetDefaultDispatcher 推迟到
	// RunnerDeps 构造完成后（见 Step 8 末尾），因为 spawn.Manager 需要 RunnerDeps
	// 来构造 ad-hoc runner。在那之前 dispatcher 未设，无 reactor 触发，安全。
	// taskEndReactor 通过 RunnerDeps.TaskEndCallbacks 注入到每个 runner.New，
	// 用于注册"清空 holder"等任务结束副作用——v5 Phase 4 完成迁移。
	_ = historyEventReactor // 计数器在 monitor / debug 路径按需读取

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
		role := kind.Description
		if role == "" {
			role = fmt.Sprintf("kind=%s（监听 event_type=%q）", kind.Kind, kind.EventType)
		}
		agentRegistry.Register(scheduler.SpecializedAgent{
			EventType:    kind.EventType,
			Count:        kind.Replicas,
			Role:         role,
			Capabilities: caps,
		})
	}
	for _, sa := range agentRegistry.Specialized() {
		desc := sa.Role
		if len(desc) > 80 {
			desc = desc[:80] + "…"
		}
		log.Printf("[启动] 特化代理已注册: EventType=%s, description=%s", sa.EventType, desc)
	}
	log.Printf("[启动] Agent 注册表初始化完成（%d 个特化代理）", len(agentRegistry.Specialized()))

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
		log.Printf("[启动] web_search: %s，已回落到 %s（工具仍可用，但能力可能降级）",
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
		log.Println("[启动] 工具可用性探测完成（全部可用）")
	} else {
		for _, r := range toolHealth.Results() {
			if !r.Available {
				fmt.Printf("[警告] 工具 %s 不可用: %s，相关代理将降级运行\n", r.Tool, r.Error)
			}
		}
	}

	// Step 7.5: 创建命令审批通道（Worker→CLI）
	approvalCh := make(chan shell.ApprovalRequest, 8)

	// Step 4.5: 创建 TUI 双通道（日志与 Agent 输出分离，避免竞争）
	statusCh := make(chan string, 1024) // 日志/进度消息
	outputCh := make(chan string, 256)  // Agent 用户可见输出（result 卡片）

	// Step 5: 创建调度器（Phase 3：scheduler 是 agent.Agent 实例 + Activator + ModeStore）
	// 工具集 = Worker 全集 + SchedulerGroup，可以读文件 / 搜索 / 查网页 / 跑 shell。
	// EventCh 由 Activator 监听，转换为 EventType="__scheduler__" 的 task。
	sched := scheduler.New(
		taskStore, r, schedulerLLM, eventCh, cfg, cancelRegistry, mbRegistry, approvalCh,
		gateReg, storeView, recordToolCall, agentRegistry, memoryStore,
		&chanWriter{ch: outputCh},
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
		GateReg:               gateReg,
		StoreView:             storeView,
		RecordToolCall:        recordToolCall,
		Memory:                memoryStore,
		MBRegistry:            mbRegistry,
		CancelRegistry:        cancelRegistry,
		SearchProvider:        searchProvider,
		ShellFilter:           shellFilter,
		ApprovalCh:            approvalCh,
		UserOutput:            &chanWriter{ch: outputCh},
		TaskEndCallbacks:      taskEndReactor,
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
			log.Printf("[启动] Runner %s 已启动 [kind=%s, model=%s]",
				rt.InstanceID, kind.Kind, rt.Model)
		}
	}

	// Step 8.5: spawn.Manager 构造 + 注册（v5 Phase 5 S5+S6）
	//
	// Manager 同时是 reactor.Reactor（订阅 task 终态触发 one_shot 销毁）。
	// RunnerDeps 此时已就绪，所以 ad-hoc runner 构造能复用与静态 kind 完全相同的 deps。
	llmFactoryForSpawn := func(model string) llm.Client {
		return buildKindLLMClient(cfg.LLM, model)
	}
	spawnMgr := spawn.NewManager(cfg, deps, llmFactoryForSpawn, taskStore)
	if err := reactorReg.Register(spawnMgr); err != nil {
		return nil, fmt.Errorf("注册 spawn.Manager 失败: %w", err)
	}

	// Step 8.6: 用户 YAML reactor（v5 Phase 5 S1-S6）
	//
	// invoke_llm 用独立的 LLM client（systemPrompt="" 不注入，原则 5）。
	// spawn_agent 走上面构造的 spawn.Manager。
	if cfg.ReactorsFile != "" {
		kindEventTypes := make(map[string]string, len(cfg.Agents))
		for _, kind := range cfg.Agents {
			kindEventTypes[kind.Kind] = kind.EventType
		}
		// 静态 agent InstanceID → kind 映射（与 buildAgentRuntime 的 InstanceID 格式一致：<kind>-<replicaIndex>）
		staticKindOf := make(map[string]string, len(cfg.Agents))
		for _, kind := range cfg.Agents {
			for i := 1; i <= kind.Replicas; i++ {
				staticKindOf[fmt.Sprintf("%s-%d", kind.Kind, i)] = kind.Kind
			}
		}
		// 合并查找：静态优先，未命中再查 spawn.Manager（ad-hoc agent，§6.2.4 继承 base_kind）
		agentKindOf := func(agentID string) string {
			if k, ok := staticKindOf[agentID]; ok {
				return k
			}
			return spawnMgr.KindOf(agentID)
		}
		userReactorDeps := userdef.Deps{
			Store: taskStore,
			LLMFactory: func(model string) userdef.LLMCompleter {
				// 独立 reactor LLM client：不复用主 agent client，避免共享 history / system prompt 状态。
				return userdef.NewLLMCompleter(buildKindLLMClient(cfg.LLM, model))
			},
			Mailbox:        mbRegistry,
			KindEventTypes: kindEventTypes,
			SpawnHost:      spawnMgr,
			AgentKindOf:    agentKindOf,
		}
		userReactors, err := userdef.LoadFromFile(cfg.ReactorsFile, cfg.ProjectRoot, userReactorDeps)
		if err != nil {
			return nil, fmt.Errorf("加载 reactors_file %q 失败: %w", cfg.ReactorsFile, err)
		}
		for _, r := range userReactors {
			if err := reactorReg.Register(r); err != nil {
				return nil, fmt.Errorf("注册用户 Reactor %q 失败: %w", r.Name(), err)
			}
		}
		log.Printf("[启动] 用户 Reactor 已加载（%d 个，来自 %s）", len(userReactors), cfg.ReactorsFile)
	}

	trace.SetDefaultDispatcher(reactorReg)
	log.Println("[启动] Reactor 系统初始化完成（record-artifact, task-end-callback, trace-history-event, read-set-write, spawn-manager）")

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
		SpawnManager:    spawnMgr,
		StatusCh:        statusCh,
		OutputCh:        outputCh,
		LogFile:         logFile,
	}

	return sys, nil
}

// Start 启动所有后台 goroutine。cancel 用于 CLI /quit 触发全局退出。
func (s *System) Start(ctx context.Context, cancel context.CancelFunc) {
	s.cancel = cancel

	// 把当前 ctx 作为所有 ad-hoc spawn 的父 ctx——Shutdown 通过 cancel() 传播。
	// 必须早于任何 reactor 触发，否则 ad-hoc runner 会用 context.Background 启动，
	// system 关闭时无法停下。
	if s.SpawnManager != nil {
		s.SpawnManager.SetParentContext(ctx)
	}

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
	log.Println("[启动] 调度器已启动 (agent + activator)")

	// Step 6: 启动看门狗
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWatchdogWithRecover(ctx)
	}()
	log.Println("[启动] 看门狗已启动")

	// Step 6.5: 启动邮差通知器（默认开启；可通过 infra.mail_notifier.enabled=false 关闭）
	if s.Config.Infra.MailNotifier.Enabled {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.MailNotifier.Run(ctx)
		}()
		log.Println("[启动] 邮差通知器已启动")
	} else {
		log.Println("[启动] 邮差通知器已禁用 (infra.mail_notifier.enabled=false) — 邮件不会自动唤醒空闲代理")
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
	log.Printf("[启动] kind-based agents 已启动 (%d 个 runner 实例)", len(s.Runners))

	fmt.Println("[启动] 系统就绪，等待用户输入")
}

// tuiLogWriter 把日志同时写入文件，并把每一行非空内容复制到 TUI 的 status channel。
// 这样日志既持久化到文件，用户又能在 TUI 内看到关键进度。
type tuiLogWriter struct {
	file   *os.File
	status chan<- string
	buf    []byte // 缓存不完整的行尾
}

func (w *tuiLogWriter) Write(p []byte) (n int, err error) {
	n, err = w.file.Write(p)
	if err != nil {
		return n, err
	}
	w.buf = append(w.buf, p...)
	lines := strings.Split(string(w.buf), "\n")
	// 最后一段可能不完整，留到下次 Write
	w.buf = []byte(lines[len(lines)-1])
	// 防止 buf 无限增长（极端情况：长时间没有换行）
	if len(w.buf) > 4096 {
		w.buf = w.buf[len(w.buf)-4096:]
	}
	for _, line := range lines[:len(lines)-1] {
		line = strings.TrimSpace(line)
		if line != "" {
			select {
			case w.status <- line:
			default:
			}
		}
	}
	return n, nil
}

// chanWriter 把写入的字节块发送到 channel，供 TUI 接收并渲染。
// 用于 agent/scheduler 的 UserOutput，将用户可见内容注入 Bubble Tea。
type chanWriter struct {
	ch chan<- string
}

func (w *chanWriter) Write(p []byte) (int, error) {
	w.ch <- string(p)
	return len(p), nil
}

// RunCLI 启动 TUI 主循环，阻塞直到用户退出或 ctx 取消。
//
// reader/writer 为兼容旧 main 签名保留；bubbletea 直接接管 stdin/stdout，
// 这两个参数在 v1 不再被读取。
func (s *System) RunCLI(ctx context.Context, reader io.Reader, writer io.Writer) {
	// 将运行时 log 重定向到文件 + TUI 消息区域。
	// Bootstrap 期间 log 已写入同一文件；此处复用句柄，追加 channel 旁路。
	oldLogWriter := log.Writer()
	if s.LogFile != nil {
		log.SetOutput(&tuiLogWriter{file: s.LogFile, status: s.StatusCh})
		defer log.SetOutput(oldLogWriter)
	}

	deps := tui.Deps{
		Store:       s.Store,
		EventCh:     s.EventCh,
		CancelFn:    s.cancel,
		Scheduler:   s.Scheduler,
		Mailbox:     s.MailboxRegistry,
		ApprovalCh:  s.ApprovalCh,
		SessionMgr:  s.SessionMgr,
		SystemMsgCh: s.StatusCh,
		OutputCh:    s.OutputCh,
	}
	if err := tui.Run(ctx, deps); err != nil {
		fmt.Fprintf(os.Stderr, "[TUI] 异常退出: %v\n", err)
	}
}

// Shutdown 优雅关闭所有服务。
func (s *System) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	// SpawnManager.Shutdown 等待所有 ad-hoc runner goroutine 退出（cancel 已通过 ctx 传播）。
	// 必须在 s.wg.Wait 之前调用——s.wg 不持有 ad-hoc runner 的 wg，那些 goroutine 由
	// SpawnManager 内部的 wg 管理。
	if s.SpawnManager != nil {
		s.SpawnManager.Shutdown()
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
	// 关闭 system.log
	if s.LogFile != nil {
		if err := s.LogFile.Close(); err != nil {
			fmt.Printf("[关闭] WARNING: system.log 关闭失败: %v\n", err)
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
