package bootstrap

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/dashboard"
	"agentgo/internal/gate"
	"agentgo/internal/hook"
	"agentgo/internal/hook/builtin"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/output"
	"agentgo/internal/plan"
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
	"agentgo/internal/team"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/tui"
	"agentgo/internal/ui"
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
	Scheduler       *scheduler.Bundle // Phase 3：scheduler 现在是 agent.Agent + Activator + 三轴 modes.Store 的复合
	PlanStore       *plan.Store
	PlanCoordinator *plan.Coordinator
	// Interactions is the authoritative structured human-response service shared
	// by Plan/Scheduler, Shell and every UI frontend.
	Interactions *interaction.Service
	// PlanBatcher 是 C1 的 Task→Plan 变更异步落盘器（单 flusher 合并 fsync）。
	// Shutdown 必须先 Drain（2s 界）再 Stop，不泄漏 goroutine。
	PlanBatcher *planMutationBatcher
	Activity    *agent.ActivityTracker
	// v4：所有执行/调查代理都是 runner.Runner（取代旧 worker.Worker / explorer.Explorer
	// 两个 package；详见 nextUpgrade_v4.md §11.6.6）。kind × replicas 实例化在 Bootstrap()
	// 主流程展开。
	Runners     []*runner.Runner
	ArtifactLog *store.ArtifactLog // Artifacts 持久化日志，Shutdown 时需 Close；nil 表示持久化已禁用
	// artifactReplay 是启动时从 ArtifactLog 重放出的 taskID→artifacts 映射（F12）。
	// store 构造时还没有任何任务，立即恢复会全部 miss；由 restoreRuntimeSnapshot
	// 在 Task 快照导入后消费。RestoreArtifacts 是覆盖式恢复（rebuilt 为完整去重
	// 列表），与 TaskSnapshot 内嵌的 Artifacts 不产生重复追加。nil 表示无日志。
	artifactReplay  map[string][]string
	SessionMgr      *session.SessionManager // Session 管理器，nil 表示无 Session 模式
	SpawnManager    *spawn.Manager          // v5 Phase 5 S5+S6：ad-hoc agent 生命周期管理器
	ReactorRegistry *reactor.Registry       // E4：Shutdown 时 Quiesce（取消在途 async reactor 并带界排空）
	AgentTemplates  *agenttemplate.Catalog  // immutable builtin/user/project template catalog
	TeamManager     *team.Manager           // Plan-scoped template Team lifecycle and recovery
	TeamStore       *team.Store             // fsynced TeamSpec identity store
	StatusCh        chan string             // TUI 日志/进度消息通道；Bootstrap 创建，UI Hub 消费
	OutputCh        chan output.Event       // Agent 用户可见输出通道（文本/结果已分类），与日志分离；UI Hub 消费
	// UIHub 是输出、状态与 Interaction 投影的统一消费者与
	// 前端控制/观测面（ui.Controller + ui.Observer）。TUI（RunCLI）只经它
	// 与系统交互；无订阅者时 Hub 也常驻排干通道，生产者永不阻塞。
	UIHub *ui.Hub
	// Dashboard 是经 UI Hub 接入的 Web Dashboard（ui.frontends 含 "web" 时在 Start 启动；
	// 否则为 nil）。
	Dashboard *dashboard.Server
	LogFile   *switchableLogFile // system.log 句柄持有器（支持 session 切换重绑），Bootstrap 打开，Shutdown 关闭
	// releaseInstanceLock 释放项目级单实例锁（E7，.agentgo/agentgo.lock），
	// 幂等；Shutdown 末尾调用。nil 表示未持有锁。
	releaseInstanceLock func()
	resultMu            sync.Mutex
	lastResult          *session.ResultSnapshot
	// resultVersion is a monotonic generation used by Session-switch rollback.
	// It prevents restoring an old result over a newer result that arrived while
	// the switch was being committed or rolled back.
	resultVersion uint64
	// snapshotMu serializes periodic/shutdown saves with Session switches so a
	// snapshot exported for the old Session can never be committed to the new one.
	snapshotMu sync.Mutex
	// sessionSwitchErr carries critical Plan/Team persistence rebind failures
	// from SessionManager's synchronous OnSwitch callback back to NewSession /
	// SwitchSession, which can then roll the committed manager switch back.
	sessionSwitchErrMu sync.Mutex
	sessionSwitchErr   error
	cancel             context.CancelFunc
	// startCtx 是 Start 传入的根 ctx（Hub 的 CancelTaskByPrefix 需要与系统
	// 生命周期绑定的 ctx，而 Hub 在 Bootstrap 装配时 Start 尚未运行）。
	startCtx context.Context
	wg       sync.WaitGroup
	// outputDone 在 Shutdown 开始时关闭，解除 eventWriter 对 OutputCh 的阻塞写；
	// outputDoneOnce 保证重复 Shutdown 不会因重复 close 而 panic。
	outputDone     chan struct{}
	outputDoneOnce sync.Once
	// 完整 Shutdown 只允许一个执行者；并发/重复调用等待同一结果，避免资源
	// Close、最终快照和 Team mailbox finalize 交错执行。
	shutdownInitOnce sync.Once
	shutdownOnce     sync.Once
	shutdownDone     chan struct{}
	shutdownErr      error
}

type BootstrapOptions struct {
	SkipStartupProbe bool
	ResumeSessionID  string
}

func resolveAgentTemplateDirs(cfg *config.Config) ([]string, []string, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("config is nil")
	}
	projectRoot := cfg.ProjectRoot
	if projectRoot == "" {
		projectRoot = "."
	}
	projectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, nil, err
	}
	resolve := func(raw, base string, expandHome bool) (string, error) {
		path := strings.TrimSpace(raw)
		if path == "" {
			return "", fmt.Errorf("template directory is empty")
		}
		if expandHome && (path == "~" || strings.HasPrefix(path, "~/")) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, filepath.FromSlash(path))
		}
		return filepath.Clean(path), nil
	}
	userDirs := make([]string, 0, len(cfg.AgentTemplates.UserDirs))
	for _, raw := range cfg.AgentTemplates.UserDirs {
		dir, err := resolve(raw, ".", true)
		if err != nil {
			return nil, nil, err
		}
		userDirs = append(userDirs, dir)
	}
	projectDirs := make([]string, 0, len(cfg.AgentTemplates.ProjectDirs))
	for _, raw := range cfg.AgentTemplates.ProjectDirs {
		dir, err := resolve(raw, projectRoot, false)
		if err != nil {
			return nil, nil, err
		}
		projectDirs = append(projectDirs, dir)
	}
	return userDirs, projectDirs, nil
}

func Bootstrap(configPath string, explicit bool, skipStartupProbe bool) (*System, error) {
	return BootstrapWithOptions(configPath, explicit, BootstrapOptions{SkipStartupProbe: skipStartupProbe})
}

