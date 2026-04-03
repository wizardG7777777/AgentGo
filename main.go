package main

import (
	"context"
	"fmt"
	"os"

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

	// 启动后台服务（调度器、看门狗、调查代理）
	sys.Start(ctx, cancel)

	// CLI 阻塞 main goroutine，/quit 或 stdin 关闭时返回
	sys.RunCLI(ctx, os.Stdin, os.Stdout)

	// CLI 退出后关闭所有服务
	sys.Shutdown()
}
