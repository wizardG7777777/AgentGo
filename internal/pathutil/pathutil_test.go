package pathutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePath_WithinRoot(t *testing.T) {
	root := filepath.Join("/", "project")
	path := filepath.Join("/", "project", "src", "main.go")

	result, err := ValidatePath(path, root)
	if err != nil {
		t.Fatalf("期望路径合法，但得到错误: %v", err)
	}
	if !filepath.IsAbs(result) {
		t.Errorf("期望绝对路径，得到: %s", result)
	}
}

func TestValidatePath_OutsideRoot(t *testing.T) {
	root := filepath.Join("/", "project")
	path := filepath.Join("/", "etc", "passwd")

	_, err := ValidatePath(path, root)
	if err == nil {
		t.Fatal("期��返回错误，但路径被允许")
	}
	if !strings.Contains(err.Error(), "超出项目根目录") {
		t.Errorf("期望包含 '超出项目根目录' 的错误，得到: %v", err)
	}
}

func TestValidatePath_TraversalAttack(t *testing.T) {
	root := filepath.Join("/", "project")
	path := filepath.Join("/", "project", "..", "etc", "passwd")

	_, err := ValidatePath(path, root)
	if err == nil {
		t.Fatal("期望路径遍历攻击被阻止，但路径被允许")
	}
	if !strings.Contains(err.Error(), "超出项目根目录") {
		t.Errorf("期望包含 '超出项目根目录' 的错误，得到: %v", err)
	}
}

func TestValidatePath_SensitiveFile(t *testing.T) {
	root := filepath.Join("/", "project")
	path := filepath.Join("/", "project", ".env")

	_, err := ValidatePath(path, root)
	if err == nil {
		t.Fatal("期望敏感文件被阻止，但路径被允许")
	}
	if !strings.Contains(err.Error(), "拒绝访问敏感文件") {
		t.Errorf("期望包含 '拒绝访问敏感文件' 的错误，得到: %v", err)
	}
}

func TestValidatePath_EmptyRoot(t *testing.T) {
	path := filepath.Join("/", "etc", "passwd")

	result, err := ValidatePath(path, "")
	if err != nil {
		t.Fatalf("空根目录应允许任何路径，但得到错误: %v", err)
	}
	if result != path {
		t.Errorf("期望原样返回路径 %s，得到: %s", path, result)
	}
}

func TestValidatePath_SshKey(t *testing.T) {
	root := filepath.Join("/", "project")
	path := filepath.Join("/", "project", ".ssh", "id_rsa")

	_, err := ValidatePath(path, root)
	if err == nil {
		t.Fatal("期望 SSH 密钥文件被阻止，但路径被允许")
	}
	if !strings.Contains(err.Error(), "拒绝访问敏感文件") {
		t.Errorf("期望包含 '拒绝访问敏感文件' 的错误，得到: %v", err)
	}
}

func TestValidatePath_RootItself(t *testing.T) {
	root := filepath.Join("/", "project")

	result, err := ValidatePath(root, root)
	if err != nil {
		t.Fatalf("项目根目录本身应该合法，但得到错误: %v", err)
	}
	if !filepath.IsAbs(result) {
		t.Errorf("期望绝对路径，得到: %s", result)
	}
}

// TestValidatePath_RelativeJoinedAgainstProjectRoot 验证相对路径以 projectRoot
// 为基准 Join，与进程 CWD 无关。
//
// 这是 2026-04-08 修复的回归保护——历史上 filepath.Abs 把相对路径解析为
// 相对**进程 CWD**而非传入的 projectRoot，导致非默认根目录场景失效。
func TestValidatePath_RelativeJoinedAgainstProjectRoot(t *testing.T) {
	// 用 t.TempDir 作为 projectRoot，它必然不是当前进程的 CWD
	root := t.TempDir()
	subDir := filepath.Join(root, "docs", "foo")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// 传入相对路径 "docs/foo"，应该被解析到 root 下
	result, err := ValidatePath("docs/foo", root)
	if err != nil {
		t.Fatalf("相对路径应被 join 到 projectRoot 下，但得到错误: %v", err)
	}

	expected, _ := filepath.Abs(filepath.Join(root, "docs", "foo"))
	if result != expected {
		t.Errorf("期望解析为 %s，实际: %s", expected, result)
	}
}

// TestValidatePath_AbsolutePathStillChecked 验证绝对路径仍然走前缀校验，
// 不会因修复相对路径解析而绕过 SSRF/sandbox 保护。
func TestValidatePath_AbsolutePathStillChecked(t *testing.T) {
	root := t.TempDir()

	// 传入项目根目录之外的绝对路径，应被拒绝
	_, err := ValidatePath("/etc/passwd", root)
	if err == nil {
		t.Errorf("绝对路径 /etc/passwd 不在 projectRoot 内，应被拒绝")
	}
	if err != nil && !filepath.IsAbs("/etc/passwd") {
		t.Errorf("不应只对相对路径生效")
	}
}

// TestValidatePath_NestedSubrootScenario 模拟"projectRoot 不是进程 CWD"场景：
// 进程 CWD 是 /tmp，projectRoot 是 /tmp/<some>/<nested>。
// 传入相对路径 "docs/activate" 应该解析到 nested 路径下。
func TestValidatePath_NestedSubrootScenario(t *testing.T) {
	mainRoot := t.TempDir()
	nestedRoot := filepath.Join(mainRoot, "sub", "nested")
	target := filepath.Join(nestedRoot, "docs", "activate")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := ValidatePath("docs/activate", nestedRoot)
	if err != nil {
		t.Fatalf("嵌套 projectRoot 下相对路径应被接受，但被拒绝: %v", err)
	}
	if !strings.Contains(result, "nested") {
		t.Errorf("期望路径包含 nested，实际: %s", result)
	}
}