func BootstrapWithOptions(configPath string, explicit bool, opts BootstrapOptions) (*System, error) {
	// Step 1: 加载配置
	cfg, err := config.LoadConfig(configPath, explicit)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("[启动] 全局配置加载完成")

	// Step 1.05: v4 配置校验。Validate 无条件执行；cfg.Agents 为空是合法的
	//             Scheduler-only 模式（v3/LLM-only 配置），此时静态 kind 规则
	//             自然不触发，仅 scheduler 模型等全局规则生效。
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("v4 配置校验失败: %w", err)
	}

	// Step 1.06: 项目级单实例锁（E7）——两个 agentgo 进程共用同一 .agentgo
	//             目录会互踩共享状态（session 固定 .tmp 原子写名冲突、trace GC
	//             互相修剪），必须在打开任何日志/存储之前抢锁。之后所有启动
	//             失败路径经 defer 释放；成功路径由 System.Shutdown 释放
	//             （进程崩溃遗留由下次启动的陈旧锁接管逻辑兜底）。
	agentgoDir := filepath.Join(cfg.ProjectRoot, ".agentgo")
	releaseLock, lockErr := acquireInstanceLock(agentgoDir)
	if lockErr != nil {
		return nil, lockErr
	}
	bootstrapCompleted := false
	defer func() {
		if !bootstrapCompleted {
			releaseLock()
		}
	}()

	userTemplateDirs, projectTemplateDirs, err := resolveAgentTemplateDirs(cfg)
	if err != nil {
		return nil, fmt.Errorf("解析 AgentTemplate 目录失败: %w", err)
	}
	templateDefaultModel := cfg.LLM.DefaultModel
	if templateDefaultModel == "" {
		templateDefaultModel = cfg.Scheduler.Model
	}
	templateCatalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		UserDirs: userTemplateDirs, ProjectDirs: projectTemplateDirs,
		DefaultModel: templateDefaultModel, ValidateTools: tools.ValidateToolNames,
	})
	if err != nil {
		return nil, fmt.Errorf("加载 AgentTemplate catalog 失败: %w", err)
	}
	log.Printf("[启动] AgentTemplate catalog 已加载 (%d 个模板)", len(templateCatalog.List()))

	// Step 1.1: 启动期 banner（§9.5.1）——打印逐 kind 摘要 + 脱敏 api_key，
	//             让用户视觉核对 YAML 是否被正确读取。
	//             configPath 单独打印，避免"测 v4 但启动了 v3 默认"之类的混淆。
	printStartupBanner(os.Stdout, configPath, cfg)

	// Step 1.2: 启动期 TCP probe（§9.5）——best-effort 连通性检查
	//             失败行为：默认 warning + 启动继续；startup_probe_failure_action="exit" 改为硬退出
	//             --skip-startup-probe 命令行旗标可整体跳过（等价于 startup_probe: off）。
	if !opts.SkipStartupProbe {
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
	sessMgr, sessErr := session.NewSessionManagerWithResume(sessDir, sessionCfg, opts.ResumeSessionID)
	if sessErr != nil {
		if opts.ResumeSessionID != "" {
			return nil, fmt.Errorf("resume session %q 失败: %w", opts.ResumeSessionID, sessErr)
		}
		fmt.Printf("[启动] WARNING: Session 初始化失败: %v —— 以无 Session 模式运行\n", sessErr)
	}
	// 开启 history.jsonl 事件溯源（默认关闭，由 bootstrap 显式启用）
	if sessMgr != nil && sessMgr.Current() != nil {
		sessMgr.EnableHistoryLog()
	}
	recoveredSnap := currentRecoveredSnapshot(sessMgr)

	// Step 1.4: Session 保留策略——把超期的已关闭 Session 移入 archive/ 并按
	// SessionArchiveMax 封顶清理。只在启动时跑一次：此刻历史 Session 无任何
	// 句柄占用，Windows 上 rename 安全；当前活跃 Session 由 RunArchive 内部
	// 显式跳过（其 history/log 句柄已打开）。Shutdown 不跑（句柄未收）。
	// 失败仅告警，不阻塞启动。
	if sessMgr != nil {
		if err := sessMgr.RunArchive(); err != nil {
			log.Printf("[启动] WARNING: Session 归档失败: %v（启动继续）", err)
		}
	}

	// 初始化 system.log，启动阶段诊断日志全部收敛到文件。
	// 句柄包在 switchableLogFile 里：session 切换（OnSwitch 钩子）会换绑到新
	// Session 的 system.log，log.SetOutput 的接线全程不动（B7）。
	logFilePath := filepath.Join(cfg.ProjectRoot, ".agentgo", "system.log")
	if sessMgr != nil && sessMgr.Current() != nil {
		logFilePath = filepath.Join(sessMgr.LogDir(), "system.log")
	}
	var logFileHolder *switchableLogFile
	logFile, logErr := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logErr == nil {
		logFileHolder = &switchableLogFile{file: logFile}
		log.SetOutput(logFileHolder)
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

	// Step 2.1: 初始化动态 DAG 的可靠控制面。Plan 状态与 pending
	// ReplanRequest 使用原子 JSON 存储；内存 signal 仅作唤醒加速。
	planStatePath := filepath.Join(cfg.ProjectRoot, ".agentgo", "state", "plans.json")
	if sessMgr != nil && sessMgr.Current() != nil {
		planStatePath = filepath.Join(sessMgr.Current().Dir, "plan-state.json")
	}
	planStore, planErr := plan.OpenStore(planStatePath)
	if planErr != nil {
		return nil, fmt.Errorf("初始化 PlanStore 失败: %w", planErr)
	}
	planCoordinator := plan.NewCoordinator(planStore, planTaskBackend{store: taskStore})
	planCoordinator.SetAcceptanceVerifier(planAcceptanceVerifier{store: taskStore, projectRoot: cfg.ProjectRoot})
	// C1：Task→Plan 变更落盘改为异步批处理——Mutated hook 仅入队（agent
	// goroutine 上不再做全量 fsync），单个 flusher 合并落盘。Plan 事实最终
	// 一致（毫秒级滞后），Shutdown 经 PlanBatcher.Drain 兜底。
	planBatcher := newPlanMutationBatcher(planCoordinator)
	planHooks := makeTaskPlanHooks(planCoordinator)
	planHooks.Mutated = planBatcher.submit
	taskStore.SetTaskPlanHooks(planHooks)
	log.Printf("[启动] PlanCoordinator 初始化完成 (state=%s)", planStatePath)

	teamStatePath := filepath.Join(cfg.ProjectRoot, ".agentgo", "state", "agent-teams.json")
	if sessMgr != nil && sessMgr.Current() != nil {
		teamStatePath = filepath.Join(sessMgr.Current().Dir, "agent-teams.json")
	}
	teamStore, teamErr := team.OpenStore(teamStatePath)
	if teamErr != nil {
		return nil, fmt.Errorf("初始化 Agent TeamStore 失败: %w", teamErr)
	}
	log.Printf("[启动] Agent TeamStore 初始化完成 (state=%s)", teamStatePath)

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
	// artifactReplay 持有日志重放结果（taskID → 去重后的完整 artifact 列表），
	// 等 Task 快照导入后再恢复——此刻 store 为空，立即 RestoreArtifacts 只会
	// 全部 miss（F12）。nil 表示无日志或重放失败。
	var artifactReplay map[string][]string
	if alErr != nil {
		fmt.Printf("[启动] WARNING: artifact log 初始化失败 (dir=%s): %v —— Artifacts 持久化已禁用\n", artifactLogDir, alErr)
	} else {
		// 先 replay 重建 map；真正的 RestoreArtifacts 在 restoreRuntimeSnapshot
		// 导入任务后执行（restoreRuntimeSnapshot 里 F12 的第二次调用）。
		rebuilt, repErr := artifactLog.Replay()
		if repErr != nil {
			fmt.Printf("[启动] WARNING: artifact log 重放失败: %v —— 以空状态启动\n", repErr)
		} else {
			artifactReplay = rebuilt
		}
		taskStore.SetArtifactLog(artifactLog)
		log.Printf("[启动] Artifact 持久化已启用 (log=%s，日志中 %d 个任务有记录，待任务导入后恢复)",
			artifactLog.Path(), len(artifactReplay))
	}

	// Step 2.5: 初始化统一 Gate 系统（v5 Phase 1，ReactiveSystem.md §4.4）
	//
	// v4 时代分立的 ToolHookRegistry / MailboxHookRegistry 在 v5 合并为单一
	// gate.Registry。impl 仍保留在 internal/hook/builtin/，注册时通过
	// gate.WrapToolHook / WrapMailboxHook 包装为 gate.Gate。
	//
	// 三轴模式 store 提前到此创建：exec-mode-guard Gate 需要注入它做 readonly
	// 判定；同一实例稍后注入 scheduler（快照消费）与 UI Hub（三轴运行时切换）。
	modeStore := modes.NewStore(cfg.ResolveModes())
	gateReg := gate.NewRegistry()
	var storeView store.StoreHookView = taskStore
	recordToolCall := func(taskID string, rec store.ToolCallRecord) {
		_ = taskStore.AppendToolCall(taskID, rec)
	}
	// 注册 7 个 Tool 域 Gate（impl 仍是 hook.ToolHook 接口，通过 adapter 包装）。
	// 注：v5 Phase 4 起 record-artifact 已迁移为 Reactor（订阅 KindFileWritten），
	// 不再走 Tool PostCall hook 路径——避免 hook 与 reactor 双写导致 task.Artifacts 重复。
	for _, h := range []hook.ToolHook{
		builtin.NewExecModeGuardHook(modeStore),
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
	log.Println("[启动] Tool 域 Gate 注册完成（exec-mode-guard, path-boundary, validate-expected-hash, validate-line-anchors, require-read-before-write, dependency-validator, enforce-expected-artifacts）")

	// Step 3: 初始化花名册
	r := roster.NewMemoryRoster()
	log.Println("[启动] 花名册初始化完成")

	// Step 3.5: 初始化邮箱注册表（v4：缓冲区大小是系统级常量，不暴露 yaml）
	mbRegistry := mailbox.NewRegistry(mailbox.DefaultInboxSize)
	log.Println("[启动] 邮箱注册表初始化完成")

	// Step 3.5.1: 将 Session 的 HistoryEmitter 注入 store/roster/mailbox，
	// 否则 history.jsonl 永远不会被写入（v3 §9.9 阶段三装配补齐）。
	//
	// 注入 SwitchingEmitter 间接层而非 sessMgr.History() 裸句柄：Session 切换
	// （/new、/session）会关闭旧 HistoryLog，裸句柄此后 Append 全部返回
	// ErrHistoryLogClosed，新 Session 的 history.jsonl 收不到任何事件。
	// 间接层每次 emit 现取当前句柄，切换后事件自动落到新 Session。
	if sessMgr != nil {
		histEmitter := session.NewSwitchingEmitter(sessMgr)
		taskStore.SetHistoryEmitter(histEmitter)
		r.SetHistoryEmitter(histEmitter)
		mbRegistry.SetHistoryEmitter(histEmitter)
		log.Println("[启动] Session history.jsonl 事件发射器已注入（store/roster/mailbox，经 SwitchingEmitter 间接层）")
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
	//   - runtime-anomaly (Async)：订阅 KindTaskCompleted，用 Store 运行时数据复刻
	//     trace CLI detectAnomalies 最高信号启发式（凭空写入 / 工具错误率>30%），
	//     命中即经 planCoordinator 请求重规划（无 Plan 降级为 trace 告警）
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
	if err := reactorReg.Register(reactorbuiltin.NewAnomalyReactor(storeView, planCoordinator)); err != nil {
		return nil, fmt.Errorf("注册 AnomalyReactor 失败: %w", err)
	}
	// 注：用户 reactor + spawn.Manager + trace.SetDefaultDispatcher 推迟到
	// RunnerDeps 构造完成后（见 Step 8 末尾），因为 spawn.Manager 需要 RunnerDeps
	// 来构造 ad-hoc runner。在那之前 dispatcher 未设，无 reactor 触发，安全。
	// taskEndReactor 通过 RunnerDeps.TaskEndCallbacks 注入到每个 runner.New，
	// 用于注册"清空 holder"等任务结束副作用——v5 Phase 4 完成迁移。
	_ = historyEventReactor // 计数器在 monitor / debug 路径按需读取

	// Step 4: 创建 scheduler LLM 客户端
	// scheduler model 优先用 cfg.Scheduler.Model；循环与历史预算稍后由
	// internal/scheduler.New 从同一 cfg.Scheduler 读取。
	// 缺省回落 cfg.LLM.DefaultModel。LLM endpoint / api_key / provider 与 worker 共享。
	schedulerLLM := buildKindLLMClient(cfg.LLM, cfg.Scheduler.Model)

	// Step 5.5: 构造特化代理注册表（Sprint 3 #7 Scheduler 分配感知）
	// v4：扫描 cfg.Agents，把每个静态 kind 注册为 ready route；EventType != ""
	// 的 kind 同时出现在 Scheduler 的特化代理视图中。默认队列也必须登记，
	// 否则 Scheduler-only 模式无法区分“空字符串 route”与“没有 route”。
	// 这取代了 v3 时代基于 cfg.AgentDeclarations + cfg.ExplorerEventType 的硬编码逻辑——
	// 现在用户可以声明任意命名的特化 kind（不止 explorer），event_type 字段就是分派键。
	agentRegistry := scheduler.NewAgentRegistry()
	for _, kind := range cfg.Agents {
		// D3：profile 解析统一走 config.ResolveProfile（缺失即报错），
		// 不再裸 map 查找静默留 nil。
		caps, err := resolveRouteCapabilities(cfg, kind)
		if err != nil {
			return nil, fmt.Errorf("注册静态 Agent route kind=%q 失败: %w", kind.Kind, err)
		}
		role := kind.Description
		if role == "" {
			role = fmt.Sprintf("kind=%s（监听 event_type=%q）", kind.Kind, kind.EventType)
		}
		if err := agentRegistry.RegisterRoute("static:"+kind.Kind, kind.EventType, "", kind.Replicas, role, caps); err != nil {
			return nil, fmt.Errorf("注册静态 Agent route kind=%q 失败: %w", kind.Kind, err)
		}
	}
	for _, sa := range agentRegistry.Specialized() {
		desc := sa.Role
		if len(desc) > 80 {
			desc = desc[:80] + "…"
		}
		log.Printf("[启动] 特化代理已注册: EventType=%s, description=%s", sa.EventType, desc)
	}
	log.Printf("[启动] Agent 注册表初始化完成（%d 个特化代理）", len(agentRegistry.Specialized()))

	// Step 6: 创建看门狗（先于 scheduler 创建）。
	w := watchdog.New(taskStore, cfg, eventCh, r, mbRegistry, watchdog.NewRuntimeRouteResolver(agentRegistry))

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

	currentSessionID := func() string {
		if sessMgr == nil {
			return ""
		}
		current := sessMgr.Current()
		if current == nil {
			return ""
		}
		return current.Metadata.SessionID
	}
	// Step 7.4: 创建通用人机 Interaction 服务。Plan、Shell 与全部前端
	// 共享同一个 CAS 状态机；UI 只消费安全投影，不持有回复通道。
	interactionService := interaction.NewService(interaction.NewMemoryStore(),
		interaction.WithSessionIDProvider(currentSessionID))

	// Step 4.5: 创建 TUI 双通道（日志与 Agent 输出分离，避免竞争）
	statusCh := make(chan string, 1024)      // 日志/进度消息
	outputCh := make(chan output.Event, 256) // Agent 用户可见输出（文本/结果已分类）
	activity := agent.NewActivityTracker()
	var sys *System
	outputDone := make(chan struct{})
	streamOutput := func(ev output.Event) {
		select {
		case outputCh <- ev:
		case <-outputDone:
		}
	}
	// 文本输出 writer：每个 agent 一个（按 agentID 标记来源），共享同一 outputCh。
	newTextWriter := func(agentID string) *eventWriter {
		return &eventWriter{ch: outputCh, done: outputDone, kind: output.KindText, agentID: agentID}
	}
	// 结果输出 writer：scheduler 的自然文本完成（agent.ResultOutput）与
	// report_done（tools.SchedulerGroup.ResultOutput）共用；onResult 把结果文本
	// 记入 ResultSnapshot（取代旧的魔法字符串分类），只在真正入队后触发。
	resultWriter := &eventWriter{
		ch:      outputCh,
		done:    outputDone,
		kind:    output.KindResult,
		agentID: "scheduler",
		onResult: func(text string) {
			if sys != nil {
				sys.recordResult(text)
			}
		},
	}

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
		PlanCoordinator:       planCoordinator,
		RouteValidator:        agentRegistry,
		Activity:              activity,
		MBRegistry:            mbRegistry,
		CancelRegistry:        cancelRegistry,
		SearchProvider:        searchProvider,
		ShellFilter:           shellFilter,
		Interactions:          interactionService,
		SessionID:             currentSessionID,
		Modes:                 modeStore,         // 与 scheduler / UI Hub 同一实例：exec 轴驱动 strict/yolo
		UserOutput:            newTextWriter(""), // 共享兜底（team/spawn ad-hoc runner）；静态 runner 在下方按实例标记
		StreamOutput:          streamOutput,
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
			rt, rtErr := buildAgentRuntime(kind, cfg.LLM, cfg.ToolProfiles, cfg.Agents, i, cfg.AgentIdleThreshold)
			if rtErr != nil {
				return nil, fmt.Errorf("kind=%q replica=%d 运行时构造失败: %w", kind.Kind, i, rtErr)
			}
			if err := tools.ValidateToolNames(rt.AllowedTools); err != nil {
				return nil, fmt.Errorf("kind=%q replica=%d 工具名校验失败: %w", kind.Kind, i, err)
			}
			kindDeps := deps
			kindDeps.LLMClient = kindLLM
			kindDeps.UserOutput = newTextWriter(rt.InstanceID)
			rn := runner.New(rt, kindDeps)
			runners = append(runners, rn)
			log.Printf("[启动] Runner %s 已启动 [kind=%s, model=%s]",
				rt.InstanceID, kind.Kind, rt.Model)
		}
	}

	// Step 8.2: 构造 Plan-scoped AgentTemplate TeamManager。它与静态
	// cfg.Agents 共用 RunnerDeps，但只在 System.Start 后恢复/启动动态 Team。
	teamLLMFactory := team.LLMFactory(func(model string) llm.Client {
		return buildKindLLMClient(cfg.LLM, model)
	})
	teamMgr := team.NewManager(
		deps, teamLLMFactory, templateCatalog, planCoordinator, teamStore,
		agentRegistry, cfg.AgentTemplates.MaxRuntimeAgents,
	)
	if err := reactorReg.Register(teamMgr); err != nil {
		return nil, fmt.Errorf("注册 AgentTemplate TeamManager 失败: %w", err)
	}

	// Step 5: Scheduler 在 TeamManager 构造后装配，因而模板发现和动态
	// provision 工具拿到的是同一个 runtime authority。
	// 三轴模式 store：初值来自 cfg.Modes（缺省 immediate/normal/team），
	// 已在 Step 2.5 创建（exec-mode-guard Gate 注入）；
	// 同一实例注入 scheduler（快照消费）与 UI Hub（三轴运行时切换）。
	sched := scheduler.New(
		taskStore, r, schedulerLLM, eventCh, cfg, cancelRegistry, mbRegistry, interactionService,
		gateReg, storeView, recordToolCall, agentRegistry, templateCatalog, teamMgr,
		memoryStore, newTextWriter("scheduler"), resultWriter, modeStore, planCoordinator,
	)
	if sched.Agent != nil {
		sched.Agent.Activity = activity
		sched.Agent.StreamOutput = streamOutput
		activity.RegisterAgent(sched.Agent.ID, "scheduler")
	}
	sched.SchedulerExec.ToolHealth = toolHealth

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
			Store:           taskStore,
			ReplanRequester: replanRequesterAdapter{coordinator: planCoordinator, store: taskStore},
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

	// Recovery/reconciliation below may itself emit Plan audit events. Keep the
	// dispatcher detached until all recovered facts are settled so user Reactors
	// cannot turn recovery diagnostics into fresh side effects.
	trace.SetDefaultDispatcher(nil)
	log.Println("[启动] Reactor 注册完成；事件派发将在运行时恢复完成后启用")

	// Step 9: 创建邮差通知器
	notifierInterval := time.Duration(cfg.Infra.MailNotifier.IntervalSec) * time.Second
	mailNotifier := mailbox.NewMailNotifier(mbRegistry, taskStore, notifierInterval)

	sys = &System{
		Config:              cfg,
		Store:               taskStore,
		Roster:              r,
		EventCh:             eventCh,
		Watchdog:            w,
		CancelRegistry:      cancelRegistry,
		MailboxRegistry:     mbRegistry,
		MailNotifier:        mailNotifier,
		ArtifactLog:         artifactLog, // 可能为 nil（OpenArtifactLog 失败时），Shutdown 会判空
		artifactReplay:      artifactReplay,
		SessionMgr:          sessMgr, // 可能为 nil（Session 初始化失败时），Shutdown 会判空
		Scheduler:           sched,
		PlanStore:           planStore,
		PlanCoordinator:     planCoordinator,
		Interactions:        interactionService,
		PlanBatcher:         planBatcher,
		Activity:            activity,
		Runners:             runners,
		ReactorRegistry:     reactorReg,
		AgentTemplates:      templateCatalog,
		TeamManager:         teamMgr,
		TeamStore:           teamStore,
		StatusCh:            statusCh,
		OutputCh:            outputCh,
		LogFile:             logFileHolder,
		releaseInstanceLock: releaseLock,
		outputDone:          outputDone,
	}
	var staleResumeBlocks []staleResumeBlock
	if recoveredSnap != nil {
		recoveredSnap, staleResumeBlocks = prepareRecoveredSnapshot(
			sys,
			recoveredSnap,
			opts.ResumeSessionID != "",
			time.Duration(cfg.SessionResumeMaxIdleSec)*time.Second,
			time.Now(),
		)
		if len(staleResumeBlocks) > 0 {
			log.Printf("[resume] 陈旧自动恢复保护已阻断 %d 个非终态任务；如需确认恢复，请显式使用 --resume <session-id>", len(staleResumeBlocks))
		}
	}
	if err := restoreRuntimeBeforeReactorActivation(sys, recoveredSnap, staleResumeBlocks, reactorReg); err != nil {
		return nil, fmt.Errorf("恢复 Plan/Task 运行时状态失败: %w", err)
	}
	log.Println("[启动] Reactor 系统初始化完成（record-artifact, task-end-callback, trace-history-event, read-set-write, runtime-anomaly, spawn-manager）")
	if recoveredSnap != nil {
		log.Printf("[resume] 已恢复 session snapshot: tasks=%d mailboxes=%d scheduler_history=%d",
			len(recoveredSnap.Tasks), len(recoveredSnap.Mailboxes), len(recoveredSnap.SchedulerHistory))
	}
	if initialResult := loadInitialResult(cfg.ProjectRoot, sessMgr, recoveredSnap); initialResult != nil {
		sys.seedResult(initialResult)
	}

	// Step 10: session 切换钩子——/new 或 /session 成功提交后，把 plan/team
	// store 的持久化位置迁移到新 Session 目录（B2），并把 trace writer、
	// prompt dumper、system.log 重绑到新 Session 的 logs/ 目录（B5/B7）。
	// 钩子只在显式 CreateNew/SwitchTo 成功后触发；启动期 initSession 不触发，
	// 因此此刻注册不会立即回调。
	if sessMgr != nil {
		sessMgr.SetOnSwitch(sys.onSessionSwitched)
	}

	// Step 11: 装配 UI Hub——Output/Status 与 Interaction 的统一投影。
	// 此后 TUI（RunCLI）只经 ui.Controller / ui.Observer 与系统交互，不再
	// 直持任何系统通道 / 组件。
	sys.UIHub = sys.buildUIHub()

	// Step 11.5: 注册 dashboard trace reactor——trace 事件流经 ReactorRegistry
	// 进入 UI Hub（KindTraceEvent），Web Dashboard 经 SSE 订阅可见。纯观测
	// 旁路（Async、低优先级、非阻塞广播），与是否启用 web 前端无关——
	// 零订阅者时 Hub 侧是纯 no-op。
	if err := sys.ReactorRegistry.Register(dashboard.NewTraceReactor(sys.UIHub)); err != nil {
		return nil, fmt.Errorf("注册 dashboard trace reactor 失败: %w", err)
	}

	// 启动成功：单实例锁转交 System.Shutdown 释放，defer 守卫不再回收。
	bootstrapCompleted = true
	return sys, nil
}

// buildUIHub 装配 UI Hub 的全部依赖：三个 UI 通道直挂；快照轮询复用
// buildAgentInfoFn / Store.ScanAll / Mode / SessionMgr；控制面复用
// sendUserText 的事件投递语义（5 秒超时）、cancelTaskByPrefix（D2）、
// MailboxRegistry.Send（steer）、三轴 modes.Store 的运行时切换（/mode）、
// System.NewSession/SwitchSession（B3）与根 cancel。
func (s *System) buildUIHub() *ui.Hub {
	return ui.NewHub(ui.Deps{
		OutputCh:     s.OutputCh,
		StatusCh:     s.StatusCh,
		Interactions: s.Interactions,
		PollAgents:   s.buildAgentInfoFn(),
		PollBoard: func() []ui.BoardTask {
			tasks, err := s.Store.ScanAll()
			if err != nil {
				return nil
			}
			board := make([]ui.BoardTask, 0, len(tasks))
			for _, t := range tasks {
				if t == nil {
					continue
				}
				board = append(board, ui.BoardTaskFromModel(*t))
			}
			return board
		},
		ModeGet: func() string {
			// gate 轴字符串保持兼容（"plan" / "immediate"）。
			return s.Scheduler.Modes.GetGate().String()
		},
		ExecModeGet: func() string {
			return s.Scheduler.Modes.GetExec().String()
		},
		TopoModeGet: func() string {
			return s.Scheduler.Modes.GetTopo().String()
		},
		SessionGet: func() ui.SessionInfo {
			if s.SessionMgr == nil {
				return ui.SessionInfo{}
			}
			cur := s.SessionMgr.Current()
			if cur == nil {
				return ui.SessionInfo{}
			}
			return ui.SessionInfoFromMetadata(cur.Metadata)
		},
		ResultGet: func() *ui.ResultItem {
			result := s.resultSnapshot()
			if result == nil || strings.TrimSpace(result.Text) == "" {
				return nil
			}
			return &ui.ResultItem{AgentID: "scheduler", Text: result.Text}
		},
		RecordUserInput: func(text string) {
			if s.SessionMgr == nil {
				return
			}
			s.SessionMgr.RecordFirstInput(text)
			s.SessionMgr.IncrementTaskCount()
		},
		// 沿用旧 tui sendUserText 的 5 秒投递超时语义：调度器阻塞时用户
		// 得到可见错误而不是静默卡住。
		SendUserEvent: func(ev model.Event) error {
			select {
			case s.EventCh <- ev:
				return nil
			case <-time.After(5 * time.Second):
				return fmt.Errorf("事件通道超时，调度器可能阻塞")
			}
		},
		CancelTaskByPrefix: func(idPrefix string) (string, error) {
			ctx := s.startCtx
			if ctx == nil {
				ctx = context.Background()
			}
			return cancelTaskByPrefix(ctx, s.Store, s.PlanCoordinator, idPrefix)
		},
		CancelLatestRequest: func() (string, error) {
			ctx := s.startCtx
			if ctx == nil {
				ctx = context.Background()
			}
			return cancelLatestActiveRequest(ctx, s.Store, s.PlanCoordinator)
		},
		SteerSend: s.MailboxRegistry.Send,
		// /mode 三轴切换：gate 轴走 ModeSet；exec / topo 轴由 Hub 解析成
		// 规范化小写串后到这里写回 modes.Store（再解析一次做防御性校验）。
		ModeSet: s.Scheduler.Modes.SetGate,
		ExecModeSet: func(mode string) error {
			m, err := modes.ParseExecMode(mode)
			if err != nil {
				return err
			}
			s.Scheduler.Modes.SetExec(m)
			return nil
		},
		TopoModeSet: func(mode string) error {
			m, err := modes.ParseTopoMode(mode)
			if err != nil {
				return err
			}
			s.Scheduler.Modes.SetTopo(m)
			return nil
		},
		SessionNew:    s.NewSession,
		SessionSwitch: s.SwitchSession,
		// /plan 三入口：gate=plan 的 plan_review 挂起由用户经 UI 批准/拒绝；
		// 批准时由 bootstrap 预发布保留 controller 并恢复 Plan（去 LLM 版的
		// Interaction response），scheduler agent 随后认领该任务按计划执行。
		ApprovePlanReview: func(idPrefix string) (string, error) {
			ctx := s.startCtx
			if ctx == nil {
				ctx = context.Background()
			}
			return s.respondPlanReviewByPrefix(ctx, idPrefix, "execute_plan")
		},
		RejectPlanReview: func(idPrefix string) (string, error) {
			ctx := s.startCtx
			if ctx == nil {
				ctx = context.Background()
			}
			return s.respondPlanReviewByPrefix(ctx, idPrefix, "cancel_request")
		},
		PendingPlanReviews: func() ([]ui.PlanReviewItem, error) {
			ctx := s.startCtx
			if ctx == nil {
				ctx = context.Background()
			}
			return s.pendingPlanReviewsFromInteractions(ctx)
		},
		ResolveInteraction: s.resolveInteraction,
		SessionList: func() ([]ui.SessionInfo, error) {
			if s.SessionMgr == nil {
				return nil, fmt.Errorf("session 管理器未初始化")
			}
			metas, err := s.SessionMgr.List()
			if err != nil {
				return nil, err
			}
			infos := make([]ui.SessionInfo, 0, len(metas))
			for _, md := range metas {
				infos = append(infos, ui.SessionInfoFromMetadata(md))
			}
			return infos, nil
		},
		QuitFn: func() {
			if s.cancel != nil {
				s.cancel()
			}
		},
	})
}

// Start 启动所有后台 goroutine。cancel 用于 CLI /quit 触发全局退出。
func (s *System) Start(ctx context.Context, cancel context.CancelFunc) error {
	s.cancel = cancel
	s.startCtx = ctx

	// SIGINT 哨兵最先武装：此后任何状态下（含启动期、TUI 事件循环死锁、
	// Shutdown 挂死）Ctrl+C 都能终止进程。headless 判定与 RunCLI 保持一致。
	// shutdownDone 提前初始化交给哨兵：Shutdown 完成即撤防 deadline 定时器
	//（Shutdown 的懒初始化会复用同一 channel）。
	s.shutdownInitOnce.Do(func() {
		s.shutdownDone = make(chan struct{})
	})
	startSigSentinel(ctx, cancel, !shouldStartTUI(s.uiFrontends()), s.shutdownDone)

	// 将 PlanStore 中的暂停事实持续物化为结构化 Interaction；恢复 Session
	// 后也由同一 reconciliation 路径重建，不依赖易丢失的瞬时事件。
	s.startInteractionRuntime(ctx)

	// Step 4.6: 启动 UI Hub（output/status 与 Interaction 投影的统一消费者；
	// 无订阅者也常驻排干，ctx 取消即退出）。
	s.startUIHub(ctx)

	// Step 4.7: 启动 Web Dashboard（ui.frontends 含 "web" 时；
	// 与 UI Hub 同一监督 goroutine 组，ctx 取消即优雅关闭）。
	s.startDashboard(ctx)

	// 把当前 ctx 作为所有 ad-hoc spawn 的父 ctx——Shutdown 通过 cancel() 传播。
	// 必须早于任何 reactor 触发，否则 ad-hoc runner 会用 context.Background 启动，
	// system 关闭时无法停下。
	if s.SpawnManager != nil {
		s.SpawnManager.SetParentContext(ctx)
	}
	// 动态 Team 必须先完成持久化恢复和 route 注册，Scheduler 才能看到一致的
	// runtime snapshot 并安全地发布任务。恢复不一致时启动 fail-closed。
	if s.TeamManager != nil {
		if err := s.TeamManager.Start(ctx); err != nil {
			return fmt.Errorf("恢复 AgentTemplate Team 失败: %w", err)
		}
		log.Printf("[启动] AgentTemplate TeamManager 已启动 (%d 个 runtime agent)", s.TeamManager.ActiveCount())
	}
	// TeamManager 已认领所有仍应运行的动态 Team 邮箱；剩余的恢复邮箱只可能
	// 属于 terminal/stopped/已删除或未知 Team。必须在 MailNotifier 启动前清掉，
	// 否则它们会为不存在的 route 反复发布 orphan wake task。
	if s.MailboxRegistry != nil {
		if discarded := s.MailboxRegistry.DiscardUnclaimedRecovered(); discarded > 0 {
			log.Printf("[启动] 已丢弃 %d 个无运行时所有者的恢复邮箱", discarded)
		}
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
	s.startPeriodicSnapshots(ctx)

	fmt.Println("[启动] 系统就绪，等待用户输入")
	return nil
}

// startUIHub 把 UI Hub 纳入 System 的监督 goroutine 组（与 scheduler /
// watchdog / notifier / runners 同一 wg；Shutdown 的 s.wg.Wait 会等它退出）。
func (s *System) startUIHub(ctx context.Context) {
	if s.UIHub == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.UIHub.Run(ctx)
	}()
	log.Println("[启动] UI Hub 已启动")
}

// startDashboard 在 ui.frontends 含 "web" 时启动 Web Dashboard（观测面 +
// 控制面均接 UI Hub），纳入与 startUIHub 同一监督 goroutine 组（Shutdown
// 经 ctx 取消触发 http.Server.Shutdown 优雅退出，s.wg.Wait 收尸）。
func (s *System) startDashboard(ctx context.Context) {
	if s.UIHub == nil || s.Config == nil || !s.Config.UI.HasFrontend("web") {
		return
	}
	srv := dashboard.NewServer(s.UIHub, s.Config.UI.Web.Listen, s.Config.UI.Web.Token)
	srv.SetController(s.UIHub)
	s.Dashboard = srv
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := srv.Run(ctx); err != nil {
			log.Printf("[关闭] WARNING: Web Dashboard 异常退出: %v", err)
		}
	}()
	log.Printf("[启动] Web Dashboard 已启动: http://%s/", s.Config.UI.Web.Listen)
	// 同步直写 stdout：log 早在 bootstrap 阶段已被重定向到 system.log，
	// 不打这一行用户在终端无从得知 Web 控制台的存在与地址（2026-07-18 诊断）。
	fmt.Fprintf(os.Stdout, "[启动] Web Dashboard: http://%s/\n", s.Config.UI.Web.Listen)
	s.maybeAutoOpenBrowser(ctx)
}

