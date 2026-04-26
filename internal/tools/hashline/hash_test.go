package hashline

import (
	"strings"
	"testing"
)

func TestComputeLineHash_Stability(t *testing.T) {
	cases := []struct {
		line    int
		content string
	}{
		{1, "package main"},
		{42, "func main() {"},
		{100, ""},
		{5, "  }"},
		{10, "---"},
	}
	for _, c := range cases {
		h1 := ComputeLineHash(c.line, c.content)
		h2 := ComputeLineHash(c.line, c.content)
		if h1 != h2 {
			t.Errorf("line %d %q: unstable hash %q vs %q", c.line, c.content, h1, h2)
		}
	}
}

func TestComputeLineHash_DictCoverage(t *testing.T) {
	// 生成足够多的不同输入，确认所有哈希值字符都在 DictStr 中
	seen := make(map[byte]bool)
	for i := 0; i < 256; i++ {
		content := string(rune('a' + i%26))
		h := ComputeLineHash(i+1, content)
		if len(h) != 2 {
			t.Fatalf("expected 2-char hash, got %q", h)
		}
		seen[h[0]] = true
		seen[h[1]] = true
	}
	for i := 0; i < len(DictStr); i++ {
		if !seen[DictStr[i]] {
			t.Errorf("dict char %q never appeared in hash output", DictStr[i])
		}
	}
}

func TestComputeLineHash_SeedBehavior(t *testing.T) {
	// 含字母数字的行：seed=0，不同行号相同内容应产生相同哈希
	content := "func foo() {}"
	h1 := ComputeLineHash(1, content)
	h2 := ComputeLineHash(99, content)
	if h1 != h2 {
		t.Errorf("alphanumeric line: same content different lines should have same hash: %q vs %q", h1, h2)
	}

	// 纯标点行：seed=lineNumber，不同行号相同内容应产生不同哈希
	punct := "  }"
	h3 := ComputeLineHash(1, punct)
	h4 := ComputeLineHash(2, punct)
	if h3 == h4 {
		t.Errorf("punctuation line: same content different lines should differ: both %q", h3)
	}

	// 纯空白行：seed=lineNumber
	blank := "   \t  "
	h5 := ComputeLineHash(10, blank)
	h6 := ComputeLineHash(20, blank)
	if h5 == h6 {
		t.Errorf("blank line: same content different lines should differ: both %q", h5)
	}
}

func TestComputeLineHash_Normalization(t *testing.T) {
	// \r 应被 strip
	h1 := ComputeLineHash(1, "hello\r")
	h2 := ComputeLineHash(1, "hello")
	if h1 != h2 {
		t.Errorf("\\r strip: %q vs %q", h1, h2)
	}

	// 尾部空白应被 strip
	h3 := ComputeLineHash(1, "hello  ")
	h4 := ComputeLineHash(1, "hello")
	if h3 != h4 {
		t.Errorf("trailing space strip: %q vs %q", h3, h4)
	}

	// 行内空白不应被 strip（中间 tab/space 差异应产生不同哈希）
	h5 := ComputeLineHash(1, "hello world")
	h6 := ComputeLineHash(1, "hello\tworld")
	if h5 == h6 {
		t.Errorf("inner whitespace should affect hash: both %q", h5)
	}
}

func TestComputeLineHash_EmptyString(t *testing.T) {
	// 空字符串 = 无字母数字 → seed=lineNumber
	h1 := ComputeLineHash(1, "")
	h2 := ComputeLineHash(2, "")
	if h1 == h2 {
		t.Errorf("empty string different lines should differ: both %q", h1)
	}
}

func TestDictStr_Length(t *testing.T) {
	if len(DictStr) != 16 {
		t.Errorf("DictStr length = %d, want 16", len(DictStr))
	}
	// 无重复字符
	seen := make(map[byte]bool)
	for i := 0; i < len(DictStr); i++ {
		if seen[DictStr[i]] {
			t.Errorf("DictStr has duplicate char %q", DictStr[i])
		}
		seen[DictStr[i]] = true
	}
}

func TestComputeLineHash_AllOutputsValid(t *testing.T) {
	// 暴力枚举一些输入，确保所有输出都是 DictStr 中的两位组合
	for line := 1; line <= 100; line++ {
		for _, content := range []string{"a", "", "}", "//", "123", "func"} {
			h := ComputeLineHash(line, content)
			if len(h) != 2 {
				t.Fatalf("line %d %q: expected 2-char hash, got %q", line, content, h)
			}
			if !strings.ContainsRune(DictStr, rune(h[0])) || !strings.ContainsRune(DictStr, rune(h[1])) {
				t.Errorf("line %d %q: hash %q contains char outside DictStr", line, content, h)
			}
		}
	}
}
