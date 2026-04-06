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
func ValidatePath(path, projectRoot string) (string, error) {
	if projectRoot == "" {
		// 未配置限制
		return path, nil
	}

	// 解析为绝对路径
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("路径解析���败: %w", err)
	}
	absPath = filepath.Clean(absPath)

	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("项目根目录解析失败: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

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