// maybeAutoOpenBrowser 在 Dashboard 真正开始监听后，用系统默认浏览器打开
// 控制台地址（ui.web.auto_open 缺省为开）。整个动作异步执行，失败仅 WARN——
// 浏览器拉不起来不影响系统本身。token 非空时地址带 ?token=（loopback 管理
// 台面，省去用户手输；URL 经 QueryEscape）。
func (s *System) maybeAutoOpenBrowser(ctx context.Context) {
	if s.Config == nil || !s.Config.UI.Web.AutoOpenEnabled() {
		return
	}
	base := fmt.Sprintf("http://%s/", s.Config.UI.Web.Listen)
	openURL := base
	if tok := s.Config.UI.Web.Token; tok != "" {
		openURL = base + "?token=" + url.QueryEscape(tok)
	}
	go func() {
		if !dashboard.WaitReady(ctx, base, 3*time.Second) {
			log.Printf("[启动] WARNING: Web Dashboard 未在 3s 内就绪，跳过自动打开浏览器")
			return
		}
		if err := dashboard.OpenBrowser(openURL); err != nil {
			log.Printf("[启动] WARNING: 自动打开浏览器失败: %v", err)
		}
	}()
}

// switchableLogFile 持有当前 system.log 文件句柄，支持 session 切换时换绑
// （B7）。Write 经 RLock 现取当前文件——Swap 与任意 goroutine 上的并发
// log.Printf 安全；log.SetOutput 的接线（含 tuiLogWriter 包装）全程不动。
//
// 写路径在 RLock 内完成：log 包本身已串行化 Write，RLock 对写者无额外
// 竞争代价，却能保证 Swap 关闭旧文件时绝不会有在途写落到已关闭句柄上。
type switchableLogFile struct {
	mu   sync.RWMutex
	file *os.File // 当前句柄，nil 表示已关闭（写丢弃）
}

