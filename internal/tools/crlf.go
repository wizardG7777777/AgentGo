package tools

import "strings"

// normalizeCRLF 把 CRLF / 孤立 CR 统一为 LF，返回归一化后的内容与是否发生过变化。
// read_file 展示层统一调用：LLM 看不到不可见的 \r，edit_file 构造 old_str 时
// 不会被行尾差异绊倒（2026-07-21 跨平台排查 M4）。磁盘上的文件字节与 SHA256
// 不受影响——归一化只发生在对 LLM 的展示边界。
func normalizeCRLF(s string) (string, bool) {
	if !strings.Contains(s, "\r") {
		return s, false
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), true
}

// isFullCRLF 报告内容里每个 \n 是否都属于 \r\n（即无孤立 LF）。
// edit_file 的 CRLF 重试只对全量 CRLF 文件做 \n→\r\n 逆变换，保证无损往返；
// 混合行尾文件不做逆变换，避免把孤立 LF 行误改成 CRLF。
func isFullCRLF(s string) bool {
	return strings.Contains(s, "\r\n") && strings.Count(s, "\n") == strings.Count(s, "\r\n")
}
