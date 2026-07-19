//go:build windows

package tools

import (
	"context"
	"os/exec"
	"time"
)

// Windows 保留 CommandContext 的直接进程终止语义；WaitDelay 避免被派生进程
// 继承的 stdout/stderr 管道让 CombinedOutput 在超时后无限等待。
func newShellExecCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd
}