// Write 实现 io.Writer：写入当前文件；无当前文件（已 Close）时丢弃并返回
// 成功，避免 log 调用方拿到错误进入重试循环。
func (s *switchableLogFile) Write(p []byte) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return len(p), nil
	}
	return s.file.Write(p)
}

// Swap 换绑到新文件并关闭旧文件。调用方必须先成功打开新文件再 Swap
// （open-new-before-close-old），失败时保持旧绑定不动。
func (s *switchableLogFile) Swap(newFile *os.File) {
	s.mu.Lock()
	old := s.file
	s.file = newFile
	s.mu.Unlock()
	if old != nil && old != newFile {
		_ = old.Close()
	}
}

// Close 关闭当前句柄并停用（此后写丢弃）。重复调用安全。
func (s *switchableLogFile) Close() error {
	s.mu.Lock()
	old := s.file
	s.file = nil
	s.mu.Unlock()
	if old != nil {
		return old.Close()
	}
	return nil
}

// tuiLogWriter 把日志同时写入文件，并把每一行非空内容复制到 TUI 的 status channel。
// 这样日志既持久化到文件，用户又能在 TUI 内看到关键进度。
type tuiLogWriter struct {
	file   *switchableLogFile // 每次 Write 现取当前句柄（session 切换后自动写新文件）
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

// onSessionSwitched 是 SessionManager 的 OnSwitch 钩子：/new 或 /session
// 成功提交后执行全部 session 目录重绑（B2/B5/B7）：
//  1. plan/team store 的持久化位置迁移到新 Session 目录（RebindDir 会把当前
//     内存态立即写一次到新路径；运行时状态跨 session 连续，仅落盘位置迁移）；
//  1.5 Session Memory（memory.jsonl）换绑到新 Session 目录（新后端实例从
//     目标文件 replay 出该 session 的历史；无常驻句柄，换绑即生效）；
//  2. trace writer / prompt dumper / system.log 重绑到新 Session 的 logs/ 目录。
//
// Plan/Team 是恢复所需的关键持久化事实：失败会记录到 sessionSwitchErr，
// 由 System.NewSession/SwitchSession 在同一 snapshotMu 边界内回滚 Session。
// trace/dumper/system.log 属于观测资源，仍各自 open-new-before-close-old，失败
// 只告警并保留旧绑定。
func (s *System) onSessionSwitched(newSess *session.Session) {
	if newSess == nil {
		return
	}

	// 1. plan/team store 持久化位置迁移。某些配置下两者可为 nil
	//    （scheduler-only / teams 禁用），判空跳过。
	if s.PlanStore != nil {
		if err := s.PlanStore.RebindDir(filepath.Join(newSess.Dir, "plan-state.json")); err != nil {
			log.Printf("[session] WARNING: PlanStore 重绑失败: %v —— plan 持久化保持旧目录", err)
			s.recordSessionSwitchError(fmt.Errorf("PlanStore 重绑失败: %w", err))
		} else {
			log.Printf("[session] PlanStore 已重绑到新 Session (dir=%s)", newSess.Dir)
		}
	}
	if s.TeamStore != nil {
		if err := s.TeamStore.RebindDir(filepath.Join(newSess.Dir, "agent-teams.json")); err != nil {
			log.Printf("[session] WARNING: TeamStore 重绑失败: %v —— team 持久化保持旧目录", err)
			s.recordSessionSwitchError(fmt.Errorf("TeamStore 重绑失败: %w", err))
		} else {
			log.Printf("[session] TeamStore 已重绑到新 Session (dir=%s)", newSess.Dir)
		}
	}

	// 1.5 Session Memory 重绑：memory.jsonl 迁移到新 Session 目录（新实例
	//    从目标文件 replay 出该 session 的历史）。SessionStore 无常驻句柄
	//    （每次写入 open→fsync→close），换绑无需关闭旧后端。失败只告警
	//    并保留旧绑定（同 trace/system.log 的观测资源语义，不阻断切换）。
	if s.Scheduler != nil && s.Scheduler.Agent != nil {
		if proc, ok := s.Scheduler.Agent.Memory.(*memory.ProcessStore); ok && proc != nil {
			memPath := filepath.Join(newSess.Dir, "memory.jsonl")
			if backend, err := memory.NewSessionStore(memPath); err != nil {
				log.Printf("[session] WARNING: Session Memory 重绑失败 (%s): %v —— 保持旧目录", memPath, err)
			} else {
				proc.AttachSessionStore(backend)
				log.Printf("[session] Session Memory 已重绑到新 Session (dir=%s)", newSess.Dir)
			}
		}
	}

	// 2. trace writer / prompt dumper / system.log
	logsDir := filepath.Join(newSess.Dir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		log.Printf("[session] WARNING: 新 Session logs 目录创建失败 (%s): %v —— trace/system.log 保持旧绑定", logsDir, err)
		return
	}

	// 2.1 trace writer：与 bootstrap Step 1.5 相同的 maxTasks=100 语义
	newWriter, err := trace.NewWriter(logsDir, 100)
	if err != nil {
		log.Printf("[session] WARNING: 新 Session trace writer 创建失败 (%s): %v —— trace 保持旧绑定", logsDir, err)
	} else {
		old := trace.SwapDefaultWriter(newWriter)
		if old != nil {
			_ = old.Close() // Close 后旧 writer 永久停用，不会复活旧目录文件
		}
		log.Printf("[session] trace 已重绑到新 Session (dir=%s)", logsDir)
	}

	// 2.2 prompt dumper：仅当前已启用（默认 dumper 非 nil）时才换绑；
	//    未启用时默认 dumper 为 nil，无需处理
	if trace.DefaultDumper() != nil {
		newDumper, derr := trace.NewPromptDumper(logsDir, true)
		if derr != nil {
			log.Printf("[session] WARNING: 新 Session prompt dumper 创建失败 (%s): %v —— dumper 保持旧绑定", logsDir, derr)
		} else {
			old := trace.SwapDefaultDumper(newDumper)
			if old != nil {
				old.Close()
			}
		}
	}

	// 2.3 system.log：新文件打开成功后才 Swap（Swap 内部关闭旧句柄）
	if s.LogFile != nil {
		newLogFile, lerr := os.OpenFile(filepath.Join(logsDir, "system.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if lerr != nil {
			log.Printf("[session] WARNING: 新 Session system.log 打开失败 (%s): %v —— 日志保持旧绑定", logsDir, lerr)
		} else {
			s.LogFile.Swap(newLogFile)
			log.Printf("[session] system.log 已重绑到新 Session (dir=%s)", logsDir)
		}
	}
}

// drainOutputChannels 持续丢弃 outputCh/statusCh 上的消息，直到 stop 关闭。
// 仅在 Shutdown 期间使用——正常运行时这两个 channel 由 UI Hub 消费，
// drainer 与 Hub 并存会吞掉用户可见输出。nil channel 的 case 永不就绪，
// 调用方传 nil 等价于只 drain 另一个。
func drainOutputChannels(outputCh <-chan output.Event, statusCh <-chan string, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-outputCh:
		case <-statusCh:
		}
	}
}

