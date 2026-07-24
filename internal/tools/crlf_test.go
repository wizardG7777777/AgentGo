package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// normalizeCRLF / isFullCRLF 单元测试 + read_file 展示归一化与 edit_file CRLF
// 重试的端到端回归（2026-07-21 跨平台排查 M4）。

func TestNormalizeCRLF(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"纯 LF 不变", "a\nb\n", "a\nb\n", false},
		{"CRLF 归一化", "a\r\nb\r\n", "a\nb\n", true},
		{"孤立 CR 归一化", "a\rb", "a\nb", true},
		{"混合行尾", "a\r\nb\nc", "a\nb\nc", true},
		{"无换行", "abc", "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := normalizeCRLF(tc.in)
			if got != tc.want || changed != tc.changed {
				t.Errorf("normalizeCRLF(%q) = (%q, %v), want (%q, %v)", tc.in, got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestIsFullCRLF(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"a\r\nb\r\n", true},
		{"a\nb\n", false},    // 纯 LF
		{"a\r\nb\nc", false}, // 混合行尾
		{"abc", false},
	}
	for _, tc := range cases {
		if got := isFullCRLF(tc.in); got != tc.want {
			t.Errorf("isFullCRLF(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestReadFile_CRLFNormalizedDisplay(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "crlf.txt")
	raw := []byte("line1\r\nline2\r\nline3\r\n")
	if err := os.WriteFile(fp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 展示层不含 \r
	body := out[strings.Index(out, "---"):]
	if strings.Contains(body, "\r") {
		t.Errorf("展示内容仍含 \\r: %q", body)
	}
	if !strings.Contains(body, "line1\nline2\nline3") {
		t.Errorf("归一化后的内容不符合预期: %q", body)
	}
	// 头部附注提示归一化已发生
	if !strings.Contains(out, "CRLF") {
		t.Errorf("缺少 CRLF 归一化附注: %q", out)
	}
	// hash 仍按磁盘原始字节计算（expected_hash 乐观锁语义不变）
	wantHash := computeSHA256(raw)
	if !strings.Contains(out, wantHash) {
		t.Errorf("hash 应为磁盘原始字节 SHA256 %s: %q", wantHash, out)
	}
}

func TestReadFile_LFFileNoNote(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "lf.txt")
	if err := os.WriteFile(fp, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if strings.Contains(out, "CRLF") {
		t.Errorf("纯 LF 文件不应出现归一化附注: %q", out)
	}
}

func TestEditFile_CRLFRetryPreservesLineEndings(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "crlf-edit.txt")
	raw := "alpha\r\nbeta\r\ngamma\r\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	// old_str 跨行，按 read_file 的 LF 展示构造——磁盘 CRLF 直接匹配必然为 0，
	// CRLF 重试应命中且替换后行尾保持 CRLF。
	out, err := callEditFile(g, path, "beta\ngamma", "BETA\ndelta", "")
	if err != nil {
		t.Fatalf("CRLF 重试应成功: %v", err)
	}
	if !strings.Contains(out, "CRLF") {
		t.Errorf("结果应提示 CRLF 兼容模式: %q", out)
	}
	data, _ := os.ReadFile(path)
	want := "alpha\r\nBETA\r\ndelta\r\n"
	if string(data) != want {
		t.Errorf("文件内容 = %q, want %q（行尾必须保持 CRLF）", string(data), want)
	}
}

func TestEditFile_CRLFNoMatchStillFails(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "crlf-nomatch.txt")
	if err := os.WriteFile(path, []byte("alpha\r\nbeta\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "nonexistent\ntext", "x", "")
	if err == nil || !strings.Contains(err.Error(), "未找到匹配内容") {
		t.Fatalf("归一化后仍无匹配应报 未找到匹配内容, got %v", err)
	}
}

func TestEditFile_MixedLineEndingsNoRetry(t *testing.T) {
	g, _, tmp := newWriteGroup(t, nil)
	path := filepath.Join(tmp, "mixed.txt")
	// 混合行尾：a 行 CRLF、b 行孤立 LF——isFullCRLF=false，不做逆变换重试，
	// 避免把孤立 LF 行误改成 CRLF。
	if err := os.WriteFile(path, []byte("a\r\nb\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := callEditFile(g, path, "a\nb", "x\ny", "")
	if err == nil || !strings.Contains(err.Error(), "未找到匹配内容") {
		t.Fatalf("混合行尾文件不应触发 CRLF 重试, got %v", err)
	}
}
