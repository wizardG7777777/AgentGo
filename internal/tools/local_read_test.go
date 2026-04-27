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
	if len(defs) != 4 {
		t.Fatalf("期望注册 4 个工具，实际 %d", len(defs))
	}
	wantNames := map[string]bool{
		"read_file":   false,
		"list_dir":    false,
		"grep_search": false,
		"glob_search": false,
	}
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

	// 第二次同参数 read_file 应命中缓存
	out2, err := g.readFile(context.Background(), map[string]any{"path": fp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "original") {
		t.Errorf("缓存未命中（应返回原内容）: %q", out2)
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

func TestListDir_Depth1(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(tmp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(tmp, nil)
	out, err := g.listDir(context.Background(), map[string]any{"path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[文件] a.txt", "[文件] b.txt", "[文件] c.txt", "[目录] sub/"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q，输出:\n%s", want, out)
		}
	}
	// 顺序检查：a 在 b 之前
	if ai, bi := strings.Index(out, "a.txt"), strings.Index(out, "b.txt"); ai < 0 || bi < 0 || ai > bi {
		t.Errorf("顺序不对:\n%s", out)
	}
}

func TestListDir_Depth2_TreeOutput(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "top.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "sub", "inner.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(tmp, nil)
	out, err := g.listDir(context.Background(), map[string]any{
		"path": tmp, "depth": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[目录] sub/") {
		t.Errorf("顶层目录缺失:\n%s", out)
	}
	if !strings.Contains(out, "  [文件] inner.txt") {
		t.Errorf("缩进子条目缺失:\n%s", out)
	}
	if !strings.Contains(out, "[文件] top.txt") {
		t.Errorf("顶层文件缺失:\n%s", out)
	}
}

func TestListDir_HidesDotfiles_AtDepthGT1(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".git", "HEAD"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "visible.txt"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(tmp, nil)
	out, err := g.listDir(context.Background(), map[string]any{
		"path": tmp, "depth": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".git") {
		t.Errorf("应跳过 .git:\n%s", out)
	}
	if !strings.Contains(out, "visible.txt") {
		t.Errorf("visible.txt 缺失:\n%s", out)
	}
}

func TestGrepSearch_Basic(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "src.txt")
	content := "foo\nbar NEEDLE here\nbaz\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(tmp, nil)
	out, err := g.grepSearch(context.Background(), map[string]any{
		"pattern": "NEEDLE", "path": tmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "src.txt:2:") {
		t.Errorf("缺少 path:lineno 格式（期望 :2:）:\n%s", out)
	}
	if !strings.Contains(out, "NEEDLE") {
		t.Errorf("缺少匹配内容:\n%s", out)
	}
}

func TestGrepSearch_MaxLines(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "many.txt")
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("HIT line\n")
	}
	if err := os.WriteFile(fp, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(tmp, nil)
	out, err := g.grepSearch(context.Background(), map[string]any{
		"pattern": "HIT", "path": tmp, "max_lines": 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Errorf("期望 5 行，实际 %d:\n%s", len(lines), out)
	}
}

func TestGlobSearch_Recursive(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(tmp, "one.txt"),
		filepath.Join(tmp, "a", "two.txt"),
		filepath.Join(tmp, "a", "b", "three.txt"),
		filepath.Join(tmp, "a", "ignore.md"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g := newTestGroup(tmp, nil)
	out, err := g.globSearch(context.Background(), map[string]any{
		"pattern":  "**/*.txt",
		"root_dir": tmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one.txt", "two.txt", "three.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ignore.md") {
		t.Errorf("不应包含 .md 文件:\n%s", out)
	}
}

// TestGrepSearch_EmptyResult_HintsDiagnostics 锁定空结果诊断消息契约。
// 修复 2026-04-23 暴露的 "result_len=18 沉默失败" P2：LLM 看到空结果
// 无法判断是 pattern 错还是 path 错，需要显式列出扫描统计 + 排错路径。
func TestGrepSearch_EmptyResult_HintsDiagnostics(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	out, err := g.grepSearch(context.Background(), map[string]any{
		"pattern": "definitely_not_present_xyz",
		"path":    tmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 关键词：扫描数量 + list_dir 指引 + 非正则提示 + 1MB 限制提示
	for _, want := range []string{"扫描", "list_dir", "非正则", "1MB"} {
		if !strings.Contains(out, want) {
			t.Errorf("空结果消息缺少 %q 诊断信息:\n%s", want, out)
		}
	}
}

// TestGlobSearch_EmptyResult_HintsDiagnostics 对称覆盖 glob_search 空结果契约。
func TestGlobSearch_EmptyResult_HintsDiagnostics(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newTestGroup(tmp, nil)
	out, err := g.globSearch(context.Background(), map[string]any{
		"pattern":  "**/*.nonexistent_ext",
		"root_dir": tmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"扫描", "list_dir", "glob", "相对路径"} {
		if !strings.Contains(out, want) {
			t.Errorf("空结果消息缺少 %q 诊断信息:\n%s", want, out)
		}
	}
}

func TestGlobSearch_PathValidation(t *testing.T) {
	tmp := t.TempDir()
	g := newTestGroup(tmp, nil)
	_, err := g.globSearch(context.Background(), map[string]any{
		"pattern":  "**/*.go",
		"root_dir": "../../",
	})
	if err == nil {
		t.Fatal("期望路径越界错误，实际 nil")
	}
}

// §10 Did-You-Mean：list_dir 路径不存在时给出近似目录候选。
func TestListDir_NotExist_DidYouMean(t *testing.T) {
	tmp := t.TempDir()
	// 创建几个目录作为候选池
	for _, d := range []string{"docs", "internal", "cmd"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g := newTestGroup(tmp, nil)
	_, err := g.listDir(context.Background(), map[string]any{
		"path": filepath.Join(tmp, "doc"), // typo：少了 s
	})
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
	if !strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("期望 'Did you mean' 提示，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "doc") {
		t.Errorf("期望候选 'doc'（含高亮形式），实际: %v", err)
	}
}

// §10 Did-You-Mean：grep_search 空结果时给出相似文件名候选。
func TestGrepSearch_Empty_DidYouMean(t *testing.T) {
	tmp := t.TempDir()
	// 创建一些文件，文件名含 "auth" 关键字
	for _, f := range []string{"auth.go", "authentication.go", "main.go"} {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := newTestGroup(tmp, nil)
	out, err := g.grepSearch(context.Background(), map[string]any{
		"pattern": "auth", // 文件内容中不存在，但文件名含 auth
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("期望 nil 错误，实际: %v", err)
	}
	if !strings.Contains(out, "Did you mean") {
		t.Errorf("期望 'Did you mean' 提示，实际: %v", out)
	}
	if !strings.Contains(out, "auth") {
		t.Errorf("期望含 'auth' 的候选，实际: %v", out)
	}
}


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
func TestGlobSearch_Empty_DidYouMean(t *testing.T) {
	tmp := t.TempDir()
	// 创建一些文件，文件名含 "config" 关键字
	for _, f := range []string{"config.yaml", "main.go", "go.mod"} {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := newTestGroup(tmp, nil)
	out, err := g.globSearch(context.Background(), map[string]any{
		"pattern": "confg.yaml", // typo：漏了一个 i
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("期望 nil 错误，实际: %v", err)
	}
	if !strings.Contains(out, "Did you mean") {
		t.Errorf("期望 'Did you mean' 提示，实际: %v", out)
	}
	if !strings.Contains(out, "conf") && !strings.Contains(out, "Conf") {
		t.Errorf("期望含 'conf' 的候选，实际: %v", out)
	}
}