// eventWriter 把写入的字节块包装成 output.Event 发送到 channel，供 TUI 接收并渲染。
// 用于 agent/scheduler 的 UserOutput（KindText）与 ResultOutput（KindResult），
// 将用户可见内容注入 Bubble Tea；分类（kind）在产生处完成，消费方不再做子串匹配。
//
// done 是 Shutdown 逃生口：UI Hub 随 ctx 取消退出后 channel 无人消费，
// 继续阻塞写会让 agent 卡死、System.Shutdown 的 wg.Wait() 永不返回（H5）。
// done 关闭后 Write 丢弃文本但返回成功（len(p), nil），避免 fmt.Fprintf 调用方
// 进入错误重试循环。done 为 nil 时保持纯阻塞语义——TUI 存活期间的背压是有意的。
type eventWriter struct {
	ch      chan<- output.Event
	done    <-chan struct{}
	kind    output.Kind
	agentID string
	// onResult 仅在 kind==KindResult 且文本真正入队后触发（recordResult 记账）；
	// 逃逸路径与普通文本写不触发。
	onResult func(string)
}

func (w *eventWriter) Write(p []byte) (int, error) {
	text := string(p)
	select {
	case w.ch <- output.Event{Kind: w.kind, AgentID: w.agentID, Text: text}:
		// onResult（recordResult）只在结果文本真正入队后执行，逃逸路径不记账。
		if w.kind == output.KindResult && w.onResult != nil {
			w.onResult(text)
		}
	case <-w.done:
	}
	return len(p), nil
}

