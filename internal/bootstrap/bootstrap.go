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
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/watchdog"
	"agentgo/internal/worker"
)

type System struct {
	Config         *config.Config
	Store          *store.MemoryTaskStore
	Roster         roster.Roster
	EventCh        chan model.Event
	Watchdog       *watchdog.Watchdog
	CancelRegistry *store.TaskCancelRegistry
	Scheduler      *scheduler.Scheduler
	Explorer       *explorer.Explorer
	Worker         *worker.Worker
	CLI            *cli.CLI
	cancel         context.CancelFunc
	wg             sync.WaitGroup
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
	sched := scheduler.New(taskStore, schedulerLLM, eventCh, cfg)

	// Step 6: 创建看门狗
	w := watchdog.New(taskStore, cfg, eventCh, r)

	// Step 7: 创建调查代理
	exp := explorer.New(taskStore, r, explorerLLM, cfg, cancelRegistry)

	// Step 8: 创建执行代理（使用主 LLM，认领 event_type="" 的执行任务）
	workerLLM := llm.NewSDKClient(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel,
		"", // system prompt 由 worker 内部管理
		time.Duration(cfg.LLMTimeoutSec)*time.Second,
	)
	wk := worker.New(taskStore, r, workerLLM, cfg, cancelRegistry)

	sys := &System{
		Config:         cfg,
		Store:          taskStore,
		Roster:         r,
		EventCh:        eventCh,
		Watchdog:       w,
		CancelRegistry: cancelRegistry,
		Scheduler:      sched,
		Explorer:       exp,
		Worker:         wk,
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

	// Step 7: 启动调查代理
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Explorer.Run(ctx)
	}()
	fmt.Println("[启动] 调查代理已启动")

	// Step 8: 启动执行代理
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Worker.Run(ctx)
	}()
	fmt.Println("[启动] 执行代理已启动")

	fmt.Println("[启动] 系统就绪，等待用户输入")
}

// RunCLI 启动 CLI 主循环，阻塞直到用户退出或 ctx 取消。
func (s *System) RunCLI(ctx context.Context, reader io.Reader, writer io.Writer) {
	s.CLI = cli.New(s.Store, s.EventCh, s.cancel, s.Scheduler, reader, writer)
	s.CLI.Run(ctx)
}

// Shutdown 优雅关闭所有服务。
func (s *System) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
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
