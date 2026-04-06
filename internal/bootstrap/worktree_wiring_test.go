package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBootstrap_WorktreeWiring_VerifyEnabledPathIsImplemented
// 该测试是“静态接线约束”：
// 当配置包含 worktree_enabled 时，Bootstrap 应该显式接线：
// 1) 按 cfg.WorktreeEnabled 创建 WorktreeManager
// 2) 创建 ConflictResolver
// 3) Start 启动 resolver.Run
// 4) Shutdown 调用 CleanupAll
//
// 当前实现若缺任一接线点，该测试会失败，提示实现尚未真正打通。
func TestBootstrap_WorktreeWiring_VerifyEnabledPathIsImplemented(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	srcPath := filepath.Join(filepath.Dir(thisFile), "bootstrap.go")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read bootstrap.go failed: %v", err)
	}
	src := string(data)

	requiredSnippets := []string{
		"cfg.WorktreeEnabled",
		"isolation.NewWorktreeManager",
		"isolation.NewConflictResolver",
		"ConflictResolver.Run",
		"CleanupAll",
	}
	for _, s := range requiredSnippets {
		if !strings.Contains(src, s) {
			t.Errorf("bootstrap wiring missing snippet: %q", s)
		}
	}
}
