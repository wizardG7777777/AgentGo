package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"agentgo/internal/bootstrap"
)

func main() {
	sys, err := bootstrap.Bootstrap("setting.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[错误] 启动失败: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys.Start(ctx)

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\n[关闭] 收到停止信号，正在关闭...")
	sys.Shutdown()
}
