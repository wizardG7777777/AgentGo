package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

// ValidatePath 检查解析后的路径是否在允许的根目录内，
// 并且不匹配敏感文件模式。
// 如果合法则返回清理后的绝对路径，否则返回错误。
//
// 路径解析规则：
//   - 绝对路径：直接 Clean 后做前缀校验
//   - 相对路径：以 projectRoot 为基准 Join，而非进程 CWD
//     这样即使 ProjectRoot 与进程 CWD 不一致，相对路径也能被一致解析
func ValidatePath(path, projectRoot string) (string, error) {
	if projectRoot == "" {
		// 未配置限制
		return path, nil
	}

	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("项目根目录解析失败: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	// 解析为绝对路径：相对路径以 projectRoot 为基准 Join，不依赖进程 CWD
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath = filepath.Clean(filepath.Join(absRoot, path))
	}

	// 检查路径是否在项目根目录内（使用 os.PathSeparator 防止前缀欺骗）
	if !strings.HasPrefix(absPath, absRoot+string(os.PathSeparator)) && absPath != absRoot {
		return "", fmt.Errorf("路径 %s 超出项目根目录 %s 的范围", path, projectRoot)
	}

	// 检查敏感文件模式
	lowerPath := strings.ToLower(absPath)
	for _, pattern := range SensitivePatterns {
		if strings.Contains(lowerPath, strings.ToLower(pattern)) {
			return "", fmt.Errorf("拒绝访问敏感文件: %s (匹配模式: %s)", path, pattern)
		}
	}

	return absPath, nil
}
