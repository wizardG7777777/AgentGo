package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
)

// newTestGroup 创建一个以 tmpDir 为 workdir 的 LocalReadGroup。
func newTestGroup(tmpDir string, cache *agent.FileStateCache) LocalReadGroup {
	return LocalReadGroup{
		Workdir: &DefaultWorkdir{ProjectRoot: tmpDir},
		Cache:   cache,
	}
}

func TestLocalReadGroup_Register_FourTools(t *testing.T) {
	r := agent.NewToolRegistry()
	g := newTestGroup(t.TempDir(), nil)
	g.Register(r)

	defs := r.Defs()
	if len(defs) != 1 {
		t.Fatalf("期望只注册 read_file，实际 %d", len(defs))
	}
	wantNames := map[string]bool{"read_file": false}

	for _, d := range defs {
		if _, ok := wantNames[d.Name]; !ok {
			t.Errorf("意外的工具: %s", d.Name)
			continue
		}
		wantNames[d.Name] = true
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("工具未注册: %s", name)
		}
	}
}

func TestReadFile_Basic(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "a.txt")
	if err := os.WriteFile(fp, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("输出缺少原始内容: %q", out)
	}
	if !strings.Contains(out, "[hash]") {
		t.Errorf("输出缺少 [hash] 行: %q", out)
	}
	if !strings.Contains(out, "[file]") {
		t.Errorf("输出缺少 [file] 行: %q", out)
	}
}

