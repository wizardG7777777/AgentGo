package hashline

import (
	"strings"
	"testing"
)

// FuzzComputeLineHash_Stability 验证：相同 lineNumber + 相同 normalized content → 相同哈希。
func FuzzComputeLineHash_Stability(f *testing.F) {
	f.Add(1, "hello world")
	f.Add(42, "")
	f.Add(100, "  }\r\n")
	f.Add(5, "package main\n")
	f.Fuzz(func(t *testing.T, line int, content string) {
		if line < 1 {
			t.Skip("line number < 1")
		}
		h1 := ComputeLineHash(line, content)
		h2 := ComputeLineHash(line, content)
		if h1 != h2 {
			t.Errorf("instability: line=%d content=%q: %q vs %q", line, content, h1, h2)
		}
	})
}

// FuzzFormatHashLineRoundTrip 验证 FormatHashLine 的输出能被 ParseLineRef 正确解析。
func FuzzFormatHashLineRoundTrip(f *testing.F) {
	f.Add(1, "hello")
	f.Add(99, "")
	f.Add(1000, "func foo() {\n}")
	f.Fuzz(func(t *testing.T, line int, content string) {
		if line < 1 {
			t.Skip("line number < 1")
		}
		formatted := FormatHashLine(line, content)
		// formatted = "N#HH|content"，ParseLineRef 应该能解析出 line 和 hash
		ref, err := ParseLineRef(formatted)
		if err != nil {
			t.Fatalf("ParseLineRef(%q) error: %v", formatted, err)
		}
		if ref.Line != line {
			t.Errorf("Line = %d, want %d", ref.Line, line)
		}
		if len(ref.Hash) != 2 {
			t.Errorf("Hash length = %d, want 2", len(ref.Hash))
		}
		// 重算哈希应匹配
		actual := ComputeLineHash(line, content)
		if ref.Hash != actual {
			t.Errorf("Hash mismatch: parsed=%q recomputed=%q", ref.Hash, actual)
		}
		// content 部分应保留
		parts := strings.SplitN(formatted, "|", 2)
		if len(parts) != 2 || parts[1] != content {
			t.Errorf("Content part wrong: %q", formatted)
		}
	})
}
