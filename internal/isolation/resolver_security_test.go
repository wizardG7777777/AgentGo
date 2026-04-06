package isolation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolverReadFileTool_RejectsAbsolutePathOutsideRepoRoot
// 期望：resolver 的 read_file 只能访问 repoRoot 范围内文件。
// 当前若允许绝对路径越界读取，该测试会失败。
func TestResolverReadFileTool_RejectsAbsolutePathOutsideRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatalf("write secret file failed: %v", err)
	}

	tool := makeResolverReadFileTool(repoRoot)
	_, err := tool(context.Background(), map[string]any{
		"path": secretPath, // 绝对路径，且在 repoRoot 外
	})
	if err == nil {
		t.Fatalf("expected read_file to reject absolute path outside repoRoot, but it succeeded")
	}
	if !strings.Contains(err.Error(), "repo") &&
		!strings.Contains(err.Error(), "根目录") &&
		!strings.Contains(err.Error(), "超出") {
		t.Fatalf("expected sandbox/path-boundary related error, got: %v", err)
	}
}

func TestWorktreeManager_PathUsesShortTaskID(t *testing.T) {
	root := t.TempDir()
	mgr := NewWorktreeManager(root)

	taskID := "12345678-abcdefgh"
	p := mgr.Path(taskID)
	if !strings.Contains(p, ".worktrees") {
		t.Fatalf("path should include .worktrees, got: %s", p)
	}
	if !strings.HasSuffix(p, "12345678") {
		t.Fatalf("path should end with short task id, got: %s", p)
	}
}
