package bootstrap

import (
	"context"
	"fmt"
	"log"
	"sync"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/watchdog"
)

type System struct {
	Config   *config.Config
	Store    store.TaskStore
	Roster   roster.Roster
	EventCh  chan model.Event
	Watchdog *watchdog.Watchdog
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func Bootstrap(configPath string) (*System, error) {
	// Step 1: Load config
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	fmt.Println("[启动] 全局配置加载完成")

	// Step 2: Initialize bulletin board
	eventCh := make(chan model.Event, cfg.EventChannelBuffer)
	taskStore := store.NewMemoryTaskStore(eventCh, cfg.FIFOLimit, cfg.DefaultConcurrency, cfg.DefaultTimeoutSec)
	fmt.Println("[启动] 公告板初始化完成")

	// Step 3: Initialize roster
	r := roster.NewMemoryRoster()
	fmt.Println("[启动] 花名册初始化完成")

	// Step 4: Create watchdog
	w := watchdog.New(taskStore, cfg, eventCh)

	sys := &System{
		Config:   cfg,
		Store:    taskStore,
		Roster:   r,
		EventCh:  eventCh,
		Watchdog: w,
	}

	return sys, nil
}

func (s *System) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)

	// Start watchdog with auto-restart
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWatchdogWithRecover(ctx)
	}()
	fmt.Println("[启动] 看门狗已启动")

	fmt.Println("[启动] 系统就绪")
}

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
	}
}
