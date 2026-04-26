// Package suggest 是 §10 工具调用错误恢复（Did-You-Mean）的薄适配层。
//
// 算法委托给 [github.com/sahilm/fuzzy]：fzf 风格子序列匹配 + 连续/位置加权
// 打分。本子包只负责 (1) 候选构造约束、(2) 命中字符方括号高亮、(3) 输出
// 格式化。设计与边界详见 docs/activate/nextUpgrade_v4.md §10。
//
// 调用方：
//   - internal/tools/local_read.go 的 globSearch / readFile / listDir / grepSearch
//     空结果路径
//   - internal/agent 工具调度层捕获 "tool not found" 时
//
// 不在范围：tiktoken-style token 切分、语义相似度（embedding）、tool→tool 自动
// 切换建议。
package suggest

import (
	"strings"

	"github.com/sahilm/fuzzy"
)

// Suggest 对 candidates 按 fzf 风格子序列匹配打分，返回前 k 条按方括号高亮
// 命中字符的字符串。
//
//   - pattern: LLM 传入的查询（如 "Archtechture.md"）
//   - candidates: 当前查询范围内实际存在的实体（glob_search 的 Walk 结果 /
//     ReadDir 列表 / 已注册工具名）
//   - k: 最多返回几条（建议 3）
//
// 返回空切片表示无合理候选——sahilm/fuzzy 的 Find 内部已用子序列匹配语义自然
// 筛除"字符未全命中"的候选，MVP 不再叠加 minScore 阈值。调用方应据此判断是否
// 显示 "Did you mean" 段。
func Suggest(pattern string, candidates []string, k int) []string {
	if pattern == "" || len(candidates) == 0 || k <= 0 {
		return nil
	}
	matches := fuzzy.Find(pattern, candidates)
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > k {
		matches = matches[:k]
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, highlightMatch(m))
	}
	return out
}

// highlightMatch 用 fuzzy.Match.MatchedIndexes 在每段连续命中字符两侧加 [...]
// 方括号。超长路径（>80 字符）时在首个命中字符之前 / 末个命中字符之后做 ...
// 截断，绝不在命中段内省略——高亮完整性优先于长度省略，详见 §10.5。
//
// 不用 *...* / **...**：前者是 markdown 斜体、后者是粗体，与候选字面字符歧义
// 大；方括号语义明确、与文件名字符冲突概率低。
func highlightMatch(m fuzzy.Match) string {
	const maxLen = 80
	const ellipsis = "..."

	if len(m.MatchedIndexes) == 0 {
		return m.Str
	}

	// Step 1: 在原始字符串上插入方括号——按字节索引（fuzzy.Match.MatchedIndexes
	// 是 byte index，不是 rune index）。
	var sb strings.Builder
	prev := -2 // 上一个匹配字节的索引，初始 -2 让首段判断为"非连续"
	open := false
	for i := 0; i < len(m.Str); i++ {
		isMatch := contains(m.MatchedIndexes, i)
		if isMatch && !open {
			sb.WriteByte('[')
			open = true
		} else if !isMatch && open {
			sb.WriteByte(']')
			open = false
		}
		_ = prev
		sb.WriteByte(m.Str[i])
	}
	if open {
		sb.WriteByte(']')
	}
	highlighted := sb.String()

	// Step 2: 长度控制——若超过 maxLen，需在首个命中之前 / 末个命中之后截断。
	// MatchedIndexes 是原串字节坐标；我们截断的是 highlighted（含 [ ] 字符），
	// 要把坐标映射过去：因每个 [ 或 ] 都让其后内容向后偏移 1 字节。
	if len(highlighted) <= maxLen {
		return highlighted
	}
	first := m.MatchedIndexes[0]
	last := m.MatchedIndexes[len(m.MatchedIndexes)-1]
	// highlighted 中 first 对应位置 = first + 已插入的左方括号数（仅在 first 之前出现的）
	// 简化：每段连续命中前出现一个 [，每段后一个 ]。我们就用一个保守估算：
	// 直接根据 [ ] 在 highlighted 中重新定位首个 [ 与末个 ] 即可。
	firstBracket := strings.Index(highlighted, "[")
	lastBracket := strings.LastIndex(highlighted, "]")
	if firstBracket < 0 || lastBracket < 0 {
		// 不应发生，但安全降级
		_ = first
		_ = last
		return highlighted[:maxLen]
	}

	// 命中段（从首个 [ 到末个 ]）必须完整保留
	matchSegment := highlighted[firstBracket : lastBracket+1]
	if len(matchSegment) >= maxLen {
		// 命中段本身就超长——返回原段加首尾省略
		return ellipsis + matchSegment + ellipsis
	}

	// 剩余字符预算分配给两端上下文
	remaining := max(maxLen-len(matchSegment)-2*len(ellipsis), 0)
	leftBudget := remaining / 2
	rightBudget := remaining - leftBudget

	leftCtx := highlighted[:firstBracket]
	rightCtx := highlighted[lastBracket+1:]

	leftStart := 0
	leftPrefix := ""
	if len(leftCtx) > leftBudget {
		leftStart = len(leftCtx) - leftBudget
		leftPrefix = ellipsis
	}
	rightEnd := len(rightCtx)
	rightSuffix := ""
	if len(rightCtx) > rightBudget {
		rightEnd = rightBudget
		rightSuffix = ellipsis
	}

	return leftPrefix + leftCtx[leftStart:] + matchSegment + rightCtx[:rightEnd] + rightSuffix
}

// contains 是 sort.SearchInts 的轻量替代——MatchedIndexes 通常 < 20 个，线性扫够用。
func contains(ints []int, target int) bool {
	for _, v := range ints {
		if v == target {
			return true
		}
		if v > target {
			return false // MatchedIndexes 是有序的
		}
	}
	return false
}

// FormatForToolMessage 把 Suggest 返回的高亮候选列表格式化为
//
//	Did you mean:
//	  - Arch[i]tecture.md
//	  - [arch]itecture_v3.md
//
// 形式的文本段。空切片返回空串，调用方据此判断是否追加。
func FormatForToolMessage(highlighted []string) string {
	if len(highlighted) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nDid you mean:\n")
	for _, h := range highlighted {
		sb.WriteString("  - ")
		sb.WriteString(h)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
