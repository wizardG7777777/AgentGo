package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"agentgo/internal/cli"
	"agentgo/internal/config"
	"agentgo/internal/explorer"
	"agentgo/internal/isolation"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/watchdog"
	"agentgo/internal/worker"
)

type System struct {
	Config           *config.Config
	Store            store.TaskStore
	Roster           roster.Roster
	EventCh          chan model.Event
	Watchdog         *watchdog.Watchdog
	CancelRegistry   *store.TaskCancelRegistry
	MailboxRegistry  *mailbox.Registry
	MailNotifier     *mailbox.MailNotifier
	Scheduler        *scheduler.Scheduler
	Explorer         *explorer.Explorer
	Workers          []*worker.Worker
	ApprovalCh       chan shell.ApprovalRequest // 命令审批通道，Worker→CLI
	WorktreeManager  *isolation.WorktreeManager
	ConflictResolver *isolation.ConflictResolver
	CLI              *cli.CLI
	cancel           context.CancelFunc
	wg               sync.WaitGroup
}

func Bootstrap(configPath string, explicit bool) (*System, error) {
	// Step 1: 加载配置
	cfg, err := config.LoadConfig(configPath, explicit)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("[启动] 全局配置加载完成")

	// Step 2: 初始化公告板
	eventCh := make(chan model.Event, cfg.EventChannelBuffer)
	taskStore := store.NewMemoryTaskStore(eventCh, cfg.FIFOLimit, cfg.DefaultConcurrency, cfg.DefaultTimeoutSec)
	cancelRegistry := store.NewTaskCancelRegistry()
	taskStore.SetCancelRegistry(cancelRegistry)
	fmt.Println("[启动] 公告板初始化完成")

	// Step 3: 初始化花名册
	r := roster.NewMemoryRoster()
	fmt.Println("[启动] 花名册初始化完成")

	// Step 3.5: 初始化邮箱注册表
	mbRegistry := mailbox.NewRegistry(cfg.MailboxBufferSize)
	fmt.Println("[启动] 邮箱注册表初始化完成")

	// Step 4: 创建 LLM 客户端
	schedulerLLM := llm.NewSDKClient(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
		"", // system prompt 由 scheduler 内部管理
		time.Duration(cfg.LLMTimeoutSec)*time.Second,
	)
	explorerLLM := llm.NewSDKClient(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.ExplorerModel,
		"", // system prompt 由 explorer 内部管理
		time.Duration(cfg.LLMTimeoutSec)*time.Second,
	)

	// Step 5: 创建调度器（eventCh 消费者，必须先于生产者启动）
	sched := scheduler.New(taskStore, schedulerLLM, eventCh, cfg, mbRegistry)

	// Step 6: 创建看门狗
	w := watchdog.New(taskStore, cfg, eventCh, r)

	// Step 6.5: 创建 worktree 管理器和冲突解决代理（可选）
	var wtManager *isolation.WorktreeManager
	var resolver *isolation.ConflictResolver
	if cfg.WorktreeEnabled {
		wtManager = isolation.NewWorktreeManager(cfg.ProjectRoot)
		resolverLLM := llm.NewSDKClient(
			cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.ExplorerModel,
			"", // system prompt 由 resolver 内部管理
			time.Duration(cfg.LLMTimeoutSec)*time.Second,
		)
		resolver = isolation.NewConflictResolver(cfg.ProjectRoot, resolverLLM, taskStore)
		fmt.Println("[启动] Worktree 隔离已启用")
	}

	// Step 7: 创建调查代理
	exp := explorer.New(taskStore, r, explorerLLM, cfg, cancelRegistry, mbRegistry, wtManager)

	// Step 7.5: 创建命令审批通道（Worker→CLI）
	approvalCh := make(chan shell.ApprovalRequest, 8)

	// Step 8: 创建执行代理（使用主 LLM，认领 event_type="" 的执行任务）
	workerCount := cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 1
	}
	var workers []*worker.Worker
	for i := 1; i <= workerCount; i++ {
		workerLLM := llm.NewSDKClient(
			cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
			"", // system prompt 由 worker 内部管理
			time.Duration(cfg.LLMTimeoutSec)*time.Second,
		)
		wk := worker.NewWithID(fmt.Sprintf("worker-%d", i), taskStore, r, workerLLM, cfg, cancelRegistry, mbRegistry, approvalCh, wtManager, resolver)
		workers = append(workers, wk)
	}

	// Step 9: 创建邮差通知器
	notifierInterval := time.Duration(cfg.MailNotifierIntervalSec) * time.Second
	mailNotifier := mailbox.NewMailNotifier(mbRegistry, taskStore, notifierInterval)

	sys := &System{
		Config:           cfg,
		Store:            taskStore,
		Roster:           r,
		EventCh:          eventCh,
		Watchdog:         w,
		CancelRegistry:   cancelRegistry,
		MailboxRegistry:  mbRegistry,
		MailNotifier:     mailNotifier,
		Scheduler:        sched,
		Explorer:         exp,
		Workers:          workers,
		ApprovalCh:       approvalCh,
		WorktreeManager:  wtManager,
		ConflictResolver: resolver,
	}

	return sys, nil
}

// Start 启动所有后台 goroutine。cancel 用于 CLI /quit 触发全局退出。
func (s *System) Start(ctx context.Context, cancel context.CancelFunc) {
	s.cancel = cancel

	// Step 5: 启动调度器（消费者先就绪）
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Scheduler.Run(ctx)
	}()
	fmt.Println("[启动] 调度器已启动")

	// Step 6: 启动看门狗
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWatchdogWithRecover(ctx)
	}()
	fmt.Println("[启动] 看门狗已启动")

	// Step 6.5: 启动邮差通知器
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.MailNotifier.Run(ctx)
	}()
	fmt.Println("[启动] 邮差通知器已启动")

	// Step 6.8: 启动冲突解决代理（可选）
	if s.ConflictResolver != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.ConflictResolver.Run(ctx)
		}()
		fmt.Println("[启动] 冲突解决代理已启动")
	}

	// Step 7: 启动调查代理
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Explorer.Run(ctx)
	}()
	fmt.Println("[启动] 调查代理已启动")

	// Step 8: 启动执行代理
	for _, wk := range s.Workers {
		wk := wk // 闭包捕获
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			wk.Run(ctx)
		}()
	}
	fmt.Printf("[启动] 执行代理已启动 (%d 个)\n", len(s.Workers))

	fmt.Println("[启动] 系统就绪，等待用户输入")
}

// RunCLI 启动 CLI 主循环，阻塞直到用户退出或 ctx 取消。
func (s *System) RunCLI(ctx context.Context, reader io.Reader, writer io.Writer) {
	s.CLI = cli.New(s.Store, s.EventCh, s.cancel, s.Scheduler, s.MailboxRegistry, s.ApprovalCh, reader, writer)
	s.CLI.Run(ctx)
}

// Shutdown 优雅关闭所有服务。
func (s *System) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.WorktreeManager != nil {
		s.WorktreeManager.CleanupAll()
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
