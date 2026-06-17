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
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[错误] 无法获取当前工作目录: %v\n", err)
			os.Exit(1)
		}
		traceDir := resolveTraceDir(cwd)
		if err := trace.CLI(os.Args[2:], traceDir, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
			os.Exit(1)
		}
		return
	}

	configPath := flag.String("config", "setting.yaml", "配置文件路径")
	skipStartupProbe := flag.Bool("skip-startup-probe", false, "跳过启动期 TCP probe（等价于 startup_probe: off）")
	resumeSessionID := flag.String("resume", "", "恢复指定 Session（完整 ID 或唯一前缀）")
	flag.Parse()

	// 判断用户是否显式指定了 -config
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})

	sys, err := bootstrap.BootstrapWithOptions(*configPath, explicit, bootstrap.BootstrapOptions{
		SkipStartupProbe: *skipStartupProbe,
		ResumeSessionID:  *resumeSessionID,
	})
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

// resolveTraceDir 解析 trace 子命令的日志目录。
// 优先使用当前活跃 Session 的 logs/ 目录，与 bootstrap.go 的重定向保持一致；
// 读不到 active-session 或目录不存在时，回退到旧路径 .agentgo/traces。
func resolveTraceDir(cwd string) string {
	sessionsDir := filepath.Join(cwd, ".agentgo", "sessions")
	activeFile := filepath.Join(sessionsDir, "active-session")
	if data, err := os.ReadFile(activeFile); err == nil && len(data) > 0 {
		sessionID := string(data)
		logsDir := filepath.Join(sessionsDir, "sess-"+sessionID, "logs")
		if info, statErr := os.Stat(logsDir); statErr == nil && info.IsDir() {
			return logsDir
		}
	}
	return filepath.Join(cwd, ".agentgo", "traces")
}
