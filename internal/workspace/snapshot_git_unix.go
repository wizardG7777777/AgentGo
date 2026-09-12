//go:build !windows

package workspace

import "os/exec"

func hideSnapshotGitWindow(cmd *exec.Cmd) {}
