package suggest

import (
	"strings"
	"testing"
)

// TestSuggest_GlobLikeTypo 对应 §10.11 V1：拼写错一字母 → 候选正确 + 含方括号高亮。
// 用 "Architecure.md"（缺 t）而不是 "Archtechture.md"（字母换位）——字母换位是
// fzf 风格的已知弱点（V8 标记），不在 V1 验证范围。
func TestSuggest_GlobLikeTypo(t *testing.T) {
	candidates := []string{
		"Architecture.md",
		"docs/architecture/overview.md",
		"unrelated.go",
	}
	results := Suggest("Architecure.md", candidates, 3)
	if len(results) == 0 {
		t.Fatal("expected non-empty suggestions for missing-letter typo")
	}
	// Architecture.md 必须出现在结果中
	found := false
	for _, r := range results {
		if strings.Contains(r, "[") && strings.Contains(r, "]") &&
			strings.Contains(stripBrackets(r), "Architecture.md") {
			found = true
		}
	}
	if !found {
		t.Errorf("Architecture.md not in suggestions: %v", results)
	}
}

// TestSuggest_NoMatch 对应 §10.11 V5：完全无相似候选 → 空切片。
func TestSuggest_NoMatch(t *testing.T) {
	candidates := []string{"foo.go", "bar.go"}
	results := Suggest("xyzqwe", candidates, 3)
	if len(results) != 0 {
		t.Errorf("expected empty for no-match pattern, got %v", results)
	}
}

// TestSuggest_LimitK 验证 k 参数生效。
func TestSuggest_LimitK(t *testing.T) {
	candidates := []string{
		"abc1.go", "abc2.go", "abc3.go", "abc4.go", "abc5.go",
	}
	results := Suggest("abc", candidates, 3)
	if len(results) > 3 {
		t.Errorf("expected at most 3 results, got %d", len(results))
	}
}

// TestFormatForToolMessage_Empty 空切片返回空串。
func TestFormatForToolMessage_Empty(t *testing.T) {
	if got := FormatForToolMessage(nil); got != "" {
		t.Errorf("expected empty string for nil input, got %q", got)
	}
}

// TestFormatForToolMessage_Lines 验证多行格式。
func TestFormatForToolMessage_Lines(t *testing.T) {
	out := FormatForToolMessage([]string{"a", "b"})
	if !strings.HasPrefix(out, "\n\nDid you mean:\n") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "  - a") || !strings.Contains(out, "  - b") {
		t.Errorf("missing items: %q", out)
	}
}

// TestSuggest_EmptyInputs 边界：空 pattern / 空 candidates / k<=0。
func TestSuggest_EmptyInputs(t *testing.T) {
	if Suggest("", []string{"a"}, 3) != nil {
		t.Error("empty pattern should return nil")
	}
	if Suggest("a", nil, 3) != nil {
		t.Error("nil candidates should return nil")
	}
	if Suggest("a", []string{"a"}, 0) != nil {
		t.Error("k=0 should return nil")
	}
}

// stripBrackets 移除高亮方括号，用于断言文件名主干。
func stripBrackets(s string) string {
	return strings.NewReplacer("[", "", "]", "").Replace(s)
}
