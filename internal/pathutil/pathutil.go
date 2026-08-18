package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const projectRulesRelativePath = ".agentgo/project_rules.yaml"

// SensitivePatterns 包含应该被阻止访问的敏感文件模式。
var SensitivePatterns = []string{
	".env",
	".ssh",
	"credentials",
	"id_rsa",
	"id_ed25519",
	".aws/credentials",
	".gitcredentials",
}

// CanonicalizeRoot 把项目根解析为真实、已存在的绝对目录。空根、不可读根、
// 非目录根一律报错，避免调用方把配置缺失静默解释为“不限制”。EvalSymlinks
// 同时统一 macOS /var → /private/var 等路径别名与显式 symlink 根。
func CanonicalizeRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("项目根目录不能为空")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("项目根目录解析失败: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(absRoot))
	if err != nil {
		return "", fmt.Errorf("项目根目录规范化失败: %w", err)
	}
	info, err := os.Stat(realRoot)
	if err != nil {
		return "", fmt.Errorf("项目根目录不可访问: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("项目根目录不是目录: %s", root)
	}
	return filepath.Clean(realRoot), nil
}

// ValidatePath 检查解析后的真实路径是否在允许的根目录内，并且不匹配敏感
// 文件模式或 AgentGo 安全控制文件。如果合法则返回 canonical 绝对路径。
//
// 路径解析规则：
//   - 绝对路径：直接 Clean 后解析 symlink / 路径别名
//   - 相对路径：以 projectRoot 为基准 Join，而非进程 CWD
//     这样即使 ProjectRoot 与进程 CWD 不一致，相对路径也能被一致解析
//   - 目标不存在：解析最近的现存祖先，再拼回尚不存在的尾部；因此根内新文件
//     仍可创建，而现存祖先 symlink 指向根外时会被拒绝
func ValidatePath(path, projectRoot string) (string, error) {
	absRoot, err := CanonicalizeRoot(projectRoot)
	if err != nil {
		return "", err
	}

	// 解析为绝对路径：相对路径以 projectRoot 为基准 Join，不依赖进程 CWD
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath = filepath.Clean(filepath.Join(absRoot, path))
	}
	realPath, err := canonicalizeTarget(absPath)
	if err != nil {
		return "", fmt.Errorf("路径 %s 规范化失败: %w", path, err)
	}

	// 检查真实路径是否在真实项目根内（使用 os.PathSeparator 防止前缀欺骗）。
	if !isWithinRoot(realPath, absRoot) {
		return "", fmt.Errorf("路径 %s 超出项目根目录 %s 的范围", path, projectRoot)
	}

	// 同时检查调用方给出的逻辑路径与解析后的真实路径，防止用无害 symlink
	// 名称别名访问 .env / credentials 等敏感目标。
	for _, candidate := range []string{absPath, realPath} {
		lowerPath := strings.ToLower(candidate)
		for _, pattern := range SensitivePatterns {
			if strings.Contains(lowerPath, strings.ToLower(pattern)) {
				return "", fmt.Errorf("拒绝访问敏感文件: %s (匹配模式: %s)", path, pattern)
			}
		}
	}

	// project_rules 决定下一次启动的 Shell 黑/灰名单，属于安全控制配置，
	// 不能通过普通 Agent 文件工具写入或读取后定向篡改。
	rel, err := filepath.Rel(absRoot, realPath)
	if err == nil && strings.EqualFold(filepath.ToSlash(rel), projectRulesRelativePath) {
		return "", fmt.Errorf("拒绝访问 AgentGo 安全控制文件: %s", path)
	}

	return realPath, nil
}

// canonicalizeTarget 解析目标的所有现存路径组件。目标尚不存在时向上寻找
// 最近现存祖先，EvalSymlinks 后再拼回尾部。Lstat 用于识别 dangling symlink：
// 它不是“普通不存在目标”，EvalSymlinks 失败必须 fail-closed。
func canonicalizeTarget(path string) (string, error) {
	current := filepath.Clean(path)
	missing := make([]string, 0, 4)
	for {
		_, err := os.Lstat(current)
		switch {
		case err == nil:
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		case !os.IsNotExist(err):
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("找不到可解析的现存祖先")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func isWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
