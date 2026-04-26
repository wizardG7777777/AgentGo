package hashline

import "regexp"

// DictStr 是 16 个低视觉歧义字母（无 0/O、1/l 等易混淆字符）。
// 两位组合 256 种对应 1 字节（crc32 mod 256）。
const DictStr = "ZPMQVRWSNKTXJBYH"

var (
	// HashLineRefRegex 匹配纯引用格式：42#VK
	HashLineRefRegex = regexp.MustCompile(`^([0-9]+)#([` + DictStr + `]{2})$`)

	// HashlinePrefixRegex 匹配行首的 hashline 前缀（含可选的 >>> 前缀和空白）。
	// 用于 StripHashPrefix 的识别。
	// 注：spec §7.3 草案曾写过 `(?:>>>|>>)?`，但 ValidateLineAnchorsHook 的错误消息
	// 只用三个尖括号（参见 validate_line_anchors.go:191），LLM 复制回来的也只会带 >>>，
	// `>>` 在实际语料中无产出方。2026-04-26 评审中删去 >> 分支以保持 ParseLineRef
	// 与本前缀正则的剥离规则一致（两侧都只识别 >>>）。
	HashlinePrefixRegex = regexp.MustCompile(`^\s*(?:>>>)?\s*\d+\s*#\s*[` + DictStr + `]{2}\|`)

	// lineRefExtractPattern 在自由文本中提取第一个 LINE#HASH 子串。
	lineRefExtractPattern = regexp.MustCompile(`([0-9]+#[` + DictStr + `]{2})`)

	// 以下 4 个正则供 ParseLineRef 使用，2026-04-26 评审中从函数体内提到包级
	// 避免每次调用都重新编译——ValidateLineAnchorsHook 按 anchor 列表循环调用 ParseLineRef，
	// 10 个 anchor 原本要编译 40 次。

	// parseStripPrefixRe 剥 ParseLineRef 输入的 >>>、+、- 前缀（含可选空白）。
	parseStripPrefixRe = regexp.MustCompile(`^(?:>>>|[+-])\s*`)

	// parseHashSpaceRe 把 # 周围的可选空白规范化掉（"42 # VK" → "42#VK"）。
	parseHashSpaceRe = regexp.MustCompile(`\s*#\s*`)

	// parseStripContentRe 剥掉 ParseLineRef 输入的 |content... 尾巴。
	parseStripContentRe = regexp.MustCompile(`\|.*$`)

	// parseNameHashRe 检测 "name#VK" 形态（左侧非数字）以给出特定错误消息。
	parseNameHashRe = regexp.MustCompile(`^([^0-9]+)#([` + DictStr + `]{2})$`)
)
