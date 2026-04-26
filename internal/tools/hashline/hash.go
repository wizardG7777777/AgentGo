package hashline

import (
	"hash/crc32"
	"strings"
	"unicode"
)

// ComputeLineHash 返回给定行号和内容的 2 字符哈希。
//
// 规范化：
//   - 去掉 \r
//   - TrimRight 尾部空白（空格、\t、\n、\r 等）
//   - 行内 TAB/SPACE 不规范化
//
// seed 规则：
//   - 若 stripped 内容中含有任何 unicode.IsLetter 或 unicode.IsDigit → seed = 0
//   - 完全无字母数字（纯空白、纯标点如 "}"、"---"）→ seed = lineNumber
//
// 算法：crc32.Update(uint32(seed), crc32.IEEETable, []byte(stripped)) % 256 → DictStr 两位
func ComputeLineHash(lineNumber int, content string) string {
	stripped := strings.ReplaceAll(content, "\r", "")
	stripped = strings.TrimRightFunc(stripped, unicode.IsSpace)

	seed := uint32(0)
	if !hasAnyLetterOrDigit(stripped) {
		seed = uint32(lineNumber)
	}

	sum := crc32.Update(seed, crc32.IEEETable, []byte(stripped))
	b := byte(sum % 256)
	return string(DictStr[b>>4]) + string(DictStr[b&0x0F])
}

func hasAnyLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
