package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"agentgo/internal/bootstrap"
	"agentgo/internal/config"
	"agentgo/internal/session"
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
		graphStateDir := filepath.Join(cwd, ".agentgo", "state", "graphs")
		if err := trace.CLI(os.Args[2:], traceDir, graphStateDir, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 子命令路由：config 族（当前含 doctor）只做配置静态检查，不启动主系统
	if len(os.Args) >= 2 && os.Args[1] == "config" {
		os.Exit(config.CLI(os.Args[2:], os.Stdout, os.Stderr))
	}

	// eval 子命令及其独立开发工具均已删除。这里继续显式拒绝，避免 "eval"
	// 被当作游离参数而静默启动主系统。
	if len(os.Args) >= 2 && os.Args[1] == "eval" {
		fmt.Fprintln(os.Stderr, "[错误] eval 子命令已删除；项目不再提供内置行为评测工具")
		os.Exit(2)
	}

	configPath := flag.String("config", "setting.yaml", "配置文件路径")
	skipStartupProbe := flag.Bool("skip-startup-probe", false, "跳过启动期 TCP probe（等价于 startup_probe: off）")
	resumeSessionID := flag.String("resume", "", "进入指定 Session（完整 ID 或唯一前缀；不自动续跑任务，需提交新提示词继续）")
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
	if err := sys.Start(ctx, cancel); err != nil {
		if shutdownErr := sys.Shutdown(); shutdownErr != nil {
			fmt.Fprintf(os.Stderr, "[错误] 启动失败后的关闭不完整: %v\n", shutdownErr)
		}
		fmt.Fprintf(os.Stderr, "[错误] 启动失败: %v\n", err)
		os.Exit(1)
	}

	// CLI 阻塞 main goroutine，/quit 或 stdin 关闭时返回
	sys.RunCLI(ctx)

	// CLI 退出后关闭所有服务
	if err := sys.Shutdown(); err != nil {
		fmt.Fprintf(os.Stderr, "[错误] 系统关闭不完整: %v\n", err)
	}
}

// resolveTraceDir 解析 trace 子命令的日志目录。
// 优先使用当前活跃 Session 的 logs/ 目录，与 bootstrap.go 的重定向保持一致；
// 读不到 active-session 或目录不存在时，回退到旧路径 .agentgo/traces。
// session 目录布局知识由 session.ActiveSessionLogsDir 统一出口（B5 收敛）。
func resolveTraceDir(cwd string) string {
	sessionsDir := filepath.Join(cwd, ".agentgo", "sessions")
	if logsDir := session.ActiveSessionLogsDir(sessionsDir); logsDir != "" {
		return logsDir
	}
	return filepath.Join(cwd, ".agentgo", "traces")
}
