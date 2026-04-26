package hashline

import (
	"fmt"
	"strings"
	"testing"
)

func TestFormatHashLine(t *testing.T) {
	got := FormatHashLine(1, "package main")
	if !strings.HasPrefix(got, "1#") {
		t.Errorf("FormatHashLine prefix wrong: %q", got)
	}
	if !strings.HasSuffix(got, "|package main") {
		t.Errorf("FormatHashLine suffix wrong: %q", got)
	}
	parts := strings.SplitN(got, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("expected exactly one '|' separator, got %q", got)
	}
	prefix := parts[0] // "1#XX"
	suffix := parts[1] // "package main"
	if suffix != "package main" {
		t.Errorf("content = %q, want package main", suffix)
	}
	// prefix 格式验证
	ref, err := ParseLineRef(prefix)
	if err != nil {
		t.Fatalf("prefix %q not parseable: %v", prefix, err)
	}
	if ref.Line != 1 {
		t.Errorf("line = %d, want 1", ref.Line)
	}
	if len(ref.Hash) != 2 {
		t.Errorf("hash length = %d, want 2", len(ref.Hash))
	}
}

func TestFormatHashLine_EmptyContent(t *testing.T) {
	got := FormatHashLine(5, "")
	if got != fmt.Sprintf("5#%s|", ComputeLineHash(5, "")) {
		t.Errorf("FormatHashLine empty content wrong: %q", got)
	}
}

func TestFormatHashLines(t *testing.T) {
	content := "line one\nline two\nline three"
	got := FormatHashLines(1, content)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), got)
	}
	for i, line := range lines {
		wantPrefix := fmt.Sprintf("%d#", i+1)
		if !strings.HasPrefix(line, wantPrefix) {
			t.Errorf("line %d prefix wrong: %q", i, line)
		}
	}
}

func TestFormatHashLines_StartLine(t *testing.T) {
	content := "a\nb"
	got := FormatHashLines(10, content)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "10#") {
		t.Errorf("first line should start at 10: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "11#") {
		t.Errorf("second line should start at 11: %q", lines[1])
	}
}

func TestFormatHashLines_TrailingNewline(t *testing.T) {
	content := "a\nb\n"
	got := FormatHashLines(1, content)
	// 尾部换行应产生一个空内容的行
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (including empty last), got %d: %q", len(lines), got)
	}
	// 最后一行应只有 "3#HH|"（空内容）
	if !strings.HasPrefix(lines[2], "3#") {
		t.Errorf("last line should be line 3: %q", lines[2])
	}
}