// cancelTaskByPrefix 实现 TUI /cancel 的任务解析与受守卫取消（D2）。
// idPrefix 经 Store.ScanAll 前缀匹配（与 trace CLI 的前缀解析语义对齐）：
// 0 个匹配 → 未找到；多于 1 个 → 歧义错误并列出候选 ID，不取消任何任务；
// 恰好 1 个 → 走与 LLM cancel_task 相同的 tools.GuardedCancel（调用方无
// Plan 上下文，source="user"——归属 Plan 的任务会被守卫拒绝）。
// 前缀短于 4 字符直接报错，不做猜测。
func cancelTaskByPrefix(ctx context.Context, s store.TaskStore, coordinator *plan.Coordinator, idPrefix string) (string, error) {
	const minPrefixLen = 4
	if len(idPrefix) < minPrefixLen {
		return "", fmt.Errorf("任务 ID 前缀过短（至少 %d 个字符）: %s", minPrefixLen, idPrefix)
	}
	tasks, err := s.ScanAll()
	if err != nil {
		return "", fmt.Errorf("读取任务列表失败: %w", err)
	}
	var matches []string
	for _, t := range tasks {
		if strings.HasPrefix(t.ID, idPrefix) {
			matches = append(matches, t.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("未找到以 %s 开头的任务", idPrefix)
	case 1:
		if err := tools.GuardedCancel(ctx, s, coordinator, "", matches[0], "user"); err != nil {
			return "", err
		}
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("找到 %d 个匹配的任务，请使用更长的任务 ID 区分:\n  %s",
			len(matches), strings.Join(matches, "\n  "))
	}
}

// RunCLI 启动主前端，阻塞直到用户退出或 ctx 取消。
// frontends 含 "tui" 时主前端是 TUI（bubbletea 直接接管 stdin/stdout，
// 无需调用方传入 reader/writer；TUI 只经 UI Hub 的 Controller/Observer
// 与系统交互，不直持任何系统通道 / 组件）；
// 不含 "tui" 时为 headless 模式：不启动 TUI，阻塞到 ctx 取消后返回，
// 由 main 继续 Shutdown（UI Hub 与 Web Dashboard 已在 Start 独立运行）。
func (s *System) RunCLI(ctx context.Context) {
	// 将运行时 log 重定向到文件 + UI Hub 状态通道（TUI 消息区 / Web SSE
	// 日志帧共用同一条 StatusCh）。
	// Bootstrap 期间 log 已写入同一文件；此处复用句柄，追加 channel 旁路。
	oldLogWriter := log.Writer()
	if s.LogFile != nil {
		log.SetOutput(&tuiLogWriter{file: s.LogFile, status: s.StatusCh})
		defer log.SetOutput(oldLogWriter)
	}

	if !shouldStartTUI(s.uiFrontends()) {
		// 同样直写 stdout（log 已重定向 system.log）——headless 模式下
		// 终端是用户唯一能看到 Web 控制台地址的地方。
		if s.Config != nil && s.Config.UI.HasFrontend("web") {
			log.Printf("[启动] headless 模式：未启用 TUI 前端；Web 控制台地址 http://%s/ ，等待关闭信号", s.Config.UI.Web.Listen)
			fmt.Fprintf(os.Stdout, "[启动] headless 模式：Web 控制台 http://%s/ ，等待关闭信号\n", s.Config.UI.Web.Listen)
		} else {
			log.Printf("[启动] headless 模式：未启用 TUI 前端，等待关闭信号")
			fmt.Fprintln(os.Stdout, "[启动] headless 模式：未启用 TUI 前端，等待关闭信号")
		}
		<-ctx.Done()
		return
	}

	deps := tui.Deps{
		InitialResult: initialResultText(s.resultSnapshot()),
	}
	if s.UIHub != nil {
		deps.Controller = s.UIHub
		deps.Observer = s.UIHub
	}
	if err := tui.Run(ctx, deps); err != nil {
		fmt.Fprintf(os.Stderr, "[TUI] 异常退出: %v\n", err)
	}
}

// uiFrontends 返回配置启用的前端列表；Config 缺失时返回 nil，
// shouldStartTUI 将其视为默认 [tui]。
func (s *System) uiFrontends() []string {
	if s.Config == nil {
		return nil
	}
	return s.Config.UI.Frontends
}

// shouldStartTUI 报告是否应启动 TUI 前端：frontends 含 "tui" 时启动。
// 空列表等价 config.validateUI 的默认回落 [tui]。
func shouldStartTUI(frontends []string) bool {
	if len(frontends) == 0 {
		return true
	}
	for _, f := range frontends {
		if f == "tui" {
			return true
		}
	}
	return false
}

// Shutdown 优雅关闭所有服务。完整流程并发幂等；所有调用者等待首个执行者
// 完成并得到同一个持久化结果。
func (s *System) Shutdown() error {
	if s == nil {
		return nil
	}
	s.shutdownInitOnce.Do(func() {
		s.shutdownDone = make(chan struct{})
	})
	s.shutdownOnce.Do(func() {
		defer close(s.shutdownDone)
		s.shutdownErr = s.shutdown()
	})
	<-s.shutdownDone
	return s.shutdownErr
}

func (s *System) shutdown() error {
	// 先解除 eventWriter 的阻塞写：UI Hub 随下面的 cancel 退出后 outputCh
	// 无人消费，若 agent 此刻正写用户可见输出，填满缓冲后会永久阻塞，
	// 导致后面的 s.wg.Wait() 挂死（H5）。closeOnce 保证重复 Shutdown 安全。
	s.outputDoneOnce.Do(func() {
		if s.outputDone != nil {
			close(s.outputDone)
		}
	})
	// 先使所有尚未回答的 Interaction 进入明确终态，唤醒 Shell Await 并
	// 避免控制面留下永久 pending 的幽灵请求；随后再取消全局 ctx。
	s.interruptPendingInteractions("system shutdown")
	if s.cancel != nil {
		s.cancel()
	}
	// UI Hub 已退出：启动临时 drainer 丢弃 outputCh/statusCh 上的残余
	// 产出，保证拆卸期间（SpawnManager/TeamManager 关闭 + wg.Wait）生产者永不阻塞。
	// wg.Wait() 返回后所有生产者已退出，停止 drainer 并等其退出，不泄漏 goroutine。
	drainStop := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		drainOutputChannels(s.OutputCh, s.StatusCh, drainStop)
	}()
	// SpawnManager.Shutdown 等待所有 ad-hoc runner goroutine 退出（cancel 已通过 ctx 传播）。
	// 必须在 s.wg.Wait 之前调用——s.wg 不持有 ad-hoc runner 的 wg，那些 goroutine 由
	// SpawnManager 内部的 wg 管理。
	if s.SpawnManager != nil {
		s.SpawnManager.Shutdown()
	}
	// TeamManager 持有自己的 runtime runner goroutine；先撤销动态 route、
	// 停止 runner 并清理 activity/roster，但把 mailbox 暂留到最终 Session
	// 快照之后，避免未读邮件被最终快照抹掉。
	if s.TeamManager != nil {
		s.TeamManager.ShutdownPreservingMailboxes()
	}
	s.wg.Wait()
	close(drainStop)
	<-drainDone
	// E4：quiesce reactor 系统——先取消 registry 派生 ctx 打断在途 async reactor
	// 的 LLM/spawn 调用，再在 2s 界内等其排空。位置约束：必须在 agent 全部停止
	// 之后（wg.Wait 返回后不再有新事件源产生 async 分发），且必须在 store /
	// trace / artifact log 关闭之前——在途 reactor 会写这些资源，晚于此点关闭
	// 就会写进已关闭句柄。超时残余不再等待（进程退出兜底），仅 WARN 计数。
	if s.ReactorRegistry != nil {
		if remaining := s.ReactorRegistry.Quiesce(2 * time.Second); remaining > 0 {
			log.Printf("[关闭] WARNING: %d 个异步 reactor 在 quiesce 超时后仍在运行，残余写可能被已关闭资源拒绝", remaining)
		}
	}
	// 最终快照对动态 Team mailbox 是提交点。对短暂 I/O 故障做有限重试；
	// 若仍失败则保留邮箱、不 finalize，并把错误返回给调用者，绝不静默删除
	// 尚无持久化副本的未读邮件。
	var snapshotErr error
	for attempt := 1; attempt <= 3; attempt++ {
		snapshotErr = s.saveRuntimeSnapshotWithError()
		if snapshotErr == nil {
			break
		}
		if attempt < 3 {
			log.Printf("[关闭] WARNING: 最终 Session 快照第 %d 次保存失败，将重试: %v", attempt, snapshotErr)
			time.Sleep(25 * time.Millisecond)
		}
	}
	if snapshotErr == nil && s.TeamManager != nil {
		s.TeamManager.FinalizeShutdownMailboxes()
	} else if snapshotErr != nil {
		log.Printf("[关闭] ERROR: 最终 Session 快照保存失败，动态 Team 邮箱保留且未注销: %v", snapshotErr)
	}
	// Drain the Task→Plan durability bridge before tearing down the store's lazy
	// retry worker. C1：变更由 PlanBatcher 的 flusher 异步落盘——先在原 2s 界内
	// 等待"队列空 + 在途 apply 完成"，再 Stop flusher（等其退出，不泄漏
	// goroutine）。超时后残余变更被丢弃并计数告警；偏差由下次启动的 resume
	// 对账检测并阻断（与既有语义一致）。batcher 的 submit hook 永不返回错误，
	// store 侧 planMutationBacklog/WaitPlanMutations 在接入 batcher 后保持空转。
	if s.PlanBatcher != nil {
		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := s.PlanBatcher.Drain(waitCtx); err != nil {
			log.Printf("[关闭] WARNING: Task→Plan mutation 未完全落盘: %v", err)
		}
		cancel()
		s.PlanBatcher.Stop()
		if dropped := s.PlanBatcher.Dropped(); dropped > 0 {
			log.Printf("[关闭] WARNING: %d 条 Task→Plan mutation 超出 drain 界被丢弃（resume 对账兜底）", dropped)
		}
	}
	if closer, ok := s.Store.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			log.Printf("[关闭] WARNING: Task store 关闭失败: %v", err)
		}
	}
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
	// 释放单实例锁（E7，幂等）。放在最后——锁守护的 .agentgo 资源（日志、
	// 存储、session）此刻已全部关闭；进程若未走到这里（崩溃），下次启动由
	// 陈旧锁检测接管。
	if s.releaseInstanceLock != nil {
		s.releaseInstanceLock()
	}
	fmt.Println("[关闭] 系统已停止")
	if snapshotErr != nil {
		return fmt.Errorf("最终 Session 快照保存失败: %w", snapshotErr)
	}
	return nil
}