// TestReadFile_SelfDescribingHeader 验证 read_file 返回的头部含有
// 自描述信息：路径、行范围、总行数、hash。这让 LLM 即使在历史压缩后
// 看到 tool result 也能知道自己读了什么。
func TestReadFile_SelfDescribingHeader(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "lines.txt")
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString(fmt.Sprintf("line%02d\n", i))
	}
	if err := os.WriteFile(fp, []byte(strings.TrimRight(b.String(), "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)

	// 完整读取：头部应显示 "10 lines, full"
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(10 lines, full)") {
		t.Errorf("完整读取头部应含 '(10 lines, full)'，实际:\n%s", out)
	}

	// 行切片读取：头部应显示 "lines 3-5 of 10"
	out, err = g.readFile(context.Background(), map[string]any{
		"path":   fp,
		"offset": 3,
		"limit":  3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(lines 3-5 of 10)") {
		t.Errorf("切片读取头部应含 '(lines 3-5 of 10)'，实际:\n%s", out)
	}

	// 切片末尾溢出：应显示到实际末尾
	out, err = g.readFile(context.Background(), map[string]any{
		"path":   fp,
		"offset": 8,
		"limit":  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(lines 8-10 of 10)") {
		t.Errorf("末尾切片头部应含 '(lines 8-10 of 10)'，实际:\n%s", out)
	}
}

func TestReadFile_OffsetLimit(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "lines.txt")
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		b.WriteString(fmt.Sprintf("line%02d\n", i))
	}
	if err := os.WriteFile(fp, []byte(strings.TrimRight(b.String(), "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)

	// offset=5, limit=3 → line05..line07
	out, err := g.readFile(context.Background(), map[string]any{
		"path": fp, "offset": 5, "limit": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"line05", "line06", "line07"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %s，输出: %q", want, out)
		}
	}
	if strings.Contains(out, "line04") || strings.Contains(out, "line08") {
		t.Errorf("包含范围外行: %q", out)
	}

	// offset=18, limit=10 → 18..20
	out, err = g.readFile(context.Background(), map[string]any{
		"path": fp, "offset": 18, "limit": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"line18", "line19", "line20"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %s，输出: %q", want, out)
		}
	}
	if strings.Contains(out, "line17") {
		t.Errorf("包含范围外行: %q", out)
	}

	// offset=100 → 溢出提示
	out, err = g.readFile(context.Background(), map[string]any{
		"path": fp, "offset": 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "超出文件总行数") {
		t.Errorf("期望溢出提示，实际: %q", out)
	}

	// offset=0 → 视为 1
	out, err = g.readFile(context.Background(), map[string]any{
		"path": fp, "offset": 0, "limit": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line01") || !strings.Contains(out, "line02") {
		t.Errorf("offset=0 未返回前两行: %q", out)
	}
}

func TestReadFile_HashStable(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "h.txt")
	if err := os.WriteFile(fp, []byte("stable content"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)

	extract := func(s string) string {
		for _, ln := range strings.Split(s, "\n") {
			if strings.HasPrefix(ln, "[hash] ") {
				return strings.TrimPrefix(ln, "[hash] ")
			}
		}
		return ""
	}

	out1, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	out2, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	h1, h2 := extract(out1), extract(out2)
	if h1 == "" || h1 != h2 {
		t.Errorf("hash 不稳定: %q vs %q", h1, h2)
	}
}

func TestReadFile_PathValidation(t *testing.T) {
	tmp := t.TempDir()
	g := newTestGroup(tmp, nil)
	_, err := g.readFile(context.Background(), map[string]any{
		"path": "../../etc/passwd",
	})
	if err == nil {
		t.Fatal("期望路径越界错误，实际 nil")
	}
}

func TestReadFile_CacheHit(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "c.txt")
	if err := os.WriteFile(fp, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := agent.NewFileStateCache(50)
	g := newTestGroup(tmp, cache)

	out1, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out1, "original") {
		t.Fatalf("首次读取内容错: %q", out1)
	}

	// 第二次同参数 read_file 命中缓存：文件未变时返回摘要 stub 而非全文
	// （闸 1，2026-07-22），并给出 force_full / offset 取回指引。
	out2, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "original") {
		t.Errorf("缓存命中不应再返回全文: %q", out2)
	}
	for _, want := range []string{"already read, unchanged", "force_full=true", "[hash]"} {
		if !strings.Contains(out2, want) {
			t.Errorf("缓存命中 stub 缺少 %q: %q", want, out2)
		}
	}

	// force_full=true 时仍返回全文。
	out2f, err := g.readFile(context.Background(), map[string]any{"path": fp, "force_full": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2f, "original") {
		t.Errorf("force_full=true 应返回全文: %q", out2f)
	}

	// 外部/他人改写文件后，stat 校验应使缓存失效并重新读盘
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(fp, []byte("MODIFIED"), 0o644); err != nil {
		t.Fatal(err)
	}
	out3, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out3, "MODIFIED") {
		t.Errorf("外部修改后仍命中陈旧缓存: %q", out3)
	}
}

// TestGrepSearch_EmptyResult_HintsDiagnostics 锁定空结果诊断消息契约。
// 修复 2026-04-23 暴露的 "result_len=18 沉默失败" P2：LLM 看到空结果
// 无法判断是 pattern 错还是 path 错，需要显式列出扫描统计 + 排错路径。

// TestGlobSearch_EmptyResult_HintsDiagnostics 对称覆盖 glob_search 空结果契约。

// §10 Did-You-Mean：list_dir 路径不存在时给出近似目录候选。

// §10 Did-You-Mean：grep_search 空结果时给出相似文件名候选。

// === §7 Hashline 行哈希增强测试 ===

func TestReadFile_HashlineEnabled(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "foo.go")
	content := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	g.HashlineEnabled = true
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 应包含 hashline 前缀的行
	if !strings.Contains(out, "1#") || !strings.Contains(out, "|") {
		t.Errorf("hashline enabled 时输出应含 N#HH| 前缀: %q", out)
	}
	// 原始内容应仍在
	if !strings.Contains(out, "package main") {
		t.Errorf("输出缺少原始内容: %q", out)
	}
}

func TestReadFile_HashlineDisabled(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "bar.go")
	content := "package main\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	g.HashlineEnabled = false
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 不应包含 hashline 前缀格式（行号#哈希|）
	if strings.Contains(out, "1#") && strings.Contains(out, "|") {
		t.Errorf("hashline disabled 时输出不应含 N#HH| 前缀: %q", out)
	}
	// 原始内容应仍在
	if !strings.Contains(out, "package main") {
		t.Errorf("输出缺少原始内容: %q", out)
	}
}

// §10 Did-You-Mean：read_file 路径不存在时给出父目录近似文件候选。
func TestReadFile_NotExist_DidYouMean(t *testing.T) {
	tmp := t.TempDir()
	// 创建几个文件作为候选池
	for _, f := range []string{"README.md", "main.go", "go.mod"} {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := newTestGroup(tmp, nil)
	_, err := g.readFile(context.Background(), map[string]any{
		"path": filepath.Join(tmp, "REDME.md"), // typo：漏了 A
	})
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
	if !strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("期望 'Did you mean' 提示，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "RE") || !strings.Contains(err.Error(), "DME") {
		t.Errorf("期望含 'README' 高亮候选，实际: %v", err)
	}
}

// §10 Did-You-Mean：glob_search 空结果时给出相似文件名候选。

// TestReadFile_TruncationNotice 验证 64 Ki 字符截断时附带元数据公告
// （2026-07-22 闸 2）：本段原大小 + 续读行号指引。
func TestReadFile_TruncationNotice(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "big.txt")
	// 1000 行 × 100 字符 = 100000 字符，必然触发截断。
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString(strings.Repeat("x", 99) + "\n")
	}
	if err := os.WriteFile(fp, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(tmp, nil)
	out, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[truncated to 65536 chars]", "已截断：本段原 100000 字符", "用 offset=", "续读"} {
		if !strings.Contains(out, want) {
			t.Errorf("截断公告缺少 %q:\n%s", want, out[len(out)-300:])
		}
	}
}
