package workspace

import (
	"os/exec"
	"syscall"
)

func hideSnapshotGitWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
