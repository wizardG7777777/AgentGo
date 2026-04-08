package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"agentgo/internal/bootstrap"
	"agentgo/internal/trace"
)

func main() {
	// 子命令路由：第一个非 flag 参数若是 "trace"，进入 trace CLI 而不启动主系统
	if len(os.Args) >= 2 && os.Args[1] == "trace" {
		// 默认 trace 目录：当前工作目录下的 .agentgo/traces
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[错误] 无法获取当前工作目录: %v\n", err)
			os.Exit(1)
		}
		traceDir := filepath.Join(cwd, ".agentgo", "traces")
		if err := trace.CLI(os.Args[2:], traceDir, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
			os.Exit(1)
		}
		return
	}

	configPath := flag.String("config", "setting.yaml", "配置文件路径")
	flag.Parse()

	// 判断用户是否显式指定了 -config
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})

	sys, err := bootstrap.Bootstrap(*configPath, explicit)
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
