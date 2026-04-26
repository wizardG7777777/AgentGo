package hashline

import (
	"strings"
	"testing"
)

func TestParseLineRef_Basic(t *testing.T) {
	ref, err := ParseLineRef("42#VK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Line != 42 {
		t.Errorf("Line = %d, want 42", ref.Line)
	}
	if ref.Hash != "VK" {
		t.Errorf("Hash = %q, want VK", ref.Hash)
	}
}

func TestParseLineRef_Tolerant(t *testing.T) {
	cases := []struct {
		input    string
		wantLine int
		wantHash string
	}{
		{"42#VK", 42, "VK"},
		{">>> 42#VK", 42, "VK"},
		{">>>42#VK", 42, "VK"},
		{"+ 42#VK", 42, "VK"},
		{"- 42#VK", 42, "VK"},
		{"42 # VK", 42, "VK"},
		{"42#VK|content here", 42, "VK"},
		{"  42#VK  ", 42, "VK"},
		{">>> 42 # VK | foo", 42, "VK"},
	}
	for _, c := range cases {
		ref, err := ParseLineRef(c.input)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.input, err)
			continue
		}
		if ref.Line != c.wantLine {
			t.Errorf("%q: Line = %d, want %d", c.input, ref.Line, c.wantLine)
		}
		if ref.Hash != c.wantHash {
			t.Errorf("%q: Hash = %q, want %q", c.input, ref.Hash, c.wantHash)
		}
	}
}

func TestParseLineRef_ExtractFromText(t *testing.T) {
	// 在一段文本中提取第一个 LINE#HASH 子串
	ref, err := ParseLineRef("some text 42#VK more text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Line != 42 || ref.Hash != "VK" {
		t.Errorf("got %+v, want Line=42 Hash=VK", ref)
	}
}

func TestParseLineRef_NameNotLineNumber(t *testing.T) {
	_, err := ParseLineRef("name#VK")
	if err == nil {
		t.Fatal("expected error for name#VK")
	}
	if !strings.Contains(err.Error(), "行号") {
		t.Errorf("error should mention '行号', got: %v", err)
	}
}

func TestParseLineRef_InvalidHash(t *testing.T) {
	_, err := ParseLineRef("42#AA")
	if err == nil {
		t.Fatal("expected error for invalid hash chars")
	}
}

func TestParseLineRef_MissingHash(t *testing.T) {
	_, err := ParseLineRef("42")
	if err == nil {
		t.Fatal("expected error for missing hash")
	}
}

func TestParseLineRef_Empty(t *testing.T) {
	_, err := ParseLineRef("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

// ---- StripHashPrefix ----

func TestStripHashPrefix_AllHashlines(t *testing.T) {
	input := "1#VK|package main\n2#QZ|func main() {"
	got := StripHashPrefix(input)
	want := "package main\nfunc main() {"
	if got != want {
		t.Errorf("StripHashPrefix = %q, want %q", got, want)
	}
}

func TestStripHashPrefix_Exactly50Percent(t *testing.T) {
	// 2 行，1 行带前缀 → 50% = 阈值，应剥掉
	input := "1#VK|hello\nworld"
	got := StripHashPrefix(input)
	want := "hello\nworld"
	if got != want {
		t.Errorf("StripHashPrefix = %q, want %q", got, want)
	}
}

func TestStripHashPrefix_BelowThreshold(t *testing.T) {
	// 3 行，1 行带前缀 → 33% < 50%，不应剥
	input := "1#VK|hello\nworld\nfoo"
	got := StripHashPrefix(input)
	if got != input {
		t.Errorf("StripHashPrefix should return unchanged, got %q", got)
	}
}

func TestStripHashPrefix_DiffPlus(t *testing.T) {
	input := "+hello\n+world"
	got := StripHashPrefix(input)
	want := "hello\nworld"
	if got != want {
		t.Errorf("StripHashPrefix diff+ = %q, want %q", got, want)
	}
}

func TestStripHashPrefix_HashlineWinsOverDiffPlus(t *testing.T) {
	// 同时满足 hashline 和 diff+ 阈值，hashline 优先
	input := "1#VK|+hello\n2#QZ|+world"
	got := StripHashPrefix(input)
	want := "+hello\n+world"
	if got != want {
		t.Errorf("hashline should win over diff+: got %q, want %q", got, want)
	}
}

func TestStripHashPrefix_NoStripping(t *testing.T) {
	input := "hello world\nnormal line\nfoo bar"
	got := StripHashPrefix(input)
	if got != input {
		t.Errorf("should return unchanged, got %q", got)
	}
}

func TestStripHashPrefix_Empty(t *testing.T) {
	got := StripHashPrefix("")
	if got != "" {
		t.Errorf("empty input: got %q", got)
	}
}

func TestStripHashPrefix_OnlyEmptyLines(t *testing.T) {
	input := "\n\n\n"
	got := StripHashPrefix(input)
	if got != input {
		t.Errorf("only empty lines: got %q", got)
	}
}

func TestStripHashPrefix_PartialHashlines(t *testing.T) {
	// 有些行带前缀，有些不带，但达阈值后只剥带前缀的
	input := "1#VK|hello\nplain line\n2#QZ|world"
	got := StripHashPrefix(input)
	want := "hello\nplain line\nworld"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripHashPrefix_DoublePlusNotDiff(t *testing.T) {
	// "++" 不应被当作 diff +（DiffPlusRegex 是 ^\+(?!\+)）
	input := "++hello\n++world"
	got := StripHashPrefix(input)
	if got != input {
		t.Errorf("++ should not be stripped: got %q", got)
	}
}

// TestStripHashPrefix_PipeInContent 是边界 case：
// hashline 前缀剥离只应吞掉到第一个 `|`，行内出现的额外 `|` 必须保留。
// 此测试针对的真实场景：LLM 把 read_file 输出粘进 edit_file 参数，
// 行内本身可能含 markdown 表格分隔、shell 管道、Go map 字面等带 `|` 的内容。
//
// 失败模式：若实现错误地用 `|.*$` 贪婪剥到行尾，"1#VK|a|b|c" 会被剥成空串，
// 而正确行为是剥成 "a|b|c"。
func TestStripHashPrefix_PipeInContent(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single pipe in content",
			input: "1#VK|content with | pipe\n2#QZ|another | line",
			want:  "content with | pipe\nanother | line",
		},
		{
			name:  "multiple pipes",
			input: "1#VK|a|b|c|d\n2#QZ|x|y|z",
			want:  "a|b|c|d\nx|y|z",
		},
		{
			name:  "empty content after prefix",
			input: "1#VK|\n2#QZ|",
			want:  "\n",
		},
		{
			name:  "pipe immediately after prefix pipe",
			input: "1#VK||double pipe content\n2#QZ||another",
			want:  "|double pipe content\n|another",
		},
		{
			name:  "markdown table syntax preserved",
			input: "1#VK|| col1 | col2 |\n2#QZ||---|---|",
			want:  "| col1 | col2 |\n|---|---|",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StripHashPrefix(c.input)
			if got != c.want {
				t.Errorf("StripHashPrefix(%q)\n  got  = %q\n  want = %q", c.input, got, c.want)
			}
		})
	}
}
