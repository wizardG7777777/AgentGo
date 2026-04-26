package hashline

import (
	"fmt"
	"strconv"
	"strings"
)

// LineRef 表示一个解析后的行哈希引用。
type LineRef struct {
	Line int
	Hash string
}

// ParseLineRef 宽容解析行哈希引用字符串。
//
// 归一化步骤（严格顺序）：
//  1. 整体 trim
//  2. 剥前缀：^(?:>>>|[+-])\s* （>>>、+、- 加可选空白）—— parseStripPrefixRe
//  3. # 周围空白规范化：\s*#\s* → "#" —— parseHashSpaceRe
//  4. 剥尾巴：\|.*$ （剥 "|content..."）—— parseStripContentRe
//  5. 再 trim
//  6. 严格匹配 HashLineRefRegex 即返回
//  7. 否则在剩余字符串中找 lineRefExtractPattern 的第一个匹配子串
//  8. 若仍失败但形如 "name#VK"（左侧非数字、右侧合法 hash），返回特定错误 —— parseNameHashRe
//
// 所有正则均为 constants.go 包级 var；本函数体不再编译正则。
func ParseLineRef(ref string) (LineRef, error) {
	s := strings.TrimSpace(ref)

	// 2-4. 剥前缀 + 规范化 # 周围空白 + 剥尾巴
	s = parseStripPrefixRe.ReplaceAllString(s, "")
	s = parseHashSpaceRe.ReplaceAllString(s, "#")
	s = parseStripContentRe.ReplaceAllString(s, "")

	// 5. 再 trim
	s = strings.TrimSpace(s)

	// 6. 严格匹配
	if m := HashLineRefRegex.FindStringSubmatch(s); m != nil {
		// m[1] 已被正则 [0-9]+ 限定为纯数字串，Atoi 不会失败
		line, _ := strconv.Atoi(m[1])
		return LineRef{Line: line, Hash: m[2]}, nil
	}

	// 7. 在剩余字符串中提取第一个 LINE#HASH 子串
	if m := lineRefExtractPattern.FindStringSubmatch(s); m != nil {
		inner := m[1]
		if mm := HashLineRefRegex.FindStringSubmatch(inner); mm != nil {
			line, _ := strconv.Atoi(mm[1])
			return LineRef{Line: line, Hash: mm[2]}, nil
		}
	}

	// 8. 检测 "name#VK" 模式（左侧非数字）给特定错误
	if parseNameHashRe.MatchString(s) {
		return LineRef{}, fmt.Errorf("'%s' 左侧不是行号（应为 数字#哈希 格式，如 42#VK）", s)
	}

	return LineRef{}, fmt.Errorf("无法解析行哈希引用: %q", ref)
}

// StripHashPrefix 在 old_str / new_str / lines 字段值上剥去可能的 hashline 前缀或 diff + 前缀。
//
// 算法：
//  1. 按 \n 切行，跳过空行统计 nonEmpty
//  2. 计 hashCount = 命中 HashlinePrefixRegex 的非空行数
//     计 plusCount = 命中 DiffPlusRegex 的非空行数
//  3. stripHash = hashCount > 0 && hashCount >= nonEmpty * 0.5
//     stripPlus = !stripHash && plusCount > 0 && plusCount >= nonEmpty * 0.5
//     hashline 前缀优先于 diff +
//  4. 命中阈值后对每行 regex.ReplaceAll（不带前缀的行原样保留）
//  5. nonEmpty == 0 或两个阈值都不命中 → 原样返回
func StripHashPrefix(text string) string {
	lines := strings.Split(text, "\n")

	// 1. 统计非空行数
	nonEmpty := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmpty++
		}
	}
	if nonEmpty == 0 {
		return text
	}

	// 2. 计数
	hashCount := 0
	plusCount := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if HashlinePrefixRegex.MatchString(line) {
			hashCount++
		} else if isDiffPlusLine(line) {
			plusCount++
		}
	}

	// 3. 阈值判断（hashline 优先）
	threshold := float64(nonEmpty) * 0.5
	stripHash := hashCount > 0 && float64(hashCount) >= threshold
	stripPlus := !stripHash && plusCount > 0 && float64(plusCount) >= threshold

	if !stripHash && !stripPlus {
		return text
	}

	// 4. 逐行替换
	out := make([]string, len(lines))
	for i, line := range lines {
		if stripHash {
			out[i] = HashlinePrefixRegex.ReplaceAllString(line, "")
		} else {
			// 剥 diff + 前缀
			out[i] = strings.TrimPrefix(line, "+")
		}
	}
	return strings.Join(out, "\n")
}

// isDiffPlusLine 判断一行是否是 diff 格式的 "+" 前缀行（排除 "++" 双加号）。
func isDiffPlusLine(line string) bool {
	return strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "++")
}