// buildAgentInfoFn creates a closure that collects agent info from all runners + scheduler.
// 产出 ui.AgentCard（字段与旧 tui.AgentInfo 完全镜像），供 UI Hub 轮询。
func (s *System) buildAgentInfoFn() func() []ui.AgentCard {
	return func() []ui.AgentCard {
		// Build agent→task lookup from store (single scan)
		// D3：busy 推导统一走 store.BusyAgentTasks（processing 任务 → agent 映射，
		// 同一 agent 多个 processing 任务时保留 first-seen 确定序）。
		type taskRef struct {
			id   string
			desc string
		}
		tasks, _ := s.Store.ScanAll()
		agentTasks := map[string]taskRef{}
		for aid, t := range store.BusyAgentTasks(tasks) {
			agentTasks[aid] = taskRef{id: t.ID, desc: t.Description}
		}

		var infos []ui.AgentCard
		seen := map[string]bool{}

		// Scheduler agent
		if s.Scheduler != nil && s.Scheduler.Agent != nil {
			a := s.Scheduler.Agent
			ref := agentTasks[a.ID]
			ts := a.TokenStatsSnapshot()
			info := ui.AgentCard{
				ID:               a.ID,
				Type:             "scheduler",
				State:            string(a.CurrentState()),
				CurrentTaskID:    ref.id,
				CurrentTaskDesc:  ref.desc,
				PromptTokens:     ts.TotalPromptTokens,
				CompletionTokens: ts.TotalCompletionTokens,
				CallCount:        ts.CallCount,
			}
			s.applyActivityInfo(&info)
			info.SchedulerControl = s.schedulerControlState(ref.id)
			infos = append(infos, info)
			seen[a.ID] = true
		}

		// Runner agents
		for _, rn := range s.Runners {
			a := rn.Agent()
			ref := agentTasks[a.ID]
			var mbPending int
			if a.Mailbox != nil {
				mbPending = a.Mailbox.Len()
			}
			agentType := a.EventType
			if agentType == "" {
				agentType = "worker"
			}
			ts := a.TokenStatsSnapshot()
			info := ui.AgentCard{
				ID:               a.ID,
				Type:             agentType,
				State:            string(a.CurrentState()),
				CurrentTaskID:    ref.id,
				CurrentTaskDesc:  ref.desc,
				MailboxPending:   mbPending,
				PromptTokens:     ts.TotalPromptTokens,
				CompletionTokens: ts.TotalCompletionTokens,
				CallCount:        ts.CallCount,
			}
			s.applyActivityInfo(&info)
			infos = append(infos, info)
			seen[a.ID] = true
		}

		if s.Activity != nil {
			for _, snap := range s.Activity.Snapshots() {
				if seen[snap.AgentID] {
					continue
				}
				info := ui.AgentCard{
					ID:              snap.AgentID,
					Type:            snap.AgentType,
					State:           "processing",
					CurrentTaskID:   snap.TaskID,
					CurrentTaskDesc: snap.TaskDesc,
				}
				if snap.TaskID == "" {
					info.State = "idle"
				}
				s.applyActivitySnapshot(&info, snap)
				infos = append(infos, info)
			}
		}

		return infos
	}
}

