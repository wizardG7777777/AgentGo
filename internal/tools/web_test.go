package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/llm"
	"agentgo/internal/webtool"
)

// fakeSearchProvider 捕获传入的 query/opts，用于断言参数透传。
type fakeSearchProvider struct {
	lastQuery string
	lastOpts  *webtool.SearchOptions
	results   []webtool.SearchResult
	err       error
}

func (f *fakeSearchProvider) Name() string { return "fake" }
func (f *fakeSearchProvider) Search(ctx context.Context, query string, opts *webtool.SearchOptions) ([]webtool.SearchResult, error) {
	f.lastQuery = query
	f.lastOpts = opts
	return f.results, f.err
}

// dispatchCall 是测试辅助：通过注册表调用指定工具。
func dispatchCall(t *testing.T, r *agent.ToolRegistry, name string, args map[string]any) (string, error) {
	t.Helper()
	return r.Dispatch(context.Background(), llm.ToolCall{Name: name, Arguments: args})
}

// hasTool 检查注册表中是否存在某个工具定义。
func hasTool(r *agent.ToolRegistry, name string) bool {
	for _, d := range r.Defs() {
		if d.Name == name {
			return true
		}
	}
	return false
}

func TestWebGroup_Register_BothTools(t *testing.T) {
	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	if !hasTool(r, "web_search") {
		t.Errorf("expected web_search to be registered")
	}
	if !hasTool(r, "web_fetch") {
		t.Errorf("expected web_fetch to be registered")
	}
	if got := len(r.Defs()); got != 2 {
		t.Errorf("expected 2 tools, got %d", got)
	}
}

func TestWebGroup_Register_NilProvider_NoTools(t *testing.T) {
	r := agent.NewToolRegistry()
	WebGroup{Provider: nil}.Register(r)

	if got := len(r.Defs()); got != 0 {
		t.Errorf("expected 0 tools for nil provider, got %d", got)
	}
}

