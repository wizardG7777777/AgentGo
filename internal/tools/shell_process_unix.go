//go:build !windows

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// newShellExecCommand 将 shell 及其派生进程放入独立进程组。
//
// exec.CommandContext 默认只终止直接启动的 shell；若 shell 已经派生了子进程，
// 子进程仍会持有 stdout/stderr 管道，CombinedOutput 会一直等到它退出。对整个
// 进程组发送 SIGKILL，才能让 timeout 同时终止命令树并及时关闭管道。
func newShellExecCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// 即使某个后代主动脱离进程组并继续持有管道，也要保证等待有界。
	cmd.WaitDelay = time.Second
	return cmd
}