func (s *System) applyActivityInfo(info *ui.AgentCard) {
	if s.Activity == nil || info == nil {
		return
	}
	snap, ok := s.Activity.Snapshot(info.ID)
	if !ok {
		return
	}
	s.applyActivitySnapshot(info, snap)
}

func (s *System) applyActivitySnapshot(info *ui.AgentCard, snap agent.ActivitySnapshot) {
	if info.Type == "" {
		info.Type = snap.AgentType
	}
	if snap.TaskID != "" && info.CurrentTaskID == "" {
		info.CurrentTaskID = snap.TaskID
	}
	if snap.TaskDesc != "" && info.CurrentTaskDesc == "" {
		info.CurrentTaskDesc = snap.TaskDesc
	}
	info.Loop = snap.Loop
	info.Phase = snap.Phase
	info.LastModelText = snap.LastModelText
	info.LastTool = snap.LastTool
	info.ToolCallCount = snap.ToolCallCount
	info.LastActivityAt = snap.LastActivityAt
	info.ActivityAge = formatActivityAge(snap.LastActivityAt)
	info.LastError = snap.LastError
	if len(snap.ActiveTools) > 0 {
		info.ActiveTools = make([]ui.AgentToolActivity, 0, len(snap.ActiveTools))
		for _, tool := range snap.ActiveTools {
			info.ActiveTools = append(info.ActiveTools, ui.AgentToolActivity{
				CallID: tool.CallID, Tool: tool.ToolName, StartedAt: tool.StartedAt,
			})
		}
	} else {
		info.ActiveTools = nil
	}
}

func formatActivityAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	age := time.Since(t)
	if age < time.Second {
		return "now"
	}
	if age < time.Minute {
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(age.Hours()))
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
