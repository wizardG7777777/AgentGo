package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestGlobSearch_DefaultRootUsesCurrentWorkdir
// 期望：当未提供 root_dir 时，glob_search 应以当前 workdir（worktree/fallback）为根目录。
// 若实现退回到 "."，该测试会失败，暴露隔离目录泄漏风险。
func TestGlobSearch_DefaultRootUsesCurrentWorkdir(t *testing.T) {
	tmpRoot := t.TempDir()
	uniqueName := "wt_" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".go"
	targetFile := filepath.Join(tmpRoot, uniqueName)
	if err := os.WriteFile(targetFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write target file failed: %v", err)
	}

	holder := &currentWorkdirHolder{fallback: tmpRoot}
	tool := makeGlobSearchTool(holder)

	out, err := tool(context.Background(), map[string]any{
		"pattern": uniqueName,
	})
	if err != nil {
		t.Fatalf("glob_search returned error: %v", err)
	}
	if !strings.Contains(out, uniqueName) {
		t.Fatalf("glob_search should find %q under current workdir, got: %s", uniqueName, out)
	}
}

// TestMakeGlobSearchTool_ShouldSetRootDirWhenMissing_StaticContract
// 静态约束：当请求未给 root_dir 时，makeGlobSearchTool 应把 root_dir 显式设置为当前 workdir。
// 若缺少这段逻辑，glob_search 的默认根目录会退回到 "."，与 worktree 隔离目标不一致。
func TestMakeGlobSearchTool_ShouldSetRootDirWhenMissing_StaticContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	srcPath := filepath.Join(filepath.Dir(thisFile), "worker.go")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read worker.go failed: %v", err)
	}
	src := string(data)

	// 检查 makeGlobSearchTool 在 root_dir 缺失时注入 workdir
	hasMissingRootFallback := strings.Contains(src, `args["root_dir"]`) &&
		(strings.Contains(src, "workdir.Get()") || strings.Contains(src, "wd :="))
	if !hasMissingRootFallback {
		t.Fatalf("makeGlobSearchTool should set root_dir to current workdir when root_dir is missing")
	}
}

// TestWorker_OnTaskEnd_ResolverDoneChErrorShouldBeHandled_StaticContract
// 静态约束：OnTaskEnd 在 <-doneCh 后应检查 error，而不是忽略结果。
func TestWorker_OnTaskEnd_ResolverDoneChErrorShouldBeHandled_StaticContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	srcPath := filepath.Join(filepath.Dir(thisFile), "worker.go")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read worker.go failed: %v", err)
	}
	src := string(data)

	// 至少要出现某种“读取 doneCh 并判断 err”的代码形态。
	hasErrCheck := strings.Contains(src, "if err := <-doneCh; err != nil") ||
		strings.Contains(src, "resolverErr := <-doneCh") ||
		strings.Contains(src, "err = <-doneCh")
	if !hasErrCheck {
		t.Fatalf("worker OnTaskEnd should check resolver doneCh error, but no error-handling pattern was found")
	}
}
