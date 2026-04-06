package pathutil

import (
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
