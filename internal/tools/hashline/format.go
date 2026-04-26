package hashline

import (
	"fmt"
	"strings"
)

// FormatHashLine 返回单行带哈希前缀的格式："N#HH|content"。
func FormatHashLine(lineNumber int, content string) string {
	return fmt.Sprintf("%d#%s|%s", lineNumber, ComputeLineHash(lineNumber, content), content)
}

// FormatHashLines 对整段内容按行切分，从 startLine 起每行附加哈希前缀。
// 行之间用 "\n" 连接，保留原始内容的尾部换行语义。
func FormatHashLines(startLine int, content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = FormatHashLine(startLine+i, line)
	}
	return strings.Join(lines, "\n")
}