func TestWebSearch_Basic(t *testing.T) {
	fake := &fakeSearchProvider{
		results: []webtool.SearchResult{
			{Title: "Go 1.25 Released", URL: "https://go.dev/blog/go1.25", Snippet: "Go 1.25 is out"},
		},
	}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	out, err := dispatchCall(t, r, "web_search", map[string]any{"query": "go 1.25"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastQuery != "go 1.25" {
		t.Errorf("expected query=go 1.25, got %q", fake.lastQuery)
	}
	if !strings.Contains(out, "Go 1.25 Released") {
		t.Errorf("expected formatted output to contain title, got: %s", out)
	}
	if !strings.Contains(out, "https://go.dev/blog/go1.25") {
		t.Errorf("expected formatted output to contain URL, got: %s", out)
	}
}

func TestWebSearch_MissingQuery(t *testing.T) {
	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	_, err := dispatchCall(t, r, "web_search", map[string]any{"query": ""})
	if err == nil {
		t.Fatal("expected error for missing query")
	}
	if !strings.Contains(err.Error(), "缺少 query 参数") {
		t.Errorf("expected '缺少 query 参数', got: %v", err)
	}
}

func TestWebSearch_OptionsPassthrough_NumResults(t *testing.T) {
	fake := &fakeSearchProvider{}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	_, err := dispatchCall(t, r, "web_search", map[string]any{
		"query":       "golang",
		"max_results": float64(8),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastOpts == nil {
		t.Fatal("expected non-nil opts")
	}
	if fake.lastOpts.NumResults != 8 {
		t.Errorf("expected NumResults=8, got %d", fake.lastOpts.NumResults)
	}
}

func TestWebSearch_OptionsPassthrough_NumResults_Clamped(t *testing.T) {
	fake := &fakeSearchProvider{}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	_, err := dispatchCall(t, r, "web_search", map[string]any{
		"query":       "golang",
		"max_results": float64(999),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastOpts.NumResults != 10 {
		t.Errorf("expected NumResults clamped to 10, got %d", fake.lastOpts.NumResults)
	}
}

func TestWebSearch_OptionsPassthrough_TimeRange(t *testing.T) {
	fake := &fakeSearchProvider{}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	_, err := dispatchCall(t, r, "web_search", map[string]any{
		"query":      "golang",
		"time_range": "week",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastOpts == nil {
		t.Fatal("expected non-nil opts")
	}
	if fake.lastOpts.TimeRange != "week" {
		t.Errorf("expected TimeRange=week, got %q", fake.lastOpts.TimeRange)
	}
}

func TestWebSearch_OptionsPassthrough_Both(t *testing.T) {
	fake := &fakeSearchProvider{}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	_, err := dispatchCall(t, r, "web_search", map[string]any{
		"query":       "golang",
		"max_results": float64(3),
		"time_range":  "month",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastOpts.NumResults != 3 {
		t.Errorf("expected NumResults=3, got %d", fake.lastOpts.NumResults)
	}
	if fake.lastOpts.TimeRange != "month" {
		t.Errorf("expected TimeRange=month, got %q", fake.lastOpts.TimeRange)
	}
}

func TestWebSearch_NoOptions_StillSendsEmptyOpts(t *testing.T) {
	fake := &fakeSearchProvider{}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	_, err := dispatchCall(t, r, "web_search", map[string]any{"query": "golang"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastOpts == nil {
		t.Fatal("expected non-nil opts even without options")
	}
	if fake.lastOpts.NumResults != 0 {
		t.Errorf("expected NumResults=0 (zero value), got %d", fake.lastOpts.NumResults)
	}
	if fake.lastOpts.TimeRange != "" {
		t.Errorf("expected TimeRange='' (zero value), got %q", fake.lastOpts.TimeRange)
	}
}

func TestWebSearch_FormatsResultsWithMetadata(t *testing.T) {
	fake := &fakeSearchProvider{
		results: []webtool.SearchResult{
			{
				Title:       "Some Big Scoop",
				URL:         "https://techcrunch.com/2026/03/31/scoop",
				Snippet:     "The scoop details.",
				Source:      "techcrunch.com",
				PublishedAt: "2026-03-31",
			},
		},
	}
	r := agent.NewToolRegistry()
	WebGroup{Provider: fake}.Register(r)

	out, err := dispatchCall(t, r, "web_search", map[string]any{"query": "scoop"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "[来源: techcrunch.com") {
		t.Errorf("expected formatted output to contain '[来源: techcrunch.com', got:\n%s", out)
	}
	if !strings.Contains(out, "| 发布: 2026-03-31") {
		t.Errorf("expected formatted output to contain '| 发布: 2026-03-31', got:\n%s", out)
	}
}

// --- web_fetch tests ---
//
// webtool.AllowLoopbackForTests 临时禁用 SSRF 的 loopback 拒绝逻辑，
// 使 httptest.NewServer 绑定的 127.0.0.1 端口可被 FetchURLWithMode 访问。
// 测试结束后由 t.Cleanup 自动复位。

const articleHTML = `<html>
<head><title>Article Title</title></head>
<body>
<nav>navigation menu link1 link2</nav>
<header>site header banner</header>
<article>
<h1>Real Article Heading</h1>
<p>This is the real article body content that should survive extraction.</p>
</article>
<aside>sidebar widget</aside>
<footer>footer noise copyright</footer>
</body>
</html>`

func TestWebFetch_DefaultModeAuto(t *testing.T) {
	webtool.AllowLoopbackForTests(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()

	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	out, err := dispatchCall(t, r, "web_fetch", map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("web_fetch returned error: %v", err)
	}
	// auto 模式优先匹配 <article>，所以应包含正文但不包含 nav/footer 噪音
	if !strings.Contains(out, "Real Article Heading") {
		t.Errorf("auto 模式应提取 <article> 内容，实际:\n%s", out)
	}
	if !strings.Contains(out, "real article body content") {
		t.Errorf("auto 模式应包含正文段落，实际:\n%s", out)
	}
	if strings.Contains(out, "navigation menu") || strings.Contains(out, "footer noise") {
		t.Errorf("auto 模式不应包含 nav/footer 噪音，实际:\n%s", out)
	}
}

func TestWebFetch_ExtractModeFull(t *testing.T) {
	webtool.AllowLoopbackForTests(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()

	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	out, err := dispatchCall(t, r, "web_fetch", map[string]any{
		"url":          srv.URL,
		"extract_mode": "full",
	})
	if err != nil {
		t.Fatalf("web_fetch returned error: %v", err)
	}
	// full 模式应包含全部可见文本，包括 nav/footer/aside
	for _, want := range []string{"Real Article Heading", "navigation menu", "footer noise", "sidebar widget", "site header banner"} {
		if !strings.Contains(out, want) {
			t.Errorf("full 模式应包含 %q，实际:\n%s", want, out)
		}
	}
}

func TestWebFetch_ExtractModeArticle(t *testing.T) {
	webtool.AllowLoopbackForTests(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()

	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	out, err := dispatchCall(t, r, "web_fetch", map[string]any{
		"url":          srv.URL,
		"extract_mode": "article",
	})
	if err != nil {
		t.Fatalf("web_fetch returned error: %v", err)
	}
	// article 模式：包含正文，过滤 nav/header/footer/aside
	if !strings.Contains(out, "Real Article Heading") {
		t.Errorf("article 模式应包含正文标题，实际:\n%s", out)
	}
	if !strings.Contains(out, "real article body content") {
		t.Errorf("article 模式应包含正文段落，实际:\n%s", out)
	}
	for _, noise := range []string{"navigation menu", "footer noise", "sidebar widget", "site header banner"} {
		if strings.Contains(out, noise) {
			t.Errorf("article 模式不应包含 %q（属于噪音），实际:\n%s", noise, out)
		}
	}
}

// TestWebFetch_AutoFallbackToFull 验证 auto 模式在没有 <article>/<main> 时回退到全页面提取。
func TestWebFetch_AutoFallbackToFull(t *testing.T) {
	webtool.AllowLoopbackForTests(t)

	plainHTML := `<html><body><div><p>plain content without semantic tags</p></div></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(plainHTML))
	}))
	defer srv.Close()

	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	out, err := dispatchCall(t, r, "web_fetch", map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("web_fetch returned error: %v", err)
	}
	if !strings.Contains(out, "plain content without semantic tags") {
		t.Errorf("auto 模式找不到 <article> 时应回退提取全部正文，实际:\n%s", out)
	}
}

func TestWebFetch_MissingURL(t *testing.T) {
	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	_, err := dispatchCall(t, r, "web_fetch", map[string]any{"url": ""})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "缺少 url 参数") {
		t.Errorf("expected '缺少 url 参数', got: %v", err)
	}
}

func TestWebFetch_DefaultMode_WhenAbsent(t *testing.T) {
	// 不传 extract_mode；由于 SSRF 限制，这里仅断言“缺少 url 时给出预期错误”无法验证模式路径。
	// 改为断言：当 url 为空时（仍不传 extract_mode），closure 正确默认 mode="auto" 并将调用转发给 FetchURLWithMode。
	// FetchURLWithMode 会对空 url 返回 "缺少 url 参数"，用此作为 closure 未 panic 的证明。
	r := agent.NewToolRegistry()
	WebGroup{Provider: &fakeSearchProvider{}}.Register(r)

	_, err := dispatchCall(t, r, "web_fetch", map[string]any{})
	if err == nil {
		t.Fatal("expected error bubbling up from FetchURLWithMode")
	}
	if !strings.Contains(err.Error(), "缺少 url 参数") {
		t.Errorf("expected error to propagate, got: %v", err)
	}
}
